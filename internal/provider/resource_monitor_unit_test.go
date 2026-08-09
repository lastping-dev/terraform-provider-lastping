package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"

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

// normaliseMonitorSets gives every set attribute an element type.
//
// A zero-value types.Set carries none, which ObjectValueFrom rejects with an
// opaque "MISSING TYPE" conversion error. Terraform itself can never hand the
// provider one — every set attribute arrives typed, null or not — so this is
// purely a test-fixture convenience: a fixture that says nothing about tags or
// a block set means "none configured". Normalising here rather than in every
// fixture keeps a new set attribute from breaking a dozen unrelated tests.
func normaliseMonitorSets(ctx context.Context, m monitorResourceModel) monitorResourceModel {
	if m.Tags.IsNull() && m.Tags.ElementType(ctx) == nil {
		m.Tags = types.SetNull(types.StringType)
	}
	if m.Assertions.IsNull() && m.Assertions.ElementType(ctx) == nil {
		m.Assertions = monitorAssertionSetNull()
	}
	if m.Guards.IsNull() && m.Guards.ElementType(ctx) == nil {
		m.Guards = monitorGuardSetNull()
	}
	return m
}

// monitorValidateConfig runs the resource's ValidateConfig against a model, so
// the plan-time refusals can be asserted without a live backend.
func monitorValidateConfig(t *testing.T, m monitorResourceModel) diag.Diagnostics {
	t.Helper()
	ctx := context.Background()
	s := monitorSchema(t)

	m = normaliseMonitorSets(ctx, m)

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
		{"expect_every_s", 60, true},
		{"expect_every_s", 31536000, true},
		{"expect_every_s", 59, false},
		{"expect_every_s", 31536001, false},
		// blocked_timeout_s has no server-side bounds at all, so the only
		// values refused here are the ones core/check would read as "unset"
		// and silently replace with the 24-hour default. There is no upper
		// bound to assert: the API declares none, and inventing one would
		// refuse a configuration the API accepts.
		{"blocked_timeout_s", 1, true},
		{"blocked_timeout_s", 86400, true},
		{"blocked_timeout_s", 31536000, true},
		{"blocked_timeout_s", 0, false},
		{"blocked_timeout_s", -1, false},
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
		Tags:       types.SetNull(types.StringType),
		Assertions: monitorAssertionSetNull(),
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
		Tags:       types.SetNull(types.StringType),
		Assertions: monitorAssertionSetNull(),
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
		Assertions:   monitorAssertionSetNull(),
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

// monitorRawValue renders a model as the raw Terraform value tfsdk.State and
// tfsdk.Plan carry, which the RequiresReplace modifier inspects to tell a
// create (null state) and a destroy (null plan) from a real change.
func monitorRawValue(t *testing.T, m monitorResourceModel) tftypes.Value {
	t.Helper()
	ctx := context.Background()
	m = normaliseMonitorSets(ctx, m)

	objType, ok := monitorSchema(t).Type().(types.ObjectType)
	require.True(t, ok, "a resource schema is always an object")

	obj, diags := types.ObjectValueFrom(ctx, objType.AttributeTypes(), m)
	require.False(t, diags.HasError(), "%v", diags)

	raw, err := obj.ToTerraformValue(ctx)
	require.NoError(t, err)
	return raw
}

// monitorStringRequiresReplace runs a string attribute's plan modifiers over an
// in-place change and reports whether they demanded a replacement.
func monitorStringRequiresReplace(t *testing.T, attrName, from, to string) bool {
	t.Helper()
	ctx := context.Background()

	attr, ok := monitorSchema(t).Attributes[attrName].(schema.StringAttribute)
	require.True(t, ok, "%s must be a string attribute", attrName)

	base := monitorResourceModel{Name: types.StringValue("acc")}
	stateModel, planModel := base, base
	stateVal, planVal := types.StringValue(from), types.StringValue(to)

	resp := &planmodifier.StringResponse{PlanValue: planVal}
	for _, mod := range attr.PlanModifiers {
		mod.PlanModifyString(ctx, planmodifier.StringRequest{
			Path:        path.Root(attrName),
			State:       tfsdk.State{Schema: monitorSchema(t), Raw: monitorRawValue(t, stateModel)},
			Plan:        tfsdk.Plan{Schema: monitorSchema(t), Raw: monitorRawValue(t, planModel)},
			StateValue:  stateVal,
			PlanValue:   planVal,
			ConfigValue: planVal,
		}, resp)
	}
	return resp.RequiresReplace
}

// TestMonitorCIProviderRequiresReplace pins the decision that a changed
// ci_provider is a replacement and not an in-place update.
//
// It is not a style choice. `PATCH /api/v1/checks/{id}` does not decode
// ci_provider at all — api/checks_patch.go's checkPatchRequest has no member
// for it, and the spec lists it beside slug, monitor_type and ci_secret as
// immutable and ignored if present. Without RequiresReplace, changing
// `github` to `gitlab` would produce a plan reading as a clean one-attribute
// update, an apply that returned 200 having changed nothing, and the same diff
// on every plan thereafter — a configuration that can never converge, with no
// error anywhere to say why.
//
// The comparison case is monitor_type, which the provider already treats this
// way for exactly the same reason; agent_id is the counter-example, an
// attribute the API really does update in place.
func TestMonitorCIProviderRequiresReplace(t *testing.T) {
	require.True(t, monitorStringRequiresReplace(t, "ci_provider", "github", "gitlab"),
		"ci_provider is create-only on the API, so changing it must replace the monitor")

	require.True(t, monitorStringRequiresReplace(t, "monitor_type", "heartbeat", "ci"),
		"monitor_type is the existing precedent this follows")

	require.False(t, monitorStringRequiresReplace(t, "agent_id",
		"11111111-2222-4333-8444-555555555555", "99999999-2222-4333-8444-555555555555"),
		"agent_id is genuinely PATCH-able, so it must NOT force a replacement")
}

// TestMonitorCIProviderRejectsUnknownProvider mirrors the API's
// knownCIProviders set (api/checks.go), which answers 400 for anything else.
func TestMonitorCIProviderRejectsUnknownProvider(t *testing.T) {
	for _, ok := range []string{"github", "gitlab", "jenkins"} {
		require.False(t, validateString(t, "ci_provider", ok).Diagnostics.HasError(), "%s must be accepted", ok)
	}
	for _, bad := range []string{"circleci", "GitHub", ""} {
		require.True(t, validateString(t, "ci_provider", bad).Diagnostics.HasError(), "%q must be refused", bad)
	}
}

// TestMonitorCIFiltersRequireProvider: the API accepts ci_workflow/ci_branch on
// a monitor with no CI binding and silently discards them, and because no
// response reports either attribute back, the provider would write the
// configured value into state unchallenged. The result is a configuration that
// applies cleanly, plans clean forever, and does not do what it says.
func TestMonitorCIFiltersRequireProvider(t *testing.T) {
	t.Run("filters without a provider are refused", func(t *testing.T) {
		diags := monitorValidateConfig(t, monitorResourceModel{
			Name:       types.StringValue("acc"),
			CiWorkflow: types.StringValue("ci.yml"),
		})
		require.True(t, diags.HasError(), "ci_workflow without ci_provider must be a plan-time error")
		require.Contains(t, diags.Errors()[0].Summary(), "ci_workflow")

		diags = monitorValidateConfig(t, monitorResourceModel{
			Name:     types.StringValue("acc"),
			CiBranch: types.StringValue("main"),
		})
		require.True(t, diags.HasError(), "ci_branch without ci_provider must be a plan-time error")
		require.Contains(t, diags.Errors()[0].Summary(), "ci_branch")
	})

	t.Run("filters with a provider are fine", func(t *testing.T) {
		diags := monitorValidateConfig(t, monitorResourceModel{
			Name:       types.StringValue("acc"),
			CiProvider: types.StringValue("github"),
			CiWorkflow: types.StringValue("ci.yml"),
			CiBranch:   types.StringValue("main"),
		})
		require.False(t, diags.HasError(), "%v", diags)
	})

	t.Run("a bare provider is fine", func(t *testing.T) {
		diags := monitorValidateConfig(t, monitorResourceModel{
			Name:       types.StringValue("acc"),
			CiProvider: types.StringValue("jenkins"),
		})
		require.False(t, diags.HasError(), "%v", diags)
	})
}

// TestMonitorCIFiltersSurviveRefresh pins the read half of the write-only
// problem.
//
// ci_workflow and ci_branch are accepted by the API and never reported back:
// api/checks.go's checkResponse has no field for either, so every GET decodes
// them as "". Mapping that "" through stringOrNull — the obvious thing, and
// what every other user-supplied string here does — would null both attributes
// on the first refresh after an apply, leaving a permanent diff against any
// configuration that sets them. The prior value has to be carried forward
// instead.
func TestMonitorCIFiltersSurviveRefresh(t *testing.T) {
	ctx := context.Background()

	// What the API actually returns for a CI monitor: the provider, never the
	// filters.
	fromAPI := &client.Monitor{
		ID: "3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f", Name: "acc",
		MonitorType: "ci", ScheduleKind: "simple", PeriodS: 3600, TZ: "UTC", GraceS: 1800,
		CiProvider: "github",
	}

	prior := monitorResourceModel{
		Tags:       types.SetNull(types.StringType),
		CiWorkflow: types.StringValue("ci.yml"),
		CiBranch:   types.StringValue("main"),
	}

	got, err := modelFromMonitor(ctx, fromAPI, prior)
	require.NoError(t, err)
	require.Equal(t, types.StringValue("github"), got.CiProvider,
		"ci_provider IS reported by the API and must refresh from it")
	require.Equal(t, types.StringValue("ci.yml"), got.CiWorkflow,
		"the API never reports ci_workflow, so refresh must keep what Terraform last wrote")
	require.Equal(t, types.StringValue("main"), got.CiBranch)

	// Import: no prior state at all. Both filters read null however the monitor
	// is really configured — the documented cost of a write-only attribute, and
	// the reason this is asserted rather than left to be discovered.
	imported, err := modelFromMonitor(ctx, fromAPI, monitorResourceModel{Tags: types.SetNull(types.StringType)})
	require.NoError(t, err)
	require.True(t, imported.CiWorkflow.IsNull(), "an import cannot recover a value the API never sends")
	require.True(t, imported.CiBranch.IsNull())
	require.Equal(t, types.StringValue("github"), imported.CiProvider,
		"ci_provider, by contrast, DOES survive an import — that is the part of the bug this fixes")

	// The seam that makes this correct the day the API starts reporting the
	// filters: a real value always beats the carried-forward one.
	answered := *fromAPI
	answered.CiWorkflow = "release.yml"
	got, err = modelFromMonitor(ctx, &answered, prior)
	require.NoError(t, err)
	require.Equal(t, types.StringValue("release.yml"), got.CiWorkflow,
		"an API that answers must win over the prior state")
}

// TestMonitorBlockedTimeoutReadsAbsentAsNull: the API omits blocked_timeout_s
// when it is unset, and "unset" means the 24-hour default applies — not that
// the monitor waits forever, and not 0. Null is the only reading that
// round-trips an omitted attribute.
func TestMonitorBlockedTimeoutReadsAbsentAsNull(t *testing.T) {
	ctx := context.Background()
	base := client.Monitor{
		ID: "3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f", Name: "acc",
		MonitorType: "heartbeat", ScheduleKind: "simple", PeriodS: 3600, TZ: "UTC", GraceS: 1800,
	}
	prior := monitorResourceModel{Tags: types.SetNull(types.StringType)}

	got, err := modelFromMonitor(ctx, &base, prior)
	require.NoError(t, err)
	require.True(t, got.BlockedTimeoutS.IsNull(), "an omitted blocked_timeout_s must read as null, never 0")

	set := base
	set.BlockedTimeoutS = ptrTo(int64(7200))
	got, err = modelFromMonitor(ctx, &set, prior)
	require.NoError(t, err)
	require.Equal(t, types.Int64Value(7200), got.BlockedTimeoutS)

	// Both monitor surfaces have to agree, for the same reason
	// TestMonitorSurfacesAgreeOnEmptyValues exists for the others.
	data, diags := monitorDataFromAPI(ctx, &base)
	require.False(t, diags.HasError(), "%v", diags)
	require.Equal(t, got.BlockedTimeoutS, types.Int64Value(7200))
	require.True(t, data.BlockedTimeoutS.IsNull())
	require.Equal(t, types.StringNull(), data.CiProvider, "no CI binding reads as null on the data source too")
}

// TestMonitorOptionalOnlyAttributesAreNotComputed pins the "attributes can never
// be unset" regression: a purely user-supplied attribute marked Computed keeps
// its old value in state when it is removed from the configuration.
func TestMonitorOptionalOnlyAttributesAreNotComputed(t *testing.T) {
	s := monitorSchema(t)
	for _, name := range []string{
		"slug", "cron_expr", "tags", "runaway_ceiling", "monitor_from",
		"probe_url", "probe_interval_s", "probe_expected_body", "max_runtime_s",
		"step_timeout_s", "expect_every_s", "blocked_timeout_s", "agent_id",
		// ci_workflow and ci_branch are the ones most likely to be "fixed" into
		// Computed one day: no API response carries them, so the provider
		// carries prior state forward, and Computed looks like the tidy way to
		// express that. It is not — it would pin the last written filter and
		// make clearing one impossible.
		"ci_provider", "ci_workflow", "ci_branch",
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
		ExpectEveryS:         types.Int64Value(1800),
		BlockedTimeoutS:      types.Int64Value(7200),
		FailureThreshold:     types.Int64Value(3),
		Tags:                 tags,
		RunawayCeiling:       types.Int64Value(40),
		MonitorFrom:          types.StringValue("2027-01-01T00:00:00Z"),
		AgentID:              types.StringValue("11111111-2222-4333-8444-555555555555"),
		CiProvider:           types.StringValue("github"),
		CiWorkflow:           types.StringValue("ci.yml"),
		CiBranch:             types.StringValue("main"),
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
		"expect_every_s":         int64(1800),
		"blocked_timeout_s":      int64(7200),
		"failure_threshold":      int64(3),
		"ci_provider":            "github",
		"ci_workflow":            "ci.yml",
		"ci_branch":              "main",
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
		cfg.ExpectEveryS = types.Int64Null()
		cfg.BlockedTimeoutS = types.Int64Null()
		cfg.AgentID = types.StringNull()
		cfg.CiWorkflow = types.StringNull()
		cfg.CiBranch = types.StringNull()

		want := client.MonitorPatch{}
		for k, v := range fullyConfigured {
			want[k] = v
		}
		want["tags"] = nil
		want["runaway_ceiling"] = nil
		want["monitor_from"] = nil
		want["max_runtime_s"] = nil
		want["step_timeout_s"] = nil
		want["expect_every_s"] = nil
		want["blocked_timeout_s"] = nil
		want["agent_id"] = nil
		// ci_workflow and ci_branch clear to null and NEVER to "": the API
		// reads "" on these two as "leave the stored filter alone", so the
		// empty-string form would be the exact opposite of deleting the
		// attribute. ci_provider is untouched here — it is immutable on the
		// API and RequiresReplace in the schema, so it has no cleared form to
		// reach through a PATCH at all.
		want["ci_workflow"] = nil
		want["ci_branch"] = nil

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
		require.Contains(t, string(body), `"blocked_timeout_s":null`)
		require.Contains(t, string(body), `"agent_id":null`)
		require.Contains(t, string(body), `"ci_workflow":null`)
		require.Contains(t, string(body), `"ci_branch":null`)
		require.NotContains(t, string(body), `"ci_workflow":""`,
			`"" preserves the stored filter server-side, so it must never stand in for a clear`)
		require.NotContains(t, string(body), `"ci_branch":""`)
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
		cfg.ExpectEveryS = types.Int64Null()
		cfg.BlockedTimeoutS = types.Int64Null()
		cfg.AgentID = types.StringNull()
		cfg.CiWorkflow = types.StringNull()
		cfg.CiBranch = types.StringNull()

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
		require.Contains(t, got, "expect_every_s")
		require.Nil(t, got["expect_every_s"])
		require.Contains(t, got, "blocked_timeout_s")
		require.Nil(t, got["blocked_timeout_s"])
		require.Contains(t, got, "agent_id")
		require.Nil(t, got["agent_id"])
		require.Contains(t, got, "ci_workflow")
		require.Nil(t, got["ci_workflow"])
		require.Contains(t, got, "ci_branch")
		require.Nil(t, got["ci_branch"])
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
		ExpectEveryS:         types.Int64Value(1800),
		BlockedTimeoutS:      types.Int64Value(7200),
		FailureThreshold:     types.Int64Value(3),
		Tags:                 types.SetValueMust(types.StringType, []attr.Value{types.StringValue("prod")}),
		RunawayCeiling:       types.Int64Value(40),
		MonitorFrom:          types.StringValue("2027-01-01T00:00:00Z"),
		AgentID:              types.StringValue("11111111-2222-4333-8444-555555555555"),
		CiProvider:           types.StringValue("gitlab"),
		CiWorkflow:           types.StringValue("nightly"),
		CiBranch:             types.StringValue("release"),
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
		Assertions: types.SetValueMust(assertionObjectType(), []attr.Value{
			types.ObjectValueMust(assertionObjectType().AttrTypes, map[string]attr.Value{
				"name":  types.StringValue("rows written"),
				"kind":  types.StringValue("json_path"),
				"value": types.StringValue("0"),
				"path":  types.StringValue("result.rows_processed"),
				"op":    types.StringValue("gt"),
			}),
		}),
		Guards: types.SetValueMust(guardObjectType(), []attr.Value{
			types.ObjectValueMust(guardObjectType().AttrTypes, map[string]attr.Value{
				"name":        types.StringValue("daily spend"),
				"path":        types.StringValue("cost.usd"),
				"window_s":    types.Int64Value(86400),
				"ceiling":     types.Float64Value(50),
				"aggregation": types.StringValue("sum"),
			}),
		}),
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

// TestMonitorOnDemandSchedule pins the two halves of on_demand support that
// were missing together: the schedule_kind validator refused the value while
// the shipped docs already showed it, so anyone copying the documented
// expect_every_s example was rejected at plan time by their own provider.
//
// The cadence half mirrors the API's ON_DEMAND_SCHEDULE_CONFLICT (400): an
// on_demand monitor is driven entirely by run events, so a period or cron
// expression would be persisted on a monitor that never reads either.
func TestMonitorOnDemandSchedule(t *testing.T) {
	t.Run("on_demand is an accepted schedule_kind", func(t *testing.T) {
		// Drive the schema's own validators, the path a plan takes, rather than
		// asserting on the OneOf list, which would restate the fix instead of
		// exercising it.
		sch := monitorSchema(t)
		attr, ok := sch.Attributes["schedule_kind"]
		require.True(t, ok, "schedule_kind attribute missing")
		strAttr, ok := attr.(schema.StringAttribute)
		require.True(t, ok, "schedule_kind is not a string attribute")

		for _, kind := range []string{"simple", "cron", "on_demand"} {
			var diags diag.Diagnostics
			for _, v := range strAttr.Validators {
				resp := &validator.StringResponse{}
				v.ValidateString(context.Background(), validator.StringRequest{
					Path:        path.Root("schedule_kind"),
					ConfigValue: types.StringValue(kind),
				}, resp)
				diags.Append(resp.Diagnostics...)
			}
			require.False(t, diags.HasError(), "schedule_kind %q must be accepted: %v", kind, diags)
		}
	})

	t.Run("a cadence on on_demand is refused at plan time", func(t *testing.T) {
		diags := monitorValidateConfig(t, monitorResourceModel{
			Name:         types.StringValue("acc"),
			ScheduleKind: types.StringValue("on_demand"),
			PeriodS:      types.Int64Value(3600),
		})
		require.True(t, diags.HasError(), "period_s on on_demand must be a plan-time error")
		require.Contains(t, diags.Errors()[0].Summary(), "period_s")

		diags = monitorValidateConfig(t, monitorResourceModel{
			Name:         types.StringValue("acc"),
			ScheduleKind: types.StringValue("on_demand"),
			CronExpr:     types.StringValue("0 3 * * *"),
		})
		require.True(t, diags.HasError(), "cron_expr on on_demand must be a plan-time error")
		require.Contains(t, diags.Errors()[0].Summary(), "cron_expr")
	})

	t.Run("on_demand without a cadence is fine", func(t *testing.T) {
		diags := monitorValidateConfig(t, monitorResourceModel{
			Name:         types.StringValue("acc"),
			ScheduleKind: types.StringValue("on_demand"),
			ExpectEveryS: types.Int64Value(900),
		})
		require.False(t, diags.HasError(), "%v", diags)
	})

	t.Run("a cadence on simple stays legal", func(t *testing.T) {
		diags := monitorValidateConfig(t, monitorResourceModel{
			Name:         types.StringValue("acc"),
			ScheduleKind: types.StringValue("simple"),
			PeriodS:      types.Int64Value(3600),
		})
		require.False(t, diags.HasError(), "%v", diags)
	})
}
