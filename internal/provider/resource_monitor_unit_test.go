package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/stretchr/testify/require"

	"github.com/lastping-dev/terraform-provider-lastping/internal/client"
)

// monitorSchema returns the resource schema for direct, backend-free assertions.
func monitorSchema(t *testing.T) schema.Schema {
	t.Helper()
	resp := &resource.SchemaResponse{}
	NewMonitorResource().Schema(context.Background(), resource.SchemaRequest{}, resp)
	require.False(t, resp.Diagnostics.HasError(), "%v", resp.Diagnostics)
	return resp.Schema
}

// validateString runs every validator declared on a string attribute.
func validateString(t *testing.T, attrName, value string) validator.StringResponse {
	t.Helper()
	attr, ok := monitorSchema(t).Attributes[attrName].(schema.StringAttribute)
	require.True(t, ok, "%s must be a string attribute", attrName)

	req := validator.StringRequest{ConfigValue: types.StringValue(value)}
	resp := validator.StringResponse{}
	for _, v := range attr.Validators {
		v.ValidateString(context.Background(), req, &resp)
	}
	return resp
}

// validateInt64 runs every validator declared on an int64 attribute.
func validateInt64(t *testing.T, attrName string, value int64) validator.Int64Response {
	t.Helper()
	attr, ok := monitorSchema(t).Attributes[attrName].(schema.Int64Attribute)
	require.True(t, ok, "%s must be an int64 attribute", attrName)

	req := validator.Int64Request{ConfigValue: types.Int64Value(value)}
	resp := validator.Int64Response{}
	for _, v := range attr.Validators {
		v.ValidateInt64(context.Background(), req, &resp)
	}
	return resp
}

// monitorValidateConfig runs the resource's ValidateConfig against a model, so
// the plan-time refusals can be asserted without a live backend.
func monitorValidateConfig(t *testing.T, m monitorResourceModel) diag.Diagnostics {
	t.Helper()
	ctx := context.Background()
	s := monitorSchema(t)

	objType, ok := s.Type().(types.ObjectType)
	require.True(t, ok, "a resource schema is always an object")

	obj, diags := types.ObjectValueFrom(ctx, objType.AttributeTypes(), m)
	require.False(t, diags.HasError(), "%v", diags)

	raw, err := obj.ToTerraformValue(ctx)
	require.NoError(t, err)

	r, ok := NewMonitorResource().(resource.ResourceWithValidateConfig)
	require.True(t, ok, "the monitor resource must implement ValidateConfig")

	resp := &resource.ValidateConfigResponse{}
	r.ValidateConfig(ctx, resource.ValidateConfigRequest{
		Config: tfsdk.Config{Schema: s, Raw: raw},
	}, resp)
	return resp.Diagnostics
}

// TestMonitorNewInt64Validators pins the two server ranges the provider now
// mirrors at plan time, so an out-of-range value is a plan error naming the
// attribute rather than an opaque 400 partway through an apply.
func TestMonitorNewInt64Validators(t *testing.T) {
	for _, tc := range []struct {
		attr  string
		value int64
		valid bool
	}{
		{"failure_threshold", 1, true},
		{"failure_threshold", 100, true},
		{"failure_threshold", 0, false},
		{"failure_threshold", 101, false},
		{"max_runtime_s", 60, true},
		{"max_runtime_s", 31536000, true},
		{"max_runtime_s", 59, false},
		{"max_runtime_s", 31536001, false},
		{"step_timeout_s", 10, true},
		{"step_timeout_s", 86400, true},
		{"step_timeout_s", 9, false},
		{"step_timeout_s", 86401, false},
	} {
		t.Run(fmt.Sprintf("%s=%d", tc.attr, tc.value), func(t *testing.T) {
			resp := validateInt64(t, tc.attr, tc.value)
			require.Equal(t, !tc.valid, resp.Diagnostics.HasError(), "%v", resp.Diagnostics)
		})
	}
}

