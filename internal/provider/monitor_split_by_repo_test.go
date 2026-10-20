package provider

import (
	"context"
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
)

// Acceptance tests for split_by_repo. Like every TestAcc* in this package they
// skip unless LASTPING_API_KEY is set, and they need a backend that serves
// split_by_repo.

// testAccCheckServerSplitByRepo reads split_by_repo off the server, not out
// of state: a Create that never sent the follow-up PATCH would leave state
// agreeing with the plan while the monitor still held the default.
func testAccCheckServerSplitByRepo(t *testing.T, name string, want bool) resource.TestCheckFunc {
	return resource.TestCheckResourceAttrWith(name, "id", func(id string) error {
		mon, err := testAccDirectClient(t).GetMonitor(context.Background(), id)
		if err != nil {
			return err
		}
		if mon.SplitByRepo == nil {
			return fmt.Errorf("server response carries no split_by_repo")
		}
		if *mon.SplitByRepo != want {
			return fmt.Errorf("server holds split_by_repo=%t, want %t", *mon.SplitByRepo, want)
		}
		return nil
	})
}

// TestAccMonitor_splitByRepo: create with false, read it back from the
// server, flip it to true in place, and import it.
func TestAccMonitor_splitByRepo(t *testing.T) {
	config := func(v bool) string {
		return fmt.Sprintf(`
resource "lastping_monitor" "sbr" {
  name          = "acc-split-by-repo"
  slug          = "acc-split-by-repo"
  schedule_kind = "on_demand"
  grace_s       = 600
  split_by_repo = %t
}`, v)
	}

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config(false),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_monitor.sbr", "split_by_repo", "false"),
					testAccCheckServerSplitByRepo(t, "lastping_monitor.sbr", false),
				),
			},
			{
				Config:   config(false),
				PlanOnly: true,
			},
			{
				Config: config(true),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("lastping_monitor.sbr", plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_monitor.sbr", "split_by_repo", "true"),
					testAccCheckServerSplitByRepo(t, "lastping_monitor.sbr", true),
				),
			},
			{
				ResourceName:      "lastping_monitor.sbr",
				ImportState:       true,
				ImportStateId:     "acc-split-by-repo",
				ImportStateVerify: true,
			},
		},
	})
}

// TestAccMonitor_splitByRepoOmitted: a monitor that never sets split_by_repo
// reads the server's default and plans nothing on the next run.
func TestAccMonitor_splitByRepoOmitted(t *testing.T) {
	const omitted = `
resource "lastping_monitor" "sbro" {
  name          = "acc-split-by-repo-omitted"
  slug          = "acc-split-by-repo-omitted"
  schedule_kind = "on_demand"
  grace_s       = 600
}`
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: omitted,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_monitor.sbro", "split_by_repo", "true"),
					testAccCheckServerSplitByRepo(t, "lastping_monitor.sbro", true),
				),
			},
			{
				Config:   omitted,
				PlanOnly: true,
			},
		},
	})
}

// TestAccMonitor_splitByRepoFalseNeedsOnDemand: false on a monitor that is
// not on_demand is refused at plan time, before a create could fail on its
// follow-up PATCH and leave a tainted monitor to replace on every apply.
func TestAccMonitor_splitByRepoFalseNeedsOnDemand(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: `
resource "lastping_monitor" "sbrc" {
  name          = "acc-split-by-repo-cron"
  slug          = "acc-split-by-repo-cron"
  schedule_kind = "cron"
  cron_expr     = "0 3 * * *"
  grace_s       = 600
  split_by_repo = false
}`,
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(`split_by_repo = false applies only to on_demand`),
		}},
	})
}
