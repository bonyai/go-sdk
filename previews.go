package bonya

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// PreviewAuth is who may open a preview URL.
type PreviewAuth string

const (
	// PreviewPrivate: only holders of a grant (members of the organization
	// who opened its exchange URL) may open the preview.
	PreviewPrivate PreviewAuth = "private"
	// PreviewPublic: anyone with the URL may open the preview.
	PreviewPublic PreviewAuth = "public"
)

// Preview is an HTTPS URL that reaches one port of a sandbox. Opening it
// wakes a sandbox that paused itself when idle.
type Preview struct {
	ID              string
	SandboxID       string
	Port            int
	Auth            PreviewAuth
	URL             string
	CreatedByUserID string
	CreatedAt       time.Time
	DeletedAt       *time.Time
	// Grant is a fresh grant for the caller, set only on the Preview
	// returned by CreatePreview for a private preview.
	Grant *PreviewGrant

	client *Client
}

// PreviewGrant lets one browser open a private preview: open ExchangeURL
// once, and the browser holds a cookie for the preview until ExpiresAt.
type PreviewGrant struct {
	ID        string
	PreviewID string
	CreatedAt time.Time
	ExpiresAt time.Time
	RevokedAt *time.Time
	// Token and ExchangeURL are set only on the grant as it was created.
	// Treat them as secrets: anyone holding them can open the preview.
	Token       string
	ExchangeURL string

	client    *Client
	sandboxID string
}

type previewWire struct {
	ID              string     `json:"id"`
	SandboxID       string     `json:"sandbox_id"`
	Port            int        `json:"port"`
	Auth            string     `json:"auth"`
	URL             string     `json:"url"`
	CreatedByUserID string     `json:"created_by_user_id"`
	CreatedAt       time.Time  `json:"created_at"`
	DeletedAt       *time.Time `json:"deleted_at,omitempty"`
}