// TestMonitorMaxRuntimeRejectedOnHTTP: the API answers 400
// MAX_RUNTIME_NOT_SUPPORTED for max_runtime_s on an http monitor, because a
// probe has no start/success pair for the overrun rule to measure. Catching it
// in ValidateConfig turns that into a plan-time error on the right attribute.
func TestMonitorMaxRuntimeRejectedOnHTTP(t *testing.T) {
	base := monitorResourceModel{
		Name:        types.StringValue("acc"),
		MonitorType: types.StringValue("http"),
		ProbeURL:    types.StringValue("https://example.com/"),
		// The zero types.Set has no element type, which ObjectValueFrom
		// rejects; every other zero value is a well-formed null.
		Tags: types.SetNull(types.StringType),
	}

	withRuntime := base
	withRuntime.MaxRuntimeS = types.Int64Value(14400)
	diags := monitorValidateConfig(t, withRuntime)
	require.True(t, diags.HasError(), "max_runtime_s on an http monitor must be refused at plan time")
	require.Contains(t, diags.Errors()[0].Summary(), "max_runtime_s")

	// Omitted on an http monitor: fine. The PATCH still carries an explicit
	// null, which the API accepts there as a no-op.
	require.False(t, monitorValidateConfig(t, base).HasError())

	// And it is only http that is refused.
	heartbeat := withRuntime
	heartbeat.MonitorType = types.StringValue("heartbeat")
	heartbeat.ProbeURL = types.StringNull()
	require.False(t, monitorValidateConfig(t, heartbeat).HasError())
}

// TestMonitorStepTimeoutRejectedOnHTTP: the API answers 400
// STEP_TIMEOUT_NOT_SUPPORTED for step_timeout_s on an http monitor. A probe
// never arms a run and has no /step endpoint to call, so the stall rule is not
// merely unlikely to fire there — it is unreachable.
func TestMonitorStepTimeoutRejectedOnHTTP(t *testing.T) {
	base := monitorResourceModel{
		Name:        types.StringValue("acc"),
		MonitorType: types.StringValue("http"),
		ProbeURL:    types.StringValue("https://example.com/"),
		// The zero types.Set has no element type, which ObjectValueFrom
		// rejects; every other zero value is a well-formed null.
		Tags: types.SetNull(types.StringType),
	}

	withStep := base
	withStep.StepTimeoutS = types.Int64Value(300)
	diags := monitorValidateConfig(t, withStep)
	require.True(t, diags.HasError(), "step_timeout_s on an http monitor must be refused at plan time")
	require.Contains(t, diags.Errors()[0].Summary(), "step_timeout_s")

	// Omitted on an http monitor: fine. The PATCH still carries an explicit
	// null, which the API accepts there as a no-op.
	require.False(t, monitorValidateConfig(t, base).HasError())

	// And it is only http that is refused. grace_s gives the heartbeat a budget
	// for the step timeout to sit inside, so the budget rule below cannot be
	// what makes this pass or fail.
	heartbeat := withStep
	heartbeat.MonitorType = types.StringValue("heartbeat")
	heartbeat.ProbeURL = types.StringNull()
	heartbeat.GraceS = types.Int64Value(3600)
	require.False(t, monitorValidateConfig(t, heartbeat).HasError())
}

