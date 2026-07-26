package provider

import (
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// dataDestinationFixture uses kind = "ntfy" so `target` carries a real,
// checkable value: ntfy's topic URL is not a credential, so the API returns it
// verbatim instead of a fixed label.
const dataDestinationFixture = `
resource "lastping_destination" "src" {
  kind      = "ntfy"
  name      = "acc-data-destination"
  topic_url = "https://ntfy.sh/acc-data-destination"
}
`

// TestAccDataDestination_byNameMatchesTheResource reads back the destination
// the same configuration created, and compares it attribute-for-attribute
// against the resource.
func TestAccDataDestination_byNameMatchesTheResource(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: dataDestinationFixture + `
data "lastping_destination" "by_name" {
  name       = lastping_destination.src.name
  depends_on = [lastping_destination.src]
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair(
						"data.lastping_destination.by_name", "id", "lastping_destination.src", "id"),
					resource.TestCheckResourceAttrPair(
						"data.lastping_destination.by_name", "kind", "lastping_destination.src", "kind"),
					resource.TestCheckResourceAttrPair(
						"data.lastping_destination.by_name", "name", "lastping_destination.src", "name"),
					resource.TestCheckResourceAttrPair(
						"data.lastping_destination.by_name", "target", "lastping_destination.src", "target"),
					resource.TestCheckResourceAttrPair(
						"data.lastping_destination.by_name", "verified", "lastping_destination.src", "verified"),
					resource.TestCheckResourceAttrPair(
						"data.lastping_destination.by_name", "created_at", "lastping_destination.src", "created_at"),

					resource.TestCheckResourceAttr("data.lastping_destination.by_name",
						"target", "https://ntfy.sh/acc-data-destination"),
					// Every kind except email is verified on creation.
					resource.TestCheckResourceAttr("data.lastping_destination.by_name", "verified", "true"),
					resource.TestCheckResourceAttr("data.lastping_destination.by_name", "disabled", "false"),
					resource.TestCheckNoResourceAttr("data.lastping_destination.by_name", "disable_reason"),
				),
			},
			{
				Config: dataDestinationFixture + `
data "lastping_destination" "by_id" {
  id = lastping_destination.src.id
}`,
				Check: resource.TestCheckResourceAttr(
					"data.lastping_destination.by_id", "name", "acc-data-destination"),
			},
		},
	})
}

// TestAccDataDestination_ambiguousNameIsAnError is the reason a name lookup is
// allowed to fail. Destination names are not unique, so resolving one
// arbitrarily would silently point a route at the wrong channel — a failure
// nobody would notice until an alert went to the wrong place.
func TestAccDataDestination_ambiguousNameIsAnError(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_destination" "twin_a" {
  kind      = "ntfy"
  name      = "acc-data-destination-twin"
  topic_url = "https://ntfy.sh/acc-twin-a"
}

resource "lastping_destination" "twin_b" {
  kind      = "ntfy"
  name      = "acc-data-destination-twin"
  topic_url = "https://ntfy.sh/acc-twin-b"
}

data "lastping_destination" "ambiguous" {
  name       = "acc-data-destination-twin"
  depends_on = [lastping_destination.twin_a, lastping_destination.twin_b]
}`,
				ExpectError: regexp.MustCompile(
					`(?s)2 destinations are named "acc-data-destination-twin".*look it up by id instead`),
			},
		},
	})
}

// TestAccDataDestination_requiresExactlyOneLookupArgument mirrors the monitor
// data source: neither argument is an unanswerable query, both could disagree.
func TestAccDataDestination_requiresExactlyOneLookupArgument(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `data "lastping_destination" "none" {}`,
				ExpectError: regexp.MustCompile(
					`(?s)Exactly one of these attributes must be configured: \[id,name\]`),
			},
			{
				Config: `
data "lastping_destination" "both" {
  id   = "c1d2e3f4-0000-0000-0000-000000000001"
  name = "acc-data-destination"
}`,
				ExpectError: regexp.MustCompile(
					`(?s)Exactly one of these attributes must be configured: \[id,name\]`),
			},
		},
	})
}
