package provider

import (
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/lastping-dev/terraform-provider-lastping/internal/client"
)

// api_key_scope.go — the scope vocabulary both API key surfaces share, and the
// two refusals that need a diagnostic of their own.
//
// The managed resource and the ephemeral resource are different in kind (one
// hands out a credential that outlives the run, the other one that does not)
// but they mint keys through the same endpoint, so the tiers, the default and
// the two scope-shaped failures are stated once here rather than twice.

const (
	// apiKeyScopeRead reaches every GET.
	apiKeyScopeRead = "read"
	// apiKeyScopeWrite reaches everything except API key management. It is the
	// server's own default for a new key on every surface, and the right tier
	// for a credential handed to CI or to an agent: it can do the work and
	// cannot mint itself a replacement that survives its own revocation.
	apiKeyScopeWrite = "write"
	// apiKeyScopeAdmin reaches everything, key management included.
	apiKeyScopeAdmin = "admin"
)

// apiKeyScopes is the exact set the API stores (the monorepo's
// api_keys_scope_check constraint). Anything else is a 400, so rejecting it at
// plan time names the attribute instead of failing partway through an apply.
var apiKeyScopes = []string{apiKeyScopeRead, apiKeyScopeWrite, apiKeyScopeAdmin}

// apiKeyCreateDiagnostic turns a failed POST /api/v1/api-keys into a summary
// and detail pair.
//
// Two of the ways this call fails are about scope, and neither is actionable
// from the raw problem detail alone:
//
//   - 400 with max_scope: the requested scope outranks the scope of the key
//     Terraform itself is authenticating with. The fix is to ask for less, or
//     to run with a stronger credential — and which one is right depends on
//     both scopes, so the diagnostic names both.
//   - 403 with required_scope: the key Terraform is authenticating with cannot
//     reach key management at all. Retrying is pointless; the key has to be
//     re-minted. This refusal is currently switched off on the hosted backend
//     and will start arriving when it is switched on, which is exactly why it
//     is handled before anyone has seen one.
//
// requested is the scope the configuration asked for, or "" when it asked for
// nothing and the server's default applies. otherwise is the summary for every
// failure that is not about scope, since the two surfaces name themselves
// differently ("Unable to create API key" / "…ephemeral API key").
//
// Everything else is reported verbatim: err is already a *client.Problem whose
// Error() carries detail, code and fix, and inventing a wrapper around a
// message that is already actionable only buries it.
func apiKeyCreateDiagnostic(requested string, err error, otherwise string) (string, string) {
	if maxScope := client.ProblemMaxScope(err); maxScope != "" {
		return "Requested scope exceeds the creating key's own scope",
			fmt.Sprintf("The configuration asks for a key with the %s scope, but the API key Terraform is "+
				"authenticating with has the %s scope, and a key may never be given more power than "+
				"the key that mints it.\n\n"+
				"Either set scope to %q (or lower), or run Terraform with an API key that itself has "+
				"the %s scope.\n\nThe API refused the request, so no key was created.",
				scopeForMessage(requested), maxScope, maxScope, scopeForMessage(requested))
	}

	if client.IsForbidden(err) {
		required := client.ProblemRequiredScope(err)
		if required == "" {
			// The route needs admin; a 403 that does not say so is still a 403
			// about scope, and naming the tier is the whole point of the message.
			required = apiKeyScopeAdmin
		}
		return "The API key running Terraform cannot manage API keys",
			fmt.Sprintf("Creating an API key requires the %s scope, and the key Terraform is "+
				"authenticating with does not have it.\n\n"+
				"Re-mint that key with the %s scope (Settings in the dashboard, or POST "+
				"/api/v1/api-keys from a key that already has it) and set it on the provider. "+
				"Retrying will be refused identically — the decision does not depend on timing.\n\n"+
				"Server response: %s", required, required, err)
	}

	return otherwise, err.Error()
}

// scopeForMessage renders a requested scope for a diagnostic, spelling out the
// case where nothing was configured — "the default" is the difference between a
// practitioner looking for the line they wrote and a practitioner looking for
// the line they did not.
func scopeForMessage(requested string) string {
	if requested == "" {
		return apiKeyScopeWrite + " (the default)"
	}
	return requested
}

// configuredScope reduces a configuration value to what a diagnostic should say
// the practitioner asked for: the string they wrote, or "" for an attribute they
// left out. It reads the CONFIG rather than the plan deliberately — the plan
// carries the default, so it can never report "nothing was configured", which is
// the case where a practitioner is looking for a line that is not in their file.
func configuredScope(v types.String) string {
	if v.IsNull() || v.IsUnknown() {
		return ""
	}
	return v.ValueString()
}
