package provider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"

	"context"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/stretchr/testify/require"

	"github.com/lastping-dev/terraform-provider-lastping/internal/client"
)

// TestAPIKeyModelPreservesPlaintext is the security-critical invariant: the
// plaintext is returned exactly once, so a refresh must carry the prior value
// through untouched. Overwriting it with the response's empty string would
// destroy the only copy that exists anywhere.
func TestAPIKeyModelPreservesPlaintext(t *testing.T) {
	created := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

	t.Run("create takes the key from the response", func(t *testing.T) {
		got := modelFromAPIKey(&client.APIKey{
			ID: "id", Name: "n", Prefix: "lp_abcdefg", CreatedAt: created, Key: "lp_abcdefg_secret",
		}, apiKeyResourceModel{Key: types.StringUnknown()})
		require.Equal(t, "lp_abcdefg_secret", got.Key.ValueString())
		require.Equal(t, "2026-07-26T12:00:00Z", got.CreatedAt.ValueString())
	})

	t.Run("refresh keeps the prior key", func(t *testing.T) {
		prior := apiKeyResourceModel{Key: types.StringValue("lp_abcdefg_secret")}
		got := modelFromAPIKey(&client.APIKey{
			ID: "id", Name: "n", Prefix: "lp_abcdefg", CreatedAt: created,
		}, prior)
		require.Equal(t, "lp_abcdefg_secret", got.Key.ValueString(),
			"a list response carries no plaintext and must not blank the stored key")
	})
}

// TestAPIKeyLastUsedAtValue covers both states the API can report: a key
// that has authenticated at least one request, and one that never has, which
// the API represents as an absent field (nil, not a zero time) rather than a
// zero/epoch timestamp.
func TestAPIKeyLastUsedAtValue(t *testing.T) {
	t.Run("populated from a response that carries it", func(t *testing.T) {
		used := time.Date(2026, 8, 1, 9, 30, 0, 0, time.UTC)
		got := modelFromAPIKey(&client.APIKey{
			ID: "id", Name: "n", Prefix: "lp_abcdefg", CreatedAt: used, LastUsedAt: &used,
		}, apiKeyResourceModel{Key: types.StringUnknown()})
		require.Equal(t, "2026-08-01T09:30:00Z", got.LastUsedAt.ValueString())
	})

	t.Run("null when the API returns none, for a never-used key", func(t *testing.T) {
		created := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
		got := modelFromAPIKey(&client.APIKey{
			ID: "id", Name: "n", Prefix: "lp_abcdefg", CreatedAt: created, LastUsedAt: nil,
		}, apiKeyResourceModel{Key: types.StringUnknown()})
		require.True(t, got.LastUsedAt.IsNull())
	})
}

// TestAPIKeyExpiresAtValue covers the UTC-normalisation round trip. The API
// stores timestamptz and always answers in UTC, so a configured offset comes
// back spelled differently; without this the apply fails as an inconsistent
// result and every later plan proposes a replacement.
//
// The last case is the one with teeth: a genuinely different instant must NOT
// be masked by echoing prior back, or an expiry changed in the database would
// be invisible forever.
func TestAPIKeyExpiresAtValue(t *testing.T) {
	utc := time.Date(2026, 12, 31, 23, 0, 0, 0, time.UTC)

	t.Run("absent stays absent", func(t *testing.T) {
		require.True(t, expiresAtValue(nil, types.StringNull()).IsNull())
		require.True(t, expiresAtValue(nil, types.StringValue("2027-01-01T00:00:00+01:00")).IsNull(),
			"an expiry removed server-side must surface, not be echoed back")
	})

	t.Run("equivalent offset keeps the configured spelling", func(t *testing.T) {
		got := expiresAtValue(&utc, types.StringValue("2027-01-01T00:00:00+01:00"))
		require.Equal(t, "2027-01-01T00:00:00+01:00", got.ValueString())
	})

	t.Run("no prior takes the server value", func(t *testing.T) {
		require.Equal(t, "2026-12-31T23:00:00Z", expiresAtValue(&utc, types.StringNull()).ValueString())
		require.Equal(t, "2026-12-31T23:00:00Z", expiresAtValue(&utc, types.StringUnknown()).ValueString())
	})

	t.Run("a different instant surfaces as drift", func(t *testing.T) {
		got := expiresAtValue(&utc, types.StringValue("2030-01-01T00:00:00Z"))
		require.Equal(t, "2026-12-31T23:00:00Z", got.ValueString())
	})
}

