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
				scopeForMessage(requested), maxScope, maxScope, effectiveScope(requested))
	}

	// Gated on the problem MEMBERS, not on the bare status. A 403 that is not
	// about scope — a cap refusal of the shape the API already uses on other
	// routes, say — would otherwise be reported as "re-mint your key", which is
	// both wrong and unactionable. Anything without these members falls through
	// to the verbatim branch, which already renders detail, code and fix.
	if required, code := client.ProblemRequiredScope(err), client.ProblemCode(err); required != "" ||
		code == "INSUFFICIENT_SCOPE" {
		if required == "" {
			// The code said scope but the member is missing: this route needs
			// admin, and naming a tier is the whole point of the message.
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

// scopeForMessage renders the requested scope for the clause that describes
// what the CONFIGURATION asked for. A configuration that asked for nothing gets
// the API's own default, and saying so is the difference between a practitioner
// looking for the line they wrote and one looking for a line that is not there.
func scopeForMessage(requested string) string {
	if requested == "" {
		return apiKeyScopeWrite + " (the API's default, since none is configured)"
	}
	return requested
}

// effectiveScope is the bare tier a request ends up asking for — the configured
// value, or the API's default when none was configured.
//
// It exists because the two clauses of the scope-cap diagnostic need different
// renderings of the same fact: one describes the configuration (and benefits
// from the parenthetical), the other describes the CREATING KEY a practitioner
// would have to run with instead, where a parenthetical about defaults would be
// attached to the wrong key entirely.
func effectiveScope(requested string) string {
	if requested == "" {
		return apiKeyScopeWrite
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
