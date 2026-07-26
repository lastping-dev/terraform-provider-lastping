package provider

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"
)

// testAccNoEphemeralInState is the assertion the ephemeral resource exists for.
//
// It checks two things, because either alone would be too weak: no ephemeral
// block may appear as a state resource, and no attribute of anything in state
// may hold a value that looks like a LastPing key. The second is what would
// catch a key smuggled into state through a consumer.
func testAccNoEphemeralInState(s *terraform.State) error {
	for _, mod := range s.Modules {
		for name, rs := range mod.Resources {
			if strings.Contains(name, "ephemeral") {
				return fmt.Errorf("ephemeral resource %s must not be in state", name)
			}
			for attr, v := range rs.Primary.Attributes {
				if strings.HasPrefix(v, "lp_") {
					// The value itself is never printed.
					return fmt.Errorf("%s.%s holds what looks like an API key", name, attr)
				}
			}
		}
	}
	return nil
}

// testAccNoKeyNamed fails when a key with the given name still exists, which is
// how "Close revoked it" is proved: the ephemeral key is minted during the
// apply and must be gone by the time the test finishes.
func testAccNoKeyNamed(t *testing.T, name string) error {
	t.Helper()
	keys, err := testAccDirectClient(t).ListAPIKeys(context.Background())
	if err != nil {
		return err
	}
	for _, k := range keys {
		if k.Name == name {
			return fmt.Errorf("ephemeral key %q (id %s, prefix %s) survived the run; Close did not revoke it",
				name, k.ID, k.Prefix)
		}
	}
	return nil
}

// TestAccEphemeralAPIKey_usableAndNotInState is the whole point of the ephemeral
// resource, in one run:
//
//   - the minted key really authenticates — it configures a second provider
//     instance, and that instance creates a monitor. A key that did not work
//     would fail the apply with a 401, so the monitor existing IS the proof;
//   - nothing about the key reaches state;
//   - the key is revoked when the run ends.
//
// Feeding an ephemeral value into a provider block is also the intended usage,
// so this doubles as the documented example being exercised.
func TestAccEphemeralAPIKey_usableAndNotInState(t *testing.T) {
	const keyName = "acc-ephemeral-key"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		// Ephemeral resources are a Terraform 1.10 feature. Skipping is the
		// honest outcome on an older CLI; failing would blame the provider.
		TerraformVersionChecks: []tfversion.TerraformVersionCheck{
			tfversion.SkipBelow(tfversion.Version1_10_0),
		},
		CheckDestroy: func(*terraform.State) error {
			return testAccNoKeyNamed(t, keyName)
		},
		Steps: []resource.TestStep{
			{
				Config: `
ephemeral "lastping_api_key" "run" {
  name = "` + keyName + `"
  ttl  = "15m"
}

provider "lastping" {
  alias   = "run"
  api_key = ephemeral.lastping_api_key.run.key
}

resource "lastping_monitor" "uses_it" {
  provider = lastping.run

  name          = "acc-ephemeral-monitor"
  slug          = "acc-ephemeral-monitor"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					// The monitor exists, therefore the ephemeral key authenticated.
					resource.TestCheckResourceAttrSet("lastping_monitor.uses_it", "id"),
					resource.TestCheckResourceAttr("lastping_monitor.uses_it", "slug", "acc-ephemeral-monitor"),
					testAccNoEphemeralInState,
					// Close runs at the end of each Terraform operation, so by the
					// time checks run the key is already revoked.
					func(*terraform.State) error { return testAccNoKeyNamed(t, keyName) },
				),
			},
		},
	})
}

// TestAccEphemeralAPIKey_invalidTTL: a ttl typo must be a plan-time error
// naming the attribute, not a key minted with a lifetime nobody intended.
func TestAccEphemeralAPIKey_invalidTTL(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		TerraformVersionChecks: []tfversion.TerraformVersionCheck{
			tfversion.SkipBelow(tfversion.Version1_10_0),
		},
		Steps: []resource.TestStep{
			{
				Config: `
ephemeral "lastping_api_key" "bad" {
  name = "acc-ephemeral-bad-ttl"
  ttl  = "half an hour"
}`,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`(?s)Invalid ttl`),
			},
			{
				Config: `
ephemeral "lastping_api_key" "bad" {
  name = "acc-ephemeral-bad-ttl"
  ttl  = "-5m"
}`,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`(?s)ttl must be positive`),
			},
		},
	})
}