// TestFutureTimestampValidator mirrors api/apikeys_api.go, which rejects an
// expires_at that is not After(now). Refusing it at plan time can only move the
// server's error earlier; refusing anything the server would accept would be a
// bug, hence the "well into the future" case.
func TestFutureTimestampValidator(t *testing.T) {
	run := func(v string) validator.StringResponse {
		req := validator.StringRequest{Path: path.Root("expires_at"), ConfigValue: types.StringValue(v)}
		resp := validator.StringResponse{}
		futureTimestampValidator{}.ValidateString(context.Background(), req, &resp)
		return resp
	}

	require.False(t, run(time.Now().Add(time.Hour).Format(time.RFC3339)).Diagnostics.HasError())
	require.True(t, run("2020-01-01T00:00:00Z").Diagnostics.HasError())
	require.Contains(t, run("2020-01-01T00:00:00Z").Diagnostics.Errors()[0].Summary(),
		"Expiry is not in the future")

	// A malformed value belongs to rfc3339Validator; two errors for one typo is
	// noise, so this validator stays quiet.
	require.False(t, run("next tuesday").Diagnostics.HasError())

	for _, absent := range []types.String{types.StringNull(), types.StringUnknown()} {
		resp := validator.StringResponse{}
		futureTimestampValidator{}.ValidateString(context.Background(),
			validator.StringRequest{Path: path.Root("expires_at"), ConfigValue: absent}, &resp)
		require.False(t, resp.Diagnostics.HasError())
	}
}

// apiKeySchema returns the resource schema for backend-free assertions.
func apiKeySchema(t *testing.T) schema.Schema {
	t.Helper()
	resp := &resource.SchemaResponse{}
	NewAPIKeyResource().Schema(context.Background(), resource.SchemaRequest{}, resp)
	require.False(t, resp.Diagnostics.HasError(), "%v", resp.Diagnostics)
	return resp.Schema
}

// apiKeyRawValue renders a model as the raw Terraform value tfsdk.State and
// tfsdk.Plan carry. The plan modifiers read it to tell a create (null state)
// and a destroy (null plan) from an ordinary change.
func apiKeyRawValue(t *testing.T, m apiKeyResourceModel) tftypes.Value {
	t.Helper()
	ctx := context.Background()

	objType, ok := apiKeySchema(t).Type().(types.ObjectType)
	require.True(t, ok, "a resource schema is always an object")

	obj, diags := types.ObjectValueFrom(ctx, objType.AttributeTypes(), m)
	require.False(t, diags.HasError(), "%v", diags)

	raw, err := obj.ToTerraformValue(ctx)
	require.NoError(t, err)
	return raw
}

