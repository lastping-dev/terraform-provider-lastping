package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/lastping-dev/terraform-provider-lastping/internal/client"
)

var (
	_ ephemeral.EphemeralResource              = (*apiKeyEphemeralResource)(nil)
	_ ephemeral.EphemeralResourceWithConfigure = (*apiKeyEphemeralResource)(nil)
	_ ephemeral.EphemeralResourceWithRenew     = (*apiKeyEphemeralResource)(nil)
	_ ephemeral.EphemeralResourceWithClose     = (*apiKeyEphemeralResource)(nil)
	_ validator.String                         = durationValidator{}
)

const (
	// defaultEphemeralTTL is the lifetime used when ttl is not configured. An
	// hour comfortably covers an ordinary apply while keeping the blast radius
	// of a leaked key small.
	defaultEphemeralTTL = time.Hour

	// renewSkew is how far before actual expiry Terraform is asked to check in,
	// so a long apply is warned before it loses its credential rather than
	// after.
	renewSkew = 5 * time.Minute

	// minRenewInterval is the shortest gap worth scheduling a check-in for.
	// Below twice this, the remaining life is too short for another round trip
	// to tell anyone anything they can act on.
	minRenewInterval = 2 * time.Minute

	// ephemeralPrivateKey names the private-state entry carrying what Renew and
	// Close need. It holds the key's UUID and expiry — never the plaintext.
	ephemeralPrivateKey = "api_key"
)

// durationValidator rejects a ttl Go cannot parse, or one that is not positive,
// at plan time instead of partway through an apply.
type durationValidator struct{}

func (durationValidator) Description(context.Context) string {
	return "must be a positive Go duration such as \"30m\" or \"2h\""
}

func (v durationValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (durationValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	d, err := time.ParseDuration(req.ConfigValue.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid ttl",
			fmt.Sprintf("%q is not a Go duration. Use a form such as \"30m\", \"1h\" or \"90m\".",
				req.ConfigValue.ValueString()))
		return
	}
	if d <= 0 {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid ttl",
			fmt.Sprintf("ttl must be positive; %q would mint a key that is already expired.",
				req.ConfigValue.ValueString()))
	}
}

// NewAPIKeyEphemeralResource returns a new lastping_api_key ephemeral resource.
func NewAPIKeyEphemeralResource() ephemeral.EphemeralResource {
	return &apiKeyEphemeralResource{}
}

type apiKeyEphemeralResource struct {
	client *client.Client
}

// apiKeyEphemeralModel is the Terraform representation of an
// ephemeral.lastping_api_key. Unlike the managed resource's model, none of this
// is ever written to plan or state.
type apiKeyEphemeralModel struct {
	Name types.String `tfsdk:"name"`
	TTL  types.String `tfsdk:"ttl"`

	ID     types.String `tfsdk:"id"`
	Prefix types.String `tfsdk:"prefix"`
	Key    types.String `tfsdk:"key"`
}

// ephemeralKeyState is what Open hands to Renew and Close through private state.
// It deliberately carries no key material: the plaintext lives only in the
// ephemeral result, which Terraform never persists.
type ephemeralKeyState struct {
	ID        string    `json:"id"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (e *apiKeyEphemeralResource) Metadata(_ context.Context, req ephemeral.MetadataRequest, resp *ephemeral.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_api_key"
}

func (e *apiKeyEphemeralResource) Schema(_ context.Context, _ ephemeral.SchemaRequest, resp *ephemeral.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A short-lived LastPing API key that exists only for the duration of a " +
			"Terraform run. **Nothing about it is written to plan or state**, and the key is revoked " +
			"when the run ends — which is what makes it the right default for a credential Terraform " +
			"needs to *use* rather than to *hand out*.\n\n" +
			"Feed it to a provider configuration, a provisioner or a `terraform_data` consumer:\n\n" +
			"```terraform\n" +
			"ephemeral \"lastping_api_key\" \"run\" {\n" +
			"  name = \"terraform-run\"\n" +
			"  ttl  = \"30m\"\n" +
			"}\n\n" +
			"provider \"lastping\" {\n" +
			"  alias   = \"run\"\n" +
			"  api_key = ephemeral.lastping_api_key.run.key\n" +
			"}\n" +
			"```\n\n" +
			"The server-side expiry is the safety net: if a run is interrupted before the key can be " +
			"revoked, the key still stops working on its own at `ttl`. LastPing mints keys with a " +
			"fixed lifetime and has no endpoint to extend one, so a key cannot actually be renewed " +
			"in place — Terraform is asked to check in shortly before expiry and warns if a run is " +
			"about to outlive its credential. Set `ttl` to comfortably exceed the longest apply you " +
			"expect.\n\n" +
			"Requires Terraform 1.10 or later. For a key that must outlive the run, use the " +
			"`lastping_api_key` **managed resource** — and read its warning about state first.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Human-readable label, shown in the dashboard while the key lives. " +
					"Use something that identifies the run, such as `terraform-ci`.",
			},
			"ttl": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "How long the key is valid, as a Go duration (`\"45m\"`, `\"2h\"`). " +
					"Defaults to `\"1h\"`. This becomes the key's server-side `expires_at`, so it bounds " +
					"the damage of a run that dies before revoking its key.",
				Validators: []validator.String{durationValidator{}},
			},

			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "API key UUID, assigned by the server. Not a credential.",
			},
			"prefix": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "First 10 characters of the key (`lp_…`). Deliberately non-secret: " +
					"safe to log when you need to correlate a run with a key in the dashboard.",
			},
			"key": schema.StringAttribute{
				Computed:  true,
				Sensitive: true,
				MarkdownDescription: "The plaintext API key (`lp_…`). Never written to plan or state, " +
					"and revoked when the run ends. Terraform refuses to place an ephemeral value " +
					"anywhere persistent, so this can only be consumed by another ephemeral context.",
			},
		},
	}
}

