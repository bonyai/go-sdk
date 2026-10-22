package bonya

import (
	"context"
	"iter"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// longCallTimeout bounds calls that wait for the service to finish real work
// (create, pause, snapshot, fork) when the caller's context has no deadline.
// The client's default timeout is meant for quick calls.
const longCallTimeout = 5 * time.Minute

// Wait intervals for WaitRunning and for repeating an unfinished snapshot or
// fork.
const (
	pollStart = 250 * time.Millisecond
	pollMax   = 2 * time.Second
)

// CreateOptions configures Client.Create. The zero value creates a sandbox
// from the default template with a generated name.
type CreateOptions struct {
	// Name is the sandbox's friendly name; empty generates one.
	Name string
	// Snapshot creates the sandbox from this named snapshot id instead of
	// the default template (a restore). The new sandbox boots the snapshot's
	// files with fresh processes.
	Snapshot string
	// IdempotencyKey makes the create safe to repeat: a second call with the
	// same key returns the first call's sandbox. Empty generates one per
	// call, which the SDK keeps across its own retries.
	IdempotencyKey string
}

// Create starts a sandbox and returns when it is running. On a repeat (same
// IdempotencyKey) whose first call is still starting it, the returned
// sandbox is still creating; call WaitRunning. A sandbox that failed to
// start is returned as an ErrUnavailable error; retry with a new key.
func (c *Client) Create(ctx context.Context, opts CreateOptions) (*Sandbox, error) {
	key := opts.IdempotencyKey
	if key == "" {
		key = newIdempotencyKey()
	}
	body := struct {
		Snapshot string `json:"snapshot,omitempty"`
		Name     string `json:"name,omitempty"`
	}{Snapshot: opts.Snapshot, Name: opts.Name}
	ctx, cancel := withLongTimeout(ctx)
	defer cancel()
	var out sandboxWire
	if err := c.doJSON(ctx, &request{
		method:         http.MethodPost,
		path:           "/v1/sandbox",
		json:           body,
		idempotencyKey: key,
	}, &out); err != nil {
		return nil, err
	}
	// The service answers a repeat of a create that failed with the failed
	// sandbox; the SDK's own retries reuse the key, so this is how a failed
	// start reaches the caller.
	if State(out.State) == StateFailed {
		msg := "the sandbox could not be started; retry with a new idempotency key"
		if out.FailureReason != "" {
			msg += " (" + out.FailureReason + ")"
		}
		return nil, &Error{Kind: ErrUnavailable, Message: msg, SandboxID: out.ID}
	}
	return c.newSandbox(out), nil
}

// Get fetches a sandbox by id.
func (c *Client) Get(ctx context.Context, id string) (*Sandbox, error) {
	if id == "" {
		return nil, invalidArgument("sandbox id is required")
	}
	var out sandboxWire
	if err := c.doJSON(ctx, &request{
		method:    http.MethodGet,
		path:      sandboxPath(id),
		sandboxID: id,
	}, &out); err != nil {
		return nil, err
	}
	return c.newSandbox(out), nil
}

// Refresh reloads the sandbox's fields from the service.
func (s *Sandbox) Refresh(ctx context.Context) error {
	var out sandboxWire
	if err := s.client.doJSON(ctx, &request{
		method:    http.MethodGet,
		path:      sandboxPath(s.ID),
		sandboxID: s.ID,
	}, &out); err != nil {
		return err
	}
	s.apply(out)
	return nil
}

// ListOptions configures Client.List and Client.ListAll.
type ListOptions struct {
	// States keeps only sandboxes in these states; empty lists every state
	// except deleted (deleted sandboxes are never listed).
	States []State
	// PageSize is the page length (1 to 100); zero uses the service default.
	PageSize int
	// PageToken continues a listing from a previous page's NextPageToken.
	PageToken string
}

// SandboxPage is one page of a listing.
type SandboxPage struct {
	// Sandboxes is newest first.
	Sandboxes []*Sandbox
	// NextPageToken continues the listing; empty on the last page.
	NextPageToken string
}

// List returns one page of the organization's sandboxes, newest first.
func (c *Client) List(ctx context.Context, opts ListOptions) (*SandboxPage, error) {
	if opts.PageSize < 0 {
		return nil, invalidArgument("page size must not be negative")
	}
	query := url.Values{}
	if opts.PageSize > 0 {
		query.Set("page_size", strconv.Itoa(opts.PageSize))
	}
	if opts.PageToken != "" {
		query.Set("page_token", opts.PageToken)
	}
	for _, state := range opts.States {
		query.Add("states", string(state))
	}
	var out struct {
		Sandboxes     []sandboxWire `json:"sandboxes"`
		NextPageToken string        `json:"next_page_token"`
	}
	if err := c.doJSON(ctx, &request{
		method: http.MethodGet,
		path:   "/v1/sandbox",
		query:  query,
	}, &out); err != nil {
		return nil, err
	}
	page := &SandboxPage{Sandboxes: make([]*Sandbox, 0, len(out.Sandboxes)), NextPageToken: out.NextPageToken}
	for _, w := range out.Sandboxes {
		page.Sandboxes = append(page.Sandboxes, c.newSandbox(w))
	}
	return page, nil
}

// ListAll iterates over every page of a listing, starting at
// opts.PageToken. It stops after the first error, which it yields once.
//
//	for sb, err := range client.ListAll(ctx, bonya.ListOptions{}) {
//		if err != nil { return err }
//		...
//	}
func (c *Client) ListAll(ctx context.Context, opts ListOptions) iter.Seq2[*Sandbox, error] {
	return func(yield func(*Sandbox, error) bool) {
		for {
			page, err := c.List(ctx, opts)
			if err != nil {
				yield(nil, err)
				return
			}
			for _, sb := range page.Sandboxes {
				if !yield(sb, nil) {
					return
				}
			}
			if page.NextPageToken == "" {
				return
			}
			opts.PageToken = page.NextPageToken
		}
	}
}

// Delete deletes the sandbox and updates the handle (State becomes deleted).
// Deleting a deleted sandbox succeeds. If the node does not confirm in time
// the SDK retries; a returned ErrUnavailable means the delete is still in
// progress and calling Delete again finishes it.
func (s *Sandbox) Delete(ctx context.Context) error {
	var out sandboxWire
	if err := s.client.doJSON(ctx, &request{
		method:    http.MethodDelete,
		path:      sandboxPath(s.ID),
		sandboxID: s.ID,
		// A repeated delete is harmless, so the call is retried.
		idempotent: true,
	}, &out); err != nil {
		return err
	}
	s.apply(out)
	return nil
}

// PauseStage is how deep Pause puts a sandbox to sleep.
type PauseStage string

const (
	// PauseWarm keeps the sandbox's memory on its node: the fastest wake.
	PauseWarm PauseStage = "warm"
	// PauseCold saves the sandbox to storage and frees its node: no running
	// cost, a slower wake with fresh processes.
	PauseCold PauseStage = "cold"
)

// Pause pauses the sandbox at stage now (no idle check) and returns when the
// stage is reached. Pausing a sandbox already at that stage succeeds. A
// sandbox paused this way is not woken by exec or preview traffic; wake it
// with Keepalive.
func (s *Sandbox) Pause(ctx context.Context, stage PauseStage) error {
	if stage != PauseWarm && stage != PauseCold {
		return invalidArgument(`pause stage must be "warm" or "cold"`)
	}
	ctx, cancel := withLongTimeout(ctx)
	defer cancel()
	var out sandboxWire
	if err := s.client.doJSON(ctx, &request{
		method: http.MethodPost,
		path:   sandboxPath(s.ID, "pause"),
		json: struct {
			Stage string `json:"stage"`
		}{string(stage)},
		sandboxID: s.ID,
		// A repeated pause is harmless, so the call is retried.
		idempotent: true,
	}, &out); err != nil {
		return err
	}
	s.apply(out)
	return nil
}

// Keepalive holds the sandbox awake for d (no automatic pause until then)
// and returns the time the hold ends. A later call never shortens a hold. A
// paused sandbox is woken: the wake runs in the background and the handle's
// State is then waking; call WaitRunning before using it.
func (s *Sandbox) Keepalive(ctx context.Context, d time.Duration) (time.Time, error) {
	if d <= 0 {
		return time.Time{}, invalidArgument("keepalive duration must be positive")
	}
	var out struct {
		sandboxWire
		KeepaliveUntil time.Time `json:"keepalive_until"`
	}
	if err := s.client.doJSON(ctx, &request{
		method: http.MethodPost,
		path:   sandboxPath(s.ID, "keepalive"),
		json: struct {
			For string `json:"for"`
		}{d.String()},
		sandboxID: s.ID,
		// A repeated keepalive is harmless, so the call is retried
		// (for example while no node has room for the wake).
		idempotent: true,
	}, &out); err != nil {
		return time.Time{}, err
	}
	s.apply(out.sandboxWire)
	return out.KeepaliveUntil, nil
}

// WaitRunning polls until the sandbox is running, for example after a
// Keepalive that woke it or a repeated Create. It returns ErrConflict when
// the sandbox reaches a state it cannot run from (failed, paused, deleted),
// and the context's error wrapped in an *Error when ctx ends first.
func (s *Sandbox) WaitRunning(ctx context.Context) error {
	wait := pollStart
	for {
		switch s.State {
		case StateRunning:
			return nil
		case StateCreating, StateWaking, StateCooling, "":
		default:
			msg := "the sandbox is " + string(s.State) + " and will not become running"
			if s.FailureReason != "" {
				msg += " (" + s.FailureReason + ")"
			}
			if s.State == StateWarm || s.State == StateCold {
				msg += "; Keepalive wakes it"
			}
			return &Error{Kind: ErrConflict, Message: msg, SandboxID: s.ID}
		}
		if err := sleepCtx(ctx, wait); err != nil {
			return &Error{Kind: ErrUnavailable, Message: "stopped waiting for the sandbox to run", SandboxID: s.ID, cause: err}
		}
		wait = min(wait*2, pollMax)
		if err := s.Refresh(ctx); err != nil {
			return err
		}
	}
}

// withLongTimeout gives a call that waits for real work a longer default
// deadline than the client's.
func withLongTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, longCallTimeout)
}

// sleepCtx waits for d or until ctx ends, returning ctx's error then.
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
