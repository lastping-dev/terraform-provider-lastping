package client

import (
	"context"
	"net/http"
)

// GetMetrics returns GET /api/v1/metrics verbatim.
//
// This endpoint is the one place the API does not answer in JSON: it emits
// Prometheus text exposition format 0.0.4, so the body is returned as-is rather
// than decoded. Parsing it here would be a lossy re-interpretation of a format
// whose whole point is to be handed to a scraper unchanged.
func (c *Client) GetMetrics(ctx context.Context) (string, error) {
	var raw []byte
	if err := c.Do(ctx, http.MethodGet, "/api/v1/metrics", nil, &raw,
		WithHeader("Accept", "text/plain")); err != nil {
		return "", err
	}
	return string(raw), nil
}
