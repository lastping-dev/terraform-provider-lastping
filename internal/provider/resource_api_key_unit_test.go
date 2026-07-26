package provider

import (
	"context"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/stretchr/testify/require"

	"github.com/lastping-dev/terraform-provider-lastping/internal/client"
)

// TestAPIKeyModelPreservesPlaintext is the security-critical invariant: the
// plaintext is returned exactly once, so a refresh must carry the prior value
// through untouched. Overwriting it with the response's empty string would
// destroy the only copy that exists anywhere.
func TestAPIKeyModelPreservesPlaintext(t *testing.T) {
	created := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

	t.Run("create takes the key from the response", func(t *testing.T) {
		got := modelFromAPIKey(&client.APIKey{
			ID: "id", Name: "n", Prefix: "lp_abcdefg", CreatedAt: created, Key: "lp_abcdefg_secret",
		}, apiKeyResourceModel{Key: types.StringUnknown()})
		require.Equal(t, "lp_abcdefg_secret", got.Key.ValueString())
		require.Equal(t, "2026-07-26T12:00:00Z", got.CreatedAt.ValueString())
	})

	t.Run("refresh keeps the prior key", func(t *testing.T) {
		prior := apiKeyResourceModel{Key: types.StringValue("lp_abcdefg_secret")}
		got := modelFromAPIKey(&client.APIKey{
			ID: "id", Name: "n", Prefix: "lp_abcdefg", CreatedAt: created,
		}, prior)
		require.Equal(t, "lp_abcdefg_secret", got.Key.ValueString(),
			"a list response carries no plaintext and must not blank the stored key")
	})
}

// TestAPIKeyExpiresAtValue covers the UTC-normalisation round trip. The API
// stores timestamptz and always answers in UTC, so a configured offset comes
// back spelled differently; without this the apply fails as an inconsistent
// result and every later plan proposes a replacement.
//
// The last case is the one with teeth: a genuinely different instant must NOT
// be masked by echoing prior back, or an expiry changed in the database would
// be invisible forever.
func TestAPIKeyExpiresAtValue(t *testing.T) {
	utc := time.Date(2026, 12, 31, 23, 0, 0, 0, time.UTC)

	t.Run("absent stays absent", func(t *testing.T) {
		require.True(t, expiresAtValue(nil, types.StringNull()).IsNull())
		require.True(t, expiresAtValue(nil, types.StringValue("2027-01-01T00:00:00+01:00")).IsNull(),
			"an expiry removed server-side must surface, not be echoed back")
	})

	t.Run("equivalent offset keeps the configured spelling", func(t *testing.T) {
		got := expiresAtValue(&utc, types.StringValue("2027-01-01T00:00:00+01:00"))
		require.Equal(t, "2027-01-01T00:00:00+01:00", got.ValueString())
	})

	t.Run("no prior takes the server value", func(t *testing.T) {
		require.Equal(t, "2026-12-31T23:00:00Z", expiresAtValue(&utc, types.StringNull()).ValueString())
		require.Equal(t, "2026-12-31T23:00:00Z", expiresAtValue(&utc, types.StringUnknown()).ValueString())
	})

	t.Run("a different instant surfaces as drift", func(t *testing.T) {
		got := expiresAtValue(&utc, types.StringValue("2030-01-01T00:00:00Z"))
		require.Equal(t, "2026-12-31T23:00:00Z", got.ValueString())
	})
}

// TestFutureTimestampValidator mirrors api/apikeys_api.go, which rejects an
// expires_at that is not After(now). Refusing it at plan time can only move the
// server's error earlier; refusing anything the server would accept would be a
// bug, hence the "well into the future" case.
func TestFutureTimestampValidator(t *testing.T) {
	run := func(v string) validator.StringResponse {
		req := validator.StringRequest{Path: path.Root("expires_at"), ConfigValue: types.StringValue(v)}
		resp := validator.StringResponse{}
		futureTimestampValidator{}.ValidateString(context.Background(), req, &resp)
		return resp
	}

	require.False(t, run(time.Now().Add(time.Hour).Format(time.RFC3339)).Diagnostics.HasError())
	require.True(t, run("2020-01-01T00:00:00Z").Diagnostics.HasError())
	require.Contains(t, run("2020-01-01T00:00:00Z").Diagnostics.Errors()[0].Summary(),
		"Expiry is not in the future")

	// A malformed value belongs to rfc3339Validator; two errors for one typo is
	// noise, so this validator stays quiet.
	require.False(t, run("next tuesday").Diagnostics.HasError())

	for _, absent := range []types.String{types.StringNull(), types.StringUnknown()} {
		resp := validator.StringResponse{}
		futureTimestampValidator{}.ValidateString(context.Background(),
			validator.StringRequest{Path: path.Root("expires_at"), ConfigValue: absent}, &resp)
		require.False(t, resp.Diagnostics.HasError())
	}
}
