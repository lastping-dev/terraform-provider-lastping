package provider

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
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
	_ resource.Resource                = (*routeResource)(nil)
	_ resource.ResourceWithConfigure   = (*routeResource)(nil)
	_ resource.ResourceWithImportState = (*routeResource)(nil)

	_ validator.String = uuidValidator{}
)

// routeEventTypes is the set the API accepts (api/routes.go: validEventTypes).
//
// This is NOT the same set the API auto-routes on monitor creation
// (api/defaultdest.go: defaultAlertEvents), which is down/fail/recovery/blocked.
// every-run, success, started and note are deliberately absent there -- they are
// RateClassInfo, one or more per run, and auto-routing them would mail a user on
// every task transition. Widening this slice must not widen the adoption
// exemption in routeIsServerDefault.
var routeEventTypes = []string{
	"down", "recovery", "fail", "every-run", "success", "started", "blocked", "note",
}

// defaultAlertEvents mirrors api/defaultdest.go: the events the API attaches to
// the project's default email destination when a monitor is created. Kept
// separate from routeEventTypes on purpose — every-run, success, started and
// note are routable but are never auto-routed.
//
// blocked joined this set on 2026-08-08. It means an agent has stopped and is
// waiting on a human, and it was silent by default, which is the whole point of
// the signal. It MUST stay in lockstep with the API: if the provider still
// thought the default set were three events, a blocked route declared in the
// same apply as its monitor would collide with the one the server just created
// and fail as an adoption conflict.
var defaultAlertEvents = []string{"down", "fail", "recovery", "blocked"}

// uuidValidator rejects a value the API could only answer with an opaque
// "invalid JSON body" 400, because it decodes ids straight into uuid.UUID.
// Unknown values (the common case — these are usually references to other
// resources) are left to apply time.
type uuidValidator struct{}

func (uuidValidator) Description(context.Context) string { return "must be a UUID" }

func (v uuidValidator) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (uuidValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if _, err := uuid.Parse(req.ConfigValue.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid ID",
			fmt.Sprintf("%q is not a UUID. Reference the resource instead, for example "+
				"lastping_destination.ops.id.", req.ConfigValue.ValueString()))
	}
}

// NewRouteResource returns a new lastping_route resource.
func NewRouteResource() resource.Resource {
	return &routeResource{}
}

type routeResource struct {
	client *client.Client
}

// routeResourceModel is the Terraform representation of a lastping_route.
//
// There is no `id`: a route is identified by the (monitor_id, event_type) pair,
// which is exactly what the API path is keyed on, and inventing a synthetic
// composite id would only add a second thing to keep in sync.
type routeResourceModel struct {
	MonitorID      types.String `tfsdk:"monitor_id"`
	EventType      types.String `tfsdk:"event_type"`
	DestinationIDs types.List   `tfsdk:"destination_ids"`
}

func (r *routeResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_route"
}

