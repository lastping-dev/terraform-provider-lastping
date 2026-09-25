package provider

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/stretchr/testify/require"

	"github.com/lastping-dev/terraform-provider-lastping/internal/client"
)

// trace_content: what a monitor's traces keep of prompt, command and tool
// content. The server owns the default ("dropped") and every monitor that
// predates the attribute already carries it, so the provider must never impose
// a default of its own: a provider-side Default is re-applied on every plan
// where the configuration is null, which would plan a write on a bare provider
// upgrade and, for a monitor someone set to "redacted" outside Terraform,
// silently turn content storage off.

// TestMonitorTraceContentIsOptionalComputedWithoutDefault pins the schema
// shape: configurable, server-settable, no Default, and an unconfigured value
// plans as the stored one.
func TestMonitorTraceContentIsOptionalComputedWithoutDefault(t *testing.T) {
	s := monitorSchema(t)
	attr, ok := s.Attributes["trace_content"].(schema.StringAttribute)
	require.True(t, ok, "lastping_monitor has no trace_content attribute")
	require.True(t, attr.IsOptional(), "trace_content must be configurable")
	require.True(t, attr.IsComputed(),
		"trace_content must be Computed: the server supplies it for a monitor that never set it")
	require.Nil(t, attr.Default,
		"trace_content must carry NO provider-side Default: the server grandfathers the value, "+
			"and a Default would overwrite it on every plan whose configuration omits it")
	require.Len(t, attr.PlanModifiers, 1, "trace_content needs UseStateForUnknown")

	raw := monitorRawValue(t, monitorResourceModel{
		Name:         types.StringValue("acc"),
		ScheduleKind: types.StringValue("simple"),
		PeriodS:      types.Int64Value(3600),
		TraceContent: types.StringValue(traceContentRedacted),
	})
	resp := &planmodifier.StringResponse{PlanValue: types.StringUnknown()}
	attr.PlanModifiers[0].PlanModifyString(context.Background(), planmodifier.StringRequest{
		State:       tfsdk.State{Raw: raw, Schema: s},
		Plan:        tfsdk.Plan{Raw: raw, Schema: s},
		StateValue:  types.StringValue(traceContentRedacted),
		PlanValue:   types.StringUnknown(),
		ConfigValue: types.StringNull(),
	}, resp)
	require.Equal(t, types.StringValue(traceContentRedacted), resp.PlanValue,
		"an omitted trace_content must plan the STORED value, not a change")
}

// TestMonitorTraceContentValidator: the API accepts exactly two values.
func TestMonitorTraceContentValidator(t *testing.T) {
	for _, ok := range []string{"dropped", "redacted"} {
		require.False(t, validateString(t, "trace_content", ok).Diagnostics.HasError(),
			"%q is a value the API accepts", ok)
	}
	for _, bad := range []string{"", "kept", "Dropped", "REDACTED", "none"} {
		require.True(t, validateString(t, "trace_content", bad).Diagnostics.HasError(),
			"%q must be refused at plan time", bad)
	}
}

// TestMonitorTraceContentCreatePayload: an unconfigured value is left off the
// create body so the server applies its default, and a configured one is sent.
func TestMonitorTraceContentCreatePayload(t *testing.T) {
	ctx := context.Background()
	base := monitorResourceModel{
		Name:         types.StringValue("acc"),
		ScheduleKind: types.StringValue("simple"),
		PeriodS:      types.Int64Value(3600),
		Tags:         types.SetNull(types.StringType),
		TraceContent: types.StringUnknown(),
	}

	out, err := monitorFromModel(ctx, base)
	require.NoError(t, err)
	raw, err := json.Marshal(out)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "trace_content",
		"an unconfigured trace_content must not reach the create body")

	base.TraceContent = types.StringValue(traceContentRedacted)
	out, err = monitorFromModel(ctx, base)
	require.NoError(t, err)
	raw, err = json.Marshal(out)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"trace_content":"redacted"`)
}

// TestMonitorTraceContentPatch: sent when configured, absent (never null, never
// "") when not.
func TestMonitorTraceContentPatch(t *testing.T) {
	ctx := context.Background()
	desired := monitorResourceModel{
		Name:         types.StringValue("acc"),
		ScheduleKind: types.StringValue("simple"),
		PeriodS:      types.Int64Value(3600),
		Tags:         types.SetNull(types.StringType),
		TraceContent: types.StringValue(traceContentRedacted),
	}

	cfg := desired
	patch, err := monitorPatchFromModel(ctx, desired, cfg)
	require.NoError(t, err)
	require.Equal(t, traceContentRedacted, patch["trace_content"])

	cfg.TraceContent = types.StringNull()
	patch, err = monitorPatchFromModel(ctx, desired, cfg)
	require.NoError(t, err)
	_, has := patch["trace_content"]
	require.False(t, has, "an unconfigured trace_content must be absent from the patch")
	require.Equal(t, "acc", patch["name"], "the patch was still built")
}

// TestMonitorTraceContentPatchNeeded: a changed value is a real diff.
func TestMonitorTraceContentPatchNeeded(t *testing.T) {
	ctx := context.Background()
	state := monitorResourceModel{
		Name:         types.StringValue("acc"),
		ScheduleKind: types.StringValue("simple"),
		PeriodS:      types.Int64Value(3600),
		Tags:         types.SetNull(types.StringType),
		TraceContent: types.StringValue(traceContentDropped),
	}
	same, err := monitorPatchNeeded(ctx, state, state)
	require.NoError(t, err)
	require.False(t, same)

	plan := state
	plan.TraceContent = types.StringValue(traceContentRedacted)
	changed, err := monitorPatchNeeded(ctx, plan, state)
	require.NoError(t, err)
	require.True(t, changed, "dropped -> redacted must reach PATCH")
}

// TestMonitorTraceContentRead: the server's value is read into state, which is
// what makes an import of a monitor that never set it show "dropped".
func TestMonitorTraceContentRead(t *testing.T) {
	ctx := context.Background()
	prior := monitorResourceModel{Tags: types.SetNull(types.StringType)}
	mon := &client.Monitor{
		ID: "3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f", Name: "acc",
		MonitorType: "heartbeat", ScheduleKind: "simple", PeriodS: 3600, TZ: "UTC", GraceS: 1800,
		TraceContent: traceContentDropped,
	}
	got, err := modelFromMonitor(ctx, mon, prior)
	require.NoError(t, err)
	require.Equal(t, types.StringValue(traceContentDropped), got.TraceContent)

	mon.TraceContent = traceContentRedacted
	got, err = modelFromMonitor(ctx, mon, prior)
	require.NoError(t, err)
	require.Equal(t, types.StringValue(traceContentRedacted), got.TraceContent)
}
