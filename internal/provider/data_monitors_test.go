package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// dataMonitorsFixture creates two monitors that differ only in their tags, so
// the tag filter has something to get wrong: a filter that is ignored would
// return both, and one that matches too loosely would too.
const dataMonitorsFixture = `
resource "lastping_monitor" "tagged" {
  name          = "acc-data-monitors-tagged"
  slug          = "acc-data-monitors-tagged"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
  tags          = ["acc:monitors-filter"]
}

resource "lastping_monitor" "untagged" {
  name          = "acc-data-monitors-untagged"
  slug          = "acc-data-monitors-untagged"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
}
`

// TestAccDataMonitors_tagFilter reads back a monitor the same configuration
// created, through the list data source, and checks the filter actually
// narrows: the tagged monitor is the only element, and the untagged one that
// exists in the same project at the same time is absent.
func TestAccDataMonitors_tagFilter(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: dataMonitorsFixture + `
data "lastping_monitors" "filtered" {
  tag        = "acc:monitors-filter"
  depends_on = [lastping_monitor.tagged, lastping_monitor.untagged]
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(
						"data.lastping_monitors.filtered", "monitors.#", "1"),
					resource.TestCheckResourceAttrPair(
						"data.lastping_monitors.filtered", "monitors.0.id",
						"lastping_monitor.tagged", "id"),
					resource.TestCheckResourceAttrPair(
						"data.lastping_monitors.filtered", "monitors.0.slug",
						"lastping_monitor.tagged", "slug"),
					resource.TestCheckResourceAttr(
						"data.lastping_monitors.filtered", "monitors.0.period_s", "3600"),
					resource.TestCheckResourceAttr(
						"data.lastping_monitors.filtered", "monitors.0.tags.#", "1"),
					// The untagged monitor has no tags, and the nested element
					// reports that as an empty set rather than null — the same
					// convention the single-monitor data source uses.
					resource.TestCheckResourceAttr(
						"data.lastping_monitors.filtered", "tag", "acc:monitors-filter"),
				),
			},
			{
				// No filter: both monitors are in the project, so both come
				// back. The project may hold monitors from other tests, so the
				// assertion is membership rather than a count.
				Config: dataMonitorsFixture + `
data "lastping_monitors" "all" {
  depends_on = [lastping_monitor.tagged, lastping_monitor.untagged]
}

locals {
  acc_all_slugs = [for m in data.lastping_monitors.all.monitors : m.slug]
}

output "acc_tagged_present" {
  value = tostring(contains(local.acc_all_slugs, "acc-data-monitors-tagged"))
}

output "acc_untagged_present" {
  value = tostring(contains(local.acc_all_slugs, "acc-data-monitors-untagged"))
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr("data.lastping_monitors.all", "tag"),
					resource.TestCheckOutput("acc_tagged_present", "true"),
					resource.TestCheckOutput("acc_untagged_present", "true"),
				),
			},
		},
	})
}

// TestAccDataMonitors_noMatchIsEmptyNotAnError pins the contract that an empty
// result set is a legitimate answer. A configuration that builds a status page
// from a tag must be able to plan before anything carries the tag.
func TestAccDataMonitors_noMatchIsEmptyNotAnError(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "lastping_monitors" "none" {
  tag = "acc:no-such-tag-exists"
}`,
				Check: resource.TestCheckResourceAttr(
					"data.lastping_monitors.none", "monitors.#", "0"),
			},
		},
	})
}
