package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/stretchr/testify/require"
)

// validateDestinationString runs every validator declared on a string attribute
// of the destination schema.
func validateDestinationString(t *testing.T, attrName, value string) validator.StringResponse {
	t.Helper()
	schemaResp := &resource.SchemaResponse{}
	NewDestinationResource().Schema(context.Background(), resource.SchemaRequest{}, schemaResp)
	require.False(t, schemaResp.Diagnostics.HasError(), "%v", schemaResp.Diagnostics)

	attr, ok := schemaResp.Schema.Attributes[attrName].(schema.StringAttribute)
	require.True(t, ok, "%s must be a string attribute", attrName)

	req := validator.StringRequest{ConfigValue: types.StringValue(value)}
	resp := validator.StringResponse{}
	for _, v := range attr.Validators {
		v.ValidateString(context.Background(), req, &resp)
	}
	return resp
}

// TestDestinationAddressIsValidatedAtPlanTime: the API runs mail.ParseAddress
// (api/channels.go: validateChannelConfig), so a typo'd address is a 400
// partway through an apply. Every other constraint on this resource is a plan
// error naming the attribute, and this one has no business being different.
func TestDestinationAddressIsValidatedAtPlanTime(t *testing.T) {
	for _, tc := range []struct {
		name    string
		address string
		valid   bool
	}{
		{"plain", "ops@example.com", true},
		{"subdomain", "on-call@alerts.example.co.uk", true},
		{"plus tag", "ops+lastping@example.com", true},
		{"display name", "Ops Team <ops@example.com>", true},
		{"no at sign", "ops.example.com", false},
		{"no domain", "ops@", false},
		{"no local part", "@example.com", false},
		{"spaces", "ops @example.com", false},
		{"two addresses", "a@example.com, b@example.com", false},
		{"trailing junk", "ops@example.com>", false},
		// An empty address is the required-attribute check's business, not
		// this validator's; reporting both would be noise.
		{"empty", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := validateDestinationString(t, "address", tc.address)
			if tc.valid {
				require.False(t, resp.Diagnostics.HasError(), "%q should be accepted: %v",
					tc.address, resp.Diagnostics)
				return
			}
			require.True(t, resp.Diagnostics.HasError(),
				"%q should be rejected at plan time, not by the API at apply time", tc.address)
		})
	}
}

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

// TestDestinationConfigPatchSkipsUnchangedConfig covers the one branch that had
// no test at all: a rename must not drag the whole `config` object along with
// it. The acceptance rename test could not catch this — it holds the address
// constant, so an always-send-config Update would produce the same observable
// result.
//
// Note what this does NOT claim. Resending an identical config would not reset
// `verified` either: the server compares the old and new addresses before
// touching verification (api/channels.go: handleUpdateChannel), so it is the
// server, not this skip, that makes a rename safe for an email destination.
// What the skip buys is a rename that stays a rename — no needless rewrite of
// write-only credentials over the wire on an update that has nothing to do with
// them, and no re-validation of a config the operator did not touch.
func TestDestinationConfigPatchSkipsUnchangedConfig(t *testing.T) {
	email := func(name, address string) destinationResourceModel {
		return destinationResourceModel{
			Kind:    types.StringValue("email"),
			Name:    types.StringValue(name),
			Address: types.StringValue(address),
		}
	}

	t.Run("pure rename sends no config", func(t *testing.T) {
		require.Nil(t, destinationConfigPatch(
			email("renamed", "ops@example.com"),
			email("original", "ops@example.com"),
		), "a rename must not resend config")
	})

	t.Run("changed address sends config", func(t *testing.T) {
		require.Equal(t, map[string]string{"address": "new@example.com"}, destinationConfigPatch(
			email("same", "new@example.com"),
			email("same", "old@example.com"),
		))
	})

	t.Run("rotated credential sends config", func(t *testing.T) {
		slack := func(hook string) destinationResourceModel {
			return destinationResourceModel{
				Kind:       types.StringValue("slack"),
				Name:       types.StringValue("oncall"),
				WebhookURL: types.StringValue(hook),
			}
		}
		require.Equal(t, map[string]string{"webhook_url": "https://hooks.slack.com/new"},
			destinationConfigPatch(slack("https://hooks.slack.com/new"), slack("https://hooks.slack.com/old")))
	})

	t.Run("newly set ntfy token sends config", func(t *testing.T) {
		ntfy := func(token types.String) destinationResourceModel {
			return destinationResourceModel{
				Kind:     types.StringValue("ntfy"),
				Name:     types.StringValue("alerts"),
				TopicURL: types.StringValue("https://ntfy.example.com/a"),
				Token:    token,
			}
		}
		// Adding a token to an existing tokenless destination is a config
		// change even though topic_url is untouched.
		require.Equal(t, map[string]string{
			"topic_url": "https://ntfy.example.com/a",
			"token":     "tk",
		}, destinationConfigPatch(ntfy(types.StringValue("tk")), ntfy(types.StringNull())))

		// And an unchanged token is not a reason to rewrite anything.
		require.Nil(t, destinationConfigPatch(
			ntfy(types.StringValue("tk")), ntfy(types.StringValue("tk"))))
	})
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
