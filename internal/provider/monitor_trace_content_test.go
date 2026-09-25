package provider

import (
	"context"
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// Acceptance tests for trace_content and for a bound ingest key. Like every
// TestAcc* in this package they skip unless LASTPING_API_KEY is set, and they
// need a backend that serves trace_content and the ingest scope.

// testAccCheckServerTraceContent reads trace_content off the server, not out of
// state: a PATCH that never carried the key would leave state agreeing with the
// plan while the row still held the old value.
func testAccCheckServerTraceContent(t *testing.T, name, want string) resource.TestCheckFunc {
	return resource.TestCheckResourceAttrWith(name, "id", func(id string) error {
		mon, err := testAccDirectClient(t).GetMonitor(context.Background(), id)
		if err != nil {
			return err
		}
		if mon.TraceContent != want {
			return fmt.Errorf("server holds trace_content=%q, want %q", mon.TraceContent, want)
		}
		return nil
	})
}

// TestAccMonitor_traceContent: set redacted, read it back from the server,
// change it in place, and import it.
func TestAccMonitor_traceContent(t *testing.T) {
	config := func(v string) string {
		return fmt.Sprintf(`
resource "lastping_monitor" "tc" {
  name          = "acc-trace-content"
  slug          = "acc-trace-content"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
  trace_content = %q
}`, v)
	}

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config("redacted"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_monitor.tc", "trace_content", "redacted"),
					testAccCheckServerTraceContent(t, "lastping_monitor.tc", "redacted"),
				),
			},
			{
				Config: config("dropped"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("lastping_monitor.tc", plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_monitor.tc", "trace_content", "dropped"),
					testAccCheckServerTraceContent(t, "lastping_monitor.tc", "dropped"),
				),
			},
			{
				ResourceName:      "lastping_monitor.tc",
				ImportState:       true,
				ImportStateId:     "acc-trace-content",
				ImportStateVerify: true,
			},
		},
	})
}

// TestAccMonitor_traceContentOmitted: a monitor that never sets trace_content
// reads the server's default, plans nothing on the next run, imports as
// dropped, and removing a configured redacted leaves the stored value alone.
func TestAccMonitor_traceContentOmitted(t *testing.T) {
	const omitted = `
resource "lastping_monitor" "tco" {
  name          = "acc-trace-content-omitted"
  slug          = "acc-trace-content-omitted"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
}`

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: omitted,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_monitor.tco", "trace_content", "dropped"),
					testAccCheckServerTraceContent(t, "lastping_monitor.tco", "dropped"),
				),
			},
			{
				// The server-supplied value must not read as drift.
				Config:   omitted,
				PlanOnly: true,
			},
			{
				// Import a monitor whose configuration never set the value:
				// it comes in as dropped.
				ResourceName:  "lastping_monitor.tco",
				ImportState:   true,
				ImportStateId: "acc-trace-content-omitted",
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if len(states) != 1 {
						return fmt.Errorf("imported %d instances, want 1", len(states))
					}
					if got := states[0].Attributes["trace_content"]; got != "dropped" {
						return fmt.Errorf("imported trace_content=%q, want dropped", got)
					}
					return nil
				},
			},
			{
				Config: `
resource "lastping_monitor" "tco" {
  name          = "acc-trace-content-omitted"
  slug          = "acc-trace-content-omitted"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
  trace_content = "redacted"
}`,
				Check: testAccCheckServerTraceContent(t, "lastping_monitor.tco", "redacted"),
			},
			{
				// Removing the line keeps what the monitor has: no plan, and
				// the server still holds redacted.
				Config: omitted,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: testAccCheckServerTraceContent(t, "lastping_monitor.tco", "redacted"),
			},
		},
	})
}

// TestAccMonitor_traceContentInvalid: a value the API does not accept is
// refused at plan time.
func TestAccMonitor_traceContentInvalid(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: `
resource "lastping_monitor" "bad" {
  name          = "acc-trace-content-bad"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
  trace_content = "kept"
}`,
			ExpectError: regexp.MustCompile(`trace_content`),
		}},
	})
}

// TestAccAPIKey_ingestBoundToMonitor mints an ingest key bound to a monitor,
// reads the binding back from the server, and refuses a binding on a write key
// at plan time.
func TestAccAPIKey_ingestBoundToMonitor(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			// The refusal first: the post-test destroy runs the LAST step's
			// configuration, and this one cannot even validate.
			{
				ExpectError: regexp.MustCompile(`check_id needs scope`),
				Config: `
resource "lastping_api_key" "wrong" {
  name     = "acc-ingest-wrong"
  scope    = "write"
  check_id = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
}`,
				PlanOnly: true,
			},
			{
				Config: `
resource "lastping_monitor" "traced" {
  name          = "acc-ingest-bound"
  slug          = "acc-ingest-bound"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
}

resource "lastping_api_key" "tracing" {
  name     = "acc-ingest-bound"
  scope    = "ingest"
  check_id = lastping_monitor.traced.id
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_api_key.tracing", "scope", "ingest"),
					resource.TestCheckResourceAttrPair("lastping_api_key.tracing", "check_id",
						"lastping_monitor.traced", "id"),
					testAccCheckServerScope(t, "lastping_api_key.tracing", "ingest"),
					// The server's binding, read off the server rather than out
					// of state, must be this monitor exactly: a key bound to
					// any other monitor, or to none, fails here.
					func(s *terraform.State) error {
						key, ok := s.RootModule().Resources["lastping_api_key.tracing"]
						if !ok {
							return fmt.Errorf("lastping_api_key.tracing not in state")
						}
						mon, ok := s.RootModule().Resources["lastping_monitor.traced"]
						if !ok {
							return fmt.Errorf("lastping_monitor.traced not in state")
						}
						k, err := testAccDirectClient(t).GetAPIKey(context.Background(), key.Primary.ID)
						if err != nil {
							return err
						}
						if k.CheckID == nil || *k.CheckID != mon.Primary.ID {
							got := "none"
							if k.CheckID != nil {
								got = *k.CheckID
							}
							return fmt.Errorf("server binds key %s to monitor %s, want %s", key.Primary.ID, got, mon.Primary.ID)
						}
						return nil
					},
				),
			},
		},
	})
}