func (e *apiKeyEphemeralResource) Configure(_ context.Context, req ephemeral.ConfigureRequest, resp *ephemeral.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	c, ok := req.ProviderData.(*client.Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected ephemeral resource configure type",
			fmt.Sprintf("Expected *client.Client, got %T. This is a provider bug.", req.ProviderData))
		return
	}
	e.client = c
}

// ttlDuration resolves the configured ttl, defaulting when it is absent.
// durationValidator has already rejected anything unparseable at plan time; a
// parse failure here would mean an unvalidated path, so it is reported rather
// than silently defaulted — defaulting would hand back a key with a lifetime
// nobody asked for.
func ttlDuration(ttl types.String) (time.Duration, diag.Diagnostics) {
	var diags diag.Diagnostics
	if ttl.IsNull() || ttl.IsUnknown() || ttl.ValueString() == "" {
		return defaultEphemeralTTL, diags
	}
	d, err := time.ParseDuration(ttl.ValueString())
	if err != nil || d <= 0 {
		diags.AddAttributeError(path.Root("ttl"), "Invalid ttl",
			fmt.Sprintf("%q is not a positive Go duration.", ttl.ValueString()))
		return 0, diags
	}
	return d, diags
}

// renewAt is when Terraform should next call Renew, or the zero time for "never
// again". It sits renewSkew before expiry, but never in the past and never so
// close to expiry that the check-in could not tell anyone anything useful.
func renewAt(now, expiresAt time.Time) time.Time {
	remaining := expiresAt.Sub(now)
	if remaining <= 2*minRenewInterval {
		return time.Time{}
	}
	at := expiresAt.Add(-renewSkew)
	if half := now.Add(remaining / 2); at.Before(half) {
		at = half
	}
	return at
}

