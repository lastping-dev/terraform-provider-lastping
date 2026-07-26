package provider

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/lastping-dev/terraform-provider-lastping/internal/client"
)

var (
	_ resource.Resource                   = (*monitorResource)(nil)
	_ resource.ResourceWithConfigure      = (*monitorResource)(nil)
	_ resource.ResourceWithImportState    = (*monitorResource)(nil)
	_ resource.ResourceWithValidateConfig = (*monitorResource)(nil)

	_ validator.String = notUUIDSlugValidator{}
	_ validator.String = rfc3339Validator{}
)

// slugPattern mirrors the server's own rule (api/slug.go: validateSlug in the
// LastPing monorepo). The server normalises (trim + lowercase) before it
// validates, so `  Rev-Case-Slug  ` would be accepted server-side and come back
// as `rev-case-slug` — a value Terraform never planned. A provider cannot fix
// that by rewriting the planned value (Terraform rejects a planned value that
// differs from a known config value), so the only correct answer is to refuse
// the un-normalised form at plan time.
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,48}[a-z0-9]$`)

// notUUIDSlugValidator rejects UUID-shaped slugs, which the server also rejects:
// they are ambiguous with a monitor id during import.
type notUUIDSlugValidator struct{}

func (notUUIDSlugValidator) Description(context.Context) string {
	return "must not be a UUID (ambiguous with a monitor id on import)"
}

func (v notUUIDSlugValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (notUUIDSlugValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if _, err := uuid.Parse(req.ConfigValue.ValueString()); err == nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid slug",
			"A slug must not be a UUID: it would be ambiguous with a monitor id when importing. "+
				"Choose a human-readable slug instead.")
	}
}

// rfc3339Validator rejects timestamps the API cannot parse, turning an opaque
// "invalid JSON body" 400 into a plan-time error on the right attribute.
type rfc3339Validator struct{}

func (rfc3339Validator) Description(context.Context) string { return "must be an RFC 3339 timestamp" }

func (v rfc3339Validator) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (rfc3339Validator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if _, err := time.Parse(time.RFC3339, req.ConfigValue.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid timestamp",
			fmt.Sprintf("%q is not an RFC 3339 timestamp (for example 2027-01-01T00:00:00Z).",
				req.ConfigValue.ValueString()))
	}
}

// NewMonitorResource returns a new lastping_monitor resource.
func NewMonitorResource() resource.Resource {
	return &monitorResource{}
}

type monitorResource struct {
	client *client.Client
}

// monitorResourceModel is the Terraform representation of a lastping_monitor.
//
// Optional+Computed is used only where the *server* genuinely supplies a value
// the configuration cannot: grace_s (floored to 2*probe_interval_s for http
// monitors), schedule_kind and period_s (derived from probe_interval_s for http
// monitors), tz (defaults to UTC), the probe_* defaults backed by column
// defaults, and the attributes carrying a provider-side Default. Everything
// else is plain Optional so that removing it from the configuration actually
// unsets it — a blanket Optional+Computed pins the previous value in state
// forever.
type monitorResourceModel struct {
	ID                   types.String `tfsdk:"id"`
	Name                 types.String `tfsdk:"name"`
	Slug                 types.String `tfsdk:"slug"`
	MonitorType          types.String `tfsdk:"monitor_type"`
	ScheduleKind         types.String `tfsdk:"schedule_kind"`
	PeriodS              types.Int64  `tfsdk:"period_s"`
	CronExpr             types.String `tfsdk:"cron_expr"`
	TZ                   types.String `tfsdk:"tz"`
	GraceS               types.Int64  `tfsdk:"grace_s"`
	Tags                 types.Set    `tfsdk:"tags"`
	RunawayCeiling       types.Int64  `tfsdk:"runaway_ceiling"`
	MonitorFrom          types.String `tfsdk:"monitor_from"`
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
	AlertAfter           types.String `tfsdk:"alert_after"`
	MaintenanceUntil     types.String `tfsdk:"maintenance_until"`
}

func (r *monitorResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_monitor"
}

func (r *monitorResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A LastPing monitor (\"check\"): a heartbeat, CI job, or HTTP probe that alerts " +
			"when it goes silent. Create sends `If-None-Match: *`, so a monitor whose slug already exists in " +
			"this project fails the apply with a clear error instead of silently being taken over — import it " +
			"instead.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Monitor UUID, assigned by the server.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Human-readable name.",
			},
			"slug": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Stable, project-scoped identifier used for import and for `If-None-Match` " +
					"collision detection. The API has no path to change a monitor's slug, so changing this " +
					"attribute replaces the resource. Must already be in normalised form: lowercase, trimmed, " +
					"matching `^[a-z0-9][a-z0-9-]{1,48}[a-z0-9]$`, and not UUID-shaped.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(slugPattern,
						"must be lowercase and trimmed, 3-50 characters of [a-z0-9-] starting and ending "+
							"alphanumeric (the server normalises slugs, so pass the lowercase/trimmed form "+
							"here — for example \"my-monitor\", not \" My-Monitor \")"),
					notUUIDSlugValidator{},
				},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"monitor_type": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString("heartbeat"),
				MarkdownDescription: "Kind of monitor: `heartbeat` (default, expects periodic pings), `ci` " +
					"(bound to a CI provider webhook), or `http` (active probe). The API treats this as " +
					"create-only — PATCH silently ignores it — so changing it here replaces the resource " +
					"rather than leaving a permanent, un-appliable diff.",
				Validators:    []validator.String{stringvalidator.OneOf("heartbeat", "ci", "http")},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"schedule_kind": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "`simple` (fixed `period_s` interval) or `cron` (`cron_expr` + `tz`). " +
					"Computed for `monitor_type = \"http\"`, which the server always schedules as `simple` " +
					"from `probe_interval_s`.",
				Validators: []validator.String{stringvalidator.OneOf("simple", "cron")},
			},
			"period_s": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Period in seconds, for `schedule_kind = \"simple\"`. Server-derived " +
					"(equal to `probe_interval_s`) for `monitor_type = \"http\"`.",
			},
			"cron_expr": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "5-field cron expression, for `schedule_kind = \"cron\"`.",
			},
			"tz": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "IANA timezone for cron evaluation. Defaults to `UTC` when unset.",
			},
			"grace_s": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Grace period in seconds after a deadline is missed before alerting. " +
					"Must be between 60 and 31536000 (one year). Required by the API for `heartbeat` and " +
					"`ci` monitors. For `monitor_type = \"http\"` the server floors the effective grace to " +
					"`2 * probe_interval_s`, so omit it to take the floor; a larger value is honoured as-is.",
				Validators: []validator.Int64{int64validator.Between(60, 31536000)},
			},
			"tags": schema.SetAttribute{
				Optional:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Labels attached to this monitor. Removing them from the configuration clears them.",
			},
			"runaway_ceiling": schema.Int64Attribute{
				Optional: true,
				MarkdownDescription: "Optional cap on pings per rolling 1-hour window. Exceeding it opens a " +
					"\"runaway\" incident. Omit to disable.",
			},
			"monitor_from": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "RFC 3339 timestamp from which deadlines are computed for a new monitor. " +
					"The API stores and returns UTC; a value written with a different offset is kept as " +
					"configured as long as it denotes the same instant.",
				Validators: []validator.String{rfc3339Validator{}},
			},
			"probe_url": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Absolute http(s) URL to probe. Required for `monitor_type = \"http\"`.",
			},
			"probe_method": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString("GET"),
				MarkdownDescription: "HTTP method used for the probe: `GET`, `HEAD`, or `POST`.",
				Validators:          []validator.String{stringvalidator.OneOf("GET", "HEAD", "POST")},
			},
			"probe_interval_s": schema.Int64Attribute{
				Optional:            true,
				MarkdownDescription: "Seconds between probes, for `monitor_type = \"http\"`. Between 30 and 86400.",
				Validators:          []validator.Int64{int64validator.Between(30, 86400)},
			},
			"probe_expected_body": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Substring the probe response body must contain to count as healthy.",
			},
			"probe_expected_status": schema.Int64Attribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "HTTP status code the probe must return. Omit to accept any 2xx.",
				Validators:          []validator.Int64{int64validator.Between(100, 599)},
			},
			"probe_timeout_s": schema.Int64Attribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "Per-probe timeout in seconds, between 1 and 30. Defaults to 10 server-side.",
				Validators:          []validator.Int64{int64validator.Between(1, 30)},
			},
			"probe_follow_redirects": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "Whether the probe follows HTTP redirects.",
			},
			"paused": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "When true, the monitor does not alert regardless of ping status. Maps " +
					"onto the `pause`/`resume` endpoints on update, not a PATCH field.",
			},
			"ping_url": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "URL your service should HTTP GET to record a successful ping.",
			},
			"status": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Current status derived from ping history: `new`, `up`, `late`, or `down`.",
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
			"alert_after": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "RFC 3339 UTC timestamp after which a missing ping raises an incident.",
			},
			"maintenance_until": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "RFC 3339 UTC timestamp until which alerting is suppressed for maintenance. " +
					"Set through the API or dashboard, not by Terraform.",
			},
		},
	}
}

