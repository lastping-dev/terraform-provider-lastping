package provider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"

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
		out := client.APIKey{
			ID:        "11111111-1111-4111-a111-111111111111",
			Name:      "k",
			Prefix:    "lp_abcdefg",
			CreatedAt: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
			ExpiresAt: expires,
			Key:       "lp_abcdefg_secret",
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
	plan.Key = types.StringUnknown()
	if plan.ExpiresAt.IsNull() {
		plan.ExpiresAt = types.StringUnknown()
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
