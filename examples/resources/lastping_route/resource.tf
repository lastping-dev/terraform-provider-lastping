# One resource per (monitor, event type). The API replaces the whole destination
# list on every write, so two resources pointing at the same pair would overwrite
# each other on every apply.

resource "lastping_monitor" "nightly_backup" {
  name          = "Nightly backup"
  slug          = "nightly-backup"
  schedule_kind = "cron"
  cron_expr     = "0 3 * * *"
  tz            = "Europe/Berlin"
  grace_s       = 1800
}

resource "lastping_destination" "oncall_slack" {
  kind        = "slack"
  name        = "#oncall"
  webhook_url = var.slack_webhook_url
}

resource "lastping_destination" "backup_owner" {
  kind      = "ntfy"
  name      = "Backup owner"
  topic_url = "https://ntfy.sh/backup-owner"
}

# destination_ids is a list, not a set: the API stores the array in the order you
# give it, so the order is real state and reordering is a real change.
resource "lastping_route" "backup_down" {
  monitor_id      = lastping_monitor.nightly_backup.id
  event_type      = "down"
  destination_ids = [lastping_destination.oncall_slack.id, lastping_destination.backup_owner.id]
}

# Recovery usually wants a narrower audience than the page-everyone "down" route.
resource "lastping_route" "backup_recovery" {
  monitor_id      = lastping_monitor.nightly_backup.id
  event_type      = "recovery"
  destination_ids = [lastping_destination.backup_owner.id]
}

# An empty list is valid and means "deliver nowhere for this event" — useful for
# muting one event type without deleting the route, and different from having no
# route at all.
resource "lastping_route" "backup_fail" {
  monitor_id      = lastping_monitor.nightly_backup.id
  event_type      = "fail"
  destination_ids = []
}

variable "slack_webhook_url" {
  type      = string
  sensitive = true
}
