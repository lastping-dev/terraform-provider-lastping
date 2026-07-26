package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/stretchr/testify/require"
)

// TestDestinationConfigCarriesNtfyToken: ntfy accepts an optional bearer token
// (internal/channels/ntfy.go sends it as `Authorization: Bearer`), which is what
// authenticated and self-hosted ntfy servers require. The API replaces `config`
// wholesale on PATCH, so a payload that omits the token silently wipes the
// stored credential on the next topic_url change.
func TestDestinationConfigCarriesNtfyToken(t *testing.T) {
	cfg := destinationConfig(destinationResourceModel{
		Kind:     types.StringValue("ntfy"),
		TopicURL: types.StringValue("https://ntfy.example.com/alerts"),
		Token:    types.StringValue("tk_ntfy"),
	})
	require.Equal(t, map[string]string{
		"topic_url": "https://ntfy.example.com/alerts",
		"token":     "tk_ntfy",
	}, cfg)
}

// TestDestinationConfigOmitsUnsetNtfyToken: the token is optional, so an ntfy
// destination without one must not send an empty `token` key — the payload has
// to stay byte-identical to what a tokenless configuration produced before.
func TestDestinationConfigOmitsUnsetNtfyToken(t *testing.T) {
	cfg := destinationConfig(destinationResourceModel{
		Kind:     types.StringValue("ntfy"),
		TopicURL: types.StringValue("https://ntfy.sh/alerts"),
	})
	require.Equal(t, map[string]string{"topic_url": "https://ntfy.sh/alerts"}, cfg)
}

// TestDestinationRequiredAttrsExcludeOptional: `token` is required for pushover
// but merely optional for ntfy, so it must not be added to ntfy's required set —
// otherwise every existing tokenless ntfy configuration fails to validate.
func TestDestinationRequiredAttrsExcludeOptional(t *testing.T) {
	require.NotContains(t, destinationKindAttrs["ntfy"], "token",
		"token is optional for ntfy; requiring it breaks public ntfy.sh topics")
	require.Contains(t, destinationKindOptionalAttrs["ntfy"], "token")
	require.Contains(t, destinationKindAttrs["pushover"], "token",
		"token stays required for pushover")
}