// TestMonitorStepTimeoutBudgetRule mirrors the server's
// STEP_TIMEOUT_EXCEEDS_BUDGET rule at plan time: step_timeout_s must be
// strictly below the effective budget, which is max_runtime_s when it is set
// and grace_s otherwise. At or above it, the stall window is empty and the rule
// can never fire — which is why the API refuses the configuration rather than
// storing a setting that does nothing.
//
// The unknowable cases are asserted too, and they matter as much as the
// rejections: a false plan-time error on a configuration the API would have
// accepted is unfixable by the practitioner.
func TestMonitorStepTimeoutBudgetRule(t *testing.T) {
	base := monitorResourceModel{
		Name:         types.StringValue("acc"),
		MonitorType:  types.StringValue("heartbeat"),
		ScheduleKind: types.StringValue("simple"),
		PeriodS:      types.Int64Value(3600),
		Tags:         types.SetNull(types.StringType),
	}

	for _, tc := range []struct {
		name       string
		grace      types.Int64
		maxRuntime types.Int64
		step       types.Int64
		wantError  bool
	}{
		// max_runtime_s is the budget whenever it is set, even though grace_s
		// is smaller here: a step timeout of 600 is fine against a 4-hour run
		// budget and would be refused against the 300s grace.
		{"below max_runtime", types.Int64Value(300), types.Int64Value(14400), types.Int64Value(600), false},
		{"equal to max_runtime", types.Int64Value(300), types.Int64Value(600), types.Int64Value(600), true},
		{"above max_runtime", types.Int64Value(300), types.Int64Value(600), types.Int64Value(900), true},

		// With max_runtime_s unset the budget falls back to grace_s. This is
		// the exact shape the API's own test calls out: grace 300,
		// step_timeout 600, no max_runtime — dead on arrival.
		{"below grace", types.Int64Value(3600), types.Int64Null(), types.Int64Value(300), false},
		{"equal to grace", types.Int64Value(300), types.Int64Null(), types.Int64Value(300), true},
		{"above grace", types.Int64Value(300), types.Int64Null(), types.Int64Value(600), true},

		// Not knowable from the configuration: defer to the API rather than
		// invent an error. grace_s omitted is the common one — it is
		// Optional+Computed, so the server supplies it.
		{"grace omitted", types.Int64Null(), types.Int64Null(), types.Int64Value(600), false},
		{"grace unknown", types.Int64Unknown(), types.Int64Null(), types.Int64Value(600), false},
		{"max_runtime unknown", types.Int64Value(300), types.Int64Unknown(), types.Int64Value(600), false},
		{"step unknown", types.Int64Value(300), types.Int64Null(), types.Int64Unknown(), false},
		{"step omitted", types.Int64Value(300), types.Int64Null(), types.Int64Null(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := base
			m.GraceS, m.MaxRuntimeS, m.StepTimeoutS = tc.grace, tc.maxRuntime, tc.step

			diags := monitorValidateConfig(t, m)
			require.Equal(t, tc.wantError, diags.HasError(), "%v", diags)
			if tc.wantError {
				require.Contains(t, diags.Errors()[0].Summary(), "step_timeout_s")
			}
		})
	}
}

// TestMonitorSlugRejectsUnnormalised: the server normalises slugs (trim +
// lowercase) before storing them, so a non-normalised slug must be rejected at
// plan time. A plan modifier cannot rewrite it — Terraform rejects a planned
// value that differs from a known config value.
func TestMonitorSlugRejectsUnnormalised(t *testing.T) {
	for _, tc := range []struct {
		name  string
		slug  string
		valid bool
	}{
		{"normalised", "acc-basic", true},
		{"digits", "job-42", true},
		{"untrimmed", "  Rev-Case-Slug  ", false},
		{"uppercase", "Rev-Case-Slug", false},
		{"leading hyphen", "-nope", false},
		{"trailing hyphen", "nope-", false},
		{"too short", "ab", false},
		{"underscore", "my_monitor", false},
		{"uuid shaped", "550e8400-e29b-41d4-a716-446655440000", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := validateString(t, "slug", tc.slug)
			if tc.valid {
				require.False(t, resp.Diagnostics.HasError(), "%q should be accepted: %v", tc.slug, resp.Diagnostics)
				return
			}
			require.True(t, resp.Diagnostics.HasError(), "%q should be rejected", tc.slug)
		})
	}
}

func TestMonitorMonitorFromRejectsNonRFC3339(t *testing.T) {
	require.True(t, validateString(t, "monitor_from", "2027-01-01").Diagnostics.HasError())
	require.False(t, validateString(t, "monitor_from", "2027-01-01T00:00:00+01:00").Diagnostics.HasError())
}

// TestMonitorAgentIDRequiresUUID pins the deliberate divergence from the API:
// resolveAgentID (api/agents_api.go) accepts either an agent's id or its slug,
// but the response always echoes back the canonical UUID, never the slug it
// was attached with. Because agent_id is plain Optional (not Computed), a
// slug written here would apply cleanly and then desync state from plan on
// every later refresh. The provider closes that gap by requiring the UUID
// form at plan time.
func TestMonitorAgentIDRequiresUUID(t *testing.T) {
	require.False(t, validateString(t, "agent_id", "3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f").Diagnostics.HasError())
	require.True(t, validateString(t, "agent_id", "nightly-etl-bot").Diagnostics.HasError(),
		"a slug must be rejected at plan time, even though the API itself would accept it")
	require.True(t, validateString(t, "agent_id", "").Diagnostics.HasError())
}

