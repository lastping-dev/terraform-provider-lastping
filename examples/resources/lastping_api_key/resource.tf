# A key for something that outlives the Terraform run — a CI job, a container,
# a teammate's laptop.
#
# The plaintext lands in Terraform state. `sensitive` hides it from CLI output
# and nothing more, so the state file is now a credential store: use a remote
# backend with encryption at rest and restricted access. If Terraform only needs
# a key for the duration of its own run, use the ephemeral resource instead.
resource "lastping_api_key" "ci" {
  name = "github-actions"

  # Optional, and strongly recommended: a key that expires is a leak with an
  # end date. Must be RFC 3339 and in the future.
  expires_at = "2027-01-01T00:00:00Z"

  # Optional. "write" is the default and what a CI job wants: it can report
  # pings and manage monitors, and it cannot mint itself a replacement key that
  # would survive this one's revocation.
  scope = "write"
}

# A read-only key, for a dashboard, a scraper or an agent that only ever looks.
resource "lastping_api_key" "grafana" {
  name  = "grafana"
  scope = "read"
}

# An admin key can manage API keys — including creating and revoking them — so
# it is the only scope that can run Terraform configurations like this one.
#
# A key may never be given a higher scope than the key that mints it, so this
# apply only succeeds when the credential the provider is configured with is
# itself admin; otherwise the API refuses with the ceiling in `max_scope`.
resource "lastping_api_key" "platform" {
  name       = "platform-team"
  scope      = "admin"
  expires_at = "2027-01-01T00:00:00Z"
}

# Destroying an API key resource REVOKES EVERY KEY IT CREATED, recursively: the
# API walks the created_by_key_id chain in one transaction. So destroying
# `platform` above also revokes anything minted with it — by another Terraform
# run, by an agent over MCP, or by hand — and those consumers stop
# authenticating at the same moment.
#
# `created_by_key_id` is the other half of that chain, and it is read back into
# state: it names the key this one was minted by, or is null for a key created
# from the dashboard.
output "ci_api_key_parent" {
  value = lastping_api_key.ci.created_by_key_id
}

# Rotation. `name` and `expires_at` both force replacement, and Terraform
# destroys before creating by default — which revokes the old key before the new
# one exists, so anything still presenting it fails in the gap. Rotate with
# create_before_destroy and roll consumers over before the next apply.
resource "lastping_api_key" "worker" {
  name       = "batch-worker-2027q1"
  expires_at = "2027-04-01T00:00:00Z"

  lifecycle {
    create_before_destroy = true
  }
}

# Hand the key to whatever needs it. Marking the output sensitive keeps it out
# of CLI output; it is still readable with `terraform output -raw`, and it is
# still in state either way.
output "ci_api_key" {
  value     = lastping_api_key.ci.key
  sensitive = true
}

# The prefix is deliberately not a secret: it is how you match a key in the
# dashboard to the one Terraform manages, so it is safe to log.
output "ci_api_key_prefix" {
  value = lastping_api_key.ci.prefix
}
