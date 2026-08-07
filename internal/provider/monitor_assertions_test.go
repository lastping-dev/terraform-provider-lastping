package provider

import (
	"context"
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/stretchr/testify/require"

	"github.com/lastping-dev/terraform-provider-lastping/internal/client"
)

// Acceptance tests for the `assertion` nested block. They are TF_ACC-gated and
// run against a live backend from the monorepo's CI, like the rest of this
// package.
//
// Every ExpectError pattern uses `\s+` rather than a literal space: Terraform
// re-wraps diagnostic text to the terminal width, so a literal space in the
// middle of a message is a newline as often as not.

// TestAccMonitorAssertions_lifecycle is the whole contract in one resource:
// create with assertions, add one, change one, and remove them all.
//
// The removal step is the one that matters most. The block is Optional-only, so
// deleting every block plans as a null set — and the endpoint is
// replace-the-set, with no encoding for "leave them alone". A provider that
// treated the null as "nothing to do" would make the assertions unremovable
// through Terraform, which is exactly the bug tags, runaway_ceiling and
// monitor_from all shipped with at one point.
func TestAccMonitorAssertions_lifecycle(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_monitor" "asrt" {
  name          = "acc-assertions"
  slug          = "acc-assertions"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 600

  assertion {
    name  = "rows written"
    kind  = "json_path"
    path  = "result.rows_processed"
    op    = "gt"
    value = "0"
  }
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_monitor.asrt", "assertion.#", "1"),
					resource.TestCheckTypeSetElemNestedAttrs("lastping_monitor.asrt", "assertion.*",
						map[string]string{
							"name":  "rows written",
							"kind":  "json_path",
							"path":  "result.rows_processed",
							"op":    "gt",
							"value": "0",
						}),
				),
			},
			{
				// Re-applying the identical configuration must plan empty. The
				// server returns assertions ordered by (created_at, id) with a
				// random-UUID tie-break, so a list-typed block or an
				// order-sensitive comparison would show a diff here on most runs.
				Config: `
resource "lastping_monitor" "asrt" {
  name          = "acc-assertions"
  slug          = "acc-assertions"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 600

  assertion {
    name  = "rows written"
    kind  = "json_path"
    path  = "result.rows_processed"
    op    = "gt"
    value = "0"
  }
}`,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				// Add a second assertion and change the first's operator.
				Config: `
resource "lastping_monitor" "asrt" {
  name          = "acc-assertions"
  slug          = "acc-assertions"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 600

  assertion {
    name  = "rows written"
    kind  = "json_path"
    path  = "result.rows_processed"
    op    = "gte"
    value = "1"
  }

  assertion {
    name  = "no traceback"
    kind  = "not_contains"
    value = "Traceback (most recent call last)"
  }
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_monitor.asrt", "assertion.#", "2"),
					resource.TestCheckTypeSetElemNestedAttrs("lastping_monitor.asrt", "assertion.*",
						map[string]string{"name": "rows written", "op": "gte", "value": "1"}),
					resource.TestCheckTypeSetElemNestedAttrs("lastping_monitor.asrt", "assertion.*",
						map[string]string{"name": "no traceback", "kind": "not_contains"}),
				),
			},
			{
				// Every block removed: the set must be cleared server-side, not
				// left in place.
				Config: `
resource "lastping_monitor" "asrt" {
  name          = "acc-assertions"
  slug          = "acc-assertions"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 600
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr("lastping_monitor.asrt", "assertion.#"),
					testAccCheckMonitorAssertionCount(t, "lastping_monitor.asrt", 0),
				),
			},
			{
				ResourceName:      "lastping_monitor.asrt",
				ImportState:       true,
				ImportStateId:     "acc-assertions",
				ImportStateVerify: true,
			},
		},
	})
}

// TestAccMonitorAssertions_importCarriesTheSet covers the read path on its own.
// Assertions live behind a second endpoint, so a Read that forgets to call it
// imports a monitor with an empty assertion set and the next plan proposes
// deleting assertions that are actually configured.
func TestAccMonitorAssertions_importCarriesTheSet(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_monitor" "asrt_import" {
  name          = "acc-assertions-import"
  slug          = "acc-assertions-import"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 600

  assertion {
    name  = "no traceback"
    kind  = "not_contains"
    value = "Traceback"
  }

  assertion {
    name  = "completion line"
    kind  = "matches"
    value = "^done: [0-9]+$"
  }
}`,
			},
			{
				ResourceName:      "lastping_monitor.asrt_import",
				ImportState:       true,
				ImportStateId:     "acc-assertions-import",
				ImportStateVerify: true,
			},
		},
	})
}

// TestAccMonitorAssertions_driftIsDetected: an assertion changed outside
// Terraform must show up as a diff. Read is the only place that can notice, and
// it is the call most easily forgotten, because everything else about the
// resource keeps working without it.
func TestAccMonitorAssertions_driftIsDetected(t *testing.T) {
	const cfg = `
