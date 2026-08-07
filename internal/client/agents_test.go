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

// TestCreateAgentSendsOnlyWritableFields pins the create payload. The endpoint
// has no `slug` field — the server derives it from `name` — and the rollup
// fields are not storable at all, so sending any of them would either be
// ignored (inviting the belief that Terraform controls them) or rejected.
func TestCreateAgentSendsOnlyWritableFields(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]any
	var gotIfNoneMatch string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotIfNoneMatch = r.Header.Get("If-None-Match")
		raw, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(raw, &gotBody))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"aid","slug":"nightly-etl-bot","name":"Nightly ETL bot",
			"description":"d","status":"idle","monitor_count":0,"created_at":"2026-07-01T12:00:00Z"}`))
	}))
	defer srv.Close()

	out, err := New(srv.URL, "key", "test").CreateAgent(context.Background(), Agent{
		Name:        "Nightly ETL bot",
		Description: "d",
		// Deliberately populated: these must not reach the wire.
		Slug: "some-other-slug", Status: "up", MonitorCount: 9, CreatedAt: "whenever",
	})
	require.NoError(t, err)

	require.Equal(t, http.MethodPost, gotMethod)
	require.Equal(t, "/api/v1/agents", gotPath)
	require.Equal(t, map[string]any{"name": "Nightly ETL bot", "description": "d"}, gotBody)

	// Unlike CreateMonitor, no precondition header: POST /api/v1/agents is
	// create-only server-side and answers 409 on a slug collision, so there is
	// no upsert branch to opt out of.
	require.Empty(t, gotIfNoneMatch)

	require.Equal(t, "nightly-etl-bot", out.Slug, "the slug in state is the one the server derived")
}

// TestCreateAgentOmitsEmptyDescription: on create an absent key and an empty
// string are the same thing, so the zero value is dropped rather than sent as
// "". (On update they differ, which is why AgentPatch is a map — see
// TestUpdateAgentSendsExplicitNull.)
func TestCreateAgentOmitsEmptyDescription(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(raw, &gotBody))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"aid","slug":"bot","name":"Bot"}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "key", "test").CreateAgent(context.Background(), Agent{Name: "Bot"})
	require.NoError(t, err)
	require.NotContains(t, gotBody, "description")
}

// TestCreateAgentConflictIsDetectable: a 409 means the derived slug is already
// taken, and the resource turns it into an "import it instead" diagnostic. If
// IsConflict stopped recognising it, the apply would report a bare 409 and an
// operator would have no idea an import was the fix.
func TestCreateAgentConflictIsDetectable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"status":409,"detail":"an agent with slug bot already exists"}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "key", "test").CreateAgent(context.Background(), Agent{Name: "Bot"})
	require.Error(t, err)
	require.True(t, IsConflict(err))
	require.False(t, IsNotFound(err))
}

// TestUpdateAgentSendsExplicitNull is the merge-patch wire check: a nil map
// value has to serialise as JSON null, because that — and only that — clears
// the description server-side. A struct with `omitempty` would drop the key and
// silently leave the stored value in place.
func TestUpdateAgentSendsExplicitNull(t *testing.T) {
	var gotRaw string
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		gotRaw = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"aid","slug":"bot","name":"Bot","description":""}`))
	}))
	defer srv.Close()

	out, err := New(srv.URL, "key", "test").UpdateAgent(context.Background(), "aid",
		AgentPatch{"name": "Bot", "description": nil})
	require.NoError(t, err)

	require.Equal(t, http.MethodPatch, gotMethod)
	require.Equal(t, "/api/v1/agents/aid", gotPath)
	require.Contains(t, gotRaw, `"description":null`)
	require.Equal(t, "", out.Description)
}

// TestGetAgentBySlug resolves through the project list, because the {id} path
// parameter is parsed as a UUID and a slug there is a 400. A slug that does not
// exist must come back as a 404-shaped *Problem so ImportState can fall through
// to the UUID attempt instead of aborting.
func TestGetAgentBySlug(t *testing.T) {
	const body = `[{"id":"a","slug":"deploy-bot","name":"Deploy bot"},
		{"id":"b","slug":"nightly-etl-bot","name":"Nightly ETL bot"}]`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/agents", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := New(srv.URL, "key", "test")

	got, err := c.GetAgentBySlug(context.Background(), "nightly-etl-bot")
	require.NoError(t, err)
	require.Equal(t, "b", got.ID)

	_, err = c.GetAgentBySlug(context.Background(), "no-such-agent")
	require.Error(t, err)
	require.True(t, IsNotFound(err),
		"a missing slug must read as a 404 so import can fall through to the UUID form")
}

// TestDeleteAgentIsANoContentCall: the endpoint answers 204 with no body, which
// Do must not try to decode.
func TestDeleteAgentIsANoContentCall(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	require.NoError(t, New(srv.URL, "key", "test").DeleteAgent(context.Background(), "aid"))
	require.Equal(t, http.MethodDelete, gotMethod)
	require.Equal(t, "/api/v1/agents/aid", gotPath)
}
