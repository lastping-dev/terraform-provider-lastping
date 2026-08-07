package provider

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
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
	_ resource.Resource                = (*agentResource)(nil)
	_ resource.ResourceWithConfigure   = (*agentResource)(nil)
	_ resource.ResourceWithImportState = (*agentResource)(nil)

	_ validator.String = agentNameValidator{}
)

// uuidShapedSlugPattern matches a canonical UUID string. The server rejects a
// derived slug of this shape (api/slug.go: validateSlug) because it would be
// ambiguous with an agent id during import — and ImportState below resolves a
// slug before it tries uuid.Parse, so the ambiguity is real here too.
var uuidShapedSlugPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// deriveAgentSlug mirrors the server's slugFromName (api/agents_api.go)
// exactly: lowercase, every run of characters outside [a-z0-9] collapses to a
// single hyphen, leading and trailing hyphens are trimmed.
//
// It exists only to predict the slug at plan time — for agentNameValidator's
// diagnostics and for the create-collision message. The provider NEVER sends a
// slug: `slug` is not a field of AgentCreate, and the value written to state is
// always the one the server actually derived. If this function and the server
// ever disagree, the state stays right and only a diagnostic reads oddly.
//
// The server then runs normalizeSlug (trim + lowercase) over the result, which
// is a no-op on output that is already lowercase, hyphen-trimmed and free of
// whitespace — so it is deliberately not repeated here.
func deriveAgentSlug(name string) string {
	var b strings.Builder
	lastHyphen := true // suppress a leading hyphen
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			lastHyphen = false
		case !lastHyphen:
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// agentNameValidator rejects a name the server could not turn into a valid
// slug. It mirrors the server exactly — deriveAgentSlug plus the same
// slugPattern and UUID-shape rules validateSlug applies (api/slug.go) — so it
// can only move the server's own 400 to plan time, never invent an error.
//
// Without it, "cannot derive a valid slug from name" arrives as an opaque 400
// partway through an apply, and the reason is genuinely hard to guess: the name
// looks perfectly reasonable, and the length limit applies to the *derived
// slug*, not to the name the practitioner wrote.
type agentNameValidator struct{}

func (agentNameValidator) Description(context.Context) string {
	return "must yield a valid slug of 3-50 characters once lowercased and hyphenated"
}

func (v agentNameValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (agentNameValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}

	name := req.ConfigValue.ValueString()
	if strings.TrimSpace(name) == "" {
		resp.Diagnostics.AddAttributeError(req.Path, "Agent name is empty",
			"The API requires a non-empty name, and derives the agent's slug from it.")
		return
	}

	slug := deriveAgentSlug(name)
	switch {
	case !slugPattern.MatchString(slug):
		resp.Diagnostics.AddAttributeError(req.Path, "Agent name does not yield a valid slug",
			fmt.Sprintf("The server derives an agent's slug from its name by lowercasing it and "+
				"collapsing every run of characters outside [a-z0-9] into a single hyphen. %q derives "+
				"%q, which is not a valid slug: it must match %s (3-50 characters).\n\nRename the agent "+
				"so the derived slug is between 3 and 50 characters of [a-z0-9-].",
				name, slug, slugPattern.String()))
	case uuidShapedSlugPattern.MatchString(slug):
		resp.Diagnostics.AddAttributeError(req.Path, "Agent name derives a UUID-shaped slug",
			fmt.Sprintf("%q derives the slug %q, which has the shape of a UUID. The API rejects that: "+
				"it would be ambiguous with an agent id when importing.\n\nChoose a human-readable name.",
				name, slug))
	}
}

// NewAgentResource returns a new lastping_agent resource.
func NewAgentResource() resource.Resource {
	return &agentResource{}
}

type agentResource struct {
	client *client.Client
}

// agentResourceModel is the Terraform representation of a lastping_agent.
//
// `description` is plain Optional and deliberately NOT Optional+Computed, which
// is what makes removing it from the configuration actually clear it: the
// framework plans an omitted Optional attribute as null, and agentPatchFromModel
// turns that null into the explicit JSON null the API needs to reset the column
// to "". Making it Computed to suppress a diff would pin the stored description
// in place forever — the exact bug that made removing a monitor's tags a silent
// no-op. TestAgentDescriptionIsNotComputed is the guardrail.
//
// Because neither configurable attribute is Optional+Computed, nothing in a
// plan for this resource is ever unknown, so it needs no equivalent of
// resolveUnknownsFromState.
//
// Everything below Slug is server-supplied. Status, MonitorCount and LastSeen
// are not even stored: the API recomputes them from the agent's monitors on
// every response (core/agent.RollUp), so they move on their own and must not
// carry a UseStateForUnknown plan modifier.
type agentResourceModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`

	Slug         types.String `tfsdk:"slug"`
	Status       types.String `tfsdk:"status"`
	MonitorCount types.Int64  `tfsdk:"monitor_count"`
	LastSeen     types.String `tfsdk:"last_seen"`
	CreatedAt    types.String `tfsdk:"created_at"`
}

func (r *agentResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_agent"
}

func (r *agentResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A registry entry for an autonomous agent: a worker — a deploy bot, an ETL " +
			"job, an LLM agent — that owns one or more monitors. Registering one gives it a stable " +
			"identity, and its `status` rolls up live from the monitors it owns, so a fleet of workers " +
			"can be watched as workers rather than as a pile of unrelated checks.\n\n" +
			"~> **`slug` is derived from `name` at creation and never changes again.** The API has no " +
			"path to rename a slug, and renaming the agent deliberately does not re-derive one: anything " +
			"already referring to the agent by slug keeps working. An agent named `Nightly ETL bot` and " +
			"later renamed to `Hourly ETL bot` keeps the slug `nightly-etl-bot`. Destroy and recreate the " +
			"agent if the slug itself has to change — which unowns its monitors (see below), it does not " +
			"delete them.\n\n" +
			"Creating an agent is create-only, not an upsert: a name whose derived slug is already taken " +
			"in this project fails the apply with a clear error instead of silently taking over an agent " +
			"this configuration does not own. Import it instead.\n\n" +
			"Destroying an agent **does not destroy its monitors**. Every monitor it owned survives with " +
			"its ping history, incidents and schedule intact and simply becomes unowned.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Agent UUID, assigned by the server.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Human-readable name, for example `Nightly ETL bot`. The server derives " +
					"`slug` from this value at creation — lowercased, with every run of characters outside " +
					"`[a-z0-9]` collapsed to a single hyphen — and the derived slug must be 3-50 characters, " +
					"which this provider checks at plan time. Renaming is an in-place update and does **not** " +
					"change the slug.",
				Validators: []validator.String{agentNameValidator{}},
			},
			"description": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Free-text description of what this agent does. Removing it from the " +
					"configuration clears it.",
			},

			"slug": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Stable, project-scoped identifier derived from `name` at creation and " +
					"immutable thereafter. Use it to import the agent, and to attach monitors to it — the " +
					"API accepts either the slug or the UUID wherever an agent is referenced.",
				// The server never re-derives a slug, so the value in state is
				// the value after any apply. Without this, every rename would
				// plan slug as "(known after apply)" and imply otherwise.
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"status": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Health rolled up live from the monitors this agent owns, worst first: " +
					"`down`, `blocked` (a run is waiting on a human), `late`, `running` (a run is in flight), " +
					"`up`, `pending` (a monitor exists but has never reported — usually broken wiring) or " +
					"`idle` (no monitors, or all of them paused or in maintenance). Paused and in-maintenance " +
					"monitors never contribute. Never stored, so it changes without any configuration change.",
			},
			"monitor_count": schema.Int64Attribute{
				Computed: true,
				MarkdownDescription: "How many monitors this agent owns. Counted live, so it changes as " +
					"monitors are attached and detached elsewhere.",
			},
			"last_seen": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "RFC 3339 UTC timestamp of the most recent ping across **all** monitors " +
					"this agent owns, including paused and in-maintenance ones — it answers \"when did I last " +
					"hear from this agent at all\", not \"is it healthy\". Null until one of its monitors is " +
					"first pinged.",
			},
			"created_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "RFC 3339 UTC timestamp when the agent was registered.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *agentResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// agentPatchFromModel builds the body of PATCH /api/v1/agents/{id}.
//
// It takes two models for the same reason monitorPatchFromModel does:
//
//   - desired is the plan. Every value in it is concrete — neither configurable
//     attribute is Optional+Computed, so the framework never plans one as
//     unknown — and it supplies the values.
//   - cfg is the practitioner's literal configuration, checked as
//     belt-and-braces. While `description` stays Optional-only, cfg can never be
//     null where desired holds a value, so dropping the argument would not
//     change a byte of any payload today. It is kept because it is the signal
//     that stays correct if `description` is ever made Optional+Computed: the
//     framework marks such an attribute unknown when the configuration omits it,
//     a resolve-from-state step would then fill in the stored description, and a
//     function trusting desired alone would leave it in place forever instead of
//     clearing it.
//
// `name` is always sent. It is Required, so there is always a value, and the API
// reads a null there as an omission anyway — a name has no "unset" state.
//
// `description` is the clearable one: an absent configuration attribute becomes
// an explicit JSON null, which is the only way merge-patch can reset the column
// to "". Omitting the key instead would leave the stored description in place
// and make "remove the description" unreachable through Terraform.
//
// `slug` is never sent. It is immutable server-side and ignored if present, and
// sending a value Terraform cannot change would only invite the belief that it
// can.
func agentPatchFromModel(desired, cfg agentResourceModel) client.AgentPatch {
	patch := client.AgentPatch{"name": desired.Name.ValueString()}

	if cfg.Description.IsNull() || desired.Description.IsNull() {
		patch["description"] = nil
	} else {
		patch["description"] = desired.Description.ValueString()
	}
	return patch
}

// modelFromAgent builds Terraform state from an API response.
//
// The API reports an unset description as "" rather than omitting it (the
// column is NOT NULL with an empty-string default), so it is mapped back to
// null via stringOrNull — otherwise an agent with no description would read as
// "" and conflict with the null a configuration that omits the attribute plans.
func modelFromAgent(a *client.Agent) agentResourceModel {
	return agentResourceModel{
		ID:           types.StringValue(a.ID),
		Name:         types.StringValue(a.Name),
		Description:  stringOrNull(a.Description),
		Slug:         types.StringValue(a.Slug),
		Status:       types.StringValue(a.Status),
		MonitorCount: types.Int64Value(a.MonitorCount),
		LastSeen:     timestampOrNull(a.LastSeen),
		CreatedAt:    types.StringValue(a.CreatedAt),
	}
}

// Create registers the agent.
//
// UPSERT-VS-CREATE, decided deliberately: unlike POST /api/v1/checks — which is
// an upsert-by-slug that only refuses to adopt because CreateMonitor sends
// `If-None-Match: *` — POST /api/v1/agents is genuinely create-only server-side.
// A collision on the slug derived from `name` is a 409 and nothing is written
// (api/agents_api.go: createAgentForProject maps the unique-violation 23505 to
// createAgentConflict). There is no upsert branch to opt out of, so this resource
// sends no precondition header; it converts the 409 into the same actionable
// "import it instead" diagnostic the monitor resource gives for its 412, because
// silently adopting an agent this configuration does not own is the outcome both
// resources exist to prevent.
//
// The collision is worth spelling out because the slug is DERIVED: two different
// names can collide ("Nightly ETL bot" and "nightly etl bot" both derive
// `nightly-etl-bot`), so a conflict does not necessarily mean an agent with a
// matching name already exists — which is why the diagnostic names the slug.
func (r *agentResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan agentResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	out, err := r.client.CreateAgent(ctx, client.Agent{
		Name:        plan.Name.ValueString(),
		Description: plan.Description.ValueString(),
	})
	if err != nil {
		if client.IsConflict(err) {
			slug := deriveAgentSlug(plan.Name.ValueString())
			resp.Diagnostics.AddError(
				"Agent already exists",
				fmt.Sprintf("An agent with slug %q already exists in this project. Terraform will not "+
					"take over an agent it did not create.\n\nThe slug is derived from the name, so a "+
					"different name can still collide: %q derives %q.\n\nTo manage the existing agent, "+
					"import it:\n  terraform import lastping_agent.<name> %s\n\nOr choose a name that "+
					"derives a different slug.",
					slug, plan.Name.ValueString(), slug, slug),
			)
			return
		}
		resp.Diagnostics.AddError("Unable to create agent", err.Error())
		return
	}

	state := modelFromAgent(out)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *agentResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state agentResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Read — an agent deleted out of band must be removed from state, not error
	// the plan.
	out, err := r.client.GetAgent(ctx, state.ID.ValueString())
	if err != nil {
		if client.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Unable to read agent", err.Error())
		return
	}

	newState := modelFromAgent(out)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *agentResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state, cfg agentResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	// The configuration is read alongside the plan because it is the only place
	// that records what the practitioner actually wrote: an attribute they
	// deleted is null here even when the plan still carries the stored value.
	// agentPatchFromModel needs that to tell "clear it" from "leave it".
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// No patch-needed gate, unlike the monitor resource: name and description
	// are the only writable attributes, so any Update at all is a change to one
	// of them, and PATCH here rewrites nothing else — there is no schedule or
	// deadline for a redundant call to disturb.
	out, err := r.client.UpdateAgent(ctx, state.ID.ValueString(), agentPatchFromModel(plan, cfg))
	if err != nil {
		resp.Diagnostics.AddError("Unable to update agent", err.Error())
		return
	}

	newState := modelFromAgent(out)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// Delete removes the agent from the registry. Its monitors are NOT deleted:
// checks.agent_id is ON DELETE SET NULL, so they survive unowned with their
// ping history, incidents and schedule intact.
func (r *agentResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state agentResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.client.DeleteAgent(ctx, state.ID.ValueString()); err != nil && !client.IsNotFound(err) {
		resp.Diagnostics.AddError("Unable to delete agent", err.Error())
		return
	}
}

// ImportState accepts a slug or a UUID. Slug is tried first, for the same
// reason the monitor resource tries it first: agent slugs are project-scoped, so
// a match against the caller's own project is unambiguous, whereas trying
// uuid.Parse first would mean an agent whose slug happens to be UUID-shaped
// could import a different agent entirely.
func (r *agentResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	id := req.ID

	a, err := r.client.GetAgentBySlug(ctx, id)
	switch {
	case err == nil:
		resource.ImportStatePassthroughID(ctx, path.Root("id"), resource.ImportStateRequest{ID: a.ID}, resp)
		return
	case !client.IsNotFound(err):
		// The lookup itself failed (network, 5xx, bad credentials). Reporting
		// that as "no such agent" would send the operator hunting for a missing
		// resource instead of a broken backend.
		resp.Diagnostics.AddError("Unable to look up agent for import",
			fmt.Sprintf("Listing agents to resolve %q failed: %s", id, err))
		return
	}

	if _, uerr := uuid.Parse(id); uerr == nil {
		resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
		return
	}

	resp.Diagnostics.AddError("Unable to import agent",
		fmt.Sprintf("No agent found with slug or ID %q in this project.", id))
}
