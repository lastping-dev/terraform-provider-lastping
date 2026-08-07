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
	"github.com/hashicorp/terraform-plugin-framework/attr"
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
// monitors), tz (defaults to UTC), failure_threshold (column default 1, always
// returned), the probe_* defaults backed by column defaults, and the attributes
// carrying a provider-side Default. Everything else is plain Optional so that
// removing it from the configuration actually unsets it — a blanket
// Optional+Computed pins the previous value in state forever.
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
	MaxRuntimeS          types.Int64  `tfsdk:"max_runtime_s"`
	StepTimeoutS         types.Int64  `tfsdk:"step_timeout_s"`
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
	AlertAfter           types.String `tfsdk:"alert_after"`
	MaintenanceUntil     types.String `tfsdk:"maintenance_until"`

	// Assertions is the `assertion` nested block set. It is NOT part of the
	// monitor payload: assertions are a sub-resource with their own
	// replace-the-set PUT, so they are read and written separately from every
	// other attribute here. See monitor_assertions.go.
	Assertions types.Set `tfsdk:"assertion"`
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
			"max_runtime_s": schema.Int64Attribute{
				Optional: true,
				MarkdownDescription: "How long a single run may take, in seconds, before it is called overdue. " +
					"Must be between 60 and 31536000 (one year). Measured from a `/start` ping that has not been " +
					"matched by a completion, so it only means anything on a monitor that reports both.\n\n" +
					"This replaces `grace_s` for the **overrun deadline only**: the silence rule " +
					"(`due_at + grace_s`) and the first-run seed (`monitor_from + grace_s`) keep using `grace_s`. " +
					"That split is the point — it is what makes \"alert if silent for more than 10 minutes, but " +
					"allow a 4-hour run\" expressible. Omit it and the overrun budget falls back to `grace_s`, " +
					"exactly as before this attribute existed; removing it from the configuration restores that " +
					"fallback.\n\n" +
					"~> **Not supported on `monitor_type = \"http\"`.** A probe has no start/success pair — the " +
					"prober mints a fresh run id for every probe — so no start is ever outstanding and the " +
					"overrun rule can never fire. The API rejects it with 400 `MAX_RUNTIME_NOT_SUPPORTED`, and " +
					"this provider rejects it at plan time. Use `probe_timeout_s` to bound a single probe.",
				Validators: []validator.Int64{int64validator.Between(60, 31536000)},
			},
			"step_timeout_s": schema.Int64Attribute{
				Optional: true,
				MarkdownDescription: "How long an armed run may go without reporting a **step** before a " +
					"`stalled` incident opens, in seconds. Between 10 and 86400. Opt-in: omit it and stall " +
					"detection is off, which is how every monitor behaved before this attribute existed; " +
					"removing it from the configuration turns it back off.\n\n" +
					"A step is a liveness marker the job posts inside a run it has already started " +
					"(`POST /ping/{id}/step?rid=…&name=…`). `max_runtime_s` alone only catches a wedged job " +
					"once its whole run budget is spent; `step_timeout_s` catches it within one step interval " +
					"and names the last step that did report. A job that never posts steps will therefore " +
					"open a `stalled` incident on **every** run — set this attribute and instrument the job in " +
					"the same change.\n\n" +
					"~> **It must be strictly less than the effective run budget**, which is `max_runtime_s`, " +
					"or `grace_s` when `max_runtime_s` is unset. The stall window is the interval between the " +
					"step timeout and the end of the budget, so a value at or above the budget leaves that " +
					"window empty and the rule could never fire — the API rejects it with 400 " +
					"`STEP_TIMEOUT_EXCEEDS_BUDGET` rather than storing a setting that does nothing. The " +
					"provider anticipates that at plan time whenever both values are known in the " +
					"configuration.\n\n" +
					"~> **Not supported on `monitor_type = \"http\"`.** A probe never arms a run and has no " +
					"`/step` endpoint to call, so the stall rule is unreachable there. The API rejects it " +
					"with 400 `STEP_TIMEOUT_NOT_SUPPORTED`, and this provider rejects it at plan time. Use " +
					"`probe_timeout_s` to bound a single probe.",
				Validators: []validator.Int64{int64validator.Between(10, 86400)},
			},
			"failure_threshold": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "How many **consecutive** explicit failures are needed before an incident " +
					"opens — retry-before-alert. Between 1 and 100; the server defaults it to `1`, meaning " +
					"\"open on the first failure\".\n\n" +
					"It gates the `fail` cause **only**. `silence`, `overrun`, `never_started` and `runaway` are " +
					"time- or rate-based, so a consecutive-failure count is meaningless for them and never gates " +
					"them — raising this does not buy a late monitor any extra time. Below the threshold nothing " +
					"alerts and the status does not change, but the ping is still recorded, an outstanding run is " +
					"still ended and the deadlines are still recomputed. Any success resets the counter.\n\n" +
					"~> **There is no \"unset\" to go back to.** The column is `NOT NULL DEFAULT 1`, so the API " +
					"cannot clear this the way it clears `runaway_ceiling`: removing the attribute from the " +
					"configuration leaves the stored value in place rather than returning it to `1`. Set it to " +
					"`1` explicitly to get the default behaviour back. Changing it also does not reset the " +
					"monitor's current failure count, so lowering it can open an incident on the very next " +
					"failure.",
				Validators: []validator.Int64{int64validator.Between(1, 100)},
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
			"agent_id": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Attach this monitor to the `lastping_agent` that owns it, for " +
					"example `lastping_agent.nightly_etl.id`. An in-place update: the API applies it " +
					"through the same `PATCH` as every other attribute here, so changing it re-attaches " +
					"the monitor rather than replacing the resource. Removing it from the configuration " +
					"detaches the monitor (`agent_id` goes back to null) without deleting either resource — " +
					"the same as setting it to `null` explicitly.\n\n" +
					"~> **Naming an agent that does not exist, or belongs to another project, is a plan-time " +
					"or apply-time error — never an implicit `register_agent`.** The API answers with 400 " +
					"`UNKNOWN_AGENT`.\n\n" +
					"~> **Reference the agent's `id`, not its `slug`, even though the API itself accepts " +
					"either.** The API always echoes back the canonical UUID — `GET`/`PATCH` on a monitor " +
					"never reports the slug it was attached with — so writing a slug here would apply " +
					"cleanly and then fail with \"provider produced inconsistent result after apply\" on " +
					"every subsequent plan, because the state Terraform is required to match is the UUID, " +
					"not the string the configuration wrote. This provider enforces the UUID form at plan " +
					"time for that reason, not because the API itself is that strict.",
				Validators: []validator.String{uuidValidator{}},
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
		Blocks: map[string]schema.Block{
			"assertion": monitorAssertionBlock(),
		},
	}
}

