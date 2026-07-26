package provider

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
)

// templateMonitor is the monitor every alert-template test hangs its messages
// off. Templates have no lifecycle of their own: they exist only as part of a
// monitor.
const templateMonitor = `
resource "lastping_monitor" "m" {
  name          = "acc-templates"
  slug          = "acc-templates"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
}
`

// checkTemplatesOnServer asserts the monitor's stored template set out-of-band,
// through the API rather than through Terraform state. That is the only way to
// catch the failure this resource is built around: a key dropped from the map
// that the provider never actually deleted would still look correct in state.
func checkTemplatesOnServer(t *testing.T, want map[string]string) resource.TestCheckFunc {
	t.Helper()
	return resource.TestCheckResourceAttrWith("lastping_alert_template.t", "monitor_id",
		func(monitorID string) error {
			got, err := testAccDirectClient(t).GetTemplates(t.Context(), monitorID)
			if err != nil {
				return err
			}
			if len(got) != len(want) {
				return fmt.Errorf("server holds %d templates %v, want %d %v", len(got), got, len(want), want)
			}
			for k, v := range want {
				if got[k] != v {
					return fmt.Errorf("server template %q is %q, want %q", k, got[k], v)
				}
			}
			return nil
		})
}

// TestAccAlertTemplate_removingAKeyRemovesItServerSide is the test that earns
// this resource its shape.
//
// The API's PUT is only half a replace: event-wide keys are wiped and reapplied,
// but a per-cause key such as `down/silence` is touched only when the request
// names it (api/api_templates.go: handleAPIPutTemplates). A provider that simply
// sent the configured map would leave a removed per-cause override in place
// forever — invisible in state, still overriding real alerts. So the assertion
// here is deliberately made against the server, not against Terraform state.
func TestAccAlertTemplate_removingAKeyRemovesItServerSide(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: templateMonitor + `
resource "lastping_alert_template" "t" {
  monitor_id = lastping_monitor.m.id
  templates = {
    "down"         = "{check_name} missed its deadline"
    "recovery"     = "{check_name} is back"
    "down/silence" = "{check_name} has been silent since {last_ping}"
  }
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_alert_template.t", "templates.%", "3"),
					resource.TestCheckResourceAttr("lastping_alert_template.t", "templates.down",
						"{check_name} missed its deadline"),
					resource.TestCheckResourceAttr("lastping_alert_template.t", "templates.down/silence",
						"{check_name} has been silent since {last_ping}"),
					checkTemplatesOnServer(t, map[string]string{
						"down":         "{check_name} missed its deadline",
						"recovery":     "{check_name} is back",
						"down/silence": "{check_name} has been silent since {last_ping}",
					}),
				),
			},
			{
				// Drop the per-cause key and one event-wide key, and change a
				// third. All in place — templates belong to the monitor, and
				// nothing here recreates it.
				Config: templateMonitor + `
resource "lastping_alert_template" "t" {
  monitor_id = lastping_monitor.m.id
  templates = {
    "down" = "{check_name} went down ({cause})"
  }
}`,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("lastping_alert_template.t",
							plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_alert_template.t", "templates.%", "1"),
					resource.TestCheckNoResourceAttr("lastping_alert_template.t", "templates.down/silence"),
					checkTemplatesOnServer(t, map[string]string{
						"down": "{check_name} went down ({cause})",
					}),
				),
			},
			{
				// Templates round-trip completely: there is nothing write-only
				// about them, so an import recovers the whole configuration.
				ResourceName:                         "lastping_alert_template.t",
				ImportState:                          true,
				ImportStateIdFunc:                    importStateAttrFunc("lastping_alert_template.t", "monitor_id"),
				ImportStateVerify:                    true,
				ImportStateVerifyIdentifierAttribute: "monitor_id",
			},
		},
	})
}

// TestAccAlertTemplate_destroyClearsTheSet: there is no DELETE endpoint for
// templates, so destroy has to clear them with a PUT — and, because per-cause
// rows are only removed when named, with a PUT that names every one of them.
//
// The monitor deliberately outlives the template resource here: if destroy were
// silently doing nothing, a test that tore down the monitor at the same time
// would never notice.
func TestAccAlertTemplate_destroyClearsTheSet(t *testing.T) {
	var monitorID string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: templateMonitor + `
resource "lastping_alert_template" "t" {
  monitor_id = lastping_monitor.m.id
  templates = {
    "down"         = "down body"
    "fail/runaway" = "too many pings"
  }
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrWith("lastping_alert_template.t", "monitor_id",
						func(id string) error {
							monitorID = id
							return nil
						}),
					checkTemplatesOnServer(t, map[string]string{
						"down":         "down body",
						"fail/runaway": "too many pings",
					}),
				),
			},
			{
				// Remove only the template resource. The monitor survives, so
				// its template set can be inspected afterwards.
				Config: templateMonitor,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("lastping_alert_template.t",
							plancheck.ResourceActionDestroy),
					},
				},
				Check: resource.TestCheckResourceAttrWith("lastping_monitor.m", "id", func(string) error {
					got, err := testAccDirectClient(t).GetTemplates(t.Context(), monitorID)
					if err != nil {
						return err
					}
					if len(got) != 0 {
						return fmt.Errorf("destroy left templates behind: %v", got)
					}
					return nil
				}),
			},
		},
	})
}

// TestAccAlertTemplate_invalidConfig: a bad key or an un-storable body is a
// plan-time error naming the attribute, or — for a body the server cannot
// render — an apply error naming the key, with nothing written either way.
func TestAccAlertTemplate_invalidConfig(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: templateMonitor + `
resource "lastping_alert_template" "t" {
  monitor_id = lastping_monitor.m.id
  templates  = { "sideways" = "nope" }
}`,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`(?s)must be an event type`),
			},
			{
				// "" means "reset to the default" to the API, so the key would
				// vanish from the applied map.
				Config: templateMonitor + `
resource "lastping_alert_template" "t" {
  monitor_id = lastping_monitor.m.id
  templates  = { "down" = "" }
}`,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`(?s)Empty alert template`),
			},
			{
				// The server trims, so a padded body would not survive the apply.
				Config: templateMonitor + `
resource "lastping_alert_template" "t" {
  monitor_id = lastping_monitor.m.id
  templates  = { "down" = "  padded  " }
}`,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`(?s)surrounding whitespace`),
			},
			{
				// Unknown fields are caught by the API's render check, which
				// rejects the whole request rather than writing part of it.
				Config: templateMonitor + `
resource "lastping_alert_template" "t" {
  monitor_id = lastping_monitor.m.id
  templates  = { "down" = "{no_such_token}" }
}`,
				ExpectError: regexp.MustCompile(`(?s)Unable to write alert templates`),
			},
		},
	})
}
