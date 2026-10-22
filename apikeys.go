package bonya

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// maxAPIKeyNameBytes is the service's limit on an API key name.
const maxAPIKeyNameBytes = 128

// APIKey describes an API key. It never contains the secret.
type APIKey struct {
	ID   string `json:"key_id"`
	Name string `json:"name,omitempty"`
	// DisplayPrefix and DisplayLast4 identify the key on screen.
	DisplayPrefix string     `json:"display_prefix"`
	DisplayLast4  string     `json:"display_last4"`
	CreatedAt     time.Time  `json:"created_at"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
}

// CreatedAPIKey is a new API key and its secret. The secret is returned
// once, here, and the service never shows it again: store it now. String and
// fmt verbs never print it.
type CreatedAPIKey struct {
	APIKey APIKey
	Secret string
}

// String describes the key without its secret.
func (k CreatedAPIKey) String() string {
	return "api key " + k.APIKey.ID + " (" + k.APIKey.DisplayPrefix + "..." + k.APIKey.DisplayLast4 + ", secret redacted)"
}

// GoString keeps %#v from printing the secret.
func (k CreatedAPIKey) GoString() string { return k.String() }

// ListAPIKeys returns the caller's API keys, revoked ones included. Needs a
// login token; API keys cannot manage keys.
func (c *Client) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	var out struct {
		APIKeys []APIKey `json:"api_keys"`
	}
	if err := c.doJSON(ctx, &request{method: http.MethodGet, path: "/v1/api-keys"}, &out); err != nil {
		return nil, err
	}
	return out.APIKeys, nil
}

// CreateAPIKey creates an API key for the caller with an optional name (at
// most 128 bytes). The secret in the result is shown only once. The call is
// not retried after a lost connection (ErrOutcomeUnknown): list keys and
// revoke any you cannot account for before trying again. Needs a login token.
func (c *Client) CreateAPIKey(ctx context.Context, name string) (*CreatedAPIKey, error) {
	if len(name) > maxAPIKeyNameBytes {
		return nil, invalidArgument("API key name must be at most 128 bytes")
	}
	body := struct {
		Name string `json:"name,omitempty"`
	}{Name: name}
	var out struct {
		APIKey APIKey `json:"api_key"`
		Secret string `json:"secret"`
	}
	if err := c.doJSON(ctx, &request{method: http.MethodPost, path: "/v1/api-keys", json: body}, &out); err != nil {
		return nil, err
	}
	return &CreatedAPIKey{APIKey: out.APIKey, Secret: out.Secret}, nil
}

// RevokeAPIKey revokes one of the caller's API keys. It stops working at
// once. Needs a login token. It is retried after transient failures, so a
// revoke whose first reply was lost may report ErrNotFound.
func (c *Client) RevokeAPIKey(ctx context.Context, keyID string) error {
	if keyID == "" {
		return invalidArgument("API key id is required")
	}
	return c.doJSON(ctx, &request{method: http.MethodDelete, path: "/v1/api-keys/" + url.PathEscape(keyID), idempotent: true}, nil)
}
