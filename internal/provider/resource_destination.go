package provider

import (
	"context"
	"fmt"
	"maps"
	"strings"

	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
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
	_ resource.Resource                     = (*destinationResource)(nil)
	_ resource.ResourceWithConfigure        = (*destinationResource)(nil)
	_ resource.ResourceWithImportState      = (*destinationResource)(nil)
	_ resource.ResourceWithConfigValidators = (*destinationResource)(nil)
	_ resource.ConfigValidator              = (*kindRequiresValidator)(nil)
)

// destinationKinds is the set of kinds the API accepts (api/channels.go:
// handleCreateChannel).
var destinationKinds = []string{
	"webhook", "telegram", "discord", "slack", "msteams", "googlechat", "ntfy", "pushover", "email",
}

// destinationKindAttrs maps each kind onto the configuration attributes that
// make up its `config` payload. Every attribute is required by the API for its
// kind (api/channels.go: validateChannelConfig), and no attribute outside a
// kind's list is meaningful for it.
var destinationKindAttrs = map[string][]string{
	"webhook":    {"url", "secret"},
	"telegram":   {"bot_token", "chat_id"},
	"discord":    {"webhook_url"},
	"slack":      {"webhook_url"},
	"msteams":    {"webhook_url"},
	"googlechat": {"webhook_url"},
	"ntfy":       {"topic_url"},
	"pushover":   {"token", "user_key"},
	"email":      {"address"},
}

// allDestinationConfigAttrs is the union of every kind's attributes, used to
// reject attributes that belong to a different kind than the one configured.
var allDestinationConfigAttrs = func() []string {
	seen := map[string]bool{}
	var out []string
	for _, kind := range destinationKinds {
		for _, a := range destinationKindAttrs[kind] {
			if !seen[a] {
				seen[a] = true
				out = append(out, a)
			}
		}
	}
	return out
}()

// kindRequiresValidator enforces one kind's config contract at plan time, so a
// missing credential is a `terraform validate` error naming the attribute
// rather than an opaque 400 halfway through an apply. Validation keys off
// `kind` because attribute names are shared: `webhook_url` belongs to four
// different kinds, and `token` means something different from `bot_token`.
type kindRequiresValidator struct {
	kind     string
	required []string
}

func (v *kindRequiresValidator) Description(context.Context) string {
	return fmt.Sprintf("kind=%s requires %s and no attributes belonging to other kinds",
		v.kind, strings.Join(v.required, ", "))
}

func (v *kindRequiresValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v *kindRequiresValidator) ValidateResource(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var kind types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("kind"), &kind)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// An unknown kind (from a variable or another resource) cannot be checked
	// here; the API still validates it at apply time.
	if kind.IsNull() || kind.IsUnknown() || kind.ValueString() != v.kind {
		return
	}

	allowed := map[string]bool{}
	for _, attr := range v.required {
		allowed[attr] = true

		var val types.String
		resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root(attr), &val)...)
		if resp.Diagnostics.HasError() {
			return
		}
		if val.IsUnknown() {
			continue
		}
		if val.IsNull() || val.ValueString() == "" {
			resp.Diagnostics.AddAttributeError(path.Root(attr),
				"Missing required destination attribute",
				fmt.Sprintf("kind=%s requires %s.\n\nSet %s to a non-empty value.",
					v.kind, attr, attr))
		}
	}

	// Attributes for a different kind are silently dropped by the API, which
	// would leave a configuration that looks right and delivers nowhere.
	for _, attr := range allDestinationConfigAttrs {
		if allowed[attr] {
			continue
		}
		var val types.String
		resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root(attr), &val)...)
		if resp.Diagnostics.HasError() {
			return
		}
		if !val.IsNull() && !val.IsUnknown() {
			resp.Diagnostics.AddAttributeError(path.Root(attr),
				"Attribute does not apply to this destination kind",
				fmt.Sprintf("kind=%s uses %s; %s belongs to a different kind and would be ignored.\n\n"+
					"Remove %s, or change kind.",
					v.kind, strings.Join(v.required, " and "), attr, attr))
		}
	}
}

// NewDestinationResource returns a new lastping_destination resource.
func NewDestinationResource() resource.Resource {
	return &destinationResource{}
}

type destinationResource struct {
	client *client.Client
}

