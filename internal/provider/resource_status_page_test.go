package provider

import (
	"context"
	"os"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"

	"github.com/lastping-dev/terraform-provider-lastping/internal/client"
)

// statusPageMonitors are the monitors the pages list. check_ids is validated
// against the caller's project, so these cannot be invented UUIDs.
const statusPageMonitors = `
resource "lastping_monitor" "first" {
  name          = "acc-page-first"
  slug          = "acc-page-first"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
}

resource "lastping_monitor" "second" {
  name          = "acc-page-second"
  slug          = "acc-page-second"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
}
`

// testAccForeignClient returns a client for a SECOND project, used to prove
// that cross-tenant resources stay invisible. It skips rather than fails when
// the second key is absent: a single-project backend cannot demonstrate
// isolation either way, and unlike the default-email-channel prerequisite (see
// testAccPreCheck), a second project is genuinely optional for local work — it
// cannot be seeded by one `psql -c INSERT` into the same backend. The skip
// message is deliberately loud rather than terse, so a run that silently
// proves nothing about tenant isolation cannot be mistaken for one that does.
func testAccForeignClient(t *testing.T) *client.Client {
	t.Helper()
	key := os.Getenv("LASTPING_ACC_FOREIGN_API_KEY")
	if key == "" {
		t.Skip("LASTPING_ACC_FOREIGN_API_KEY not set: skipping " +
			"TestAccStatusPage_importOfForeignSlugIsNotFound. Cross-tenant " +
			"isolation on status page import is NOT being verified this run. " +
			"Set LASTPING_ACC_FOREIGN_API_KEY to a valid API key for a SECOND, " +
			"distinct LastPing project to cover it; see README.md's testing " +
			"section.")
	}
	endpoint := os.Getenv("LASTPING_ENDPOINT")
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	return client.New(endpoint, key, "acc-test")
}

