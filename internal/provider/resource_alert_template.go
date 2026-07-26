package provider

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/mapvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/lastping-dev/terraform-provider-lastping/internal/client"
)

var (
	_ resource.Resource                = (*alertTemplateResource)(nil)
	_ resource.ResourceWithConfigure   = (*alertTemplateResource)(nil)
	_ resource.ResourceWithImportState = (*alertTemplateResource)(nil)

	_ validator.String = trimmedValidator{}
)

// templateKeyPattern mirrors the server's key grammar (api/api_templates.go:
// parseTemplateKey plus alertevent.IsValidEventType): an event type on its own,
// or `event/cause` for a per-cause override. The cause half is deliberately
// unconstrained — the server does not validate it either, and pinning a list
// here would reject causes added later by the backend.
var templateKeyPattern = regexp.MustCompile(`^(down|recovery|fail|every-run)(/.+)?$`)

// trimmedValidator rejects a body that is not already in the form the server
// would store. handleAPIPutTemplates runs strings.TrimSpace on every value, so
// a padded template comes back trimmed and the apply fails as an inconsistent
// result. A provider cannot fix that by rewriting the planned value — Terraform
// rejects a planned value that differs from a known config value — so the only
// correct answer is to refuse the un-normalised form at plan time.
//
// A whitespace-only body is rejected for a different reason: the server reads
// "" as "delete this key", so the applied map would come back missing a key the
// configuration still lists.
type trimmedValidator struct{}

func (trimmedValidator) Description(context.Context) string {
	return "must be non-empty and free of leading or trailing whitespace"
}

func (v trimmedValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (trimmedValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	v := req.ConfigValue.ValueString()
	switch {
	case strings.TrimSpace(v) == "":
		resp.Diagnostics.AddAttributeError(req.Path, "Empty alert template",
			"An empty template body means \"reset this event to the built-in default\" to the API, so the "+
				"key would disappear from the applied map.\n\nRemove the key from `templates` instead.")
	case strings.TrimSpace(v) != v:
		resp.Diagnostics.AddAttributeError(req.Path, "Alert template has surrounding whitespace",
			"The API trims every template body before storing it, so this value would not survive the "+
				"apply as written.\n\nRemove the leading or trailing whitespace.")
	}
}

// NewAlertTemplateResource returns a new lastping_alert_template resource.
func NewAlertTemplateResource() resource.Resource {
	return &alertTemplateResource{}
}

type alertTemplateResource struct {
	client *client.Client
}

// alertTemplateResourceModel is the Terraform representation of a
// lastping_alert_template: one monitor's entire set of custom alert bodies.
type alertTemplateResourceModel struct {
	MonitorID types.String `tfsdk:"monitor_id"`
	Templates types.Map    `tfsdk:"templates"`
}

func (r *alertTemplateResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_alert_template"
}

func (r *alertTemplateResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Custom alert message bodies for one monitor, overriding the built-in wording.\n\n" +
			"One resource owns a monitor's **whole** template map. `PUT /api/v1/checks/{id}/templates` replaces " +
			"the set rather than merging into it, so modelling individual messages as separate resources would " +
			"make them clobber each other on every apply — the last one applied would win and the others would " +
			"show a permanent diff. Destroying the resource clears every template and returns the monitor to the " +
			"default wording; it does not delete the monitor.\n\n" +
			"A body is plain text with `{token}` placeholders. The tokens are `{check_name}`, `{status}`, " +
			"`{event}`, `{cause}`, `{last_ping}`, `{schedule}`, `{incident_url}`, `{run_url}`, `{branch}`, " +
			"`{commit}`, `{actor}`, `{failing_stage}`, `{duration}`, `{latency}`, `{status_code}` and `{url}`; " +
			"not every one is populated for every event. The API renders each body against a sample context " +
			"before storing it, so an unrecognised `{token}` fails the apply naming the offending key, and " +
			"nothing at all is written — the whole set is validated before any of it is applied.",
		Attributes: map[string]schema.Attribute{
			"monitor_id": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "UUID of the monitor these messages belong to. Templates live at a path " +
					"keyed on this id, so changing it replaces the resource — which clears the old monitor's " +
					"templates and writes them onto the new one.",
				Validators:    []validator.String{uuidValidator{}},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"templates": schema.MapAttribute{
				Required:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Message bodies keyed by event. A key is either an event type on its own — " +
					"`down`, `recovery`, `fail`, `every-run` — which covers every cause, or `event/cause` for a " +
					"narrower override such as `down/silence`, `fail/runaway` or `fail/ci`. The more specific key " +
					"wins when both are present. Removing a key removes the message server-side and restores " +
					"the default wording for that event; an empty map is valid and means \"use the defaults " +
					"everywhere\".",
				// Keys and the storable shape of a body are checked here; the
				// {token} allowlist deliberately is not. Mirroring it would
				// mean rejecting a token the backend adds later before the
				// backend ever sees it, and the server's own error already
				// names the key and the offending token without writing
				// anything.
				Validators: []validator.Map{
					mapvalidator.KeysAre(stringvalidator.RegexMatches(templateKeyPattern,
						"must be an event type (down, recovery, fail, every-run), optionally followed by "+
							"\"/\" and a cause — for example \"down\" or \"down/silence\"")),
					mapvalidator.ValueStringsAre(trimmedValidator{}),
				},
			},
		},
	}
}

