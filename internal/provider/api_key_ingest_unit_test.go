package provider

import (
	"context"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	eschema "github.com/hashicorp/terraform-plugin-framework/ephemeral/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/stretchr/testify/require"

	"github.com/lastping-dev/terraform-provider-lastping/internal/client"
)

// The ingest scope and the monitor binding. An ingest key sends pings and
// telemetry and cannot call the management API; check_id binds one to a single
// monitor, and the API accepts it only alongside scope "ingest".

const testMonitorID = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"

// TestAPIKeyScopesIncludeIngest: both key surfaces accept the fourth scope.
func TestAPIKeyScopesIncludeIngest(t *testing.T) {
	require.Contains(t, apiKeyScopes, apiKeyScopeIngest)
	for _, attrs := range []map[string]validator.String{
		{"resource": apiKeySchema(t).Attributes["scope"].(schema.StringAttribute).Validators[0]},
		{"ephemeral": ephemeralAPIKeySchema(t).Attributes["scope"].(eschema.StringAttribute).Validators[0]},
	} {
		for surface, v := range attrs {
			r := validator.StringResponse{}
			v.ValidateString(context.Background(),
				validator.StringRequest{Path: path.Root("scope"), ConfigValue: types.StringValue("ingest")}, &r)
			require.False(t, r.Diagnostics.HasError(), "%s: ingest is a scope the API stores", surface)
		}
	}
}

// TestAPIKeyCheckIDSchema: configurable, never server-assigned, replaced on
// change (the API cannot rebind a key), and shaped like a monitor id.
func TestAPIKeyCheckIDSchema(t *testing.T) {
	attr, ok := apiKeySchema(t).Attributes["check_id"].(schema.StringAttribute)
	require.True(t, ok, "lastping_api_key has no check_id attribute")
	require.True(t, attr.Optional)
	require.False(t, attr.Computed, "the server never binds a key the configuration did not ask to bind")
	require.Nil(t, attr.Default)
	require.Len(t, attr.PlanModifiers, 1, "check_id needs RequiresReplace and nothing else")

	eattr, ok := ephemeralAPIKeySchema(t).Attributes["check_id"].(eschema.StringAttribute)
	require.True(t, ok, "the ephemeral lastping_api_key has no check_id attribute")
	require.True(t, eattr.Optional)

	for _, vs := range [][]validator.String{attr.Validators, eattr.Validators} {
		for _, good := range []string{testMonitorID, "6BA7B810-9DAD-11D1-80B4-00C04FD430C8"} {
			r := validator.StringResponse{}
			for _, v := range vs {
				v.ValidateString(context.Background(),
					validator.StringRequest{Path: path.Root("check_id"), ConfigValue: types.StringValue(good)}, &r)
			}
			require.False(t, r.Diagnostics.HasError(), "%q is a monitor id", good)
		}
		for _, bad := range []string{"nightly-backup", "", "6ba7b810"} {
			r := validator.StringResponse{}
			for _, v := range vs {
				v.ValidateString(context.Background(),
					validator.StringRequest{Path: path.Root("check_id"), ConfigValue: types.StringValue(bad)}, &r)
			}
			require.True(t, r.Diagnostics.HasError(), "%q is not a monitor id", bad)
		}
	}
}

// TestValidateIngestBinding: check_id is refused on any scope but ingest, and
// the positive companion shows it is accepted there.
func TestValidateIngestBinding(t *testing.T) {
	id := types.StringValue(testMonitorID)

	require.False(t, validateIngestBinding(types.StringValue("ingest"), id).HasError(),
		"a bound ingest key is the whole point")
	require.False(t, validateIngestBinding(types.StringValue("write"), types.StringNull()).HasError(),
		"no check_id, nothing to refuse")
	require.False(t, validateIngestBinding(types.StringUnknown(), id).HasError(),
		"an unresolved scope is left to the API")
	require.False(t, validateIngestBinding(types.StringValue("write"), types.StringUnknown()).HasError(),
		"an unresolved check_id is left to the API")

	for _, scope := range []types.String{
		types.StringValue("write"), types.StringValue("admin"), types.StringValue("read"), types.StringNull(),
	} {
		d := validateIngestBinding(scope, id)
		require.True(t, d.HasError(), "check_id with scope %v must be refused at plan time", scope)
		require.Contains(t, d.Errors()[0].Summary(), "ingest")
	}
}