// ValidateConfig rejects an http monitor whose grace_s is below the server's
// floor of 2*probe_interval_s. Without this the configuration plans cleanly and
// then fails the apply with "provider produced inconsistent result", because
// Terraform requires a known planned value to survive apply unchanged and the
// server would have raised the grace.
func (r *monitorResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg monitorResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if cfg.MonitorType.IsUnknown() || cfg.MonitorType.ValueString() != "http" {
		return
	}
	if cfg.GraceS.IsNull() || cfg.GraceS.IsUnknown() ||
		cfg.ProbeIntervalS.IsNull() || cfg.ProbeIntervalS.IsUnknown() {
		return
	}
	if floor := 2 * cfg.ProbeIntervalS.ValueInt64(); cfg.GraceS.ValueInt64() < floor {
		resp.Diagnostics.AddAttributeError(path.Root("grace_s"), "grace_s below the http probe floor",
			fmt.Sprintf("For monitor_type = \"http\" the server raises grace_s to 2 * probe_interval_s "+
				"(here: at least %d), so %d would never be applied as written.\n\nSet grace_s to at least %d, "+
				"or omit it to take the server floor.",
				floor, cfg.GraceS.ValueInt64(), floor))
	}
}

func (r *monitorResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	c, ok := req.ProviderData.(*client.Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected resource configure type",
			fmt.Sprintf("Expected *client.Client, got %T. This is a provider bug.", req.ProviderData))
		return
	}
	r.client = c
}

