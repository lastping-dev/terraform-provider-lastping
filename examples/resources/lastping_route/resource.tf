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

# "every-run", "success" and "started" notify per run rather than on a state
# change, so they are far chattier than the three above and none of them is
# flap-damped. They also share one per-destination rate budget, separate from the
# one down/fail/recovery draws from, so a busy informational route can exhaust it
# and suppress the later informational alerts on that same destination. Give them
# a low-stakes destination rather than the one that pages someone.
resource "lastping_route" "backup_every_run" {
  monitor_id      = lastping_monitor.nightly_backup.id
  event_type      = "every-run"
  destination_ids = [lastping_destination.backup_owner.id]
}

# "success" is the narrower half of "every-run": it fires only when a run
# completes successfully, where "every-run" fires on success and failure alike.
# Use it where a failure must not land on this destination at all.
resource "lastping_route" "backup_success" {
  monitor_id      = lastping_monitor.nightly_backup.id
  event_type      = "success"
  destination_ids = [lastping_destination.backup_owner.id]
}

# "started" fires when a run begins — useful for a long job where "did it start
# at all" is the question, since the overrun rule only reports much later.
resource "lastping_route" "backup_started" {
  monitor_id      = lastping_monitor.nightly_backup.id
  event_type      = "started"
  destination_ids = [lastping_destination.backup_owner.id]
}

# "blocked" fires immediately when an agent reports it is waiting on a human via
# a blocked ping. Unlike the three informational events above, it draws on the
# same alert budget as down/fail/recovery rather than the shared informational
# one, so a blocked run cannot be starved by chatty per-run traffic — point it
# at the same destination that pages someone for "down".
resource "lastping_route" "backup_blocked" {
  monitor_id      = lastping_monitor.nightly_backup.id
  event_type      = "blocked"
  destination_ids = [lastping_destination.oncall_slack.id]
}

# "note" is an agent-reported free-text signal with no state change. It shares
# the every-run/success/started informational budget.
resource "lastping_route" "backup_note" {
  monitor_id      = lastping_monitor.nightly_backup.id
  event_type      = "note"
  destination_ids = [lastping_destination.backup_owner.id]
}

variable "slack_webhook_url" {
  type      = string
  sensitive = true
}
