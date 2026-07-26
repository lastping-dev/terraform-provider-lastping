package provider

import (
	"fmt"
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

// TestAccMonitor_httpProbe covers monitor_type = "http". The API floors the
// effective grace to 2*probe_interval_s (api/checks.go: createHTTPCheckFromValidated),
// so a configuration that asks for less must still apply: with grace_s Required
// (non-computed) Terraform rejects the server's larger value as an inconsistent
// result after apply, which made http monitors impossible to create at all.
func TestAccMonitor_httpProbe(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// grace_s omitted: the server derives 2*probe_interval_s = 600.
				Config: `
resource "lastping_monitor" "probe" {
  name             = "acc-http"
  slug             = "acc-http"
  monitor_type     = "http"
  probe_url        = "https://example.com/"
  probe_interval_s = 300
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_monitor.probe", "monitor_type", "http"),
					resource.TestCheckResourceAttr("lastping_monitor.probe", "probe_interval_s", "300"),
					// Server floor, not a configured value.
					resource.TestCheckResourceAttr("lastping_monitor.probe", "grace_s", "600"),
					// Derived server-side from probe_interval_s.
					resource.TestCheckResourceAttr("lastping_monitor.probe", "schedule_kind", "simple"),
					resource.TestCheckResourceAttr("lastping_monitor.probe", "period_s", "300"),
					resource.TestCheckResourceAttr("lastping_monitor.probe", "probe_timeout_s", "10"),
				),
			},
			{
				// A grace_s above the floor is honoured as-is.
				Config: `
resource "lastping_monitor" "probe" {
  name             = "acc-http"
  slug             = "acc-http"
  monitor_type     = "http"
  probe_url        = "https://example.com/"
  probe_interval_s = 300
  grace_s          = 900
}`,
				Check: resource.TestCheckResourceAttr("lastping_monitor.probe", "grace_s", "900"),
			},
			{
				ResourceName:      "lastping_monitor.probe",
				ImportState:       true,
				ImportStateId:     "acc-http",
				ImportStateVerify: true,
			},
		},
	})
}

// TestAccMonitor_httpProbeGraceBelowFloor asserts the below-the-floor case is
// rejected at plan time with an actionable message rather than blowing up during
// apply with "inconsistent result after apply".
func TestAccMonitor_httpProbeGraceBelowFloor(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: `
resource "lastping_monitor" "probe" {
  name             = "acc-http-floor"
  slug             = "acc-http-floor"
  monitor_type     = "http"
  probe_url        = "https://example.com/"
  probe_interval_s = 300
  grace_s          = 120
}`,
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(`grace_s.*(floor|at least 600)`),
		}},
	})
}

// TestAccMonitor_monitorFromRoundTrips pins the offset-timestamp round trip: the
// API stores and returns UTC, so a configured +01:00 timestamp comes back as Z.
// The provider must treat the two as the same instant instead of reporting an
// inconsistent result after apply.
func TestAccMonitor_monitorFromRoundTrips(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_monitor" "mf" {
  name          = "acc-monitor-from"
  slug          = "acc-monitor-from"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
  monitor_from  = "2027-01-01T00:00:00+01:00"
}`,
				Check: resource.TestCheckResourceAttr(
					"lastping_monitor.mf", "monitor_from", "2027-01-01T00:00:00+01:00"),
			},
			{
				// And it stays stable: no perpetual diff on re-plan.
				Config: `
resource "lastping_monitor" "mf" {
  name          = "acc-monitor-from"
  slug          = "acc-monitor-from"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
  monitor_from  = "2027-01-01T00:00:00+01:00"
}`,
				PlanOnly: true,
			},
		},
	})
}

// TestAccMonitor_tagsCanBeRemoved is the "Optional + Computed makes attributes
// unsettable" regression: dropping tags from the configuration must clear them,
// not leave the previous value pinned in state forever.
func TestAccMonitor_tagsCanBeRemoved(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_monitor" "tagged" {
  name          = "acc-tags"
  slug          = "acc-tags"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
  tags          = ["env:prod"]
  cron_expr     = null
}`,
				Check: resource.TestCheckResourceAttr("lastping_monitor.tagged", "tags.#", "1"),
			},
			{
				Config: `
resource "lastping_monitor" "tagged" {
  name          = "acc-tags"
  slug          = "acc-tags"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr("lastping_monitor.tagged", "tags.#"),
					resource.TestCheckNoResourceAttr("lastping_monitor.tagged", "tags.0"),
				),
			},
			{
				// And the server really did drop them.
				PlanOnly: true,
				Config: `
resource "lastping_monitor" "tagged" {
  name          = "acc-tags"
  slug          = "acc-tags"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
}`,
			},
		},
	})
}

// TestAccMonitor_pause exercises the update path where the only change is
// `paused`: it maps onto the pause/resume endpoints and must not also PATCH the
// monitor's configuration.
func TestAccMonitor_pause(t *testing.T) {
	const cfg = `
resource "lastping_monitor" "p" {
  name          = "acc-pause"
  slug          = "acc-pause"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
  paused        = %s
}`
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(cfg, "false"),
				Check:  resource.TestCheckResourceAttr("lastping_monitor.p", "paused", "false"),
			},
			{
				Config: fmt.Sprintf(cfg, "true"),
				Check:  resource.TestCheckResourceAttr("lastping_monitor.p", "paused", "true"),
			},
			{
				Config: fmt.Sprintf(cfg, "false"),
				Check:  resource.TestCheckResourceAttr("lastping_monitor.p", "paused", "false"),
			},
		},
	})
}

// TestAccMonitor_slugMustBeNormalised: the server normalises (trim + lowercase)
// and rejects anything outside ^[a-z0-9][a-z0-9-]{1,48}[a-z0-9]$, so a
// non-normalised slug must fail at plan time rather than plan to one value and
// apply to another.
func TestAccMonitor_slugMustBeNormalised(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: `
resource "lastping_monitor" "bad" {
  name          = "acc-bad-slug"
  slug          = "  Rev-Case-Slug  "
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
}`,
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(`(?s)slug.*lowercase`),
		}},
	})
}

// TestAccMonitor_slugCollisionFails is the H1 guarantee: creating a monitor whose
// slug already exists must error, never silently adopt.
func TestAccMonitor_slugCollisionFails(t *testing.T) {
	const firstOnly = `
resource "lastping_monitor" "first" {
  name          = "acc-collide"
  slug          = "acc-collide"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
}`
	var firstID string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: firstOnly,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrWith("lastping_monitor.first", "id", func(v string) error {
						firstID = v
						return nil
					}),
				),
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
			{
				// The failed create must not have touched the monitor that was
				// already there: same id (not recreated, not adopted) and the
				// same configuration, read back from the API.
				Config: firstOnly,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrWith("lastping_monitor.first", "id", func(v string) error {
						if v != firstID {
							return fmt.Errorf("first monitor id changed: was %s, now %s", firstID, v)
						}
						return nil
					}),
					resource.TestCheckResourceAttr("lastping_monitor.first", "name", "acc-collide"),
					resource.TestCheckResourceAttr("lastping_monitor.first", "period_s", "3600"),
					resource.TestCheckResourceAttr("lastping_monitor.first", "grace_s", "300"),
				),
			},
		},
	})
}
