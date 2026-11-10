package bonya

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

func itoa(n int) string { return strconv.Itoa(n) }

func idPath(base, id, what string) (string, error) {
	if id == "" {
		return "", invalidArgument(what + " id is required")
	}
	return base + "/" + url.PathEscape(id), nil
}

// RegistryConnection holds stored credentials for pulling private container
// images. The password is never returned.
type RegistryConnection struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organization_id"`
	Name           string    `json:"name"`
	Registry       string    `json:"registry"`
	Username       string    `json:"username"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// RegistryConnectionWrite creates or updates a registry connection.
type RegistryConnectionWrite struct {
	Name     string `json:"name"`
	Registry string `json:"registry"`
	Username string `json:"username"`
	// Password is write-only.
	Password string `json:"password"`
}

const registryPath = "/v1/registry-connections"

// ListRegistryConnections lists the organization's registry connections.
func (c *Client) ListRegistryConnections(ctx context.Context) ([]RegistryConnection, error) {
	var out struct {
		Connections []RegistryConnection `json:"registry_connections"`
	}
	if err := c.doJSON(ctx, &request{method: http.MethodGet, path: registryPath}, &out); err != nil {
		return nil, err
	}
	return out.Connections, nil
}

// CreateRegistryConnection stores registry credentials.
func (c *Client) CreateRegistryConnection(ctx context.Context, w RegistryConnectionWrite) (*RegistryConnection, error) {
	var out RegistryConnection
	if err := c.doJSON(ctx, &request{
		method: http.MethodPost, path: registryPath, json: w, idempotencyKey: newIdempotencyKey(),
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateRegistryConnection replaces a connection's fields.
func (c *Client) UpdateRegistryConnection(ctx context.Context, id string, w RegistryConnectionWrite) (*RegistryConnection, error) {
	path, err := idPath(registryPath, id, "registry connection")
	if err != nil {
		return nil, err
	}
	var out RegistryConnection
	if err := c.doJSON(ctx, &request{method: http.MethodPatch, path: path, json: w, idempotent: true}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteRegistryConnection deletes a connection.
func (c *Client) DeleteRegistryConnection(ctx context.Context, id string) error {
	path, err := idPath(registryPath, id, "registry connection")
	if err != nil {
		return err
	}
	return c.doJSON(ctx, &request{method: http.MethodDelete, path: path, idempotent: true}, nil)
}

// ValidateRegistryConnection checks that image can be pulled through the
// connection and returns its resolved digest.
func (c *Client) ValidateRegistryConnection(ctx context.Context, id, image string) (string, error) {
	path, err := idPath(registryPath, id, "registry connection")
	if err != nil {
		return "", err
	}
	if image == "" {
		return "", invalidArgument("image is required")
	}
	var out struct {
		Digest      string `json:"digest"`
		ImageDigest string `json:"image_digest"`
	}
	if err := c.doJSON(ctx, &request{
		method: http.MethodPost, path: path + "/validate", json: map[string]string{"image": image}, idempotent: true,
	}, &out); err != nil {
		return "", err
	}
	if out.ImageDigest != "" {
		return out.ImageDigest, nil
	}
	return out.Digest, nil
}

// Secret is a named, write-only value. Its value is never returned.
type Secret struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organization_id"`
	Name           string    `json:"name"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

const secretsPath = "/v1/secrets"

// ListSecrets lists secrets (names and ids only).
func (c *Client) ListSecrets(ctx context.Context) ([]Secret, error) {
	var out struct {
		Secrets []Secret `json:"secrets"`
	}
	if err := c.doJSON(ctx, &request{method: http.MethodGet, path: secretsPath}, &out); err != nil {
		return nil, err
	}
	return out.Secrets, nil
}

// CreateSecret stores a secret value.
func (c *Client) CreateSecret(ctx context.Context, name, value string) (*Secret, error) {
	var out Secret
	if err := c.doJSON(ctx, &request{
		method: http.MethodPost, path: secretsPath, json: map[string]string{"name": name, "value": value},
		idempotencyKey: newIdempotencyKey(),
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateSecret replaces a secret's name and value.
func (c *Client) UpdateSecret(ctx context.Context, id, name, value string) (*Secret, error) {
	path, err := idPath(secretsPath, id, "secret")
	if err != nil {
		return nil, err
	}
	var out Secret
	if err := c.doJSON(ctx, &request{
		method: http.MethodPatch, path: path, json: map[string]string{"name": name, "value": value}, idempotent: true,
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteSecret deletes a secret.
func (c *Client) DeleteSecret(ctx context.Context, id string) error {
	path, err := idPath(secretsPath, id, "secret")
	if err != nil {
		return err
	}
	return c.doJSON(ctx, &request{method: http.MethodDelete, path: path, idempotent: true}, nil)
}

// SecretSet is a named group of environment variables. Reference it by Name
// in SecretSets on sandboxes, templates and jobs. Values are never returned.
type SecretSet struct {
	ID               string    `json:"id"`
	OrganizationID   string    `json:"organization_id"`
	Name             string    `json:"name"`
	Revision         int       `json:"revision"`
	Keys             []string  `json:"keys"`
	EnvironmentBytes int       `json:"environment_bytes"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// SecretSetWrite creates or updates a secret set.