type grantWire struct {
	ID          string     `json:"id"`
	PreviewID   string     `json:"preview_id"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
	Token       string     `json:"token,omitempty"`
	ExchangeURL string     `json:"exchange_url,omitempty"`
}

func (p *Preview) apply(w previewWire) {
	p.ID = w.ID
	p.SandboxID = w.SandboxID
	p.Port = w.Port
	p.Auth = PreviewAuth(w.Auth)
	p.URL = w.URL
	p.CreatedByUserID = w.CreatedByUserID
	p.CreatedAt = w.CreatedAt
	p.DeletedAt = w.DeletedAt
}

func (g *PreviewGrant) apply(w grantWire) {
	g.ID = w.ID
	g.PreviewID = w.PreviewID
	g.CreatedAt = w.CreatedAt
	g.ExpiresAt = w.ExpiresAt
	g.RevokedAt = w.RevokedAt
	// A renew or revoke answer has no token; keep the one already held.
	if w.Token != "" {
		g.Token = w.Token
	}
	if w.ExchangeURL != "" {
		g.ExchangeURL = w.ExchangeURL
	}
}

func (s *Sandbox) newPreview(w previewWire) *Preview {
	p := &Preview{client: s.client}
	p.apply(w)
	return p
}

func (p *Preview) newGrant(w grantWire) *PreviewGrant {
	g := &PreviewGrant{client: p.client, sandboxID: p.SandboxID}
	g.apply(w)
	return g
}

func previewPath(sandboxID, previewID string, parts ...string) string {
	return sandboxPath(sandboxID, append([]string{"previews", url.PathEscape(previewID)}, parts...)...)
}

// ttlSeconds turns a grant lifetime into whole seconds (rounded up); zero
// means the service default.
func ttlSeconds(ttl time.Duration) (int64, error) {
	if ttl < 0 {
		return 0, invalidArgument("grant lifetime must not be negative")
	}
	return int64((ttl + time.Second - 1) / time.Second), nil
}

type ttlBody struct {
	TTLSeconds int64 `json:"ttl_seconds,omitempty"`
}

// PreviewOptions configures Sandbox.CreatePreview.
type PreviewOptions struct {
	// Auth defaults to PreviewPrivate. Public is only ever explicit.
	Auth PreviewAuth
	// GrantTTL is the lifetime of the caller's first grant on a private
	// preview (1 minute to the service maximum, 24 hours by default); zero
	// uses the service default (1 hour by default).
	GrantTTL time.Duration
	// IdempotencyKey makes the call safe to repeat: a second call with the
	// same key returns the first call's preview. Empty generates one per
	// call.
	IdempotencyKey string
}

// CreatePreview publishes a preview URL for port. A private preview comes
// with a first grant for the caller (Preview.Grant). One port has at most one
// live preview; a second create for it fails with ErrConflict.
func (s *Sandbox) CreatePreview(ctx context.Context, port int, opts PreviewOptions) (*Preview, error) {
	if port < 1 || port > 65535 {
		return nil, invalidArgument("port must be between 1 and 65535")
	}
	ttl, err := ttlSeconds(opts.GrantTTL)
	if err != nil {
		return nil, err
	}
	key := opts.IdempotencyKey
	if key == "" {
		key = newIdempotencyKey()
	}
	auth := opts.Auth
	if auth == "" {
		auth = PreviewPrivate
	}
	var out struct {
		previewWire
		Grant *grantWire `json:"grant,omitempty"`
	}
	if err := s.client.doJSON(ctx, &request{
		method: http.MethodPost,
		path:   sandboxPath(s.ID, "previews"),
		json: struct {
			Port       int    `json:"port"`
			Auth       string `json:"auth,omitempty"`
			TTLSeconds int64  `json:"ttl_seconds,omitempty"`
		}{port, string(auth), ttl},
		sandboxID:      s.ID,
		idempotencyKey: key,
	}, &out); err != nil {
		return nil, err
	}
	p := s.newPreview(out.previewWire)
	if out.Grant != nil {
		p.Grant = p.newGrant(*out.Grant)
	}
	return p, nil
}

// ListPreviews returns the sandbox's live previews.
func (s *Sandbox) ListPreviews(ctx context.Context) ([]*Preview, error) {
	var out struct {
		Previews []previewWire `json:"previews"`
	}
	if err := s.client.doJSON(ctx, &request{
		method:    http.MethodGet,
		path:      sandboxPath(s.ID, "previews"),
		sandboxID: s.ID,
	}, &out); err != nil {
		return nil, err
	}
	list := make([]*Preview, 0, len(out.Previews))
	for _, w := range out.Previews {
		list = append(list, s.newPreview(w))
	}
	return list, nil
}

// GetPreview fetches one of the sandbox's previews by id.
func (s *Sandbox) GetPreview(ctx context.Context, previewID string) (*Preview, error) {
	if previewID == "" {
		return nil, invalidArgument("preview id is required")
	}
	var out previewWire
	if err := s.client.doJSON(ctx, &request{
		method:    http.MethodGet,
		path:      previewPath(s.ID, previewID),
		sandboxID: s.ID,
	}, &out); err != nil {
		return nil, err
	}
	return s.newPreview(out), nil
}

// Preview returns a handle for a known preview id without a network call,
// for Delete and grant calls. Its fields other than ID and SandboxID are
// empty.
func (s *Sandbox) Preview(previewID string) *Preview {
	return &Preview{ID: previewID, SandboxID: s.ID, client: s.client}
}

// DeletePreview deletes one of the sandbox's previews; its URL stops working
// at once and its grants go with it. Deleting a deleted preview succeeds.
func (s *Sandbox) DeletePreview(ctx context.Context, previewID string) error {
	return s.Preview(previewID).Delete(ctx)
}

// Delete deletes the preview (see Sandbox.DeletePreview) and updates it.
func (p *Preview) Delete(ctx context.Context) error {
	if p.ID == "" {
		return invalidArgument("preview id is required")
	}
	var out previewWire
	if err := p.client.doJSON(ctx, &request{
		method:    http.MethodDelete,
		path:      previewPath(p.SandboxID, p.ID),
		sandboxID: p.SandboxID,
		// A repeated delete is harmless, so the call is retried.
		idempotent: true,
	}, &out); err != nil {
		return err
	}
	p.apply(out)
	return nil
}

// CreateGrant issues a new grant on a private preview for the caller, valid
// for ttl (zero uses the service default). Hand its ExchangeURL to the
// browser that should open the preview.
func (p *Preview) CreateGrant(ctx context.Context, ttl time.Duration) (*PreviewGrant, error) {
	seconds, err := ttlSeconds(ttl)
	if err != nil {
		return nil, err
	}
	var out grantWire
	if err := p.client.doJSON(ctx, &request{
		method:    http.MethodPost,
		path:      previewPath(p.SandboxID, p.ID, "grants"),
		json:      ttlBody{TTLSeconds: seconds},
		sandboxID: p.SandboxID,
	}, &out); err != nil {
		return nil, err
	}
	return p.newGrant(out), nil
}

// ListGrants returns the caller's own grants on the preview, without tokens.
func (p *Preview) ListGrants(ctx context.Context) ([]*PreviewGrant, error) {
	var out struct {
		Grants []grantWire `json:"grants"`
	}
	if err := p.client.doJSON(ctx, &request{
		method:    http.MethodGet,
		path:      previewPath(p.SandboxID, p.ID, "grants"),
		sandboxID: p.SandboxID,
	}, &out); err != nil {
		return nil, err
	}
	list := make([]*PreviewGrant, 0, len(out.Grants))
	for _, w := range out.Grants {
		list = append(list, p.newGrant(w))
	}
	return list, nil
}

// GrantHandle returns a handle for a known grant id without a network call, for
// Renew and Revoke.
func (p *Preview) GrantHandle(grantID string) *PreviewGrant {
	return &PreviewGrant{ID: grantID, PreviewID: p.ID, client: p.client, sandboxID: p.SandboxID}
}

// Renew sets the grant to expire ttl from now (zero uses the service
// default) and updates it. Only the grant's creator may renew it; a revoked
// or expired grant fails with ErrConflict.
func (g *PreviewGrant) Renew(ctx context.Context, ttl time.Duration) error {
	seconds, err := ttlSeconds(ttl)
	if err != nil {
		return err
	}
	var out grantWire
	if err := g.client.doJSON(ctx, &request{
		method:    http.MethodPost,
		path:      previewPath(g.sandboxID, g.PreviewID, "grants", url.PathEscape(g.ID), "renew"),
		json:      ttlBody{TTLSeconds: seconds},
		sandboxID: g.sandboxID,
		// Renewing twice sets the same kind of expiry, so the call is
		// retried.
		idempotent: true,
	}, &out); err != nil {
		return err
	}
	g.apply(out)
	return nil
}

// Revoke ends the grant at once and updates it. Only its creator may revoke
// it; revoking a revoked grant succeeds.
func (g *PreviewGrant) Revoke(ctx context.Context) error {
	var out grantWire
	if err := g.client.doJSON(ctx, &request{
		method:    http.MethodDelete,
		path:      previewPath(g.sandboxID, g.PreviewID, "grants", url.PathEscape(g.ID)),
		sandboxID: g.sandboxID,
		// A repeated revoke is harmless, so the call is retried.
		idempotent: true,
	}, &out); err != nil {
		return err
	}
	g.apply(out)
	return nil
}