// TestMonitorOptionalOnlyAttributesAreNotComputed pins the "attributes can never
// be unset" regression: a purely user-supplied attribute marked Computed keeps
// its old value in state when it is removed from the configuration.
func TestMonitorOptionalOnlyAttributesAreNotComputed(t *testing.T) {
	s := monitorSchema(t)
	for _, name := range []string{
		"slug", "cron_expr", "tags", "runaway_ceiling", "monitor_from",
		"probe_url", "probe_interval_s", "probe_expected_body", "max_runtime_s",
		"step_timeout_s", "agent_id",
	} {
		attr, ok := s.Attributes[name]
		require.True(t, ok, "missing attribute %s", name)
		require.True(t, attr.IsOptional(), "%s must be optional", name)
		require.False(t, attr.IsComputed(),
			"%s is only ever supplied by the configuration, so Computed would make it impossible to unset", name)
	}

	// The converse: these are genuinely server-supplied and must stay Computed.
	for _, name := range []string{
		"grace_s", "monitor_type", "schedule_kind", "period_s", "tz", "failure_threshold",
	} {
		attr, ok := s.Attributes[name]
		require.True(t, ok, "missing attribute %s", name)
		require.True(t, attr.IsComputed(), "%s is server-derived and must stay computed", name)
	}
}

// TestMonitorFromValueKeepsEquivalentInstant: the API answers in UTC, so a
// configured offset timestamp must survive the round trip.
func TestMonitorFromValueKeepsEquivalentInstant(t *testing.T) {
	utc := "2026-12-31T23:00:00Z"

	got := monitorFromValue(&utc, types.StringValue("2027-01-01T00:00:00+01:00"))
	require.Equal(t, "2027-01-01T00:00:00+01:00", got.ValueString(),
		"same instant: keep the configured spelling")

	got = monitorFromValue(&utc, types.StringValue("2030-01-01T00:00:00Z"))
	require.Equal(t, utc, got.ValueString(), "different instant: take the server value")

	got = monitorFromValue(&utc, types.StringNull())
	require.Equal(t, utc, got.ValueString(), "no prior: take the server value")

	require.True(t, monitorFromValue(nil, types.StringNull()).IsNull())
}

// TestMonitorPatchNeeded: `paused` is applied through the pause/resume
// endpoints, so a plan that only pauses a monitor must not also PATCH it.
func TestMonitorPatchNeeded(t *testing.T) {
	ctx := context.Background()
	tags, diags := types.SetValueFrom(ctx, types.StringType, []string{"env:prod"})
	require.False(t, diags.HasError())

	base := monitorResourceModel{
		Name:         types.StringValue("acc"),
		Slug:         types.StringValue("acc"),
		MonitorType:  types.StringValue("heartbeat"),
		ScheduleKind: types.StringValue("simple"),
		PeriodS:      types.Int64Value(3600),
		GraceS:       types.Int64Value(300),
		TZ:           types.StringValue("UTC"),
		Tags:         tags,
		Paused:       types.BoolValue(false),
	}

	pausedOnly := base
	pausedOnly.Paused = types.BoolValue(true)
	// Response-only attributes move on their own between refreshes.
	pausedOnly.Status = types.StringValue("late")
	pausedOnly.DueAt = types.StringValue("2027-01-01T00:00:00Z")

	need, err := monitorPatchNeeded(ctx, pausedOnly, base)
	require.NoError(t, err)
	require.False(t, need, "pausing a monitor must not trigger a PATCH")

	renamed := base
	renamed.Name = types.StringValue("acc-renamed")
	need, err = monitorPatchNeeded(ctx, renamed, base)
	require.NoError(t, err)
	require.True(t, need, "a renamed monitor must be PATCHed")

	untagged := base
	untagged.Tags = types.SetNull(types.StringType)
	need, err = monitorPatchNeeded(ctx, untagged, base)
	require.NoError(t, err)
	require.True(t, need, "clearing tags must be PATCHed")
}