// apiKeyExpiresAtPlan runs the plan modifiers the SCHEMA actually declares for
// expires_at — not a copy of them — over one plan, and reports the planned value
// and whether a replacement was demanded. Each modifier sees the value the one
// before it produced, exactly as the framework chains them.
//
// creating models Terraform's own create call, where prior state is null: the
// framework marks a Computed attribute with no configured value unknown, and
// whether it STAYS unknown decides whether the server is allowed to answer.
func apiKeyExpiresAtPlan(t *testing.T, state, config, plan types.String, creating bool) (types.String, bool) {
	t.Helper()
	ctx := context.Background()

	attr, ok := apiKeySchema(t).Attributes["expires_at"].(schema.StringAttribute)
	require.True(t, ok, "expires_at must be a string attribute")

	stateRaw := apiKeyRawValue(t, apiKeyResourceModel{Name: types.StringValue("k"), ExpiresAt: state})
	if creating {
		stateRaw = tftypes.NewValue(stateRaw.Type(), nil)
	}
	planRaw := apiKeyRawValue(t, apiKeyResourceModel{Name: types.StringValue("k"), ExpiresAt: plan})
	configRaw := apiKeyRawValue(t, apiKeyResourceModel{Name: types.StringValue("k"), ExpiresAt: config})

	resp := &planmodifier.StringResponse{PlanValue: plan}
	for _, mod := range attr.PlanModifiers {
		mod.PlanModifyString(ctx, planmodifier.StringRequest{
			Path:        path.Root("expires_at"),
			State:       tfsdk.State{Schema: apiKeySchema(t), Raw: stateRaw},
			Plan:        tfsdk.Plan{Schema: apiKeySchema(t), Raw: planRaw},
			Config:      tfsdk.Config{Schema: apiKeySchema(t), Raw: configRaw},
			StateValue:  state,
			PlanValue:   resp.PlanValue,
			ConfigValue: config,
		}, resp)
	}
	return resp.PlanValue, resp.RequiresReplace
}

// TestAPIKeyExpiresAtIsServerSettable pins the schema shape the whole change
// rests on. Optional alone cannot hold a value the server chose: an apply that
// returns one fails with "Provider produced inconsistent result after apply".
func TestAPIKeyExpiresAtIsServerSettable(t *testing.T) {
	attr, ok := apiKeySchema(t).Attributes["expires_at"].(schema.StringAttribute)
	require.True(t, ok)
	require.True(t, attr.Optional, "a practitioner must still be able to ask for an expiry")
	require.True(t, attr.Computed, "the server assigns one when the creating key expires")
}

// TestAPIKeyExpiresAtPlanIsStable is the property the fix exists for: whatever
// the server decided about an OMITTED expires_at, the next plan proposes
// nothing. Optional+Computed without UseStateForUnknown plans unknown on every
// run instead, and for this resource a proposed change means revoking a live
// credential.
func TestAPIKeyExpiresAtPlanIsStable(t *testing.T) {
	inherited := types.StringValue("2027-03-01T00:00:00Z")

	t.Run("omitted and server-assigned: state is kept, nothing is replaced", func(t *testing.T) {
		// State holds the creating key's expiry, copied in by Create. The
		// framework offers unknown because nothing is configured.
		got, replace := apiKeyExpiresAtPlan(t, inherited, types.StringNull(), types.StringUnknown(), false)
		require.Equal(t, inherited, got, "a server-assigned expiry must survive the next plan")
		require.False(t, got.IsUnknown(), "an unknown here is a diff on every plan")
		require.False(t, replace, "the server assigning an expiry is not a configuration change")
	})

	t.Run("omitted and never expiring: unknown collapses to null", func(t *testing.T) {
		got, replace := apiKeyExpiresAtPlan(t, types.StringNull(), types.StringNull(), types.StringUnknown(), false)
		require.True(t, got.IsNull(), "a null expiry must stay null, not become known after apply")
		require.False(t, replace)
	})

	t.Run("create leaves it unknown so the server may decide", func(t *testing.T) {
		// Terraform re-plans a replacement against null prior state, and that is
		// the plan the new key's expiry has to fit: pinning the OLD key's expiry
		// here would fail the apply whenever the creating key has changed.
		got, replace := apiKeyExpiresAtPlan(t, types.StringNull(), types.StringNull(), types.StringUnknown(), true)
		require.True(t, got.IsUnknown())
		require.False(t, replace)
	})
}

