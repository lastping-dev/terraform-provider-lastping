package provider

import (
	"os"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// testAccPreCheck skips acceptance tests unless a backend is configured. These
// run from the monorepo CI against docker-compose, not from public CI.
func testAccPreCheck(t *testing.T) {
	t.Helper()
	if os.Getenv("LASTPING_API_KEY") == "" {
		t.Skip("LASTPING_API_KEY not set; skipping acceptance test")
	}
}

func TestAccMonitor_basic(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_monitor" "test" {
  name          = "acc-basic"
  slug          = "acc-basic"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
  tags          = ["acc:test"]
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_monitor.test", "name", "acc-basic"),
					resource.TestCheckResourceAttr("lastping_monitor.test", "period_s", "3600"),
					resource.TestCheckResourceAttrSet("lastping_monitor.test", "id"),
					resource.TestCheckResourceAttrSet("lastping_monitor.test", "ping_url"),
				),
			},
			{
				// Update in place — must not force replacement.
				Config: `
resource "lastping_monitor" "test" {
  name          = "acc-basic-renamed"
  slug          = "acc-basic"
  schedule_kind = "simple"
  period_s      = 7200
  grace_s       = 300
  tags          = ["acc:test"]
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_monitor.test", "name", "acc-basic-renamed"),
					resource.TestCheckResourceAttr("lastping_monitor.test", "period_s", "7200"),
				),
			},
			{
				ResourceName:      "lastping_monitor.test",
				ImportState:       true,
				ImportStateId:     "acc-basic",
				ImportStateVerify: true,
			},
		},
	})
}

func TestAccMonitor_cron(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			// grace_s is required by the API (validated [60, 31536000]); it is
			// included here even though early drafts of this test omitted it.
			Config: `
resource "lastping_monitor" "cron" {
  name          = "acc-cron"
  slug          = "acc-cron"
  schedule_kind = "cron"
  cron_expr     = "0 3 * * *"
  tz            = "Europe/London"
  grace_s       = 300
}`,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("lastping_monitor.cron", "cron_expr", "0 3 * * *"),
				resource.TestCheckResourceAttr("lastping_monitor.cron", "tz", "Europe/London"),
			),
		}},
	})
}

// TestAccMonitor_slugCollisionFails is the H1 guarantee: creating a monitor whose
// slug already exists must error, never silently adopt.
func TestAccMonitor_slugCollisionFails(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_monitor" "first" {
  name          = "acc-collide"
  slug          = "acc-collide"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
}`,
			},
			{
				Config: `
resource "lastping_monitor" "first" {
  name          = "acc-collide"
  slug          = "acc-collide"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
}

resource "lastping_monitor" "second" {
  name          = "acc-collide-two"
  slug          = "acc-collide"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
}`,
				ExpectError: regexp.MustCompile(`already exists|terraform import`),
			},
		},
	})
}
