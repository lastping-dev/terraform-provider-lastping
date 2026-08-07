package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/stretchr/testify/require"

	"github.com/lastping-dev/terraform-provider-lastping/internal/client"
)

// guardObj builds one `metric_guard` block element. Every attribute is
// Required, so unlike assertionObj there is no empty-string-to-null rule here.
func guardObj(name, path string, windowS int64, ceiling float64, aggregation string) attr.Value {
	return types.ObjectValueMust(guardObjectType().AttrTypes, map[string]attr.Value{
		"name":        types.StringValue(name),
		"path":        types.StringValue(path),
		"window_s":    types.Int64Value(windowS),
		"ceiling":     types.Float64Value(ceiling),
		"aggregation": types.StringValue(aggregation),
	})
}

func guardSet(elems ...attr.Value) types.Set {
	return types.SetValueMust(guardObjectType(), elems)
}

// guardBaseModel is the minimal valid monitor every validateGuards case mutates.
func guardBaseModel() monitorResourceModel {
	return monitorResourceModel{
		Name:         types.StringValue("acc"),
		MonitorType:  types.StringValue("heartbeat"),
		ScheduleKind: types.StringValue("simple"),
		PeriodS:      types.Int64Value(3600),
		Tags:         types.SetNull(types.StringType),
		Assertions:   monitorAssertionSetNull(),
		Guards:       monitorGuardSetNull(),
	}
}

// A null set is "no metric_guard blocks configured", and it has to leave the
// API as an EMPTY ARRAY rather than a null: the endpoint is replace-the-set, so
// `[]` is the only thing that means "this monitor now has none".
func TestGuardsFromModel_NullSetIsAnEmptyArrayNotNil(t *testing.T) {
	got, err := guardsFromModel(context.Background(), monitorGuardSetNull())
	require.NoError(t, err)
	require.NotNil(t, got, "a null set must become [], not nil")
	require.Empty(t, got)
}

func TestGuardsFromModel_CarriesEveryField(t *testing.T) {
	got, err := guardsFromModel(context.Background(), guardSet(
		guardObj("daily spend", "cost.usd", 86400, 50, "sum"),
	))
	require.NoError(t, err)
	require.Equal(t, []client.MetricGuard{
		{Name: "daily spend", Path: "cost.usd", WindowS: 86400, Ceiling: 50, Aggregation: "sum"},
	}, got)
}

// An empty answer maps back to the null set, because Terraform has no "explicit
// empty set of blocks" distinct from "no blocks". Returning an empty-but-known
// set instead would show a permanent diff against a configuration with no
// blocks in it.
func TestGuardsToModel_EmptyAnswerIsNull(t *testing.T) {
	got, diags := guardsToModel(context.Background(), []client.MetricGuard{}, monitorGuardSetNull())
	require.False(t, diags.HasError(), "%v", diags)
	require.True(t, got.IsNull())
}

// The server-assigned id must not appear in state: it is not in the schema, and
// a round trip has to produce exactly the configured blocks back.
func TestGuardsToModel_RoundTripsWithoutTheServerID(t *testing.T) {
	ctx := context.Background()
	in := []client.MetricGuard{
		{ID: "server-assigned", Name: "daily spend", Path: "cost.usd", WindowS: 86400, Ceiling: 50, Aggregation: "sum"},
	}
	set, diags := guardsToModel(ctx, in, monitorGuardSetNull())
	require.False(t, diags.HasError(), "%v", diags)

	back, err := guardsFromModel(ctx, set)
	require.NoError(t, err)
	require.Equal(t, []client.MetricGuard{
		{Name: "daily spend", Path: "cost.usd", WindowS: 86400, Ceiling: 50, Aggregation: "sum"},
	}, back, "the id must be dropped, every other field preserved")
}

