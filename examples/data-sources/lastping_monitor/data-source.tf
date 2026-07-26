# Look up a monitor by slug — the identifier a human or an agent actually knows.
data "lastping_monitor" "nightly_backup" {
  slug = "nightly-backup"
}

# ...or by UUID, when you already have one from another resource or an output.
data "lastping_monitor" "by_id" {
  id = var.monitor_id
}

# Exactly one of slug/id must be set. Supplying neither is an unanswerable
# query and supplying both could contradict each other, so both are rejected
# at plan time rather than resolved arbitrarily.

# Route alerts to a monitor that lives in the dashboard, without importing it
# into Terraform's state.
resource "lastping_route" "backup_down" {
  monitor_id      = data.lastping_monitor.nightly_backup.id
  event_type      = "down"
  destination_ids = [lastping_destination.oncall.id]
}

resource "lastping_destination" "oncall" {
  kind        = "slack"
  name        = "#oncall"
  webhook_url = var.slack_webhook_url
}

output "backup_status" {
  # new, up, late, or down.
  value = data.lastping_monitor.nightly_backup.status
}

variable "monitor_id" {
  type = string
}

variable "slack_webhook_url" {
  type      = string
  sensitive = true
}
