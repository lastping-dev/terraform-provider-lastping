package provider

import (
	"context"
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// TestAccDataProject_matchesTheKeysProject asserts the data source reports the
// same project the configured API key actually authenticates against, resolved
// independently through the client rather than compared with itself.
//
// The lookup is inside the check function, not in the TestCase literal, so it
// runs after PreCheck — a run without LASTPING_API_KEY skips instead of making
// an unauthenticated request.
func TestAccDataProject_matchesTheKeysProject(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `data "lastping_project" "current" {}`,
				Check: resource.TestCheckResourceAttrWith(
					"data.lastping_project.current", "project_id",
					func(got string) error {
						out, err := testAccDirectClient(t).Whoami(context.Background())
						if err != nil {
							return fmt.Errorf("resolve the acceptance key's project: %w", err)
						}
						if got != out.ProjectID {
							return fmt.Errorf("data source reported project_id %q, API says %q",
								got, out.ProjectID)
						}
						return nil
					}),
			},
		},
	})
}

// TestAccDataProject_isThePreconditionSourceForAGuardedWorkspace covers the
// documented use: a precondition that refuses to apply against the wrong
// project. The failing direction is the one worth testing — a guard that
// cannot fail is not a guard.
func TestAccDataProject_isThePreconditionSourceForAGuardedWorkspace(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "lastping_project" "current" {}

resource "lastping_monitor" "guarded" {
  name          = "acc-data-project-guard"
  slug          = "acc-data-project-guard"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300

  lifecycle {
    precondition {
      condition     = data.lastping_project.current.project_id == "00000000-0000-0000-0000-00000000dead"
      error_message = "wrong project"
    }
  }
}`,
				ExpectError: regexp.MustCompile(`(?s)wrong project`),
			},
		},
	})
}