// TestAccStatusPage_privateWithExplicitSlug covers the ordinary lifecycle:
// create, in-place update, and both import forms. public_url is asserted empty
// because a private page has no public address — the API returns "" rather than
// omitting the field.
func TestAccStatusPage_privateWithExplicitSlug(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: statusPageMonitors + `
resource "lastping_status_page" "p" {
  slug      = "acc-private-page"
  title     = "Acceptance"
  check_ids = [lastping_monitor.first.id, lastping_monitor.second.id]
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_status_page.p", "slug", "acc-private-page"),
					resource.TestCheckResourceAttr("lastping_status_page.p", "title", "Acceptance"),
					// Defaulted server-side and by the provider alike.
					resource.TestCheckResourceAttr("lastping_status_page.p", "visibility", "private"),
					resource.TestCheckResourceAttr("lastping_status_page.p", "public_url", ""),
					resource.TestCheckResourceAttrSet("lastping_status_page.p", "id"),
					resource.TestCheckResourceAttrSet("lastping_status_page.p", "created_at"),
					// check_ids is a list, and the API preserves its order.
					resource.TestCheckResourceAttr("lastping_status_page.p", "check_ids.#", "2"),
					resource.TestCheckResourceAttrPair("lastping_status_page.p", "check_ids.0",
						"lastping_monitor.first", "id"),
					resource.TestCheckResourceAttrPair("lastping_status_page.p", "check_ids.1",
						"lastping_monitor.second", "id"),
				),
			},
			{
				// Retitling and reordering are in-place: the slug is untouched,
				// so nothing is released and reclaimed.
				Config: statusPageMonitors + `
resource "lastping_status_page" "p" {
  slug      = "acc-private-page"
  title     = "Acceptance, renamed"
  check_ids = [lastping_monitor.second.id]
}`,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("lastping_status_page.p", plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_status_page.p", "title", "Acceptance, renamed"),
					resource.TestCheckResourceAttr("lastping_status_page.p", "check_ids.#", "1"),
					resource.TestCheckResourceAttrPair("lastping_status_page.p", "check_ids.0",
						"lastping_monitor.second", "id"),
				),
			},
			{
				ResourceName:      "lastping_status_page.p",
				ImportState:       true,
				ImportStateVerify: true,
			},
			{
				// Import by slug, which is what a human or an agent has to hand.
				// It resolves through the project's own list and nothing else.
				ResourceName:      "lastping_status_page.p",
				ImportState:       true,
				ImportStateId:     "acc-private-page",
				ImportStateVerify: true,
			},
			{
				// Changing the slug replaces the page: the API cannot rename one.
				Config: statusPageMonitors + `
resource "lastping_status_page" "p" {
  slug      = "acc-private-renamed"
  title     = "Acceptance, renamed"
  check_ids = [lastping_monitor.second.id]
}`,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("lastping_status_page.p",
							plancheck.ResourceActionDestroyBeforeCreate),
					},
				},
				Check: resource.TestCheckResourceAttr("lastping_status_page.p", "slug", "acc-private-renamed"),
			},
		},
	})
}

// TestAccStatusPage_publicHasPublicURL: `public` is the only visibility that
// produces a URL, and the URL is derived from the slug — which is exactly why
// slugs are global and why this resource is the awkward one.
//
// This test creates the project's single free-tier public page, so it must not
// run alongside another that does the same.
func TestAccStatusPage_publicHasPublicURL(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_status_page" "pub" {
  slug       = "acc-public-page"
  title      = "Public"
  visibility = "public"

  lifecycle {
    create_before_destroy = true
  }
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_status_page.pub", "visibility", "public"),
					resource.TestMatchResourceAttr("lastping_status_page.pub", "public_url",
						regexp.MustCompile(`/status/acc-public-page$`)),
					// check_ids was never configured, so it must read back as
					// absent rather than as an empty list.
					resource.TestCheckNoResourceAttr("lastping_status_page.pub", "check_ids.#"),
				),
			},
			{
				// Going back to private withdraws the public URL.
				Config: `
resource "lastping_status_page" "pub" {
  slug       = "acc-public-page"
  title      = "Public"
  visibility = "private"

  lifecycle {
    create_before_destroy = true
  }
}`,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("lastping_status_page.pub", plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.TestCheckResourceAttr("lastping_status_page.pub", "public_url", ""),
			},
		},
	})
}

// TestAccStatusPage_generatedSlug: slug is Optional AND Computed because the
// server genuinely supplies one (api/statuspages.go: generateStatusPageSlug).
// The PlanOnly step is the real assertion — a computed value that is not held
// in state would show up as a perpetual diff, and for this attribute a diff
// means a replacement that releases a live URL.
func TestAccStatusPage_generatedSlug(t *testing.T) {
	const config = `
resource "lastping_status_page" "gen" {
  title = "Generated"
}`

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					// 10 characters from the server's unambiguous alphabet.
					resource.TestMatchResourceAttr("lastping_status_page.gen", "slug",
						regexp.MustCompile(`^[a-z0-9]{10}$`)),
					resource.TestCheckResourceAttr("lastping_status_page.gen", "visibility", "private"),
				),
			},
			{
				Config:   config,
				PlanOnly: true,
			},
			{
				// An unconfigured slug must not be re-planned on an unrelated
				// change either: that would replace the page and release its URL.
				Config: `
resource "lastping_status_page" "gen" {
  title = "Generated, renamed"
}`,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("lastping_status_page.gen", plancheck.ResourceActionUpdate),
					},
				},
			},
		},
	})
}

// TestAccStatusPage_slugTakenGivesActionableError is the cross-tenant case.
//
// The slug namespace is global — `status_pages_slug_idx` is UNIQUE (slug) with
// no project column — so in production this failure is usually caused by a
// DIFFERENT account, and the practitioner has no way to see the page that is in
// their way. A generic "unable to create status page" would send them hunting
// for a bug in their own configuration that is not there. The diagnostic has to
// say the namespace is global and offer both remedies.
//
// The collision is simulated within one account because the 409 is the same
// code path either way (api/statuspages.go: errSlugConflict).
func TestAccStatusPage_slugTakenGivesActionableError(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_status_page" "held" {
  slug  = "acc-contested-slug"
  title = "Holder"
}`,
			},
			{
				Config: `
resource "lastping_status_page" "held" {
  slug  = "acc-contested-slug"
  title = "Holder"
}

resource "lastping_status_page" "contender" {
  slug  = "acc-contested-slug"
  title = "Contender"
}`,
				ExpectError: regexp.MustCompile(
					`(?s)Status page slug already taken.*GLOBAL across all LastPing accounts.*Choose a different slug`),
			},
		},
	})
}

// TestAccStatusPage_publicCapErrorIsDistinct: the free-tier cap is also a 4xx
// on create, and it means something entirely different from a slug collision —
// the name is available, the plan limit is not. Conflating the two would send
// someone renaming a page that never needed renaming, so the diagnostic must
// name the limit and must not mention slugs at all.
func TestAccStatusPage_publicCapErrorIsDistinct(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_status_page" "one" {
  slug       = "acc-cap-first"
  title      = "First"
  visibility = "public"
}`,
			},
			{
				Config: `
resource "lastping_status_page" "one" {
  slug       = "acc-cap-first"
  title      = "First"
  visibility = "public"
}