// TestAPIKeyExpiresAtConfiguredRoundTrip: a configured value is still the
// practitioner's, Computed or not. It survives a plan untouched, and changing it
// still replaces the key, because the API cannot move an expiry.
func TestAPIKeyExpiresAtConfiguredRoundTrip(t *testing.T) {
	configured := types.StringValue("2027-01-01T00:00:00+01:00")

	t.Run("unchanged configuration proposes nothing", func(t *testing.T) {
		got, replace := apiKeyExpiresAtPlan(t, configured, configured, configured, false)
		require.Equal(t, configured, got, "the configured spelling must not be rewritten")
		require.False(t, replace)
	})

	t.Run("a changed expiry replaces the key", func(t *testing.T) {
		next := types.StringValue("2028-01-01T00:00:00Z")
		got, replace := apiKeyExpiresAtPlan(t, configured, next, next, false)
		require.Equal(t, next, got)
		require.True(t, replace, "the API has no way to move an expiry, so this must replace")
	})
}

// TestAPIKeyExpiresAtConflict covers how a server value that CONTRADICTS a
// configured one is surfaced. Silence is the one unacceptable answer: it would
// hand back a credential with a lifetime nobody asked for.
func TestAPIKeyExpiresAtConflict(t *testing.T) {
	want := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)

	t.Run("an omitted expiry is never a conflict", func(t *testing.T) {
		_, conflict := expiresAtConflict(types.StringNull(), &want)
		require.False(t, conflict, "a server value for an omitted attribute IS the answer")
		_, conflict = expiresAtConflict(types.StringUnknown(), &want)
		require.False(t, conflict)
	})

	t.Run("the same instant in another spelling agrees", func(t *testing.T) {
		_, conflict := expiresAtConflict(types.StringValue("2027-01-01T01:00:00+01:00"), &want)
		require.False(t, conflict, "UTC normalisation is not a disagreement")
	})

	t.Run("a different instant conflicts and reports the server value", func(t *testing.T) {
		got, conflict := expiresAtConflict(types.StringValue("2030-01-01T00:00:00Z"), &want)
		require.True(t, conflict)
		require.Equal(t, want, *got, "the diagnostic needs the ceiling the server allowed")
	})

	t.Run("a key asked to expire that does not conflicts", func(t *testing.T) {
		got, conflict := expiresAtConflict(types.StringValue("2027-01-01T00:00:00Z"), nil)
		require.True(t, conflict)
		require.Nil(t, got)
	})
}

// apiKeyCreateServer answers POST /api/v1/api-keys with a fixed expires_at,
// standing in for a backend that gives a new key its creator's expiry
// (LP_API_KEY_INHERIT_EXPIRY). requested captures the body so a test can prove
// what the provider actually asked for.
func apiKeyCreateServer(t *testing.T, expires *time.Time, requested *map[string]any) *client.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if requested != nil {
			*requested = body
		}
		// The real API echoes the scope it granted and names the key that
		// minted this one; a stub that omitted both would let a provider that
		// never reads them pass.
		scope, _ := body["scope"].(string)
		if scope == "" {
			scope = "write"
		}
		parent := "22222222-2222-4222-a222-222222222222"
		out := client.APIKey{
			ID:             "11111111-1111-4111-a111-111111111111",
			Name:           "k",
			Prefix:         "lp_abcdefg",
			CreatedAt:      time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
			ExpiresAt:      expires,
			Scope:          scope,
			CreatedByKeyID: &parent,
			Key:            "lp_abcdefg_secret",
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return client.New(srv.URL, "lp_test", "unit")
}

