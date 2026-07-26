package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/stretchr/testify/require"

	"github.com/lastping-dev/terraform-provider-lastping/internal/client"
)

// TestEphemeralTTLDuration: an absent ttl means the documented default, and a
// value that survived validation is honoured exactly.
func TestEphemeralTTLDuration(t *testing.T) {
	for _, absent := range []types.String{types.StringNull(), types.StringUnknown(), types.StringValue("")} {
		d, diags := ttlDuration(absent)
		require.False(t, diags.HasError(), "%v", diags)
		require.Equal(t, defaultEphemeralTTL, d)
	}

	d, diags := ttlDuration(types.StringValue("45m"))
	require.False(t, diags.HasError(), "%v", diags)
	require.Equal(t, 45*time.Minute, d)

	// Unreachable through configuration, but it must not silently fall back to
	// the default: that would hand out a key with a lifetime nobody asked for.
	for _, bad := range []string{"half an hour", "-5m", "0s"} {
		_, diags := ttlDuration(types.StringValue(bad))
		require.True(t, diags.HasError(), "%q should not be accepted", bad)
	}
}

// TestEphemeralDurationValidator keeps ttl typos at plan time.
func TestEphemeralDurationValidator(t *testing.T) {
	run := func(v types.String) validator.StringResponse {
		resp := validator.StringResponse{}
		durationValidator{}.ValidateString(context.Background(),
			validator.StringRequest{Path: path.Root("ttl"), ConfigValue: v}, &resp)
		return resp
	}

	require.False(t, run(types.StringValue("2h")).Diagnostics.HasError())
	require.False(t, run(types.StringNull()).Diagnostics.HasError())
	require.False(t, run(types.StringUnknown()).Diagnostics.HasError())

	require.Contains(t, run(types.StringValue("half an hour")).Diagnostics.Errors()[0].Summary(), "Invalid ttl")
	require.Contains(t, run(types.StringValue("-5m")).Diagnostics.Errors()[0].Detail(), "must be positive")
}

// TestEphemeralRenewAt: the check-in must land before expiry, never in the past,
// and must stop scheduling once the remaining life is too short to act on —
// otherwise Terraform would busy-loop on Renew as expiry approached.
func TestEphemeralRenewAt(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

	t.Run("an hour out, renews renewSkew before expiry", func(t *testing.T) {
		at := renewAt(now, now.Add(time.Hour))
		require.Equal(t, now.Add(time.Hour-renewSkew), at)
	})

	t.Run("close to expiry, renews halfway rather than in the past", func(t *testing.T) {
		at := renewAt(now, now.Add(6*time.Minute))
		require.True(t, at.After(now), "a RenewAt in the past would fire immediately")
		require.True(t, at.Before(now.Add(6*time.Minute)))
		require.Equal(t, now.Add(3*time.Minute), at)
	})

	t.Run("too little left to be worth a round trip", func(t *testing.T) {
		require.True(t, renewAt(now, now.Add(4*time.Minute)).IsZero())
		require.True(t, renewAt(now, now.Add(-time.Minute)).IsZero())
	})

	t.Run("successive renewals terminate", func(t *testing.T) {
		expiry := now.Add(time.Hour)
		at := renewAt(now, expiry)
		for i := 0; !at.IsZero(); i++ {
			require.Less(t, i, 20, "renewal schedule must converge")
			require.True(t, at.Before(expiry), "a renewal must never be scheduled past expiry")
			at = renewAt(at, expiry)
		}
	})
}

