package bonya

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const (
	deviceLoginDefaultInterval = 3 * time.Second
	deviceLoginMinInterval     = time.Second
	// deviceLoginSlowDown is added to the poll interval each time the
	// service says the CLI is polling too fast.
	deviceLoginSlowDown = 5 * time.Second
)

// DeviceLoginOptions configures StartDeviceLogin. Device login needs no
// credential; it produces one.
type DeviceLoginOptions struct {
	// Endpoint is the API base URL. Defaults to BONYA_ENDPOINT, then
	// DefaultEndpoint.
	Endpoint string
	// CABundle trusts the PEM certificates in this file in addition to the
	// system roots. Defaults to BONYA_CA_BUNDLE. Ignored when HTTPClient is set.
	CABundle string
	// HTTPClient replaces the HTTP client (its Timeout should be zero).
	HTTPClient *http.Client
}

// DeviceLogin is a pending browser login for a command-line tool. Show
// VerificationURL to the user, then call Wait.
type DeviceLogin struct {
	// VerificationURL is the page where the user approves the login. It
	// carries the login's one-time token: show it only to the user.
	VerificationURL string
	// ExpiresAt is when the login stops being approvable.
	ExpiresAt time.Time
	// Interval is how often Wait polls, as the service advised.
	Interval time.Duration

	client *Client
	token  string

	mu     sync.Mutex
	result *DeviceLoginResult
}

// DeviceLoginResult is the outcome of an approved device login.
type DeviceLoginResult struct {
	// APIKey is a new API key for the user who approved. It is returned once
	// and never shown again: store it now. String and fmt verbs never print it.
	APIKey string
	// UserEmail is the approving user's email.
	UserEmail string
}

// String describes the result without its API key.
func (r DeviceLoginResult) String() string {
	return "device login approved by " + r.UserEmail + " (api key redacted)"
}

// GoString keeps %#v from printing the API key.
func (r DeviceLoginResult) GoString() string { return r.String() }

