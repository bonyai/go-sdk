package bonya

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// SnapshotState is a named snapshot's state.
type SnapshotState string

const (
	// SnapshotWriting: the snapshot's files are being written.
	SnapshotWriting SnapshotState = "writing"
	// SnapshotCommitted: the files are stored but not yet visible.
	SnapshotCommitted SnapshotState = "committed"
	// SnapshotPublished: the snapshot can be restored with Client.Create.
	SnapshotPublished SnapshotState = "published"
	// SnapshotDeleted: the snapshot was deleted, or its write was given up.
	SnapshotDeleted SnapshotState = "deleted"
)

// Snapshot is a named snapshot of a sandbox's files (no memory). Restore it
// with Client.Create and CreateOptions.Snapshot.
type Snapshot struct {
	ID             string
	OrganizationID string
	// SandboxID is the sandbox the snapshot was taken from.
	SandboxID       string
	Kind            string
	Name            string
	State           SnapshotState
	TemplateVersion string
	// Image is the source image and resolved digest; nil for the default template.
	Image          *Image
	ManifestDigest string
	// FileBytes is the size of the captured files; zero until published.
	FileBytes int64
	// StoredBytes is what the snapshot adds to storage; zero until published.
	StoredBytes   int64
	FailureReason string
	CreatedAt     time.Time
	PublishedAt   *time.Time
	DeletedAt     *time.Time

	client *Client
}

// snapshotWire is the service's snapshot object.
type snapshotWire struct {
	ID              string     `json:"id"`
	OrganizationID  string     `json:"organization_id"`
	SandboxID       string     `json:"sandbox_id"`
	Kind            string     `json:"kind"`
	Name            string     `json:"name"`
	State           string     `json:"state"`
	TemplateVersion string     `json:"template_version"`
	Image           *Image     `json:"image,omitempty"`
	ManifestDigest  string     `json:"manifest_digest,omitempty"`
	FileBytes       *int64     `json:"file_bytes,omitempty"`
	StoredBytes     *int64     `json:"stored_bytes,omitempty"`
	FailureReason   string     `json:"failure_reason,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	PublishedAt     *time.Time `json:"published_at,omitempty"`
	DeletedAt       *time.Time `json:"deleted_at,omitempty"`
}

func (c *Client) newSnapshot(w snapshotWire) *Snapshot {
	p := &Snapshot{client: c}
	p.apply(w)
	return p
}

func (p *Snapshot) apply(w snapshotWire) {
	p.ID = w.ID
	p.OrganizationID = w.OrganizationID
	p.SandboxID = w.SandboxID
	p.Kind = w.Kind
	p.Name = w.Name
	p.State = SnapshotState(w.State)
	p.TemplateVersion = w.TemplateVersion
	p.Image = w.Image
	p.ManifestDigest = w.ManifestDigest
	p.FileBytes = derefInt64(w.FileBytes)
	p.StoredBytes = derefInt64(w.StoredBytes)
	p.FailureReason = w.FailureReason
	p.CreatedAt = w.CreatedAt
	p.PublishedAt = w.PublishedAt
	p.DeletedAt = w.DeletedAt
}

func snapshotPath(id string) string { return "/v1/snapshot/" + url.PathEscape(id) }

// SnapshotOptions configures Sandbox.CreateSnapshot.
type SnapshotOptions struct {
	// Name is required (at most 128 characters).
	Name string
	// IdempotencyKey makes the call safe to repeat: a second call with the
	// same key returns, or finishes, the first call's snapshot. Empty
	// generates one per call.
	IdempotencyKey string
}

// CreateSnapshot takes a named snapshot of the sandbox's files and returns
// once it is published. The sandbox keeps running (or stays paused). When an
// earlier write with the same key is still running elsewhere, the SDK
// repeats the call with that key until the snapshot is published or ctx
// ends. A write the service gave up is returned as ErrConflict.
func (s *Sandbox) CreateSnapshot(ctx context.Context, opts SnapshotOptions) (*Snapshot, error) {
	if opts.Name == "" {
		return nil, invalidArgument("snapshot name is required")
	}
	key := opts.IdempotencyKey
	if key == "" {
		key = newIdempotencyKey()
	}
	ctx, cancel := withLongTimeout(ctx)
	defer cancel()
	wait := pollStart
	for {
		var out snapshotWire
		if err := s.client.doJSON(ctx, &request{
			method: http.MethodPost,
			path:   sandboxPath(s.ID, "snapshots"),
			json: struct {
				Name string `json:"name"`
			}{opts.Name},
			sandboxID:      s.ID,
			idempotencyKey: key,
		}, &out); err != nil {
			return nil, err
		}
		switch SnapshotState(out.State) {
		case SnapshotPublished:
			return s.client.newSnapshot(out), nil
		case SnapshotWriting, SnapshotCommitted:
			// Still being written by another call; repeating with the same
			// key waits for it or takes it over.
		default:
			msg := "the snapshot was not published (state " + out.State + ")"
			if out.FailureReason != "" {
				msg += ": " + out.FailureReason
			}
			return nil, &Error{Kind: ErrConflict, Message: msg, SandboxID: s.ID}
		}
		if err := sleepCtx(ctx, wait); err != nil {
			return nil, &Error{Kind: ErrUnavailable, Message: "stopped waiting for the snapshot " + out.ID, SandboxID: s.ID, cause: err}
		}
		wait = min(wait*2, pollMax)
	}
}

// ListSnapshots returns the sandbox's named snapshots that are not deleted,
// newest first (at most 500).
func (s *Sandbox) ListSnapshots(ctx context.Context) ([]*Snapshot, error) {
	var out struct {
		Snapshots []snapshotWire `json:"snapshots"`
	}
	if err := s.client.doJSON(ctx, &request{
		method:    http.MethodGet,
		path:      sandboxPath(s.ID, "snapshots"),
		sandboxID: s.ID,
	}, &out); err != nil {
		return nil, err
	}
	list := make([]*Snapshot, 0, len(out.Snapshots))
	for _, w := range out.Snapshots {
		list = append(list, s.client.newSnapshot(w))
	}
	return list, nil
}

// GetSnapshot fetches a named snapshot by id.
func (c *Client) GetSnapshot(ctx context.Context, id string) (*Snapshot, error) {
	if id == "" {
		return nil, invalidArgument("snapshot id is required")
	}
	var out snapshotWire
	if err := c.doJSON(ctx, &request{method: http.MethodGet, path: snapshotPath(id)}, &out); err != nil {
		return nil, err
	}
	return c.newSnapshot(out), nil
}

// DeleteSnapshot deletes a named snapshot by id. Deleting a deleted snapshot
// succeeds. It fails with ErrSnapshotInUse while a sandbox created from it
// exists, and with ErrConflict while it is still being written.
func (c *Client) DeleteSnapshot(ctx context.Context, id string) error {
	_, err := c.deleteSnapshot(ctx, id)
	return err
}

// Delete deletes the snapshot (see Client.DeleteSnapshot) and updates it.
func (p *Snapshot) Delete(ctx context.Context) error {
	out, err := p.client.deleteSnapshot(ctx, p.ID)
	if err != nil {
		return err
	}
	p.apply(out)
	return nil
}

func (c *Client) deleteSnapshot(ctx context.Context, id string) (snapshotWire, error) {
	var out snapshotWire
	if id == "" {
		return out, invalidArgument("snapshot id is required")
	}
	err := c.doJSON(ctx, &request{
		method: http.MethodDelete,
		path:   snapshotPath(id),
		// A repeated delete is harmless, so the call is retried.
		idempotent: true,
	}, &out)
	return out, err
}

func derefInt64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}
