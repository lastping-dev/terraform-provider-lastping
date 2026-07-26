# Look up a destination someone created in the dashboard, by name.
#
# Destination names are not unique — the API imposes no constraint on them — so
# a name matching more than one destination is an error naming every match,
# not an arbitrary pick. Use `id` for those.
data "lastping_destination" "oncall" {
  name = "#oncall"
}

data "lastping_destination" "by_id" {
  id = var.destination_id
}

# Credentials are never returned by the API, so this data source can only report
# the non-secret `target` hint: the real URL for kinds where it is not itself a
# credential, and a fixed label such as "slack webhook" for kinds where it is.
output "oncall_target" {
  value = data.lastping_destination.oncall.target
}

# An unverified email destination silently receives nothing, which is worth
# surfacing rather than discovering during an incident.
output "oncall_ready" {
  value = data.lastping_destination.oncall.verified && !data.lastping_destination.oncall.disabled
}

resource "lastping_monitor" "nightly_backup" {
  name          = "Nightly backup"
  slug          = "nightly-backup"
  schedule_kind = "simple"
  period_s      = 86400
  grace_s       = 1800
}

resource "lastping_route" "backup_down" {
  monitor_id      = lastping_monitor.nightly_backup.id
  event_type      = "down"
  destination_ids = [data.lastping_destination.oncall.id]
}

variable "destination_id" {
  type = string
}