// apiKeyCreate drives the real Create entry point against that server and hands
// back the state it wrote plus its diagnostics.
func apiKeyCreate(t *testing.T, c *client.Client, config apiKeyResourceModel) (apiKeyResourceModel, diag.Diagnostics) {
	t.Helper()
	ctx := context.Background()
	s := apiKeySchema(t)

	// The plan is what the framework produces from this config: a configured
	// expires_at is planned as itself, an omitted one as unknown.
	plan := config
	plan.ID = types.StringUnknown()
	plan.Prefix = types.StringUnknown()
	plan.CreatedAt = types.StringUnknown()
	plan.LastUsedAt = types.StringUnknown()
	plan.CreatedByKeyID = types.StringUnknown()
	plan.Key = types.StringUnknown()
	if plan.ExpiresAt.IsNull() {
		plan.ExpiresAt = types.StringUnknown()
	}
	// scope has no Default, so an omitted one reaches Create as unknown, the
	// same as expires_at — the server is what fills it in.
	if plan.Scope.IsNull() {
		plan.Scope = types.StringUnknown()
	}

	r := &apiKeyResource{client: c}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(apiKeyRawValue(t, config).Type(), nil)},
	}
	r.Create(ctx, resource.CreateRequest{
		Plan:   tfsdk.Plan{Schema: s, Raw: apiKeyRawValue(t, plan)},
		Config: tfsdk.Config{Schema: s, Raw: apiKeyRawValue(t, config)},
	}, resp)

	var got apiKeyResourceModel
	if !resp.State.Raw.IsNull() {
		resp.Diagnostics.Append(resp.State.Get(ctx, &got)...)
	}
	return got, resp.Diagnostics
}

// TestAPIKeyCreateTakesServerExpiry drives Create itself, because the schema
// change is worthless if the response value never reaches state. The two halves
// the server distinguishes are both here: an omitted expiry the server fills in,
// and a configured one it accepts unchanged.
func TestAPIKeyCreateTakesServerExpiry(t *testing.T) {
	inherited := time.Date(2027, 3, 1, 9, 0, 0, 0, time.UTC)

	t.Run("omitted: the server's value lands in state", func(t *testing.T) {
		var sent map[string]any
		got, diags := apiKeyCreate(t, apiKeyCreateServer(t, &inherited, &sent),
			apiKeyResourceModel{Name: types.StringValue("k")})
		require.False(t, diags.HasError(), "%v", diags)
		require.Equal(t, "2027-03-01T09:00:00Z", got.ExpiresAt.ValueString(),
			"an inherited expiry must be recorded, not dropped")
		require.NotContains(t, sent, "expires_at",
			"a configuration that omits an expiry must not request one")
		require.Equal(t, "lp_abcdefg_secret", got.Key.ValueString())
	})

	t.Run("configured: the accepted value keeps its spelling", func(t *testing.T) {
		// The same instant, spelled with an offset, as the API answers in UTC.
		utc := time.Date(2026, 12, 31, 23, 0, 0, 0, time.UTC)
		var sent map[string]any
		got, diags := apiKeyCreate(t, apiKeyCreateServer(t, &utc, &sent), apiKeyResourceModel{
			Name:      types.StringValue("k"),
			ExpiresAt: types.StringValue("2027-01-01T00:00:00+01:00"),
		})
		require.False(t, diags.HasError(), "%v", diags)
		require.Equal(t, "2027-01-01T00:00:00+01:00", got.ExpiresAt.ValueString())
		require.Equal(t, "2026-12-31T23:00:00Z", sent["expires_at"],
			"the configured expiry is what gets requested")
	})

	t.Run("contradicted: an error, and the key is still recorded", func(t *testing.T) {
		got, diags := apiKeyCreate(t, apiKeyCreateServer(t, &inherited, nil), apiKeyResourceModel{
			Name:      types.StringValue("k"),
			ExpiresAt: types.StringValue("2030-01-01T00:00:00Z"),
		})
		require.True(t, diags.HasError(), "a lifetime the practitioner did not ask for must be reported")
		require.Contains(t, diags.Errors()[0].Summary(), "Server did not honour the configured expiry")
		require.Contains(t, diags.Errors()[0].Detail(), "2027-03-01T09:00:00Z",
			"the diagnostic must name the expiry the key actually got")
		require.Equal(t, "11111111-1111-4111-a111-111111111111", got.ID.ValueString(),
			"state must still hold the minted key, or it is orphaned on the server")
	})
}

