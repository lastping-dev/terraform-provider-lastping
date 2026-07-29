package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDefaultEmailDestinationID pins the provider's copy of the server's
// "which channel does a new monitor get routed to" rule.
//
// The API auto-routes every new monitor to the FIRST verified email channel in
// `SELECT * FROM channels WHERE project_id = $1 ORDER BY created_at`
// (api/defaultdest.go: attachDefaultRoutes). GET /api/v1/channels serves those
// same rows in that same order, so the provider can reproduce the choice — but
// only if it reads the list the same way. Picking the wrong one turns the route
// resource's auto-adoption into either a refusal on a legitimate first apply or,
// far worse, a silent takeover of a route somebody configured.
func TestDefaultEmailDestinationID(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "no destinations at all",
			body: `[]`,
			want: "",
		},
		{
			// attachDefaultRoutes returns early here and writes no routes, so
			// there is nothing for the route resource to adopt.
			name: "no email destination",
			body: `[{"id":"a","kind":"slack","verified":true}]`,
			want: "",
		},
		{
			// An unverified email channel is skipped server-side, so it must be
			// skipped here.
			name: "email destination is unverified",
			body: `[{"id":"a","kind":"email","verified":false}]`,
			want: "",
		},
		{
			name: "the only verified email destination",
			body: `[{"id":"a","kind":"slack","verified":true},{"id":"b","kind":"email","verified":true}]`,
			want: "b",
		},
		{
			// "First" is list order, which is created_at order. Taking the last
			// match, or the lowest id, would name a different channel than the
			// one the auto-created route actually points at.
			name: "first of several verified email destinations wins",
			body: `[{"id":"b","kind":"email","verified":true},{"id":"a","kind":"email","verified":true}]`,
			want: "b",
		},
		{
			name: "an unverified email destination does not shadow a later verified one",
			body: `[{"id":"a","kind":"email","verified":false},{"id":"b","kind":"email","verified":true}]`,
			want: "b",
		},
		{
			// attachDefaultRoutes tests verified_at only; it has no opinion on
			// disabled_at. Skipping a disabled channel here would name a
			// different channel than the route holds.
			name: "a disabled but verified email destination is still the default",
			body: `[{"id":"a","kind":"email","verified":true,"disabled":true},{"id":"b","kind":"email","verified":true}]`,
			want: "a",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/api/v1/channels", r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			got, err := New(srv.URL, "key", "test").DefaultEmailDestinationID(context.Background())
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestDefaultEmailDestinationID_ErrorIsReported: the route resource fails closed
// on this error rather than guessing, so it must actually surface instead of
// coming back as "the project has no default destination" — which would look
// exactly like a project where every existing route is somebody's own.
func TestDefaultEmailDestinationID_ErrorIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	got, err := New(srv.URL, "key", "test").DefaultEmailDestinationID(context.Background())
	require.Error(t, err)
	require.Equal(t, "", got)
}
