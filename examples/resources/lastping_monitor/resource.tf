# A nightly backup job that pings in on a cron schedule. grace_s is required
# by the API (validated to the range [60, 31536000] seconds) — it is how long
# after the scheduled 03:00 UTC run the monitor waits before alerting.
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
  name          = "Public API health"
  slug          = "public-api-health"
  monitor_type  = "http"
  probe_url     = "https://api.example.com/healthz"
  probe_method  = "GET"
  probe_interval_s    = 60
  probe_expected_body = "ok"

  # The effective grace is floored to 2x the probe interval server-side, so a
  # single slow or missed probe cannot false-fire the absence detector; a
  # larger requested grace is honored as-is.
  grace_s = 120

  tags = ["env:prod", "kind:http"]
}
