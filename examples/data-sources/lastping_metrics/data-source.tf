# The project's monitors as Prometheus gauges, exactly as
# GET /api/v1/metrics returns them.
data "lastping_metrics" "current" {}

# Snapshot the exposition to disk, for a scraper that reads from a file rather
# than over HTTP.
resource "local_file" "metrics_snapshot" {
  filename = "${path.module}/lastping.prom"
  content  = data.lastping_metrics.current.text
}

output "monitor_count" {
  # lastping_checks is the one unlabelled gauge: the number of monitors.
  value = tonumber(
    regex("(?m)^lastping_checks (\\d+)$", data.lastping_metrics.current.text)[0]
  )
}

# The body is not parsed into attributes on purpose: the metric set is
# documented to grow, and projecting today's names onto a Terraform schema
# would turn every server-side addition into a provider release.
#
# It also changes as monitors ping, so a configuration that interpolates it
# produces a diff on nearly every plan. Prefer a real Prometheus scrape for
# ongoing monitoring; use this for one-off snapshots and bootstrapping.
