// Package bonya is the Go SDK for Bonya sandboxes.
//
// A Client talks to the Bonya API over HTTPS. Sandbox lifecycle, snapshots,
// previews and accounts go to the API endpoint; exec, files and terminal
// sessions go to the edge endpoint, which is the same host unless configured
// otherwise. Every failed call returns an *Error (see errors.go).
package bonya

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// DefaultEndpoint is used when neither WithEndpoint nor BONYA_ENDPOINT is set.
const DefaultEndpoint = "https://api.bonya.ai"

const (
	defaultTimeout    = 30 * time.Second
	defaultMaxRetries = 2
	userAgent         = "bonya-sdk-go/0.1.0"
)

// Client is safe for concurrent use. Create one with NewClient and reuse it.
type Client struct {
	apiKey         string
	apiURL         *url.URL
	edgeURL        *url.URL
	organizationID string
	timeout        time.Duration
	maxRetries     int
	http           *http.Client
}

// Option configures NewClient.
type Option func(*clientConfig)

type clientConfig struct {
	apiKey         *string
	endpoint       *string
	edgeEndpoint   *string
	caBundle       *string
	organizationID *string
	timeout        *time.Duration
	maxRetries     *int
	httpClient     *http.Client
}

// WithAPIKey sets the credential (an API key or a login token). Defaults to
// BONYA_API_KEY.
func WithAPIKey(key string) Option { return func(c *clientConfig) { c.apiKey = &key } }

// WithEndpoint sets the API base URL. Defaults to BONYA_ENDPOINT, then
// DefaultEndpoint. Plain http is accepted only for loopback hosts.
func WithEndpoint(endpoint string) Option { return func(c *clientConfig) { c.endpoint = &endpoint } }

// WithEdgeEndpoint sets the base URL for exec, files and sessions. Defaults to
// BONYA_EDGE_ENDPOINT, then the API endpoint.
func WithEdgeEndpoint(endpoint string) Option {
	return func(c *clientConfig) { c.edgeEndpoint = &endpoint }
}

// WithCABundle trusts the PEM certificates in path in addition to the system
// roots (for private development CAs). Defaults to BONYA_CA_BUNDLE.
func WithCABundle(path string) Option { return func(c *clientConfig) { c.caBundle = &path } }

// WithOrganizationID selects the organization calls act on. Defaults to
// BONYA_ORGANIZATION_ID, then the caller's personal organization.
func WithOrganizationID(id string) Option {
	return func(c *clientConfig) { c.organizationID = &id }
}

// WithTimeout sets the deadline for calls whose context has none. Streams
// (interactive exec, terminal attach, file transfers) are bounded only by
// their context.
func WithTimeout(timeout time.Duration) Option {
	return func(c *clientConfig) { c.timeout = &timeout }
}

// WithMaxRetries sets how often a repeatable call is retried after a
// transient failure. Defaults to 2.
func WithMaxRetries(n int) Option { return func(c *clientConfig) { c.maxRetries = &n } }

// WithHTTPClient replaces the HTTP client (its Timeout should be zero; the
// SDK sets deadlines per call). WithCABundle is ignored when this is set.
func WithHTTPClient(hc *http.Client) Option { return func(c *clientConfig) { c.httpClient = hc } }

// NewClient builds a Client from options and environment variables.
func NewClient(opts ...Option) (*Client, error) {
	var cfg clientConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	apiKey := strings.TrimSpace(stringOrEnv(cfg.apiKey, "BONYA_API_KEY"))
	if apiKey == "" {
		return nil, &Error{Kind: ErrUnauthenticated, Message: "an API key is required (set BONYA_API_KEY or run `bonya login`)"}
	}

	endpoint := stringOrEnv(cfg.endpoint, "BONYA_ENDPOINT")
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	apiURL, err := parseEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	edgeURL := apiURL
	if edge := stringOrEnv(cfg.edgeEndpoint, "BONYA_EDGE_ENDPOINT"); edge != "" {
		if edgeURL, err = parseEndpoint(edge); err != nil {
			return nil, err
		}
	}

	timeout := defaultTimeout
	if cfg.timeout != nil {
		timeout = *cfg.timeout
	}
	if timeout <= 0 {
		return nil, invalidArgument("timeout must be positive")
	}
	maxRetries := defaultMaxRetries
	if cfg.maxRetries != nil {
		maxRetries = *cfg.maxRetries
	}
	if maxRetries < 0 {
		return nil, invalidArgument("max retries must not be negative")
	}

	organizationID := strings.TrimSpace(stringOrEnv(cfg.organizationID, "BONYA_ORGANIZATION_ID"))
	if cfg.organizationID != nil && organizationID == "" {
		return nil, invalidArgument("organization id must not be empty when set explicitly")
	}

	hc := cfg.httpClient
	if hc == nil {
		if hc, err = newHTTPClient(stringOrEnv(cfg.caBundle, "BONYA_CA_BUNDLE")); err != nil {
			return nil, err
		}
	}

	return &Client{
		apiKey:         apiKey,
		apiURL:         apiURL,
		edgeURL:        edgeURL,
		organizationID: organizationID,
		timeout:        timeout,
		maxRetries:     maxRetries,
		http:           hc,
	}, nil
}

// OrganizationID returns the organization the client acts on, or "" for the
// caller's personal organization.
func (c *Client) OrganizationID() string { return c.organizationID }

func newHTTPClient(caBundle string) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 16
	if caBundle != "" {
		pem, err := os.ReadFile(caBundle)
		if err != nil {
			return nil, &Error{Kind: ErrInvalidRequest, Message: "read CA bundle", cause: err}
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, invalidArgument("CA bundle contains no PEM certificates")
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Transport: transport}, nil
}

// parseEndpoint accepts an https URL (or http for loopback hosts) with an
// optional path prefix and no credentials, query or fragment.
func parseEndpoint(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return nil, invalidArgument("endpoint " + raw + " is not a valid URL")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopback(u.Hostname()) {
			return nil, invalidArgument("endpoint must use https unless it is a loopback address")
		}
	default:
		return nil, invalidArgument("endpoint must use https")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, invalidArgument("endpoint must not contain credentials, a query or a fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func stringOrEnv(value *string, env string) string {
	if value != nil {
		return *value
	}
	return os.Getenv(env)
}