// ephemeralTestServer stands in for the API so Renew can be exercised without a
// live backend. keys is the set of ids GET /api/v1/api-keys reports.
func ephemeralTestServer(t *testing.T, keys []string, status int) *client.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"title": "boom", "status": status})
			return
		}
		out := make([]client.APIKey, 0, len(keys))
		for _, id := range keys {
			out = append(out, client.APIKey{ID: id, Name: "acc", Prefix: "lp_zzzzzzz"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return client.New(srv.URL, "lp_test", "unit")
}

// TestEphemeralRenewPlan covers what a check-in can and cannot do. The key has a
// fixed server-side lifetime and the API exposes no way to extend it, so Renew
// is a health check plus a warning — and the distinction between the three
// outcomes matters:
//
//   - revoked out of band: an error, because every later call in the run 401s
//     and the operator needs the cause named once rather than inferred from a
//     trail of auth failures;
//   - API unreachable: a warning, because a failed check-in is not evidence the
//     key is gone, and failing the run over it is worse than the risk;
//   - near expiry: a warning that says renewal is impossible and names the fix.
func TestEphemeralRenewPlan(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	st := ephemeralKeyState{ID: "key-1", ExpiresAt: now.Add(time.Hour)}

	t.Run("healthy key schedules the next check-in silently", func(t *testing.T) {
		e := &apiKeyEphemeralResource{client: ephemeralTestServer(t, []string{"key-1"}, http.StatusOK)}
		at, diags := e.renewPlan(ctx, st, now)
		require.Empty(t, diags, "%v", diags)
		require.Equal(t, now.Add(time.Hour-renewSkew), at)
	})

	t.Run("revoked out of band is an error", func(t *testing.T) {
		e := &apiKeyEphemeralResource{client: ephemeralTestServer(t, nil, http.StatusOK)}
		_, diags := e.renewPlan(ctx, st, now)
		require.True(t, diags.HasError())
		require.Contains(t, diags.Errors()[0].Summary(), "was revoked")
	})

	t.Run("a failed check-in warns but does not fail the run", func(t *testing.T) {
		e := &apiKeyEphemeralResource{client: ephemeralTestServer(t, nil, http.StatusInternalServerError)}
		at, diags := e.renewPlan(ctx, st, now)
		require.False(t, diags.HasError(), "%v", diags)
		require.Len(t, diags.Warnings(), 1)
		require.Contains(t, diags.Warnings()[0].Summary(), "Unable to check")
		require.False(t, at.IsZero(), "the run keeps its key and its schedule")
	})

	t.Run("near expiry warns that renewal is impossible", func(t *testing.T) {
		e := &apiKeyEphemeralResource{client: ephemeralTestServer(t, []string{"key-1"}, http.StatusOK)}
		near := ephemeralKeyState{ID: "key-1", ExpiresAt: now.Add(3 * time.Minute)}
		at, diags := e.renewPlan(ctx, near, now)
		require.False(t, diags.HasError(), "%v", diags)
		require.Len(t, diags.Warnings(), 1)
		require.Contains(t, diags.Warnings()[0].Detail(), "cannot extend one")
		require.Contains(t, diags.Warnings()[0].Detail(), "Raise ttl")
		require.True(t, at.IsZero())
	})
}

// TestEphemeralRevokeIsNeverFatal: Close must not fail a run over cleanup. The
// server-side expiry already bounds the leak, and an already-gone key is the
// outcome Close wanted anyway.
func TestEphemeralRevokeIsNeverFatal(t *testing.T) {
	ctx := context.Background()

	t.Run("failure is a warning", func(t *testing.T) {
		e := &apiKeyEphemeralResource{client: ephemeralTestServer(t, nil, http.StatusInternalServerError)}
		diags := e.revoke(ctx, "key-1")
		require.False(t, diags.HasError())
		require.Len(t, diags.Warnings(), 1)
		require.Contains(t, diags.Warnings()[0].Detail(), "still stop working when it expires")
	})

	t.Run("already gone is silent success", func(t *testing.T) {
		e := &apiKeyEphemeralResource{client: ephemeralTestServer(t, nil, http.StatusNotFound)}
		require.Empty(t, e.revoke(ctx, "key-1"))
	})
}

// TestEphemeralPrivateStateCarriesNoKeyMaterial: private state survives to Renew
// and Close, so it must hold only the identifiers those need. A plaintext key in
// there would be a credential in a place nobody audits.
func TestEphemeralPrivateStateCarriesNoKeyMaterial(t *testing.T) {
	raw, err := json.Marshal(ephemeralKeyState{ID: "key-1", ExpiresAt: time.Now()})
	require.NoError(t, err)

	var fields map[string]any
	require.NoError(t, json.Unmarshal(raw, &fields))
	require.ElementsMatch(t, []string{"id", "expires_at"}, keysOf(fields))
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
