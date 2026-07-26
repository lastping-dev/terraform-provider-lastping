package client

import (
	"context"
	"net/http"
)

// Project is what the API will tell you about the credential you presented.
// GET /api/v1/whoami returns exactly one field, the project the key belongs to;
// there is no richer project resource in the public API, so this is the whole
// shape rather than a subset of one.
type Project struct {
	ProjectID string `json:"project_id"`
}

// Whoami resolves the authenticated project. It doubles as an auth smoke-test:
// an invalid or revoked key answers 401, which surfaces as a *Problem.
func (c *Client) Whoami(ctx context.Context) (*Project, error) {
	var out Project
	if err := c.Do(ctx, http.MethodGet, "/api/v1/whoami", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
