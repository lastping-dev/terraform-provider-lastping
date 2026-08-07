package provider

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// routeFixtures is the monitor and the two destinations every route test routes
// to. ntfy is used rather than email because the API refuses to route to an
// unverified destination, and email destinations start unverified.
const routeFixtures = `
resource "lastping_monitor" "m" {
  name          = "acc-route"
  slug          = "acc-route"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
}

resource "lastping_destination" "first" {
  kind      = "ntfy"
  name      = "acc-route-first"
  topic_url = "https://ntfy.sh/acc-route-first"
}

resource "lastping_destination" "second" {
  kind      = "ntfy"
  name      = "acc-route-second"
  topic_url = "https://ntfy.sh/acc-route-second"
}
`

// importStateAttrFunc reads one attribute out of the applied state to use as an
// import ID. Resources keyed on something other than a server-assigned `id`
// need this: the value is not known until apply.
func importStateAttrFunc(addr, attr string) resource.ImportStateIdFunc {
	return func(s *terraform.State) (string, error) {
		rs, ok := s.RootModule().Resources[addr]
		if !ok {
			return "", fmt.Errorf("%s not found in state", addr)
		}
		return rs.Primary.Attributes[attr], nil
	}
}

// captureAttr stashes an applied attribute so a later check — or an
// out-of-band API call standing in for the dashboard — can use it. Server-side
// ids are not known until apply, so there is no other way to address them.
func captureAttr(addr, attr string, into *string) resource.TestCheckFunc {
	return resource.TestCheckResourceAttrWith(addr, attr, func(v string) error {
		*into = v
		return nil
	})
}

// importStateIDFunc builds the composite "<monitor_id>:<event_type>" import ID
// from the applied state, since the monitor's UUID is not known until apply.
func importStateIDFunc(routeAddr string) resource.ImportStateIdFunc {
	return func(s *terraform.State) (string, error) {
		rs, ok := s.RootModule().Resources[routeAddr]
		if !ok {
			return "", fmt.Errorf("%s not found in state", routeAddr)
		}
		return fmt.Sprintf("%s:%s", rs.Primary.Attributes["monitor_id"],
			rs.Primary.Attributes["event_type"]), nil
	}
}

