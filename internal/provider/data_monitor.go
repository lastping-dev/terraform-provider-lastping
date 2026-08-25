package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/datasourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/lastping-dev/terraform-provider-lastping/internal/client"
)

var (
	_ datasource.DataSource                     = (*monitorDataSource)(nil)
	_ datasource.DataSourceWithConfigure        = (*monitorDataSource)(nil)
	_ datasource.DataSourceWithConfigValidators = (*monitorDataSource)(nil)
)

// monitorDataModel is one monitor as read back from the API. The same struct
// backs both `lastping_monitor` and each element of `lastping_monitors`, so the
// two can never disagree about an attribute name or type.
type monitorDataModel struct {
	ID                   types.String `tfsdk:"id"`
	Name                 types.String `tfsdk:"name"`
	Slug                 types.String `tfsdk:"slug"`
	MonitorType          types.String `tfsdk:"monitor_type"`
	ScheduleKind         types.String `tfsdk:"schedule_kind"`
	PeriodS              types.Int64  `tfsdk:"period_s"`
	CronExpr             types.String `tfsdk:"cron_expr"`
	TZ                   types.String `tfsdk:"tz"`
	GraceS               types.Int64  `tfsdk:"grace_s"`
	MaxRuntimeS          types.Int64  `tfsdk:"max_runtime_s"`
	StepTimeoutS         types.Int64  `tfsdk:"step_timeout_s"`
	ExpectEveryS         types.Int64  `tfsdk:"expect_every_s"`
	NotifyMinRunS        types.Int64  `tfsdk:"notify_min_run_s"`
	BlockedTimeoutS      types.Int64  `tfsdk:"blocked_timeout_s"`
	CiProvider           types.String `tfsdk:"ci_provider"`
	CiWebhookURL         types.String `tfsdk:"ci_webhook_url"`
	CiSecret             types.String `tfsdk:"ci_secret"`
	SourceKind           types.String `tfsdk:"source_kind"`
	SourceRef            types.String `tfsdk:"source_ref"`
	FailureThreshold     types.Int64  `tfsdk:"failure_threshold"`
	Tags                 types.Set    `tfsdk:"tags"`
	RunawayCeiling       types.Int64  `tfsdk:"runaway_ceiling"`
	MonitorFrom          types.String `tfsdk:"monitor_from"`
	AgentID              types.String `tfsdk:"agent_id"`
	ProbeURL             types.String `tfsdk:"probe_url"`
	ProbeMethod          types.String `tfsdk:"probe_method"`
	ProbeIntervalS       types.Int64  `tfsdk:"probe_interval_s"`
	ProbeExpectedBody    types.String `tfsdk:"probe_expected_body"`
	ProbeExpectedStatus  types.Int64  `tfsdk:"probe_expected_status"`
	ProbeTimeoutS        types.Int64  `tfsdk:"probe_timeout_s"`
	ProbeFollowRedirects types.Bool   `tfsdk:"probe_follow_redirects"`
	Paused               types.Bool   `tfsdk:"paused"`
	PingURL              types.String `tfsdk:"ping_url"`
	Status               types.String `tfsdk:"status"`
	CreatedAt            types.String `tfsdk:"created_at"`
	LastPingAt           types.String `tfsdk:"last_ping_at"`
	DueAt                types.String `tfsdk:"due_at"`
	NextProbeAt          types.String `tfsdk:"next_probe_at"`
	AlertAfter           types.String `tfsdk:"alert_after"`
	MaintenanceUntil     types.String `tfsdk:"maintenance_until"`
}