type SecretSetWrite struct {
	Name string `json:"name,omitempty"`
	// Values maps variable names to values. Write-only.
	Values map[string]string `json:"values,omitempty"`
	// Revision is the expected current revision (update only).
	Revision int `json:"revision,omitempty"`
}

const secretSetsPath = "/v1/secret-sets"

// ListSecretSets lists secret sets.
func (c *Client) ListSecretSets(ctx context.Context) ([]SecretSet, error) {
	var out struct {
		SecretSets []SecretSet `json:"secret_sets"`
	}
	if err := c.doJSON(ctx, &request{method: http.MethodGet, path: secretSetsPath}, &out); err != nil {
		return nil, err
	}
	return out.SecretSets, nil
}

// GetSecretSet returns a secret set (variable names, never values).
func (c *Client) GetSecretSet(ctx context.Context, id string) (*SecretSet, error) {
	path, err := idPath(secretSetsPath, id, "secret set")
	if err != nil {
		return nil, err
	}
	var out SecretSet
	if err := c.doJSON(ctx, &request{method: http.MethodGet, path: path}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateSecretSet creates a secret set. Name is required.
func (c *Client) CreateSecretSet(ctx context.Context, w SecretSetWrite) (*SecretSet, error) {
	if w.Name == "" {
		return nil, invalidArgument("secret set name is required")
	}
	var out SecretSet
	if err := c.doJSON(ctx, &request{
		method: http.MethodPost, path: secretSetsPath, json: w, idempotencyKey: newIdempotencyKey(),
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateSecretSet updates a secret set. Omitted fields keep their value.
func (c *Client) UpdateSecretSet(ctx context.Context, id string, w SecretSetWrite) (*SecretSet, error) {
	path, err := idPath(secretSetsPath, id, "secret set")
	if err != nil {
		return nil, err
	}
	var out SecretSet
	if err := c.doJSON(ctx, &request{method: http.MethodPatch, path: path, json: w, idempotent: true}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteSecretSet deletes a secret set.
func (c *Client) DeleteSecretSet(ctx context.Context, id string) error {
	path, err := idPath(secretSetsPath, id, "secret set")
	if err != nil {
		return err
	}
	return c.doJSON(ctx, &request{method: http.MethodDelete, path: path, idempotent: true}, nil)
}

// ListGPUOptions returns the GPU type and count combinations a sandbox can be
// created with. An empty list means no GPU capacity is available.
func (c *Client) ListGPUOptions(ctx context.Context) ([]GPURequest, error) {
	var out struct {
		Options []GPURequest `json:"gpu_options"`
	}
	if err := c.doJSON(ctx, &request{method: http.MethodGet, path: "/v1/sandbox/gpu-options"}, &out); err != nil {
		return nil, err
	}
	return out.Options, nil
}

// UpdateNetwork changes the sandbox's network policy.
func (s *Sandbox) UpdateNetwork(ctx context.Context, policy NetworkPolicy) error {
	var out sandboxWire
	if err := s.client.doJSON(ctx, &request{
		method: http.MethodPatch, path: sandboxPath(s.ID, "network"), json: policy,
		sandboxID: s.ID, idempotent: true,
	}, &out); err != nil {
		return err
	}
	s.apply(out)
	return nil
}

// GetByName returns the sandbox with this exact name. It is ErrNotFound when
// none matches and ErrConflict when several do (names are not unique).
func (c *Client) GetByName(ctx context.Context, name string) (*Sandbox, error) {
	if name == "" {
		return nil, invalidArgument("sandbox name is required")
	}
	var found []*Sandbox
	for sb, err := range c.ListAll(ctx, ListOptions{Name: name}) {
		if err != nil {
			return nil, err
		}
		found = append(found, sb)
	}
	switch len(found) {
	case 0:
		return nil, &Error{Kind: ErrNotFound, Message: "no sandbox is named " + strconv.Quote(name)}
	case 1:
		return found[0], nil
	}
	return nil, &Error{Kind: ErrConflict, Message: "several sandboxes are named " + strconv.Quote(name) + "; use an id"}
}