// TestMonitorPatchFromModel pins the exact PATCH document.
//
// PATCH /api/v1/checks/{id} is a JSON Merge Patch: an absent key preserves the
// stored value. "Which keys are present" is therefore the entire contract — and
// every bug this builder exists to fix was a key that was silently missing — so
// these assertions compare the whole document rather than probing one key at a
// time. A response-only attribute leaking into the body fails here too.
func TestMonitorPatchFromModel(t *testing.T) {
	ctx := context.Background()
	tags := types.SetValueMust(types.StringType, []attr.Value{types.StringValue("env:prod")})

	// A monitor as the server holds it, which after resolveUnknownsFromState is
	// also what `desired` looks like. The response-only attributes are populated
	// on purpose: none of them may appear in any expected document below.
	stored := monitorResourceModel{
		ID:                   types.StringValue("3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f"),
		Name:                 types.StringValue("acc"),
		Slug:                 types.StringValue("acc"),
		MonitorType:          types.StringValue("heartbeat"),
		ScheduleKind:         types.StringValue("simple"),
		PeriodS:              types.Int64Value(3600),
		TZ:                   types.StringValue("UTC"),
		GraceS:               types.Int64Value(1800),
		MaxRuntimeS:          types.Int64Value(14400),
		StepTimeoutS:         types.Int64Value(900),
		FailureThreshold:     types.Int64Value(3),
		Tags:                 tags,
		RunawayCeiling:       types.Int64Value(40),
		MonitorFrom:          types.StringValue("2027-01-01T00:00:00Z"),
		AgentID:              types.StringValue("11111111-2222-4333-8444-555555555555"),
		ProbeMethod:          types.StringValue("GET"),
		ProbeExpectedStatus:  types.Int64Value(200),
		ProbeTimeoutS:        types.Int64Value(10),
		ProbeFollowRedirects: types.BoolValue(true),
		Paused:               types.BoolValue(true),
		Status:               types.StringValue("up"),
		PingURL:              types.StringValue("https://ping.lastping.dev/p/abc"),
		CreatedAt:            types.StringValue("2026-01-01T00:00:00Z"),
	}

	// cron_expr, probe_expected_body and probe_follow_redirects are always
	// present even at their zero value: a configured "" must be able to delete
	// the cron expression or the body assertion, and a configured false must be
	// able to turn redirect-following off. probe_url and probe_interval_s are
	// absent because they are unset on a heartbeat monitor.
	fullyConfigured := client.MonitorPatch{
		"name":                   "acc",
		"slug":                   "acc",
		"monitor_type":           "heartbeat",
		"schedule_kind":          "simple",
		"period_s":               int64(3600),
		"tz":                     "UTC",
		"grace_s":                int64(1800),
		"max_runtime_s":          int64(14400),
		"step_timeout_s":         int64(900),
		"failure_threshold":      int64(3),
		"cron_expr":              "",
		"probe_method":           "GET",
		"probe_expected_status":  int64(200),
		"probe_timeout_s":        int64(10),
		"probe_expected_body":    "",
		"probe_follow_redirects": true,
		"tags":                   []string{"env:prod"},
		"runaway_ceiling":        int64(40),
		"monitor_from":           "2027-01-01T00:00:00Z",
		"agent_id":               "11111111-2222-4333-8444-555555555555",
	}

	t.Run("attribute present with a value", func(t *testing.T) {
		got, err := monitorPatchFromModel(ctx, stored, stored)
		require.NoError(t, err)
		require.Equal(t, fullyConfigured, got)
	})

	t.Run("attribute removed from config is an explicit null", func(t *testing.T) {
		// tags, runaway_ceiling, monitor_from, max_runtime_s, step_timeout_s and
		// agent_id are plain Optional, so deleting them from the HCL makes them
		// null in both the config and the plan.
		cfg := stored
		cfg.Tags = types.SetNull(types.StringType)
		cfg.RunawayCeiling = types.Int64Null()
		cfg.MonitorFrom = types.StringNull()
		cfg.MaxRuntimeS = types.Int64Null()
		cfg.StepTimeoutS = types.Int64Null()
		cfg.AgentID = types.StringNull()

		want := client.MonitorPatch{}
		for k, v := range fullyConfigured {
			want[k] = v
		}
		want["tags"] = nil
		want["runaway_ceiling"] = nil
		want["monitor_from"] = nil
		want["max_runtime_s"] = nil
		want["step_timeout_s"] = nil
		want["agent_id"] = nil

		got, err := monitorPatchFromModel(ctx, cfg, cfg)
		require.NoError(t, err)
		require.Equal(t, want, got)

		// nil has to reach the wire as JSON null, not as a dropped key: an
		// absent key is exactly what the old payload sent and exactly what
		// merge-patch reads as "leave it alone". For agent_id specifically,
		// that null is what api/checks_patch.go reads as "detach from the
		// agent" — the whole point of the attribute being clearable.
		body, err := json.Marshal(got)
		require.NoError(t, err)
		require.Contains(t, string(body), `"tags":null`)
		require.Contains(t, string(body), `"runaway_ceiling":null`)
		require.Contains(t, string(body), `"monitor_from":null`)
		require.Contains(t, string(body), `"max_runtime_s":null`)
		require.Contains(t, string(body), `"step_timeout_s":null`)
		require.Contains(t, string(body), `"agent_id":null`)
	})

	// failure_threshold is the counter-example to the block above: it is
	// Optional+Computed and NOT NULL DEFAULT 1 server-side, so it belongs in the
	// omit-when-zero group. Removing it from the configuration must leave the
	// stored value alone (an absent key), never send a null — the API would read
	// that as an omission anyway, and sending 0 would be outside its [1, 100]
	// range. Putting it in the clearable group by mistake is the bug this pins.
	t.Run("failure_threshold is omitted, never nulled", func(t *testing.T) {
		cfg := stored
		cfg.FailureThreshold = types.Int64Null()

		// Optional+Computed: the plan still carries the stored value.
		got, err := monitorPatchFromModel(ctx, stored, cfg)
		require.NoError(t, err)
		require.Equal(t, int64(3), got["failure_threshold"])

		// The shape resolveUnknownsFromState cannot produce, asserted anyway:
		// with no stored value either, the key must be absent rather than null.
		unset := stored
		unset.FailureThreshold = types.Int64Null()
		got, err = monitorPatchFromModel(ctx, unset, cfg)
		require.NoError(t, err)
		require.NotContains(t, got, "failure_threshold",
			"failure_threshold has no cleared state, so it must be omitted rather than sent as null")

		body, err := json.Marshal(got)
		require.NoError(t, err)
		require.NotContains(t, string(body), `"failure_threshold"`)
	})

	t.Run("optional+computed absent from config is still sent", func(t *testing.T) {
		// The server supplies these when the configuration omits them, so they
		// are null in the config while the plan (after resolveUnknownsFromState)
		// still holds the stored value. Omitting them would be correct only
		// against a merge-patch server; against the full-replace server this
		// provider also supports it would reset them. So: still sent, verbatim.
		cfg := stored
		cfg.MonitorType = types.StringNull()
		cfg.ScheduleKind = types.StringNull()
		cfg.PeriodS = types.Int64Null()
		cfg.TZ = types.StringNull()
		cfg.GraceS = types.Int64Null()
		cfg.FailureThreshold = types.Int64Null()
		cfg.ProbeMethod = types.StringNull()
		cfg.ProbeExpectedStatus = types.Int64Null()
		cfg.ProbeTimeoutS = types.Int64Null()
		cfg.ProbeFollowRedirects = types.BoolNull()

		got, err := monitorPatchFromModel(ctx, stored, cfg)
		require.NoError(t, err)
		require.Equal(t, fullyConfigured, got)
	})

	t.Run("null config clears even when the plan carries a value", func(t *testing.T) {
		// This shape cannot arise with the current schema: tags,
		// runaway_ceiling and monitor_from are Optional-only (pinned by
		// TestMonitorOptionalOnlyAttributesAreNotComputed), so a null config
		// always means a null plan — which is why the subtests above pass the
		// same model as both arguments.
		//
		// It is asserted anyway, because it is the only case the cfg argument
		// exists for. Making one of the three Optional+Computed would produce
		// exactly this shape — config null, plan holding the stored value — on
		// every apply that does not mention the attribute, and this assertion
		// is what keeps the defensive branch from rotting into dead code
		// nobody dares delete.
		cfg := stored
		cfg.Tags = types.SetNull(types.StringType)
		cfg.RunawayCeiling = types.Int64Null()
		cfg.MonitorFrom = types.StringNull()
		cfg.MaxRuntimeS = types.Int64Null()
		cfg.StepTimeoutS = types.Int64Null()
		cfg.AgentID = types.StringNull()

		got, err := monitorPatchFromModel(ctx, stored, cfg)
		require.NoError(t, err)

		require.Contains(t, got, "tags")
		require.Nil(t, got["tags"], "a null config must clear, not echo the plan's value back")
		require.Contains(t, got, "runaway_ceiling")
		require.Nil(t, got["runaway_ceiling"])
		require.Contains(t, got, "monitor_from")
		require.Nil(t, got["monitor_from"])
		require.Contains(t, got, "max_runtime_s")
		require.Nil(t, got["max_runtime_s"])
		require.Contains(t, got, "step_timeout_s")
		require.Nil(t, got["step_timeout_s"])
		require.Contains(t, got, "agent_id")
		require.Nil(t, got["agent_id"])
	})

	t.Run("explicitly empty tags are sent as an empty array", func(t *testing.T) {
		empty := stored
		empty.Tags = types.SetValueMust(types.StringType, []attr.Value{})

		// ElementsAs yields a non-nil zero-length slice here, so this reaches
		// the wire as `[]` and not as `null`. Both clear the tags; the empty
		// array is the one that says what was configured.
		got, err := monitorPatchFromModel(ctx, empty, empty)
		require.NoError(t, err)
		require.Equal(t, []string{}, got["tags"])

		body, err := json.Marshal(got)
		require.NoError(t, err)
		require.Contains(t, string(body), `"tags":[]`)
	})
}