// monitorDataAttributes is the read-only schema for one monitor. Every
// attribute is Computed — a data source reports what the API holds and has
// nothing to write back — including `id` and `slug`, which `lastping_monitor`
// then re-declares as lookup arguments.
func monitorDataAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"id": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "Monitor UUID.",
		},
		"name": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "Human-readable name.",
		},
		"slug": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "Stable, project-scoped identifier. Null for a monitor created without one.",
		},
		"monitor_type": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "`heartbeat`, `ci`, or `http`.",
		},
		"schedule_kind": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "`simple` (fixed `period_s`) or `cron` (`cron_expr` + `tz`).",
		},
		"period_s": schema.Int64Attribute{
			Computed:            true,
			MarkdownDescription: "Expected ping interval in seconds, for `schedule_kind = \"simple\"`.",
		},
		"cron_expr": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "5-field cron expression, for `schedule_kind = \"cron\"`.",
		},
		"tz": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "IANA timezone used to evaluate `cron_expr`.",
		},
		"grace_s": schema.Int64Attribute{
			Computed:            true,
			MarkdownDescription: "Grace period in seconds after a deadline is missed before alerting.",
		},
		"max_runtime_s": schema.Int64Attribute{
			Computed: true,
			MarkdownDescription: "How long a single run may take before it is called overdue, in seconds. " +
				"Governs the overrun deadline only; the silence rule keeps using `grace_s`. Null when " +
				"unset, in which case the overrun budget falls back to `grace_s`. Never set on an " +
				"`http` monitor.",
		},
		"step_timeout_s": schema.Int64Attribute{
			Computed: true,
			MarkdownDescription: "How long an armed run may go without reporting a step before a " +
				"`stalled` incident opens, in seconds. Null when unset, in which case stall detection " +
				"is off. Always strictly below the effective run budget (`max_runtime_s`, or `grace_s` " +
				"when that is unset), and never set on an `http` monitor.",
		},
		"expect_every_s": schema.Int64Attribute{
			Computed: true,
			MarkdownDescription: "Silence floor in seconds: the longest this monitor may go with no ping of " +
				"any kind before a `silence` incident opens, regardless of its schedule. Null when unset, " +
				"in which case absence is detected only by the schedule — and, on a " +
				"`schedule_kind = \"on_demand\"` monitor, not at all between runs.",
		},
		"notify_min_run_s": schema.Int64Attribute{
			Computed: true,
			MarkdownDescription: "Notification duration floor in seconds: a run shorter than this produces " +
				"no info-class notification (`success`, `every-run`, `note`). Null when unset, in which " +
				"case every info-class event notifies regardless of how short the run was. Never suppresses " +
				"`down`, `fail`, `recovery` or `blocked`, and never set on an `http` monitor.",
		},
		"blocked_timeout_s": schema.Int64Attribute{
			Computed: true,
			MarkdownDescription: "How long a run may sit `blocked` — waiting on a human — before a `blocked` " +
				"incident opens, in seconds. Null when unset, which does **not** mean the monitor waits " +
				"forever: the server falls back to a 24-hour default. There is no state in which the " +
				"blocked timeout is off.",
		},
		"ci_provider": schema.StringAttribute{
			Computed: true,
			MarkdownDescription: "CI provider bound to this monitor (`github`, `gitlab` or `jenkins`), or " +
				"null when it has no CI binding.\n\n" +
				"The binding's `ci_workflow` and `ci_branch` filters have no attribute here, and cannot: " +
				"no API response carries them, so a data source could only ever report null. The " +
				"`lastping_monitor` resource exposes them as write-only attributes for that reason.",
		},
		"ci_webhook_url": schema.StringAttribute{
			Computed: true,
			MarkdownDescription: "The URL this monitor's CI pipeline `POST`s to, signed with `ci_secret`. " +
				"Reported by every `GET`, unlike `ci_secret`. Null when the monitor has no CI binding.",
		},
		"ci_secret": schema.StringAttribute{
			Computed:  true,
			Sensitive: true,
			MarkdownDescription: "The HMAC key `ci_webhook_url` requests are signed with.\n\n" +
				"~> **Always null through this data source.** The API returns `ci_secret` only in the " +
				"response to the `POST` that sets `ci_provider` — never on `GET` or list, which is all a " +
				"data source ever calls. The `lastping_monitor` resource can hold the value because it " +
				"captures it at creation and carries it forward across refreshes; a data source has no " +
				"creation event of its own to capture it from.",
		},
		"source_kind": schema.StringAttribute{
			Computed: true,
			MarkdownDescription: "**Discovery source identity, part 1 of 2.** The scanner that " +
				"discovered this monitor: `crontab`, `github-actions`, `k8s-cronjob`, `systemd-timer`. " +
				"**Null when a human created the monitor**, which is the state of every monitor that " +
				"predates discovery — and such a monitor is never reconciled, so a scan can neither adopt " +
				"nor delete it.\n\n" +
				"Use it to tell apart the monitors a scan owns from the ones you wrote by hand, without " +
				"importing either.",
		},
		"source_ref": schema.StringAttribute{
			Computed: true,
			MarkdownDescription: "**Discovery source identity, part 2 of 2.** The stable path within " +
				"`source_kind` that identifies exactly what was found, such as " +
				"`.github/workflows/nightly.yml#build`. Together with `source_kind` and the project it is " +
				"the discovery reconcile key.\n\n" +
				"Always either both present or both null with `source_kind`: a database CHECK constraint " +
				"makes a half-set identity unrepresentable.",
		},
		"failure_threshold": schema.Int64Attribute{
			Computed: true,
			MarkdownDescription: "Consecutive explicit failures required before an incident opens " +
				"(the `fail` cause only). Always present; `1` is the default, \"open on the first failure\".",
		},
		"tags": schema.SetAttribute{
			Computed:            true,
			ElementType:         types.StringType,
			MarkdownDescription: "Labels attached to this monitor. Empty when it has none.",
		},
		"runaway_ceiling": schema.Int64Attribute{
			Computed: true,
			MarkdownDescription: "Cap on pings per rolling 1-hour window, above which a `runaway` " +
				"incident opens. Null when disabled.",
		},
		"monitor_from": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "RFC 3339 UTC timestamp before which deadlines are not computed.",
		},
		"agent_id": schema.StringAttribute{
			Computed: true,
			MarkdownDescription: "UUID of the `lastping_agent` that owns this monitor. Null when the " +
				"monitor is unattached — including right after its owning agent was deleted.",
		},
		"probe_url": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "URL probed for `monitor_type = \"http\"`.",
		},
		"probe_method": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "HTTP method used for the probe.",
		},
		"probe_interval_s": schema.Int64Attribute{
			Computed:            true,
			MarkdownDescription: "Seconds between probes.",
		},
		"probe_expected_body": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "Substring the probe response body must contain to count as healthy.",
		},
		"probe_expected_status": schema.Int64Attribute{
			Computed:            true,
			MarkdownDescription: "HTTP status code the probe must return.",
		},
		"probe_timeout_s": schema.Int64Attribute{
			Computed:            true,
			MarkdownDescription: "Per-probe timeout in seconds.",
		},
		"probe_follow_redirects": schema.BoolAttribute{
			Computed:            true,
			MarkdownDescription: "Whether the probe follows HTTP redirects.",
		},
		"paused": schema.BoolAttribute{
			Computed:            true,
			MarkdownDescription: "Whether alerting is suspended for this monitor.",
		},
		"ping_url": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "URL a service HTTP GETs to record a successful ping.",
		},
		"status": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "Current status: `new`, `up`, `late`, or `down`.",
		},
		"created_at": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "RFC 3339 UTC timestamp when the monitor was created.",
		},
		"last_ping_at": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "RFC 3339 UTC timestamp of the most recent ping. Null until the first ping.",
		},
		"due_at": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "RFC 3339 UTC timestamp of the next expected ping.",
		},
		"next_probe_at": schema.StringAttribute{
			Computed: true,
			MarkdownDescription: "RFC 3339 UTC timestamp when the prober will next probe this monitor — " +
				"`due_at`'s counterpart for `monitor_type = \"http\"`. Null for every other monitor type.",
		},
		"alert_after": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "RFC 3339 UTC timestamp after which a missing ping raises an incident.",
		},
		"maintenance_until": schema.StringAttribute{
			Computed: true,
			MarkdownDescription: "RFC 3339 UTC timestamp until which alerting is suppressed for " +
				"maintenance. Null outside a maintenance window.",
		},
	}
}

