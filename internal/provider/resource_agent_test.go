package provider

import (
	"context"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"

	"github.com/lastping-dev/terraform-provider-lastping/internal/client"
)

// TestAccAgent_basic covers the ordinary lifecycle: create, an in-place rename,
// and both import forms.
//
// The rename step is the one with teeth. `slug` is derived from `name` at
// creation and is NEVER re-derived, so renaming must (a) not replace the
// resource and (b) leave the original slug in state — anything already
// referring to the agent by slug depends on that.
func TestAccAgent_basic(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_agent" "test" {
  name        = "acc agent basic"
  description = "Acceptance fixture."
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_agent.test", "name", "acc agent basic"),
					resource.TestCheckResourceAttr("lastping_agent.test", "description", "Acceptance fixture."),
					// Derived server-side from the name.
					resource.TestCheckResourceAttr("lastping_agent.test", "slug", "acc-agent-basic"),
					// A brand-new agent owns nothing, so it is idle and never seen.
					resource.TestCheckResourceAttr("lastping_agent.test", "status", "idle"),
					resource.TestCheckResourceAttr("lastping_agent.test", "monitor_count", "0"),
					resource.TestCheckNoResourceAttr("lastping_agent.test", "last_seen"),
					resource.TestCheckResourceAttrSet("lastping_agent.test", "id"),
					resource.TestCheckResourceAttrSet("lastping_agent.test", "created_at"),
				),
			},
			{
				// Rename in place — must not force replacement, and must not
				// move the slug.
				Config: `
resource "lastping_agent" "test" {
  name        = "acc agent renamed"
  description = "Acceptance fixture."
}`,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("lastping_agent.test", plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_agent.test", "name", "acc agent renamed"),
					resource.TestCheckResourceAttr("lastping_agent.test", "slug", "acc-agent-basic"),
				),
			},
			{
				ResourceName:      "lastping_agent.test",
				ImportState:       true,
				ImportStateVerify: true,
			},
			{
				// Import by slug, which is what a human or an agent has to hand.
				// It resolves through the project's own list and nothing else.
				ResourceName:      "lastping_agent.test",
				ImportState:       true,
				ImportStateId:     "acc-agent-basic",
				ImportStateVerify: true,
			},
		},
	})
}

// TestAccAgent_descriptionCleared is the merge-patch regression test, end to
// end. Removing `description` from the configuration has to reach the API as an
// explicit JSON null; an absent key would leave the stored text in place and the
// attribute could never be unset once set.
func TestAccAgent_descriptionCleared(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_agent" "clear" {
  name        = "acc agent clear"
  description = "Set on the first apply."
}`,
				Check: resource.TestCheckResourceAttr(
					"lastping_agent.clear", "description", "Set on the first apply."),
			},
			{
				Config: `
resource "lastping_agent" "clear" {
  name = "acc agent clear"
}`,
				Check: resource.TestCheckNoResourceAttr("lastping_agent.clear", "description"),
			},
			{
				// And it stays cleared: a second plan with the attribute still
				// absent must be empty, not propose the removal again.
				Config: `
resource "lastping_agent" "clear" {
  name = "acc agent clear"
}`,
				PlanOnly: true,
			},
		},
	})
}

// TestAccAgent_createDoesNotAdoptExisting is the whole create-vs-upsert story.
//
// POST /api/v1/agents is create-only server-side — a collision on the derived
// slug is a 409, never an adoption — and this proves the provider surfaces that
// as an actionable error rather than quietly taking over an agent it did not
// create. The agent is registered out of band first, so Terraform has no state
// for it and genuinely believes it is creating something new.
//
// Note the two configurations use DIFFERENT names that derive the SAME slug:
// the collision is on the derived slug, not on the name, and a test using an
// identical name would not show that.
func TestAccAgent_createDoesNotAdoptExisting(t *testing.T) {
	c := testAccDirectClient(t)
	ctx := context.Background()

	var existingID string
	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			out, err := c.CreateAgent(ctx, client.Agent{
				Name:        "acc agent adopt",
				Description: "Registered outside Terraform.",
			})
			if err != nil {
				t.Fatalf("seed agent: %v", err)
			}
			existingID = out.ID
			t.Cleanup(func() { _ = c.DeleteAgent(ctx, existingID) })
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_agent" "adopt" {
  name = "ACC   Agent / Adopt"
}`,
				// A literal-space match here happens to sit right at a wrap
				// boundary today, which makes it one word of message drift
				// away from breaking the same way the pattern below did.
				// \s+ makes it wrap-proof instead of merely wrap-lucky.
				ExpectError: regexp.MustCompile(
					`An\s+agent\s+with\s+slug\s+"acc-agent-adopt"\s+already\s+exists\s+in\s+this\s+project`),
			},
		},
	})
}

// TestAccAgent_importOfUnknownSlugIsNotFound: import resolves a slug against
// the caller's own project list only. A slug that is not there — whether it
// belongs to another project or to nobody — reports the same thing, so import
// can never become an oracle for other tenants' agent names.
func TestAccAgent_importOfUnknownSlugIsNotFound(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_agent" "mine" {
  name = "acc agent import probe"
}`,
			},
			{
				ResourceName:  "lastping_agent.mine",
				ImportState:   true,
				ImportStateId: "acc-agent-that-does-not-exist",
				// This wraps between "in this" and "project." at the widths
				// the renderer actually uses in CI, so a literal space here
				// fails deterministically. \s+ tolerates the wrap.
				ExpectError: regexp.MustCompile(
					`No\s+agent\s+found\s+with\s+slug\s+or\s+ID\s+"acc-agent-that-does-not-exist"\s+in\s+this\s+project`),
			},
		},
	})
}

// TestAccAgent_deletedOutOfBandIsRecreated: an agent removed through the
// dashboard, the API or an MCP tool must drop out of state on refresh so the
// next apply registers a new one, rather than failing every plan with a 404.
func TestAccAgent_deletedOutOfBandIsRecreated(t *testing.T) {
	const cfg = `
resource "lastping_agent" "oob" {
  name = "acc agent oob"
}`

	c := testAccDirectClient(t)
	ctx := context.Background()

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: cfg},
			{
				PreConfig: func() {
					a, err := c.GetAgentBySlug(ctx, "acc-agent-oob")
					if err != nil {
						t.Fatalf("look up agent to delete out of band: %v", err)
					}
					if err := c.DeleteAgent(ctx, a.ID); err != nil {
						t.Fatalf("delete agent out of band: %v", err)
					}
				},
				Config: cfg,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("lastping_agent.oob", plancheck.ResourceActionCreate),
					},
				},
			},
		},
	})
}

// TestAccAgent_nameThatCannotBeSluggedIsRejectedAtPlanTime: the server answers
// 400 for a name it cannot slugify, and the provider mirrors the rule so the
// failure lands at plan time naming the attribute. The 50-character limit is on
// the DERIVED SLUG, which is the part no configuration makes obvious.
func TestAccAgent_nameThatCannotBeSluggedIsRejectedAtPlanTime(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_agent" "bad" {
  name = "!!!"
}`,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`does not yield a valid slug`),
			},
		},
	})
}
