package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// The endpoint is replace-the-set, so an empty set has to reach it as `[]`. A
// nil slice marshals to `null`, and {"guards": null} is not the instruction
// "this monitor now has none" — it is the absence of one.
func TestPutMetricGuardsSendsEmptyArrayNotNull(t *testing.T) {
	var gotPath, gotMethod string
	var gotRaw []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotRaw, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"guards":[]}`))
	}))
	defer srv.Close()

	out, err := New(srv.URL, "key", "test").PutMetricGuards(context.Background(), "mid", nil)
	require.NoError(t, err)

	require.Equal(t, http.MethodPut, gotMethod)
	require.Equal(t, "/api/v1/checks/mid/guards", gotPath)
	require.JSONEq(t, `{"guards":[]}`, string(gotRaw))

	require.NotNil(t, out)
	require.Empty(t, out)
}

// Every field must reach the wire under the API's own names. `path` is the one
// worth pinning: the column behind it is `json_path`, so a struct tag written
// from the schema rather than from the DTO would send a field the API ignores
// and the guard would be stored with an empty path.
//
// Nothing here is omitempty except id: a ceiling of 0 is a legitimate guard
// ("must never report any spend"), and omitempty would drop it.
func TestPutMetricGuardsSendsEveryFieldUnderTheAPIsNames(t *testing.T) {
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"guards":[]}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "key", "test").PutMetricGuards(context.Background(), "mid", []MetricGuard{
		{Name: "daily spend", Path: "cost.usd", WindowS: 86400, Ceiling: 50, Aggregation: "sum"},
		{Name: "never spends", Path: "cost.usd", WindowS: 3600, Ceiling: 0, Aggregation: "max"},
	})
	require.NoError(t, err)

	guards, ok := gotBody["guards"].([]any)
	require.True(t, ok, "body must carry a guards array; got %v", gotBody)
	require.Len(t, guards, 2)

	first := guards[0].(map[string]any)
	require.Equal(t, "daily spend", first["name"])
	require.Equal(t, "cost.usd", first["path"])
	require.Equal(t, float64(86400), first["window_s"])
	require.Equal(t, float64(50), first["ceiling"])
	require.Equal(t, "sum", first["aggregation"])
	require.NotContains(t, first, "id", "a server-assigned id must not be sent back")

	second := guards[1].(map[string]any)
	require.Contains(t, second, "ceiling", "a ceiling of 0 must be sent, not omitted")
	require.Equal(t, float64(0), second["ceiling"])
}

func TestGetMetricGuards(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/api/v1/checks/mid/guards", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"guards":[
			{"id":"g1","name":"daily spend","path":"cost.usd","window_s":86400,"ceiling":50,"aggregation":"sum"},
			{"id":"g2","name":"worst run","path":"usage.total_tokens","window_s":3600,"ceiling":200000,"aggregation":"max"}
		]}`))
	}))
	defer srv.Close()

	out, err := New(srv.URL, "key", "test").GetMetricGuards(context.Background(), "mid")
	require.NoError(t, err)
	require.Equal(t, []MetricGuard{
		{ID: "g1", Name: "daily spend", Path: "cost.usd", WindowS: 86400, Ceiling: 50, Aggregation: "sum"},
		{ID: "g2", Name: "worst run", Path: "usage.total_tokens", WindowS: 3600, Ceiling: 200000, Aggregation: "max"},
	}, out)
}

// The read half must return a non-nil empty slice for a monitor with no guards:
// syncGuards uses nil to mean "not read yet", so a nil here would send it back
// to the API for a set it just fetched.
func TestGetMetricGuardsEmptyIsNeverNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"guards":[]}`))
	}))
	defer srv.Close()

	out, err := New(srv.URL, "key", "test").GetMetricGuards(context.Background(), "mid")
	require.NoError(t, err)
	require.NotNil(t, out)
	require.Empty(t, out)
}
