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
