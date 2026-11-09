package bonya

import (
	"context"
	"net/http"
)

// TemplateStack is one language or runtime toolchain preinstalled in a
// template.
type TemplateStack struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// TemplateMetadata describes what a template offers. Every field may be empty:
// the operator may never have annotated the template.
type TemplateMetadata struct {
	// Description is a short summary of the template's purpose.
	Description string `json:"description,omitempty"`
	// OS is the guest's base operating system, e.g. "ubuntu".
	OS string `json:"os,omitempty"`
	// OSVersion is e.g. "latest" or "24.04".
	OSVersion string `json:"os_version,omitempty"`
	// Stacks lists the toolchains preinstalled in the template.
	Stacks []TemplateStack `json:"stacks,omitempty"`
	// AgentCLISupport lists AI agent CLIs on PATH, e.g. "claude-code".
	AgentCLISupport []string `json:"agent_cli_support,omitempty"`
}

// Template is one entry of the deployment's template catalog.
type Template struct {
	ID      string `json:"template_id"`
	Version string `json:"version"`
	Digest  string `json:"digest"`
	// IsDefault is true when this is the version ID resolves to if a request
	// omits the version.
	IsDefault bool             `json:"is_default"`
	Metadata  TemplateMetadata `json:"metadata"`
}

// ListTemplates returns the deployment's immutable template catalog: every
// template id and version a job's NewSandbox accepts. It is the same for every
// caller and needs no organization.
func (c *Client) ListTemplates(ctx context.Context) ([]Template, error) {
	var out struct {
		Templates []Template `json:"templates"`
	}
	if err := c.doJSON(ctx, &request{method: http.MethodGet, path: "/v1/templates"}, &out); err != nil {
		return nil, err
	}
	return out.Templates, nil
}
