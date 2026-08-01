package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// Monitor mirrors the Check schema in the LastPing OpenAPI spec. It is the
// create request body and every response body — but deliberately NOT the update
// body.
//
// `omitempty` on every field but Name is right for create, where an absent key
// means "use the default". It is wrong for update: PATCH /api/v1/checks/{id} is a
// JSON Merge Patch (RFC 7396), so an absent key means "leave the stored value
// alone", and a struct that drops every zero value can therefore never clear
// anything. Updates go through MonitorPatch instead.
type Monitor struct {
	ID                   string   `json:"id,omitempty"`
	Name                 string   `json:"name"`
	Slug                 string   `json:"slug,omitempty"`
	MonitorType          string   `json:"monitor_type,omitempty"`
	ScheduleKind         string   `json:"schedule_kind,omitempty"`
	PeriodS              int64    `json:"period_s,omitempty"`
	CronExpr             string   `json:"cron_expr,omitempty"`
	TZ                   string   `json:"tz,omitempty"`
	GraceS               int64    `json:"grace_s,omitempty"`
	Tags                 []string `json:"tags,omitempty"`
	RunawayCeiling       *int64   `json:"runaway_ceiling,omitempty"`
	MonitorFrom          *string  `json:"monitor_from,omitempty"`
	ProbeURL             string   `json:"probe_url,omitempty"`
	ProbeMethod          string   `json:"probe_method,omitempty"`
	ProbeIntervalS       int64    `json:"probe_interval_s,omitempty"`
	ProbeExpectedBody    string   `json:"probe_expected_body,omitempty"`
	ProbeExpectedStatus  int64    `json:"probe_expected_status,omitempty"`
	ProbeTimeoutS        int64    `json:"probe_timeout_s,omitempty"`
	ProbeFollowRedirects bool     `json:"probe_follow_redirects,omitempty"`

	// Computed.
	Paused           bool    `json:"paused,omitempty"`
	Status           string  `json:"status,omitempty"`
	PingURL          string  `json:"ping_url,omitempty"`
	CreatedAt        string  `json:"created_at,omitempty"`
	LastPingAt       *string `json:"last_ping_at,omitempty"`
	DueAt            *string `json:"due_at,omitempty"`
	AlertAfter       *string `json:"alert_after,omitempty"`
	MaintenanceUntil *string `json:"maintenance_until,omitempty"`
}

// CreateMonitor creates a monitor with create-only semantics: If-None-Match: *
// makes a slug collision a 412 rather than a silent adoption of an existing
// monitor. See spec §4.5 / §5.6 H1.
func (c *Client) CreateMonitor(ctx context.Context, m Monitor) (*Monitor, error) {
	var out Monitor
	err := c.Do(ctx, http.MethodPost, "/api/v1/checks", m, &out, WithHeader("If-None-Match", "*"))
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetMonitor fetches a monitor by its UUID.
func (c *Client) GetMonitor(ctx context.Context, id string) (*Monitor, error) {
	var out Monitor
	if err := c.Do(ctx, http.MethodGet, "/api/v1/checks/"+id, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListMonitors returns every monitor in the caller's project. A non-empty tag
// narrows the list to monitors carrying that exact tag (GET /api/v1/checks?tag=),
// which the server evaluates as a containment match against the tag array.
//
// The tag is query-escaped: the `agent:` convention and `env=prod`-style tags
// both contain characters that would otherwise change the query's meaning.
func (c *Client) ListMonitors(ctx context.Context, tag string) ([]Monitor, error) {
	path := "/api/v1/checks"
	if tag != "" {
		path += "?tag=" + url.QueryEscape(tag)
	}
	var out []Monitor
	if err := c.Do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetMonitorBySlug supports import by slug, which agents reason about far more
// reliably than UUIDs. Monitor slugs are project-scoped, so a match against the
// caller's own project list is unambiguous.
func (c *Client) GetMonitorBySlug(ctx context.Context, slug string) (*Monitor, error) {
	list, err := c.ListMonitors(ctx, "")
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Slug == slug {
			return &list[i], nil
		}
	}
	return nil, &Problem{Status: http.StatusNotFound, Detail: fmt.Sprintf("no monitor with slug %q", slug)}
}

// MonitorPatch is the body of PATCH /api/v1/checks/{id}: a sparse JSON Merge
// Patch (RFC 7396) document.
//
//   - a key that is absent leaves the stored value alone;
//   - a key whose value is nil serialises as JSON `null`, which clears the
//     field — the API honours that for runaway_ceiling, monitor_from, tags,
//     ci_workflow and ci_branch, and treats it as "absent" everywhere else;
//   - any other value replaces the stored one.
//
// slug, monitor_type, ci_provider and ci_secret are create-only and ignored if
// present.
//
// This is a map and not a struct on purpose. Presence is the entire point of a
// merge patch, and encoding/json cannot express "present, and null" for a
// struct field without a wrapper type per field. Reusing the Monitor struct —
// whose blanket `omitempty` makes "cleared" and "unset" the same wire bytes —
// is precisely what made removing tags from a configuration a silent no-op, so
// do not reintroduce a second Monitor-shaped struct here.
type MonitorPatch map[string]any

// UpdateMonitor applies a merge patch to a monitor. See MonitorPatch for how an
// absent key, an explicit null and a value differ.
func (c *Client) UpdateMonitor(ctx context.Context, id string, patch MonitorPatch) (*Monitor, error) {
	var out Monitor
	if err := c.Do(ctx, http.MethodPatch, "/api/v1/checks/"+id, patch, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteMonitor deletes a monitor.
func (c *Client) DeleteMonitor(ctx context.Context, id string) error {
	return c.Do(ctx, http.MethodDelete, "/api/v1/checks/"+id, nil, nil)
}

// SetMonitorPaused maps the paused attribute onto the pause/resume endpoints.
func (c *Client) SetMonitorPaused(ctx context.Context, id string, paused bool) error {
	action := "resume"
	if paused {
		action = "pause"
	}
	return c.Do(ctx, http.MethodPost, "/api/v1/checks/"+id+"/"+action, nil, nil)
}