func (r *alertTemplateResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// templatesFromModel reads the configured map. A Required map is never null,
// but it can be unknown as a whole while it is still being built from other
// resources' attributes.
func templatesFromModel(ctx context.Context, m alertTemplateResourceModel) (map[string]string, diag.Diagnostics) {
	out := map[string]string{}
	if m.Templates.IsNull() || m.Templates.IsUnknown() {
		return out, nil
	}
	diags := m.Templates.ElementsAs(ctx, &out, false)
	return out, diags
}

// replacementPayload turns a desired map into the request body that actually
// produces it, by adding an explicit empty value for every key the server holds
// and the configuration does not.
//
// This is NOT redundant with the API's replace semantics, which are uniform
// only for event-wide keys (api/api_templates.go: handleAPIPutTemplates deletes
// `down`, `recovery`, `fail` and `every-run` before applying the request, but
// touches a per-cause row only when the request names it). Sending just the
// desired map would therefore leave `down/silence` behind forever once it had
// been set — a removed key that never actually goes away, and a permanent diff.
func replacementPayload(desired, current map[string]string) map[string]string {
	out := maps.Clone(desired)
	if out == nil {
		out = map[string]string{}
	}
	for key := range current {
		if _, keep := out[key]; !keep {
			out[key] = ""
		}
	}
	return out
}

// write applies desired as the monitor's complete template set and returns the
// resulting server state.
func (r *alertTemplateResource) write(ctx context.Context, monitorID string, desired map[string]string) (map[string]string, error) {
	current, err := r.client.GetTemplates(ctx, monitorID)
	if err != nil {
		return nil, err
	}
	return r.client.PutTemplates(ctx, monitorID, replacementPayload(desired, current))
}

func (r *alertTemplateResource) setState(ctx context.Context, monitorID string, templates map[string]string) (alertTemplateResourceModel, diag.Diagnostics) {
	m, diags := types.MapValueFrom(ctx, types.StringType, templates)
	return alertTemplateResourceModel{
		MonitorID: types.StringValue(monitorID),
		Templates: m,
	}, diags
}

func (r *alertTemplateResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan alertTemplateResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	desired, diags := templatesFromModel(ctx, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	monitorID := plan.MonitorID.ValueString()
	// Create asserts the whole set, so any template already on the monitor —
	// set through the dashboard, or left behind by an earlier resource — is
	// cleared rather than silently surviving as an unmanaged extra key.
	out, err := r.write(ctx, monitorID, desired)
	if err != nil {
		resp.Diagnostics.AddError("Unable to write alert templates", err.Error())
		return
	}

	state, diags := r.setState(ctx, monitorID, out)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *alertTemplateResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state alertTemplateResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	monitorID := state.MonitorID.ValueString()

	// Read — templates hang off the monitor, so a deleted monitor is the only
	// thing that removes this resource. An empty map is drift, not absence:
	// it means the templates were cleared and the next apply restores them.
	out, err := r.client.GetTemplates(ctx, monitorID)
	if err != nil {
		if client.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Unable to read alert templates", err.Error())
		return
	}

	newState, diags := r.setState(ctx, monitorID, out)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *alertTemplateResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan alertTemplateResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	desired, diags := templatesFromModel(ctx, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	monitorID := plan.MonitorID.ValueString()
	out, err := r.write(ctx, monitorID, desired)
	if err != nil {
		resp.Diagnostics.AddError("Unable to update alert templates", err.Error())
		return
	}

	state, diags := r.setState(ctx, monitorID, out)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Delete clears the monitor's templates with a PUT, because there is no DELETE
// endpoint for them: the set is emptied by writing an empty one. Per-cause keys
// need naming explicitly (see replacementPayload), so this is not simply
// PUT {"templates":{}} whenever any per-cause override exists.
func (r *alertTemplateResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state alertTemplateResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// A destroy that also removes the monitor usually removes it first, so a
	// 404 here means the templates went with it.
	if _, err := r.write(ctx, state.MonitorID.ValueString(), nil); err != nil && !client.IsNotFound(err) {
		resp.Diagnostics.AddError("Unable to clear alert templates", err.Error())
	}
}

// ImportState takes a monitor UUID: the monitor is the resource's identity, and
// importing pulls in whatever templates it currently has.
func (r *alertTemplateResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("monitor_id"), req, resp)
}
