# Every monitor in the project the API key belongs to.
data "lastping_monitors" "all" {}

# Or just the ones carrying a tag. Matching is exact — "agent" does not match
# "agent:claude".
data "lastping_monitors" "production" {
  tag = "env:prod"
}

# Build a status page from a tag instead of a hand-maintained list of ids: new
# monitors tagged env:prod join the page on the next apply, with no edit here.
resource "lastping_status_page" "production" {
  slug       = "production"
  title      = "Production"
  visibility = "public"
  check_ids  = [for m in data.lastping_monitors.production.monitors : m.id]
}

# An empty result is not an error, so an audit output is safe to write even
# before anything is tagged.
output "untagged_monitors" {
  value = [
    for m in data.lastping_monitors.all.monitors : m.name
    if length(m.tags) == 0
  ]
}

output "paused_monitors" {
  value = [for m in data.lastping_monitors.all.monitors : m.slug if m.paused]
}
