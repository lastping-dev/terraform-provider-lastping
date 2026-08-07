package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/stretchr/testify/require"

	"github.com/lastping-dev/terraform-provider-lastping/internal/client"
)

// agentSchema returns the resource schema for direct, backend-free assertions.
func agentSchema(t *testing.T) schema.Schema {
	t.Helper()
	resp := &resource.SchemaResponse{}
	NewAgentResource().Schema(context.Background(), resource.SchemaRequest{}, resp)
	require.False(t, resp.Diagnostics.HasError(), "%v", resp.Diagnostics)
	return resp.Schema
}

// validateAgentName runs every validator declared on the name attribute.
func validateAgentName(t *testing.T, value string) validator.StringResponse {
	t.Helper()
	attr, ok := agentSchema(t).Attributes["name"].(schema.StringAttribute)
	require.True(t, ok, "name must be a string attribute")

	req := validator.StringRequest{ConfigValue: types.StringValue(value)}
	resp := validator.StringResponse{}
	for _, v := range attr.Validators {
		v.ValidateString(context.Background(), req, &resp)
	}
	return resp
}

// TestDeriveAgentSlug pins the provider's copy of the server's slugFromName
// (api/agents_api.go). The provider never SENDS a slug, so a disagreement here
// cannot corrupt state — but it would misreport the slug in the collision
// diagnostic and in the plan-time name check, which is where an operator is
// least able to tell a provider bug from an API one.
func TestDeriveAgentSlug(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"Nightly ETL bot", "nightly-etl-bot"},
		{"nightly-etl-bot", "nightly-etl-bot"},
		// Runs of non-alphanumerics collapse to a single hyphen, and leading
		// and trailing ones disappear entirely.
		{"  Deploy   Bot!!  ", "deploy-bot"},
		{"--Deploy--Bot--", "deploy-bot"},
		{"CI/CD runner (eu-west-1)", "ci-cd-runner-eu-west-1"},
		// Non-ASCII is outside [a-z0-9] even after lowercasing, so it behaves
		// like punctuation rather than transliterating.
		{"Ünicode bot", "nicode-bot"},
		// Nothing slugifiable at all. The server answers 400 here; the
		// validator below turns it into a plan-time error.
		{"!!!", ""},
		{"", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, deriveAgentSlug(tc.name))
		})
	}
}

// TestAgentNameIsValidatedAtPlanTime covers the refusals that mirror the
// server's own validateSlug, so "cannot derive a valid slug from name" is a
// plan-time error naming the attribute instead of an opaque 400 partway through
// an apply. The length rule is the one worth catching early: it applies to the
// DERIVED SLUG, not to the name, so a name that looks entirely reasonable can
// fail for a reason nothing in the configuration shows.
func TestAgentNameIsValidatedAtPlanTime(t *testing.T) {
	t.Run("an ordinary name is accepted", func(t *testing.T) {
		require.False(t, validateAgentName(t, "Nightly ETL bot").Diagnostics.HasError())
	})

	t.Run("a name with nothing slugifiable is rejected", func(t *testing.T) {
		resp := validateAgentName(t, "!!!")
		require.True(t, resp.Diagnostics.HasError())
		require.Contains(t, resp.Diagnostics.Errors()[0].Summary(), "does not yield a valid slug")
	})

	t.Run("an empty name is rejected", func(t *testing.T) {
		resp := validateAgentName(t, "   ")
		require.True(t, resp.Diagnostics.HasError())
		require.Contains(t, resp.Diagnostics.Errors()[0].Summary(), "empty")
	})

	t.Run("a derived slug under 3 characters is rejected", func(t *testing.T) {
		resp := validateAgentName(t, "AI")
		require.True(t, resp.Diagnostics.HasError())
		// The diagnostic has to show the derived slug: the name is two
		// characters, but the rule the server applied is about "ai".
		require.Contains(t, resp.Diagnostics.Errors()[0].Detail(), `"ai"`)
	})

	t.Run("a derived slug over 50 characters is rejected", func(t *testing.T) {
		long := strings.Repeat("a", 51)
		resp := validateAgentName(t, long)
		require.True(t, resp.Diagnostics.HasError(),
			"the 50-character limit is on the derived slug, and the API rejects this")
	})

	t.Run("a UUID-shaped derived slug is rejected", func(t *testing.T) {
		// The name is not itself a UUID, but the derivation turns it into one —
		// which is exactly the case a check on the raw name would miss.
		resp := validateAgentName(t, "6ba7b810 9dad 11d1 80b4 00c04fd430c8")
		require.True(t, resp.Diagnostics.HasError())
		require.Contains(t, resp.Diagnostics.Errors()[0].Summary(), "UUID-shaped")
	})
}