// apiKeyScopePlan runs the plan modifiers the SCHEMA declares for scope — not a
// copy of them — over one plan, and reports whether a replacement was demanded.
// Mirrors apiKeyExpiresAtPlan; the two attributes deliberately carry different
// modifiers, and asserting on the declared ones is what keeps that deliberate.
func apiKeyScopePlan(t *testing.T, state, config, plan types.String, creating bool) (types.String, bool) {
	t.Helper()
	ctx := context.Background()

	attr, ok := apiKeySchema(t).Attributes["scope"].(schema.StringAttribute)
	require.True(t, ok, "scope must be a string attribute")

	model := func(v types.String) apiKeyResourceModel {
		return apiKeyResourceModel{Name: types.StringValue("k"), Scope: v}
	}
	stateRaw := apiKeyRawValue(t, model(state))
	if creating {
		stateRaw = tftypes.NewValue(stateRaw.Type(), nil)
	}
	resp := &planmodifier.StringResponse{PlanValue: plan}
	for _, mod := range attr.PlanModifiers {
		mod.PlanModifyString(ctx, planmodifier.StringRequest{
			Path:        path.Root("scope"),
			State:       tfsdk.State{Schema: apiKeySchema(t), Raw: stateRaw},
			Plan:        tfsdk.Plan{Schema: apiKeySchema(t), Raw: apiKeyRawValue(t, model(plan))},
			Config:      tfsdk.Config{Schema: apiKeySchema(t), Raw: apiKeyRawValue(t, model(config))},
			StateValue:  state,
			PlanValue:   resp.PlanValue,
			ConfigValue: config,
		}, resp)
	}
	return resp.PlanValue, resp.RequiresReplace
}

// TestAPIKeyScopeSchema pins the shape, and the absence of a Default is the
// load-bearing half.
//
// A Default is re-applied on EVERY plan wherever the config is null — the
// framework never looks at the value carried forward from prior state — so
// `Default: "write"` next to a replacement modifier planned any key the server
// says is `admin` (every key that predates the scope column) down to `write`
// and proposed replacing it. Replacing an API key revokes it, and revocation
// cascades to every key it minted, so a bare provider upgrade would have
// proposed destroying a subtree of live credentials.
func TestAPIKeyScopeSchema(t *testing.T) {
	attr, ok := apiKeySchema(t).Attributes["scope"].(schema.StringAttribute)
	require.True(t, ok)
	require.True(t, attr.Optional, "a practitioner must be able to choose a tier")
	require.True(t, attr.Computed, "the server assigns one for a configuration that omits it")
	require.Nil(t, attr.Default,
		"a Default is re-applied on every plan, which turns an omitted scope into a proposed "+
			"replacement of a live credential")

	// The modifier PAIR, asserted structurally, because the two replacement
	// modifiers are behaviourally identical while there is no Default: with a
	// null config the planned value is the prior one, so neither fires. They
	// diverge only once something re-introduces a Default — which is exactly
	// the change this test exists to stop being silent. Pinning the safe
	// modifier here means such a change has to walk past two failing
	// assertions, not one.
	var sawUseState, sawReplaceIfConfigured bool
	for _, mod := range attr.PlanModifiers {
		// The framework's modifier types are unexported, so they are named by
		// the description they carry rather than by a type assertion.
		switch d := mod.Description(context.Background()); {
		case strings.Contains(d, "value of this attribute in state will not change"):
			sawUseState = true
		case strings.Contains(d, "is configured and changes"):
			// "If the value of this attribute IS CONFIGURED and changes…" —
			// the unconditional RequiresReplace describes itself without that
			// clause, so this distinguishes the two.
			sawReplaceIfConfigured = true
		}
	}
	require.True(t, sawUseState,
		"without UseStateForUnknown the attribute plans as unknown on every run, which is a diff "+
			"on a resource whose replacement revokes a live credential")
	require.True(t, sawReplaceIfConfigured,
		"replacement is for a scope a practitioner wrote down; an omitted scope means "+
			"\"whatever this key already has\"")
	require.Len(t, attr.PlanModifiers, 2, "an unreviewed third modifier changes what a plan does")

	t.Run("only the three storable values are accepted", func(t *testing.T) {
		for _, ok := range apiKeyScopes {
			r := validator.StringResponse{}
			for _, v := range attr.Validators {
				v.ValidateString(context.Background(),
					validator.StringRequest{Path: path.Root("scope"), ConfigValue: types.StringValue(ok)}, &r)
			}
			require.False(t, r.Diagnostics.HasError(), "%q is a scope the API stores", ok)
		}
		for _, bad := range []string{"owner", "Write", "readwrite", ""} {
			r := validator.StringResponse{}
			for _, v := range attr.Validators {
				v.ValidateString(context.Background(),
					validator.StringRequest{Path: path.Root("scope"), ConfigValue: types.StringValue(bad)}, &r)
			}
			require.True(t, r.Diagnostics.HasError(),
				"%q is not a scope the API stores, and a 400 mid-apply is the alternative", bad)
		}
	})
}