// monitorFromModel builds the API payload from the plan/config. Zero-value
// optional fields are omitted by the client's `omitempty` JSON tags.
func monitorFromModel(ctx context.Context, m monitorResourceModel) (client.Monitor, error) {
	out := client.Monitor{
		Name:                 m.Name.ValueString(),
		Slug:                 m.Slug.ValueString(),
		MonitorType:          m.MonitorType.ValueString(),
		ScheduleKind:         m.ScheduleKind.ValueString(),
		PeriodS:              m.PeriodS.ValueInt64(),
		CronExpr:             m.CronExpr.ValueString(),
		TZ:                   m.TZ.ValueString(),
		GraceS:               m.GraceS.ValueInt64(),
		ProbeURL:             m.ProbeURL.ValueString(),
		ProbeMethod:          m.ProbeMethod.ValueString(),
		ProbeIntervalS:       m.ProbeIntervalS.ValueInt64(),
		ProbeExpectedBody:    m.ProbeExpectedBody.ValueString(),
		ProbeExpectedStatus:  m.ProbeExpectedStatus.ValueInt64(),
		ProbeTimeoutS:        m.ProbeTimeoutS.ValueInt64(),
		ProbeFollowRedirects: m.ProbeFollowRedirects.ValueBool(),
	}
	if !m.RunawayCeiling.IsNull() && !m.RunawayCeiling.IsUnknown() {
		v := m.RunawayCeiling.ValueInt64()
		out.RunawayCeiling = &v
	}
	if !m.MonitorFrom.IsNull() && !m.MonitorFrom.IsUnknown() && m.MonitorFrom.ValueString() != "" {
		v := m.MonitorFrom.ValueString()
		out.MonitorFrom = &v
	}
	if !m.Tags.IsNull() && !m.Tags.IsUnknown() {
		var tags []string
		if err := m.Tags.ElementsAs(ctx, &tags, false); err != nil {
			return out, fmt.Errorf("read tags: %v", err)
		}
		out.Tags = tags
	}
	return out, nil
}