func (r *routeResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Routes one monitor's alerts for a single event type to a list of destinations.\n\n" +
			"The API endpoint (`PUT /api/v1/checks/{id}/routes/{event_type}`) replaces the whole destination " +
			"list on every write, so this resource owns `destination_ids` outright — there is no add-one/remove-one " +
			"operation, and a second resource pointing at the same `(monitor_id, event_type)` pair would silently " +
			"overwrite the first on every apply. Use one resource per event type.\n\n" +
			"~> **Terraform will not adopt a route it did not create — except one shape.** The API has no " +
			"create-only mode for routes, so `Create` reads the event type's current route first and refuses " +
			"when one already exists with a different, non-empty destination list — otherwise the apply would " +
			"silently redirect somebody else's alerts, and the discovery channel would be the next incident. Run " +
			"`terraform import lastping_route.<name> \"<monitor_id>:<event_type>\"` to take over an existing " +
			"route instead, or remove the conflicting resource. A route that is absent, empty, or already " +
			"identical to the configuration is not a conflict and applies normally, so re-creating one after " +
			"losing state still works. The check is a read followed by a write and is not atomic: a route " +
			"created by another writer inside that window is still overwritten.\n\n" +
			"The one exception: LastPing routes every new monitor's `down`, `fail` and `recovery` events to " +
			"the project's default email destination automatically (its first verified `kind = \"email\"` " +
			"destination), so a monitor created moments earlier already carries three routes Terraform did " +
			"not write. A route whose destination list is exactly that one destination — nothing more, " +
			"nothing less — is taken over rather than refused; without this, a monitor and its routes could " +
			"never be created in a single apply. Read that plainly: **any single-destination route that " +
			"happens to point at the default email destination is treated as that auto-created route and " +
			"adopted, whether or not it actually is one.** Two shapes a reader would not expect from " +
			"\"will not adopt a route it did not create\" fall into this — a route someone deliberately " +
			"narrowed to only the default email destination, and a human-set single-destination route left " +
			"over on a monitor created before the project had any verified email channel, because the check " +
			"compares against the project's default *now*, not against what was actually written for that " +
			"monitor at the time. Both are adopted, silently and indistinguishably from a genuine auto-route. " +
			"A route with any other shape — more than one destination, or a single destination that is not " +
			"the default — is a person's own configuration, and refused.\n\n" +
			"A destination must be verified and enabled before it can be routed to: an unconfirmed " +
			"`kind = \"email\"` destination is rejected with `channel not verified or is disabled`.",
		Attributes: map[string]schema.Attribute{
			"monitor_id": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "UUID of the monitor whose alerts are being routed. The route lives at a " +
					"path keyed on this id, so changing it replaces the resource.",
				Validators:    []validator.String{uuidValidator{}},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"event_type": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Which event this route covers: `" +
					strings.Join(routeEventTypes, "`, `") + "`. Part of the resource's identity, so changing it " +
					"replaces the resource.\n\n" +
					"The first three fire on a **state change**: `down` when a monitor misses its deadline, " +
					"`recovery` when it comes back, and `fail` when a run reports failure explicitly. The next " +
					"three are **informational** and fire per run: `every-run` once per completed run whether it " +
					"succeeded or failed, `success` only on a successful completion, and `started` when a run " +
					"begins. Use `success` rather than `every-run` where a failure is not wanted on the same " +
					"channel — `every-run` conflates the two.\n\n" +
					"`blocked` fires immediately when an agent reports it is blocked (waiting on a human) via a " +
					"`blocked` ping. It is agent-reported rather than a system-derived state change, but it draws " +
					"on the `down`/`recovery`/`fail` alert budget rather than the informational one, because a " +
					"blocked run needs a human and must not be starved by chatty informational traffic. It is " +
					"distinct from the `blocked`-cause incident: if the block outlasts the monitor's blocked " +
					"timeout, a separate `down` event fires with cause `blocked` (routable via the bare `down` " +
					"route or a `down/blocked` per-cause override), not another `blocked`-event-type " +
					"notification.\n\n" +
					"`note` is an agent-reported, informational free-text signal — no state change — and shares " +
					"the `every-run`/`success`/`started` informational budget.\n\n" +
					"~> **`every-run`, `success`, `started` and `note` are much chattier than the state-change " +
					"events, and they compete with each other.** " +
					"A state-change event's volume is bounded by how often the monitor changes state; theirs is " +
					"bounded only by how often the monitor runs or reports a note. None of them is flap-damped " +
					"either — a `fail` is held for the flap window so a short blip can be cancelled before it " +
					"pages, while an `every-run` is released immediately. Most importantly they share **one " +
					"per-destination rate budget** (60 notifications per hour by default) that is separate from " +
					"the one `down`, `fail`, `recovery` and `blocked` draw from: a chatty monitor routed to any " +
					"combination of the four can exhaust that shared budget and suppress its own later " +
					"informational notifications on that " +
					"destination. Point them at a low-stakes destination, not at the one that pages someone.",
				Validators:    []validator.String{stringvalidator.OneOf(routeEventTypes...)},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"destination_ids": schema.ListAttribute{
				Required:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Destinations notified for this event, as a **list**: the API stores and " +
					"returns the array in the order given, and that order is the order alerts are dispatched in. " +
					"An empty list is valid and means \"deliver nowhere for this event\", which is not the same as " +
					"removing the resource. Duplicates are rejected at plan time because the API silently " +
					"de-duplicates them, which would otherwise fail the apply as an inconsistent result.",
				Validators: []validator.List{
					listvalidator.UniqueValues(),
					listvalidator.ValueStringsAre(uuidValidator{}),
				},
			},
		},
	}
}

