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

	// LastUsedAt is when this key last authenticated a request, updated on
	// every authenticated call (api/auth.go's TouchAPIKey). Omitted — nil
	// here — for a key that has never been used.
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`

	// Scope is what the key is permitted to do: "read", "write" or "admin".
	// The API always sends it, so an empty string here means the response came
	// from a backend that predates scopes rather than "unscoped" — there is no
	// such key.
	Scope string `json:"scope,omitempty"`

	// CreatedByKeyID is the key that minted this one. Omitted — nil here — for
	// a key created from a dashboard session (which has no parent key) or one
	// whose parent has since been deleted. Revoking a key revokes every key
	// below it in this chain.
	CreatedByKeyID *string `json:"created_by_key_id,omitempty"`

	// Key is the plaintext key. Non-empty only on the CreateAPIKey response.
	Key string `json:"key,omitempty"`
}

// createAPIKeyRequest is the POST /api/v1/api-keys body. A nil ExpiresAt means
// the key never expires; the server rejects any value that is not in the future.
//
// Scope is omitempty and that is load-bearing: the API distinguishes an ABSENT
// scope (apply the default) from an explicitly empty one (a caller error it
// refuses rather than silently upgrading). Sending "" would turn a caller that
// simply did not choose into a 400.
type createAPIKeyRequest struct {
	Name      string     `json:"name"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Scope     string     `json:"scope,omitempty"`
}

// CreateAPIKeyInput is what CreateAPIKey sends. It is a struct rather than a
// positional list because the two optional fields are both "leave it to the
// server" when unset, and a bare nil/"" pair at a call site says nothing about
// which is which.
type CreateAPIKeyInput struct {
	// Name is the key's human-readable label. Required.
	Name string
	// ExpiresAt is the absolute expiry, or nil to send none.
	ExpiresAt *time.Time
	// Scope is "read", "write" or "admin", or "" to send no scope at all and
	// let the server apply its own default.
	Scope string
}

// CreateAPIKey mints an API key. The returned APIKey is the only time the
// plaintext Key is ever available.
func (c *Client) CreateAPIKey(ctx context.Context, in CreateAPIKeyInput) (*APIKey, error) {
	body := createAPIKeyRequest{Name: in.Name, Scope: in.Scope}
	if in.ExpiresAt != nil {
		utc := in.ExpiresAt.UTC()
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
