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

// The endpoint is replace-the-set, so an empty set has to reach it as `[]`.
// A nil slice marshals to `null`, and a body of {"assertions": null} is not the
// instruction "this monitor now has none" — it is the absence of one. Clearing
// every assertion from a monitor is the whole reason this distinction matters.
func TestPutAssertionsSendsEmptyArrayNotNull(t *testing.T) {
	var gotPath, gotMethod string
	var gotRaw []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotRaw, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"assertions":[]}`))
	}))
	defer srv.Close()

	out, err := New(srv.URL, "key", "test").PutAssertions(context.Background(), "mid", nil)
	require.NoError(t, err)

	require.Equal(t, http.MethodPut, gotMethod)
	require.Equal(t, "/api/v1/checks/mid/assertions", gotPath)
	require.JSONEq(t, `{"assertions":[]}`, string(gotRaw))

	// And an empty response decodes to an empty slice, never nil, so callers do
	// not have to distinguish the two.
	require.NotNil(t, out)
	require.Empty(t, out)
}

// Every field must reach the wire under the name the API uses, and the three
// optional ones must be omitted when empty rather than sent as "". The API
// stores what it is given, so a stray `"path":""` on a `contains` assertion is
// a value that comes straight back and shows up as a diff.
func TestPutAssertionsSendsEveryFieldUnderTheAPIsNames(t *testing.T) {
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(raw, &gotBody))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"assertions":[]}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "key", "test").PutAssertions(context.Background(), "mid", []Assertion{
		{Name: "rows written", Kind: "json_path", Value: "0", Path: "result.rows_processed", Op: "gt"},
		{Name: "no traceback", Kind: "not_contains", Value: "Traceback"},
		// A server-assigned id must not be echoed back: the endpoint replaces
		// the whole set, so an id in the request is meaningless.
		{Name: "with id", Kind: "contains", Value: "x", ID: "should-not-matter"},
	})
	require.NoError(t, err)

	list, ok := gotBody["assertions"].([]any)
	require.True(t, ok, "body must carry an assertions array, got %#v", gotBody)
	require.Len(t, list, 3)

	require.Equal(t, map[string]any{
		"name": "rows written", "kind": "json_path", "value": "0",
		"path": "result.rows_processed", "op": "gt",
	}, list[0])

	// path and op absent, not "".
	require.Equal(t, map[string]any{
		"name": "no traceback", "kind": "not_contains", "value": "Traceback",
	}, list[1])
}

// GetAssertions is the read half. It must return a non-nil empty slice for a
// monitor with none, so "no assertions" and "not read yet" stay distinguishable
// in the caller (monitorResource.syncAssertions keys off exactly that).
func TestGetAssertions(t *testing.T) {
	var gotPath, gotMethod string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"assertions":[
			{"id":"a1","name":"rows written","kind":"json_path","value":"0",
			 "path":"result.rows_processed","op":"gt"},
			{"id":"a2","name":"no traceback","kind":"not_contains","value":"Traceback"}
		]}`))
	}))
	defer srv.Close()

	out, err := New(srv.URL, "key", "test").GetAssertions(context.Background(), "mid")
	require.NoError(t, err)

	require.Equal(t, http.MethodGet, gotMethod)
	require.Equal(t, "/api/v1/checks/mid/assertions", gotPath)
	require.Equal(t, []Assertion{
		{ID: "a1", Name: "rows written", Kind: "json_path", Value: "0",
			Path: "result.rows_processed", Op: "gt"},
		{ID: "a2", Name: "no traceback", Kind: "not_contains", Value: "Traceback"},
	}, out)
}

func TestGetAssertionsEmptyIsNeverNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The `assertions` key present but null — the shape a stricter encoder
		// could produce, and the one that would hand a nil slice to a caller
		// that treats nil as "not read yet".
		_, _ = w.Write([]byte(`{"assertions":null}`))
	}))
	defer srv.Close()

	out, err := New(srv.URL, "key", "test").GetAssertions(context.Background(), "mid")
	require.NoError(t, err)
	require.NotNil(t, out)
	require.Empty(t, out)
}
