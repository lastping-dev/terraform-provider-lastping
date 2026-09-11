package provider

import (
	"context"
	"fmt"
	"time"

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
	Scope     types.String `tfsdk:"scope"`

	Prefix         types.String `tfsdk:"prefix"`
	CreatedAt      types.String `tfsdk:"created_at"`
	LastUsedAt     types.String `tfsdk:"last_used_at"`
	CreatedByKeyID types.String `tfsdk:"created_by_key_id"`
	Key            types.String `tfsdk:"key"`
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
			"The API mints a key once and never lets it be read again or changed, so there is no " +
			"update path and no import: an imported key could never populate `key`. Changing `name`, " +
			"or changing a configured `expires_at` or `scope`, replaces the key; REMOVING either of " +
			"those from configuration changes nothing, because the expiry and the scope the key " +
			"already has are kept — mint a never-expiring or lower-scoped key with " +
			"`terraform apply -replace` instead. Replacing a key revokes the " +
			"old one, so anything still presenting it stops authenticating — plan rotations with " +
			"`create_before_destroy` if that matters.\n\n" +
			"~> **Destroying this resource revokes every key this key created, recursively.** The API " +
			"cascades revocation down the `created_by_key_id` chain in one transaction, so a key " +
			"minted *by* this key — including one minted by a `lastping_api_key` resource that used " +
			"it, or by an agent through the MCP server — stops authenticating at the same moment. " +
			"That is what stops a compromised credential outliving its own revocation, and it makes " +
			"`terraform destroy` (or any change that replaces this key) reach further than the one " +
			"resource in the plan.",
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
			// Optional AND Computed: the server has the last word on a key's
			// expiry. A key may not outlive the key that minted it
			// (api/apikeys_api.go: handleCreateAPIKey), so an apply
			// authenticated with an expiring key can be handed an expires_at it
			// never asked for. Optional alone fails that apply outright with
			// "Provider produced inconsistent result after apply".
			//
			// UseStateForUnknown is what stops the perpetual diff Optional+Computed
			// otherwise creates: with nothing configured the attribute plans as
			// unknown on every run — including the ordinary never-expires case,
			// where state is null — and each plan then proposes a change to a
			// resource that must never be replaced casually, because replacing a
			// key revokes a live credential.
			//
			// RequiresReplaceIfConfigured, not RequiresReplace: replacement is for
			// a practitioner CHANGING a configured expiry. A server-assigned value
			// arriving in state for a config that says nothing is not a config
			// change and must not revoke the key.
			"expires_at": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "RFC 3339 timestamp after which the key stops authenticating, for " +
					"example `2027-01-01T00:00:00Z`. Must be in the future.\n\n" +
					"Omit it for a key that never expires — unless the credential running Terraform " +
					"expires itself, in which case the server gives the new key its creator's expiry, " +
					"because a key may never outlive the key that minted it. That server-assigned " +
					"value is read back into state, so an omitted `expires_at` can come back " +
					"populated; it is not drift and no later plan proposes a change for it.\n\n" +
					"A configured value beyond the creating key's own expiry is refused by the API, " +
					"with the ceiling reported as `max_expires_at` — it is never quietly lowered.\n\n" +
					"The API cannot change a key's expiry, so changing this replaces the key. " +
					"REMOVING it does not: with nothing configured Terraform keeps whatever expiry " +
					"the key already has, so minting a never-expiring replacement takes " +
					"`terraform apply -replace`.",
				Validators: []validator.String{rfc3339Validator{}, futureTimestampValidator{}},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
					stringplanmodifier.RequiresReplaceIfConfigured(),
				},
			},

			// Optional AND Computed with NO Default, exactly like expires_at
			// above, and for the same reason: the server decides for a
			// configuration that says nothing, and a value already on the key
			// must survive a plan untouched.
			//
			// A Default here was a REVOCATION HAZARD, which is worth writing
			// down because it is not obvious from the framework's docs. A
			// Default is re-applied on EVERY plan (fwserver's
			// TransformDefaults, not only on create) wherever the CONFIG is
			// null — it never looks at the value carried forward from prior
			// state. So `Default: "write"` + RequiresReplace meant that a key
			// which is `admin` on the server (every key minted before the
			// scope column existed is, by that migration's grandfathering
			// clause) and whose configuration predates this attribute would be
			// planned down to `write` and REPLACED — revoking a live
			// credential, and cascading to every key it ever minted, on a bare
			// provider upgrade with no user action at all.
			//
			// RequiresReplaceIfConfigured, therefore: replacement is for a
			// practitioner CHANGING a scope they wrote down. An omitted scope
			// means "whatever this key already has", and demoting a key is
			// `terraform apply -replace`, the same deliberate act expires_at
			// asks for.
			"scope": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "What the key may do: `read` (every GET), `write` (everything " +
					"except API key management) or `admin` (everything, key management included).\n\n" +
					"Omit it and the API chooses: a new key gets the API's own default, which is " +
					"`write` — the tier a credential handed to CI or to an agent should have, since " +
					"it can do the work and cannot mint itself a replacement that survives its own " +
					"revocation. That server-assigned value is read back into state, so an omitted " +
					"`scope` can come back populated; it is not drift and no later plan proposes a " +
					"change for it.\n\n" +
					"**A key may not be given a higher scope than the key that mints it.** Asking for " +
					"one is refused by the API with the ceiling reported as `max_scope` — so a " +
					"Terraform run authenticating with a `write` key cannot create an `admin` key, " +
					"whatever the configuration says.\n\n" +
					"The API cannot change a key's scope, so changing a configured `scope` replaces " +
					"the key — which revokes the old one, and every key that old key created. " +
					"REMOVING it does not: with nothing configured Terraform keeps whatever scope the " +
					"key already has, so demoting a key takes `terraform apply -replace`. Keys that " +
					"predate scopes are `admin`, and that is why the removal case must never replace " +
					"on its own.",
				Validators: []validator.String{stringvalidator.OneOf(apiKeyScopes...)},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
					stringplanmodifier.RequiresReplaceIfConfigured(),
				},
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
			"last_used_at": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "RFC 3339 UTC timestamp of the most recent request this key " +
					"authenticated. Null until the key is used for the first time. The API records this " +
					"on every authenticated request, so it can change between one `terraform apply` and " +
					"the next with nothing in configuration to compare it against — Terraform's refresh " +
					"absorbs that silently, the same way it already does for a monitor's `due_at` and " +
					"`next_probe_at`; it surfaces only under the opt-in `terraform plan -refresh-only`.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"created_by_key_id": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "UUID of the API key that minted this one — the key the provider " +
					"was configured with. Null for a key created from a dashboard session, which has " +
					"no parent key, and for one whose parent has since been deleted.\n\n" +
					"It is the lineage the API's cascading revocation walks: revoking a key revokes " +
					"every key whose `created_by_key_id` chain leads back to it.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
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