// Order must not matter. The endpoint returns rows by (created_at, id), and
// every row written by one request shares a created_at, so the tie-break is a
// random UUID — an order-sensitive comparison would issue a pointless
// replace-the-set PUT on most applies that changed nothing.
func TestGuardsEqual_IgnoresOrderAndIDs(t *testing.T) {
	a := []client.MetricGuard{
		{ID: "1", Name: "spend", Path: "cost.usd", WindowS: 86400, Ceiling: 50, Aggregation: "sum"},
		{ID: "2", Name: "tokens", Path: "usage.tok", WindowS: 3600, Ceiling: 1000, Aggregation: "max"},
	}
	b := []client.MetricGuard{
		{ID: "9", Name: "tokens", Path: "usage.tok", WindowS: 3600, Ceiling: 1000, Aggregation: "max"},
		{ID: "8", Name: "spend", Path: "cost.usd", WindowS: 86400, Ceiling: 50, Aggregation: "sum"},
	}
	require.True(t, guardsEqual(a, b))
}

// Every field must participate in the comparison, or an apply that changed only
// that field would issue no write at all. Each case differs from the base in
// exactly ONE field, and each replacement value is distinct from the original.
func TestGuardsEqual_EveryFieldParticipates(t *testing.T) {
	base := []client.MetricGuard{
		{Name: "spend", Path: "cost.usd", WindowS: 86400, Ceiling: 50, Aggregation: "sum"},
	}
	for _, tc := range []struct {
		name  string
		other client.MetricGuard
	}{
		{"name", client.MetricGuard{Name: "budget", Path: "cost.usd", WindowS: 86400, Ceiling: 50, Aggregation: "sum"}},
		{"path", client.MetricGuard{Name: "spend", Path: "cost.eur", WindowS: 86400, Ceiling: 50, Aggregation: "sum"}},
		{"window_s", client.MetricGuard{Name: "spend", Path: "cost.usd", WindowS: 3600, Ceiling: 50, Aggregation: "sum"}},
		{"ceiling", client.MetricGuard{Name: "spend", Path: "cost.usd", WindowS: 86400, Ceiling: 50.5, Aggregation: "sum"}},
		{"aggregation", client.MetricGuard{Name: "spend", Path: "cost.usd", WindowS: 86400, Ceiling: 50, Aggregation: "avg"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.False(t, guardsEqual(base, []client.MetricGuard{tc.other}),
				"a change to %s must be a difference", tc.name)
		})
	}

	require.False(t, guardsEqual(base, nil), "a differing length must be a difference")
}

// A ceiling difference smaller than a fixed-precision format would show still
// has to register: 0.1 and the double just above it are different stored
// values, and suppressing that would make the guard un-editable at that
// precision.
func TestGuardsEqual_CeilingComparisonIsExact(t *testing.T) {
	a := []client.MetricGuard{{Name: "n", Path: "p", WindowS: 60, Ceiling: 0.1, Aggregation: "sum"}}
	b := []client.MetricGuard{{Name: "n", Path: "p", WindowS: 60, Ceiling: 0.10000000000000002, Aggregation: "sum"}}
	require.False(t, guardsEqual(a, b))
	require.True(t, guardsEqual(a, a))
}

// validateGuards mirrors core/metricguard.Validate at plan time. Every case
// here is a configuration the API would reject with a 400 partway through an
// apply.
func TestValidateGuardsRejectsWhatTheAPIWouldReject(t *testing.T) {
	for _, tc := range []struct {
		name      string
		set       types.Set
		wantError string // "" means the configuration must be accepted
	}{
		{
			name: "valid sum guard",
			set:  guardSet(guardObj("daily spend", "cost.usd", 86400, 50, "sum")),
		},
		{
			name: "a ceiling of zero is legitimate",
			set:  guardSet(guardObj("never spends", "cost.usd", 3600, 0, "max")),
		},
		{
			name:      "path carrying query syntax",
			set:       guardSet(guardObj("queryish", "items[*].cost", 3600, 5, "sum")),
			wantError: "query, not a dotted path",
		},
		{
			name:      "path carrying a wildcard",
			set:       guardSet(guardObj("starry", "items.*.cost", 3600, 5, "sum")),
			wantError: "query, not a dotted path",
		},
		{
			// The schema's int64validator.AtMost catches this first, so the
			// message comes from there; either way the plan must fail.
			name:      "window one second past the cap",
			set:       guardSet(guardObj("weekly plus one", "cost.usd", maxGuardWindowSeconds+1, 5, "sum")),
			wantError: "",
		},
		{
			name: "window at exactly the cap",
			set:  guardSet(guardObj("weekly", "cost.usd", maxGuardWindowSeconds, 5, "sum")),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := guardBaseModel()
			cfg.Guards = tc.set
			diags := monitorValidateConfig(t, cfg)
			if tc.wantError == "" {
				// The over-cap case is caught by the schema validator, not by
				// ValidateConfig, so it is asserted separately below.
				if tc.name == "window one second past the cap" {
					return
				}
				require.False(t, diags.HasError(), "%v", diags)
				return
			}
			require.True(t, diags.HasError(), "this configuration must be refused at plan time")
			joined := diags.Errors()[0].Summary() + " " + diags.Errors()[0].Detail()
			require.Contains(t, joined, tc.wantError)
		})
	}
}

