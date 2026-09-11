package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// apiKeyEchoServer answers POST /api/v1/api-keys, capturing the request body so
// a test can assert what actually went on the wire, and answering with whatever
// response document it is given.
func apiKeyEchoServer(t *testing.T, response string, sent *map[string]any) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if sent != nil {
			*sent = body
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "lp_test", "unit")
}

// TestCreateAPIKey_OmitsAnUnsetScope is the reason Scope is omitempty. The API
// treats an ABSENT scope as "apply the default" and an explicitly empty one as
// a caller error it refuses, so sending "" would turn a caller that simply did
// not choose into a 400.
func TestCreateAPIKey_OmitsAnUnsetScope(t *testing.T) {
	var sent map[string]any
	c := apiKeyEchoServer(t, `{"id":"1","name":"k","prefix":"lp_abcdefg","scope":"write"}`, &sent)

	_, err := c.CreateAPIKey(context.Background(), CreateAPIKeyInput{Name: "k"})
	require.NoError(t, err)
	require.NotContains(t, sent, "scope", "an unset scope must not appear in the body at all")
}

// TestCreateAPIKey_SendsTheRequestedScope: the value reaches the API unchanged.
func TestCreateAPIKey_SendsTheRequestedScope(t *testing.T) {
	var sent map[string]any
	c := apiKeyEchoServer(t, `{"id":"1","name":"k","prefix":"lp_abcdefg","scope":"admin"}`, &sent)

	_, err := c.CreateAPIKey(context.Background(), CreateAPIKeyInput{Name: "k", Scope: "admin"})
	require.NoError(t, err)
	require.Equal(t, "admin", sent["scope"])
}

// TestAPIKeyDecodesScopeAndLineage: both new response members land on the
// struct, and an omitted created_by_key_id is nil rather than the zero UUID —
// which would read as a real parent key that happens to be all zeros.
func TestAPIKeyDecodesScopeAndLineage(t *testing.T) {
	parent := "e1d2c3b4-0000-0000-0000-000000000002"
	c := apiKeyEchoServer(t, `{"id":"1","name":"k","prefix":"lp_abcdefg","scope":"read",
		"created_by_key_id":"`+parent+`"}`, nil)

	got, err := c.CreateAPIKey(context.Background(), CreateAPIKeyInput{Name: "k", Scope: "read"})
	require.NoError(t, err)
	require.Equal(t, "read", got.Scope)
	require.NotNil(t, got.CreatedByKeyID)
	require.Equal(t, parent, *got.CreatedByKeyID)

	t.Run("a parentless key reports no lineage", func(t *testing.T) {
		c := apiKeyEchoServer(t, `{"id":"1","name":"k","prefix":"lp_abcdefg","scope":"admin"}`, nil)
		got, err := c.CreateAPIKey(context.Background(), CreateAPIKeyInput{Name: "k"})
		require.NoError(t, err)
		require.Nil(t, got.CreatedByKeyID,
			"a key minted from a dashboard session has no parent, and nil is how that is said")
	})
}

// TestProblem_ScopeExtensionsAreSurfaced covers both scope-shaped refusals: the
// members have to survive decoding, and Error() has to render them, or a
// caller that does nothing special still sees an opaque sentence.
func TestProblem_ScopeExtensionsAreSurfaced(t *testing.T) {
	t.Run("400 max_scope", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"title":"Bad Request","status":400,
				"detail":"scope may not exceed the creating key's own scope","max_scope":"write"}`))
		}))
		defer srv.Close()

		_, err := New(srv.URL, "lp_x", "test").
			CreateAPIKey(context.Background(), CreateAPIKeyInput{Name: "k", Scope: "admin"})
		require.Error(t, err)
		require.Equal(t, "write", ProblemMaxScope(err))
		require.Contains(t, err.Error(), "max_scope: write")
		require.Equal(t, "", ProblemRequiredScope(err),
			"required_scope belongs to the 403 and must not appear on the cap refusal")
	})

	t.Run("403 required_scope", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"title":"Forbidden","status":403,
				"detail":"this API key's scope does not allow this request",
				"code":"INSUFFICIENT_SCOPE","required_scope":"admin"}`))
		}))
		defer srv.Close()

		_, err := New(srv.URL, "lp_x", "test").
			CreateAPIKey(context.Background(), CreateAPIKeyInput{Name: "k"})
		require.Error(t, err)
		require.Equal(t, "admin", ProblemRequiredScope(err))
		require.Equal(t, "INSUFFICIENT_SCOPE", ProblemCode(err),
			"the provider selects its diagnostic from these two members, never from the bare status")
		require.Equal(t, "", ProblemMaxScope(err))
		require.Contains(t, err.Error(), "required_scope: admin")
	})

	t.Run("a problem with neither renders neither", func(t *testing.T) {
		p := &Problem{Status: http.StatusBadRequest, Detail: "name is required"}
		require.Equal(t, "name is required", p.Error(),
			"the parenthetical must not appear on refusals that carry no scope member")
	})
}
