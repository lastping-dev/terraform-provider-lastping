resource "lastping_monitor" "nightly_backup" {
  name          = "Nightly backup"
  slug          = "nightly-backup"
  schedule_kind = "cron"
  cron_expr     = "0 3 * * *"
  tz            = "Europe/Berlin"
  grace_s       = 1800
}

# monitor_id is required. The API exposes incidents only per monitor
# (GET /api/v1/checks/{id}/incidents) — there is no project-wide endpoint — so
# there is no project-wide form of this query.
data "lastping_incidents" "backup" {
  monitor_id = lastping_monitor.nightly_backup.id
  limit      = 20
}

output "open_incident_causes" {
  value = [
    for i in data.lastping_incidents.backup.incidents : i.cause
    if i.closed_at == null
  ]
}

output "last_incident_opened_at" {
  # Newest first, and empty for a monitor that has never had one.
  value = try(data.lastping_incidents.backup.incidents[0].opened_at, null)
}

# Incidents move without Terraform doing anything, so read them for outputs and
# reports — not as an input to a managed resource, which would then show a diff
# on nearly every plan.
