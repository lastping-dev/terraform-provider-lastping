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

// TestRouteIsServerDefault pins the exemption that lets a monitor and its routes
// be created in one apply.
//
// The API auto-routes every new monitor's down/fail/recovery events to the
// project's default email channel, so the routes Terraform meets on a monitor
// it created a millisecond ago are the server's, not a person's. Adopting them
// is right. Adopting anything wider than the exact signature attachDefaultRoutes
// writes — one destination, and that destination is the project's first verified
// email channel — would reinstate the hazard the guard exists for, which is a
// silent redirect of somebody's alerts.
func TestRouteIsServerDefault(t *testing.T) {
	const (
		defaultEmail = "70d93e48-1a2b-4c3d-8e9f-0a1b2c3d4e5f"
		otherEmail   = "5b6d7e8f-9a0b-4c1d-8e2f-3a4b5c6d7e8f"
		slack        = "8a1e7c92-4d3b-4a1f-9c2e-5b6d7e8f9a0b"
	)

	for _, tc := range []struct {
		name          string
		existing      []string
		defaultDestID string
		want          bool
	}{
		{
			name:          "exactly what attachDefaultRoutes writes",
			existing:      []string{defaultEmail},
			defaultDestID: defaultEmail,
			want:          true,
		},
		{
			// Two destinations cannot have come from attachDefaultRoutes: it
			// writes a one-element list. Somebody added the second one.
			name:          "default destination plus another",
			existing:      []string{defaultEmail, slack},
			defaultDestID: defaultEmail,
			want:          false,
		},
		{
			name:          "default destination, but not first in the list",
			existing:      []string{slack, defaultEmail},
			defaultDestID: defaultEmail,
			want:          false,
		},
		{
			// A hand-made route that happens to have one destination is the
			// ordinary conflict case, and must stay one.
			name:          "single destination that is not the default",
			existing:      []string{slack},
			defaultDestID: defaultEmail,
			want:          false,
		},
		{
			// A second verified email channel is not the one the server picks.
			name:          "a different email destination",
			existing:      []string{otherEmail},
			defaultDestID: defaultEmail,
			want:          false,
		},
		{
			// No verified email channel means attachDefaultRoutes returned
			// early and never wrote anything, so nothing can be its output.
			// Without the guard, "" would match a project whose route list the
			// API somehow answered with an empty id.
			name:          "project has no default destination",
			existing:      []string{defaultEmail},
			defaultDestID: "",
			want:          false,
		},
		{
			name:          "no route at all",
			existing:      nil,
			defaultDestID: defaultEmail,
			want:          false,
		},
		{
			name:          "empty route",
			existing:      []string{},
			defaultDestID: defaultEmail,
			want:          false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, routeIsServerDefault(tc.existing, tc.defaultDestID))
		})
	}
}
