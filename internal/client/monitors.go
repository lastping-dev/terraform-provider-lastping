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
	MaxRuntimeS          *int64   `json:"max_runtime_s,omitempty"`
	StepTimeoutS         *int64   `json:"step_timeout_s,omitempty"`
	ExpectEveryS         *int64   `json:"expect_every_s,omitempty"`
	BlockedTimeoutS      *int64   `json:"blocked_timeout_s,omitempty"`
	NotifyMinRunS        *int64   `json:"notify_min_run_s,omitempty"`
	FailureThreshold     int64    `json:"failure_threshold,omitempty"`
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
	// AgentID attaches this monitor to an agent, by either the agent's id or
	// its slug (api/checks.go: createCheckRequest.AgentID / checkResponse.AgentID).
	// Empty/omitted means "no attachment" on create, and is never populated by a
	// response that reports one — see modelFromMonitor's use of stringOrNull.
	// Updates go through MonitorPatch, not this field, the same as every other
	// clearable attribute.
	AgentID string `json:"agent_id,omitempty"`

	// CI binding. CiProvider is create-only: POST /api/v1/checks generates a
	// webhook secret and binds it, and PATCH does not decode the field at all
	// (api/checks_patch.go: checkPatchRequest has no ci_provider member), so a
	// changed provider is only reachable by replacing the monitor. GET and list
	// DO report it (checkResponse.CiProvider), so it round-trips.
	//
	// CiWorkflow and CiBranch are WRITE-ONLY, and this is the sharpest edge in
	// this struct: both are accepted on create and on PATCH, but no response
	// ever carries them — api/checks.go's checkResponse has no field for
	// either, so rowToDTO cannot populate them and GET/list omit them
	// unconditionally. They therefore always decode as "" here regardless of
	// what the server holds, and Terraform state for them has to be carried
	// forward from the prior state rather than refreshed. See
	// modelFromMonitor's writeOnlyString.
	//
	// Their PATCH semantics deliberately deviate from RFC 7396: an explicit ""
	// PRESERVES the stored value (so a full-body client cannot silently unbind
	// a live filter) and only an explicit null clears it. That is why
	// monitorPatchFromModel never sends "" for either — see the clearable group
	// there.
	CiProvider string `json:"ci_provider,omitempty"`
	CiWorkflow string `json:"ci_workflow,omitempty"`
	CiBranch   string `json:"ci_branch,omitempty"`

	// CiWebhookURL is, unlike CiWorkflow/CiBranch, an ordinary readable
	// response field: api/checks.go's rowToDTO populates it from row.CiProvider
	// on every GET and list, exactly like CiProvider itself. Omitted (empty)
	// when the monitor has no CI binding.
	CiWebhookURL string `json:"ci_webhook_url,omitempty"`

	// CiSecret is WRITE-ONCE in the other direction from CiWorkflow/CiBranch:
	// the API returns it, but only in the 201 response to POST /api/v1/checks
	// when ci_provider was set on that same request, and from
	// POST /api/v1/checks/{id}/ci/regenerate (which this provider does not
	// call). rowToDTO never populates it — api/checks.go's own comment on
	// rowToDTO says so explicitly: "ci_secret is NEVER populated here". Every
	// GET, list and PATCH response therefore decodes this as "", regardless of
	// whether the monitor has a live secret, and modelFromMonitor has to carry
	// the value forward from prior state instead of refreshing it — see
	// writeOnlyString, the same mechanism CiWorkflow/CiBranch use for the
	// opposite write-only shape.
	CiSecret string `json:"ci_secret,omitempty"`

	// SourceKind and SourceRef are the discovery source identity: the scanner
	// that found this thing (crontab, github-actions, k8s-cronjob,
	// systemd-timer) and a stable path within it
	// (.github/workflows/nightly.yml#build). Together with the project they are
	// the reconcile key that makes a discovery scan safe to re-run — the second
	// run diffs against what exists instead of creating every monitor again.
	//
	// They are ordinary readable response fields: api/checks.go's checkResponse
	// carries both with `omitempty`, so a hand-made monitor (the state of every
	// monitor that predates discovery) decodes both as "". That is why the
	// provider maps them through stringOrNull and never through
	// writeOnlyString — absent means "no source", not "the API declined to
	// tell me".
	//
	// The `omitempty` here is load-bearing in the SEND direction and is the
	// first of the three ways a discovery identity can be silently destroyed:
	// the API treats an explicit "" on these two as OMISSION, not as a clear
	// (the same RFC 7396 deviation ci_workflow/ci_branch carry, for the same
	// full-body clients). So "" on the wire never means what a caller thinks it
	// means, and dropping it is the only safe rendering. Only an explicit null
	// clears, and only through MonitorPatch — see monitorPatchFromModel.
	//
	// They are also the only pair in this struct the API validates jointly:
	// exactly one of the two is 400 SOURCE_INCOMPLETE, a pair another monitor
	// in the project already claims is 409 SOURCE_ALREADY_MONITORED, and a slug
	// upsert offering a different pair is 409 SOURCE_IMMUTABLE_ON_UPSERT.
	SourceKind string `json:"source_kind,omitempty"`
	SourceRef  string `json:"source_ref,omitempty"`

	// Computed.
	Paused           bool    `json:"paused,omitempty"`
	Status           string  `json:"status,omitempty"`
	PingURL          string  `json:"ping_url,omitempty"`
	CreatedAt        string  `json:"created_at,omitempty"`
	LastPingAt       *string `json:"last_ping_at,omitempty"`
	DueAt            *string `json:"due_at,omitempty"`
	AlertAfter       *string `json:"alert_after,omitempty"`
	MaintenanceUntil *string `json:"maintenance_until,omitempty"`
	// NextProbeAt is due_at's counterpart for monitor_type = "http": when the
	// prober will next probe the URL. api/checks.go's checkResponse omits it
	// for every other monitor_type, so it decodes as nil there, the same as
	// due_at would for a monitor type that has no due_at.
	NextProbeAt *string `json:"next_probe_at,omitempty"`
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
//     field — the API honours that for runaway_ceiling, max_runtime_s,
//     step_timeout_s, expect_every_s, blocked_timeout_s, notify_min_run_s,
//     monitor_from, tags,
//     ci_workflow, ci_branch, source_kind and source_ref, and treats
//     it as "absent" everywhere else. A null blocked_timeout_s is the odd one
//     out in that list: it does not mean "wait forever", it restores the
//     check.DefaultBlockedTimeout fallback of 24 hours.
//     failure_threshold is deliberately NOT in
//     that list: its
//     column is NOT NULL DEFAULT 1, so a null there is read as an omission and
//     leaves the stored value alone;
//   - any other value replaces the stored one.
//
// source_kind and source_ref are the one pair in that list with a joint rule:
// they are resolved TOGETHER, so whatever the request and the stored row
// combine to must be either both set or both empty. A lone
// {"source_ref": null} is a 400 SOURCE_INCOMPLETE, not a half-cleared
// identity, and clearing therefore takes an explicit null on both keys. They
// also share ci_workflow/ci_branch's deviation from RFC 7396 — an explicit ""
// PRESERVES the stored value — with a sharper consequence, because this pair is
// an identity rather than a setting: a source cleared by accident is a monitor
// the next discovery scan no longer recognises, so the scan creates a SECOND
// monitor for the same source. A patch that mentions neither key issues no
// source write at all, which is what keeps an unrelated edit (a rename, say)
// off the reconcile key entirely.
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
