package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
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
	_ resource.Resource                = (*monitorResource)(nil)
	_ resource.ResourceWithConfigure   = (*monitorResource)(nil)
	_ resource.ResourceWithImportState = (*monitorResource)(nil)

	_ planmodifier.String = normalizeSlugModifier{}
)

// normalizeSlugModifier lowercases and trims the configured slug before
// Terraform locks in the planned value, matching the server's own
// normalisation (trim + lowercase — see api/slug.go: normalizeSlug in the
// LastPing monorepo). Without this, a mixed-case slug in config would plan to
// itself, then the server-normalised response would make Terraform report
// "produced inconsistent result after apply".
type normalizeSlugModifier struct{}

func (normalizeSlugModifier) Description(context.Context) string {
	return "Normalises the slug (trim + lowercase) to match server-side normalisation."
}

func (m normalizeSlugModifier) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (normalizeSlugModifier) PlanModifyString(_ context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	resp.PlanValue = types.StringValue(strings.ToLower(strings.TrimSpace(req.ConfigValue.ValueString())))
}

// NewMonitorResource returns a new lastping_monitor resource.
func NewMonitorResource() resource.Resource {
	return &monitorResource{}
}

type monitorResource struct {
	client *client.Client
}

// monitorResourceModel is the Terraform representation of a lastping_monitor.
// Every attribute other than name/slug/grace_s is Optional+Computed: the API
// derives or defaults most of these server-side (tz defaults to UTC,
// probe_method defaults to GET, tags is always returned as an array, and so
// on), and a plain Optional attribute would make the provider "produce an
// inconsistent result after apply" the moment the server fills one in that
// the config left unset.
type monitorResourceModel struct {
	ID                types.String `tfsdk:"id"`
	Name              types.String `tfsdk:"name"`
	Slug              types.String `tfsdk:"slug"`
	MonitorType       types.String `tfsdk:"monitor_type"`
	ScheduleKind      types.String `tfsdk:"schedule_kind"`
	PeriodS           types.Int64  `tfsdk:"period_s"`
	CronExpr          types.String `tfsdk:"cron_expr"`
	TZ                types.String `tfsdk:"tz"`
	GraceS            types.Int64  `tfsdk:"grace_s"`
	Tags              types.Set    `tfsdk:"tags"`
	RunawayCeiling    types.Int64  `tfsdk:"runaway_ceiling"`
	MonitorFrom       types.String `tfsdk:"monitor_from"`
	ProbeURL          types.String `tfsdk:"probe_url"`
	ProbeMethod       types.String `tfsdk:"probe_method"`
	ProbeIntervalS    types.Int64  `tfsdk:"probe_interval_s"`
	ProbeExpectedBody types.String `tfsdk:"probe_expected_body"`
	Paused            types.Bool   `tfsdk:"paused"`
	PingURL           types.String `tfsdk:"ping_url"`
	Status            types.String `tfsdk:"status"`
	CreatedAt         types.String `tfsdk:"created_at"`
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
				Computed: true,
				MarkdownDescription: "Stable, project-scoped identifier used for import and for `If-None-Match` " +
					"collision detection. The API has no path to change a monitor's slug, so changing this " +
					"attribute replaces the resource. Normalised server-side (trimmed, lowercased); must match " +
					"`^[a-z0-9][a-z0-9-]{1,48}[a-z0-9]$` and must not be UUID-shaped.",
				PlanModifiers: []planmodifier.String{
					normalizeSlugModifier{},
					stringplanmodifier.RequiresReplace(),
				},
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
					"Ignored for `monitor_type = \"http\"`, which derives its own schedule from `probe_interval_s`.",
				Validators: []validator.String{stringvalidator.OneOf("simple", "cron")},
			},
			"period_s": schema.Int64Attribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "Period in seconds, for `schedule_kind = \"simple\"`.",
			},
			"cron_expr": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "5-field cron expression, for `schedule_kind = \"cron\"`.",
			},
			"tz": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "IANA timezone for cron evaluation. Defaults to `UTC` when unset.",
			},
			"grace_s": schema.Int64Attribute{
				Required: true,
				MarkdownDescription: "Grace period in seconds after a deadline is missed before alerting. " +
					"Required by the API; must be between 60 and 31536000 (one year).",
				Validators: []validator.Int64{int64validator.Between(60, 31536000)},
			},
			"tags": schema.SetAttribute{
				Optional:            true,
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Labels attached to this monitor. Always returned as a set (never null).",
			},
			"runaway_ceiling": schema.Int64Attribute{
				Optional: true,
				MarkdownDescription: "Optional cap on pings per rolling 1-hour window. Exceeding it opens a " +
					"\"runaway\" incident. Omit to disable.",
			},
			"monitor_from": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "RFC 3339 timestamp from which deadlines are computed for a new monitor.",
			},
			"probe_url": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
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
				Computed:            true,
				MarkdownDescription: "Seconds between probes, for `monitor_type = \"http\"`. Between 30 and 86400.",
			},
			"probe_expected_body": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "Substring the probe response body must contain to count as healthy.",
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
		},
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
		Name:              m.Name.ValueString(),
		Slug:              m.Slug.ValueString(),
		MonitorType:       m.MonitorType.ValueString(),
		ScheduleKind:      m.ScheduleKind.ValueString(),
		PeriodS:           m.PeriodS.ValueInt64(),
		CronExpr:          m.CronExpr.ValueString(),
		TZ:                m.TZ.ValueString(),
		GraceS:            m.GraceS.ValueInt64(),
		ProbeURL:          m.ProbeURL.ValueString(),
		ProbeMethod:       m.ProbeMethod.ValueString(),
		ProbeIntervalS:    m.ProbeIntervalS.ValueInt64(),
		ProbeExpectedBody: m.ProbeExpectedBody.ValueString(),
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

