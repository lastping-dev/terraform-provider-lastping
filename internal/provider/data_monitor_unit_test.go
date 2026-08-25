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

// TestMonitorDataSourceCiWebhookURLAgreesWithResource pins ci_webhook_url as
// an ordinary readable field on the data source too: unlike ci_secret, it is
// populated by rowToDTO on every GET, so both surfaces must report the
// identical value for the identical response instead of the data source
// reading a stale or null value.
func TestMonitorDataSourceCiWebhookURLAgreesWithResource(t *testing.T) {
	ctx := context.Background()
	mon := client.Monitor{
		ID: "3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f", Name: "acc",
		MonitorType: "heartbeat", ScheduleKind: "simple", PeriodS: 3600, TZ: "UTC", GraceS: 1800,
		CiProvider:   "github",
		CiWebhookURL: "https://ingest.lastping.dev/ci/github/3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f",
	}

	res, err := modelFromMonitor(ctx, &mon, monitorResourceModel{Tags: types.SetNull(types.StringType)})
	require.NoError(t, err)
	data, diags := monitorDataFromAPI(ctx, &mon)
	require.False(t, diags.HasError(), "%v", diags)

	require.Equal(t, res.CiWebhookURL, data.CiWebhookURL)
	require.Equal(t, types.StringValue(
		"https://ingest.lastping.dev/ci/github/3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f"), data.CiWebhookURL)
}

// TestMonitorDataSourceCiSecretAlwaysNull pins the documented asymmetry
// against ci_webhook_url above: data.lastping_monitor and
// data.lastping_monitors only ever call GET/list, which never carry
// ci_secret, and a data source has no prior state of its own to carry a
// create-time value forward from the way the resource does. So even when the
// underlying client.Monitor happens to carry a secret (as it would for the
// literal create response, if one were ever routed through here), the data
// source must still read null — it is not this surface's job to interpret
// where the value came from, and pretending otherwise would be lying about
// what GET actually returns for every other caller of this data source.
func TestMonitorDataSourceCiSecretAlwaysNull(t *testing.T) {
	ctx := context.Background()
	mon := client.Monitor{
		ID: "3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f", Name: "acc",
		MonitorType: "heartbeat", ScheduleKind: "simple", PeriodS: 3600, TZ: "UTC", GraceS: 1800,
		CiProvider: "github",
	}
	data, diags := monitorDataFromAPI(ctx, &mon)
	require.False(t, diags.HasError(), "%v", diags)
	require.True(t, data.CiSecret.IsNull(), "GET never carries ci_secret; the data source must not invent one")
}

// TestMonitorDataSourceNextProbeAtAgreesWithResource pins next_probe_at
// parity between the two surfaces, the same way DueAt already implicitly
// agrees by sharing timestampOrNull.
func TestMonitorDataSourceNextProbeAtAgreesWithResource(t *testing.T) {
	ctx := context.Background()
	mon := client.Monitor{
		ID: "3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f", Name: "acc",
		MonitorType: "http", ScheduleKind: "simple", PeriodS: 60, TZ: "UTC", GraceS: 120,
		ProbeURL: "https://example.com/healthz", ProbeMethod: "GET",
		NextProbeAt: ptrTo("2026-07-13T03:01:00Z"),
	}

	res, err := modelFromMonitor(ctx, &mon, monitorResourceModel{Tags: types.SetNull(types.StringType)})
	require.NoError(t, err)
	data, diags := monitorDataFromAPI(ctx, &mon)
	require.False(t, diags.HasError(), "%v", diags)

	require.Equal(t, res.NextProbeAt, data.NextProbeAt)
	require.Equal(t, types.StringValue("2026-07-13T03:01:00Z"), data.NextProbeAt)
}

// TestMonitorDataSourceSourceAgreesWithResource pins the discovery identity's
// null-versus-empty convention across both surfaces at once.
//
// This is the read hazard from the resource's own section, checked where it can
// diverge silently: the resource and the data source map the same response
// through two different functions, and a data source that reported "" for a
// hand-made monitor would make `data.lastping_monitor.x.source_kind == ""` the
// test for "discovered" in every user's configuration — a condition that is
// true for exactly the monitors that are NOT discovered.
func TestMonitorDataSourceSourceAgreesWithResource(t *testing.T) {
	ctx := context.Background()

	handMade := client.Monitor{
		ID: "3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f", Name: "acc",
		MonitorType: "heartbeat", ScheduleKind: "simple", PeriodS: 3600, TZ: "UTC", GraceS: 1800,
	}
	discovered := handMade
	discovered.SourceKind = "github-actions"
	discovered.SourceRef = ".github/workflows/nightly.yml#build"

	for _, tc := range []struct {
		name string
		mon  client.Monitor
	}{
		{"hand-made monitor has no source", handMade},
		{"discovered monitor carries both halves", discovered},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := modelFromMonitor(ctx, &tc.mon, monitorResourceModel{
				Tags: types.SetNull(types.StringType),
			})
			require.NoError(t, err)
			data, diags := monitorDataFromAPI(ctx, &tc.mon)
			require.False(t, diags.HasError(), "%v", diags)

			require.Equal(t, res.SourceKind, data.SourceKind,
				"source_kind must read identically on both surfaces; the API omits it for a "+
					"hand-made monitor, so both sides must report null rather than \"\"")
			require.Equal(t, res.SourceRef, data.SourceRef, "likewise for source_ref")
		})
	}

	data, diags := monitorDataFromAPI(ctx, &handMade)
	require.False(t, diags.HasError(), "%v", diags)
	require.True(t, data.SourceKind.IsNull())
	require.True(t, data.SourceRef.IsNull())
}

func ptrTo[T any](v T) *T { return &v }
