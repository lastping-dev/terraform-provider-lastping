package provider

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
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

// TestAccAlertTemplate_successAndStartedKeys covers the two event types the
// server added alongside every-run. The key grammar is validated at plan time
// against templateKeyPattern, so a key the API accepts but the pattern does not
// would be rejected here before the request was ever made — which is why this
// asserts against the server rather than stopping at a clean plan.
func TestAccAlertTemplate_successAndStartedKeys(t *testing.T) {
	want := map[string]string{
		"success":    "{check_name} finished cleanly in {duration}.",
		"started":    "{check_name} started.",
		"success/ci": "{check_name} passed on {branch} — {run_url}.",
	}

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: templateMonitor + `
resource "lastping_alert_template" "t" {
  monitor_id = lastping_monitor.m.id

  templates = {
    "success"    = "{check_name} finished cleanly in {duration}."
    "started"    = "{check_name} started."
    "success/ci" = "{check_name} passed on {branch} — {run_url}."
  }
}`,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("lastping_alert_template.t", "templates.started",
					"{check_name} started."),
				checkTemplatesOnServer(t, want),
			),
		}},
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

// TestAccAlertTemplate_refusesToAdoptExistingTemplates is the adoption guard.
//
// One write replaces the monitor's whole set, so before this guard a first
// `terraform apply` silently wiped every template set in the dashboard or over
// MCP. Nothing failed and nothing was logged; the operator found out at the
// next incident, when carefully worded alerts arrived in wording nobody had
// chosen. Create must refuse, and must leave the stored templates alone.
func TestAccAlertTemplate_refusesToAdoptExistingTemplates(t *testing.T) {
	var monitorID string

	// The set written out of band: one event-wide key that the configuration
	// below contradicts, and one per-cause key it omits entirely. Both are
	// destroyed by an unguarded create — the first overwritten, the second
	// cleared by the explicit delete replacementPayload sends for it.
	preexisting := map[string]string{
		"down":         "dashboard wording for down",
		"fail/runaway": "dashboard wording for runaway",
	}

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: templateMonitor,
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr("lastping_monitor.m", "id", &monitorID),
					func(*terraform.State) error {
						_, err := testAccDirectClient(t).PutTemplates(t.Context(), monitorID, preexisting)
						return err
					},
				),
			},
			{
				Config: templateMonitor + `
resource "lastping_alert_template" "t" {
  monitor_id = lastping_monitor.m.id
  templates = {
    "down" = "terraform wording for down"
  }
}`,
				ExpectError: regexp.MustCompile(
					`(?s)Alert templates already exist.*"down", "fail/runaway".*` +
						`terraform import lastping_alert_template\.<name> [0-9a-f-]+`),
			},
			{
				// Assert against the server: the resource never entered state,
				// so state proves nothing either way.
				Config: templateMonitor,
				Check: func(*terraform.State) error {
					got, err := testAccDirectClient(t).GetTemplates(t.Context(), monitorID)
					if err != nil {
						return err
					}
					if len(got) != len(preexisting) {
						return fmt.Errorf("the refusal still wrote: server holds %v, want %v", got, preexisting)
					}
					for k, v := range preexisting {
						if got[k] != v {
							return fmt.Errorf("template %q is %q, want %q", k, got[k], v)
						}
					}
					return nil
				},
			},
		},
	})
}

// TestAccAlertTemplate_createsOverAnIdenticalSet: the guard must not fire when
// the write would change nothing. Re-creating the resource after losing the
// state file is a legitimate recovery, and failing it would be worse than the
// hazard the guard exists for.
func TestAccAlertTemplate_createsOverAnIdenticalSet(t *testing.T) {
	var monitorID string

	preexisting := map[string]string{
		"down":         "{check_name} missed its deadline",
		"fail/runaway": "too many pings",
	}

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: templateMonitor,
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr("lastping_monitor.m", "id", &monitorID),
					func(*terraform.State) error {
						_, err := testAccDirectClient(t).PutTemplates(t.Context(), monitorID, preexisting)
						return err
					},
				),
			},
			{
				Config: templateMonitor + `
resource "lastping_alert_template" "t" {
  monitor_id = lastping_monitor.m.id
  templates = {
    "down"         = "{check_name} missed its deadline"
    "fail/runaway" = "too many pings"
  }
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_alert_template.t", "templates.%", "2"),
					checkTemplatesOnServer(t, preexisting),
				),
			},
		},
	})
}