// modelFromMonitor builds Terraform state from an API response. Config-only
// fields the API never echoes back (paused is handled by the caller since it
// is not part of the monitor payload returned here in a way tests rely on)
// are copied straight through; everything the API can return is taken from
// the API response.
func modelFromMonitor(ctx context.Context, mon *client.Monitor) (monitorResourceModel, error) {
	m := monitorResourceModel{
		ID:                types.StringValue(mon.ID),
		Name:              types.StringValue(mon.Name),
		Slug:              types.StringValue(mon.Slug),
		MonitorType:       types.StringValue(mon.MonitorType),
		ScheduleKind:      types.StringValue(mon.ScheduleKind),
		PeriodS:           types.Int64Value(mon.PeriodS),
		CronExpr:          types.StringValue(mon.CronExpr),
		TZ:                types.StringValue(mon.TZ),
		GraceS:            types.Int64Value(mon.GraceS),
		ProbeURL:          types.StringValue(mon.ProbeURL),
		ProbeMethod:       types.StringValue(mon.ProbeMethod),
		ProbeIntervalS:    types.Int64Value(mon.ProbeIntervalS),
		ProbeExpectedBody: types.StringValue(mon.ProbeExpectedBody),
		Paused:            types.BoolValue(mon.Paused),
		Status:            types.StringValue(mon.Status),
		PingURL:           types.StringValue(mon.PingURL),
		CreatedAt:         types.StringValue(mon.CreatedAt),
	}
	if mon.RunawayCeiling != nil {
		m.RunawayCeiling = types.Int64Value(*mon.RunawayCeiling)
	} else {
		m.RunawayCeiling = types.Int64Null()
	}
	if mon.MonitorFrom != nil && *mon.MonitorFrom != "" {
		m.MonitorFrom = types.StringValue(*mon.MonitorFrom)
	} else {
		m.MonitorFrom = types.StringNull()
	}
	tagsSet, diags := types.SetValueFrom(ctx, types.StringType, mon.Tags)
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

	state, err := modelFromMonitor(ctx, out)
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

	newState, err := modelFromMonitor(ctx, out)
	if err != nil {
		resp.Diagnostics.AddError("Unable to process monitor response", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *monitorResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state monitorResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	id := state.ID.ValueString()

	payload, err := monitorFromModel(ctx, plan)
	if err != nil {
		resp.Diagnostics.AddError("Unable to build monitor payload", err.Error())
		return
	}

	out, err := r.client.UpdateMonitor(ctx, id, payload)
	if err != nil {
		resp.Diagnostics.AddError("Unable to update monitor", err.Error())
		return
	}

	// Update — paused maps onto the pause/resume endpoints, not a PATCH field.
	if !plan.Paused.Equal(state.Paused) {
		if err := r.client.SetMonitorPaused(ctx, id, plan.Paused.ValueBool()); err != nil {
			resp.Diagnostics.AddError("Unable to change paused state", err.Error())
			return
		}
		out.Paused = plan.Paused.ValueBool()
	}

	newState, err := modelFromMonitor(ctx, out)
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
	if err == nil {
		resource.ImportStatePassthroughID(ctx, path.Root("id"), resource.ImportStateRequest{ID: m.ID}, resp)
		return
	}

	if _, uerr := uuid.Parse(id); uerr == nil {
		resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
		return
	}

	resp.Diagnostics.AddError("Unable to import monitor",
		fmt.Sprintf("No monitor found with slug or ID %q: %s", id, err))
}
