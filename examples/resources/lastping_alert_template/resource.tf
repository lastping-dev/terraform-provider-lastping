# One resource owns a monitor's whole template map. The API's PUT replaces the
# set rather than merging into it, so splitting these across two resources would
# make them clobber each other on every apply.

resource "lastping_monitor" "checkout_api" {
  name          = "Checkout API"
  slug          = "checkout-api"
  schedule_kind = "simple"
  period_s      = 300
  grace_s       = 120
}

resource "lastping_alert_template" "checkout_api" {
  monitor_id = lastping_monitor.checkout_api.id

  templates = {
    # An event type on its own covers every cause of that event.
    "down"     = "{check_name} is DOWN. Last ping {last_ping}, expected {schedule}. {incident_url}"
    "recovery" = "{check_name} recovered."

    # "event/cause" narrows it. The more specific key wins when both are present,
    # so this one is what actually fires when the monitor simply went quiet.
    "down/silence" = "No ping from {check_name} since {last_ping}. Check the cron host before escalating."

    # Runaway means the monitor pinged far more often than its ceiling allows —
    # usually a retry loop rather than an outage.
    "fail/runaway" = "{check_name} is pinging in a loop ({cause}). {incident_url}"

    # "started" and "success" are informational rather than state changes. A
    # template only decides the wording — these reach someone only if a
    # lastping_route sends that event type to a destination.
    "started" = "{check_name} started."
    "success" = "{check_name} finished in {duration}."
  }
}

# Removing a key from the map removes the message server-side and restores the
# default wording; destroying the resource clears all of them. Neither touches
# the monitor itself.