// TestAPIKeyScopePlan is the regression for the revocation hazard above, plus
// its positive companion so the absence assertion cannot degrade into
// asserting nothing.
//
// The first case is the one that matters: a key the server says is `admin` —
// which is what EVERY key minted before the scope column existed is, by that
// migration's grandfathering clause — whose configuration says nothing about
// scope, because it was written before the attribute existed. That must plan
// as no change at all. The second and third prove a configured change still
// replaces, which is the only way the API can change a key's scope.
func TestAPIKeyScopePlan(t *testing.T) {
	write := types.StringValue(apiKeyScopeWrite)
	admin := types.StringValue(apiKeyScopeAdmin)
	read := types.StringValue(apiKeyScopeRead)

	t.Run("a grandfathered admin key with nothing configured is left alone", func(t *testing.T) {
		// Terraform offers unknown for a Computed attribute with a null config.
		got, replace := apiKeyScopePlan(t, admin, types.StringNull(), types.StringUnknown(), false)
		require.Equal(t, admin, got, "the scope the key already has must survive the plan")
		require.False(t, got.IsUnknown(), "an unknown here is a diff on every plan")
		require.False(t, replace,
			"an omitted scope is not a demotion request, and replacing revokes a live credential "+
				"along with every key it minted")
	})

	t.Run("unchanged configuration proposes nothing", func(t *testing.T) {
		got, replace := apiKeyScopePlan(t, admin, admin, admin, false)
		require.Equal(t, admin, got)
		require.False(t, replace)
	})

	t.Run("a changed scope replaces the key", func(t *testing.T) {
		got, replace := apiKeyScopePlan(t, write, admin, admin, false)
		require.Equal(t, admin, got)
		require.True(t, replace, "the API cannot move a key's scope, so this must replace")
	})

	t.Run("a demotion a practitioner actually wrote still replaces", func(t *testing.T) {
		_, replace := apiKeyScopePlan(t, admin, read, read, false)
		require.True(t, replace, "asking for less is still a different credential")
	})

	t.Run("create leaves it unknown so the server may decide", func(t *testing.T) {
		got, replace := apiKeyScopePlan(t, types.StringNull(), types.StringNull(),
			types.StringUnknown(), true)
		require.True(t, got.IsUnknown(),
			"pinning a value here would stop the API applying its own default")
		require.False(t, replace)
	})
}

