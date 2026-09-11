# A key that exists only while Terraform is running. Nothing about it reaches
# plan or state, and it is revoked when the run ends — so the usual question
# "where did this credential end up?" has one answer: nowhere.
#
# Requires Terraform 1.10 or later.
ephemeral "lastping_api_key" "run" {
  name = "terraform-run"

  # The key's server-side expiry, and the safety net: if a run is killed before
  # it can revoke the key, the key still stops working on its own. LastPing
  # cannot extend a key's lifetime, so set this to comfortably exceed your
  # longest apply. Defaults to "1h".
  ttl = "30m"
}

# The aliased-provider pattern this resource exists for, with one addition: a
# run that goes on to manage API KEYS needs key-management power, and the API's
# default scope of "write" is precisely everything except that. Ask for admin
# explicitly.
#
# A key may never be given a higher scope than the key that mints it, so this
# only works when the provider's own credential is already admin.
ephemeral "lastping_api_key" "admin_run" {
  name  = "terraform-run-admin"
  ttl   = "30m"
  scope = "admin"
}

provider "lastping" {
  alias   = "admin_run"
  api_key = ephemeral.lastping_api_key.admin_run.key
}

# WHAT THIS CANNOT DO, and it is not obvious: anything minted THROUGH this
# provider dies with the run. Revocation cascades down the created_by_key_id
# chain, so when the ephemeral key is revoked at the end of the run, every key
# it created is revoked in the same transaction — including a
# `lastping_api_key` resource applied through `provider = lastping.admin_run`,
# which would then be missing on the next plan and proposed for creation again,
# forever.
#
# So: use a run-scoped admin key for key management whose EFFECT is meant to
# outlive it (revoking keys, or auditing them), and mint keys that must survive
# the run with a credential that survives the run — configure the default
# provider with an admin key instead.

# On Terraform 1.11 and later an ephemeral value can be handed to a calling
# module, which is how a run-scoped admin credential reaches code that does its
# own key management. It is still never persisted.
output "admin_run_key" {
  value     = ephemeral.lastping_api_key.admin_run.key
  ephemeral = true
  sensitive = true
}

# Note what you cannot do: every attribute here is ephemeral, `prefix` and `id`
# included, so none of them can go into a resource argument or an ordinary
# output. On Terraform 1.11 and later, `output { ephemeral = true }` will carry
# one to a calling module — but nowhere that persists.
