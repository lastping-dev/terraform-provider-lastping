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
		{"notify_min_run_s", 60, true},
		{"notify_min_run_s", 31536000, true},
		{"notify_min_run_s", 59, false},
		{"notify_min_run_s", 31536001, false},
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

// TestMonitorNotifyMinRunRejectedOnHTTP: the API answers 400
// NOTIFY_MIN_RUN_NOT_SUPPORTED for notify_min_run_s on an http monitor, for the
// same precondition as max_runtime_s: the floor only evaluates once a run's
// duration is known, and that duration comes exclusively from a matched
// /start + success pair, which a probe never sends.
func TestMonitorNotifyMinRunRejectedOnHTTP(t *testing.T) {
	base := monitorResourceModel{
		Name:        types.StringValue("acc"),
		MonitorType: types.StringValue("http"),
		ProbeURL:    types.StringValue("https://example.com/"),
		// The zero types.Set has no element type, which ObjectValueFrom
		// rejects; every other zero value is a well-formed null.
		Tags:       types.SetNull(types.StringType),
		Assertions: monitorAssertionSetNull(),
	}

	withFloor := base
	withFloor.NotifyMinRunS = types.Int64Value(300)
	diags := monitorValidateConfig(t, withFloor)
	require.True(t, diags.HasError(), "notify_min_run_s on an http monitor must be refused at plan time")
	require.Contains(t, diags.Errors()[0].Summary(), "notify_min_run_s")

	// Omitted on an http monitor: fine. The PATCH still carries an explicit
	// null, which the API accepts there as a no-op.
	require.False(t, monitorValidateConfig(t, base).HasError())

	// And it is only http that is refused.
	heartbeat := withFloor
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

	require.True(t, monitorStringRequiresReplace(t, "monitor_type", "heartbeat", "http"),
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

	// What the API actually returns for a CI monitor: a heartbeat monitor with
	// ci_provider set, never the filters. "ci" is not a value the API ever
	// writes to monitor_type.
	fromAPI := &client.Monitor{
		ID: "3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f", Name: "acc",
		MonitorType: "heartbeat", ScheduleKind: "simple", PeriodS: 3600, TZ: "UTC", GraceS: 1800,
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

// TestMonitorCiWebhookURLRefreshesNormally pins the OTHER half of the CI
// fields: unlike ci_workflow/ci_branch/ci_secret, ci_webhook_url is an
// ordinary readable response field (api/checks.go's rowToDTO populates it
// from row.CiProvider on every GET, the same as ci_provider itself), so it
// must NOT go through writeOnlyString's carry-forward — a stale prior value
// must not survive a response that now says something different, and an
// absent binding must read as null even with a stale prior in state.
func TestMonitorCiWebhookURLRefreshesNormally(t *testing.T) {
	ctx := context.Background()
	prior := monitorResourceModel{
		Tags:         types.SetNull(types.StringType),
		CiWebhookURL: types.StringValue("https://ingest.lastping.dev/ci/github/OLD-ID"),
	}

	fromAPI := &client.Monitor{
		ID: "3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f", Name: "acc",
		MonitorType: "heartbeat", ScheduleKind: "simple", PeriodS: 3600, TZ: "UTC", GraceS: 1800,
		CiProvider:   "github",
		CiWebhookURL: "https://ingest.lastping.dev/ci/github/3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f",
	}
	got, err := modelFromMonitor(ctx, fromAPI, prior)
	require.NoError(t, err)
	require.Equal(t, types.StringValue("https://ingest.lastping.dev/ci/github/3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f"),
		got.CiWebhookURL, "the API's answer must win over a stale prior value")

	unbound := *fromAPI
	unbound.CiProvider, unbound.CiWebhookURL = "", ""
	got, err = modelFromMonitor(ctx, &unbound, prior)
	require.NoError(t, err)
	require.True(t, got.CiWebhookURL.IsNull(),
		"no CI binding must read as null even with a stale prior value in state")
}

// TestMonitorCiSecretSurvivesRefresh is the security-critical invariant for
// ci_secret, on identical terms to TestAPIKeyModelPreservesPlaintext for
// apiKeyResourceModel.Key: the API returns the plaintext secret exactly once,
// in the create response, and never again — api/checks.go's rowToDTO comment
// says so explicitly. A refresh (or an update, which also calls
// modelFromMonitor) must carry the value already in state forward rather than
// nulling it, or the one copy that exists anywhere is destroyed.
//
// This is the same shape TestMonitorCIFiltersSurviveRefresh already pins for
// ci_workflow/ci_branch, but the failure mode ci_secret adds is sharper: for
// ci_workflow, losing the value just breaks a filter that can be reconfigured.
// For ci_secret there is no reconfiguring it — see the schema description's
// import caveat.
func TestMonitorCiSecretSurvivesRefresh(t *testing.T) {
	ctx := context.Background()

	t.Run("create takes the secret from the response", func(t *testing.T) {
		fromAPI := &client.Monitor{
			ID: "3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f", Name: "acc",
			MonitorType: "heartbeat", ScheduleKind: "simple", PeriodS: 3600, TZ: "UTC", GraceS: 1800,
			CiProvider: "github", CiSecret: "a3f8c2d1e4b7a9f0c3d2e1b4a7f8c0d3",
		}
		// plan.CiSecret is Unknown here, exactly as terraform-plugin-framework
		// plans an unconfigured Computed attribute on create — this is the case
		// that must NOT leak an unknown value into state when the API omits it
		// (covered below); here the API answers, so that path is not exercised.
		plan := monitorResourceModel{Tags: types.SetNull(types.StringType), CiSecret: types.StringUnknown()}
		got, err := modelFromMonitor(ctx, fromAPI, plan)
		require.NoError(t, err)
		require.Equal(t, types.StringValue("a3f8c2d1e4b7a9f0c3d2e1b4a7f8c0d3"), got.CiSecret)
	})

	t.Run("create of a non-CI monitor reads null, not unknown", func(t *testing.T) {
		// The common case: no ci_provider, so the API never sets CiSecret and
		// plan.CiSecret is Unknown (Computed, nothing configured it). Writing
		// that unknown straight into state would be a hard "invalid result
		// object after apply" error — this is exactly the edge case
		// writeOnlyString's unknown-prior handling exists for.
		fromAPI := &client.Monitor{
			ID: "3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f", Name: "acc",
			MonitorType: "heartbeat", ScheduleKind: "simple", PeriodS: 3600, TZ: "UTC", GraceS: 1800,
		}
		plan := monitorResourceModel{Tags: types.SetNull(types.StringType), CiSecret: types.StringUnknown()}
		got, err := modelFromMonitor(ctx, fromAPI, plan)
		require.NoError(t, err)
		require.True(t, got.CiSecret.IsNull(), "must be null, not unknown")
	})

	t.Run("refresh keeps the prior secret", func(t *testing.T) {
		fromAPI := &client.Monitor{
			ID: "3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f", Name: "acc",
			MonitorType: "heartbeat", ScheduleKind: "simple", PeriodS: 3600, TZ: "UTC", GraceS: 1800,
			CiProvider: "github", // GET reports the binding, but never the secret.
		}
		prior := monitorResourceModel{
			Tags: types.SetNull(types.StringType), CiSecret: types.StringValue("a3f8c2d1e4b7a9f0c3d2e1b4a7f8c0d3"),
		}
		got, err := modelFromMonitor(ctx, fromAPI, prior)
		require.NoError(t, err)
		require.Equal(t, types.StringValue("a3f8c2d1e4b7a9f0c3d2e1b4a7f8c0d3"), got.CiSecret,
			"a GET response carries no secret and must not blank the stored one")
	})

	t.Run("import cannot recover the secret", func(t *testing.T) {
		fromAPI := &client.Monitor{
			ID: "3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f", Name: "acc",
			MonitorType: "heartbeat", ScheduleKind: "simple", PeriodS: 3600, TZ: "UTC", GraceS: 1800,
			CiProvider: "github",
		}
		imported, err := modelFromMonitor(ctx, fromAPI, monitorResourceModel{Tags: types.SetNull(types.StringType)})
		require.NoError(t, err)
		require.True(t, imported.CiSecret.IsNull(),
			"import performs a GET, which never carries ci_secret, and no later apply can fill it in")
	})
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
		"step_timeout_s", "expect_every_s", "blocked_timeout_s", "notify_min_run_s", "agent_id",
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

	// source_kind and source_ref are deliberately NOT in the list above, and
	// this is the one place that fact is visible next to the rule it breaks.
	// They are Optional+Computed, which is exactly the shape this test refuses
	// everywhere else — and for once "pins the previous value" is the correct
	// behaviour rather than the bug, because the value is a reconcile key and
	// the alternative silently destroys it. See TestMonitorSourceIsOptionalComputed,
	// which pins that from the other direction, and the source_kind schema
	// description for the reasoning and the cost.

	// The converse: these are genuinely server-supplied and must stay Computed.
	for _, name := range []string{
		"grace_s", "monitor_type", "schedule_kind", "period_s", "tz", "failure_threshold",
		"ci_webhook_url", "ci_secret", "next_probe_at",
	} {
		attr, ok := s.Attributes[name]
		require.True(t, ok, "missing attribute %s", name)
		require.True(t, attr.IsComputed(), "%s is server-derived and must stay computed", name)
	}
}

// TestMonitorCiSecretIsSensitive pins the CLI-rendering half of the ci_secret
// invariant, the same way TestProvider_Schema does for api_key. Sensitive does
// not affect storage — see writeOnlyString's comment and ci_secret's own
// MarkdownDescription — but it does keep the value out of the plan/apply
// output a practitioner sees on their terminal, and losing the flag would be a
// silent regression no other test here would catch.
func TestMonitorCiSecretIsSensitive(t *testing.T) {
	s := monitorSchema(t)
	attr, ok := s.Attributes["ci_secret"]
	require.True(t, ok, "missing attribute ci_secret")
	require.True(t, attr.IsSensitive(), "ci_secret must be marked sensitive")
	require.True(t, attr.IsComputed(), "ci_secret can only ever come from the server")
	require.False(t, attr.IsOptional(), "ci_secret cannot be configured — the API has no field for it")
}

// TestMonitorNextProbeAtIsHTTPOnly pins due_at's http-monitor counterpart: the
// API omits next_probe_at for every monitor_type other than http
// (api/checks.go's checkResponse has `omitempty` on it), which has to read as
// null rather than as an empty string.
func TestMonitorNextProbeAtIsHTTPOnly(t *testing.T) {
	ctx := context.Background()

	httpMon := &client.Monitor{
		ID: "3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f", Name: "acc",
		MonitorType: "http", ScheduleKind: "simple", PeriodS: 60, TZ: "UTC", GraceS: 120,
		ProbeURL: "https://example.com/healthz", ProbeMethod: "GET",
		NextProbeAt: ptrTo("2026-07-13T03:01:00Z"),
	}
	got, err := modelFromMonitor(ctx, httpMon, monitorResourceModel{Tags: types.SetNull(types.StringType)})
	require.NoError(t, err)
	require.Equal(t, types.StringValue("2026-07-13T03:01:00Z"), got.NextProbeAt)

	heartbeatMon := &client.Monitor{
		ID: "3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f", Name: "acc",
		MonitorType: "heartbeat", ScheduleKind: "simple", PeriodS: 3600, TZ: "UTC", GraceS: 1800,
	}
	got, err = modelFromMonitor(ctx, heartbeatMon, monitorResourceModel{Tags: types.SetNull(types.StringType)})
	require.NoError(t, err)
	require.True(t, got.NextProbeAt.IsNull(), "non-http monitor types have no probe schedule to report")
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
		NotifyMinRunS:        types.Int64Value(120),
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
		"notify_min_run_s":       int64(120),
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
		cfg.NotifyMinRunS = types.Int64Null()
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
		want["notify_min_run_s"] = nil
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
		require.Contains(t, string(body), `"notify_min_run_s":null`)
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
		cfg.NotifyMinRunS = types.Int64Null()
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
		require.Contains(t, got, "notify_min_run_s")
		require.Nil(t, got["notify_min_run_s"])
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
		NotifyMinRunS:        types.Int64Value(120),
		FailureThreshold:     types.Int64Value(3),
		Tags:                 types.SetValueMust(types.StringType, []attr.Value{types.StringValue("prod")}),
		RunawayCeiling:       types.Int64Value(40),
		MonitorFrom:          types.StringValue("2027-01-01T00:00:00Z"),
		AgentID:              types.StringValue("11111111-2222-4333-8444-555555555555"),
		CiProvider:           types.StringValue("gitlab"),
		CiWorkflow:           types.StringValue("nightly"),
		CiBranch:             types.StringValue("release"),
		CiWebhookURL:         types.StringValue("https://ingest.lastping.dev/ci/gitlab/3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f"),
		CiSecret:             types.StringValue("a3f8c2d1e4b7a9f0c3d2e1b4a7f8c0d3"),
		SourceKind:           types.StringValue("github-actions"),
		SourceRef:            types.StringValue(".github/workflows/nightly.yml#build"),
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
		NextProbeAt:          types.StringValue("2026-01-03T00:05:00Z"),
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

// ─────────────────────────────────────────────────────────────────────────────
// Discovery source identity (source_kind / source_ref)
//
// These are the reconcile key that makes a discovery scan re-runnable, and the
// tests below are organised around the three ways it can be silently destroyed
// rather than around the three functions that touch it. Each of the three was
// found by review on the API side and each has a matching hazard here:
//
//  1. sending "" (which the API reads as "leave it alone", never as a clear);
//  2. sending null because the CONFIGURATION does not mention the pair — the
//     normal state after importing a discovered monitor, and the case that
//     would clear the identity on the first unrelated apply;
//  3. reading an absent field back as "" instead of null, which is a permanent
//     diff on every hand-made monitor in a user's configuration.
//
// None of the three is loud. All three are only visible as a duplicate monitor
// on somebody's next scan, or as a plan that never converges.
// ─────────────────────────────────────────────────────────────────────────────

// TestMonitorSourceIsOptionalComputed pins the schema decision the other tests
// in this section depend on, and it is the one guardrail here whose absence
// would not fail anything else.
//
// Optional+Computed is the deliberate exception to
// TestMonitorOptionalOnlyAttributesAreNotComputed's rule, and it looks exactly
// like the mistake that test exists to prevent — so the temptation to "fix" it
// into plain Optional is real and this is what refuses it. Plain Optional plans
// null for an attribute the configuration does not mention, and null on this
// pair means CLEAR: the identity would be destroyed on the first apply after an
// import, and the next scan would create a second monitor for the same source.
//
// UseStateForUnknown is half of the same decision, not decoration. Without it
// the unwritten attribute stays unknown through the plan, which is both a
// permanent "known after apply" diff and a value monitorFromModel would render
// as "".
func TestMonitorSourceIsOptionalComputed(t *testing.T) {
	s := monitorSchema(t)
	for _, name := range []string{"source_kind", "source_ref"} {
		attr, ok := s.Attributes[name].(schema.StringAttribute)
		require.True(t, ok, "missing attribute %s", name)
		require.True(t, attr.IsOptional(), "%s must be configurable", name)
		require.True(t, attr.IsComputed(),
			"%s must be Computed: a plain Optional attribute plans null when the configuration does "+
				"not mention it, and null CLEARS a discovery identity", name)
		require.Len(t, attr.PlanModifiers, 1, "%s needs UseStateForUnknown", name)

		// Exercised rather than type-asserted: what matters is the BEHAVIOUR
		// that an attribute left unknown by an omitted configuration plans as
		// the stored value. A modifier of the right type that did something
		// else would pass an identity check and still lose the identity.
		//
		// State.Raw and Plan.Raw have to be real: UseStateForUnknown returns
		// early on a null state (a create, where there is nothing to carry
		// forward) and on a null plan (a destroy), and a zero-value request
		// looks exactly like the former.
		s := monitorSchema(t)
		raw := monitorRawValue(t, monitorResourceModel{
			Name:         types.StringValue("acc"),
			ScheduleKind: types.StringValue("simple"),
			PeriodS:      types.Int64Value(3600),
			SourceKind:   types.StringValue("github-actions"),
			SourceRef:    types.StringValue(".github/workflows/nightly.yml#build"),
		})
		resp := &planmodifier.StringResponse{PlanValue: types.StringUnknown()}
		attr.PlanModifiers[0].PlanModifyString(context.Background(), planmodifier.StringRequest{
			State:       tfsdk.State{Raw: raw, Schema: s},
			Plan:        tfsdk.Plan{Raw: raw, Schema: s},
			StateValue:  types.StringValue("github-actions"),
			PlanValue:   types.StringUnknown(),
			ConfigValue: types.StringNull(),
		}, resp)
		require.Equal(t, types.StringValue("github-actions"), resp.PlanValue,
			"%s must plan its STORED value when the configuration does not mention it; an unknown "+
				"here is both a permanent \"known after apply\" diff and a value that serialises as \"\"",
			name)
	}
}

// TestMonitorSourceKindValidators mirrors api/check_source.go's sourceKindRe and
// maxSourceKindLen at plan time.
//
// The pattern is load-bearing for re-runnability, not tidiness: the reconcile
// key is compared byte-for-byte by a unique index, so "GitHub Actions" and
// "github-actions" are two different identities for one scanner, and a scanner
// whose spelling drifts between releases rediscovers and re-creates every
// monitor it already made.
func TestMonitorSourceKindValidators(t *testing.T) {
	for _, ok := range []string{"crontab", "github-actions", "k8s-cronjob", "systemd-timer", "s3"} {
		require.False(t, validateString(t, "source_kind", ok).Diagnostics.HasError(),
			"%q is a legal scanner name", ok)
	}
	for _, bad := range []string{
		"GitHub Actions", "github_actions", "github-", "-github", "github--actions", "",
	} {
		require.True(t, validateString(t, "source_kind", bad).Diagnostics.HasError(),
			"%q must be refused at plan time: the reconcile key is compared byte-for-byte", bad)
	}
	long := ""
	for range 65 {
		long += "a"
	}
	require.True(t, validateString(t, "source_kind", long).Diagnostics.HasError(),
		"source_kind is capped at 64 characters server-side")
}

// TestMonitorSourceRefValidators pins the 512-character cap and, more
// interestingly, the FLOOR.
//
// The cap is a real server limit rather than a style rule: (project_id,
// source_kind, source_ref) is a btree unique index, and an oversized tuple
// fails the INSERT with an internal error rather than a usable 400.
//
// The floor is the plan-time half of hazard 1. A configured "" would be sent
// verbatim by any faithful serializer, and the API reads "" on this pair as
// "leave the stored value alone" — so it would apply with a 200, change
// nothing, and leave a configuration claiming a ref the monitor does not have.
// Refusing it here is what makes that state unreachable.
func TestMonitorSourceRefValidators(t *testing.T) {
	require.False(t,
		validateString(t, "source_ref", ".github/workflows/nightly.yml#build").Diagnostics.HasError())
	require.True(t, validateString(t, "source_ref", "").Diagnostics.HasError(),
		"an empty source_ref must be refused: the API reads \"\" as \"leave it alone\", not as a clear")

	long := ""
	for range 513 {
		long += "a"
	}
	require.True(t, validateString(t, "source_ref", long).Diagnostics.HasError(),
		"source_ref is capped at 512 characters server-side")
}

// TestMonitorSourceRequiredTogether pins the set-together rule at plan time.
//
// The API answers 400 SOURCE_INCOMPLETE for exactly one of the pair, because
// half an identity reconciles against nothing: a row carrying source_kind with
// a NULL source_ref falls outside the partial unique index entirely, so it
// could be created any number of times while still reading as "discovered"
// everywhere it is displayed.
func TestMonitorSourceRequiredTogether(t *testing.T) {
	validators := (&monitorResource{}).ConfigValidators(context.Background())
	require.Len(t, validators, 1)

	run := func(m monitorResourceModel) diag.Diagnostics {
		t.Helper()
		req := resource.ValidateConfigRequest{
			Config: tfsdk.Config{Raw: monitorRawValue(t, m), Schema: monitorSchema(t)},
		}
		resp := &resource.ValidateConfigResponse{}
		validators[0].ValidateResource(context.Background(),
			resource.ValidateConfigRequest{Config: req.Config}, resp)
		return resp.Diagnostics
	}

	base := monitorResourceModel{
		Name:         types.StringValue("acc"),
		ScheduleKind: types.StringValue("simple"),
		PeriodS:      types.Int64Value(3600),
	}

	neither := base
	require.False(t, run(normaliseMonitorSets(context.Background(), neither)).HasError(),
		"a configuration mentioning neither half is the normal state for a hand-made monitor, and "+
			"for an imported discovered one")

	both := base
	both.SourceKind = types.StringValue("github-actions")
	both.SourceRef = types.StringValue(".github/workflows/nightly.yml#build")
	require.False(t, run(normaliseMonitorSets(context.Background(), both)).HasError())

	kindOnly := base
	kindOnly.SourceKind = types.StringValue("github-actions")
	require.True(t, run(normaliseMonitorSets(context.Background(), kindOnly)).HasError(),
		"source_kind alone is 400 SOURCE_INCOMPLETE server-side")

	refOnly := base
	refOnly.SourceRef = types.StringValue(".github/workflows/nightly.yml#build")
	require.True(t, run(normaliseMonitorSets(context.Background(), refOnly)).HasError(),
		"source_ref alone is 400 SOURCE_INCOMPLETE server-side")
}

// TestMonitorSourcePatchOmitsKeysWhenUnconfigured is THE test in this section.
//
// It is hazard 2, and it is the one that destroys data. `desired` holds the
// stored identity — that is what UseStateForUnknown and resolveUnknownsFromState
// leave behind for an attribute the configuration does not mention — while `cfg`
// is null for both, because the practitioner wrote neither. That is not an
// exotic state: it is what every configuration looks like after
// `terraform import` of a discovered monitor, and after every `export_terraform`
// whose source lines were deleted for an older provider.
//
// Every neighbouring clearable attribute in monitorPatchFromModel answers that
// shape with an explicit null, and an explicit null here CLEARS the reconcile
// key. The next scan would not recognise the monitor and would create a second
// one for the same source — the exact duplicate the key exists to prevent,
// produced by an apply that only renamed something.
//
// So the assertion is about absence, and it is deliberately not
// `require.Nil(patch["source_kind"])`: a map lookup of a missing key returns nil
// too, and this test would pass against the very bug it exists to catch.
func TestMonitorSourcePatchOmitsKeysWhenUnconfigured(t *testing.T) {
	ctx := context.Background()

	desired := monitorResourceModel{
		Name:         types.StringValue("acc-renamed"),
		ScheduleKind: types.StringValue("simple"),
		PeriodS:      types.Int64Value(3600),
		Tags:         types.SetNull(types.StringType),
		// The stored identity, carried into the plan by UseStateForUnknown.
		SourceKind: types.StringValue("github-actions"),
		SourceRef:  types.StringValue(".github/workflows/nightly.yml#build"),
	}
	// The configuration mentions neither half.
	cfg := desired
	cfg.SourceKind = types.StringNull()
	cfg.SourceRef = types.StringNull()

	patch, err := monitorPatchFromModel(ctx, desired, cfg)
	require.NoError(t, err)

	_, hasKind := patch["source_kind"]
	_, hasRef := patch["source_ref"]
	require.False(t, hasKind,
		"source_kind must be ABSENT from the patch, not null: a null clears the reconcile key, and "+
			"an unwritten attribute is the normal state after importing a discovered monitor")
	require.False(t, hasRef, "source_ref must be absent for the same reason")

	// And the document really did carry the rename, so this is not passing
	// because nothing was built at all.
	require.Equal(t, "acc-renamed", patch["name"])
}

// TestMonitorSourcePatchSendsBothWhenConfigured: a configured pair is sent, as
// a pair, with no empty string anywhere.
//
// Both keys go even when only one changed. The API resolves the two together —
// whatever the request and the stored row combine to must be both set or both
// empty — so a lone key relies on the stored value to complete the identity,
// and a lone `{"source_ref": ...}` against a monitor with no source is a 400.
func TestMonitorSourcePatchSendsBothWhenConfigured(t *testing.T) {
	ctx := context.Background()

	cfg := monitorResourceModel{
		Name:         types.StringValue("acc"),
		ScheduleKind: types.StringValue("simple"),
		PeriodS:      types.Int64Value(3600),
		Tags:         types.SetNull(types.StringType),
		SourceKind:   types.StringValue("crontab"),
		SourceRef:    types.StringValue("/etc/cron.d/backup#0 4 * * *"),
	}

	patch, err := monitorPatchFromModel(ctx, cfg, cfg)
	require.NoError(t, err)
	require.Equal(t, "crontab", patch["source_kind"])
	require.Equal(t, "/etc/cron.d/backup#0 4 * * *", patch["source_ref"])
}

// TestMonitorSourcePatchNeverSendsEmptyString is hazard 1 at the serializer,
// behind the plan-time validators that already make a configured "" unreachable.
//
// It is defence in depth on purpose. "" is the single most dangerous value this
// pair can carry, because it is the one the API reads as the OPPOSITE of what a
// caller means: not "clear this", but "leave it alone". A serializer that ever
// emitted it would produce applies that report success and change nothing,
// forever.
func TestMonitorSourcePatchNeverSendsEmptyString(t *testing.T) {
	ctx := context.Background()

	cfg := monitorResourceModel{
		Name:         types.StringValue("acc"),
		ScheduleKind: types.StringValue("simple"),
		PeriodS:      types.Int64Value(3600),
		Tags:         types.SetNull(types.StringType),
		SourceKind:   types.StringValue(""),
		SourceRef:    types.StringValue(""),
	}

	patch, err := monitorPatchFromModel(ctx, cfg, cfg)
	require.NoError(t, err)
	_, hasKind := patch["source_kind"]
	_, hasRef := patch["source_ref"]
	require.False(t, hasKind, "an empty source_kind must never reach the wire")
	require.False(t, hasRef, "an empty source_ref must never reach the wire")

	// Nor may it appear anywhere in the encoded document, which is the form the
	// API actually reads.
	encoded, err := json.Marshal(patch)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), `"source_kind"`)
	require.NotContains(t, string(encoded), `"source_ref"`)
}

// TestMonitorSourceCreatePayloadOmitsUnset: the create half of hazard 1.
//
// On create, an Optional+Computed attribute the configuration does not mention
// is UNKNOWN, and ValueString() renders an unknown as "". client.Monitor's
// `omitempty` is what turns that into an absent key rather than the
// leave-it-alone empty string, and this pins it on the encoded bytes rather
// than on the struct, because `omitempty` is the only thing standing between
// the two.
func TestMonitorSourceCreatePayloadOmitsUnset(t *testing.T) {
	ctx := context.Background()

	unset := monitorResourceModel{
		Name:         types.StringValue("acc"),
		ScheduleKind: types.StringValue("simple"),
		PeriodS:      types.Int64Value(3600),
		Tags:         types.SetNull(types.StringType),
		SourceKind:   types.StringUnknown(),
		SourceRef:    types.StringUnknown(),
	}
	payload, err := monitorFromModel(ctx, unset)
	require.NoError(t, err)
	encoded, err := json.Marshal(payload)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "source_kind")
	require.NotContains(t, string(encoded), "source_ref")

	set := unset
	set.SourceKind = types.StringValue("k8s-cronjob")
	set.SourceRef = types.StringValue("default/nightly-etl")
	payload, err = monitorFromModel(ctx, set)
	require.NoError(t, err)
	require.Equal(t, "k8s-cronjob", payload.SourceKind)
	require.Equal(t, "default/nightly-etl", payload.SourceRef)
}

// TestMonitorSourceReadsAbsentAsNull is hazard 3.
//
// The API's checkResponse carries both fields with `omitempty`, so a monitor a
// human created — every monitor that predates discovery, and the overwhelming
// majority of monitors in any real configuration — omits both keys, which
// decode as "". Mapping that to StringValue("") would put an empty string into
// state where the configuration says null, which is a diff on every plan
// forever and is never resolvable, because no configuration can produce a "".
//
// The second half is the mirror: a real value must survive, or an imported
// discovered monitor would arrive with no identity at all.
func TestMonitorSourceReadsAbsentAsNull(t *testing.T) {
	ctx := context.Background()
	prior := monitorResourceModel{Tags: types.SetNull(types.StringType)}

	handMade := &client.Monitor{
		ID: "3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f", Name: "acc",
		MonitorType: "heartbeat", ScheduleKind: "simple", PeriodS: 3600, TZ: "UTC", GraceS: 1800,
	}
	got, err := modelFromMonitor(ctx, handMade, prior)
	require.NoError(t, err)
	require.True(t, got.SourceKind.IsNull(),
		"a monitor with no source must read as null, not as an empty string")
	require.True(t, got.SourceRef.IsNull(), "likewise for source_ref")

	discovered := *handMade
	discovered.SourceKind = "github-actions"
	discovered.SourceRef = ".github/workflows/nightly.yml#build"
	got, err = modelFromMonitor(ctx, &discovered, prior)
	require.NoError(t, err)
	require.Equal(t, types.StringValue("github-actions"), got.SourceKind)
	require.Equal(t, types.StringValue(".github/workflows/nightly.yml#build"), got.SourceRef)
}

// TestMonitorSourceClearedOutOfBandReadsAsNull pins the read behaviour that
// makes the documented clearing route work at all.
//
// Clearing an identity is deliberately not something deleting the attributes
// from a configuration does — that is the whole point of Optional+Computed —
// so the supported route is an out-of-band clear (PATCH with two nulls, the MCP
// tool, or the dashboard) that the next refresh adopts. That only converges if
// the read reports the clear rather than echoing the prior state back, which is
// exactly why this pair does NOT use writeOnlyString despite superficially
// resembling ci_workflow/ci_branch: here "" is a real answer ("this monitor has
// no source"), not the API declining to report a value it holds.
func TestMonitorSourceClearedOutOfBandReadsAsNull(t *testing.T) {
	ctx := context.Background()

	// Prior state says the monitor is discovered; the server now says it is not.
	prior := monitorResourceModel{
		Tags:       types.SetNull(types.StringType),
		SourceKind: types.StringValue("github-actions"),
		SourceRef:  types.StringValue(".github/workflows/nightly.yml#build"),
	}
	cleared := &client.Monitor{
		ID: "3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f", Name: "acc",
		MonitorType: "heartbeat", ScheduleKind: "simple", PeriodS: 3600, TZ: "UTC", GraceS: 1800,
	}

	got, err := modelFromMonitor(ctx, cleared, prior)
	require.NoError(t, err)
	require.True(t, got.SourceKind.IsNull(),
		"a source cleared outside Terraform must surface, not be masked by the prior state")
	require.True(t, got.SourceRef.IsNull(), "likewise for source_ref")
}

// TestMonitorSourcePatchNeededSeesASourceChange: a source change is a real
// diff, not a no-op apply.
//
// monitorPatchNeeded gates the PATCH entirely, comparing two models through
// monitorFromModel. An attribute monitorFromModel does not carry is invisible
// to it, so a configuration that changed only the source would issue a GET and
// report success without writing anything.
func TestMonitorSourcePatchNeededSeesASourceChange(t *testing.T) {
	ctx := context.Background()

	state := monitorResourceModel{
		Name:         types.StringValue("acc"),
		ScheduleKind: types.StringValue("simple"),
		PeriodS:      types.Int64Value(3600),
		Tags:         types.SetNull(types.StringType),
		SourceKind:   types.StringValue("github-actions"),
		SourceRef:    types.StringValue(".github/workflows/nightly.yml#build"),
	}

	same, err := monitorPatchNeeded(ctx, state, state)
	require.NoError(t, err)
	require.False(t, same, "an unchanged source must not force a write")

	moved := state
	moved.SourceRef = types.StringValue(".github/workflows/nightly.yml#publish")
	changed, err := monitorPatchNeeded(ctx, moved, state)
	require.NoError(t, err)
	require.True(t, changed, "a moved source_ref is a real change and must reach PATCH")
}

// TestAddSourceConflictDiag: both 409s are surfaced with their code, and
// nothing else is swallowed.
//
// The two conflicts are about the same pair of fields and mean entirely
// different things — "somebody else already holds this identity" versus "you
// asked the wrong endpoint to change one" — so a shared "conflict" diagnostic
// would leave a practitioner with no way to tell which happened. The code is
// the searchable part of the answer, so it goes in the title.
func TestAddSourceConflictDiag(t *testing.T) {
	const (
		kind = "github-actions"
		ref  = ".github/workflows/nightly.yml#build"
	)

	t.Run("already monitored", func(t *testing.T) {
		var diags diag.Diagnostics
		err := &client.Problem{
			Status: 409,
			Detail: "a monitor already exists for source github-actions .github/workflows/nightly.yml#build",
			Code:   "SOURCE_ALREADY_MONITORED",
		}
		require.True(t, addSourceConflictDiag(&diags, err, kind, ref))
		require.True(t, diags.HasError())
		require.Contains(t, diags.Errors()[0].Summary(), "SOURCE_ALREADY_MONITORED")
		require.Contains(t, diags.Errors()[0].Detail(), ref)
	})

	t.Run("immutable on upsert", func(t *testing.T) {
		var diags diag.Diagnostics
		err := &client.Problem{
			Status: 409,
			Detail: "source_kind/source_ref cannot be changed through a slug upsert",
			Code:   "SOURCE_IMMUTABLE_ON_UPSERT",
		}
		require.True(t, addSourceConflictDiag(&diags, err, kind, ref))
		require.True(t, diags.HasError())
		require.Contains(t, diags.Errors()[0].Summary(), "SOURCE_IMMUTABLE_ON_UPSERT")
	})

	t.Run("anything else is left to the caller", func(t *testing.T) {
		var diags diag.Diagnostics
		err := &client.Problem{Status: 409, Detail: "some other conflict", Code: "SLUG_TAKEN"}
		require.False(t, addSourceConflictDiag(&diags, err, kind, ref))
		require.False(t, diags.HasError(),
			"an unrelated error must fall through to the generic diagnostic, not be relabelled")

		require.False(t, addSourceConflictDiag(&diags, fmt.Errorf("dial tcp: timeout"), kind, ref),
			"a transport error carries no problem code at all")
	})
}