// monitorDataFromAPI maps an API monitor onto the data-source model. It uses
// the same per-attribute empty/zero-to-null conventions as
// lastping_monitor's modelFromMonitor, attribute by attribute, so
// `data.lastping_monitor.x.<attr>` and `lastping_monitor.x.<attr>` are
// comparable values rather than "" or 0 versus null.
//
// That parity is deliberate, not automatic: period_s, grace_s,
// failure_threshold, probe_timeout_s and probe_expected_status are never null
// on either side — the API always reports a concrete number for them (0 is a
// real answer, e.g. period_s on a cron monitor), so both surfaces report it as
// 0, not null. probe_interval_s, runaway_ceiling, max_runtime_s and
// step_timeout_s are the opposite: both surfaces map an absent value (0, or a
// nil pointer) to null, because 0 is not a value either can take on
// legitimately.
//
// Two tests keep it that way, and they cover different halves.
// data_monitor_test.go pairs each of these attributes against the resource
// against a live backend, which catches a renamed or dropped field. It cannot
// catch a wrong empty-value mapping for an attribute the backend never returns
// empty — failure_threshold is NOT NULL DEFAULT 1, so int64OrNull would agree
// with Int64Value on every monitor that can exist. That half is pinned by
// TestMonitorSurfacesAgreeOnEmptyValues, which feeds both mappers a hand-built
// zero/nil response and requires them to agree.
func monitorDataFromAPI(ctx context.Context, m *client.Monitor) (monitorDataModel, diag.Diagnostics) {
	tags, diags := types.SetValueFrom(ctx, types.StringType, m.Tags)
	if m.Tags == nil {
		// The API always returns an array, but a nil slice would otherwise
		// become a null set and read differently from an empty one.
		tags, diags = types.SetValueFrom(ctx, types.StringType, []string{})
	}

	out := monitorDataModel{
		ID:               types.StringValue(m.ID),
		Name:             types.StringValue(m.Name),
		Slug:             stringOrNull(m.Slug),
		MonitorType:      stringOrNull(m.MonitorType),
		ScheduleKind:     stringOrNull(m.ScheduleKind),
		PeriodS:          types.Int64Value(m.PeriodS),
		CronExpr:         stringOrNull(m.CronExpr),
		TZ:               stringOrNull(m.TZ),
		GraceS:           types.Int64Value(m.GraceS),
		FailureThreshold: types.Int64Value(m.FailureThreshold),
		Tags:             tags,
		MonitorFrom:      timestampOrNull(m.MonitorFrom),
		AgentID:          stringOrNull(m.AgentID),
		CiProvider:       stringOrNull(m.CiProvider),
		// ci_webhook_url is an ordinary readable response field, refreshed the
		// same as ci_provider. ci_secret is not: the API never returns it from
		// GET/list (only from the create response), so it reads as null on
		// every data source lookup — see its schema description. There is no
		// prior state for a data source to carry a create-time value forward
		// from, unlike the resource's CiSecret.
		CiWebhookURL: stringOrNull(m.CiWebhookURL),
		CiSecret:     stringOrNull(m.CiSecret),
		// The discovery identity. stringOrNull for the same reason the resource
		// uses it: the API omits both fields for a hand-made monitor, and "" in
		// state would claim an identity of "" rather than none.
		SourceKind:           stringOrNull(m.SourceKind),
		SourceRef:            stringOrNull(m.SourceRef),
		ProbeURL:             stringOrNull(m.ProbeURL),
		ProbeMethod:          stringOrNull(m.ProbeMethod),
		ProbeIntervalS:       int64OrNull(m.ProbeIntervalS),
		ProbeExpectedBody:    stringOrNull(m.ProbeExpectedBody),
		ProbeExpectedStatus:  types.Int64Value(m.ProbeExpectedStatus),
		ProbeTimeoutS:        types.Int64Value(m.ProbeTimeoutS),
		ProbeFollowRedirects: types.BoolValue(m.ProbeFollowRedirects),
		Paused:               types.BoolValue(m.Paused),
		PingURL:              stringOrNull(m.PingURL),
		Status:               stringOrNull(m.Status),
		CreatedAt:            stringOrNull(m.CreatedAt),
		LastPingAt:           timestampOrNull(m.LastPingAt),
		DueAt:                timestampOrNull(m.DueAt),
		NextProbeAt:          timestampOrNull(m.NextProbeAt),
		AlertAfter:           timestampOrNull(m.AlertAfter),
		MaintenanceUntil:     timestampOrNull(m.MaintenanceUntil),
	}
	if m.RunawayCeiling != nil {
		out.RunawayCeiling = types.Int64Value(*m.RunawayCeiling)
	} else {
		out.RunawayCeiling = types.Int64Null()
	}
	if m.MaxRuntimeS != nil {
		out.MaxRuntimeS = types.Int64Value(*m.MaxRuntimeS)
	} else {
		out.MaxRuntimeS = types.Int64Null()
	}
	if m.StepTimeoutS != nil {
		out.StepTimeoutS = types.Int64Value(*m.StepTimeoutS)
	} else {
		out.StepTimeoutS = types.Int64Null()
	}
	if m.ExpectEveryS != nil {
		out.ExpectEveryS = types.Int64Value(*m.ExpectEveryS)
	} else {
		out.ExpectEveryS = types.Int64Null()
	}
	if m.NotifyMinRunS != nil {
		out.NotifyMinRunS = types.Int64Value(*m.NotifyMinRunS)
	} else {
		out.NotifyMinRunS = types.Int64Null()
	}
	if m.BlockedTimeoutS != nil {
		out.BlockedTimeoutS = types.Int64Value(*m.BlockedTimeoutS)
	} else {
		out.BlockedTimeoutS = types.Int64Null()
	}
	return out, diags
}

