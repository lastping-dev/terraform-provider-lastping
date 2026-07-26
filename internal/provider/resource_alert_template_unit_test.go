package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/stretchr/testify/require"
)

// TestReplacementPayload is the whole reason this resource does a GET before
// every write. The API's replace semantics are not uniform: event-wide keys are
// wiped and reapplied, but a per-cause row is only touched when the request
// names it (api/api_templates.go: handleAPIPutTemplates). Sending just the
// desired map would leave a removed `down/silence` in place forever.
func TestReplacementPayload(t *testing.T) {
	for _, tc := range []struct {
		name    string
		desired map[string]string
		current map[string]string
		want    map[string]string
	}{
		{
			name:    "removed per-cause key is sent as an explicit delete",
			desired: map[string]string{"down": "a"},
			current: map[string]string{"down": "a", "down/silence": "b"},
			want:    map[string]string{"down": "a", "down/silence": ""},
		},
		{
			name:    "removed event-wide key is sent too, not merely omitted",
			desired: map[string]string{"down": "a"},
			current: map[string]string{"down": "a", "recovery": "b"},
			want:    map[string]string{"down": "a", "recovery": ""},
		},
		{
			name:    "a kept key is never overwritten by its own deletion",
			desired: map[string]string{"down": "new"},
			current: map[string]string{"down": "old"},
			want:    map[string]string{"down": "new"},
		},
		{
			name:    "destroy clears every key, including per-cause ones",
			desired: nil,
			current: map[string]string{"down": "a", "fail/runaway": "b"},
			want:    map[string]string{"down": "", "fail/runaway": ""},
		},
		{
			name:    "destroy with nothing stored is an empty PUT",
			desired: nil,
			current: map[string]string{},
			want:    map[string]string{},
		},
		{
			name:    "a new key on an empty monitor is sent as-is",
			desired: map[string]string{"every-run": "a"},
			current: map[string]string{},
			want:    map[string]string{"every-run": "a"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, replacementPayload(tc.desired, tc.current))
		})
	}
}

// TestAlertTemplateBodyValidation: the server trims every body and reads an
// empty one as "delete", so both forms would apply as something other than what
// was written. Terraform rejects a planned value that differs from a known
// config value, so these can only be caught by refusing them at plan time.
func TestAlertTemplateBodyValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "plain body", body: "{check_name} is down"},
		{name: "internal whitespace is fine", body: "line one\n\nline two"},
		{name: "empty", body: "", wantErr: "Empty alert template"},
		{name: "whitespace only", body: "   \n", wantErr: "Empty alert template"},
		{name: "trailing newline", body: "down\n", wantErr: "surrounding whitespace"},
		{name: "leading space", body: " down", wantErr: "surrounding whitespace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := validator.StringResponse{}
			trimmedValidator{}.ValidateString(context.Background(),
				validator.StringRequest{ConfigValue: types.StringValue(tc.body)}, &resp)

			if tc.wantErr == "" {
				require.False(t, resp.Diagnostics.HasError(), "%v", resp.Diagnostics)
				return
			}
			require.True(t, resp.Diagnostics.HasError())
			require.Contains(t, resp.Diagnostics.Errors()[0].Summary()+resp.Diagnostics.Errors()[0].Detail(),
				tc.wantErr)
		})
	}
}

// TestTemplateKeyPattern pins the key grammar against the server's: an event
// type alone, or event/cause. The cause half stays unconstrained so a cause
// added by the backend later is not rejected here first.
func TestTemplateKeyPattern(t *testing.T) {
	for _, key := range []string{"down", "recovery", "fail", "every-run", "down/silence", "fail/runaway", "fail/ci"} {
		require.True(t, templateKeyPattern.MatchString(key), "%q must be accepted", key)
	}
	for _, key := range []string{"", "up", "Down", "down/", "/silence", "down-silence", "everyrun"} {
		require.False(t, templateKeyPattern.MatchString(key), "%q must be rejected", key)
	}
}