// TestAgentDescriptionIsNotComputed pins the "attributes can never be unset"
// regression: description is only ever supplied by the configuration, so
// marking it Computed to suppress a diff would keep the stored value in state
// forever when it is removed. slug/status/monitor_count/last_seen are the
// converse — genuinely server-supplied, and never configurable.
func TestAgentDescriptionIsNotComputed(t *testing.T) {
	s := agentSchema(t)

	desc, ok := s.Attributes["description"]
	require.True(t, ok, "missing attribute description")
	require.True(t, desc.IsOptional(), "description must be optional")
	require.False(t, desc.IsComputed(),
		"description is only ever supplied by the configuration, so Computed would make it "+
			"impossible to unset")

	name, ok := s.Attributes["name"]
	require.True(t, ok, "missing attribute name")
	require.True(t, name.IsRequired(), "the API requires a name and derives the slug from it")

	for _, attrName := range []string{"id", "slug", "status", "monitor_count", "last_seen", "created_at"} {
		attr, ok := s.Attributes[attrName]
		require.True(t, ok, "missing attribute %s", attrName)
		require.True(t, attr.IsComputed(), "%s is server-supplied and must stay computed", attrName)
		require.False(t, attr.IsOptional(), "%s is not configurable", attrName)
	}
}

// TestAgentVolatileAttributesHaveNoUseStateForUnknown guards the rollup fields.
//
// status, monitor_count and last_seen are recomputed by the API on every
// response from the monitors the agent owns — nothing stores them. Pinning them
// with UseStateForUnknown would freeze a stale rollup into the plan and, worse,
// promise a value the apply then contradicts. slug and created_at are the
// opposite: immutable once set, so they SHOULD be pinned, or every rename plans
// them as "(known after apply)" and implies the slug is about to move.
func TestAgentVolatileAttributesHaveNoUseStateForUnknown(t *testing.T) {
	s := agentSchema(t)

	for _, name := range []string{"status", "monitor_count", "last_seen"} {
		attr, ok := s.Attributes[name]
		require.True(t, ok, "missing attribute %s", name)
		switch a := attr.(type) {
		case schema.StringAttribute:
			require.Empty(t, a.PlanModifiers, "%s is rolled up live and must not be pinned to prior state", name)
		case schema.Int64Attribute:
			require.Empty(t, a.PlanModifiers, "%s is counted live and must not be pinned to prior state", name)
		default:
			t.Fatalf("unexpected attribute type for %s", name)
		}
	}

	for _, name := range []string{"id", "slug", "created_at"} {
		a, ok := s.Attributes[name].(schema.StringAttribute)
		require.True(t, ok, "%s must be a string attribute", name)
		require.NotEmpty(t, a.PlanModifiers,
			"%s never changes after creation, so it must use prior state rather than plan as unknown", name)
	}
}

