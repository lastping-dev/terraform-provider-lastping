package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/lastping-dev/terraform-provider-lastping/internal/client"
)

var (
	_ resource.Resource              = (*apiKeyResource)(nil)
	_ resource.ResourceWithConfigure = (*apiKeyResource)(nil)
	_ validator.String               = futureTimestampValidator{}
)

// futureTimestampValidator mirrors the server's own check
// (api/apikeys_api.go: handleCreateAPIKey rejects an expires_at that is not
// After(now)), so an already-expired timestamp is a plan-time error naming the
// attribute rather than an opaque 400 partway through an apply. It can only move
// the server's error earlier, never invent one: a value the server would accept
// at plan time is in the future, and a value it would reject is not.
//
// A malformed timestamp is rfc3339Validator's business; reporting it twice would
// be noise.
type futureTimestampValidator struct{}

func (futureTimestampValidator) Description(context.Context) string {
	return "must be a timestamp in the future"
}

func (v futureTimestampValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (futureTimestampValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	t, err := time.Parse(time.RFC3339, req.ConfigValue.ValueString())
	if err != nil {
		return
	}
	if !t.After(time.Now()) {
		resp.Diagnostics.AddAttributeError(req.Path, "Expiry is not in the future",
			fmt.Sprintf("%q has already passed, and the API rejects a key that is born expired.\n\n"+
				"Set expires_at to a future timestamp, or remove it for a key that never expires.",
				req.ConfigValue.ValueString()))
	}
}

// NewAPIKeyResource returns a new lastping_api_key resource.
func NewAPIKeyResource() resource.Resource {
	return &apiKeyResource{}
}

type apiKeyResource struct {
	client *client.Client
}

// apiKeyResourceModel is the Terraform representation of a lastping_api_key.
//
// Key holds the plaintext credential. It is Computed because only the server can
// produce it — and it is written to state, because the framework forbids
// combining WriteOnly with Computed and Sensitive affects only CLI rendering,
// not storage. See the resource description; the ephemeral resource exists for
// callers who cannot accept that.
type apiKeyResourceModel struct {
	ID        types.String `tfsdk:"id"`
	Name      types.String `tfsdk:"name"`
	ExpiresAt types.String `tfsdk:"expires_at"`

	Prefix    types.String `tfsdk:"prefix"`
	CreatedAt types.String `tfsdk:"created_at"`
	Key       types.String `tfsdk:"key"`
}

func (r *apiKeyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_api_key"
}

func (r *apiKeyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A LastPing API key, for automation that needs a credential outliving the " +
			"Terraform run that created it.\n\n" +
			"~> **The plaintext key is stored in Terraform state.** `sensitive` only obscures CLI " +
			"output; it does not affect storage. Use a remote backend with encryption at rest and " +
			"restricted access, and treat state as a credential store. For a key that only needs to " +
			"exist during a run, prefer the **ephemeral** `lastping_api_key`, which is never " +
			"persisted and is revoked when the run ends.\n\n" +
			"The API mints a key once and never lets it be read again or changed, so every " +
			"configurable attribute forces replacement and there is no import: an imported key could " +
			"never populate `key`. Replacing a key revokes the old one, so anything still presenting " +
			"it stops authenticating — plan rotations with `create_before_destroy` if that matters.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "API key UUID, assigned by the server. Not a credential.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Human-readable label, shown in the dashboard alongside the key's " +
					"prefix. The API has no rename path, so changing this replaces the key.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"expires_at": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "RFC 3339 timestamp after which the key stops authenticating, for " +
					"example `2027-01-01T00:00:00Z`. Must be in the future. Omit for a key that never " +
					"expires. The API cannot change a key's expiry, so changing this replaces the key.",
				Validators:    []validator.String{rfc3339Validator{}, futureTimestampValidator{}},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},

			"prefix": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "First 10 characters of the key (`lp_…`). Deliberately non-secret: " +
					"it is how a key is identified in the dashboard and in audit output.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"created_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "RFC 3339 UTC timestamp when the key was created.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"key": schema.StringAttribute{
				Computed:  true,
				Sensitive: true,
				MarkdownDescription: "The plaintext API key (`lp_…`). **Stored in Terraform state** — " +
					"see the warning above. The API returns it exactly once, on creation, so it is held " +
					"in state rather than refreshed; a key whose state entry is lost cannot be " +
					"recovered and must be replaced.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *apiKeyResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// expiresAtValue keeps the configured spelling of expires_at when the API's
// UTC-normalised answer denotes the same instant: the column is timestamptz and
// always answers in UTC, so "2027-01-01T00:00:00+01:00" comes back as
// "2026-12-31T23:00:00Z". Without this, an apply fails as an inconsistent result
// and every subsequent plan proposes a replacement.
//
// A genuinely different instant is NOT masked — it is written through as drift,
// so an expiry changed in the database surfaces instead of being papered over.
func expiresAtValue(apiVal *time.Time, prior types.String) types.String {
	if apiVal == nil {
		return types.StringNull()
	}
	if !prior.IsNull() && !prior.IsUnknown() {
		if want, err := time.Parse(time.RFC3339, prior.ValueString()); err == nil && apiVal.Equal(want) {
			return prior
		}
	}
	return types.StringValue(apiVal.UTC().Format(time.RFC3339))
}

// modelFromAPIKey builds Terraform state from an API response.
//
// SECURITY / CORRECTNESS: the plaintext key is absent from every response except
// the one to POST, so Key is taken from the response only when the response
// actually carries it, and from prior otherwise. Overwriting it with "" on a
// refresh would destroy the one copy that exists.
func modelFromAPIKey(k *client.APIKey, prior apiKeyResourceModel) apiKeyResourceModel {
	m := apiKeyResourceModel{
		ID:        types.StringValue(k.ID),
		Name:      types.StringValue(k.Name),
		ExpiresAt: expiresAtValue(k.ExpiresAt, prior.ExpiresAt),
		Prefix:    types.StringValue(k.Prefix),
		CreatedAt: types.StringValue(k.CreatedAt.UTC().Format(time.RFC3339)),
		Key:       prior.Key,
	}
	if k.Key != "" {
		m.Key = types.StringValue(k.Key)
	}
	return m
}

func (r *apiKeyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan apiKeyResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var expiresAt *time.Time
	if !plan.ExpiresAt.IsNull() && !plan.ExpiresAt.IsUnknown() {
		t, err := time.Parse(time.RFC3339, plan.ExpiresAt.ValueString())
		if err != nil {
			// Unreachable via configuration — rfc3339Validator rejects this at
			// plan time — but a bad value must not be silently sent as "never
			// expires", which is the failure mode that matters here.
			resp.Diagnostics.AddError("Invalid expires_at",
				fmt.Sprintf("%q is not an RFC 3339 timestamp: %s", plan.ExpiresAt.ValueString(), err))
			return
		}
		expiresAt = &t
	}

	out, err := r.client.CreateAPIKey(ctx, plan.Name.ValueString(), expiresAt)
	if err != nil {
		// err never contains key material: the plaintext exists only in a 2xx body.
		resp.Diagnostics.AddError("Unable to create API key", err.Error())
		return
	}
	if out.Key == "" {
		resp.Diagnostics.AddError("API key created without a plaintext key",
			fmt.Sprintf("The server accepted the key %q (id %s) but returned no plaintext, so it can "+
				"never be used. Revoke it in the dashboard and report this as a bug.", out.Name, out.ID))
		return
	}

	state := modelFromAPIKey(out, plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Read refreshes metadata only. The plaintext key can never be retrieved again,
// so it is carried over from prior state untouched — see modelFromAPIKey. A key
// revoked out of band is removed from state so the next apply mints a new one
// rather than failing the plan.
func (r *apiKeyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state apiKeyResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	out, err := r.client.GetAPIKey(ctx, state.ID.ValueString())
	if err != nil {
		if client.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Unable to read API key", err.Error())
		return
	}

	newState := modelFromAPIKey(out, state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// Update is unreachable: every configurable attribute is RequiresReplace,
// because the API has no key-update endpoint at all.
func (r *apiKeyResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError("API keys cannot be updated in place",
		"Every configurable attribute of lastping_api_key forces replacement, so this should not "+
			"be reachable. Report this as a provider bug.")
}

func (r *apiKeyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state apiKeyResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.client.RevokeAPIKey(ctx, state.ID.ValueString()); err != nil && !client.IsNotFound(err) {
		resp.Diagnostics.AddError("Unable to revoke API key", err.Error())
		return
	}
}
