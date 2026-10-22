package bonya

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	idempotencyHeader  = "Idempotency-Key"
	organizationHeader = "X-Bonya-Organization-ID"
	maxErrorBodyBytes  = 64 << 10
	retryBackoffStart  = 200 * time.Millisecond
	retryBackoffMax    = 2 * time.Second
)

// request describes one HTTP call. A request is repeatable (and so retried
// after transient failures) when it is a GET or carries an idempotency key,
// and its body can be produced again.
type request struct {
	method string
	path   string // already escaped, e.g. "/v1/sandbox/" + url.PathEscape(id)
	query  url.Values

	// json is encoded as the body when non-nil.
	json any
	// body produces a raw body; it is called again for each attempt, so a
	// request with a body that cannot be produced twice must not set it to a
	// function that returns the same reader.
	body        func() (io.Reader, error)
	contentType string

	// edge sends the call to the edge endpoint instead of the API.
	edge bool
	// bearer replaces the client credential (a sandbox capability). The
	// organization header is then omitted; the token is bound to its sandbox.
	bearer string
	// idempotencyKey makes a mutation safe to resend.
	idempotencyKey string
	// sandboxID is attached to errors.
	sandboxID string
	// stream: the caller reads the body; no default timeout is applied.
	stream bool
	// singleShot: a body is sent that cannot be produced again; never retry
	// after sending it.
	singleShot bool
}

func (r *request) repeatable() bool {
	if r.singleShot {
		return false
	}
	return r.method == http.MethodGet || r.method == http.MethodHead || r.idempotencyKey != ""
}

// newIdempotencyKey returns a random key for one logical mutation. Reuse it
// for every retry of that mutation.
func newIdempotencyKey() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// doJSON performs req and decodes a JSON success body into out (when non-nil).
func (c *Client) doJSON(ctx context.Context, req *request, out any) error {
	resp, err := c.do(ctx, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return &Error{Kind: ErrInternal, Message: "decode response: " + err.Error(), SandboxID: req.sandboxID, cause: err}
	}
	return nil
}

// do performs req with retries and returns a 2xx response whose body the
// caller must close. Any failure is an *Error.
func (c *Client) do(ctx context.Context, req *request) (*http.Response, error) {
	ctx, cancel := c.withDefaultTimeout(ctx, req.stream)
	provisioned := false
	for attempt := 0; ; attempt++ {
		resp, err := c.send(ctx, req)
		if err == nil {
			resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
			return resp, nil
		}
		var sdkErr *Error
		if !errors.As(err, &sdkErr) {
			cancel()
			return nil, err
		}
		// A login token whose user was never provisioned: listing
		// organizations provisions it, then the call can run (once).
		if sdkErr.Code == "unknown_identity" && !provisioned && req.bearer == "" {
			provisioned = true
			if perr := c.provision(ctx); perr == nil {
				attempt--
				continue
			}
		}
		if attempt >= c.maxRetries || !req.repeatable() || !retryable(sdkErr) {
			cancel()
			return nil, err
		}
		if !sleepBeforeRetry(ctx, sdkErr.RetryAfter, attempt) {
			cancel()
			return nil, err
		}
	}
}

func (c *Client) provision(ctx context.Context) error {
	resp, err := c.send(ctx, &request{method: http.MethodGet, path: "/v1/organizations"})
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}

