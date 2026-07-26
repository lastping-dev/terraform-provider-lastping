package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDo_SendsBearerAuth(t *testing.T) {
	var gotAuth, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "lp_secret", "1.2.3")
	require.NoError(t, c.Do(context.Background(), http.MethodGet, "/api/v1/whoami", nil, nil))
	require.Equal(t, "Bearer lp_secret", gotAuth)
	require.Contains(t, gotUA, "terraform-provider-lastping/1.2.3")
}

// TestDo_ProblemIncludesCodeAndFix — the API returns agent-oriented code and fix
// fields; surfacing them is the whole point of custom error mapping.
func TestDo_ProblemIncludesCodeAndFix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"title":"Bad Request","status":400,
			"detail":"monitor cap reached","code":"MONITOR_CAP_REACHED",
			"fix":"delete an unused monitor or contact support"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "lp_x", "test")
	err := c.Do(context.Background(), http.MethodPost, "/api/v1/checks", map[string]string{"name": "x"}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "monitor cap reached")
	require.Contains(t, err.Error(), "MONITOR_CAP_REACHED")
	require.Contains(t, err.Error(), "delete an unused monitor")
}

func TestDo_NotFoundIsDetectable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"title":"Not Found","status":404,"detail":"check not found"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "lp_x", "test")
	err := c.Do(context.Background(), http.MethodGet, "/api/v1/checks/abc", nil, nil)
	require.True(t, IsNotFound(err), "404 must be detectable so Read can remove from state")
}

// TestDo_PreconditionFailedIsDetectable — Create relies on this to turn a slug
// collision into an actionable diagnostic.
func TestDo_PreconditionFailedIsDetectable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = w.Write([]byte(`{"title":"Precondition Failed","status":412,"detail":"already exists"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "lp_x", "test")
	err := c.Do(context.Background(), http.MethodPost, "/api/v1/checks", nil, nil)
	require.True(t, IsPreconditionFailed(err))
}

func TestWithHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("If-None-Match")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "lp_x", "test")
	require.NoError(t, c.Do(context.Background(), http.MethodPost, "/api/v1/checks", nil, nil,
		WithHeader("If-None-Match", "*")))
	require.Equal(t, "*", got)
}

func TestDo_DecodesResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"abc","name":"Nightly"}`))
	}))
	defer srv.Close()

	var out struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	c := New(srv.URL, "lp_x", "test")
	require.NoError(t, c.Do(context.Background(), http.MethodGet, "/api/v1/checks/abc", nil, &out))
	require.Equal(t, "abc", out.ID)
	require.Equal(t, "Nightly", out.Name)
}
