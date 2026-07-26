package provider

import (
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// TestAccDataIncidents_readsBackTheMonitorItWasGiven reads incidents for a
// monitor the same configuration just created.
//
// A brand-new monitor has no incidents, so the assertion is that the query
// succeeds and returns an empty list: an empty history is a real answer, and a
// data source that errored on it would break every configuration that reports
// on a healthy monitor. It also proves monitor_id is wired to the right path
// segment — a wrong id is a 404, not an empty list.
func TestAccDataIncidents_readsBackTheMonitorItWasGiven(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_monitor" "src" {
  name          = "acc-data-incidents"
  slug          = "acc-data-incidents"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
}

data "lastping_incidents" "src" {
  monitor_id = lastping_monitor.src.id
  limit      = 10
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair(
						"data.lastping_incidents.src", "monitor_id", "lastping_monitor.src", "id"),
					resource.TestCheckResourceAttr("data.lastping_incidents.src", "limit", "10"),
					resource.TestCheckResourceAttr("data.lastping_incidents.src", "incidents.#", "0"),
				),
			},
			{
				// limit is optional: omitting it takes the server default.
				Config: `
resource "lastping_monitor" "src" {
  name          = "acc-data-incidents"
  slug          = "acc-data-incidents"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
}

data "lastping_incidents" "no_limit" {
  monitor_id = lastping_monitor.src.id
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr("data.lastping_incidents.no_limit", "limit"),
					resource.TestCheckResourceAttr("data.lastping_incidents.no_limit", "incidents.#", "0"),
				),
			},
		},
	})
}

// TestAccDataIncidents_unknownMonitorIsAnError checks that a monitor outside
// the caller's project (or one that never existed) fails rather than reporting
// an empty history — the API deliberately answers 404 in both cases so it never
// becomes an existence oracle, and the provider must not soften that into
// "no incidents".
func TestAccDataIncidents_unknownMonitorIsAnError(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "lastping_incidents" "missing" {
  monitor_id = "550e8400-e29b-41d4-a716-446655440000"
}`,
				ExpectError: regexp.MustCompile(`(?s)Unable to read incidents`),
			},
		},
	})
}