// stringOrNull maps the API's empty string — which is how it reports "unset" for
// every purely user-supplied string field — back to null, so an attribute
// removed from the configuration reads back as removed rather than as "".
func stringOrNull(v string) types.String {
	if v == "" {
		return types.StringNull()
	}
	return types.StringValue(v)
}

// int64OrNull is stringOrNull for the numeric equivalents (the API omits them
// entirely when unset, which decodes as 0).
func int64OrNull(v int64) types.Int64 {
	if v == 0 {
		return types.Int64Null()
	}
	return types.Int64Value(v)
}

// timestampOrNull maps an optional API timestamp to state.
func timestampOrNull(v *string) types.String {
	if v == nil || *v == "" {
		return types.StringNull()
	}
	return types.StringValue(*v)
}

// tagsValue maps the API's tag list to state. The API always returns an array,
// never null, so "no tags" has to be mapped back onto whichever empty form the
// configuration used — null when tags is absent, [] when it is explicitly
// empty — or the applied state would not match the planned state.
func tagsValue(ctx context.Context, apiTags []string, prior types.Set) (types.Set, diag.Diagnostics) {
	if len(apiTags) == 0 {
		// Only reuse prior's shape when prior was itself empty (an explicit
		// tags = []). A non-empty prior means the API actually dropped tags
		// that Terraform thought were still set — that must surface as null,
		// not be masked by echoing the stale value back.
		if prior.IsNull() || prior.IsUnknown() || len(prior.Elements()) > 0 {
			return types.SetNull(types.StringType), nil
		}
		return prior, nil
	}
	return types.SetValueFrom(ctx, types.StringType, apiTags)
}

// monitorFromValue keeps the configured spelling of monitor_from when the API's
// UTC-normalised answer denotes the same instant. The API stores timestamptz and
// always answers in UTC, so "2027-01-01T00:00:00+01:00" comes back as
// "2026-12-31T23:00:00Z"; without this the apply fails as an inconsistent result.
func monitorFromValue(apiVal *string, prior types.String) types.String {
	if apiVal == nil || *apiVal == "" {
		return types.StringNull()
	}
	if !prior.IsNull() && !prior.IsUnknown() {
		got, errGot := time.Parse(time.RFC3339, *apiVal)
		want, errWant := time.Parse(time.RFC3339, prior.ValueString())
		if errGot == nil && errWant == nil && got.Equal(want) {
			return prior
		}
	}
	return types.StringValue(*apiVal)
}