resource "lastping_monitor" "asrt_drift" {
  name          = "acc-assertions-drift"
  slug          = "acc-assertions-drift"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 600

  assertion {
    name  = "rows written"
    kind  = "json_path"
    path  = "result.rows_processed"
    op    = "gt"
    value = "0"
  }
}`

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: cfg},
			{
				// Replace the set out of band, then plan: the provider must
				// propose putting the configured one back.
				PreConfig: func() {
					c := testAccDirectClient(t)
					id := testAccMonitorIDBySlug(t, c, "acc-assertions-drift")
					_, err := c.PutAssertions(context.Background(), id, []client.Assertion{
						{Name: "something else", Kind: "contains", Value: "x"},
					})
					require.NoError(t, err)
				},
				Config:             cfg,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{Config: cfg},
		},
	})
}

// TestAccMonitorAssertions_invalidRejectedAtPlanTime: every one of these is a
// 400 from the API, anticipated at plan time so the error names the assertion
// instead of arriving after Terraform has begun applying.
func TestAccMonitorAssertions_invalidRejectedAtPlanTime(t *testing.T) {
	for _, tc := range []struct {
		name    string
		block   string
		wantErr *regexp.Regexp
	}{
		{
			name: "uncompilable regexp",
			block: `
  assertion {
    name  = "broken regex"
    kind  = "matches"
    value = "(unclosed"
  }`,
			wantErr: regexp.MustCompile(`not\s+a\s+valid\s+regular\s+expression`),
		},
		{
			name: "json_path carrying query syntax",
			block: `
  assertion {
    name  = "query path"
    kind  = "json_path"
    path  = "items[*].id"
    op    = "eq"
    value = "1"
  }`,
			wantErr: regexp.MustCompile(`query,\s+not\s+a\s+dotted\s+path`),
		},
		{
			name: "json_path without op",
			block: `
  assertion {
    name  = "no op"
    kind  = "json_path"
    path  = "result.rows"
    value = "1"
  }`,
			wantErr: regexp.MustCompile(`missing\s+op`),
		},
		{
			name: "contains without value",
			block: `
  assertion {
    name = "empty contains"
    kind = "contains"
  }`,
			wantErr: regexp.MustCompile(`missing\s+value`),
		},
		{
			name: "unknown kind",
			block: `
  assertion {
    name  = "telepathy"
    kind  = "telepathy"
    value = "x"
  }`,
			wantErr: regexp.MustCompile(`(?s)kind.*value\s+must\s+be\s+one\s+of`),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resource.Test(t, resource.TestCase{
				PreCheck:                 func() { testAccPreCheck(t) },
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{{
					Config: `
resource "lastping_monitor" "asrt_bad" {
  name          = "acc-assertions-bad"
  slug          = "acc-assertions-bad"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 600
` + tc.block + `
}`,
					PlanOnly:    true,
					ExpectError: tc.wantErr,
				}},
			})
		})
	}
}

// TestAccMonitorAssertions_rejectedOnHTTPMonitor: an http monitor is probed by
// LastPing and has no ping body, so an assertion on one would be stored and
// never evaluated. The API accepts it; the provider does not.
func TestAccMonitorAssertions_rejectedOnHTTPMonitor(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: `
resource "lastping_monitor" "asrt_http" {
  name             = "acc-assertions-http"
  slug             = "acc-assertions-http"
  monitor_type     = "http"
  probe_url        = "https://example.com/"
  probe_interval_s = 300

  assertion {
    name  = "body ok"
    kind  = "contains"
    value = "ok"
  }
}`,
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(`not\s+supported\s+on\s+an\s+http\s+monitor`),
		}},
	})
}

// testAccCheckMonitorAssertionCount asserts the SERVER's assertion count, not
// the state's. The two disagree exactly when a write was skipped — which is the
// failure the "remove every block" step is looking for, and one a state-only
// check cannot see: state would show no assertions because the configuration
// has none, while the monitor still carried them.
func testAccCheckMonitorAssertionCount(t *testing.T, resourceName string, want int) resource.TestCheckFunc {
	t.Helper()
	return resource.TestCheckResourceAttrWith(resourceName, "id", func(monitorID string) error {
		got, err := testAccDirectClient(t).GetAssertions(t.Context(), monitorID)
		if err != nil {
			return err
		}
		if len(got) != want {
			return fmt.Errorf("server holds %d assertions %v, want %d", len(got), got, want)
		}
		return nil
	})
}

// testAccMonitorIDBySlug resolves a monitor's UUID for an out-of-band call.
func testAccMonitorIDBySlug(t *testing.T, c *client.Client, slug string) string {
	t.Helper()
	m, err := c.GetMonitorBySlug(context.Background(), slug)
	require.NoError(t, err, "look up monitor %q", slug)
	return m.ID
}
