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
		{name: "every-run", id: monitorID + ":every-run", wantEvent: "every-run"},
		{name: "success", id: monitorID + ":success", wantEvent: "success"},
		{name: "started", id: monitorID + ":started", wantEvent: "started"},
		{name: "blocked", id: monitorID + ":blocked", wantEvent: "blocked"},
		{name: "note", id: monitorID + ":note", wantEvent: "note"},
		{
			name:    "unknown event",
			id:      monitorID + ":runaway",
			wantErr: "not one of down, recovery, fail, every-run, success, started, blocked, note",
		},
		{
			name:    "empty event",
			id:      monitorID + ":",
			wantErr: "not one of down, recovery, fail, every-run, success, started, blocked, note",
		},
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

// TestRouteEventTypeSetsStayDistinct is the guardrail on the one mistake that
// widening routeEventTypes invites: copying the new event types into
// defaultAlertEvents as well.
//
// The two slices look interchangeable and are not. routeEventTypes is what the
// API will *route* (api/routes.go: validEventTypes); defaultAlertEvents is the
// much smaller set the API *auto-attaches* to a new monitor's default email
// destination (api/defaultdest.go). Only the second one drives
// routeIsServerDefault's adoption exemption, so an entry added there tells
// Terraform to silently take over a route a person created by hand — the exact
// clobber the whole adoption guard exists to prevent.
func TestRouteEventTypeSetsStayDistinct(t *testing.T) {
	require.Equal(t, []string{"down", "fail", "recovery", "blocked"}, defaultAlertEvents,
		"defaultAlertEvents mirrors api/defaultdest.go and must not drift from it")

	// The invariant is not the literal list, it is the CLASS. The API auto-routes
	// its RateClassAlert events and never its RateClassInfo ones: every-run,
	// success, started and note fire one or more times per run, so auto-routing
	// them would mail a user on every task transition. Listing any of them here
	// would also make routeIsServerDefault adopt a route a person wrote by hand.
	//
	// blocked is RateClassAlert and joined the set on 2026-08-08; it means an
	// agent is waiting on a human, and it was silent by default until then.
	neverAutoRouted := []string{"every-run", "success", "started", "note"}
	for _, e := range neverAutoRouted {
		require.Contains(t, routeEventTypes, e, "%s must still be routable by hand", e)
		require.NotContains(t, defaultAlertEvents, e,
			"%s is RateClassInfo: routable, but must never be auto-routed", e)
	}

	for _, e := range defaultAlertEvents {
		require.Contains(t, routeEventTypes, e,
			"%s is auto-routed but is not in the routable set, which cannot be right", e)
	}
}

// TestRouteEventTypesMatchSpec pins routeEventTypes against the vendored
// OpenAPI spec's Route.event_type enum (testdata/openapi.yaml, refreshed by
// `make sync-openapi`) rather than a literal restated in this file.
//
// A literal only catches a typo made here; it says nothing about the actual
// failure mode this guards against — the API's canonical event-type set
// (internal/alertevent.validEventTypes in the monorepo) growing while
// routeEventTypes does not. Comparing against the spec catches that the next
// time the spec is refreshed, the same way contract_test.go catches field
// drift for every other resource.
func TestRouteEventTypesMatchSpec(t *testing.T) {
	doc := loadSpec(t)
	node, err := resolveNode(doc, []string{"components", "schemas", "Route", "properties", "event_type"})
	require.NoError(t, err, "locate components.schemas.Route.properties.event_type in %s", specPath)

	rawEnum, ok := node["enum"].([]any)
	require.True(t, ok, "components.schemas.Route.properties.event_type has no enum in %s", specPath)

	specEventTypes := make([]string, len(rawEnum))
	for i, v := range rawEnum {
		s, ok := v.(string)
		require.True(t, ok, "enum entry %#v is not a string", v)
		specEventTypes[i] = s
	}

	require.Equal(t, specEventTypes, routeEventTypes,
		"routeEventTypes has drifted from the OpenAPI spec's Route.event_type enum "+
			"(%s). If the API genuinely changed, run `make sync-openapi` and update "+
			"routeEventTypes to match — and re-check whether the new event type belongs "+
			"in defaultAlertEvents (see TestRouteEventTypeSetsStayDistinct; usually it does not).",
		specPath)
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
		eventType     string
		existing      []string
		defaultDestID string
		want          bool
	}{
		{
			name:          "exactly what attachDefaultRoutes writes",
			eventType:     "down",
			existing:      []string{defaultEmail},
			defaultDestID: defaultEmail,
			want:          true,
		},
		{
			// attachDefaultRoutes writes down, fail and recovery — never
			// every-run. An every-run route pointing at the default destination
			// is therefore somebody's deliberate choice, and adopting it would
			// silently redirect routing nobody asked Terraform to touch.
			name:          "every-run is never auto-routed, so never adopted",
			eventType:     "every-run",
			existing:      []string{defaultEmail},
			defaultDestID: defaultEmail,
			want:          false,
		},
		{
			// Same reasoning for the two event types added alongside them.
			// routeEventTypes grew; defaultAlertEvents deliberately did not, and
			// these two cases are what would fail if somebody widened it to
			// match.
			name:          "success is never auto-routed, so never adopted",
			eventType:     "success",
			existing:      []string{defaultEmail},
			defaultDestID: defaultEmail,
			want:          false,
		},
		{
			name:          "started is never auto-routed, so never adopted",
			eventType:     "started",
			existing:      []string{defaultEmail},
			defaultDestID: defaultEmail,
			want:          false,
		},
		{
			name:          "fail is auto-routed and still adopted",
			eventType:     "fail",
			existing:      []string{defaultEmail},
			defaultDestID: defaultEmail,
			want:          true,
		},
		{
			name:          "recovery is auto-routed and still adopted",
			eventType:     "recovery",
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
			if tc.eventType == "" {
				tc.eventType = "down"
			}
			require.Equal(t, tc.want, routeIsServerDefault(tc.eventType, tc.existing, tc.defaultDestID))
		})
	}
}