// modelFromMonitor builds Terraform state from an API response. prior is the
// plan (create/update) or the previous state (read); it is only consulted where
// the API's answer is semantically but not textually identical to what Terraform
// planned — see tagsValue and monitorFromValue.
func modelFromMonitor(ctx context.Context, mon *client.Monitor, prior monitorResourceModel) (monitorResourceModel, error) {
	m := monitorResourceModel{
		ID:           types.StringValue(mon.ID),
		Name:         types.StringValue(mon.Name),
		Slug:         stringOrNull(mon.Slug),
		MonitorType:  types.StringValue(mon.MonitorType),
		ScheduleKind: types.StringValue(mon.ScheduleKind),
		PeriodS:      types.Int64Value(mon.PeriodS),
		CronExpr:     stringOrNull(mon.CronExpr),
		TZ:           types.StringValue(mon.TZ),
		GraceS:       types.Int64Value(mon.GraceS),
		ProbeURL:     stringOrNull(mon.ProbeURL),
		ProbeMethod:  types.StringValue(mon.ProbeMethod),

		ProbeIntervalS:       int64OrNull(mon.ProbeIntervalS),
		ProbeExpectedBody:    stringOrNull(mon.ProbeExpectedBody),
		ProbeExpectedStatus:  types.Int64Value(mon.ProbeExpectedStatus),
		ProbeTimeoutS:        types.Int64Value(mon.ProbeTimeoutS),
		ProbeFollowRedirects: types.BoolValue(mon.ProbeFollowRedirects),

		Paused:           types.BoolValue(mon.Paused),
		Status:           types.StringValue(mon.Status),
		PingURL:          types.StringValue(mon.PingURL),
		CreatedAt:        types.StringValue(mon.CreatedAt),
		LastPingAt:       timestampOrNull(mon.LastPingAt),
		DueAt:            timestampOrNull(mon.DueAt),
		AlertAfter:       timestampOrNull(mon.AlertAfter),
		MaintenanceUntil: timestampOrNull(mon.MaintenanceUntil),
	}
	if mon.RunawayCeiling != nil {
		m.RunawayCeiling = types.Int64Value(*mon.RunawayCeiling)
	} else {
		m.RunawayCeiling = types.Int64Null()
	}
	m.MonitorFrom = monitorFromValue(mon.MonitorFrom, prior.MonitorFrom)

	tagsSet, diags := tagsValue(ctx, mon.Tags, prior.Tags)
	if diags.HasError() {
		return m, fmt.Errorf("build tags set: %v", diags)
	}
	m.Tags = tagsSet
	return m, nil
}

