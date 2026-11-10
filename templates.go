package bonya

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// NetworkRule allows egress to a domain or a CIDR block.
type NetworkRule struct {
	Domain string `json:"domain,omitempty"`
	CIDR   string `json:"cidr,omitempty"`
}

// NetworkPolicy controls a sandbox's outbound network.
type NetworkPolicy struct {
	// Mode is "open" (default), "allowlist" or "airgap".
	Mode  string        `json:"mode"`
	Allow []NetworkRule `json:"allow,omitempty"`
}

// GPURequest asks for GPUs. Pick a combination from ListGPUOptions.
type GPURequest struct {
	Type  string `json:"type"`
	Count int    `json:"count"`
}

// TemplatePreview is a port a template publishes on start.
type TemplatePreview struct {
	Port       int    `json:"port"`
	Visibility string `json:"visibility,omitempty"`
}

// TemplateConfig is a template's launch configuration. It holds secret
// references, never values.
type TemplateConfig struct {
	Image string `json:"image,omitempty"`
	// ImageDigest is resolved by the service on save.
	ImageDigest          string            `json:"image_digest,omitempty"`
	RegistryConnectionID string            `json:"registry_connection_id,omitempty"`
	CPU                  int               `json:"cpu,omitempty"`
	MemoryMiB            int               `json:"memory_mib,omitempty"`
	DiskGiB              int               `json:"disk_gib,omitempty"`
	Env                  map[string]string `json:"env,omitempty"`
	// SecretBindings maps an environment variable name to a secret id.
	SecretBindings map[string]string `json:"secret_bindings,omitempty"`
	// SecretSets are secret set names injected as environment variables.
	SecretSets []string `json:"secret_sets,omitempty"`
	// Entrypoint and Command: nil inherits the image default.
	Entrypoint []string          `json:"entrypoint,omitempty"`
	Command    []string          `json:"command,omitempty"`
	WorkingDir string            `json:"working_dir,omitempty"`
	Network    *NetworkPolicy    `json:"network,omitempty"`
	Previews   []TemplatePreview `json:"previews,omitempty"`
}

// Template is a saved, versioned launch configuration of the organization.
type Template struct {
	ID             string         `json:"id"`
	OrganizationID string         `json:"organization_id"`
	Name           string         `json:"name"`
	Description    string         `json:"description"`
	Revision       int            `json:"revision"`
	Config         TemplateConfig `json:"config"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

// TemplateWrite creates or updates a template. On update, omitted fields keep
// their value and a changed Config publishes a new revision.
type TemplateWrite struct {
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Config      *TemplateConfig `json:"config,omitempty"`
	// Revision is the expected current revision, to guard against concurrent
	// edits (update only).
	Revision       int    `json:"revision,omitempty"`
	IdempotencyKey string `json:"-"`
}

func templatePath(id string) (string, error) {
	if id == "" {
		return "", invalidArgument("template id is required")
	}
	return "/v1/templates/" + url.PathEscape(id), nil
}

// ListTemplates returns the organization's templates, each at its latest
// revision.
func (c *Client) ListTemplates(ctx context.Context) ([]Template, error) {
	var out struct {
		Templates []Template `json:"templates"`
	}
	if err := c.doJSON(ctx, &request{method: http.MethodGet, path: "/v1/templates"}, &out); err != nil {
		return nil, err
	}
	return out.Templates, nil
}

// GetTemplate returns a template. A revision of 0 means the latest.
func (c *Client) GetTemplate(ctx context.Context, id string, revision int) (*Template, error) {
	path, err := templatePath(id)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	if revision > 0 {
		q.Set("revision", itoa(revision))
	}
	var out Template
	if err := c.doJSON(ctx, &request{method: http.MethodGet, path: path, query: q}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateTemplate validates the configuration, resolves the image to a digest
// and publishes revision 1. Name is required.
func (c *Client) CreateTemplate(ctx context.Context, t TemplateWrite) (*Template, error) {
	if t.Name == "" {
		return nil, invalidArgument("template name is required")
	}
	var out Template
	if err := c.doJSON(ctx, &request{
		method: http.MethodPost, path: "/v1/templates", json: t, idempotencyKey: keyOrNew(t.IdempotencyKey),
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateTemplate updates a template. Omitted fields keep their value.
func (c *Client) UpdateTemplate(ctx context.Context, id string, t TemplateWrite) (*Template, error) {
	path, err := templatePath(id)
	if err != nil {
		return nil, err
	}
	var out Template
	if err := c.doJSON(ctx, &request{method: http.MethodPatch, path: path, json: t, idempotent: true}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteTemplate deletes a template.
func (c *Client) DeleteTemplate(ctx context.Context, id string) error {
	path, err := templatePath(id)
	if err != nil {
		return err
	}
	return c.doJSON(ctx, &request{method: http.MethodDelete, path: path, idempotent: true}, nil)
}