resource "lastping_status_page" "two" {
  slug       = "acc-cap-second"
  title      = "Second"
  visibility = "public"
}`,
				// Note what this regex does NOT contain: any mention of slugs.
				ExpectError: regexp.MustCompile(
					`(?s)Project already has a public status page.*nothing to do with the slug`),
			},
		},
	})
}

// TestAccStatusPage_importOfForeignSlugIsNotFound: a slug owned by another
// project exists globally but is not yours.
//
// Import must resolve within the caller's project only and report a plain
// not-found. Anything that distinguished "exists elsewhere" from "does not
// exist" would turn `terraform import` into an oracle for other accounts' page
// names — which, since those names are public URLs someone chose deliberately,
// is worth protecting.
func TestAccStatusPage_importOfForeignSlugIsNotFound(t *testing.T) {
	foreign := testAccForeignClient(t)

	const foreignSlug = "acc-foreign-page"
	page, err := foreign.CreateStatusPage(t.Context(), client.StatusPage{
		Slug:       foreignSlug,
		Title:      "Someone else's page",
		Visibility: "private",
	})
	if err != nil {
		t.Fatalf("seed foreign status page: %v", err)
	}
	t.Cleanup(func() {
		// context.Background(), not t.Context(): the test's context is already
		// cancelled by the time cleanups run, and this seed holds a global slug
		// until it is deleted.
		if err := foreign.DeleteStatusPage(context.Background(), page.ID); err != nil {
			t.Errorf("clean up foreign status page %s: %v", page.ID, err)
		}
	})

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// A page of our own, so the import step has prior state to
				// address — and so the failure below cannot be blamed on an
				// empty project.
				Config: `
resource "lastping_status_page" "mine" {
  slug  = "acc-own-page"
  title = "Mine"
}`,
			},
			{
				ResourceName:  "lastping_status_page.mine",
				ImportState:   true,
				ImportStateId: foreignSlug,
				// Same message shape as the agent "not found" diagnostic,
				// which wraps between "in this" and "project." at the widths
				// the renderer uses in CI - \s+ tolerates that wrap instead
				// of depending on this exact slug length staying just short
				// enough to stay on one line.
				ExpectError: regexp.MustCompile(
					`No\s+status\s+page\s+found\s+with\s+slug\s+or\s+ID\s+"` + foreignSlug + `"\s+in\s+this\s+project`),
			},
			{
				// The same message for a slug nobody owns: the two cases must be
				// indistinguishable.
				ResourceName:  "lastping_status_page.mine",
				ImportState:   true,
				ImportStateId: "acc-nobody-owns-this",
				ExpectError: regexp.MustCompile(
					`No\s+status\s+page\s+found\s+with\s+slug\s+or\s+ID\s+"acc-nobody-owns-this"\s+in\s+this\s+project`),
			},
		},
	})
}

// TestAccStatusPage_deletedOutOfBand: a page deleted through the dashboard must
// be recreated on the next apply, not fail the plan.
func TestAccStatusPage_deletedOutOfBand(t *testing.T) {
	const config = `
resource "lastping_status_page" "gone" {
  slug  = "acc-page-gone"
  title = "Gone"
}`

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.TestCheckResourceAttrWith("lastping_status_page.gone", "id", func(id string) error {
					return testAccDirectClient(t).DeleteStatusPage(t.Context(), id)
				}),
				ExpectNonEmptyPlan: true,
			},
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("lastping_status_page.gone", plancheck.ResourceActionCreate),
					},
				},
				Check: resource.TestCheckResourceAttr("lastping_status_page.gone", "slug", "acc-page-gone"),
			},
		},
	})
}

// TestAccStatusPage_invalidSlug: the server normalises before it validates, so
// a padded or uppercase slug would be accepted and come back as something the
// configuration never asked for. Terraform rejects a planned value that differs
// from a known config value, so the only correct answer is to refuse the
// un-normalised form at plan time.
func TestAccStatusPage_invalidSlug(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_status_page" "bad" {
  slug  = "Acc-Uppercase"
  title = "Bad"
}`,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`(?s)must be lowercase and trimmed`),
			},
			{
				Config: `
resource "lastping_status_page" "bad" {
  slug  = "3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f"
  title = "Bad"
}`,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`(?s)Invalid slug`),
			},
			{
				Config: `
resource "lastping_status_page" "bad" {
  slug       = "acc-bad-visibility"
  title      = "Bad"
  visibility = "unlisted"
}`,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`(?s)visibility value must be one of`),
			},
		},
	})
}