// NewMonitorDataSource returns a new lastping_monitor data source.
func NewMonitorDataSource() datasource.DataSource {
	return &monitorDataSource{}
}

type monitorDataSource struct {
	client *client.Client
}

func (d *monitorDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_monitor"
}

func (d *monitorDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	attrs := monitorDataAttributes()

	// id and slug are the lookup arguments as well as outputs: whichever one is
	// supplied, the other is filled in from the response. That is the one place
	// Optional+Computed is right here — everything else stays plain Computed.
	attrs["id"] = schema.StringAttribute{
		Optional: true,
		Computed: true,
		MarkdownDescription: "Monitor UUID. Supply this or `slug`, not both. Computed when the " +
			"monitor is looked up by slug.",
	}
	attrs["slug"] = schema.StringAttribute{
		Optional: true,
		Computed: true,
		MarkdownDescription: "Project-scoped slug. Supply this or `id`, not both. Computed when the " +
			"monitor is looked up by id, and null if the monitor has no slug.",
	}

	resp.Schema = schema.Schema{
		MarkdownDescription: "Look up a single monitor by `id` or `slug`.\n\n" +
			"Use this to reference a monitor Terraform does not manage — for example to attach a " +
			"`lastping_route` to a monitor created in the dashboard, without importing it.",
		Attributes: attrs,
	}
}

// ConfigValidators requires exactly one lookup argument. Neither would be an
// unanswerable query and both could disagree, so both are configuration errors
// rather than something to resolve at read time.
func (d *monitorDataSource) ConfigValidators(context.Context) []datasource.ConfigValidator {
	return []datasource.ConfigValidator{
		datasourcevalidator.ExactlyOneOf(path.MatchRoot("id"), path.MatchRoot("slug")),
	}
}

func (d *monitorDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	c, ok := req.ProviderData.(*client.Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected data source configure type",
			fmt.Sprintf("Expected *client.Client, got %T. This is a provider bug.", req.ProviderData))
		return
	}
	d.client = c
}

func (d *monitorDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg monitorDataModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var (
		out *client.Monitor
		err error
	)
	switch {
	case !cfg.ID.IsNull():
		out, err = d.client.GetMonitor(ctx, cfg.ID.ValueString())
	default:
		out, err = d.client.GetMonitorBySlug(ctx, cfg.Slug.ValueString())
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to read monitor", err.Error())
		return
	}

	state, diags := monitorDataFromAPI(ctx, out)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
