package provider

import (
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// dataMonitorFixture is a monitor with every attribute the data source reports
// set to something distinctive, so a read-back of the wrong field is visible
// rather than a coincidental match on a default.
const dataMonitorFixture = `
resource "lastping_monitor" "src" {
  name            = "acc-data-monitor"
  slug            = "acc-data-monitor"
  schedule_kind   = "cron"
  cron_expr       = "0 3 * * *"
  tz              = "Europe/Berlin"
  grace_s         = 1800
  tags            = ["acc:data-monitor", "env:test"]
  runaway_ceiling = 42
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
					resource.TestCheckResourceAttrPair(
						"data.lastping_monitor.by_slug", "runaway_ceiling", "lastping_monitor.src", "runaway_ceiling"),
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
