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
				Config: `
resource "lastping_route" "bad" {
  monitor_id      = "3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f"
  event_type      = "every-run"
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
				ExpectError:   regexp.MustCompile(`(?s)not one of down, recovery, fail`),
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
