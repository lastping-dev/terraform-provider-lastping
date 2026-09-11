package provider

import (
	"errors"
	"net/http"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/stretchr/testify/require"

	"github.com/lastping-dev/terraform-provider-lastping/internal/client"
)

// TestAPIKeyCreateDiagnostic_ScopeCapNamesBothScopes is the diagnostic the
// 400 exists for. "scope may not exceed the creating key's own scope" alone
// leaves a practitioner with two unknowns — what was asked for, and what the
// ceiling is — and the fix depends on both: ask for less, or run with a
// stronger credential.
func TestAPIKeyCreateDiagnostic_ScopeCapNamesBothScopes(t *testing.T) {
	err := &client.Problem{
		Status:   http.StatusBadRequest,
		Title:    "Bad Request",
		Detail:   "scope may not exceed the creating key's own scope",
		MaxScope: "write",
	}

	summary, detail := apiKeyCreateDiagnostic("admin", err, "Unable to create API key")
	require.Contains(t, summary, "exceeds the creating key's own scope")
	require.Contains(t, detail, "a key with the admin scope",
		"the diagnostic must name the scope that was requested")
	require.Contains(t, detail, "has the write scope",
		"the diagnostic must name the ceiling the API reported, not merely mention a tier")
	require.Contains(t, detail, `set scope to "write"`,
		"the ceiling is only useful if it is offered as the value to retry with")
	require.Contains(t, detail, "no key was created",
		"a practitioner has to know whether a credential is now loose on the server")

	t.Run("an omitted scope is reported as the default", func(t *testing.T) {
		_, detail := apiKeyCreateDiagnostic("", err, "Unable to create API key")
		require.Contains(t, detail, "write (the default)",
			"there is no line in the configuration to point at, and saying so is the difference "+
				"between a practitioner searching their file and understanding the default")
	})
}

// TestAPIKeyCreateDiagnostic_InsufficientScopeNamesRequired covers the 403 the
// hosted backend does not send yet: enforcement is behind a flag there, so this
// arrives for the first time on somebody's apply the day it is switched on.
func TestAPIKeyCreateDiagnostic_InsufficientScopeNamesRequired(t *testing.T) {
	err := &client.Problem{
		Status:        http.StatusForbidden,
		Title:         "Forbidden",
		Detail:        "this API key's scope does not allow this request",
		Code:          "INSUFFICIENT_SCOPE",
		Fix:           "Use an API key with the admin scope, or re-mint this key with that scope from Settings.",
		RequiredScope: "admin",
	}

	summary, detail := apiKeyCreateDiagnostic("write", err, "Unable to create API key")
	require.Contains(t, summary, "cannot manage API keys")
	require.Contains(t, detail, "admin", "required_scope names the tier to re-mint at")
	require.Contains(t, detail, "Retrying will be refused identically",
		"a deterministic refusal must not read as something to wait out")

	t.Run("a 403 without required_scope still names admin", func(t *testing.T) {
		_, detail := apiKeyCreateDiagnostic("write",
			&client.Problem{Status: http.StatusForbidden, Detail: "forbidden"}, "Unable to create API key")
		require.Contains(t, detail, "admin",
			"the route needs admin; a refusal that names no tier is one a practitioner cannot act on")
	})
}

// TestAPIKeyCreateDiagnostic_OtherFailuresAreVerbatim: everything that is not
// about scope keeps the message the API wrote. Problem.Error already carries
// detail, code and fix, and wrapping it would bury the actionable half.
func TestAPIKeyCreateDiagnostic_OtherFailuresAreVerbatim(t *testing.T) {
	summary, detail := apiKeyCreateDiagnostic("write",
		&client.Problem{Status: http.StatusBadRequest, Detail: "name is required"},
		"Unable to create ephemeral API key")
	require.Equal(t, "Unable to create ephemeral API key", summary,
		"each surface names itself; the shared helper must not rename the other one's failure")
	require.Contains(t, detail, "name is required")

	summary, detail = apiKeyCreateDiagnostic("write", errors.New("connection refused"),
		"Unable to create API key")
	require.Equal(t, "Unable to create API key", summary)
	require.Equal(t, "connection refused", detail)
}

// TestConfiguredScope: a diagnostic quotes what the practitioner wrote, and an
// attribute they left out has to be distinguishable from one they set.
func TestConfiguredScope(t *testing.T) {
	require.Equal(t, "admin", configuredScope(types.StringValue("admin")))
	require.Equal(t, "", configuredScope(types.StringNull()))
	require.Equal(t, "", configuredScope(types.StringUnknown()))
}