// send performs one attempt.
func (c *Client) send(ctx context.Context, req *request) (*http.Response, error) {
	httpReq, err := c.newHTTPRequest(ctx, req)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, transportError(ctx, req, err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	return nil, responseError(resp, req.sandboxID)
}

func (c *Client) newHTTPRequest(ctx context.Context, req *request) (*http.Request, error) {
	base := c.apiURL
	if req.edge {
		base = c.edgeURL
	}
	u := *base
	unescaped, err := url.PathUnescape(req.path)
	if err != nil {
		return nil, invalidArgument("request path is not valid: " + err.Error())
	}
	u.Path = base.Path + unescaped
	u.RawPath = base.EscapedPath() + req.path
	if len(req.query) > 0 {
		u.RawQuery = req.query.Encode()
	}

	var body io.Reader
	contentType := req.contentType
	switch {
	case req.json != nil:
		encoded, err := json.Marshal(req.json)
		if err != nil {
			return nil, invalidArgument("encode request: " + err.Error())
		}
		body = bytes.NewReader(encoded)
		contentType = "application/json"
	case req.body != nil:
		b, err := req.body()
		if err != nil {
			return nil, &Error{Kind: ErrInvalidRequest, Message: "open request body", SandboxID: req.sandboxID, cause: err}
		}
		body = b
	}

	httpReq, err := http.NewRequestWithContext(ctx, req.method, u.String(), body)
	if err != nil {
		return nil, invalidArgument("build request: " + err.Error())
	}
	if contentType != "" {
		httpReq.Header.Set("Content-Type", contentType)
	}
	httpReq.Header.Set("User-Agent", userAgent)
	httpReq.Header.Set("Accept", "application/json")
	if req.bearer != "" {
		httpReq.Header.Set("Authorization", "Bearer "+req.bearer)
	} else {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
		if c.organizationID != "" {
			httpReq.Header.Set(organizationHeader, c.organizationID)
		}
	}
	if req.idempotencyKey != "" {
		httpReq.Header.Set(idempotencyHeader, req.idempotencyKey)
	}
	return httpReq, nil
}

// problem is the service's RFC 9457 error body.
type problem struct {
	Code   string `json:"code"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
}

// responseError turns a non-2xx response into an *Error.
func responseError(resp *http.Response, sandboxID string) *Error {
	e := &Error{SandboxID: sandboxID, RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	var p problem
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if (mediaType == "application/problem+json" || mediaType == "application/json") && json.Unmarshal(raw, &p) == nil && p.Code != "" {
		e.Code = p.Code
		e.Message = p.Detail
		if e.Message == "" {
			e.Message = p.Title
		}
		if kind, ok := codeKinds[p.Code]; ok {
			e.Kind = kind
			return e
		}
	}
	e.Kind = kindForStatus(resp.StatusCode)
	if e.Message == "" {
		e.Message = strings.ToLower(http.StatusText(resp.StatusCode))
	}
	return e
}

// transportError classifies a failure to get any response. A dial failure
// means nothing was sent; otherwise a non-repeatable request may have run.
func transportError(ctx context.Context, req *request, err error) *Error {
	e := &Error{SandboxID: req.sandboxID, cause: err}
	if ctxErr := ctx.Err(); ctxErr != nil {
		e.cause = ctxErr
	}
	var opErr *net.OpError
	notSent := errors.As(err, &opErr) && opErr.Op == "dial"
	switch {
	case notSent || req.repeatable():
		e.Kind = ErrUnavailable
		e.Message = "could not reach the service: " + err.Error()
	default:
		e.Kind = ErrOutcomeUnknown
		e.Message = "connection failed after the request was sent: " + err.Error()
	}
	return e
}

func retryable(e *Error) bool {
	if e.cause != nil && (errors.Is(e.cause, context.Canceled) || errors.Is(e.cause, context.DeadlineExceeded)) {
		return false
	}
	return e.Kind == ErrUnavailable || e.Kind == ErrNoCapacity || e.Kind == ErrRateLimited
}

// sleepBeforeRetry waits for the server's Retry-After, or an exponential
// backoff. It reports false when the context ends first or the wait would
// pass its deadline.
func sleepBeforeRetry(ctx context.Context, retryAfter time.Duration, attempt int) bool {
	wait := retryAfter
	if wait <= 0 {
		wait = retryBackoffStart << attempt
		if wait > retryBackoffMax {
			wait = retryBackoffMax
		}
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < wait {
		return false
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func parseRetryAfter(value string) time.Duration {
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		if d := time.Until(at); d > 0 {
			return d
		}
	}
	return 0
}

func (c *Client) withDefaultTimeout(ctx context.Context, stream bool) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok || stream {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, c.timeout)
}

// cancelOnClose releases the call's context when the body is closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelOnClose) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}
