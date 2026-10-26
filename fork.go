package bonya

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// MaxForkCount is the most children one Fork call may create.
const MaxForkCount = 16

// ForkOptions configures Sandbox.Fork.
type ForkOptions struct {
	// Count is how many children to create (1 to MaxForkCount); zero means 1.
	Count int
	// IdempotencyKey makes the call safe to repeat: a second call with the
	// same key returns, or finishes, the first call's children. Empty
	// generates one per call.
	IdempotencyKey string
}

// ForkResult is the outcome of a fork.
type ForkResult struct {
	// SourceSandboxID is the sandbox that was forked.
	SourceSandboxID string
	// FromWarm reports that the source was paused warm when it was forked.
	FromWarm bool
	// Sandboxes are the children. Each is running unless it failed to
	// start; check every child's State.
	Sandboxes []*Sandbox
}

// Fork creates Count new sandboxes with the source's memory and files, on
// the source's node; the source keeps running (or stays warm). It returns
// when no child is still starting: when an earlier call with the same key is
// still running elsewhere, the SDK repeats the call with that key until it
// finishes or ctx ends. A child that failed to start is returned with State
// failed rather than as an error. A cold source must be woken first
// (Keepalive).
func (s *Sandbox) Fork(ctx context.Context, opts ForkOptions) (*ForkResult, error) {
	count := opts.Count
	if count == 0 {
		count = 1
	}
	if count < 1 || count > MaxForkCount {
		return nil, invalidArgument("fork count must be between 1 and 16")
	}
	key := opts.IdempotencyKey
	if key == "" {
		key = newIdempotencyKey()
	}
	ctx, cancel := withLongTimeout(ctx)
	defer cancel()
	wait := pollStart
	for {
		var out struct {
			SandboxID string        `json:"sandbox_id"`
			FromWarm  bool          `json:"from_warm"`
			Sandboxes []sandboxWire `json:"sandboxes"`
		}
		if err := s.client.doJSON(ctx, &request{
			method: http.MethodPost,
			path:   sandboxPath(s.ID, "fork"),
			json: struct {
				Count int `json:"count"`
			}{count},
			sandboxID:      s.ID,
			idempotencyKey: key,
		}, &out); err != nil {
			var serviceErr *Error
			if errors.As(err, &serviceErr) && serviceErr.Code == "lifecycle_conflict" &&
				strings.Contains(serviceErr.Message, "forking a gVisor sandbox is not supported") {
				serviceErr.Message = "forking an image-based sandbox is not supported yet; create a named snapshot and new sandboxes from it"
			}
			return nil, err
		}
		unfinished := len(out.Sandboxes) == 0
		for _, child := range out.Sandboxes {
			if State(child.State) == StateCreating {
				unfinished = true
			}
		}
		if !unfinished {
			result := &ForkResult{SourceSandboxID: out.SandboxID, FromWarm: out.FromWarm,
				Sandboxes: make([]*Sandbox, 0, len(out.Sandboxes))}
			for _, child := range out.Sandboxes {
				result.Sandboxes = append(result.Sandboxes, s.client.newSandbox(child))
			}
			return result, nil
		}
		// A repeat whose first fork is still running; repeating with the
		// same key waits for it or takes it over.
		if err := sleepCtx(ctx, wait); err != nil {
			return nil, &Error{Kind: ErrUnavailable, Message: "stopped waiting for the fork to finish", SandboxID: s.ID, cause: err}
		}
		wait = min(wait*2, pollMax)
	}
}
