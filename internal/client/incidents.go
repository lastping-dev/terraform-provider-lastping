package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// Incident mirrors the Incident schema in the LastPing OpenAPI spec: one
// downtime window on a single monitor.
//
// There is no project-wide incident endpoint — the public API exposes incidents
// only under GET /api/v1/checks/{id}/incidents — so every read is scoped to one
// monitor.
type Incident struct {
	OpenedAt string  `json:"opened_at"`
	ClosedAt *string `json:"closed_at"`
	Cause    string  `json:"cause"`
	Detail   string  `json:"detail,omitempty"`
}

// ListIncidents returns a monitor's incidents, newest first. A limit of zero
// omits the parameter and takes the server default (50); the server clamps
// anything above its own maximum (200) rather than rejecting it.
//
// A monitor that does not exist in the caller's project returns 404 — the API
// deliberately gives no existence oracle across tenants.
func (c *Client) ListIncidents(ctx context.Context, monitorID string, limit int64) ([]Incident, error) {
	path := "/api/v1/checks/" + url.PathEscape(monitorID) + "/incidents"
	if limit > 0 {
		path += "?limit=" + strconv.FormatInt(limit, 10)
	}
	var out []Incident
	if err := c.Do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}