// TestAPIKeyCreateSendsCheckID drives the real Create: the binding is sent with
// the ingest scope and the monitor the server reports reaches state; an
// unbound key sends none and reads back null.
func TestAPIKeyCreateSendsCheckID(t *testing.T) {
	t.Run("a bound ingest key", func(t *testing.T) {
		var sent map[string]any
		got, diags := apiKeyCreate(t, apiKeyCreateServer(t, nil, &sent), apiKeyResourceModel{
			Name:    types.StringValue("tracing"),
			Scope:   types.StringValue(apiKeyScopeIngest),
			CheckID: types.StringValue(testMonitorID),
		})
		require.False(t, diags.HasError(), "%v", diags)
		require.Equal(t, apiKeyScopeIngest, sent["scope"])
		require.Equal(t, testMonitorID, sent["check_id"])
		require.Equal(t, apiKeyScopeIngest, got.Scope.ValueString())
		require.Equal(t, testMonitorID, got.CheckID.ValueString())
	})

	t.Run("an unbound key", func(t *testing.T) {
		var sent map[string]any
		got, diags := apiKeyCreate(t, apiKeyCreateServer(t, nil, &sent), apiKeyResourceModel{
			Name: types.StringValue("k"),
		})
		require.False(t, diags.HasError(), "%v", diags)
		require.NotContains(t, sent, "check_id", "an unbound key must not send the member at all")
		require.True(t, got.CheckID.IsNull())
	})
}

// TestAPIKeyReadsCheckID: a refresh reports the binding the server holds.
func TestAPIKeyReadsCheckID(t *testing.T) {
	id := testMonitorID
	got := modelFromAPIKey(&client.APIKey{
		ID: "id", Name: "n", Prefix: "lp_abcdefg",
		CreatedAt: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
		Scope:     apiKeyScopeIngest,
		CheckID:   &id,
	}, apiKeyResourceModel{Key: types.StringValue("lp_abcdefg_secret")})
	require.Equal(t, testMonitorID, got.CheckID.ValueString())

	got = modelFromAPIKey(&client.APIKey{
		ID: "id", Name: "n", Prefix: "lp_abcdefg",
		CreatedAt: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
		Scope:     apiKeyScopeWrite,
	}, apiKeyResourceModel{})
	require.True(t, got.CheckID.IsNull())
}

// TestEphemeralOpenSendsCheckID: the ephemeral key carries the binding too.
func TestEphemeralOpenSendsCheckID(t *testing.T) {
	sent, _ := ephemeralOpen(t, apiKeyEphemeralModel{
		Name:    types.StringValue("tracing-run"),
		Scope:   types.StringValue(apiKeyScopeIngest),
		CheckID: types.StringValue(testMonitorID),
	})
	require.Equal(t, apiKeyScopeIngest, sent["scope"])
	require.Equal(t, testMonitorID, sent["check_id"])

	sent, _ = ephemeralOpen(t, apiKeyEphemeralModel{Name: types.StringValue("terraform-run")})
	require.NotContains(t, sent, "check_id")
}

// TestAPIKeyValidateConfigRefusesUnboundScope drives both surfaces'
// ValidateConfig, so the rule above is proven wired, not only written.
func TestAPIKeyValidateConfigRefusesUnboundScope(t *testing.T) {
	ctx := context.Background()

	run := func(scope types.String) bool {
		s := apiKeySchema(t)
		resp := &resource.ValidateConfigResponse{}
		(&apiKeyResource{}).ValidateConfig(ctx, resource.ValidateConfigRequest{
			Config: tfsdk.Config{Schema: s, Raw: apiKeyRawValue(t, apiKeyResourceModel{
				Name: types.StringValue("k"), Scope: scope, CheckID: types.StringValue(testMonitorID),
			})},
		}, resp)
		return resp.Diagnostics.HasError()
	}
	require.True(t, run(types.StringValue("write")), "resource: check_id on a write key must be refused")
	require.False(t, run(types.StringValue("ingest")), "resource: check_id on an ingest key is accepted")

	erun := func(scope types.String) bool {
		s := ephemeralAPIKeySchema(t)
		objType, ok := s.Type().(types.ObjectType)
		require.True(t, ok)
		obj, diags := types.ObjectValueFrom(ctx, objType.AttributeTypes(), apiKeyEphemeralModel{
			Name: types.StringValue("k"), TTL: types.StringNull(), Scope: scope,
			CheckID: types.StringValue(testMonitorID),
			ID:      types.StringNull(), Prefix: types.StringNull(), Key: types.StringNull(),
		})
		require.False(t, diags.HasError(), "%v", diags)
		raw, err := obj.ToTerraformValue(ctx)
		require.NoError(t, err)
		resp := &ephemeral.ValidateConfigResponse{}
		(&apiKeyEphemeralResource{}).ValidateConfig(ctx, ephemeral.ValidateConfigRequest{
			Config: tfsdk.Config{Schema: s, Raw: raw},
		}, resp)
		return resp.Diagnostics.HasError()
	}
	require.True(t, erun(types.StringNull()), "ephemeral: check_id with no scope must be refused")
	require.False(t, erun(types.StringValue("ingest")), "ephemeral: check_id on an ingest key is accepted")
}