func (r *monitorResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan monitorResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	payload, err := monitorFromModel(ctx, plan)
	if err != nil {
		resp.Diagnostics.AddError("Unable to build monitor payload", err.Error())
		return
	}

	// Create — turn a slug collision into an actionable diagnostic rather than
	// silently adopting a monitor this configuration does not own.
	out, err := r.client.CreateMonitor(ctx, payload)
	if err != nil {
		if client.IsPreconditionFailed(err) {
			resp.Diagnostics.AddError(
				"Monitor already exists",
				fmt.Sprintf("A monitor with slug %q already exists in this project. Terraform will not "+
					"take over a monitor it did not create.\n\nTo manage the existing monitor, import it:\n"+
					"  terraform import lastping_monitor.<name> %s\n\nOr choose a different slug.",
					plan.Slug.ValueString(), plan.Slug.ValueString()),
			)
			return
		}
		resp.Diagnostics.AddError("Unable to create monitor", err.Error())
		return
	}

	// paused is applied after create: the create payload has no paused field,
	// and a freshly created monitor always starts unpaused.
	if !plan.Paused.IsNull() && !plan.Paused.IsUnknown() && plan.Paused.ValueBool() {
		if err := r.client.SetMonitorPaused(ctx, out.ID, true); err != nil {
			resp.Diagnostics.AddError("Unable to pause monitor", err.Error())
			return
		}
		out.Paused = true
	}

	state, err := modelFromMonitor(ctx, out, plan)
	if err != nil {
		resp.Diagnostics.AddError("Unable to process monitor response", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *monitorResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state monitorResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Read — a deleted monitor must be removed from state, not error the plan.
	out, err := r.client.GetMonitor(ctx, state.ID.ValueString())
	if err != nil {
		if client.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Unable to read monitor", err.Error())
		return
	}

	newState, err := modelFromMonitor(ctx, out, state)
	if err != nil {
		resp.Diagnostics.AddError("Unable to process monitor response", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// monitorPatchNeeded reports whether any PATCH-able attribute actually differs
// between the plan and the current state. `paused` is not one of them — it is
// applied through the pause/resume endpoints — so a plan that only pauses a
// monitor must not send a PATCH, which would needlessly rewrite the schedule
// and reset the server-side deadlines.
func monitorPatchNeeded(ctx context.Context, plan, state monitorResourceModel) (bool, error) {
	planPayload, err := monitorFromModel(ctx, plan)
	if err != nil {
		return false, err
	}
	statePayload, err := monitorFromModel(ctx, state)
	if err != nil {
		return false, err
	}
	// monitorFromModel only populates request fields, so response-only state
	// (status, deadlines, paused) cannot masquerade as a configuration change.
	return !reflect.DeepEqual(planPayload, statePayload), nil
}

func (r *monitorResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state monitorResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	id := state.ID.ValueString()

	planPayload, err := monitorFromModel(ctx, plan)
	if err != nil {
		resp.Diagnostics.AddError("Unable to build monitor payload", err.Error())
		return
	}
	patchNeeded, err := monitorPatchNeeded(ctx, plan, state)
	if err != nil {
		resp.Diagnostics.AddError("Unable to build monitor payload", err.Error())
		return
	}

	var out *client.Monitor
	if patchNeeded {
		out, err = r.client.UpdateMonitor(ctx, id, planPayload)
		if err != nil {
			resp.Diagnostics.AddError("Unable to update monitor", err.Error())
			return
		}
	} else {
		out, err = r.client.GetMonitor(ctx, id)
		if err != nil {
			resp.Diagnostics.AddError("Unable to read monitor", err.Error())
			return
		}
	}

	// Update — paused maps onto the pause/resume endpoints, not a PATCH field.
	if !plan.Paused.Equal(state.Paused) {
		if err := r.client.SetMonitorPaused(ctx, id, plan.Paused.ValueBool()); err != nil {
			resp.Diagnostics.AddError("Unable to change paused state", err.Error())
			return
		}
		out.Paused = plan.Paused.ValueBool()
	}

	newState, err := modelFromMonitor(ctx, out, plan)
	if err != nil {
		resp.Diagnostics.AddError("Unable to process monitor response", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *monitorResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state monitorResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.client.DeleteMonitor(ctx, state.ID.ValueString()); err != nil && !client.IsNotFound(err) {
		resp.Diagnostics.AddError("Unable to delete monitor", err.Error())
		return
	}
}

// ImportState accepts a slug or a UUID. Slug is tried first: monitor slugs are
// project-scoped, so a match against the caller's own project is unambiguous,
// whereas trying uuid.Parse first would mean a monitor whose slug happens to
// be UUID-shaped (legacy rows created before the API rejected UUID-shaped
// slugs) could import the wrong monitor.
func (r *monitorResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	id := req.ID

	m, err := r.client.GetMonitorBySlug(ctx, id)
	switch {
	case err == nil:
		resource.ImportStatePassthroughID(ctx, path.Root("id"), resource.ImportStateRequest{ID: m.ID}, resp)
		return
	case !client.IsNotFound(err):
		// The lookup itself failed (network, 5xx, bad credentials). Reporting
		// that as "no such monitor" would send the operator hunting for a
		// missing resource instead of a broken backend.
		resp.Diagnostics.AddError("Unable to look up monitor for import",
			fmt.Sprintf("Listing monitors to resolve %q failed: %s", id, err))
		return
	}

	if _, uerr := uuid.Parse(id); uerr == nil {
		resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
		return
	}

	resp.Diagnostics.AddError("Unable to import monitor",
		fmt.Sprintf("No monitor found with slug or ID %q in this project.", id))
}