// destinationResourceModel is the Terraform representation of a
// lastping_destination.
//
// Config attributes are flat and plain Optional (never Optional+Computed): the
// API cannot supply them, and Optional+Computed would pin a removed attribute
// in state forever. Only the genuinely server-supplied fields are Computed.
type destinationResourceModel struct {
	ID   types.String `tfsdk:"id"`
	Kind types.String `tfsdk:"kind"`
	Name types.String `tfsdk:"name"`

	// Per-kind config attributes.
	URL        types.String `tfsdk:"url"`
	Secret     types.String `tfsdk:"secret"`
	BotToken   types.String `tfsdk:"bot_token"`
	ChatID     types.String `tfsdk:"chat_id"`
	WebhookURL types.String `tfsdk:"webhook_url"`
	TopicURL   types.String `tfsdk:"topic_url"`
	Token      types.String `tfsdk:"token"`
	UserKey    types.String `tfsdk:"user_key"`
	Address    types.String `tfsdk:"address"`

	// Computed.
	Target        types.String `tfsdk:"target"`
	Verified      types.Bool   `tfsdk:"verified"`
	Disabled      types.Bool   `tfsdk:"disabled"`
	DisableReason types.String `tfsdk:"disable_reason"`
	CreatedAt     types.String `tfsdk:"created_at"`
}

func (r *destinationResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_destination"
}

func (r *destinationResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "An alert destination (\"channel\"): where LastPing delivers notifications when a " +
			"monitor goes down or recovers. Attach one to a monitor with `lastping_route`.\n\n" +
			"Credentials are **write-only**. The API stores them but never returns them, so Terraform keeps " +
			"the configured value in state and refreshes only the non-secret fields. A destination that is " +
			"imported rather than created therefore starts with its credential attributes empty; the next " +
			"apply writes the configured value back.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Destination UUID, assigned by the server.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"kind": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Delivery mechanism: `" + strings.Join(destinationKinds, "`, `") + "`. " +
					"The API rejects a change of kind (the config shape and any routes pointing here would " +
					"no longer be valid), so changing this attribute replaces the resource.",
				Validators:    []validator.String{stringvalidator.OneOf(destinationKinds...)},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Human-readable name, shown in the dashboard and in alert routing. " +
					"Renaming is an in-place update: it does not recreate the destination, and for " +
					"`kind = \"email\"` it does not reset verification.",
			},

			"url": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Endpoint LastPing POSTs to. Required for `kind = \"webhook\"`.",
			},
			"secret": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "Shared secret used to sign webhook deliveries. Required for " +
					"`kind = \"webhook\"`. Write-only: never returned by the API.",
			},
			"bot_token": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "Telegram bot token. Required for `kind = \"telegram\"`. Write-only: " +
					"never returned by the API.",
			},
			"chat_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Telegram chat ID to post into. Required for `kind = \"telegram\"`.",
			},
			"webhook_url": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "Incoming-webhook URL. Required for `kind` `discord`, `slack`, " +
					"`msteams`, and `googlechat`. Write-only: the URL is itself the credential, so the API " +
					"never returns it.",
			},
			"topic_url": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Full ntfy topic URL, for example `https://ntfy.sh/my-alerts`. " +
					"Required for `kind = \"ntfy\"`.",
			},
			"token": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "Pushover application token. Required for `kind = \"pushover\"`. " +
					"Write-only: never returned by the API.",
			},
			"user_key": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "Pushover user or group key. Required for `kind = \"pushover\"`. " +
					"Write-only: never returned by the API.",
			},
			"address": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Recipient email address. Required for `kind = \"email\"`. Changing it " +
					"clears `verified` and sends a fresh confirmation email to the new address.",
			},

			"target": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Non-secret delivery hint reported by the API: the URL, address or " +
					"topic for the kinds where that value is not a credential, and a fixed label such as " +
					"`slack webhook` for the kinds where it is.",
			},
			"verified": schema.BoolAttribute{
				Computed: true,
				MarkdownDescription: "Whether the destination is confirmed. Every kind except `email` is " +
					"verified on creation; an email destination stays unverified until the recipient clicks " +
					"the confirmation link, and does not receive alerts before then.",
			},
			"disabled": schema.BoolAttribute{
				Computed: true,
				MarkdownDescription: "Whether LastPing has disabled this destination after repeated " +
					"delivery failures.",
			},
			"disable_reason": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Why the destination was disabled. Null while it is healthy.",
			},
			"created_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "RFC 3339 UTC timestamp when the destination was created.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

// ConfigValidators enforces each kind's required attributes at plan time.
func (r *destinationResource) ConfigValidators(context.Context) []resource.ConfigValidator {
	out := make([]resource.ConfigValidator, 0, len(destinationKinds))
	for _, kind := range destinationKinds {
		out = append(out, &kindRequiresValidator{kind: kind, required: destinationKindAttrs[kind]})
	}
	return out
}

