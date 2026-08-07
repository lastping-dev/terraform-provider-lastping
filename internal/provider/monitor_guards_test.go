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

// Acceptance tests for the `metric_guard` nested block. They are TF_ACC-gated
// and run against a live backend from the monorepo's CI, like the rest of this
// package.
//
// Every ExpectError pattern uses `\s+` rather than a literal space: Terraform
// re-wraps diagnostic text to the terminal width, so a literal space in the
// middle of a message is a newline as often as not.

// TestAccMonitorGuards_lifecycle is the whole contract in one resource: create
// with a guard, add one, change one, and remove them all.
//
// The removal step matters most. The block is Optional-only, so deleting every
// block plans as a null set — and the endpoint is replace-the-set, with no
// encoding for "leave them alone". A provider treating the null as "nothing to
// do" would make guards unremovable through Terraform.
func TestAccMonitorGuards_lifecycle(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_monitor" "guard" {
  name          = "acc-guards"
  slug          = "acc-guards"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 600

  metric_guard {
    name        = "daily spend"
    path        = "cost.usd"
    window_s    = 86400
    ceiling     = 50
    aggregation = "sum"
  }
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_monitor.guard", "metric_guard.#", "1"),
					resource.TestCheckTypeSetElemNestedAttrs("lastping_monitor.guard", "metric_guard.*",
						map[string]string{
							"name":        "daily spend",
							"path":        "cost.usd",
							"window_s":    "86400",
							"ceiling":     "50",
							"aggregation": "sum",
						}),
				),
			},
			{
				// Re-applying the identical configuration must plan empty. The
				// server returns guards ordered by (created_at, id) with a
				// random-UUID tie-break, so a list-typed block or an
				// order-sensitive comparison would show a diff here on most runs.
				Config: `
resource "lastping_monitor" "guard" {
  name          = "acc-guards"
  slug          = "acc-guards"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 600

  metric_guard {
    name        = "daily spend"
    path        = "cost.usd"
    window_s    = 86400
    ceiling     = 50
    aggregation = "sum"
  }
}`,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				// Add a second guard and change the first's ceiling and window.
				Config: `
resource "lastping_monitor" "guard" {
  name          = "acc-guards"
  slug          = "acc-guards"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 600

  metric_guard {
    name        = "daily spend"
    path        = "cost.usd"
    window_s    = 43200
    ceiling     = 25.5
    aggregation = "sum"
  }

  metric_guard {
    name        = "worst single run"
    path        = "usage.total_tokens"
    window_s    = 3600
    ceiling     = 200000
    aggregation = "max"
  }
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_monitor.guard", "metric_guard.#", "2"),
					resource.TestCheckTypeSetElemNestedAttrs("lastping_monitor.guard", "metric_guard.*",
						map[string]string{"name": "daily spend", "window_s": "43200", "ceiling": "25.5"}),
					resource.TestCheckTypeSetElemNestedAttrs("lastping_monitor.guard", "metric_guard.*",
						map[string]string{"name": "worst single run", "aggregation": "max"}),
				),
			},
			{
				// Every block removed: the set must be cleared server-side, not
				// left in place. The server-side count is the assertion that
				// matters — state would show no guards either way.
				Config: `
resource "lastping_monitor" "guard" {
  name          = "acc-guards"
  slug          = "acc-guards"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 600
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr("lastping_monitor.guard", "metric_guard.#"),
					testAccCheckMonitorGuardCount(t, "lastping_monitor.guard", 0),
				),
			},
			{
				ResourceName:      "lastping_monitor.guard",
				ImportState:       true,
				ImportStateId:     "acc-guards",
				ImportStateVerify: true,
			},
		},
	})
}

// A monitor may carry both kinds of block at once, and the two sub-resources
// must not interfere: each has its own endpoint, its own read and its own
// replace-the-set write. A provider that reused one sync path for both would
// clear one set while writing the other.
func TestAccMonitorGuards_coexistWithAssertions(t *testing.T) {
	const cfg = `
resource "lastping_monitor" "both" {
  name          = "acc-guards-both"
  slug          = "acc-guards-both"
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

  metric_guard {
    name        = "daily spend"
    path        = "cost.usd"
    window_s    = 86400
    ceiling     = 50
    aggregation = "sum"
  }
}`

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: cfg,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_monitor.both", "assertion.#", "1"),
					resource.TestCheckResourceAttr("lastping_monitor.both", "metric_guard.#", "1"),
					testAccCheckMonitorGuardCount(t, "lastping_monitor.both", 1),
					testAccCheckMonitorAssertionCount(t, "lastping_monitor.both", 1),
				),
			},
			{
				Config: cfg,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}

// TestAccMonitorGuards_importCarriesTheSet covers the read path on its own.
// Guards live behind a second endpoint, so a Read that forgets to call it
// imports a monitor with an empty guard set and the next plan proposes deleting
// guards that are actually configured.
func TestAccMonitorGuards_importCarriesTheSet(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_monitor" "guard_import" {
  name          = "acc-guards-import"
  slug          = "acc-guards-import"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 600

  metric_guard {
    name        = "daily spend"
    path        = "cost.usd"
    window_s    = 86400
    ceiling     = 50
    aggregation = "sum"
  }

  metric_guard {
    name        = "average call"
    path        = "usage.total_tokens"
    window_s    = 3600
    ceiling     = 12000
    aggregation = "avg"
  }
}`,
			},
			{
				ResourceName:      "lastping_monitor.guard_import",
				ImportState:       true,
				ImportStateId:     "acc-guards-import",
				ImportStateVerify: true,
			},
		},
	})
}

// A guard changed outside Terraform must show up as a diff. Read is the only
// place that can notice, and it is the call most easily forgotten, because
// everything else about the resource keeps working without it.
func TestAccMonitorGuards_driftIsDetected(t *testing.T) {
	const cfg = `
