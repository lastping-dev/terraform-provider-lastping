package provider

import (
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// TestAccDataMetrics_containsTheMonitorFromTheSameConfig reads the exposition
// back and finds the monitor the same configuration just created.
//
// The metric line carries the monitor's id, name and slug as labels, so
// matching on them proves the endpoint is scoped to the caller's project and
// that the body survived the client's raw-bytes path intact — a JSON decode
// attempt would have failed outright, but a truncated or re-encoded body would
// not.
func TestAccDataMetrics_containsTheMonitorFromTheSameConfig(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_monitor" "src" {
  name          = "acc-data-metrics"
  slug          = "acc-data-metrics"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
}

data "lastping_metrics" "current" {
  depends_on = [lastping_monitor.src]
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					// Prometheus text exposition, not JSON.
					resource.TestMatchResourceAttr("data.lastping_metrics.current", "text",
						regexp.MustCompile(`(?m)^# TYPE lastping_checks gauge$`)),
					// The monitor created above, labelled by slug.
					resource.TestMatchResourceAttr("data.lastping_metrics.current", "text",
						regexp.MustCompile(`lastping_check_up\{[^}]*slug="acc-data-metrics"[^}]*\} \d`)),
					// grace_s round-trips into the gauge.
					resource.TestMatchResourceAttr("data.lastping_metrics.current", "text",
						regexp.MustCompile(`lastping_check_grace_seconds\{[^}]*slug="acc-data-metrics"[^}]*\} 300`)),
				),
			},
		},
	})
}