func (r *destinationResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// destinationConfig builds the API `config` object for the configured kind.
// Attributes belonging to other kinds are ignored — ConfigValidators has
// already rejected them.
func destinationConfig(m destinationResourceModel) map[string]string {
	values := map[string]types.String{
		"url":         m.URL,
		"secret":      m.Secret,
		"bot_token":   m.BotToken,
		"chat_id":     m.ChatID,
		"webhook_url": m.WebhookURL,
		"topic_url":   m.TopicURL,
		"token":       m.Token,
		"user_key":    m.UserKey,
		"address":     m.Address,
	}
	cfg := map[string]string{}
	for _, attr := range destinationKindAttrs[m.Kind.ValueString()] {
		cfg[attr] = values[attr].ValueString()
	}
	return cfg
}

// modelFromDestination builds Terraform state from an API response.
//
// SECURITY / CORRECTNESS: the API never returns `config`, so the credential
// attributes can only come from prior — the plan on create/update, the previous
// state on read. Overwriting them from the response would null them out and
// produce a spurious diff on every subsequent plan.
//
// The non-secret config attributes are a different matter: for four kinds the
// API's `target` *is* that value (channelDTO), so they are refreshed from the
// response. That keeps genuine out-of-band edits visible as drift instead of
// being masked by state, and it is what lets an email or ntfy destination be
// imported with its configuration intact.
func modelFromDestination(d *client.Destination, prior destinationResourceModel) destinationResourceModel {
	m := prior
	m.ID = types.StringValue(d.ID)
	m.Kind = types.StringValue(d.Kind)
	m.Name = types.StringValue(d.Name)
	m.Target = types.StringValue(d.Target)
	m.Verified = types.BoolValue(d.Verified)
	m.Disabled = types.BoolValue(d.Disabled)
	m.DisableReason = stringOrNull(d.DisableReason)
	m.CreatedAt = types.StringValue(d.CreatedAt)

	switch d.Kind {
	case "webhook":
		m.URL = stringOrNull(d.Target)
	case "ntfy":
		m.TopicURL = stringOrNull(d.Target)
	case "email":
		m.Address = stringOrNull(d.Target)
	case "telegram":
		// channelDTO renders the chat id as "chat <id>".
		m.ChatID = stringOrNull(strings.TrimPrefix(d.Target, "chat "))
	}
	return m
}

func (r *destinationResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan destinationResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	out, err := r.client.CreateDestination(ctx, client.Destination{
		Kind:   plan.Kind.ValueString(),
		Name:   plan.Name.ValueString(),
		Config: destinationConfig(plan),
	})
	if err != nil {
		resp.Diagnostics.AddError("Unable to create destination", err.Error())
		return
	}

	state := modelFromDestination(out, plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *destinationResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state destinationResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Read — a deleted destination must be removed from state, not error the plan.
	out, err := r.client.GetDestination(ctx, state.ID.ValueString())
	if err != nil {
		if client.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Unable to read destination", err.Error())
		return
	}

	newState := modelFromDestination(out, state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *destinationResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state destinationResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var name *string
	if !plan.Name.Equal(state.Name) {
		v := plan.Name.ValueString()
		name = &v
	}

	// The API replaces config wholesale, and for an email destination a changed
	// address resets verification. Send config only when it actually changed, so
	// a pure rename stays a rename.
	var config map[string]string
	planCfg, stateCfg := destinationConfig(plan), destinationConfig(state)
	if !maps.Equal(planCfg, stateCfg) {
		config = planCfg
	}

	out, err := r.client.UpdateDestination(ctx, state.ID.ValueString(), name, config)
	if err != nil {
		resp.Diagnostics.AddError("Unable to update destination", err.Error())
		return
	}

	newState := modelFromDestination(out, plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *destinationResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state destinationResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.client.DeleteDestination(ctx, state.ID.ValueString()); err != nil && !client.IsNotFound(err) {
		resp.Diagnostics.AddError("Unable to delete destination", err.Error())
		return
	}
}

// ImportState accepts a UUID or a destination name. Unlike monitors,
// destinations have no slug, and names are not unique — so a name is resolved
// only when exactly one destination matches, and an ambiguous name is an error
// rather than an arbitrary pick.
//
// Importing cannot recover credentials: the API never returns them. Use
// `ImportStateVerifyIgnore` in tests, and expect the first apply after an import
// to write the configured credential back.
func (r *destinationResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if _, err := uuid.Parse(req.ID); err == nil {
		resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
		return
	}

	list, err := r.client.ListDestinations(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Unable to look up destination for import",
			fmt.Sprintf("Listing destinations to resolve %q failed: %s", req.ID, err))
		return
	}

	var matches []client.Destination
	for _, d := range list {
		if d.Name == req.ID {
			matches = append(matches, d)
		}
	}
	switch len(matches) {
	case 0:
		resp.Diagnostics.AddError("Unable to import destination",
			fmt.Sprintf("No destination found with ID or name %q in this project.", req.ID))
	case 1:
		resource.ImportStatePassthroughID(ctx, path.Root("id"),
			resource.ImportStateRequest{ID: matches[0].ID}, resp)
	default:
		ids := make([]string, 0, len(matches))
		for _, d := range matches {
			ids = append(ids, d.ID)
		}
		resp.Diagnostics.AddError("Ambiguous destination name",
			fmt.Sprintf("%d destinations are named %q: %s.\n\nImport by UUID instead.",
				len(matches), req.ID, strings.Join(ids, ", ")))
	}
}
