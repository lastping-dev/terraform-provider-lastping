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

  # The backup itself may legitimately run for hours, but it must never go
  # quiet for more than 15 minutes past its slot. max_runtime_s governs the
  # overrun deadline only, so the two budgets are set independently: grace_s
  # (900s) still decides how late the *start* may be.
  max_runtime_s = 14400
}

# A flaky-by-nature job: a single failed run is noise, three in a row is a
# problem. failure_threshold defers the incident until the third consecutive
# failure, and any success resets the count.
#
# It gates the `fail` cause only. Silence, overrun and runaway are time- or
# rate-based, so this monitor still alerts the moment it goes quiet.
resource "lastping_monitor" "flaky_import" {
  name          = "Vendor feed import"
  slug          = "vendor-feed-import"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 600

  failure_threshold = 3

  tags = ["env:prod", "team:data"]
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
