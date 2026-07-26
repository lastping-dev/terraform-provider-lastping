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

# The intended consumer: another provider instance. An ephemeral value can be
# used anywhere Terraform does not persist it, which rules out resource
# arguments and non-ephemeral outputs.
provider "lastping" {
  alias   = "run"
  api_key = ephemeral.lastping_api_key.run.key
}

resource "lastping_monitor" "nightly_backup" {
  provider = lastping.run

  name          = "Nightly backup"
  slug          = "nightly-backup"
  schedule_kind = "cron"
  cron_expr     = "0 3 * * *"
  tz            = "UTC"
  grace_s       = 1800
}

# Note what you cannot do: every attribute here is ephemeral, `prefix` and `id`
# included, so none of them can go into a resource argument or an ordinary
# output. On Terraform 1.11 and later, `output { ephemeral = true }` will carry
# one to a calling module — but nowhere that persists.
