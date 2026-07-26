package provider

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestParseRouteImportID: a route has no id of its own, so the whole of its
// identity comes from the import string. A malformed value must say what the
// expected shape is — "not found" would send the operator looking for a route
// that was never addressed.
func TestParseRouteImportID(t *testing.T) {
	const monitorID = "3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f"

	for _, tc := range []struct {
		name      string
		id        string
		wantErr   string
		wantEvent string
	}{
		{name: "down", id: monitorID + ":down", wantEvent: "down"},
		{name: "recovery", id: monitorID + ":recovery", wantEvent: "recovery"},
		{name: "fail", id: monitorID + ":fail", wantEvent: "fail"},
		{name: "no separator", id: monitorID, wantErr: "missing the \":\" separator"},
		{name: "bare event type", id: "down", wantErr: "missing the \":\" separator"},
		{name: "monitor slug, not id", id: "my-monitor:down", wantErr: "not a monitor UUID"},
		{name: "empty monitor", id: ":down", wantErr: "not a monitor UUID"},
		{name: "unknown event", id: monitorID + ":every-run", wantErr: "not one of down, recovery, fail"},
		{name: "empty event", id: monitorID + ":", wantErr: "not one of down, recovery, fail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotMonitor, gotEvent, err := parseRouteImportID(tc.id)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, monitorID, gotMonitor)
			require.Equal(t, tc.wantEvent, gotEvent)
		})
	}
}

// TestRouteAdoptionConflict pins where the line falls between "this apply would
// silently redirect somebody's alerts" and "this apply changes nothing".
//
// Both halves matter. Missing the conflicts reinstates the hazard; firing on
// the non-conflicts breaks re-creating a route after a lost state file, which
// is a normal recovery and would push people to delete the route by hand first
// — the very thing the guard exists to prevent.
func TestRouteAdoptionConflict(t *testing.T) {
	const (
		a = "8a1e7c92-4d3b-4a1f-9c2e-5b6d7e8f9a0b"
		b = "1c2d3e4f-5a6b-4c7d-8e9f-0a1b2c3d4e5f"
	)

	for _, tc := range []struct {
		name     string
		existing []string
		want     []string
		conflict bool
	}{
		{name: "no route at all", existing: nil, want: []string{a}},
		{name: "existing route is empty", existing: []string{}, want: []string{a}},
		{name: "identical single", existing: []string{a}, want: []string{a}},
		{name: "identical pair", existing: []string{a, b}, want: []string{a, b}},
		{
			name:     "different destination",
			existing: []string{a},
			want:     []string{b},
			conflict: true,
		},
		{
			// Order is dispatch order and round-trips through state, so the
			// same set in a different order is a real change to a real route.
			name:     "same set, different order",
			existing: []string{a, b},
			want:     []string{b, a},
			conflict: true,
		},
		{
			name:     "existing has an extra destination",
			existing: []string{a, b},
			want:     []string{a},
			conflict: true,
		},
		{
			// Not a no-op: the write would take the existing destination away.
			name:     "creating an empty route over a populated one",
			existing: []string{a},
			want:     []string{},
			conflict: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.conflict, routeAdoptionConflict(tc.existing, tc.want))
		})
	}
}
