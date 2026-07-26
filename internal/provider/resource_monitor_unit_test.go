package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/stretchr/testify/require"
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

// TestMonitorOptionalOnlyAttributesAreNotComputed pins the "attributes can never
// be unset" regression: a purely user-supplied attribute marked Computed keeps
// its old value in state when it is removed from the configuration.
func TestMonitorOptionalOnlyAttributesAreNotComputed(t *testing.T) {
	s := monitorSchema(t)
	for _, name := range []string{
		"slug", "cron_expr", "tags", "runaway_ceiling", "monitor_from",
		"probe_url", "probe_interval_s", "probe_expected_body",
	} {
		attr, ok := s.Attributes[name]
		require.True(t, ok, "missing attribute %s", name)
		require.True(t, attr.IsOptional(), "%s must be optional", name)
		require.False(t, attr.IsComputed(),
			"%s is only ever supplied by the configuration, so Computed would make it impossible to unset", name)
	}

	// The converse: these are genuinely server-supplied and must stay Computed.
	for _, name := range []string{"grace_s", "monitor_type", "schedule_kind", "period_s", "tz"} {
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
}