// TestTagsValueMapsEmptyToPriorShape: the API always returns an array, so an
// empty answer has to map back onto whichever empty form the config used.
func TestTagsValueMapsEmptyToPriorShape(t *testing.T) {
	ctx := context.Background()

	emptySet, diags := types.SetValueFrom(ctx, types.StringType, []string{})
	require.False(t, diags.HasError())

	got, diags := tagsValue(ctx, nil, types.SetNull(types.StringType))
	require.False(t, diags.HasError())
	require.True(t, got.IsNull(), "tags absent from config stay absent")

	got, diags = tagsValue(ctx, []string{}, emptySet)
	require.False(t, diags.HasError())
	require.False(t, got.IsNull(), "an explicitly empty tags list stays an empty list")
	require.Empty(t, got.Elements())

	got, diags = tagsValue(ctx, []string{"env:prod"}, types.SetNull(types.StringType))
	require.False(t, diags.HasError())
	require.Len(t, got.Elements(), 1)

	// Non-empty prior: the API says tags are gone (cleared out-of-band via the
	// dashboard, a direct API call, or MCP) but the prior state still holds
	// ["env:prod"]. Echoing prior back here would permanently mask the
	// removal — refresh must be able to see the drift, so this must come back
	// null, not the stale prior value.
	nonEmptyPrior, diags := types.SetValueFrom(ctx, types.StringType, []string{"env:prod"})
	require.False(t, diags.HasError())

	got, diags = tagsValue(ctx, nil, nonEmptyPrior)
	require.False(t, diags.HasError())
	require.True(t, got.IsNull(), "tags cleared out-of-band must not be masked by the stale prior value")
}

