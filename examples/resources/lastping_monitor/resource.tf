# A nightly backup job that pings in on a cron schedule. grace_s is how long
# after the scheduled 03:00 UTC run the monitor waits before alerting; the API
# validates it to the range [60, 31536000] seconds.
resource "lastping_monitor" "nightly_backup" {
  name          = "Nightly backup"
  slug          = "nightly-backup"
  schedule_kind = "cron"
  cron_expr     = "0 3 * * *"
  tz            = "UTC"
  grace_s       = 900

  tags = ["env:prod", "team:platform"]

  # Caps ping volume to catch a runaway cron entry (e.g. a misconfigured
  # scheduler firing every minute instead of once a night). The ceiling is
  # measured per rolling 1-hour window, not per scheduled period.
  runaway_ceiling = 5
}

# An HTTP probe monitor: LastPing actively fetches probe_url on an interval
# instead of waiting for an inbound ping.
resource "lastping_monitor" "public_api" {
  name             = "Public API health"
  slug             = "public-api-health"
  monitor_type     = "http"
  probe_url        = "https://api.example.com/healthz"
  probe_method     = "GET"
  probe_interval_s = 60

  probe_expected_status = 200
  probe_expected_body   = "ok"
  probe_timeout_s       = 5

  # grace_s is omitted on purpose. The server floors the effective grace to
  # 2 * probe_interval_s (120s here) so a single slow or missed probe cannot
  # false-fire the absence detector. Set it only to ask for *more* than the
  # floor: a smaller value is rejected at plan time, because the server would
  # silently raise it and the applied state could never match the plan.

  tags = ["env:prod", "kind:http"]
}
