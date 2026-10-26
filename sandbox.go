package bonya

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// State is a sandbox lifecycle state.
type State string

const (
	StateCreating State = "creating"
	StateRunning  State = "running"
	StateCooling  State = "cooling"
	StateWarm     State = "warm"
	StateCold     State = "cold"
	StateWaking   State = "waking"
	StateFailed   State = "failed"
	StateDeleting State = "deleting"
	StateDeleted  State = "deleted"
	StateLost     State = "lost"
)

// Sandbox is a handle to one sandbox. Its fields are the state the service
// last returned; Refresh updates them. A Sandbox is safe for concurrent use
// once returned, but its fields are not updated concurrently-safely: call
// Refresh from one goroutine.
type Sandbox struct {
	ID              string
	Name            string
	OrganizationID  string
	TemplateVersion string
	// Image is the image source and resolved digest; nil for the default template.
	Image            *Image
	State            State
	Generation       int64
	VCPUs            int32
	MemoryMiB        int64
	UserLayerBytes   int64
	FailureReason    string
	CreatedAt        time.Time
	StartedAt        *time.Time
	DeletedAt        *time.Time
	ParentSnapshotID string
	PauseSource      string

	client *Client
	cap    *capability
}

// Image identifies a sandbox's or snapshot's public container image.
type Image struct {
	// Reference is the normalized image reference supplied at creation.
	Reference string `json:"reference"`
	// Digest is the resolved immutable manifest digest, once the image is resolved.
	Digest string `json:"digest,omitempty"`
}