// TestResolveUnknownsFromState_CoversEveryAttribute is the guard against the
// bug coming back through a new attribute.
//
// terraform-plugin-framework marks every Optional+Computed attribute the
// configuration omits as UNKNOWN in the plan. `ValueInt64()` on an unknown is
// 0 and `ValueString()` is "", and `PATCH /api/v1/checks/{id}` replaces the
// whole object, so any attribute that is not carried forward from the prior
// state is silently reset — or, for grace_s, fails the apply outright with
// "grace 0s outside [60, 31536000]".
//
// This builds a plan in which EVERY attribute is unknown, so a field added to
// monitorResourceModel and forgotten by the resolver fails here rather than in
// somebody's production apply.
func TestResolveUnknownsFromState_CoversEveryAttribute(t *testing.T) {
	state := monitorResourceModel{
		ID:                   types.StringValue("3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f"),
		Name:                 types.StringValue("nightly-backup"),
		Slug:                 types.StringValue("nightly-backup"),
		MonitorType:          types.StringValue("heartbeat"),
		ScheduleKind:         types.StringValue("cron"),
		PeriodS:              types.Int64Value(3600),
		CronExpr:             types.StringValue("0 3 * * *"),
		TZ:                   types.StringValue("Europe/Berlin"),
		GraceS:               types.Int64Value(1800),
		MaxRuntimeS:          types.Int64Value(14400),
		StepTimeoutS:         types.Int64Value(900),
		FailureThreshold:     types.Int64Value(3),
		Tags:                 types.SetValueMust(types.StringType, []attr.Value{types.StringValue("prod")}),
		RunawayCeiling:       types.Int64Value(40),
		MonitorFrom:          types.StringValue("2027-01-01T00:00:00Z"),
		AgentID:              types.StringValue("11111111-2222-4333-8444-555555555555"),
		ProbeURL:             types.StringValue("https://example.com/health"),
		ProbeMethod:          types.StringValue("HEAD"),
		ProbeIntervalS:       types.Int64Value(120),
		ProbeExpectedBody:    types.StringValue("ok"),
		ProbeExpectedStatus:  types.Int64Value(204),
		ProbeTimeoutS:        types.Int64Value(25),
		ProbeFollowRedirects: types.BoolValue(true),
		Paused:               types.BoolValue(true),
		PingURL:              types.StringValue("https://ping.lastping.dev/p/abc"),
		Status:               types.StringValue("up"),
		CreatedAt:            types.StringValue("2026-01-01T00:00:00Z"),
		LastPingAt:           types.StringValue("2026-01-02T00:00:00Z"),
		DueAt:                types.StringValue("2026-01-03T00:00:00Z"),
		AlertAfter:           types.StringValue("2026-01-04T00:00:00Z"),
		MaintenanceUntil:     types.StringValue("2026-01-05T00:00:00Z"),
	}

	// Every field unknown, and every field of `state` set to a distinctive
	// value, so "carried forward" cannot be confused with "happened to match".
	var allUnknown monitorResourceModel
	planV := reflect.ValueOf(&allUnknown).Elem()
	stateV := reflect.ValueOf(state)
	for i := range planV.NumField() {
		require.False(t, stateV.Field(i).Interface().(attr.Value).IsNull(),
			"state field %s is unset, so this test cannot tell a carried-forward value from a zero",
			planV.Type().Field(i).Name)
		switch planV.Field(i).Interface().(type) {
		case types.String:
			planV.Field(i).Set(reflect.ValueOf(types.StringUnknown()))
		case types.Int64:
			planV.Field(i).Set(reflect.ValueOf(types.Int64Unknown()))
		case types.Bool:
			planV.Field(i).Set(reflect.ValueOf(types.BoolUnknown()))
		case types.Set:
			planV.Field(i).Set(reflect.ValueOf(types.SetUnknown(types.StringType)))
		default:
			t.Fatalf("field %s has an unhandled type %T; extend this test",
				planV.Type().Field(i).Name, planV.Field(i).Interface())
		}
	}

	require.Equal(t, state, resolveUnknownsFromState(allUnknown, state))
}

