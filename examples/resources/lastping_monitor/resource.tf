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

  # The third and tightest clock. A run that reports no step for 10 minutes is
  # wedged, and waiting out the whole 4-hour max_runtime_s to hear about it is
  # the difference between noticing before breakfast and noticing after. It
  # must be strictly below the effective budget (max_runtime_s here, grace_s
  # when that is unset), or the stall window is empty and the rule could never
  # fire — the API rejects that rather than storing it.
  #
  # Only set it if the job actually reports steps (POST /ping/{id}/step inside
  # a run it has already started): a job that never does opens a stalled
  # incident on every run.
  step_timeout_s = 600
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

# A monitor owned by an autonomous agent, the whole point of pairing
# lastping_agent with agent_id: register the agent once, and every monitor it
# owns rolls up into its status and monitor_count instead of sitting there as
# an unrelated pile of checks.
#
# Reference the agent's `id`, not its `slug`: the API accepts either, but
# always echoes back the canonical UUID, and only the UUID round-trips
# cleanly through Terraform's plan/apply consistency check.
resource "lastping_agent" "nightly_etl" {
  name        = "Nightly ETL bot"
  description = "Runs the nightly ETL pipeline and owns its monitors."
}

resource "lastping_monitor" "etl_ingest" {
  name          = "ETL ingest"
  slug          = "etl-ingest"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 600

  agent_id = lastping_agent.nightly_etl.id

  tags = ["env:prod", "team:data"]
}

# agent_id is an in-place update, not a replacement: re-pointing a monitor at
# a different agent, or removing the attribute to detach it, PATCHes the
# monitor rather than destroying and recreating it. Detaching does not delete
# either resource — the monitor keeps its ping history and simply becomes
# unowned, same as `terraform destroy` on the agent itself.
