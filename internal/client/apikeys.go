package client

import (
	"context"
	"net/http"
	"time"
)

// APIKey mirrors the API key DTO in the LastPing OpenAPI spec.
//
// SECURITY: Key is the plaintext credential and is populated by CreateAPIKey
// ONLY. The server returns it exactly once, from POST; list responses carry the
// non-secret prefix and nothing else (api/apikeys_api.go: apiKeyResponse). It
// must never be logged, and never sent back to the server — which is why
// createAPIKeyRequest is a separate type rather than this struct reused.
type APIKey struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Prefix    string     `json:"prefix"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`

	// Key is the plaintext key. Non-empty only on the CreateAPIKey response.
	Key string `json:"key,omitempty"`
}

// createAPIKeyRequest is the POST /api/v1/api-keys body. A nil ExpiresAt means
// the key never expires; the server rejects any value that is not in the future.
type createAPIKeyRequest struct {
	Name      string     `json:"name"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// CreateAPIKey mints an API key. The returned APIKey is the only time the
// plaintext Key is ever available.
func (c *Client) CreateAPIKey(ctx context.Context, name string, expiresAt *time.Time) (*APIKey, error) {
	body := createAPIKeyRequest{Name: name}
	if expiresAt != nil {
		utc := expiresAt.UTC()
		body.ExpiresAt = &utc
	}
	var out APIKey
	if err := c.Do(ctx, http.MethodPost, "/api/v1/api-keys", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListAPIKeys returns every key in the caller's project, without plaintext.
// There is no per-key GET endpoint, so this is how a single key is refreshed.
func (c *Client) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	var out []APIKey
	if err := c.Do(ctx, http.MethodGet, "/api/v1/api-keys", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetAPIKey returns the key with the given ID, or a 404 *Problem when the
// project has no such key. It is a filter over ListAPIKeys because the API
// exposes no single-key endpoint.
func (c *Client) GetAPIKey(ctx context.Context, id string) (*APIKey, error) {
	keys, err := c.ListAPIKeys(ctx)
	if err != nil {
		return nil, err
	}
	for i := range keys {
		if keys[i].ID == id {
			return &keys[i], nil
		}
	}
	return nil, &Problem{
		Status: http.StatusNotFound,
		Title:  "api key not found",
		Detail: "No API key with ID " + id + " exists in this project.",
	}
}

// RevokeAPIKey deletes a key. Revocation is immediate: the next request
// presenting it is unauthenticated.
func (c *Client) RevokeAPIKey(ctx context.Context, id string) error {
	return c.Do(ctx, http.MethodDelete, "/api/v1/api-keys/"+id, nil, nil)
}
