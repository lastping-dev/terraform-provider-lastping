package provider

import (
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// dataMonitorFixture is a monitor with every attribute the data source reports
// set to something distinctive, so a read-back of the wrong field is visible
// rather than a coincidental match on a default.
// max_runtime_s, step_timeout_s and failure_threshold carry non-default values
// for the same reason as the rest: failure_threshold is 1 on every monitor that
// never sets it, and max_runtime_s and step_timeout_s are unset by default, so
// pairing them against a fixture that omitted them would pass even if the data
// source read the wrong field.
const dataMonitorFixture = `
resource "lastping_monitor" "src" {
  name              = "acc-data-monitor"
  slug              = "acc-data-monitor"
  schedule_kind     = "cron"
  cron_expr         = "0 3 * * *"
  tz                = "Europe/Berlin"
  grace_s           = 1800
  max_runtime_s     = 14400
  step_timeout_s    = 900
  failure_threshold = 3
  tags              = ["acc:data-monitor", "env:test"]
  runaway_ceiling   = 42
}
`

// TestAccDataMonitor_bySlugMatchesTheResource reads back, through the data
// source, the monitor the same configuration just created.
//
// Comparing attribute-for-attribute against the resource is the point: it
// proves the two agree on every field NAME (a typo would read as null) and on
// every representation (the resource's empty-string-to-null convention has to
// hold in the data source too, or `data.x.cron_expr` and `x.cron_expr` would
// not be comparable values).
func TestAccDataMonitor_bySlugMatchesTheResource(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: dataMonitorFixture + `
data "lastping_monitor" "by_slug" {
  slug       = lastping_monitor.src.slug
  depends_on = [lastping_monitor.src]
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair(
						"data.lastping_monitor.by_slug", "id", "lastping_monitor.src", "id"),
					resource.TestCheckResourceAttrPair(
						"data.lastping_monitor.by_slug", "name", "lastping_monitor.src", "name"),
					resource.TestCheckResourceAttrPair(
						"data.lastping_monitor.by_slug", "slug", "lastping_monitor.src", "slug"),
					resource.TestCheckResourceAttrPair(
						"data.lastping_monitor.by_slug", "monitor_type", "lastping_monitor.src", "monitor_type"),
					resource.TestCheckResourceAttrPair(
						"data.lastping_monitor.by_slug", "schedule_kind", "lastping_monitor.src", "schedule_kind"),
					resource.TestCheckResourceAttrPair(
						"data.lastping_monitor.by_slug", "cron_expr", "lastping_monitor.src", "cron_expr"),
					resource.TestCheckResourceAttrPair(
						"data.lastping_monitor.by_slug", "tz", "lastping_monitor.src", "tz"),
					resource.TestCheckResourceAttrPair(
						"data.lastping_monitor.by_slug", "grace_s", "lastping_monitor.src", "grace_s"),
					// period_s, probe_timeout_s and probe_expected_status are all
					// inapplicable to this cron monitor, so the API reports 0 for
					// each. Pairing them here pins down that both surfaces read
					// back that same concrete 0 rather than one reporting null —
					// see the int64OrNull-vs-Int64Value split in
					// monitorDataFromAPI / modelFromMonitor.
					resource.TestCheckResourceAttrPair(
						"data.lastping_monitor.by_slug", "period_s", "lastping_monitor.src", "period_s"),
					resource.TestCheckResourceAttrPair(
						"data.lastping_monitor.by_slug", "probe_timeout_s", "lastping_monitor.src", "probe_timeout_s"),
					resource.TestCheckResourceAttrPair(
						"data.lastping_monitor.by_slug", "probe_expected_status", "lastping_monitor.src", "probe_expected_status"),
					resource.TestCheckResourceAttrPair(
						"data.lastping_monitor.by_slug", "runaway_ceiling", "lastping_monitor.src", "runaway_ceiling"),
					// The two detection attributes. These pairs prove the field
					// NAMES match and that a set value round-trips identically
					// on both surfaces — a typo on either side reads as null and
					// fails here.
					//
					// They do NOT prove the null-versus-zero convention holds.
					// The fixture sets failure_threshold to 3, and the backend
					// can never answer 0 for it, so a data source that mapped it
					// through int64OrNull would agree on every monitor that can
					// exist and this pair would still pass. That case is pinned
					// in TestMonitorSurfacesAgreeOnEmptyValues, which builds the
					// empty response by hand precisely because a live backend
					// cannot produce it.
					resource.TestCheckResourceAttrPair(
						"data.lastping_monitor.by_slug", "failure_threshold",
						"lastping_monitor.src", "failure_threshold"),
					resource.TestCheckResourceAttrPair(
						"data.lastping_monitor.by_slug", "max_runtime_s",
						"lastping_monitor.src", "max_runtime_s"),
					resource.TestCheckResourceAttrPair(
						"data.lastping_monitor.by_slug", "step_timeout_s",
						"lastping_monitor.src", "step_timeout_s"),
					// Pinned as a literal as well as a pair, because the pair
					// alone passes vacuously if the fixture ever stops setting
					// step_timeout_s: both surfaces would then read null and
					// agree. Every monitor that does not ask for the attribute
					// leaves it unset, so that is a live way for this pair to
					// quietly stop testing anything — mutating the fixture to
					// drop it leaves the pair green and fails only here.
					resource.TestCheckResourceAttr(
						"data.lastping_monitor.by_slug", "step_timeout_s", "900"),
					resource.TestCheckResourceAttrPair(
						"data.lastping_monitor.by_slug", "paused", "lastping_monitor.src", "paused"),
					resource.TestCheckResourceAttrPair(
						"data.lastping_monitor.by_slug", "ping_url", "lastping_monitor.src", "ping_url"),
					resource.TestCheckResourceAttrPair(
						"data.lastping_monitor.by_slug", "created_at", "lastping_monitor.src", "created_at"),

					resource.TestCheckResourceAttr(
						"data.lastping_monitor.by_slug", "tags.#", "2"),
					resource.TestCheckTypeSetElemAttr(
						"data.lastping_monitor.by_slug", "tags.*", "acc:data-monitor"),
					resource.TestCheckTypeSetElemAttr(
						"data.lastping_monitor.by_slug", "tags.*", "env:test"),

					// Never pinged, so the API omits these and the data source
					// reports null rather than "".
					resource.TestCheckNoResourceAttr(
						"data.lastping_monitor.by_slug", "last_ping_at"),
					resource.TestCheckResourceAttr(
						"data.lastping_monitor.by_slug", "status", "new"),
					// probe_* belong to http monitors; a cron monitor leaves them unset.
					resource.TestCheckNoResourceAttr(
						"data.lastping_monitor.by_slug", "probe_url"),
					// The fixture attaches no agent, so both surfaces must agree on
					// null rather than one reporting "" — the same stringOrNull
					// convention pinned above for cron_expr/probe_url.
					resource.TestCheckNoResourceAttr(
						"data.lastping_monitor.by_slug", "agent_id"),
					resource.TestCheckNoResourceAttr(
						"lastping_monitor.src", "agent_id"),
				),
			},
		},
	})
}

// TestAccDataMonitor_byID covers the other lookup argument, and asserts the
// slug is filled in from the response rather than left null — the reason id and
// slug are Optional+Computed and not plain Optional.
func TestAccDataMonitor_byID(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: dataMonitorFixture + `
data "lastping_monitor" "by_id" {
  id = lastping_monitor.src.id
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(
						"data.lastping_monitor.by_id", "slug", "acc-data-monitor"),
					resource.TestCheckResourceAttr(
						"data.lastping_monitor.by_id", "name", "acc-data-monitor"),
				),
			},
		},
	})
}