// TestAccRoute_orderingAndInPlaceUpdate pins the two decisions that make this
// resource what it is.
//
// First, destination_ids is a list and not a set: the API stores the array
// verbatim and returns it in the same order, so the order is real state and
// reordering is a real change. A set would silently normalise it away.
//
// Second, the endpoint replaces the whole list on every write, so both adding
// and removing a destination are in-place updates — never a destroy and
// recreate, which would leave the monitor unrouted in between.
func TestAccRoute_orderingAndInPlaceUpdate(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: routeFixtures + `
resource "lastping_route" "down" {
  monitor_id      = lastping_monitor.m.id
  event_type      = "down"
  destination_ids = [lastping_destination.first.id, lastping_destination.second.id]
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_route.down", "event_type", "down"),
					resource.TestCheckResourceAttr("lastping_route.down", "destination_ids.#", "2"),
					resource.TestCheckResourceAttrPair("lastping_route.down", "monitor_id",
						"lastping_monitor.m", "id"),
					resource.TestCheckResourceAttrPair("lastping_route.down", "destination_ids.0",
						"lastping_destination.first", "id"),
					resource.TestCheckResourceAttrPair("lastping_route.down", "destination_ids.1",
						"lastping_destination.second", "id"),
				),
			},
			{
				// Swapping the two entries is a genuine diff, and it must apply
				// in place rather than being normalised away as a set would.
				Config: routeFixtures + `
resource "lastping_route" "down" {
  monitor_id      = lastping_monitor.m.id
  event_type      = "down"
  destination_ids = [lastping_destination.second.id, lastping_destination.first.id]
}`,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("lastping_route.down", plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair("lastping_route.down", "destination_ids.0",
						"lastping_destination.second", "id"),
					resource.TestCheckResourceAttrPair("lastping_route.down", "destination_ids.1",
						"lastping_destination.first", "id"),
				),
			},
			{
				// Removing one destination is an in-place update: the route is
				// never torn down, so alerts keep flowing to the survivor.
				Config: routeFixtures + `
resource "lastping_route" "down" {
  monitor_id      = lastping_monitor.m.id
  event_type      = "down"
  destination_ids = [lastping_destination.second.id]
}`,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("lastping_route.down", plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_route.down", "destination_ids.#", "1"),
					resource.TestCheckResourceAttrPair("lastping_route.down", "destination_ids.0",
						"lastping_destination.second", "id"),
				),
			},
			{
				// A route has no id of its own, so import carries both halves of
				// its identity in the ID string.
				ResourceName:                         "lastping_route.down",
				ImportState:                          true,
				ImportStateIdFunc:                    importStateIDFunc("lastping_route.down"),
				ImportStateVerify:                    true,
				ImportStateVerifyIdentifierAttribute: "monitor_id",
			},
		},
	})
}

// TestAccRoute_multipleEventTypes: one resource per event type, all on the same
// monitor, must coexist. They write to different paths, so nothing clobbers
// anything — the failure mode this rules out is a shared key.
func TestAccRoute_multipleEventTypes(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: routeFixtures + `
resource "lastping_route" "down" {
  monitor_id      = lastping_monitor.m.id
  event_type      = "down"
  destination_ids = [lastping_destination.first.id]
}

resource "lastping_route" "fail" {
  monitor_id      = lastping_monitor.m.id
  event_type      = "fail"
  destination_ids = []
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair("lastping_route.down", "destination_ids.0",
						"lastping_destination.first", "id"),
					// An empty list is a valid route meaning "deliver nowhere",
					// which is not the same as having no route at all.
					resource.TestCheckResourceAttr("lastping_route.fail", "destination_ids.#", "0"),
				),
			},
			{
				// event_type is part of the resource's identity: changing it
				// must replace, so the abandoned event type is actually deleted
				// rather than left routed to a destination nothing references.
				Config: routeFixtures + `
resource "lastping_route" "down" {
  monitor_id      = lastping_monitor.m.id
  event_type      = "recovery"
  destination_ids = [lastping_destination.first.id]
}

resource "lastping_route" "fail" {
  monitor_id      = lastping_monitor.m.id
  event_type      = "fail"
  destination_ids = []
}`,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("lastping_route.down",
							plancheck.ResourceActionDestroyBeforeCreate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_route.down", "event_type", "recovery"),
					resource.TestCheckResourceAttrWith("lastping_route.down", "monitor_id",
						func(id string) error {
							routes, err := testAccDirectClient(t).GetRoutes(t.Context(), id)
							if err != nil {
								return err
							}
							for _, rt := range routes {
								if rt.EventType == "down" {
									return fmt.Errorf("the replaced down route was left behind: %+v", rt)
								}
							}
							return nil
						}),
				),
			},
		},
	})
}