// expiresAtConflict reports whether the server's answer contradicts an expiry
// the practitioner actually configured, and returns the API's own value for the
// diagnostic.
//
// It never fires for a configuration that omitted expires_at: there the server's
// value IS the answer, which is the whole point of the attribute being Computed.
// It fires only when a configured value was not honoured — either a different
// instant, or no expiry at all for a key that was asked to have one.
//
// Today the API refuses such a request outright (a 400 carrying max_expires_at),
// so this cannot be reached from the hosted backend; it exists because the
// alternative to reporting it is far worse than an unreachable branch. Writing a
// server value over a configured one would either be swallowed by the Computed
// attribute or surface as Terraform's own "Provider produced inconsistent result
// after apply", which reads as a provider bug and names neither the ceiling nor
// the fix. A key whose expiry is not the one that was requested is a credential
// with the wrong lifetime, which is exactly the kind of thing a practitioner has
// to be told about rather than left to notice.
func expiresAtConflict(configured types.String, apiVal *time.Time) (*time.Time, bool) {
	if configured.IsNull() || configured.IsUnknown() {
		return nil, false
	}
	want, err := time.Parse(time.RFC3339, configured.ValueString())
	if err != nil {
		// Not this function's error to report: Create already rejects an
		// unparseable value before it ever reaches the API.
		return nil, false
	}
	if apiVal == nil {
		return nil, true
	}
	return apiVal, !apiVal.Equal(want)
}