// StartDeviceLogin begins a browser login. The user opens VerificationURL,
// signs in and approves; Wait then returns a new API key for that user.
func StartDeviceLogin(ctx context.Context, opts DeviceLoginOptions) (*DeviceLogin, error) {
	c, err := newDeviceLoginClient(opts)
	if err != nil {
		return nil, err
	}
	var out struct {
		Token           string `json:"token"`
		VerificationURL string `json:"verification_url"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
	}
	req := &request{method: http.MethodPost, path: "/v1/device-login", json: struct{}{}}
	if err := c.doJSON(ctx, req, &out); err != nil {
		return nil, err
	}
	if out.Token == "" || out.VerificationURL == "" {
		return nil, &Error{Kind: ErrInternal, Message: "device login response is missing its token or verification URL"}
	}
	verificationURL, err := resolveVerificationURL(c.apiURL, out.VerificationURL)
	if err != nil {
		return nil, err
	}
	interval := time.Duration(out.Interval) * time.Second
	if interval <= 0 {
		interval = deviceLoginDefaultInterval
	}
	if interval < deviceLoginMinInterval {
		interval = deviceLoginMinInterval
	}
	login := &DeviceLogin{VerificationURL: verificationURL, Interval: interval, client: c, token: out.Token}
	if out.ExpiresIn > 0 {
		login.ExpiresAt = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	}
	return login, nil
}

// Wait polls until the user approves the login, the login expires, or ctx
// ends, and returns the new API key. It respects the service's poll interval
// and slows down when told to. Calling Wait again after success returns the
// same result. An expired login is ErrUnauthenticated; start a new one.
func (d *DeviceLogin) Wait(ctx context.Context) (*DeviceLoginResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.result != nil {
		r := *d.result
		return &r, nil
	}

	interval := d.Interval
	if interval < deviceLoginMinInterval {
		interval = deviceLoginMinInterval
	}
	// lostReply: a poll reached the service but its answer was lost. If the
	// login was approved, that poll redeemed it and the key is gone.
	lostReply := false
	for {
		var out struct {
			Status    string `json:"status"`
			APIKey    string `json:"api_key"`
			UserEmail string `json:"user_email"`
		}
		req := &request{method: http.MethodPost, path: "/v1/device-login/poll", json: map[string]string{"token": d.token}}
		err := d.client.doJSON(ctx, req, &out)
		wait := interval
		var sdkErr *Error
		if err != nil && !errors.As(err, &sdkErr) {
			return nil, err
		}
		switch {
		case err == nil:
			switch out.Status {
			case "approved":
				if out.APIKey == "" {
					return nil, &Error{Kind: ErrInternal, Message: "device login was approved but no API key was returned"}
				}
				d.result = &DeviceLoginResult{APIKey: out.APIKey, UserEmail: out.UserEmail}
				r := *d.result
				return &r, nil
			case "expired":
				return nil, deviceLoginExpired()
			case "pending":
				// Still pending, so no earlier poll redeemed it.
				lostReply = false
			default:
				return nil, &Error{Kind: ErrInternal, Message: "device login poll returned an unknown status"}
			}
		case ctx.Err() != nil:
			return nil, err
		case errors.Is(err, ErrNotFound):
			if lostReply {
				return nil, &Error{Kind: ErrOutcomeUnknown, Message: "the device login was redeemed but the reply was lost; " +
					"revoke the unused key in the dashboard and log in again", cause: err}
			}
			return nil, &Error{Kind: ErrNotFound, Code: sdkErr.Code, Message: "device login is unknown or was already used; start again", cause: err}
		case errors.Is(err, ErrRateLimited):
			interval += deviceLoginSlowDown
			wait = max(interval, sdkErr.RetryAfter)
		case errors.Is(err, ErrOutcomeUnknown):
			lostReply = true
		case errors.Is(err, ErrUnavailable), errors.Is(err, ErrNoCapacity):
			if sdkErr.RetryAfter > wait {
				wait = sdkErr.RetryAfter
			}
		default:
			return nil, err
		}

		if !d.ExpiresAt.IsZero() && time.Now().After(d.ExpiresAt) {
			return nil, deviceLoginExpired()
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, &Error{Kind: ErrUnavailable, Message: "stopped waiting for device login approval", cause: ctx.Err()}
		case <-timer.C:
		}
	}
}

func deviceLoginExpired() *Error {
	return &Error{Kind: ErrUnauthenticated, Message: "device login expired before it was approved; start again"}
}

// newDeviceLoginClient builds a Client with no credential, for the two
// device-login calls the service accepts without one.
func newDeviceLoginClient(opts DeviceLoginOptions) (*Client, error) {
	endpoint := opts.Endpoint
	if endpoint == "" {
		endpoint = stringOrEnv(nil, "BONYA_ENDPOINT")
	}
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	apiURL, err := parseEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	hc := opts.HTTPClient
	if hc == nil {
		caBundle := opts.CABundle
		if caBundle == "" {
			caBundle = stringOrEnv(nil, "BONYA_CA_BUNDLE")
		}
		if hc, err = newHTTPClient(caBundle); err != nil {
			return nil, err
		}
	}
	return &Client{
		apiURL:     apiURL,
		edgeURL:    apiURL,
		timeout:    defaultTimeout,
		maxRetries: defaultMaxRetries,
		http:       hc,
	}, nil
}

// resolveVerificationURL makes a path-only verification URL (a service with
// no dashboard address configured) absolute against the API endpoint.
func resolveVerificationURL(base *url.URL, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		// The URL carries the login token; keep it out of the error.
		return "", &Error{Kind: ErrInternal, Message: "device login verification URL is not valid"}
	}
	if u.IsAbs() {
		return u.String(), nil
	}
	origin := url.URL{Scheme: base.Scheme, Host: base.Host}
	return origin.ResolveReference(u).String(), nil
}