// sandboxWire is the service's sandbox object.
type sandboxWire struct {
	ID                string     `json:"id"`
	Name              string     `json:"name"`
	OrganizationID    string     `json:"organization_id"`
	TemplateVersion   string     `json:"template_version"`
	Image             *Image     `json:"image,omitempty"`
	State             string     `json:"state"`
	Generation        int64      `json:"generation"`
	VCPUs             int32      `json:"vcpus"`
	MemoryMiB         int64      `json:"memory_mib"`
	UserLayerBytes    int64      `json:"user_layer_bytes"`
	FailureReason     string     `json:"failure_reason,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	StartedAt         *time.Time `json:"started_at,omitempty"`
	DeletedAt         *time.Time `json:"deleted_at,omitempty"`
	ParentSnapshotID  string     `json:"parent_snapshot_id,omitempty"`
	PauseSource       string     `json:"pause_source,omitempty"`
	ExecCapabilityJWS string     `json:"exec_capability_jws,omitempty"`
}

// newSandbox builds a handle from a service response.
func (c *Client) newSandbox(w sandboxWire) *Sandbox {
	s := &Sandbox{client: c, cap: &capability{}}
	s.apply(w)
	return s
}

// apply copies a service response into the handle and keeps a newer
// capability when the response carries one.
func (s *Sandbox) apply(w sandboxWire) {
	s.ID = w.ID
	s.Name = w.Name
	s.OrganizationID = w.OrganizationID
	s.TemplateVersion = w.TemplateVersion
	s.Image = w.Image
	s.State = State(w.State)
	s.Generation = w.Generation
	s.VCPUs = w.VCPUs
	s.MemoryMiB = w.MemoryMiB
	s.UserLayerBytes = w.UserLayerBytes
	s.FailureReason = w.FailureReason
	s.CreatedAt = w.CreatedAt
	s.StartedAt = w.StartedAt
	s.DeletedAt = w.DeletedAt
	s.ParentSnapshotID = w.ParentSnapshotID
	s.PauseSource = w.PauseSource
	if w.ExecCapabilityJWS != "" {
		s.cap.set(w.ExecCapabilityJWS, tokenExpiry(w.ExecCapabilityJWS))
	}
}

// Sandbox returns a handle for a known sandbox id without a network call.
// Its fields other than ID are empty until Refresh.
func (c *Client) Sandbox(id string) *Sandbox {
	return &Sandbox{ID: id, client: c, cap: &capability{}}
}

func sandboxPath(id string, parts ...string) string {
	p := "/v1/sandbox/" + url.PathEscape(id)
	for _, part := range parts {
		p += "/" + part
	}
	return p
}

// Capability is a short-lived token that lets its holder run commands, use
// files and terminal sessions in one sandbox, and nothing else. Hand it to a
// browser or another process instead of an API key.
type Capability struct {
	Token               string
	ExpiresAt           time.Time
	AuthorityGeneration int64
}

// Capability issues a fresh capability for the sandbox. The handle also
// starts using it for its own exec, files and sessions calls.
func (s *Sandbox) Capability(ctx context.Context) (*Capability, error) {
	var out struct {
		CapabilityJWS       string    `json:"capability_jws"`
		ExpiresAt           time.Time `json:"expires_at"`
		AuthorityGeneration int64     `json:"authority_generation"`
	}
	err := s.client.doJSON(ctx, &request{
		method:    http.MethodPost,
		path:      sandboxPath(s.ID, "capability"),
		sandboxID: s.ID,
	}, &out)
	if err != nil {
		return nil, err
	}
	s.cap.set(out.CapabilityJWS, out.ExpiresAt)
	return &Capability{Token: out.CapabilityJWS, ExpiresAt: out.ExpiresAt, AuthorityGeneration: out.AuthorityGeneration}, nil
}

// edgeDo performs an exec, files or sessions call. It authenticates with the
// sandbox capability when it has one, and the client credential otherwise.
// A refused capability (expired, or outdated after a wake) is refreshed once
// and the call repeated: the edge refuses capabilities at admission, before
// any effect, so the repeat is safe whenever the body can be produced again.
// When the service cannot issue capabilities the handle falls back to the
// client credential for good.
func (s *Sandbox) edgeDo(ctx context.Context, req *request) (*http.Response, error) {
	req.edge = true
	req.sandboxID = s.ID
	token := s.cap.usable(ctx, s)
	req.bearer = token
	resp, err := s.client.do(ctx, req)
	if err == nil || token == "" || !capabilityRefused(err) || req.singleShot {
		return resp, err
	}
	s.cap.invalidate(token)
	req.bearer = s.cap.usable(ctx, s)
	return s.client.do(ctx, req)
}

// edgeJSON is edgeDo with a JSON success body decoded into out (when non-nil).
func (s *Sandbox) edgeJSON(ctx context.Context, req *request, out any) error {
	resp, err := s.edgeDo(ctx, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return &Error{Kind: ErrInternal, Message: "decode response: " + err.Error(), SandboxID: s.ID, cause: err}
	}
	return nil
}

// edgeBearer returns the credential for a WebSocket dial (capability when
// available), and whether it is a capability.
func (s *Sandbox) edgeBearer(ctx context.Context) (string, bool) {
	if token := s.cap.usable(ctx, s); token != "" {
		return token, true
	}
	return s.client.apiKey, false
}

func capabilityRefused(err error) bool {
	var e *Error
	return errors.As(err, &e) && (e.Code == "capability_expired" || e.Code == "forbidden")
}

// capabilityRefreshMargin: a capability this close to expiry is replaced
// before use.
const capabilityRefreshMargin = time.Minute

// capability holds a sandbox's current token. Shared by copies of a handle.
type capability struct {
	mu        sync.Mutex
	token     string
	expiresAt time.Time
	disabled  bool // the service does not issue capabilities
}

func (c *capability) set(token string, expiresAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token, c.expiresAt = token, expiresAt
}

func (c *capability) invalidate(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == token {
		c.token = ""
	}
}

// usable returns a token valid for at least the refresh margin, issuing a new
// one when the handle had one that expired or was refused. It returns "" when
// the client credential should be used: the handle never had a capability, or
// the service cannot issue them.
func (c *capability) usable(ctx context.Context, s *Sandbox) string {
	c.mu.Lock()
	if c.disabled {
		c.mu.Unlock()
		return ""
	}
	if c.token != "" && (c.expiresAt.IsZero() || time.Until(c.expiresAt) > capabilityRefreshMargin) {
		token := c.token
		c.mu.Unlock()
		return token
	}
	hadToken := c.token != "" || !c.expiresAt.IsZero()
	c.mu.Unlock()
	if !hadToken {
		return ""
	}
	if _, err := s.Capability(ctx); err != nil {
		// Use the client credential for this call. Stop asking only when
		// the service says it cannot issue capabilities at all; a sandbox
		// without a node binding (for example cold) can get one later.
		var e *Error
		if errors.As(err, &e) && e.Code == "upstream_unavailable" {
			c.mu.Lock()
			c.token, c.disabled = "", true
			c.mu.Unlock()
		}
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token
}

// tokenExpiry reads the exp claim of a compact JWS without verifying it; the
// edge verifies. It returns the zero time when the token cannot be read.
func tokenExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		ExpiresAt int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.ExpiresAt == 0 {
		return time.Time{}
	}
	return time.Unix(claims.ExpiresAt, 0)
}