// TestAccDataMonitor_requiresExactlyOneLookupArgument proves the ExactlyOneOf
// validator fires at plan time in both failing directions, so neither an
// unanswerable query nor a self-contradicting one reaches the API.
func TestAccDataMonitor_requiresExactlyOneLookupArgument(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `data "lastping_monitor" "none" {}`,
				ExpectError: regexp.MustCompile(
					`(?s)Exactly one of these attributes must be configured: \[id,slug\]`),
			},
			{
				Config: `
data "lastping_monitor" "both" {
  id   = "550e8400-e29b-41d4-a716-446655440000"
  slug = "acc-data-monitor"
}`,
				ExpectError: regexp.MustCompile(
					`(?s)Exactly one of these attributes must be configured: \[id,slug\]`),
			},
		},
	})
}

// TestAccDataMonitor_missingSlugIsAnError checks that a lookup for something
// that is not there fails the plan rather than yielding an empty object that
// would interpolate as null halfway down a configuration.
func TestAccDataMonitor_missingSlugIsAnError(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "lastping_monitor" "missing" {
  slug = "acc-no-such-monitor-anywhere"
}`,
				ExpectError: regexp.MustCompile(`(?s)Unable to read monitor.*acc-no-such-monitor-anywhere`),
			},
		},
	})
}