resource "lastping_monitor" "guard_drift" {
  name          = "acc-guards-drift"
  slug          = "acc-guards-drift"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 600

  metric_guard {
    name        = "daily spend"
    path        = "cost.usd"
    window_s    = 86400
    ceiling     = 50
    aggregation = "sum"
  }
}`

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: cfg},
			{
				// Raise the ceiling out of band, then plan: the provider must
				// propose putting the configured one back. A ceiling-only change
				// is the case an equality check that ignored the number would
				// miss entirely.
				PreConfig: func() {
					c := testAccDirectClient(t)
					id := testAccMonitorIDBySlug(t, c, "acc-guards-drift")
					_, err := c.PutMetricGuards(context.Background(), id, []client.MetricGuard{
						{Name: "daily spend", Path: "cost.usd", WindowS: 86400, Ceiling: 5000, Aggregation: "sum"},
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

// Every one of these is a 400 from the API (or a guard that could never fire),
// anticipated at plan time so the error names the guard instead of arriving
// after Terraform has begun applying.
func TestAccMonitorGuards_invalidRejectedAtPlanTime(t *testing.T) {
	for _, tc := range []struct {
		name    string
		block   string
		wantErr *regexp.Regexp
	}{
		{
			name: "path carrying query syntax",
			block: `
  metric_guard {
    name        = "queryish"
    path        = "items[*].cost"
    window_s    = 3600
    ceiling     = 5
    aggregation = "sum"
  }`,
			wantErr: regexp.MustCompile(`query,\s+not\s+a\s+dotted\s+path`),
		},
		{
			name: "window past the cap",
			block: `
  metric_guard {
    name        = "monthly spend"
    path        = "cost.usd"
    window_s    = 2592000
    ceiling     = 500
    aggregation = "sum"
  }`,
			wantErr: regexp.MustCompile(`(?s)window_s.*604800`),
		},
		{
			name: "zero window",
			block: `
  metric_guard {
    name        = "instant"
    path        = "cost.usd"
    window_s    = 0
    ceiling     = 5
    aggregation = "sum"
  }`,
			wantErr: regexp.MustCompile(`(?s)window_s.*at\s+least\s+1`),
		},
		{
			name: "unknown aggregation",
			block: `
  metric_guard {
    name        = "medianish"
    path        = "cost.usd"
    window_s    = 3600
    ceiling     = 5
    aggregation = "median"
  }`,
			wantErr: regexp.MustCompile(`(?s)aggregation.*value\s+must\s+be\s+one\s+of`),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resource.Test(t, resource.TestCase{
				PreCheck:                 func() { testAccPreCheck(t) },
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{{
					Config: `
resource "lastping_monitor" "guard_bad" {
  name          = "acc-guards-bad"
  slug          = "acc-guards-bad"
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

// A guard on an http monitor is refused at plan time: a probe has no ping body,
// so the guard would be stored and never evaluated — a silent monitoring hole
// rather than an error the operator would ever see.
func TestAccMonitorGuards_refusedOnHTTPMonitor(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: `
resource "lastping_monitor" "guard_http" {
  name              = "acc-guards-http"
  slug              = "acc-guards-http"
  monitor_type      = "http"
  probe_url         = "https://example.test/health"
  probe_interval_s  = 300

  metric_guard {
    name        = "daily spend"
    path        = "cost.usd"
    window_s    = 86400
    ceiling     = 50
    aggregation = "sum"
  }
}`,
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(`not\s+supported\s+on\s+an\s+http\s+monitor`),
		}},
	})
}

// The cap is 5. Six blocks must be refused at plan time rather than as a 400
// partway through an apply.
func TestAccMonitorGuards_capRejectedAtPlanTime(t *testing.T) {
	block := func(i int) string {
		return fmt.Sprintf(`
  metric_guard {
    name        = "guard %d"
    path        = "cost.usd"
    window_s    = 3600
    ceiling     = %d
    aggregation = "sum"
  }`, i, i+1)
	}
	cfg := `
resource "lastping_monitor" "guard_cap" {
  name          = "acc-guards-cap"
  slug          = "acc-guards-cap"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 600
`
	for i := 0; i < maxGuardsPerMonitor+1; i++ {
		cfg += block(i)
	}
	cfg += "\n}"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config:      cfg,
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(`Too\s+many\s+metric\s+guards`),
		}},
	})
}

// testAccCheckMonitorGuardCount asks the API how many guards the monitor really
// has, rather than trusting the state's view. The two disagree exactly when a
// write was skipped — the failure the "remove every block" step is looking for,
// and one a state-only check cannot see.
func testAccCheckMonitorGuardCount(t *testing.T, resourceName string, want int) resource.TestCheckFunc {
	t.Helper()
	return resource.TestCheckResourceAttrWith(resourceName, "id", func(monitorID string) error {
		got, err := testAccDirectClient(t).GetMetricGuards(t.Context(), monitorID)
		if err != nil {
			return err
		}
		if len(got) != want {
			return fmt.Errorf("server holds %d metric guards %v, want %d", len(got), got, want)
		}
		return nil
	})
}