// ValidateConfig rejects the configurations the server will not honour as
// written. They are caught here rather than at apply time, because a plan-time
// error names the attribute and costs nothing, while the apply-time failures
// are opaque: an inconsistent-result panic for the first and a bare 400 for the
// rest, all after Terraform has already started changing things.
//
// The http-monitor rules:
//
//   - grace_s below the server's floor of 2*probe_interval_s. Terraform requires
//     a known planned value to survive apply unchanged, and the server would
//     have raised the grace — so the apply dies with "provider produced
//     inconsistent result".
//
//   - max_runtime_s set at all. An http probe has no start/success pair, so the
//     overrun rule can never fire and the API answers 400
//     MAX_RUNTIME_NOT_SUPPORTED. Only a non-null configured value is rejected:
//     an omitted max_runtime_s still reaches the API as an explicit null on
//     update (see monitorPatchFromModel), which the API accepts as a no-op.
//
//   - step_timeout_s set at all, for the stricter version of the same reason:
//     an http probe never arms a run and has no /step endpoint, so the stall
//     rule is unreachable. 400 STEP_TIMEOUT_NOT_SUPPORTED, and — as with
//     max_runtime_s — the explicit null the update path always sends is a no-op
//     the API accepts.
//
// And two rules that are not about http at all: step_timeout_s must be strictly
// below the effective run budget (see validateStepTimeoutBudget), and every
// `assertion` block must be one core/assertion.Validate would accept (see
// validateAssertions, which also refuses assertions on an http monitor — a
// probe has no ping body, so they could never be evaluated).
func (r *monitorResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg monitorResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Assertions are validated for every monitor type — the http rule for them
	// lives inside validateAssertions — so this runs before the http early
	// return below rather than inside either branch.
	validateAssertions(ctx, cfg, resp)

	if cfg.MonitorType.IsUnknown() || cfg.MonitorType.ValueString() != "http" {
		// Not an http monitor, so step_timeout_s is legal — but it still has to
		// fit inside the run budget. The check is skipped for http monitors
		// below, where the attribute is refused outright and the server's own
		// grace floor (2*probe_interval_s) would make any budget quoted here
		// wrong anyway.
		validateStepTimeoutBudget(cfg, resp)
		return
	}

	if !cfg.StepTimeoutS.IsNull() && !cfg.StepTimeoutS.IsUnknown() {
		resp.Diagnostics.AddAttributeError(path.Root("step_timeout_s"),
			"step_timeout_s is not supported on an http monitor",
			"The API rejects step_timeout_s on monitor_type = \"http\" with 400 "+
				"STEP_TIMEOUT_NOT_SUPPORTED. An HTTP probe never arms a run — the prober mints a fresh "+
				"run id for every probe, and there is no /step endpoint a probe could call — so the "+
				"stall rule can never fire.\n\nRemove step_timeout_s. To bound a single probe, use "+
				"probe_timeout_s.")
	}

	if !cfg.MaxRuntimeS.IsNull() && !cfg.MaxRuntimeS.IsUnknown() {
		resp.Diagnostics.AddAttributeError(path.Root("max_runtime_s"),
			"max_runtime_s is not supported on an http monitor",
			"The API rejects max_runtime_s on monitor_type = \"http\" with 400 "+
				"MAX_RUNTIME_NOT_SUPPORTED. An HTTP probe has no start/success pair — the prober mints a "+
				"fresh run id for every probe — so no start is ever outstanding and the overrun rule can "+
				"never fire.\n\nRemove max_runtime_s. To bound a single probe, use probe_timeout_s.")
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

// validateStepTimeoutBudget mirrors the server's STEP_TIMEOUT_EXCEEDS_BUDGET
// rule: step_timeout_s must be STRICTLY less than the effective run budget,
// which is max_runtime_s, or grace_s when max_runtime_s is unset.
//
// The rule is not a style preference. The stall window is the interval between
// the step timeout and the end of the run budget, so a step_timeout_s at or
// above the budget leaves it empty and the rule can never fire, for any input —
// the monitor would carry stall detection that is permanently, invisibly off.
// That is why the API refuses it instead of storing it.
//
// It is only checked when the answer is knowable from the configuration alone:
//
//   - step_timeout_s null or unknown — nothing to check.
//   - max_runtime_s unknown — the budget itself is unknown. Deferred to the API.
//   - max_runtime_s known and non-null — it IS the budget. Compare against it.
//   - max_runtime_s null — the budget is grace_s. grace_s is Optional+Computed,
//     so a configuration that omits it leaves the server to supply the value
//     (an http probe floor, or the stored value on update) and the budget is
//     unknowable here; only a grace_s written in the configuration is compared.
//
// Deferring those cases costs nothing but a later error: the API still enforces
// the rule on the merged check, which is the only place it can be enforced in
// full — a PATCH that raises grace_s alone can push an already-stored
// step_timeout_s over the budget, and the configuration under validation here
// need not mention either attribute.
func validateStepTimeoutBudget(cfg monitorResourceModel, resp *resource.ValidateConfigResponse) {
	if cfg.StepTimeoutS.IsNull() || cfg.StepTimeoutS.IsUnknown() || cfg.MaxRuntimeS.IsUnknown() {
		return
	}

	budget, source := cfg.MaxRuntimeS.ValueInt64(), "max_runtime_s"
	if cfg.MaxRuntimeS.IsNull() {
		if cfg.GraceS.IsNull() || cfg.GraceS.IsUnknown() {
			return
		}
		budget, source = cfg.GraceS.ValueInt64(), "grace_s"
	}

	if step := cfg.StepTimeoutS.ValueInt64(); step >= budget {
		resp.Diagnostics.AddAttributeError(path.Root("step_timeout_s"),
			"step_timeout_s is not below the run budget",
			fmt.Sprintf("step_timeout_s (%d) must be strictly less than the effective run budget, "+
				"which is %s = %d here. The stall rule only fires between the step timeout and the end "+
				"of that budget, so %d could never fire — the API rejects it with 400 "+
				"STEP_TIMEOUT_EXCEEDS_BUDGET.\n\nLower step_timeout_s below %d, or raise the budget "+
				"above it.", step, source, budget, step, budget))
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

// monitorFromModel builds the *create* payload from the plan. Zero-value
// optional fields are omitted by the client's `omitempty` JSON tags, which on
// create correctly means "take the server default".
//
// Update does not use this: an omitted key cannot clear anything. See
// monitorPatchFromModel. It is still the right shape for monitorPatchNeeded,
// which only ever compares two models against each other.
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
		FailureThreshold:     m.FailureThreshold.ValueInt64(),
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
	if !m.MaxRuntimeS.IsNull() && !m.MaxRuntimeS.IsUnknown() {
		v := m.MaxRuntimeS.ValueInt64()
		out.MaxRuntimeS = &v
	}
	if !m.StepTimeoutS.IsNull() && !m.StepTimeoutS.IsUnknown() {
		v := m.StepTimeoutS.ValueInt64()
		out.StepTimeoutS = &v
	}
	if !m.MonitorFrom.IsNull() && !m.MonitorFrom.IsUnknown() && m.MonitorFrom.ValueString() != "" {
		v := m.MonitorFrom.ValueString()
		out.MonitorFrom = &v
	}
	// Attach at create time when configured. Empty/unknown is left as the zero
	// value, which client.Monitor's `omitempty` drops from the wire — exactly
	// "no attachment", the same as omitting the field entirely.
	out.AgentID = m.AgentID.ValueString()
	if !m.Tags.IsNull() && !m.Tags.IsUnknown() {
		var tags []string
		if err := m.Tags.ElementsAs(ctx, &tags, false); err != nil {
			return out, fmt.Errorf("read tags: %v", err)
		}
		out.Tags = tags
	}
	return out, nil
}

// monitorPatchFromModel builds the body of PATCH /api/v1/checks/{id}.
//
// It takes two models:
//
//   - desired is the plan with unknowns resolved from the prior state (see
//     resolveUnknownsFromState), so every value in it is concrete. It supplies
//     the values — and, with the current schema, presence too: the six
//     clearable attributes are Optional-only, so removing one from the
//     configuration plans it as null.
//   - cfg is the practitioner's literal configuration. Its null-ness is checked
//     as well, but strictly as belt-and-braces: while those attributes stay
//     Optional-only, cfg can never be null where desired holds a value, so
//     dropping the argument would not change a byte of any payload today.
//
// The cfg check is kept because it is the signal that stays correct if one of
// the four is ever made Optional+Computed — and that is exactly the change to
// be careful about. terraform-plugin-framework marks an Optional+Computed
// attribute absent from the configuration as unknown, which
// resolveUnknownsFromState then fills from prior state; cfg would be null while
// desired carried the stored value, and a function trusting desired alone would
// send an explicit null and destroy tags on an apply that only renamed the
// monitor. monitorPatchNeeded would not gate it either, because it compares
// desired against state and the two would agree.
//
// TestMonitorOptionalOnlyAttributesAreNotComputed is the guardrail that keeps
// tags, runaway_ceiling, monitor_from, max_runtime_s, step_timeout_s and
// agent_id Optional-only. Read it before making any of them Computed to
// suppress drift.
//
// The document is sparse (see client.MonitorPatch) and its keys fall into three
// groups:
//
//  1. Clearable — tags, runaway_ceiling, monitor_from, max_runtime_s,
//     step_timeout_s, agent_id. Absent
//     from the configuration, they are sent as an explicit JSON null. Under
//     merge-patch that is the only way to clear them; under the older
//     full-replace server a null decodes to a nil slice / nil pointer and clears
//     them just as an absent key did. One document is therefore correct against
//     both servers.
//
//  2. Meaningful zero — cron_expr, probe_expected_body,
//     probe_follow_redirects. Always sent, never dropped. A configured "" has
//     to be able to delete a cron expression or a body assertion, and a
//     configured false has to be able to turn redirect-following off; dropping
//     the zero makes all three states unreachable, because the stored value
//     survives instead.
//
//  3. Everything else — sent only when non-zero, which is exactly what the
//     `omitempty` tags used to do. These are the Optional+Computed and
//     create-only attributes, and they must keep being sent: omitting them is
//     only safe against a server that merges, and this provider still has to
//     work against one that replaces. Narrowing them is a separate change.
func monitorPatchFromModel(ctx context.Context, desired, cfg monitorResourceModel) (client.MonitorPatch, error) {
	patch := client.MonitorPatch{
		"name": desired.Name.ValueString(),

		// Group 2.
		"cron_expr":              desired.CronExpr.ValueString(),
		"probe_expected_body":    desired.ProbeExpectedBody.ValueString(),
		"probe_follow_redirects": desired.ProbeFollowRedirects.ValueBool(),
	}

	// Group 3.
	putString := func(key string, v types.String) {
		if s := v.ValueString(); s != "" {
			patch[key] = s
		}
	}
	putInt64 := func(key string, v types.Int64) {
		if n := v.ValueInt64(); n != 0 {
			patch[key] = n
		}
	}
	putString("slug", desired.Slug)
	putString("monitor_type", desired.MonitorType)
	putString("schedule_kind", desired.ScheduleKind)
	putInt64("period_s", desired.PeriodS)
	putString("tz", desired.TZ)
	putInt64("grace_s", desired.GraceS)
	putString("probe_url", desired.ProbeURL)
	putString("probe_method", desired.ProbeMethod)
	putInt64("probe_interval_s", desired.ProbeIntervalS)
	putInt64("probe_expected_status", desired.ProbeExpectedStatus)
	putInt64("probe_timeout_s", desired.ProbeTimeoutS)
	// failure_threshold belongs here and not in group 1: the column is
	// NOT NULL DEFAULT 1, so the API has no "cleared" state to send it to and
	// reads an explicit null as an omission. Its valid range starts at 1, so
	// omit-when-zero never drops a value a practitioner could have written.
	putInt64("failure_threshold", desired.FailureThreshold)

	// Group 1. A nil value marshals to JSON null, which clears.
	if cfg.RunawayCeiling.IsNull() || desired.RunawayCeiling.IsNull() {
		patch["runaway_ceiling"] = nil
	} else {
		patch["runaway_ceiling"] = desired.RunawayCeiling.ValueInt64()
	}

	// max_runtime_s is clearable, so it needs the explicit-null form rather than
	// omit-when-zero: dropping the key would make "go back to the grace_s
	// fallback" unreachable, which is the bug tags, runaway_ceiling and
	// monitor_from all had. The null is safe to send even on an http monitor,
	// where any non-null value is a 400 — the API accepts a null there as a
	// no-op, and ValidateConfig has already refused a configured value.
	if cfg.MaxRuntimeS.IsNull() || desired.MaxRuntimeS.IsNull() {
		patch["max_runtime_s"] = nil
	} else {
		patch["max_runtime_s"] = desired.MaxRuntimeS.ValueInt64()
	}

	// step_timeout_s is clearable in exactly the same way, and belongs in this
	// group for exactly the same reason: an absent key under merge-patch leaves
	// the stored timeout in place, so "turn stall detection back off" would be
	// unreachable through Terraform. Its range starts at 10, so omit-when-zero
	// would have looked harmless while quietly pinning the attribute forever.
	// The null is safe on an http monitor too — the API keys its
	// STEP_TIMEOUT_NOT_SUPPORTED refusal off a non-null incoming value, and
	// ValidateConfig has already refused a configured one.
	if cfg.StepTimeoutS.IsNull() || desired.StepTimeoutS.IsNull() {
		patch["step_timeout_s"] = nil
	} else {
		patch["step_timeout_s"] = desired.StepTimeoutS.ValueInt64()
	}

	if cfg.MonitorFrom.IsNull() || desired.MonitorFrom.ValueString() == "" {
		patch["monitor_from"] = nil
	} else {
		patch["monitor_from"] = desired.MonitorFrom.ValueString()
	}

	// agent_id is clearable for the same reason as the rest of this group: an
	// absent key under merge-patch leaves the stored attachment alone, so
	// "detach this monitor from its agent" would be unreachable through
	// Terraform if the key were only ever omitted. api/checks_patch.go resolves
	// an explicit null to SetCheckAgent(agent_id = NULL) — always a 200, never
	// a 400, so sending it on every PATCH (even one that never had an
	// attachment) is a harmless no-op, exactly like monitor_from and the
	// budgets above.
	if cfg.AgentID.IsNull() || desired.AgentID.IsNull() {
		patch["agent_id"] = nil
	} else {
		patch["agent_id"] = desired.AgentID.ValueString()
	}

	// No IsUnknown() arm here on purpose: desired has been through
	// resolveUnknownsFromState and prior state never holds unknowns, so tags
	// cannot be unknown at this point. Adding the arm back would also pick the
	// wrong default — "I don't know what the tags are" must never resolve to
	// "clear them".
	if cfg.Tags.IsNull() || desired.Tags.IsNull() {
		patch["tags"] = nil
	} else {
		// An explicit `tags = []` lands here and sends `[]` rather than null.
		// Both clear the tags, but the empty array says what was configured.
		// ElementsAs yields a non-nil zero-length slice for an empty set, so no
		// nil-normalising is needed to get `[]` instead of `null` on the wire.
		var tags []string
		if diags := desired.Tags.ElementsAs(ctx, &tags, false); diags.HasError() {
			return nil, fmt.Errorf("read tags: %v", diags)
		}
		patch["tags"] = tags
	}

	return patch, nil
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

		// agent_id: the API omits it entirely when the monitor is unattached
		// (checkResponse.AgentID has `omitempty`), and always reports the
		// canonical UUID — never the slug a caller may have attached it with.
		AgentID: stringOrNull(mon.AgentID),

		// failure_threshold is NOT NULL DEFAULT 1 server-side and always comes
		// back, so it is a concrete number rather than an int64OrNull.
		FailureThreshold: types.Int64Value(mon.FailureThreshold),

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
	// The API omits max_runtime_s entirely when it is unset, which is the
	// "fall back to grace_s" state and has to read as null, not 0.
	if mon.MaxRuntimeS != nil {
		m.MaxRuntimeS = types.Int64Value(*mon.MaxRuntimeS)
	} else {
		m.MaxRuntimeS = types.Int64Null()
	}
	// Likewise for step_timeout_s: absent means stall detection is off, which
	// has to read as null rather than as a 0 no practitioner could have written
	// (the valid range starts at 10).
	if mon.StepTimeoutS != nil {
		m.StepTimeoutS = types.Int64Value(*mon.StepTimeoutS)
	} else {
		m.StepTimeoutS = types.Int64Null()
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

	// Assertions are a sub-resource, applied after the monitor exists. A freshly
	// created monitor has none, so the current set is known to be empty without
	// asking — and when the configuration has no assertion blocks either, there
	// is nothing to write at all.
	desiredAssertions, err := assertionsFromModel(ctx, plan.Assertions)
	if err != nil {
		resp.Diagnostics.AddError("Unable to build assertions payload", err.Error())
		return
	}
	appliedAssertions, err := r.syncAssertions(ctx, out.ID, desiredAssertions, []client.Assertion{})
	if err != nil {
		// The monitor itself exists. Saving state before returning the error is
		// what stops the next apply from trying to create it a second time and
		// hitting the If-None-Match slug collision instead of retrying the
		// assertions.
		resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
		resp.Diagnostics.AddError("Unable to set monitor assertions",
			fmt.Sprintf("The monitor was created, but its output assertions were not written: %s\n\n"+
				"The monitor is in state; re-running the apply will retry the assertions alone.", err))
		return
	}
	assertionSet, diags := assertionsToModel(ctx, appliedAssertions, plan.Assertions)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	state.Assertions = assertionSet

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

	// Assertions live behind their own endpoint, so refreshing the monitor does
	// not refresh them. Without this call an assertion added or removed outside
	// Terraform is invisible to `terraform plan` — the attribute would only ever
	// echo back whatever the last apply wrote.
	current, err := r.client.GetAssertions(ctx, state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Unable to read monitor assertions", err.Error())
		return
	}
	assertionSet, diags := assertionsToModel(ctx, current, state.Assertions)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	newState.Assertions = assertionSet

	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// resolveUnknownsFromState returns plan with every unknown attribute replaced by
// the value the prior state holds for it.
//
// This exists because of the way terraform-plugin-framework plans an
// Optional+Computed attribute that the configuration omits: it marks it UNKNOWN
// in the plan (fwserver.MarkComputedNilsAsUnknown keys off the *config*, not
// the proposed new state), throwing away the prior value Terraform core had
// already carried forward. `types.Int64.ValueInt64()` on an unknown returns 0
// and `ValueString()` returns "", so the payload builder then invents a zero
// nobody wrote — and against the full-replace `PATCH /api/v1/checks/{id}` this
// provider still has to support, that zero lands.
//
// The reported failure: grace_s = 1800 in state and in production, a
// configuration omitting grace_s, an apply touching only `tags` — and
// `check: grace 0s outside [60, 31536000]`, apply stopped dead. The silent
// variants were worse: on an http monitor the same PATCH reset probe_timeout_s
// and probe_expected_status to server defaults and turned probe_follow_redirects
// off, with no error at all.
//
// Only unknowns are resolved, never nulls. A null is a real instruction: a
// plain Optional attribute (cron_expr, runaway_ceiling, probe_interval_s,
// probe_url, probe_expected_body, monitor_from, tags) removed from the
// configuration plans as null and has to reach the API as a clear — for the
// clearable attributes that means an explicit JSON null, see
// monitorPatchFromModel. Carrying nulls forward too would pin every removed
// attribute in place forever.
//
// Resolving the whole model rather than a hand-picked list of attributes is
// deliberate: the next Optional+Computed attribute added to the schema is
// covered without anyone remembering this function exists. The result feeds
// monitorPatchFromModel and monitorPatchNeeded only and is never written to state,
// so the response-only fields it also fills in (status, ping_url, the
// deadlines) cannot mask a server-side change.
func resolveUnknownsFromState(plan, state monitorResourceModel) monitorResourceModel {
	out := plan
	outV := reflect.ValueOf(&out).Elem()
	stateV := reflect.ValueOf(state)
	for i := range outV.NumField() {
		v, ok := outV.Field(i).Interface().(attr.Value)
		if !ok || !v.IsUnknown() {
			continue
		}
		outV.Field(i).Set(stateV.Field(i))
	}
	return out
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
	var plan, state, cfg monitorResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	// The configuration is read alongside the plan because it is the only place
	// that records what the practitioner actually wrote: an attribute they
	// deleted is null here even when the plan still carries the stored value.
	// monitorPatchFromModel needs that to tell "clear it" from "leave it".
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	id := state.ID.ValueString()

	// Anything the plan left unknown has to come from the prior state before the
	// payload is built — otherwise the write carries a zero the operator never
	// configured, which a full-replace server applies verbatim. See
	// resolveUnknownsFromState.
	desired := resolveUnknownsFromState(plan, state)

	patch, err := monitorPatchFromModel(ctx, desired, cfg)
	if err != nil {
		resp.Diagnostics.AddError("Unable to build monitor payload", err.Error())
		return
	}
	patchNeeded, err := monitorPatchNeeded(ctx, desired, state)
	if err != nil {
		resp.Diagnostics.AddError("Unable to build monitor payload", err.Error())
		return
	}

	var out *client.Monitor
	if patchNeeded {
		out, err = r.client.UpdateMonitor(ctx, id, patch)
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

	// Assertions come from the CONFIGURATION, not from `desired`: the block is
	// Optional-only, so removing every block plans as a null set, and a null set
	// has to mean "clear them" — assertionsFromModel turns it into the empty
	// array the replace-the-set PUT needs. Passing nil as current makes
	// syncAssertions read the stored set first, so an apply that leaves the
	// assertions alone issues no write at all.
	desiredAssertions, err := assertionsFromModel(ctx, cfg.Assertions)
	if err != nil {
		resp.Diagnostics.AddError("Unable to build assertions payload", err.Error())
		return
	}
	appliedAssertions, err := r.syncAssertions(ctx, id, desiredAssertions, nil)
	if err != nil {
		resp.Diagnostics.AddError("Unable to update monitor assertions", err.Error())
		return
	}
	assertionSet, diags := assertionsToModel(ctx, appliedAssertions, plan.Assertions)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	newState.Assertions = assertionSet

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