// Open mints a short-lived key. RenewAt is set before the key actually expires
// so a long apply is told it is about to lose its credential.
func (e *apiKeyEphemeralResource) Open(ctx context.Context, req ephemeral.OpenRequest, resp *ephemeral.OpenResponse) {
	var cfg apiKeyEphemeralModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	ttl, diags := ttlDuration(cfg.TTL)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	expiresAt := time.Now().Add(ttl)
	key, err := e.client.CreateAPIKey(ctx, cfg.Name.ValueString(), &expiresAt)
	if err != nil {
		// err never contains key material: the plaintext exists only in a 2xx body.
		resp.Diagnostics.AddError("Unable to create ephemeral API key", err.Error())
		return
	}
	if key.Key == "" {
		resp.Diagnostics.AddError("Ephemeral API key created without a plaintext key",
			fmt.Sprintf("The server accepted the key (id %s) but returned no plaintext, so it cannot "+
				"be used. Report this as a bug.", key.ID))
		return
	}
	// The server is the authority on expiry — it stores what it stored, and the
	// safety net has to be measured against that rather than against our clock.
	if key.ExpiresAt != nil {
		expiresAt = *key.ExpiresAt
	}

	priv, err := json.Marshal(ephemeralKeyState{ID: key.ID, ExpiresAt: expiresAt})
	if err != nil {
		// Nothing downstream could revoke the key, so it is better to revoke it
		// here and fail than to hand out a credential nothing will clean up.
		resp.Diagnostics.Append(e.revoke(ctx, key.ID)...)
		resp.Diagnostics.AddError("Unable to record ephemeral API key state", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.Private.SetKey(ctx, ephemeralPrivateKey, priv)...)
	if resp.Diagnostics.HasError() {
		resp.Diagnostics.Append(e.revoke(ctx, key.ID)...)
		return
	}

	resp.RenewAt = renewAt(time.Now(), expiresAt)

	cfg.ID = types.StringValue(key.ID)
	cfg.Prefix = types.StringValue(key.Prefix)
	cfg.Key = types.StringValue(key.Key)
	resp.Diagnostics.Append(resp.Result.Set(ctx, &cfg)...)
}

// Renew is a check-in, not an extension: LastPing mints keys with a fixed
// lifetime and exposes no way to move expires_at, and the plaintext handed out
// by Open can never be swapped for a different one. So this confirms the key is
// still usable, warns when the run is about to outlive it, and schedules the
// next check-in.
func (e *apiKeyEphemeralResource) Renew(ctx context.Context, req ephemeral.RenewRequest, resp *ephemeral.RenewResponse) {
	if req.Private == nil {
		return
	}
	st, ok := e.privateState(ctx, req.Private, &resp.Diagnostics)
	if !ok {
		return
	}

	at, diags := e.renewPlan(ctx, st, time.Now())
	resp.Diagnostics.Append(diags...)
	resp.RenewAt = at
}

// renewPlan holds Renew's decisions, separated from private-state plumbing so
// they can be tested without the framework's internal private-state types.
func (e *apiKeyEphemeralResource) renewPlan(ctx context.Context, st ephemeralKeyState, now time.Time) (time.Time, diag.Diagnostics) {
	var diags diag.Diagnostics

	if _, err := e.client.GetAPIKey(ctx, st.ID); err != nil {
		if client.IsNotFound(err) {
			// Revoked out of band. Every subsequent call in this run will 401;
			// saying so names the cause instead of leaving a trail of auth
			// failures on unrelated resources.
			diags.AddError("Ephemeral API key was revoked",
				fmt.Sprintf("API key %s no longer exists. It was revoked outside Terraform while this "+
					"run was using it, so the rest of the run cannot authenticate.", st.ID))
			return time.Time{}, diags
		}
		// A transient API problem is not evidence the key is gone; failing the
		// run over a failed check-in would be worse than the risk it covers.
		diags.AddWarning("Unable to check ephemeral API key",
			fmt.Sprintf("Verifying API key %s failed: %s. The run continues; the key remains valid "+
				"until it expires.", st.ID, err))
	}

	if remaining := st.ExpiresAt.Sub(now); remaining <= renewSkew {
		diags.AddWarning("Ephemeral API key is close to expiry",
			fmt.Sprintf("API key %s expires at %s. LastPing keys have a fixed lifetime and the API "+
				"cannot extend one, so this key cannot be renewed in place — if the run is still "+
				"going then, it will start failing to authenticate.\n\n"+
				"Raise ttl on this ephemeral resource if applies routinely run this long.",
				st.ID, st.ExpiresAt.UTC().Format(time.RFC3339)))
	}

	return renewAt(now, st.ExpiresAt), diags
}

// Close revokes the key. This is what makes the ephemeral path safe: the
// credential does not outlive the run even if something wrote it somewhere.
//
// A failure is a warning, never an error. The key still expires on its own —
// that is what the server-side expiry is for — and failing the whole run over a
// cleanup problem is worse than the already-bounded leak.
func (e *apiKeyEphemeralResource) Close(ctx context.Context, req ephemeral.CloseRequest, resp *ephemeral.CloseResponse) {
	if req.Private == nil {
		return
	}
	st, ok := e.privateState(ctx, req.Private, &resp.Diagnostics)
	if !ok {
		return
	}
	resp.Diagnostics.Append(e.revoke(ctx, st.ID)...)
}

// revoke deletes a key, downgrading every failure to a warning. An already-gone
// key is a success: the goal is that the credential stops working, and it has.
func (e *apiKeyEphemeralResource) revoke(ctx context.Context, id string) diag.Diagnostics {
	var diags diag.Diagnostics
	if err := e.client.RevokeAPIKey(ctx, id); err != nil && !client.IsNotFound(err) {
		diags.AddWarning("Unable to revoke ephemeral API key",
			fmt.Sprintf("API key %s could not be revoked: %s.\n\nIt will still stop working when it "+
				"expires. Revoke it in the dashboard if you want it gone sooner.", id, err))
	}
	return diags
}

// privateState decodes what Open stashed. A missing entry means Open never
// completed, so there is nothing to renew or revoke and nothing to report.
func (e *apiKeyEphemeralResource) privateState(ctx context.Context, priv privateGetter, diags *diag.Diagnostics) (ephemeralKeyState, bool) {
	var st ephemeralKeyState
	if priv == nil {
		return st, false
	}
	raw, d := priv.GetKey(ctx, ephemeralPrivateKey)
	diags.Append(d...)
	if diags.HasError() || len(raw) == 0 {
		return st, false
	}
	if err := json.Unmarshal(raw, &st); err != nil || st.ID == "" {
		diags.AddWarning("Unable to read ephemeral API key state",
			"The key created for this run could not be identified, so it was not revoked. It will "+
				"still stop working when it expires.")
		return st, false
	}
	return st, true
}

// privateGetter is the read side of the framework's private-state type, which
// lives in an internal package and so cannot be named directly.
type privateGetter interface {
	GetKey(ctx context.Context, key string) ([]byte, diag.Diagnostics)
}