// modelFromAPIKey builds Terraform state from an API response.
//
// SECURITY / CORRECTNESS: the plaintext key is absent from every response except
// the one to POST, so Key is taken from the response only when the response
// actually carries it, and from prior otherwise. Overwriting it with "" on a
// refresh would destroy the one copy that exists.
func modelFromAPIKey(k *client.APIKey, prior apiKeyResourceModel) apiKeyResourceModel {
	m := apiKeyResourceModel{
		ID:             types.StringValue(k.ID),
		Name:           types.StringValue(k.Name),
		ExpiresAt:      expiresAtValue(k.ExpiresAt, prior.ExpiresAt),
		Scope:          scopeValue(k.Scope, prior.Scope),
		Prefix:         types.StringValue(k.Prefix),
		CreatedAt:      types.StringValue(k.CreatedAt.UTC().Format(time.RFC3339)),
		LastUsedAt:     lastUsedAtValue(k.LastUsedAt),
		CreatedByKeyID: createdByKeyIDValue(k.CreatedByKeyID),
		Key:            prior.Key,
	}
	if k.Key != "" {
		m.Key = types.StringValue(k.Key)
	}
	return m
}

// lastUsedAtValue maps the API's optional last_used_at to state: null for a
// key that has never authenticated a request, otherwise its UTC RFC 3339
// spelling. Unlike expiresAtValue there is no configured value to reconcile
// against — last_used_at is Computed-only — so there is nothing to preserve
// and every read simply reports what the server has now.
func lastUsedAtValue(v *time.Time) types.String {
	if v == nil {
		return types.StringNull()
	}
	return types.StringValue(v.UTC().Format(time.RFC3339))
}

// scopeValue maps the API's scope onto state. The API always sends one — every
// row has a scope and there is no such thing as an unscoped key — so an EMPTY
// value means the backend predates scopes, not that this key has none. Writing
// "" into state there would contradict the planned value and fail the apply
// with Terraform's own "Provider produced inconsistent result after apply",
// which names neither the cause nor the fix; carrying the prior value forward
// keeps such a backend working exactly as it did before this attribute existed.
func scopeValue(apiVal string, prior types.String) types.String {
	if apiVal == "" {
		return prior
	}
	return types.StringValue(apiVal)
}

// createdByKeyIDValue maps the API's optional created_by_key_id onto state:
// null for a key with no parent — one minted from a dashboard session, or one
// whose parent has since been revoked — otherwise the parent key's UUID.
func createdByKeyIDValue(v *string) types.String {
	if v == nil {
		return types.StringNull()
	}
	return types.StringValue(*v)
}