// TestAccRoute_everyRun: every-run is routable through the dashboard and has
// been permitted by the check_routes constraint since the CI/CD signal-sources
// migration, but the REST validator used to reject it — so a project configured
// by clicking could not be reproduced in Terraform at all. This asserts the
// fourth event type applies, round-trips through Read, and imports.
//
// every-run does not go through the auto-route adoption exemption: the API
// never attaches an every-run route itself, so there is nothing to adopt.
func TestAccRoute_everyRun(t *testing.T) {
	config := routeFixtures + `
resource "lastping_route" "every_run" {
  monitor_id      = lastping_monitor.m.id
  event_type      = "every-run"
  destination_ids = [lastping_destination.first.id]
}`

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_route.every_run", "event_type", "every-run"),
					resource.TestCheckResourceAttrPair("lastping_route.every_run", "destination_ids.0",
						"lastping_destination.first", "id"),
					// Confirm against the API rather than trusting state: the
					// bug being fixed was the server rejecting this value.
					resource.TestCheckResourceAttrWith("lastping_route.every_run", "monitor_id",
						func(id string) error {
							routes, err := testAccDirectClient(t).GetRoutes(t.Context(), id)
							if err != nil {
								return err
							}
							for _, rt := range routes {
								if rt.EventType == "every-run" {
									return nil
								}
							}
							return fmt.Errorf("the API has no every-run route for monitor %s: %+v", id, routes)
						}),
				),
			},
			{
				// The import ID is "<monitor_id>:<event_type>", so this also
				// pins that parseRouteImportID accepts the hyphenated type.
				ResourceName: "lastping_route.every_run",
				ImportState:  true,
				// A route has no id of its own, so verification has to key on
				// the monitor id instead (same as the other import tests).
				ImportStateVerify:                    true,
				ImportStateVerifyIdentifierAttribute: "monitor_id",
				ImportStateIdFunc: func(s *terraform.State) (string, error) {
					rs, ok := s.RootModule().Resources["lastping_route.every_run"]
					if !ok {
						return "", fmt.Errorf("lastping_route.every_run not found in state")
					}
					return rs.Primary.Attributes["monitor_id"] + ":every-run", nil
				},
			},
			{
				// Widening must not have broken the original three.
				Config: routeFixtures + `
resource "lastping_route" "every_run" {
  monitor_id      = lastping_monitor.m.id
  event_type      = "every-run"
  destination_ids = [lastping_destination.first.id]
}

resource "lastping_route" "down" {
  monitor_id      = lastping_monitor.m.id
  event_type      = "down"
  destination_ids = [lastping_destination.second.id]
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_route.every_run", "event_type", "every-run"),
					resource.TestCheckResourceAttr("lastping_route.down", "event_type", "down"),
					resource.TestCheckResourceAttrPair("lastping_route.down", "destination_ids.0",
						"lastping_destination.second", "id"),
				),
			},
		},
	})
}

// TestAccRoute_successAndStarted covers the two event types added after
// every-run. `success` fires only on a successful run completion — which
// every-run cannot express, because it conflates success and failure — and
// `started` fires when a run begins.
//
// Both are routed in the same apply on purpose: they are separate rows keyed on
// (monitor_id, event_type), so a bug that keyed only on the monitor would show
// up here as one route overwriting the other rather than as two coexisting.
// Neither goes through the auto-route adoption exemption; the API attaches
// down/fail/recovery to a new monitor and nothing else.
func TestAccRoute_successAndStarted(t *testing.T) {
	config := routeFixtures + `
resource "lastping_route" "success" {
  monitor_id      = lastping_monitor.m.id
  event_type      = "success"
  destination_ids = [lastping_destination.first.id]
}

resource "lastping_route" "started" {
  monitor_id      = lastping_monitor.m.id
  event_type      = "started"
  destination_ids = [lastping_destination.second.id]
}`

	// Captured during the apply so the import step can assert against the real
	// ids: ImportStateCheck is handed only the imported instance, with no view
	// of the rest of the state.
	var monitorID, startedDestID string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr("lastping_monitor.m", "id", &monitorID),
					captureAttr("lastping_destination.second", "id", &startedDestID),
					resource.TestCheckResourceAttr("lastping_route.success", "event_type", "success"),
					resource.TestCheckResourceAttr("lastping_route.started", "event_type", "started"),
					resource.TestCheckResourceAttrPair("lastping_route.success", "destination_ids.0",
						"lastping_destination.first", "id"),
					resource.TestCheckResourceAttrPair("lastping_route.started", "destination_ids.0",
						"lastping_destination.second", "id"),
					// Confirm against the API rather than trusting state: what is
					// being widened is a server-side validator, so state agreeing
					// with the plan proves nothing on its own.
					resource.TestCheckResourceAttrWith("lastping_route.success", "monitor_id",
						func(id string) error {
							routes, err := testAccDirectClient(t).GetRoutes(t.Context(), id)
							if err != nil {
								return err
							}
							seen := map[string]bool{}
							for _, rt := range routes {
								seen[rt.EventType] = true
							}
							for _, want := range []string{"success", "started"} {
								if !seen[want] {
									return fmt.Errorf("the API has no %s route for monitor %s: %+v",
										want, id, routes)
								}
							}
							return nil
						}),
				),
			},
			{
				// The import ID is "<monitor_id>:<event_type>", so this pins that
				// parseRouteImportID accepts the new values too.
				//
				// Checked with ImportStateCheck rather than ImportStateVerify.
				// Verification compares the imported instance against one found
				// in prior state, and a route has no id of its own, so the only
				// attribute available to find it by is monitor_id — which both
				// routes in this config deliberately share. The framework matched
				// the imported `started` route against `success` and failed on
				// the difference between them. ImportStateCheck is handed the
				// imported instance directly, so no sibling can be confused for
				// it, and asserting on the attributes is stricter than the
				// equivalence check it replaces.
				ResourceName:      "lastping_route.started",
				ImportState:       true,
				ImportStateIdFunc: importStateIDFunc("lastping_route.started"),
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if len(states) != 1 {
						return fmt.Errorf("expected 1 imported instance, got %d", len(states))
					}
					attrs := states[0].Attributes
					for _, tc := range []struct{ attr, want string }{
						{"monitor_id", monitorID},
						{"event_type", "started"},
						{"destination_ids.#", "1"},
						{"destination_ids.0", startedDestID},
					} {
						if got := attrs[tc.attr]; got != tc.want {
							return fmt.Errorf("imported %s = %q, want %q (attrs: %+v)",
								tc.attr, got, tc.want, attrs)
						}
					}
					return nil
				},
			},
		},
	})
}

// TestAccRoute_deletedOutOfBand: a route removed through the dashboard must be
// recreated on the next apply, not fail the plan.
func TestAccRoute_deletedOutOfBand(t *testing.T) {
	config := routeFixtures + `
resource "lastping_route" "down" {
  monitor_id      = lastping_monitor.m.id
  event_type      = "down"
  destination_ids = [lastping_destination.first.id]
}`

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.TestCheckResourceAttrWith("lastping_route.down", "monitor_id",
					func(id string) error {
						// Delete it behind Terraform's back so the next step's
						// refresh sees it gone.
						return testAccDirectClient(t).DeleteRoute(t.Context(), id, "down")
					}),
				ExpectNonEmptyPlan: true,
			},
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("lastping_route.down", plancheck.ResourceActionCreate),
					},
				},
				Check: resource.TestCheckResourceAttr("lastping_route.down", "destination_ids.#", "1"),
			},
		},
	})
}

// TestAccRoute_invalidConfig: everything the API would reject with a 400 partway
// through an apply is a plan-time error naming the attribute instead.
func TestAccRoute_invalidConfig(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// Was "every-run" until every-run became routable. The
				// validator still has to reject something, so use a value the
				// API has never accepted: "runaway" is a real incident cause,
				// but it is surfaced through the fail event rather than routed
				// on its own.
				Config: `
resource "lastping_route" "bad" {
  monitor_id      = "3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f"
  event_type      = "runaway"
  destination_ids = []
}`,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`(?s)event_type value must be one of`),
			},
			{
				Config: `
resource "lastping_route" "bad" {
  monitor_id      = "acc-route"
  event_type      = "down"
  destination_ids = []
}`,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`(?s)Invalid ID`),
			},
			{
				// The API de-duplicates channel_ids, so a duplicate would come
				// back as a shorter list and fail the apply as an inconsistent
				// result.
				Config: `
resource "lastping_route" "bad" {
  monitor_id      = "3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f"
  event_type      = "down"
  destination_ids = [
    "8a1e7c92-4d3b-4a1f-9c2e-5b6d7e8f9a0b",
    "8a1e7c92-4d3b-4a1f-9c2e-5b6d7e8f9a0b",
  ]
}`,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`(?s)[Dd]uplicate`),
			},
		},
	})
}

// TestAccRoute_importMalformedID: the import ID carries the whole identity of
// the resource, so a malformed one has to say what the right shape is rather
// than fail as a lookup miss.
func TestAccRoute_importMalformedID(t *testing.T) {
	config := routeFixtures + `
resource "lastping_route" "down" {
  monitor_id      = lastping_monitor.m.id
  event_type      = "down"
  destination_ids = [lastping_destination.first.id]
}`

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: config},
			{
				ResourceName:  "lastping_route.down",
				ImportState:   true,
				ImportStateId: "not-a-composite-id",
				ExpectError:   regexp.MustCompile(`(?s)Invalid route import ID.*<monitor_id>:<event_type>`),
			},
			{
				ResourceName:  "lastping_route.down",
				ImportState:   true,
				ImportStateId: "3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f:sideways",
				ExpectError: regexp.MustCompile(
					`(?s)not one of down, recovery, fail, every-run, success, started, blocked, note`),
			},
		},
	})
}

// TestAccRoute_refusesToAdoptAnExistingRoute is the adoption guard.
//
// PUT replaces the whole destination list and the API has no create-only mode
// for routes, so before this guard a `terraform apply` against an event type
// somebody had already routed — in the dashboard, or over MCP — silently took
// it over. Nothing failed, nothing was logged, and the discovery channel was
// the next incident: the page went to Terraform's destinations instead of the
// ones actually on call. Create must refuse, and must leave the route alone.
func TestAccRoute_refusesToAdoptAnExistingRoute(t *testing.T) {
	var monitorID, firstID string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// Fixtures only. The route is then written behind Terraform's
				// back, which is exactly what a dashboard user would do.
				Config: routeFixtures,
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr("lastping_monitor.m", "id", &monitorID),
					captureAttr("lastping_destination.first", "id", &firstID),
					func(*terraform.State) error {
						_, err := testAccDirectClient(t).UpsertRoute(t.Context(),
							monitorID, "down", []string{firstID})
						return err
					},
				),
			},
			{
				// Same (monitor_id, event_type), different destinations.
				Config: routeFixtures + `
resource "lastping_route" "down" {
  monitor_id      = lastping_monitor.m.id
  event_type      = "down"
  destination_ids = [lastping_destination.second.id]
}`,
				ExpectError: regexp.MustCompile(
					`(?s)Route already exists.*terraform import lastping_route\.<name> [0-9a-f-]+:down`),
			},
			{
				// Assert against the server, not state: a refusal that had
				// already written would still leave state looking correct,
				// because the resource never entered state at all.
				Config: routeFixtures,
				Check: func(*terraform.State) error {
					rt, err := testAccDirectClient(t).GetRoute(t.Context(), monitorID, "down")
					if err != nil {
						return fmt.Errorf("the pre-existing route is gone: %w", err)
					}
					if len(rt.ChannelIDs) != 1 || rt.ChannelIDs[0] != firstID {
						return fmt.Errorf("the refusal still overwrote the route: got %v, want [%s]",
							rt.ChannelIDs, firstID)
					}
					return nil
				},
			},
		},
	})
}

// TestAccRoute_createsOverAnIdenticalRoute: the guard must not fire when the
// write would change nothing. Re-creating a route after losing the state file
// is a legitimate, common recovery, and failing it would be worse than the
// hazard the guard exists for.
func TestAccRoute_createsOverAnIdenticalRoute(t *testing.T) {
	var monitorID, firstID string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: routeFixtures,
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr("lastping_monitor.m", "id", &monitorID),
					captureAttr("lastping_destination.first", "id", &firstID),
					func(*terraform.State) error {
						_, err := testAccDirectClient(t).UpsertRoute(t.Context(),
							monitorID, "down", []string{firstID})
						return err
					},
				),
			},
			{
				Config: routeFixtures + `
resource "lastping_route" "down" {
  monitor_id      = lastping_monitor.m.id
  event_type      = "down"
  destination_ids = [lastping_destination.first.id]
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair("lastping_route.down", "destination_ids.0",
						"lastping_destination.first", "id"),
					func(*terraform.State) error {
						rt, err := testAccDirectClient(t).GetRoute(t.Context(), monitorID, "down")
						if err != nil {
							return err
						}
						if len(rt.ChannelIDs) != 1 || rt.ChannelIDs[0] != firstID {
							return fmt.Errorf("server holds %v, want [%s]", rt.ChannelIDs, firstID)
						}
						return nil
					},
				),
			},
		},
	})
}

