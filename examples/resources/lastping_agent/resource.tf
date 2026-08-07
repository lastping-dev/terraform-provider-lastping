# An agent is the worker, not the check. Register one per autonomous worker —
# a deploy bot, an ETL job, an LLM agent — and let it own as many monitors as it
# needs. Its status rolls up live from those monitors, so a fleet of workers can
# be watched as workers rather than as a pile of unrelated checks.

resource "lastping_agent" "nightly_etl" {
  name        = "Nightly ETL bot"
  description = "Runs the nightly ETL pipeline and owns its monitors."
}

# The slug is derived from the name at creation — lowercased, with every run of
# characters outside [a-z0-9] collapsed to a single hyphen — and never changes
# again. "Nightly ETL bot" gives "nightly-etl-bot"; renaming the agent later
# keeps that slug, so anything already referring to it by slug keeps working.
#
# The derived slug must be 3-50 characters, which this provider checks at plan
# time rather than letting the API answer 400 partway through an apply.
output "nightly_etl_slug" {
  value = lastping_agent.nightly_etl.slug
}

# Rolled up live from the monitors this agent owns, worst first:
# down > blocked > late > running > up > pending > idle. It is never stored, so
# it changes without any configuration change — expect it to differ between a
# plan and the refresh that follows.
output "nightly_etl_status" {
  value = lastping_agent.nightly_etl.status
}

# Registering an agent is create-only, not an upsert. A name whose derived slug
# is already taken in this project fails the apply with a clear error instead of
# silently adopting an agent this configuration does not own — import it
# instead. Note that two different names can collide: "Deploy Bot" and
# "deploy bot" both derive "deploy-bot".
resource "lastping_agent" "deploy" {
  name = "Deploy Bot"
}

# Destroying an agent does NOT destroy its monitors. Every monitor it owned
# survives with its ping history, incidents and schedule intact and simply
# becomes unowned, so `terraform destroy` on an agent is a safe operation to
# reason about — it removes the grouping, not the monitoring.