func (r *routeResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// routeDestinationIDs reads the planned destination list. A Required list is
// never null, but it can be unknown as a whole when it is built from other
// resources' attributes — that only happens at plan time, never in Create or
// Update.
func routeDestinationIDs(ctx context.Context, m routeResourceModel) ([]string, diag.Diagnostics) {
	ids := []string{}
	if m.DestinationIDs.IsNull() || m.DestinationIDs.IsUnknown() {
		return ids, nil
	}
	diags := m.DestinationIDs.ElementsAs(ctx, &ids, false)
	return ids, diags
}

// modelFromRoute builds Terraform state from an API response. The API always
// returns an array, so an empty route round-trips as [] — the same shape the
// configuration must have used, since destination_ids is Required.
func modelFromRoute(ctx context.Context, monitorID string, rt *client.Route) (routeResourceModel, diag.Diagnostics) {
	ids := rt.ChannelIDs
	if ids == nil {
		ids = []string{}
	}
	list, diags := types.ListValueFrom(ctx, types.StringType, ids)
	return routeResourceModel{
		MonitorID:      types.StringValue(monitorID),
		EventType:      types.StringValue(rt.EventType),
		DestinationIDs: list,
	}, diags
}

// upsert backs both Create and Update: PUT is idempotent and replaces the whole
// list, so there is nothing for the provider to distinguish between them.
func (r *routeResource) upsert(ctx context.Context, plan routeResourceModel, diags *diag.Diagnostics) (routeResourceModel, bool) {
	ids, d := routeDestinationIDs(ctx, plan)
	diags.Append(d...)
	if diags.HasError() {
		return plan, false
	}

	monitorID := plan.MonitorID.ValueString()
	out, err := r.client.UpsertRoute(ctx, monitorID, plan.EventType.ValueString(), ids)
	if err != nil {
		diags.AddError("Unable to write route", err.Error())
		return plan, false
	}

	state, d := modelFromRoute(ctx, monitorID, out)
	diags.Append(d...)
	return state, !diags.HasError()
}

// routeAdoptionConflict reports whether writing want over an existing route
// would destroy routing this configuration did not create.
//
// Only a *meaningful* clobber counts. A route that does not exist, one that
// exists with no destinations, and one that already holds exactly what is being
// created are all fine: the first two lose nothing, and the third makes the
// write a no-op. Refusing on those would break the legitimate case of
// re-creating a route after a state file is lost.
//
// The comparison is order-sensitive because the order is real state — the API
// dispatches in the order the array was stored (see client.Route) — so a
// reordered list is a different route, not the same one.
func routeAdoptionConflict(existing, want []string) bool {
	return len(existing) > 0 && !slices.Equal(existing, want)
}

// routeIsServerDefault reports whether existing is the route the API attached
// by itself when the monitor was created, rather than routing a person chose.
//
// The API auto-routes every new monitor's down/fail/recovery events to the
// project's default email channel (api/defaultdest.go: attachDefaultRoutes),
// which means a monitor Terraform created a millisecond ago already has three
// routes Terraform did not create. Treating those as somebody else's
// configuration made the obvious first apply — a monitor and its routes
// together — impossible: it always failed, and the only way forward was to
// apply, discover the auto-routes, write import blocks and apply again.
//
// The signature is deliberately narrow. attachDefaultRoutes writes exactly one
// destination, and that destination is the project's default email channel, so
// anything else — two destinations, one destination that is something else —
// cannot have come from it and is left to the guard. A human who deliberately
// narrowed a route down to just the default email channel is indistinguishable
// from the auto-route and will be adopted; that is the one accepted
// false-positive, and it costs them a destination list Terraform is about to
// write explicitly rather than an alert path they lose silently.
//
// defaultDestID is "" when the project has no verified email channel. Then
// attachDefaultRoutes returned early, no auto-route exists, and nothing can
// match — hence the explicit guard rather than a comparison against "".
//
// eventType gates the exemption to the three events the API actually
// auto-routes. every-run, success and started are routable but are NOT
// auto-routed, so an existing route of one of those pointing at the default
// destination cannot have come from the server — somebody chose it. Adopting it
// would be a pure false-positive that buys nothing, so they get the full guard
// instead.
func routeIsServerDefault(eventType string, existing []string, defaultDestID string) bool {
	if defaultDestID == "" {
		return false
	}
	if !slices.Contains(defaultAlertEvents, eventType) {
		return false
	}
	return len(existing) == 1 && existing[0] == defaultDestID
}

func (r *routeResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan routeResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	want, diags := routeDestinationIDs(ctx, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	monitorID := plan.MonitorID.ValueString()
	eventType := plan.EventType.ValueString()

	// Create — the API has no create-only mode for routes (no If-None-Match on
	// this endpoint), and PUT replaces the whole destination list, so a create
	// against an already-routed event type would silently take it over. That
	// failure is invisible until the next incident, when the alert goes to
	// Terraform's destinations instead of the ones on call. So look first.
	//
	// This GET→PUT window is inherently racy: another writer can create the
	// route in between and we will overwrite it. Accepted knowingly — closing
	// it needs a server-side precondition on the route PUT, which is an API
	// change, not a provider one. The race needs two writers within one
	// round-trip; the hazard being fixed here needs only one, at any point in
	// the past.
	existing, err := r.client.GetRoute(ctx, monitorID, eventType)
	switch {
	case err == nil:
		if routeAdoptionConflict(existing.ChannelIDs, want) {
			// One exception, and only one: the route the API attached to this
			// monitor by itself when it was created. Looked up lazily, so the
			// ordinary path — no route, or a route that matches — never pays
			// for the extra round-trip.
			defaultDestID, derr := r.client.DefaultEmailDestinationID(ctx)
			if derr != nil {
				// Fail closed. Without knowing the project's default channel
				// there is no way to tell an auto-route from a person's
				// routing, and guessing in the permissive direction is the
				// hazard this guard exists for.
				resp.Diagnostics.AddError("Unable to identify the project's default destination",
					fmt.Sprintf("The %q route on monitor %s already sends to %s, and listing the "+
						"project's destinations to check whether that is the route LastPing "+
						"attaches to new monitors failed: %s\n\nTerraform will not take over an "+
						"existing route it cannot account for. Retry, or import the route:\n"+
						"  terraform import lastping_route.<name> %s:%s",
						eventType, monitorID, strings.Join(existing.ChannelIDs, ", "), derr,
						monitorID, eventType))
				return
			}
			if !routeIsServerDefault(eventType, existing.ChannelIDs, defaultDestID) {
				resp.Diagnostics.AddError(
					"Route already exists",
					fmt.Sprintf("The %q route on monitor %s already sends to %s. Terraform will not "+
						"take over a route it did not create: applying this would replace that "+
						"destination list, and alerts for this event would stop reaching whoever is "+
						"on it.\n\nTo manage the existing route, import it:\n"+
						"  terraform import lastping_route.<name> %s:%s\n\n"+
						"Or remove this resource from your configuration.",
						eventType, monitorID, strings.Join(existing.ChannelIDs, ", "),
						monitorID, eventType),
				)
				return
			}
		}
	case client.IsNotFound(err):
		// No route for this event type — or no such monitor, which the write
		// below reports with the API's own 404.
	default:
		resp.Diagnostics.AddError("Unable to read existing route", err.Error())
		return
	}

	state, ok := r.upsert(ctx, plan, &resp.Diagnostics)
	if !ok {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *routeResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state routeResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	monitorID := state.MonitorID.ValueString()

	// Read — a route (or the whole monitor) removed out of band must drop out
	// of state rather than fail the plan. GetRoute reports both as 404.
	out, err := r.client.GetRoute(ctx, monitorID, state.EventType.ValueString())
	if err != nil {
		if client.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Unable to read route", err.Error())
		return
	}

	newState, diags := modelFromRoute(ctx, monitorID, out)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *routeResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan routeResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	state, ok := r.upsert(ctx, plan, &resp.Diagnostics)
	if !ok {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *routeResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state routeResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	err := r.client.DeleteRoute(ctx, state.MonitorID.ValueString(), state.EventType.ValueString())
	// A 404 covers both "already gone" and "the monitor was deleted first",
	// which is the normal order when a whole configuration is destroyed.
	if err != nil && !client.IsNotFound(err) {
		resp.Diagnostics.AddError("Unable to delete route", err.Error())
	}
}

// parseRouteImportID splits the composite import ID "<monitor_id>:<event_type>".
// It is a function rather than inline code so the parsing rules can be unit
// tested without a backend.
func parseRouteImportID(id string) (monitorID, eventType string, err error) {
	monitorID, eventType, found := strings.Cut(id, ":")
	if !found {
		return "", "", fmt.Errorf("%q is missing the \":\" separator", id)
	}
	if _, perr := uuid.Parse(monitorID); perr != nil {
		return "", "", fmt.Errorf("%q is not a monitor UUID", monitorID)
	}
	for _, e := range routeEventTypes {
		if eventType == e {
			return monitorID, eventType, nil
		}
	}
	return "", "", fmt.Errorf("%q is not one of %s", eventType, strings.Join(routeEventTypes, ", "))
}

// ImportState accepts "<monitor_id>:<event_type>". A route has no id of its own,
// so both halves of its identity have to come from the import string; a
// malformed value is reported with the expected shape rather than a bare
// "not found" after a doomed lookup.
func (r *routeResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	monitorID, eventType, err := parseRouteImportID(req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Invalid route import ID",
			fmt.Sprintf("%s.\n\nImport a route as \"<monitor_id>:<event_type>\", for example:\n"+
				"  terraform import lastping_route.down "+
				"3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f:down", err))
		return
	}

	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("monitor_id"), monitorID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("event_type"), eventType)...)
}