// TestResolveUnknownsFromState_LeavesKnownAndNullAlone: only unknowns are
// resolved.
//
// A null is a real instruction. A plain Optional attribute removed from the
// configuration plans as null and has to reach the API as unset — carrying
// nulls forward as well would pin every removed attribute in place forever, so
// `cron_expr` could never be deleted and `tags` could never be cleared.
func TestResolveUnknownsFromState_LeavesKnownAndNullAlone(t *testing.T) {
	state := monitorResourceModel{
		Name:         types.StringValue("old-name"),
		CronExpr:     types.StringValue("0 3 * * *"),
		ScheduleKind: types.StringValue("cron"),
		GraceS:       types.Int64Value(1800),
		PeriodS:      types.Int64Value(3600),
	}
	plan := monitorResourceModel{
		Name:         types.StringValue("new-name"), // configured: keep it
		CronExpr:     types.StringNull(),            // removed: must stay unset
		ScheduleKind: types.StringValue("simple"),   // configured: keep it
		GraceS:       types.Int64Unknown(),          // omitted: carry forward
		PeriodS:      types.Int64Value(7200),        // configured: keep it
	}

	got := resolveUnknownsFromState(plan, state)

	require.Equal(t, types.StringValue("new-name"), got.Name)
	require.True(t, got.CronExpr.IsNull(), "a removed attribute must not be resurrected from state")
	require.Equal(t, types.StringValue("simple"), got.ScheduleKind)
	require.Equal(t, types.Int64Value(1800), got.GraceS)
	require.Equal(t, types.Int64Value(7200), got.PeriodS)
}