// TestAPIKeyCreateSendsScopeAndRecordsLineage drives the real Create entry
// point, because a schema attribute nothing sends and nothing reads back is
// inert: the request has to carry the scope, and the response's scope and
// created_by_key_id have to reach state.
func TestAPIKeyCreateSendsScopeAndRecordsLineage(t *testing.T) {
	t.Run("an omitted scope asks for nothing and records the server's answer", func(t *testing.T) {
		var sent map[string]any
		got, diags := apiKeyCreate(t, apiKeyCreateServer(t, nil, &sent),
			apiKeyResourceModel{Name: types.StringValue("k")})
		require.False(t, diags.HasError(), "%v", diags)
		require.NotContains(t, sent, "scope",
			"a configuration that chose no scope must let the API apply its own default, and an "+
				"explicit empty scope is a caller error the API refuses")
		require.Equal(t, apiKeyScopeWrite, got.Scope.ValueString(),
			"whatever the server decided has to reach state, or the apply is inconsistent")
		require.Equal(t, "22222222-2222-4222-a222-222222222222", got.CreatedByKeyID.ValueString(),
			"the key that minted this one is what a cascading revocation follows")
	})

	t.Run("a configured scope is what gets requested", func(t *testing.T) {
		var sent map[string]any
		got, diags := apiKeyCreate(t, apiKeyCreateServer(t, nil, &sent), apiKeyResourceModel{
			Name:  types.StringValue("k"),
			Scope: types.StringValue(apiKeyScopeAdmin),
		})
		require.False(t, diags.HasError(), "%v", diags)
		require.Equal(t, apiKeyScopeAdmin, sent["scope"])
		require.Equal(t, apiKeyScopeAdmin, got.Scope.ValueString())
	})

	t.Run("a refused scope names both scopes and creates nothing", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"title":"Bad Request","status":400,
				"detail":"scope may not exceed the creating key's own scope","max_scope":"write"}`))
		}))
		t.Cleanup(srv.Close)

		got, diags := apiKeyCreate(t, client.New(srv.URL, "lp_test", "unit"), apiKeyResourceModel{
			Name:  types.StringValue("k"),
			Scope: types.StringValue(apiKeyScopeAdmin),
		})
		require.True(t, diags.HasError())
		require.Contains(t, diags.Errors()[0].Summary(), "exceeds the creating key's own scope")
		require.Contains(t, diags.Errors()[0].Detail(), "admin")
		require.Contains(t, diags.Errors()[0].Detail(), "write")
		require.True(t, got.ID.IsNull(), "a refused create must leave nothing in state")
	})
}

// TestAPIKeyScopeValue covers the one response the API does not send: a backend
// that predates scopes. Writing "" into state there would contradict the planned
// value and fail the apply with Terraform's own inconsistent-result error, which
// names neither the cause nor the fix.
func TestAPIKeyScopeValue(t *testing.T) {
	prior := types.StringValue(apiKeyScopeAdmin)
	require.Equal(t, prior, scopeValue("", prior),
		"an absent scope means an older backend, not a key without one")
	require.Equal(t, apiKeyScopeRead, scopeValue(apiKeyScopeRead, prior).ValueString(),
		"a scope the server reports is the truth, prior state or not")

	// Through the mapping Read actually uses, not just the helper: a scope
	// changed out of band has to surface as drift, and a model that quietly
	// keeps prior state would hide it forever.
	got := modelFromAPIKey(&client.APIKey{
		ID: "id", Name: "n", Prefix: "lp_abcdefg",
		CreatedAt: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
		Scope:     apiKeyScopeRead,
	}, apiKeyResourceModel{Scope: prior, Key: types.StringValue("lp_abcdefg_secret")})
	require.Equal(t, apiKeyScopeRead, got.Scope.ValueString(),
		"a refresh must report the server's scope, not the one state remembers")
}

// TestAPIKeyCreatedByKeyIDValue: a key with no parent — minted from a dashboard
// session, or one whose parent has been revoked — must read as null, never as
// the zero UUID, which would look like a real key that happens to be all zeros.
func TestAPIKeyCreatedByKeyIDValue(t *testing.T) {
	require.True(t, createdByKeyIDValue(nil).IsNull())

	parent := "e1d2c3b4-0000-0000-0000-000000000002"
	require.Equal(t, parent, createdByKeyIDValue(&parent).ValueString())
}