// TestAgentPatchFromModel is the merge-patch gate. The API distinguishes an
// absent key (preserve) from an explicit null (clear), and description is the
// only clearable field — so a name-only rename must not wipe it, and a removed
// description must actually reach the wire as `null`.
func TestAgentPatchFromModel(t *testing.T) {
	stored := agentResourceModel{
		Name:        types.StringValue("Nightly ETL bot"),
		Description: types.StringValue("Runs the nightly ETL pipeline."),
	}

	t.Run("a configured description is sent verbatim", func(t *testing.T) {
		got := agentPatchFromModel(stored, stored)
		require.Equal(t, "Nightly ETL bot", got["name"])
		require.Equal(t, "Runs the nightly ETL pipeline.", got["description"])
	})

	t.Run("a removed description is sent as an explicit null", func(t *testing.T) {
		cleared := stored
		cleared.Description = types.StringNull()

		got := agentPatchFromModel(cleared, cleared)
		require.Contains(t, got, "description",
			"omitting the key would leave the stored description in place under merge-patch, "+
				"making 'remove the description' unreachable")
		require.Nil(t, got["description"])

		body, err := json.Marshal(got)
		require.NoError(t, err)
		require.Contains(t, string(body), `"description":null`)
	})

	t.Run("a rename keeps a description the configuration still sets", func(t *testing.T) {
		renamed := stored
		renamed.Name = types.StringValue("Hourly ETL bot")

		got := agentPatchFromModel(renamed, renamed)
		require.Equal(t, "Hourly ETL bot", got["name"])
		require.Equal(t, "Runs the nightly ETL pipeline.", got["description"],
			"a name-only change must never blank the description")
	})

	t.Run("a null configuration clears even when the plan carries a value", func(t *testing.T) {
		// Unreachable while description stays Optional-only (see
		// TestAgentDescriptionIsNotComputed): a null config always means a null
		// plan, which is why the subtests above pass the same model twice.
		//
		// It is asserted anyway, because it is the only case the cfg argument
		// exists for. Making description Optional+Computed would produce
		// exactly this shape on every apply that does not mention it, and this
		// assertion is what keeps the defensive branch from rotting into dead
		// code nobody dares delete.
		cfg := stored
		cfg.Description = types.StringNull()

		got := agentPatchFromModel(stored, cfg)
		require.Nil(t, got["description"], "a null config must clear, not echo the plan's value back")
	})

	t.Run("slug is never sent", func(t *testing.T) {
		got := agentPatchFromModel(stored, stored)
		require.NotContains(t, got, "slug",
			"slug is immutable server-side; sending it would imply Terraform can change it")
	})
}

// TestModelFromAgent covers the two mappings that are not straight copies.
//
// The API reports "no description" as "" rather than omitting it (the column is
// NOT NULL with an empty-string default), so it has to come back as null —
// otherwise an agent created without one reads as "" and permanently disagrees
// with the null a configuration that omits the attribute plans.
func TestModelFromAgent(t *testing.T) {
	t.Run("an empty description reads back as null", func(t *testing.T) {
		got := modelFromAgent(&client.Agent{
			ID: "id", Slug: "bot", Name: "Bot", Description: "",
			Status: "idle", MonitorCount: 0, CreatedAt: "2026-07-01T12:00:00Z",
		})
		require.True(t, got.Description.IsNull(),
			`the API's "" means "unset", and must not plan against a configuration's null`)
	})

	t.Run("a never-seen agent reads back as null, and zero monitors as 0", func(t *testing.T) {
		got := modelFromAgent(&client.Agent{
			ID: "id", Slug: "bot", Name: "Bot", Status: "idle",
			MonitorCount: 0, LastSeen: nil, CreatedAt: "2026-07-01T12:00:00Z",
		})
		require.True(t, got.LastSeen.IsNull())
		require.False(t, got.MonitorCount.IsNull(),
			"an agent with no monitors owns zero of them; that is a value, not an absence")
		require.Equal(t, int64(0), got.MonitorCount.ValueInt64())
	})

	t.Run("the rollup fields are copied through", func(t *testing.T) {
		seen := "2026-07-10T03:00:05Z"
		got := modelFromAgent(&client.Agent{
			ID: "id", Slug: "bot", Name: "Bot", Description: "d",
			Status: "late", MonitorCount: 3, LastSeen: &seen,
			CreatedAt: "2026-07-01T12:00:00Z",
		})
		require.Equal(t, "late", got.Status.ValueString())
		require.Equal(t, int64(3), got.MonitorCount.ValueInt64())
		require.Equal(t, seen, got.LastSeen.ValueString())
		require.Equal(t, "bot", got.Slug.ValueString())
	})
}