// TestAccRoute_createsWhenNoRouteExists: the ordinary path, pinned separately
// so a guard that read the wrong key — or treated a 404 as a conflict — could
// not pass the two tests above while breaking every first apply.
func TestAccRoute_createsWhenNoRouteExists(t *testing.T) {
	var monitorID, firstID string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: routeFixtures + `
resource "lastping_route" "down" {
  monitor_id      = lastping_monitor.m.id
  event_type      = "down"
  destination_ids = [lastping_destination.first.id]
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr("lastping_monitor.m", "id", &monitorID),
					captureAttr("lastping_destination.first", "id", &firstID),
					func(*terraform.State) error {
						rt, err := testAccDirectClient(t).GetRoute(t.Context(), monitorID, "down")
						if err != nil {
							return err
						}
						if len(rt.ChannelIDs) != 1 || rt.ChannelIDs[0] != firstID {
							return fmt.Errorf("server holds %v, want [%s]", rt.ChannelIDs, firstID)
						}
						return nil
					},
				),
			},
		},
	})
}

// testAccDefaultEmailDestinationID resolves the destination the API auto-routes
// every new monitor to. testAccPreCheck calls this for every acceptance test in
// the suite, not just the adoption-exemption tests here, so a project lacking
// one can never again pass the suite without exercising the auto-route path.
// The adoption-exemption tests also call it directly to get the id itself,
// which PreCheck's use of the return value discards.
//
// It fails rather than skips. A skip here would be indistinguishable from a
// pass, and the bug this guards against — a monitor and its routes being
// impossible to create in one apply — is invisible in a project that has no
// verified email channel, because attachDefaultRoutes then returns early and
// writes no routes at all. That is exactly how the whole suite missed it.
func testAccDefaultEmailDestinationID(t *testing.T) string {
	t.Helper()
	id, err := testAccDirectClient(t).DefaultEmailDestinationID(t.Context())
	if err != nil {
		t.Fatalf("listing destinations: %v", err)
	}
	if id == "" {
		t.Fatal("this project has no verified email destination, so the API will not " +
			"auto-route new monitors and this test cannot exercise anything.\n" +
			"Seed one in the acceptance backend, for example:\n" +
			"  docker compose exec -T postgres psql -U lastping -d lastping -c \\\n" +
			"    \"INSERT INTO channels (id, project_id, kind, name, config, verified_at) \\\n" +
			"     VALUES (gen_random_uuid(), '<project>', 'email', 'Email', \\\n" +
			"             '{\\\"address\\\":\\\"acc@example.com\\\"}'::jsonb, now());\"")
	}
	return id
}

// TestAccRoute_createsAMonitorAndItsRoutesInOneApply is the provider's most
// obvious use case, and before the auto-route exemption it was impossible.
//
// The API attaches down/fail/recovery routes to the project's default email
// channel the moment a monitor is created (api/defaultdest.go:
// attachDefaultRoutes), so by the time Terraform creates the route resources in
// the same apply, all three event types are already routed — to destinations
// Terraform did not write. The adoption guard refused, every time, with
// "Route already exists", and the only way forward was to apply, read the
// auto-routes back, write import blocks and apply again.
//
// This test is the whole scenario in one apply. It fails before the fix.
func TestAccRoute_createsAMonitorAndItsRoutesInOneApply(t *testing.T) {
	var monitorID string

	resource.Test(t, resource.TestCase{
		// testAccPreCheck itself now requires the default email channel to
		// exist (it is what this test needs), so no extra call is needed here.
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_monitor" "fresh" {
  name          = "acc-route-oneshot"
  slug          = "acc-route-oneshot"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
}

resource "lastping_destination" "oncall" {
  kind      = "ntfy"
  name      = "acc-route-oneshot-oncall"
  topic_url = "https://ntfy.sh/acc-route-oneshot-oncall"
}

resource "lastping_route" "down" {
  monitor_id      = lastping_monitor.fresh.id
  event_type      = "down"
  destination_ids = [lastping_destination.oncall.id]
}

resource "lastping_route" "fail" {
  monitor_id      = lastping_monitor.fresh.id
  event_type      = "fail"
  destination_ids = [lastping_destination.oncall.id]
}

resource "lastping_route" "recovery" {
  monitor_id      = lastping_monitor.fresh.id
  event_type      = "recovery"
  destination_ids = [lastping_destination.oncall.id]
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr("lastping_monitor.fresh", "id", &monitorID),
					resource.TestCheckResourceAttrPair("lastping_route.down", "destination_ids.0",
						"lastping_destination.oncall", "id"),
					resource.TestCheckResourceAttrPair("lastping_route.fail", "destination_ids.0",
						"lastping_destination.oncall", "id"),
					resource.TestCheckResourceAttrPair("lastping_route.recovery", "destination_ids.0",
						"lastping_destination.oncall", "id"),
					// Assert against the server: adoption has to have replaced
					// the auto-routes, not merely written state that says so.
					resource.TestCheckResourceAttrWith("lastping_destination.oncall", "id",
						func(oncallID string) error {
							routes, err := testAccDirectClient(t).GetRoutes(t.Context(), monitorID)
							if err != nil {
								return err
							}
							seen := map[string]bool{}
							for _, rt := range routes {
								seen[rt.EventType] = true
								if len(rt.ChannelIDs) != 1 || rt.ChannelIDs[0] != oncallID {
									return fmt.Errorf("%s route holds %v, want [%s]",
										rt.EventType, rt.ChannelIDs, oncallID)
								}
							}
							for _, ev := range []string{"down", "fail", "recovery"} {
								if !seen[ev] {
									return fmt.Errorf("no %s route on the monitor", ev)
								}
							}
							return nil
						}),
				),
			},
		},
	})
}

// TestAccRoute_refusesARouteBuiltOnTheDefaultDestination keeps the exemption
// from widening into the hazard.
//
// The auto-route the API writes has exactly one destination. A route that also
// contains the default email destination but has been extended by hand — the
// ordinary "page me as well as emailing the team" edit — is a person's
// configuration, and taking it over would drop whoever was added. It must be
// refused, and the route must survive the refusal untouched.
func TestAccRoute_refusesARouteBuiltOnTheDefaultDestination(t *testing.T) {
	var monitorID, firstID, defaultID string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: routeFixtures,
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr("lastping_monitor.m", "id", &monitorID),
					captureAttr("lastping_destination.first", "id", &firstID),
					func(*terraform.State) error {
						defaultID = testAccDefaultEmailDestinationID(t)
						// Widen the auto-route by hand, exactly as somebody
						// would in the dashboard.
						_, err := testAccDirectClient(t).UpsertRoute(t.Context(),
							monitorID, "down", []string{defaultID, firstID})
						return err
					},
				),
			},
			{
				Config: routeFixtures + `
resource "lastping_route" "down" {
  monitor_id      = lastping_monitor.m.id
  event_type      = "down"
  destination_ids = [lastping_destination.second.id]
}`,
				ExpectError: regexp.MustCompile(
					`(?s)Route already exists.*terraform import lastping_route\.<name> [0-9a-f-]+:down`),
			},
			{
				Config: routeFixtures,
				Check: func(*terraform.State) error {
					rt, err := testAccDirectClient(t).GetRoute(t.Context(), monitorID, "down")
					if err != nil {
						return fmt.Errorf("the pre-existing route is gone: %w", err)
					}
					want := []string{defaultID, firstID}
					if len(rt.ChannelIDs) != 2 || rt.ChannelIDs[0] != want[0] || rt.ChannelIDs[1] != want[1] {
						return fmt.Errorf("the refusal still overwrote the route: got %v, want %v",
							rt.ChannelIDs, want)
					}
					return nil
				},
			},
		},
	})
}
