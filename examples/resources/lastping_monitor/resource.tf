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

# Output assertions: the job runs on time, exits zero — and produced nothing.
# A dead-man's-switch alone cannot tell that apart from a healthy run, because
# nothing ever looks at what the run reported. An assertion does: it inspects
# the body of the success ping, and when it does not hold the ping opens an
# incident with cause `assertion` naming the assertion that failed.
#
# The job has to send a body for any of this to mean anything, e.g.
#   curl -fsS -d '{"result":{"rows_processed":128}}' "$PING_URL"
#
# The blocks REPLACE the whole set: these three are the monitor's complete
# assertion set after an apply, and deleting them all removes them all.
resource "lastping_monitor" "warehouse_export" {
  name          = "Nightly warehouse export"
  slug          = "warehouse-export"
  schedule_kind = "cron"
  cron_expr     = "0 2 * * *"
  grace_s       = 1800

  # The one that matters: an export that wrote zero rows is a failure, however
  # cleanly the process exited. Both sides parse as numbers here, so the
  # comparison is numeric — "12" > "3" the way you would expect, not the way
  # string ordering would have it.
  assertion {
    name  = "rows were written"
    kind  = "json_path"
    path  = "result.rows_processed"
    op    = "gt"
    value = "0"
  }

  # A job that catches its own exceptions and still exits zero leaves the
  # evidence in its output. This turns that into an alert.
  assertion {
    name  = "no traceback in output"
    kind  = "not_contains"
    value = "Traceback (most recent call last)"
  }

  # `matches` is a Go RE2 regular expression — no backtracking, so pattern
  # complexity is not a denial-of-service risk, and the pattern is capped at
  # 1000 bytes.
  assertion {
    name  = "reported a completion line"
    kind  = "matches"
    value = "^export complete: [0-9]+ rows$"
  }
}

# Metric guards: the opposite failure. An assertion catches a run that did
# nothing; a guard catches one that did far too much. An agent that loops,
# retries and burns $400 in a single call is not pinging fast, so a
# pings-per-hour ceiling never sees it — but the number is right there in the
# body the run already sends.
#
# A guard reads one number out of that body at a dotted path, rolls it up over
# a trailing window, and opens an incident with cause `runaway` when the result
# EXCEEDS the ceiling. Equal does not trip. The guard's name is written to the
# incident detail, which is what tells it apart from a ping-rate runaway in the
# alert.
#
#   curl -fsS -d '{"cost":{"usd":0.42},"usage":{"total_tokens":18320}}' "$PING_URL"
#
# Two caps, both measured cost rather than policy: at most 5 guards per
# monitor, and window_s at most 604800 (7 days). A guard re-aggregates every
# ping body in its window on every ping, so the per-ping work is linear in both
# the window and the number of guards.
#
# Like `assertion`, the blocks REPLACE the whole set.
resource "lastping_monitor" "research_agent" {
  name          = "Research agent"
  slug          = "research-agent"
  schedule_kind = "on_demand"
  grace_s       = 900

  # The budget. Sum every dollar the agent reported over a rolling day; alert
  # once the day's total passes $50, whether that was one runaway call or two
  # hundred ordinary ones.
  metric_guard {
    name        = "daily spend"
    path        = "cost.usd"
    window_s    = 86400
    ceiling     = 50
    aggregation = "sum"
  }

  # The blast radius. `max` asks a different question from `sum`: not "have we
  # spent too much today" but "did any single run go off the rails". A loop
  # that burns its whole budget in one call trips this an hour before the daily
  # total would have noticed.
  metric_guard {
    name        = "worst single run"
    path        = "usage.total_tokens"
    window_s    = 3600
    ceiling     = 200000
    aggregation = "max"
  }

  # `avg` is for creep rather than spikes: a prompt change that quietly doubles
  # the typical run's cost never trips a max, and takes all day to trip a sum.
  # Pings that report no number at this path are skipped, not counted as zero,
  # so `start` pings do not drag the average down.
  metric_guard {
    name        = "average run cost"
    path        = "cost.usd"
    window_s    = 21600
    ceiling     = 0.75
    aggregation = "avg"
  }
}

# An on-demand agent. This is the shape `expect_every_s` exists for.
#
# `schedule_kind = "on_demand"` arms NO absence deadlines between runs: that is
# the whole point of the kind — an agent nobody happens to invoke for a week
# must not page for simply not having been asked to run. The cost is that a
# genuinely dead agent is indistinguishable from an idle one, and the monitor
# reads healthy either way.
#
# `expect_every_s` buys the detection back without buying the false pages back.
# It is a floor on SILENCE measured from the monitor's last activity, not a
# cadence: if this agent says nothing at all for 30 minutes, something is
# wrong. It stands down entirely while a run is in flight, so the four-hour run
# `max_runtime_s` allows below is still not an incident, and a `blocked` ping
# pauses it while the agent waits on a human.
resource "lastping_monitor" "research_agent" {
  name          = "Research agent"
  slug          = "research-agent"
  schedule_kind = "on_demand"
  grace_s       = 300

  # A run may take four hours. Silence between runs may last thirty minutes.
  # The two clocks are independent, which is why both attributes exist.
  max_runtime_s  = 14400
  expect_every_s = 1800
}