// The window cap lives on the block attribute's own validators, so it is
// enforced before ValidateConfig ever runs. Both sides of the boundary are
// pinned: the cap itself is accepted, one second past it is not.
func TestGuardWindowValidatorBoundary(t *testing.T) {
	att, ok := monitorGuardBlock().NestedObject.Attributes["window_s"].(schema.Int64Attribute)
	require.True(t, ok, "window_s must be an int64 attribute")

	run := func(v int64) validator.Int64Response {
		req := validator.Int64Request{ConfigValue: types.Int64Value(v)}
		resp := validator.Int64Response{}
		for _, val := range att.Validators {
			val.ValidateInt64(context.Background(), req, &resp)
		}
		return resp
	}

	for _, tc := range []struct {
		window  int64
		wantErr bool
	}{
		{maxGuardWindowSeconds, false},
		{maxGuardWindowSeconds + 1, true},
		{1, false},
		{0, true},
	} {
		resp := run(tc.window)
		require.Equal(t, tc.wantErr, resp.Diagnostics.HasError(),
			"window_s=%d: unexpected validation outcome: %v", tc.window, resp.Diagnostics)
	}
}

// The per-monitor cap is 5. Five is accepted and six is refused, so an
// off-by-one in either direction fails here rather than becoming a 400
// mid-apply.
func TestValidateGuardsCapIsFive(t *testing.T) {
	build := func(n int) types.Set {
		elems := make([]attr.Value, 0, n)
		for i := 0; i < n; i++ {
			elems = append(elems, guardObj("guard "+string(rune('a'+i)), "cost.usd", 3600, float64(i+1), "sum"))
		}
		return guardSet(elems...)
	}

	cfg := guardBaseModel()
	cfg.Guards = build(maxGuardsPerMonitor)
	require.False(t, monitorValidateConfig(t, cfg).HasError(), "exactly the cap must be accepted")

	cfg = guardBaseModel()
	cfg.Guards = build(maxGuardsPerMonitor + 1)
	diags := monitorValidateConfig(t, cfg)
	require.True(t, diags.HasError(), "one past the cap must be refused")
	require.Contains(t, diags.Errors()[0].Summary(), "Too many metric guards")
}

// A guard on an http monitor is refused at plan time. The API would store it
// happily; it would simply never fire, because a probe has no ping body — which
// is a silent monitoring hole rather than an error.
func TestValidateGuardsRefusedOnHTTPMonitor(t *testing.T) {
	cfg := guardBaseModel()
	cfg.MonitorType = types.StringValue("http")
	cfg.ScheduleKind = types.StringNull()
	cfg.PeriodS = types.Int64Null()
	cfg.ProbeURL = types.StringValue("https://example.test/health")
	cfg.ProbeIntervalS = types.Int64Value(300)
	cfg.Guards = guardSet(guardObj("daily spend", "cost.usd", 86400, 50, "sum"))

	diags := monitorValidateConfig(t, cfg)
	require.True(t, diags.HasError(), "a guard on an http monitor must be refused")
	joined := diags.Errors()[0].Summary() + " " + diags.Errors()[0].Detail()
	require.Contains(t, joined, "not supported on an http monitor")
}