func (r *apiKeyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan apiKeyResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// The CONFIG, not the plan, decides what is sent: expires_at is Computed, so
	// a plan value can be the server's own answer carried forward rather than
	// anything the practitioner wrote. Only a configured value may be requested,
	// and only a configured value can be contradicted afterwards.
	var config apiKeyResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var expiresAt *time.Time
	if !config.ExpiresAt.IsNull() && !config.ExpiresAt.IsUnknown() {
		t, err := time.Parse(time.RFC3339, config.ExpiresAt.ValueString())
		if err != nil {
			// Unreachable via configuration — rfc3339Validator rejects this at
			// plan time — but a bad value must not be silently sent as "never
			// expires", which is the failure mode that matters here.
			resp.Diagnostics.AddError("Invalid expires_at",
				fmt.Sprintf("%q is not an RFC 3339 timestamp: %s", config.ExpiresAt.ValueString(), err))
			return
		}
		expiresAt = &t
	}

	// The CONFIG decides the scope sent, the same way it decides expires_at
	// above and for the same reason: scope is Computed, so a plan value can be
	// the server's own earlier answer carried forward rather than anything the
	// practitioner wrote. An unset scope sends no `scope` member at all (the
	// client's omitempty), which is what lets the API apply its own default —
	// and it is also the only value that can be contradicted afterwards.
	requestedScope := configuredScope(config.Scope)
	out, err := r.client.CreateAPIKey(ctx, client.CreateAPIKeyInput{
		Name:      plan.Name.ValueString(),
		ExpiresAt: expiresAt,
		Scope:     requestedScope,
	})
	if err != nil {
		// err never contains key material: the plaintext exists only in a 2xx body.
		summary, detail := apiKeyCreateDiagnostic(requestedScope, err, "Unable to create API key")
		resp.Diagnostics.AddError(summary, detail)
		return
	}
	if out.Key == "" {
		resp.Diagnostics.AddError("API key created without a plaintext key",
			fmt.Sprintf("The server accepted the key %q (id %s) but returned no plaintext, so it can "+
				"never be used. Revoke it in the dashboard and report this as a bug.", out.Name, out.ID))
		return
	}

	// State is written BEFORE any expiry complaint below, and deliberately: the
	// key exists on the server now, and a Create that errors without saving
	// state leaves a live credential nothing in Terraform knows about. With
	// state saved, the error is a normal failed apply the practitioner can fix
	// and re-plan, and the stray key is destroyed rather than orphaned.
	state := modelFromAPIKey(out, plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Compared against what was REQUESTED, never against the plan: for a
	// configuration that omits scope the server's answer IS the answer, which
	// is the whole point of the attribute being Computed, and complaining about
	// it would fire on every such apply.
	//
	// The API refuses a scope it will not grant rather than lowering it, so a
	// mismatch here cannot happen against today's backend. It is reported
	// anyway, and as an error: a key minted with a scope nobody asked for is a
	// credential with the wrong power, which is the same class of problem as one
	// with the wrong lifetime — and the alternative to saying so is Terraform's
	// own inconsistent-result error, which reads as a provider bug and names
	// neither value.
	if requestedScope != "" && out.Scope != "" && out.Scope != requestedScope {
		resp.Diagnostics.AddAttributeError(path.Root("scope"),
			"Server did not honour the requested scope",
			fmt.Sprintf("The key %q (id %s) was created with the %s scope rather than the requested "+
				"%s.\n\nA key may not be given a higher scope than the key that mints it, and the API "+
				"refuses such a request instead of lowering it — so this is unexpected and worth "+
				"reporting as a bug. The minted key is recorded in state, so it is not orphaned; the "+
				"next apply replaces it.",
				out.Name, out.ID, out.Scope, requestedScope))
	}

	if got, conflict := expiresAtConflict(config.ExpiresAt, out.ExpiresAt); conflict {
		served := "never expires"
		advice := "Remove expires_at, or report this as a bug: a key that was asked to expire and " +
			"does not is a credential with the wrong lifetime."
		if got != nil {
			served = "expires " + got.UTC().Format(time.RFC3339)
			advice = "Set expires_at to " + got.UTC().Format(time.RFC3339) + " or earlier, or run " +
				"Terraform with a credential that lives at least as long as the key it mints."
		}
		resp.Diagnostics.AddAttributeError(path.Root("expires_at"),
			"Server did not honour the configured expiry",
			fmt.Sprintf("The key %q (id %s) was created, but it %s rather than at the configured "+
				"%s.\n\n"+
				"A key may not outlive the key that created it, so an expiry beyond the creating "+
				"key's own is refused or capped. %s\n\n"+
				"The minted key is recorded in state, so it is not orphaned; the next apply "+
				"replaces it.",
				out.Name, out.ID, served, config.ExpiresAt.ValueString(), advice))
	}
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

// Update is unreachable, and stays unreachable now that expires_at is
// Optional+Computed. The API has no key-update endpoint at all, so nothing here
// can change in place: a changed name or a changed configured expires_at is a
// replacement, and an expires_at REMOVED from configuration plans as no change
// whatsoever, because Terraform carries the prior value forward for an
// Optional+Computed attribute with a null config.
func (r *apiKeyResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError("API keys cannot be updated in place",
		"lastping_api_key has no in-place change: every configured attribute that can change "+
			"forces replacement, and an attribute dropped from configuration keeps its current "+
			"value. Reaching this is a provider bug — please report it.")
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
