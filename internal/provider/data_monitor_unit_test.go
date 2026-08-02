package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/stretchr/testify/require"

	"github.com/lastping-dev/terraform-provider-lastping/internal/client"
)

// TestMonitorSurfacesAgreeOnEmptyValues pins the null-versus-zero convention
// between lastping_monitor and data.lastping_monitor, attribute by attribute,
// for the same API response.
//
// The acceptance test pairs in data_monitor_test.go cannot reach this. They
// compare the two surfaces against a real backend, and the backend never
// answers 0 for failure_threshold (the column is NOT NULL DEFAULT 1 and the
// valid range starts at 1) — so a data source that mapped it through
// int64OrNull would report the identical value for every monitor that exists
// and every pair would still pass. The divergence only appears at the zero
// value, which is why it has to be asserted here against a hand-built response
// rather than a live one.
//
// That is not a hypothetical: int64OrNull is what probe_interval_s,
// runaway_ceiling and max_runtime_s use, and reaching for it again on
// failure_threshold "for symmetry" is the obvious next edit. It would make the
// data source report null where the resource reports 0.
func TestMonitorSurfacesAgreeOnEmptyValues(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		mon  client.Monitor
	}{
		{
			name: "everything set",
			mon: client.Monitor{
				ID: "3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f", Name: "acc",
				MonitorType: "heartbeat", ScheduleKind: "simple",
				PeriodS: 3600, TZ: "UTC", GraceS: 1800,
				FailureThreshold: 3,
				MaxRuntimeS:      ptrTo(int64(14400)),
				StepTimeoutS:     ptrTo(int64(900)),
			},
		},
		{
			// The case the live backend cannot produce: both attributes at
			// their empty form. max_runtime_s absent must read as null on both
			// surfaces; failure_threshold at 0 must read as 0 on both, NOT as
			// null on one of them.
			name: "both empty",
			mon: client.Monitor{
				ID: "3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f", Name: "acc",
				MonitorType: "heartbeat", ScheduleKind: "simple",
				PeriodS: 3600, TZ: "UTC", GraceS: 1800,
				FailureThreshold: 0,
				MaxRuntimeS:      nil,
				StepTimeoutS:     nil,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := modelFromMonitor(ctx, &tc.mon, monitorResourceModel{
				Tags: types.SetNull(types.StringType),
			})
			require.NoError(t, err)

			data, diags := monitorDataFromAPI(ctx, &tc.mon)
			require.False(t, diags.HasError(), "%v", diags)

			require.Equal(t, res.FailureThreshold, data.FailureThreshold,
				"failure_threshold must read identically on both surfaces; the API always "+
					"reports a concrete number for it, so neither side may map it to null")
			require.Equal(t, res.MaxRuntimeS, data.MaxRuntimeS,
				"max_runtime_s must read identically on both surfaces; the API omits it when "+
					"unset, so both sides must report null rather than 0")
			require.Equal(t, res.StepTimeoutS, data.StepTimeoutS,
				"step_timeout_s must read identically on both surfaces; the API omits it when "+
					"stall detection is off, so both sides must report null rather than 0")
		})
	}
}

func ptrTo[T any](v T) *T { return &v }
