package provider

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/stretchr/testify/require"

	"github.com/lastping-dev/terraform-provider-lastping/internal/client"
)

// testAccPreCheck skips acceptance tests unless a backend is configured, and
// then hard-fails unless that backend's project has a verified email channel.
//
// The second half exists because a project without one hid a real bug: the API
// only auto-routes a new monitor's down/fail/recovery events when it has a
// default email channel to route to (attachDefaultRoutes no-ops otherwise), so
// the auto-route/adoption scenario simply never arose against an unseeded
// project, and 43 passing acceptance tests never touched it. That is not a
// property of one or two route tests — it is a property of the fixture the
// whole suite runs against — so the requirement lives here, not in individual
// tests' PreChecks, and applies to every acceptance test without exception.
//
// These run from the monorepo CI against docker-compose, not from public CI.
func testAccPreCheck(t *testing.T) {
	t.Helper()
	if os.Getenv("LASTPING_API_KEY") == "" {
		t.Skip("LASTPING_API_KEY not set; skipping acceptance test")
	}
	testAccDefaultEmailDestinationID(t)
}

// testAccDirectClient returns a client that talks to the same backend as the
// provider under test, for out-of-band calls the provider itself must not be
// aware of (see TestAccMonitor_tagsClearedOutOfBand).
func testAccDirectClient(t *testing.T) *client.Client {
	t.Helper()
	endpoint := os.Getenv("LASTPING_ENDPOINT")
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	return client.New(endpoint, os.Getenv("LASTPING_API_KEY"), "acc-test")
}

func TestAccMonitor_basic(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_monitor" "test" {
  name          = "acc-basic"
  slug          = "acc-basic"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
  tags          = ["acc:test"]
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_monitor.test", "name", "acc-basic"),
					resource.TestCheckResourceAttr("lastping_monitor.test", "period_s", "3600"),
					resource.TestCheckResourceAttrSet("lastping_monitor.test", "id"),
					resource.TestCheckResourceAttrSet("lastping_monitor.test", "ping_url"),
				),
			},
			{
				// Update in place — must not force replacement.
				Config: `
resource "lastping_monitor" "test" {
  name          = "acc-basic-renamed"
  slug          = "acc-basic"
  schedule_kind = "simple"
  period_s      = 7200
  grace_s       = 300
  tags          = ["acc:test"]
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_monitor.test", "name", "acc-basic-renamed"),
					resource.TestCheckResourceAttr("lastping_monitor.test", "period_s", "7200"),
				),
			},
			{
				ResourceName:      "lastping_monitor.test",
				ImportState:       true,
				ImportStateId:     "acc-basic",
				ImportStateVerify: true,
			},
		},
	})
}

func TestAccMonitor_cron(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			// grace_s is required by the API (validated [60, 31536000]); it is
			// included here even though early drafts of this test omitted it.
			Config: `
resource "lastping_monitor" "cron" {
  name          = "acc-cron"
  slug          = "acc-cron"
  schedule_kind = "cron"
  cron_expr     = "0 3 * * *"
  tz            = "Europe/London"
  grace_s       = 300
}`,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("lastping_monitor.cron", "cron_expr", "0 3 * * *"),
				resource.TestCheckResourceAttr("lastping_monitor.cron", "tz", "Europe/London"),
			),
		}},
	})
}

// TestAccMonitor_httpProbe covers monitor_type = "http". The API floors the
// effective grace to 2*probe_interval_s (api/checks.go: createHTTPCheckFromValidated),
// so a configuration that asks for less must still apply: with grace_s Required
// (non-computed) Terraform rejects the server's larger value as an inconsistent
// result after apply, which made http monitors impossible to create at all.
func TestAccMonitor_httpProbe(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// grace_s omitted: the server derives 2*probe_interval_s = 600.
				Config: `
resource "lastping_monitor" "probe" {
  name             = "acc-http"
  slug             = "acc-http"
  monitor_type     = "http"
  probe_url        = "https://example.com/"
  probe_interval_s = 300
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_monitor.probe", "monitor_type", "http"),
					resource.TestCheckResourceAttr("lastping_monitor.probe", "probe_interval_s", "300"),
					// Server floor, not a configured value.
					resource.TestCheckResourceAttr("lastping_monitor.probe", "grace_s", "600"),
					// Derived server-side from probe_interval_s.
					resource.TestCheckResourceAttr("lastping_monitor.probe", "schedule_kind", "simple"),
					resource.TestCheckResourceAttr("lastping_monitor.probe", "period_s", "300"),
					resource.TestCheckResourceAttr("lastping_monitor.probe", "probe_timeout_s", "10"),
				),
			},
			{
				// A grace_s above the floor is honoured as-is.
				Config: `
resource "lastping_monitor" "probe" {
  name             = "acc-http"
  slug             = "acc-http"
  monitor_type     = "http"
  probe_url        = "https://example.com/"
  probe_interval_s = 300
  grace_s          = 900
}`,
				Check: resource.TestCheckResourceAttr("lastping_monitor.probe", "grace_s", "900"),
			},
			{
				ResourceName:      "lastping_monitor.probe",
				ImportState:       true,
				ImportStateId:     "acc-http",
				ImportStateVerify: true,
			},
		},
	})
}

// TestAccMonitor_httpProbeGraceBelowFloor asserts the below-the-floor case is
// rejected at plan time with an actionable message rather than blowing up during
// apply with "inconsistent result after apply".
func TestAccMonitor_httpProbeGraceBelowFloor(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: `
resource "lastping_monitor" "probe" {
  name             = "acc-http-floor"
  slug             = "acc-http-floor"
  monitor_type     = "http"
  probe_url        = "https://example.com/"
  probe_interval_s = 300
  grace_s          = 120
}`,
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(`grace_s.*(floor|at least 600)`),
		}},
	})
}

// TestAccMonitor_maxRuntimeRejectedOnHTTP asserts the API's 400
// MAX_RUNTIME_NOT_SUPPORTED is anticipated at plan time. An http probe has no
// start/success pair, so the overrun rule the attribute configures can never
// fire on one.
func TestAccMonitor_maxRuntimeRejectedOnHTTP(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: `
resource "lastping_monitor" "probe" {
  name             = "acc-http-maxruntime"
  slug             = "acc-http-maxruntime"
  monitor_type     = "http"
  probe_url        = "https://example.com/"
  probe_interval_s = 300
  max_runtime_s    = 14400
}`,
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(`max_runtime_s.*not supported`),
		}},
	})
}

// TestAccMonitor_failureThreshold covers the retry-before-alert setting end to
// end. It is Optional+Computed and NOT NULL DEFAULT 1 server-side, so the two
// things worth pinning are that a configured value survives the round trip and
// that omitting it does NOT reset the stored value — there is no cleared state
// for the API to go back to, and pretending otherwise would produce a perpetual
// diff.
func TestAccMonitor_failureThreshold(t *testing.T) {
	const withThreshold = `
resource "lastping_monitor" "ft" {
  name              = "acc-failure-threshold"
  slug              = "acc-failure-threshold"
  schedule_kind     = "simple"
  period_s          = 3600
  grace_s           = 300
  failure_threshold = 3
}`
	const withoutThreshold = `
resource "lastping_monitor" "ft" {
  name          = "acc-failure-threshold"
  slug          = "acc-failure-threshold"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
}`

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// Omitted first: the server's own default has to land in state,
				// or the very next plan shows a diff against nothing.
				Config: withoutThreshold,
				Check:  resource.TestCheckResourceAttr("lastping_monitor.ft", "failure_threshold", "1"),
			},
			{
				Config: withThreshold,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_monitor.ft", "failure_threshold", "3"),
					resource.TestCheckResourceAttrWith("lastping_monitor.ft", "id", func(id string) error {
						mon, err := testAccDirectClient(t).GetMonitor(t.Context(), id)
						if err != nil {
							return err
						}
						if mon.FailureThreshold != 3 {
							return fmt.Errorf("server holds failure_threshold=%d, want 3", mon.FailureThreshold)
						}
						return nil
					}),
				),
			},
			{
				// Removing it keeps 3: Optional+Computed, and the column has no
				// null state. The plan must be empty rather than proposing 1.
				Config:   withoutThreshold,
				PlanOnly: true,
			},
			{
				ResourceName:      "lastping_monitor.ft",
				ImportState:       true,
				ImportStateId:     "acc-failure-threshold",
				ImportStateVerify: true,
			},
		},
	})
}

// TestAccMonitor_maxRuntimeCanBeRemoved is the clearable half of the pair, and
// the reason max_runtime_s sits in monitorPatchFromModel's explicit-null group
// rather than the omit-when-zero one: an absent key under merge-patch leaves the
// stored budget in place, so "go back to the grace_s fallback" would be
// unreachable — exactly the bug tags, runaway_ceiling and monitor_from had.
func TestAccMonitor_maxRuntimeCanBeRemoved(t *testing.T) {
	const withRuntime = `
resource "lastping_monitor" "mr" {
  name          = "acc-max-runtime"
  slug          = "acc-max-runtime"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 600
  max_runtime_s = 14400
}`
	const withoutRuntime = `
resource "lastping_monitor" "mr" {
  name          = "acc-max-runtime"
  slug          = "acc-max-runtime"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 600
}`

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: withRuntime,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_monitor.mr", "max_runtime_s", "14400"),
					// grace_s is untouched by it: the two govern different
					// deadlines, which is the whole point of the attribute.
					resource.TestCheckResourceAttr("lastping_monitor.mr", "grace_s", "600"),
				),
			},
			{
				Config: withoutRuntime,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr("lastping_monitor.mr", "max_runtime_s"),
					// Read it back from the API: state agreeing with the plan
					// proves nothing when the plan said "removed" and the server
					// was never told.
					resource.TestCheckResourceAttrWith("lastping_monitor.mr", "id", func(id string) error {
						mon, err := testAccDirectClient(t).GetMonitor(t.Context(), id)
						if err != nil {
							return err
						}
						if mon.MaxRuntimeS != nil {
							return fmt.Errorf("server still holds max_runtime_s=%d, want it cleared",
								*mon.MaxRuntimeS)
						}
						return nil
					}),
				),
			},
			{
				// And it stays cleared: no perpetual diff on re-plan.
				Config:   withoutRuntime,
				PlanOnly: true,
			},
		},
	})
}

// TestAccMonitor_agentIDAttachAndDetach is the clearable proof for agent_id,
// the same shape as TestAccMonitor_maxRuntimeCanBeRemoved: attach at create,
// re-attach to a different agent in place (no replacement), then remove the
// attribute and confirm the server actually cleared it rather than trusting
// state, which would prove nothing if Terraform merely stopped asking.
//
// It is also the only acceptance test that exercises agent_id and a
// lastping_agent resource in the same apply, which is the scenario the whole
// feature exists for: a Terraform-managed fleet where monitor_count and
// status roll up from monitors this same configuration attached.
//
// monitor_count assertions live in their own RefreshState steps rather than
// in the Check of the step that attaches or detaches the monitor. Within a
// single apply, lastping_agent.a is read (Create's response, in the attach
// case) before lastping_monitor.ag is created against it, and nothing forces
// a second read of the agent afterwards - Terraform does not re-read a
// resource's computed attributes just because a different resource's apply,
// later in the same plan, made them stale. So immediately after the apply
// that attaches or detaches, state still holds whatever monitor_count was as
// of the agent's own last read: the pre-attach value. A dedicated
// RefreshState step forces the re-read `terraform refresh` would do, which is
// the only way this ever becomes visible outside the provider - see the
// `monitor_count` schema description for the same caveat spelled out for
// practitioners.
func TestAccMonitor_agentIDAttachAndDetach(t *testing.T) {
	const attachedToFirst = `
resource "lastping_agent" "a" {
  name = "acc-agent-id-first"
}
resource "lastping_agent" "b" {
  name = "acc-agent-id-second"
}
resource "lastping_monitor" "ag" {
  name          = "acc-agent-id"
  slug          = "acc-agent-id"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 600
  agent_id      = lastping_agent.a.id
}`
	const attachedToSecond = `
resource "lastping_agent" "a" {
  name = "acc-agent-id-first"
}
resource "lastping_agent" "b" {
  name = "acc-agent-id-second"
}
resource "lastping_monitor" "ag" {
  name          = "acc-agent-id"
  slug          = "acc-agent-id"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 600
  agent_id      = lastping_agent.b.id
}`
	const detached = `
resource "lastping_agent" "a" {
  name = "acc-agent-id-first"
}
resource "lastping_agent" "b" {
  name = "acc-agent-id-second"
}
resource "lastping_monitor" "ag" {
  name          = "acc-agent-id"
  slug          = "acc-agent-id"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 600
}`

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: attachedToFirst,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair("lastping_monitor.ag", "agent_id", "lastping_agent.a", "id"),
				),
			},
			{
				// Proves the attach actually took: force the re-read that
				// `terraform refresh` would do, and only then check
				// monitor_count. Checking it as part of the step above would
				// prove nothing either way - lastping_agent.a is read before
				// lastping_monitor.ag is created against it in that same
				// apply, so it would read 0 whether or not the attach
				// server-side worked.
				RefreshState: true,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_agent.a", "monitor_count", "1"),
					resource.TestCheckResourceAttr("lastping_agent.b", "monitor_count", "0"),
				),
			},
			{
				// Re-attaching to a different agent is an in-place PATCH, not a
				// replacement: the API supports agent_id on PATCH, so there is
				// nothing here that forces RequiresReplace.
				Config: attachedToSecond,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("lastping_monitor.ag", plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair("lastping_monitor.ag", "agent_id", "lastping_agent.b", "id"),
				),
			},
			{
				// Same reasoning as the RefreshState step above, for the
				// move from agent a to agent b.
				RefreshState: true,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_agent.a", "monitor_count", "0"),
					resource.TestCheckResourceAttr("lastping_agent.b", "monitor_count", "1"),
				),
			},
			{
				Config: detached,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr("lastping_monitor.ag", "agent_id"),
					// Read it back from the API: state agreeing with the plan
					// proves nothing when the plan said "removed" and the server
					// was never told.
					resource.TestCheckResourceAttrWith("lastping_monitor.ag", "id", func(id string) error {
						mon, err := testAccDirectClient(t).GetMonitor(t.Context(), id)
						if err != nil {
							return err
						}
						if mon.AgentID != "" {
							return fmt.Errorf("server still holds agent_id=%q, want it cleared", mon.AgentID)
						}
						return nil
					}),
				),
			},
			{
				// Same reasoning again, for the detach.
				RefreshState: true,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_agent.b", "monitor_count", "0"),
				),
			},
			{
				// And it stays cleared: no perpetual diff on re-plan.
				Config:   detached,
				PlanOnly: true,
			},
		},
	})
}

// TestAccMonitor_stepTimeoutCanBeRemoved is the clearable proof for
// step_timeout_s, and the reason it sits in monitorPatchFromModel's
// explicit-null group rather than the omit-when-zero one.
//
// Under merge-patch an absent key leaves the stored timeout in place, so a
// step_timeout_s in the omit-when-zero group could be set through Terraform and
// then never turned off again: the practitioner deletes the attribute, plan and
// state both say it is gone, and the monitor keeps opening `stalled` incidents
// nobody can find the source of. That is exactly the bug tags, runaway_ceiling
// and monitor_from shipped with, and the reason the API reads step_timeout_s
// null as "clear" at all.
//
// The middle step therefore reads the monitor back through the API rather than
// trusting state: state agreeing with the plan proves nothing when the plan
// said "removed" and the server was never told.
func TestAccMonitor_stepTimeoutCanBeRemoved(t *testing.T) {
	const withStep = `
resource "lastping_monitor" "st" {
  name           = "acc-step-timeout"
  slug           = "acc-step-timeout"
  schedule_kind  = "simple"
  period_s       = 3600
  grace_s        = 600
  max_runtime_s  = 14400
  step_timeout_s = 900
}`
	const withoutStep = `
resource "lastping_monitor" "st" {
  name          = "acc-step-timeout"
  slug          = "acc-step-timeout"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 600
  max_runtime_s = 14400
}`

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: withStep,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_monitor.st", "step_timeout_s", "900"),
					// Neither budget is disturbed by it: the three clocks are
					// independent, which is the point of the attribute.
					resource.TestCheckResourceAttr("lastping_monitor.st", "grace_s", "600"),
					resource.TestCheckResourceAttr("lastping_monitor.st", "max_runtime_s", "14400"),
					resource.TestCheckResourceAttrWith("lastping_monitor.st", "id", func(id string) error {
						mon, err := testAccDirectClient(t).GetMonitor(t.Context(), id)
						if err != nil {
							return err
						}
						if mon.StepTimeoutS == nil || *mon.StepTimeoutS != 900 {
							return fmt.Errorf("server holds step_timeout_s=%v, want 900", mon.StepTimeoutS)
						}
						return nil
					}),
				),
			},
			{
				Config: withoutStep,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr("lastping_monitor.st", "step_timeout_s"),
					resource.TestCheckResourceAttrWith("lastping_monitor.st", "id", func(id string) error {
						mon, err := testAccDirectClient(t).GetMonitor(t.Context(), id)
						if err != nil {
							return err
						}
						if mon.StepTimeoutS != nil {
							return fmt.Errorf("server still holds step_timeout_s=%d, want it cleared",
								*mon.StepTimeoutS)
						}
						// The clear must not have taken the neighbouring budget
						// with it: both are sent as explicit nulls or values in
						// the same document.
						if mon.MaxRuntimeS == nil || *mon.MaxRuntimeS != 14400 {
							return fmt.Errorf("clearing step_timeout_s disturbed max_runtime_s=%v",
								mon.MaxRuntimeS)
						}
						return nil
					}),
				),
			},
			{
				// And it stays cleared: no perpetual diff on re-plan.
				Config:   withoutStep,
				PlanOnly: true,
			},
			{
				ResourceName:      "lastping_monitor.st",
				ImportState:       true,
				ImportStateId:     "acc-step-timeout",
				ImportStateVerify: true,
			},
		},
	})
}

// TestAccMonitor_stepTimeoutRejectedOnHTTP asserts the API's 400
// STEP_TIMEOUT_NOT_SUPPORTED is anticipated at plan time. An http probe never
// arms a run and has no /step endpoint, so the stall rule the attribute
// configures is unreachable on one.
func TestAccMonitor_stepTimeoutRejectedOnHTTP(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: `
resource "lastping_monitor" "probe" {
  name             = "acc-http-steptimeout"
  slug             = "acc-http-steptimeout"
  monitor_type     = "http"
  probe_url        = "https://example.com/"
  probe_interval_s = 300
  step_timeout_s   = 300
}`,
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(`step_timeout_s.*not supported`),
		}},
	})
}

// TestAccMonitor_stepTimeoutAboveBudgetRejected covers the interaction rule the
// server enforces as 400 STEP_TIMEOUT_EXCEEDS_BUDGET.
//
// The first step is the shape that is easiest to write by accident: no
// max_runtime_s, so the budget is grace_s, and a step timeout at or above it
// leaves an empty stall window. The API refuses it rather than storing a rule
// that can never fire, and the provider says so at plan time whenever both
// values are known.
//
// The second step is the guard against over-eager validation: the same 600 is
// legitimate once max_runtime_s raises the budget above it, and a plan-time
// check that got the COALESCE backwards would refuse a configuration the API
// accepts — an error the practitioner could do nothing about.
func TestAccMonitor_stepTimeoutAboveBudgetRejected(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_monitor" "budget" {
  name           = "acc-step-budget"
  slug           = "acc-step-budget"
  schedule_kind  = "simple"
  period_s       = 3600
  grace_s        = 300
  step_timeout_s = 600
}`,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`step_timeout_s.*(budget|less than)`),
			},
			{
				Config: `
resource "lastping_monitor" "budget" {
  name           = "acc-step-budget"
  slug           = "acc-step-budget"
  schedule_kind  = "simple"
  period_s       = 3600
  grace_s        = 300
  max_runtime_s  = 14400
  step_timeout_s = 600
}`,
				Check: resource.TestCheckResourceAttr("lastping_monitor.budget", "step_timeout_s", "600"),
			},
		},
	})
}

// TestAccMonitor_monitorFromRoundTrips pins the offset-timestamp round trip: the
// API stores and returns UTC, so a configured +01:00 timestamp comes back as Z.
// The provider must treat the two as the same instant instead of reporting an
// inconsistent result after apply.
func TestAccMonitor_monitorFromRoundTrips(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_monitor" "mf" {
  name          = "acc-monitor-from"
  slug          = "acc-monitor-from"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
  monitor_from  = "2027-01-01T00:00:00+01:00"
}`,
				Check: resource.TestCheckResourceAttr(
					"lastping_monitor.mf", "monitor_from", "2027-01-01T00:00:00+01:00"),
			},
			{
				// And it stays stable: no perpetual diff on re-plan.
				Config: `
resource "lastping_monitor" "mf" {
  name          = "acc-monitor-from"
  slug          = "acc-monitor-from"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
  monitor_from  = "2027-01-01T00:00:00+01:00"
}`,
				PlanOnly: true,
			},
		},
	})
}

// TestAccMonitor_tagsCanBeRemoved is the "Optional + Computed makes attributes
// unsettable" regression: dropping tags from the configuration must clear them,
// not leave the previous value pinned in state forever.
func TestAccMonitor_tagsCanBeRemoved(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_monitor" "tagged" {
  name          = "acc-tags"
  slug          = "acc-tags"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
  tags          = ["env:prod"]
  cron_expr     = null
}`,
				Check: resource.TestCheckResourceAttr("lastping_monitor.tagged", "tags.#", "1"),
			},
			{
				Config: `
resource "lastping_monitor" "tagged" {
  name          = "acc-tags"
  slug          = "acc-tags"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr("lastping_monitor.tagged", "tags.#"),
					resource.TestCheckNoResourceAttr("lastping_monitor.tagged", "tags.0"),
				),
			},
			{
				// And the server really did drop them.
				PlanOnly: true,
				Config: `
resource "lastping_monitor" "tagged" {
  name          = "acc-tags"
  slug          = "acc-tags"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
}`,
			},
		},
	})
}

// TestAccMonitor_runawayCeilingCanBeRemoved is `tags` clearing for the other
// null-clearable numeric field, which had no coverage at all.
//
// runaway_ceiling is a *int64 on the wire. `omitempty` dropped the key when the
// attribute was removed from the configuration, which a merge-patch server
// reads as "leave it alone" — the ceiling stayed on the monitor and kept
// opening runaway incidents nobody could turn off through Terraform.
func TestAccMonitor_runawayCeilingCanBeRemoved(t *testing.T) {
	const withCeiling = `
resource "lastping_monitor" "runaway" {
  name            = "acc-runaway"
  slug            = "acc-runaway"
  schedule_kind   = "simple"
  period_s        = 3600
  grace_s         = 300
  runaway_ceiling = 40
}`
	const withoutCeiling = `
resource "lastping_monitor" "runaway" {
  name          = "acc-runaway"
  slug          = "acc-runaway"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
}`

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: withCeiling,
				Check:  resource.TestCheckResourceAttr("lastping_monitor.runaway", "runaway_ceiling", "40"),
			},
			{
				Config: withoutCeiling,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr("lastping_monitor.runaway", "runaway_ceiling"),
					// Read it back from the API: state agreeing with the plan
					// proves nothing when the plan said "removed" and the
					// server was never told.
					resource.TestCheckResourceAttrWith("lastping_monitor.runaway", "id", func(id string) error {
						mon, err := testAccDirectClient(t).GetMonitor(t.Context(), id)
						if err != nil {
							return err
						}
						if mon.RunawayCeiling != nil {
							return fmt.Errorf("server still holds runaway_ceiling=%d, want it cleared",
								*mon.RunawayCeiling)
						}
						return nil
					}),
				),
			},
			{
				// And it stays cleared: no perpetual diff on re-plan.
				Config:   withoutCeiling,
				PlanOnly: true,
			},
		},
	})
}

// TestAccMonitor_monitorFromCanBeRemoved covers the third null-clearable
// attribute. Same mechanism as runaway_ceiling: a *string that `omitempty`
// dropped, leaving the stored anchor timestamp in place forever.
func TestAccMonitor_monitorFromCanBeRemoved(t *testing.T) {
	const withFrom = `
resource "lastping_monitor" "mfclear" {
  name          = "acc-monitor-from-clear"
  slug          = "acc-monitor-from-clear"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
  monitor_from  = "2027-01-01T00:00:00Z"
}`
	const withoutFrom = `
resource "lastping_monitor" "mfclear" {
  name          = "acc-monitor-from-clear"
  slug          = "acc-monitor-from-clear"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
}`

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: withFrom,
				Check: resource.TestCheckResourceAttr(
					"lastping_monitor.mfclear", "monitor_from", "2027-01-01T00:00:00Z"),
			},
			{
				Config: withoutFrom,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr("lastping_monitor.mfclear", "monitor_from"),
					resource.TestCheckResourceAttrWith("lastping_monitor.mfclear", "id", func(id string) error {
						mon, err := testAccDirectClient(t).GetMonitor(t.Context(), id)
						if err != nil {
							return err
						}
						if mon.MonitorFrom != nil && *mon.MonitorFrom != "" {
							return fmt.Errorf("server still holds monitor_from=%q, want it cleared",
								*mon.MonitorFrom)
						}
						return nil
					}),
				),
			},
			{
				Config:   withoutFrom,
				PlanOnly: true,
			},
		},
	})
}

// TestAccMonitor_probeExpectedBodyCanBeRemoved is the fourth broken field, and
// the other meaningful-zero one.
//
// probe_expected_body is not null-clearable server-side: "" is what clears it,
// and "" is exactly what `omitempty` used to drop. So removing the assertion
// from the configuration left the probe still requiring the substring while
// plan and state both agreed it was gone — a monitor that keeps failing on a
// rule its own configuration no longer contains.
func TestAccMonitor_probeExpectedBodyCanBeRemoved(t *testing.T) {
	const withBody = `
resource "lastping_monitor" "body" {
  name                = "acc-expected-body"
  slug                = "acc-expected-body"
  monitor_type        = "http"
  probe_url           = "https://example.com/health"
  probe_interval_s    = 300
  probe_expected_body = "ok"
}`
	const withoutBody = `
resource "lastping_monitor" "body" {
  name             = "acc-expected-body"
  slug             = "acc-expected-body"
  monitor_type     = "http"
  probe_url        = "https://example.com/health"
  probe_interval_s = 300
}`

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: withBody,
				Check: resource.TestCheckResourceAttr(
					"lastping_monitor.body", "probe_expected_body", "ok"),
			},
			{
				Config: withoutBody,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr("lastping_monitor.body", "probe_expected_body"),
					resource.TestCheckResourceAttrWith("lastping_monitor.body", "id", func(id string) error {
						mon, err := testAccDirectClient(t).GetMonitor(t.Context(), id)
						if err != nil {
							return err
						}
						if mon.ProbeExpectedBody != "" {
							return fmt.Errorf("server still holds probe_expected_body=%q, want it cleared",
								mon.ProbeExpectedBody)
						}
						return nil
					}),
				),
			},
			{
				Config:   withoutBody,
				PlanOnly: true,
			},
		},
	})
}

// TestAccMonitor_probeFollowRedirectsCanBeTurnedOff is the meaningful-zero half
// of the same bug, and it needs no attribute removal at all to bite.
//
// probe_follow_redirects defaults to true server-side. `omitempty` on a bool
// drops the key when it is false, so `probe_follow_redirects = false` — a value
// the practitioner wrote explicitly — produced the same bytes as not
// configuring it. The stored `true` survived, and the probe kept following
// redirects while both the plan and state said it did not.
func TestAccMonitor_probeFollowRedirectsCanBeTurnedOff(t *testing.T) {
	const cfg = `
resource "lastping_monitor" "redir" {
  name                   = "acc-redirects"
  slug                   = "acc-redirects"
  monitor_type           = "http"
  probe_url              = "https://example.com/"
  probe_interval_s       = 300
  probe_follow_redirects = %s
}`

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(cfg, "true"),
				Check: resource.TestCheckResourceAttr(
					"lastping_monitor.redir", "probe_follow_redirects", "true"),
			},
			{
				Config: fmt.Sprintf(cfg, "false"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_monitor.redir", "probe_follow_redirects", "false"),
					resource.TestCheckResourceAttrWith("lastping_monitor.redir", "id", func(id string) error {
						mon, err := testAccDirectClient(t).GetMonitor(t.Context(), id)
						if err != nil {
							return err
						}
						if mon.ProbeFollowRedirects {
							return fmt.Errorf("server still follows redirects, want it turned off")
						}
						return nil
					}),
				),
			},
			{
				Config:   fmt.Sprintf(cfg, "false"),
				PlanOnly: true,
			},
		},
	})
}

// TestAccMonitor_pause exercises the update path where the only change is
// `paused`: it maps onto the pause/resume endpoints and must not also PATCH the
// monitor's configuration.
func TestAccMonitor_pause(t *testing.T) {
	const cfg = `
resource "lastping_monitor" "p" {
  name          = "acc-pause"
  slug          = "acc-pause"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
  paused        = %s
}`
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(cfg, "false"),
				Check:  resource.TestCheckResourceAttr("lastping_monitor.p", "paused", "false"),
			},
			{
				Config: fmt.Sprintf(cfg, "true"),
				Check:  resource.TestCheckResourceAttr("lastping_monitor.p", "paused", "true"),
			},
			{
				Config: fmt.Sprintf(cfg, "false"),
				Check:  resource.TestCheckResourceAttr("lastping_monitor.p", "paused", "false"),
			},
		},
	})
}

// TestAccMonitor_slugMustBeNormalised: the server normalises (trim + lowercase)
// and rejects anything outside ^[a-z0-9][a-z0-9-]{1,48}[a-z0-9]$, so a
// non-normalised slug must fail at plan time rather than plan to one value and
// apply to another.
func TestAccMonitor_slugMustBeNormalised(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: `
resource "lastping_monitor" "bad" {
  name          = "acc-bad-slug"
  slug          = "  Rev-Case-Slug  "
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
}`,
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(`(?s)slug.*lowercase`),
		}},
	})
}

// TestAccMonitor_slugCollisionFails is the H1 guarantee: creating a monitor whose
// slug already exists must error, never silently adopt.
func TestAccMonitor_slugCollisionFails(t *testing.T) {
	const firstOnly = `
resource "lastping_monitor" "first" {
  name          = "acc-collide"
  slug          = "acc-collide"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
}`
	var firstID string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: firstOnly,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrWith("lastping_monitor.first", "id", func(v string) error {
						firstID = v
						return nil
					}),
				),
			},
			{
				Config: `
resource "lastping_monitor" "first" {
  name          = "acc-collide"
  slug          = "acc-collide"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
}

resource "lastping_monitor" "second" {
  name          = "acc-collide-two"
  slug          = "acc-collide"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
}`,
				ExpectError: regexp.MustCompile(`already exists|terraform import`),
			},
			{
				// The failed create must not have touched the monitor that was
				// already there: same id (not recreated, not adopted) and the
				// same configuration, read back from the API.
				Config: firstOnly,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrWith("lastping_monitor.first", "id", func(v string) error {
						if v != firstID {
							return fmt.Errorf("first monitor id changed: was %s, now %s", firstID, v)
						}
						return nil
					}),
					resource.TestCheckResourceAttr("lastping_monitor.first", "name", "acc-collide"),
					resource.TestCheckResourceAttr("lastping_monitor.first", "period_s", "3600"),
					resource.TestCheckResourceAttr("lastping_monitor.first", "grace_s", "300"),
				),
			},
		},
	})
}

// TestAccMonitor_tagsClearedOutOfBand is the regression test for the
// tagsValue masking bug: tags removed by something other than this Terraform
// run (the dashboard, a direct API call, an MCP tool) must show up as drift
// on the next refresh. Before the fix, tagsValue echoed the stale prior value
// back whenever the API reported no tags, so `terraform plan -refresh-only`
// reported "No changes." with the removal invisible.
func TestAccMonitor_tagsClearedOutOfBand(t *testing.T) {
	const cfg = `
resource "lastping_monitor" "oob" {
  name          = "acc-tags-oob"
  slug          = "acc-tags-oob"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 300
  tags          = ["env:prod"]
}`
	var monitorID string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: cfg,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_monitor.oob", "tags.#", "1"),
					resource.TestCheckResourceAttrWith("lastping_monitor.oob", "id", func(v string) error {
						monitorID = v
						return nil
					}),
				),
			},
			{
				// Clear tags the way the dashboard, a direct API call, or MCP
				// would — this is how "cleared out-of-band" happens in
				// practice, not a contrivance of the test.
				//
				// The clear is an explicit `tags: null`, which is correct
				// against both server generations: a merge-patch server treats
				// it as "clear", and the older full-replace server decoded it
				// to a nil slice and cleared too. Simply omitting the key is
				// NOT equivalent — under merge patch it preserves the tags and
				// the test would silently stop testing anything.
				//
				// The other fields are echoed back unchanged so that a
				// full-replace server rewrites them to the values they already
				// hold rather than to zeroes the schedule cannot survive.
				PreConfig: func() {
					c := testAccDirectClient(t)
					mon, err := c.GetMonitor(context.Background(), monitorID)
					require.NoError(t, err)
					_, err = c.UpdateMonitor(context.Background(), monitorID, client.MonitorPatch{
						"name":          mon.Name,
						"schedule_kind": mon.ScheduleKind,
						"period_s":      mon.PeriodS,
						"tz":            mon.TZ,
						"grace_s":       mon.GraceS,
						"tags":          nil,
					})
					require.NoError(t, err)
				},
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
				Check: resource.ComposeAggregateTestCheckFunc(
					// The refreshed state must reflect the removal, not the
					// stale ["env:prod"] Terraform last knew about.
					resource.TestCheckNoResourceAttr("lastping_monitor.oob", "tags.#"),
				),
			},
		},
	})
}

// TestAccMonitor_omittedOptionalComputedSurviveAnUnrelatedUpdate is the exact
// apply that broke in production.
//
// A monitor holds grace_s = 1800 in state and on the server. The configuration
// never mentions grace_s. An apply that changes only `tags` used to fail with
//
//	check: grace 0s outside [60, 31536000] [INVALID_GRACE_PERIOD]
//
// because terraform-plugin-framework marks every Optional+Computed attribute
// absent from the configuration as unknown in the plan, `ValueInt64()` on an
// unknown is 0, and `PATCH /api/v1/checks/{id}` replaces the whole object
// rather than merging. The same mechanism emptied schedule_kind and zeroed
// period_s and tz in the same request.
//
// Step 2 omits all four and touches only `tags`. It fails before the fix.
func TestAccMonitor_omittedOptionalComputedSurviveAnUnrelatedUpdate(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// Establish non-default values the server could not have
				// guessed, so "unchanged" cannot be confused with "reset to a
				// default that happens to match".
				Config: `
resource "lastping_monitor" "carry" {
  name          = "acc-carry"
  slug          = "acc-carry"
  schedule_kind = "cron"
  cron_expr     = "0 3 * * *"
  tz            = "Europe/Berlin"
  grace_s       = 1800
  tags          = ["acc:test"]
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_monitor.carry", "grace_s", "1800"),
					resource.TestCheckResourceAttr("lastping_monitor.carry", "tz", "Europe/Berlin"),
					resource.TestCheckResourceAttr("lastping_monitor.carry", "schedule_kind", "cron"),
				),
			},
			{
				// grace_s, tz and schedule_kind are gone from the configuration.
				// Only tags changed. Nothing else may move.
				Config: `
resource "lastping_monitor" "carry" {
  name      = "acc-carry"
  slug      = "acc-carry"
  cron_expr = "0 3 * * *"
  tags      = ["acc:test", "acc:second"]
}`,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("lastping_monitor.carry", plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_monitor.carry", "grace_s", "1800"),
					resource.TestCheckResourceAttr("lastping_monitor.carry", "tz", "Europe/Berlin"),
					resource.TestCheckResourceAttr("lastping_monitor.carry", "schedule_kind", "cron"),
					resource.TestCheckResourceAttr("lastping_monitor.carry", "cron_expr", "0 3 * * *"),
					resource.TestCheckResourceAttr("lastping_monitor.carry", "tags.#", "2"),
					// Read it back from the API, not just from state: a PATCH
					// that reset the server would still leave state agreeing
					// with the plan, because the plan said "known after apply".
					resource.TestCheckResourceAttrWith("lastping_monitor.carry", "id",
						func(id string) error {
							mon, err := testAccDirectClient(t).GetMonitor(t.Context(), id)
							if err != nil {
								return err
							}
							if mon.GraceS != 1800 {
								return fmt.Errorf("server holds grace_s=%d, want 1800", mon.GraceS)
							}
							if mon.TZ != "Europe/Berlin" {
								return fmt.Errorf("server holds tz=%q, want Europe/Berlin", mon.TZ)
							}
							if mon.ScheduleKind != "cron" {
								return fmt.Errorf("server holds schedule_kind=%q, want cron", mon.ScheduleKind)
							}
							return nil
						}),
				),
			},
		},
	})
}

// TestAccMonitor_omittedPeriodSurvivesAnUnrelatedUpdate covers the simple
// schedule half of the same bug. period_s is Optional+Computed too, and a
// simple-schedule monitor whose configuration omits it had its period sent as
// 0 on every update.
func TestAccMonitor_omittedPeriodSurvivesAnUnrelatedUpdate(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_monitor" "period" {
  name          = "acc-period"
  slug          = "acc-period"
  schedule_kind = "simple"
  period_s      = 7200
  grace_s       = 900
  tags          = ["acc:test"]
}`,
				Check: resource.TestCheckResourceAttr("lastping_monitor.period", "period_s", "7200"),
			},
			{
				Config: `
resource "lastping_monitor" "period" {
  name = "acc-period"
  slug = "acc-period"
  tags = ["acc:test", "acc:second"]
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_monitor.period", "period_s", "7200"),
					resource.TestCheckResourceAttr("lastping_monitor.period", "grace_s", "900"),
					resource.TestCheckResourceAttr("lastping_monitor.period", "schedule_kind", "simple"),
				),
			},
		},
	})
}

// TestAccMonitor_omittedProbeSettingsSurviveAnUnrelatedUpdate is the silent
// half of the same bug, and the more dangerous one: it raised no error at all.
//
// probe_expected_status, probe_timeout_s and probe_follow_redirects are
// Optional+Computed. Omitted from the configuration they planned as "known
// after apply", so the zero the provider then sent was accepted without
// complaint — the probe quietly went back to accepting any 2xx, timing out at
// the default, and not following redirects. Nothing in state looked wrong.
func TestAccMonitor_omittedProbeSettingsSurviveAnUnrelatedUpdate(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_monitor" "probe" {
  name                   = "acc-probe-carry"
  slug                   = "acc-probe-carry"
  monitor_type           = "http"
  probe_url              = "https://example.com/health"
  probe_interval_s       = 300
  probe_expected_status  = 204
  probe_timeout_s        = 25
  probe_follow_redirects = true
  tags                   = ["acc:test"]
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_monitor.probe", "probe_expected_status", "204"),
					resource.TestCheckResourceAttr("lastping_monitor.probe", "probe_timeout_s", "25"),
					resource.TestCheckResourceAttr("lastping_monitor.probe", "probe_follow_redirects", "true"),
				),
			},
			{
				// All three are gone from the configuration; only tags changed.
				Config: `
resource "lastping_monitor" "probe" {
  name             = "acc-probe-carry"
  slug             = "acc-probe-carry"
  monitor_type     = "http"
  probe_url        = "https://example.com/health"
  probe_interval_s = 300
  tags             = ["acc:test", "acc:second"]
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_monitor.probe", "probe_expected_status", "204"),
					resource.TestCheckResourceAttr("lastping_monitor.probe", "probe_timeout_s", "25"),
					resource.TestCheckResourceAttr("lastping_monitor.probe", "probe_follow_redirects", "true"),
					resource.TestCheckResourceAttrWith("lastping_monitor.probe", "id",
						func(id string) error {
							mon, err := testAccDirectClient(t).GetMonitor(t.Context(), id)
							if err != nil {
								return err
							}
							if mon.ProbeExpectedStatus != 204 {
								return fmt.Errorf("server holds probe_expected_status=%d, want 204",
									mon.ProbeExpectedStatus)
							}
							if mon.ProbeTimeoutS != 25 {
								return fmt.Errorf("server holds probe_timeout_s=%d, want 25", mon.ProbeTimeoutS)
							}
							if !mon.ProbeFollowRedirects {
								return fmt.Errorf("server turned probe_follow_redirects off")
							}
							return nil
						}),
				),
			},
		},
	})
}

// TestAccMonitor_blockedTimeoutCanBeRemoved is the clearable proof for
// blocked_timeout_s, and it differs from every other member of that group in
// what "cleared" means.
//
// A `blocked` ping suspends the ordinary absence rules — the silence deadline,
// expect_every_s and the run clock all stand down while a job waits on a human
// — and blocked_timeout_s is the bound on that suspension. Removing it does NOT
// mean "wait forever": the server falls back to check.DefaultBlockedTimeout,
// 24 hours. So the attribute has no "off" state at all, only a length, and the
// property under test is that Terraform can hand it back to the default.
//
// It still has to travel as an explicit null. Under merge-patch an absent key
// leaves the stored bound in place, so a bespoke 2-hour timeout set once
// through Terraform could never be returned to the default — and the second
// step reads the monitor back through the API rather than trusting state,
// because state agreeing with the plan proves nothing when the plan said
// "removed" and the server was never told.
func TestAccMonitor_blockedTimeoutCanBeRemoved(t *testing.T) {
	const withBlocked = `
resource "lastping_monitor" "bt" {
  name              = "acc-blocked-timeout"
  slug              = "acc-blocked-timeout"
  schedule_kind     = "simple"
  period_s          = 3600
  grace_s           = 600
  max_runtime_s     = 14400
  blocked_timeout_s = 7200
}`
	const withoutBlocked = `
resource "lastping_monitor" "bt" {
  name          = "acc-blocked-timeout"
  slug          = "acc-blocked-timeout"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 600
  max_runtime_s = 14400
}`

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: withBlocked,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_monitor.bt", "blocked_timeout_s", "7200"),
					// The blocked clock is independent of the run clock: setting
					// one must not disturb the other.
					resource.TestCheckResourceAttr("lastping_monitor.bt", "max_runtime_s", "14400"),
					resource.TestCheckResourceAttrWith("lastping_monitor.bt", "id", func(id string) error {
						mon, err := testAccDirectClient(t).GetMonitor(t.Context(), id)
						if err != nil {
							return err
						}
						if mon.BlockedTimeoutS == nil || *mon.BlockedTimeoutS != 7200 {
							return fmt.Errorf("server holds blocked_timeout_s=%v, want 7200", mon.BlockedTimeoutS)
						}
						return nil
					}),
				),
			},
			{
				Config: withoutBlocked,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr("lastping_monitor.bt", "blocked_timeout_s"),
					resource.TestCheckResourceAttrWith("lastping_monitor.bt", "id", func(id string) error {
						mon, err := testAccDirectClient(t).GetMonitor(t.Context(), id)
						if err != nil {
							return err
						}
						// Cleared server-side means "the 24-hour default now
						// applies", not "unbounded" — the column is NULL and the
						// rule still fires.
						if mon.BlockedTimeoutS != nil {
							return fmt.Errorf("server still holds blocked_timeout_s=%d, want it back at the default",
								*mon.BlockedTimeoutS)
						}
						if mon.MaxRuntimeS == nil || *mon.MaxRuntimeS != 14400 {
							return fmt.Errorf("clearing blocked_timeout_s disturbed max_runtime_s=%v",
								mon.MaxRuntimeS)
						}
						return nil
					}),
				),
			},
			{
				// And it stays cleared: no perpetual diff on re-plan.
				Config:   withoutBlocked,
				PlanOnly: true,
			},
			{
				ResourceName:      "lastping_monitor.bt",
				ImportState:       true,
				ImportStateId:     "acc-blocked-timeout",
				ImportStateVerify: true,
			},
		},
	})
}

// TestAccMonitor_blockedTimeoutOnHTTPMonitor guards against the obvious wrong
// generalisation.
//
// max_runtime_s and step_timeout_s are both refused on monitor_type = "http",
// so it is natural to assume every detection attribute is. blocked_timeout_s is
// not: it has no run-scoped precondition — a `blocked` ping is a statement
// about the monitor, not about an armed run — and the API accepts it on every
// monitor type. A ValidateConfig rule copied from its two neighbours would
// refuse a configuration the server is perfectly happy with, which is the kind
// of error a practitioner can do nothing about.
func TestAccMonitor_blockedTimeoutOnHTTPMonitor(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: `
resource "lastping_monitor" "probe" {
  name              = "acc-http-blocked"
  slug              = "acc-http-blocked"
  monitor_type      = "http"
  probe_url         = "https://example.com/"
  probe_interval_s  = 300
  blocked_timeout_s = 3600
}`,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("lastping_monitor.probe", "blocked_timeout_s", "3600"),
			),
		}},
	})
}

// TestAccMonitor_notifyMinRunSetUpdateAndClear covers the notification
// duration floor end to end: set at create, changed in place, then cleared —
// and the clear must actually reach the server, not merely stop showing up in
// the plan, because that is the whole failure mode the clearable group exists
// to rule out (see monitorPatchFromModel).
func TestAccMonitor_notifyMinRunSetUpdateAndClear(t *testing.T) {
	const withFloor = `
resource "lastping_monitor" "nmr" {
  name             = "acc-notify-min-run"
  slug             = "acc-notify-min-run"
  schedule_kind    = "on_demand"
  grace_s          = 600
  notify_min_run_s = 120
}`
	const floorUpdated = `
resource "lastping_monitor" "nmr" {
  name             = "acc-notify-min-run"
  slug             = "acc-notify-min-run"
  schedule_kind    = "on_demand"
  grace_s          = 600
  notify_min_run_s = 300
}`
	const withoutFloor = `
resource "lastping_monitor" "nmr" {
  name          = "acc-notify-min-run"
  slug          = "acc-notify-min-run"
  schedule_kind = "on_demand"
  grace_s       = 600
}`

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: withFloor,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_monitor.nmr", "notify_min_run_s", "120"),
					resource.TestCheckResourceAttrWith("lastping_monitor.nmr", "id", func(id string) error {
						mon, err := testAccDirectClient(t).GetMonitor(t.Context(), id)
						if err != nil {
							return err
						}
						if mon.NotifyMinRunS == nil || *mon.NotifyMinRunS != 120 {
							return fmt.Errorf("server holds notify_min_run_s=%v, want 120", mon.NotifyMinRunS)
						}
						return nil
					}),
				),
			},
			{
				// Update in place — must not force replacement.
				Config: floorUpdated,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_monitor.nmr", "notify_min_run_s", "300"),
					resource.TestCheckResourceAttrWith("lastping_monitor.nmr", "id", func(id string) error {
						mon, err := testAccDirectClient(t).GetMonitor(t.Context(), id)
						if err != nil {
							return err
						}
						if mon.NotifyMinRunS == nil || *mon.NotifyMinRunS != 300 {
							return fmt.Errorf("server holds notify_min_run_s=%v, want 300", mon.NotifyMinRunS)
						}
						return nil
					}),
				),
			},
			{
				Config: withoutFloor,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr("lastping_monitor.nmr", "notify_min_run_s"),
					// Read it back from the API: state agreeing with the plan
					// proves nothing when the plan said "removed" and the server
					// was never told.
					resource.TestCheckResourceAttrWith("lastping_monitor.nmr", "id", func(id string) error {
						mon, err := testAccDirectClient(t).GetMonitor(t.Context(), id)
						if err != nil {
							return err
						}
						if mon.NotifyMinRunS != nil {
							return fmt.Errorf("server still holds notify_min_run_s=%d, want it cleared",
								*mon.NotifyMinRunS)
						}
						return nil
					}),
				),
			},
			{
				// And it stays cleared: no perpetual diff on re-plan.
				Config:   withoutFloor,
				PlanOnly: true,
			},
			{
				ResourceName:      "lastping_monitor.nmr",
				ImportState:       true,
				ImportStateId:     "acc-notify-min-run",
				ImportStateVerify: true,
			},
		},
	})
}

// TestAccMonitor_notifyMinRunRejectedOnHTTP asserts the API's 400
// NOTIFY_MIN_RUN_NOT_SUPPORTED is anticipated at plan time. Like max_runtime_s
// and step_timeout_s, the floor only evaluates once a run's duration is known,
// and an http probe never sends the /start that duration is measured from.
func TestAccMonitor_notifyMinRunRejectedOnHTTP(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: `
resource "lastping_monitor" "probe" {
  name              = "acc-http-notifyminrun"
  slug              = "acc-http-notifyminrun"
  monitor_type      = "http"
  probe_url         = "https://example.com/"
  probe_interval_s  = 300
  notify_min_run_s  = 120
}`,
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(`notify_min_run_s.*not supported`),
		}},
	})
}

// TestAccMonitor_ciBinding is the end-to-end proof that a CI monitor can be
// declared in HCL at all, which before these attributes it could not.
//
// The import step deliberately ignores ci_workflow and ci_branch, and that is
// the finding rather than a workaround: no API response carries either filter,
// so an imported monitor cannot recover them however it is really configured.
// ci_provider IS returned, so it is verified normally — an import that silently
// dropped it was the original bug.
func TestAccMonitor_ciBinding(t *testing.T) {
	const config = `
resource "lastping_monitor" "ci" {
  name          = "acc-ci-binding"
  slug          = "acc-ci-binding"
  schedule_kind = "simple"
  period_s      = 86400
  grace_s       = 3600
  ci_provider   = "github"
  ci_workflow   = "ci.yml"
  ci_branch     = "main"
}`
	const filtersChanged = `
resource "lastping_monitor" "ci" {
  name          = "acc-ci-binding"
  slug          = "acc-ci-binding"
  schedule_kind = "simple"
  period_s      = 86400
  grace_s       = 3600
  ci_provider   = "github"
  ci_workflow   = "release.yml"
  ci_branch     = "release"
}`
	const filtersRemoved = `
resource "lastping_monitor" "ci" {
  name          = "acc-ci-binding"
  slug          = "acc-ci-binding"
  schedule_kind = "simple"
  period_s      = 86400
  grace_s       = 3600
  ci_provider   = "github"
}`

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_monitor.ci", "ci_provider", "github"),
					resource.TestCheckResourceAttr("lastping_monitor.ci", "ci_workflow", "ci.yml"),
					resource.TestCheckResourceAttr("lastping_monitor.ci", "ci_branch", "main"),
					resource.TestCheckResourceAttrWith("lastping_monitor.ci", "id", func(id string) error {
						mon, err := testAccDirectClient(t).GetMonitor(t.Context(), id)
						if err != nil {
							return err
						}
						if mon.CiProvider != "github" {
							return fmt.Errorf("server holds ci_provider=%q, want github", mon.CiProvider)
						}
						return nil
					}),
				),
			},
			{
				// The filters update in place: only ci_provider is immutable.
				Config: filtersChanged,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("lastping_monitor.ci", plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_monitor.ci", "ci_workflow", "release.yml"),
					resource.TestCheckResourceAttr("lastping_monitor.ci", "ci_branch", "release"),
				),
			},
			{
				// Removing them clears them. The API reads an explicit "" on
				// these two as "preserve", so this only works because
				// monitorPatchFromModel sends null and never "".
				Config: filtersRemoved,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr("lastping_monitor.ci", "ci_workflow"),
					resource.TestCheckNoResourceAttr("lastping_monitor.ci", "ci_branch"),
					resource.TestCheckResourceAttr("lastping_monitor.ci", "ci_provider", "github"),
				),
			},
			{
				Config:   filtersRemoved,
				PlanOnly: true,
			},
			{
				ResourceName:      "lastping_monitor.ci",
				ImportState:       true,
				ImportStateId:     "acc-ci-binding",
				ImportStateVerify: true,
			},
		},
	})
}

// TestAccMonitor_ciFiltersAreNotRefreshable pins the documented cost of
// ci_workflow and ci_branch being write-only.
//
// An import cannot recover them — no response carries them — so
// ImportStateVerify has to be told to ignore both, and that ignore list IS the
// assertion. If the API ever starts returning the filters, this step begins
// passing without the ignore and the exemption should be deleted along with
// modelFromMonitor's writeOnlyString fallback.
func TestAccMonitor_ciFiltersAreNotRefreshable(t *testing.T) {
	const config = `
resource "lastping_monitor" "ciimp" {
  name          = "acc-ci-import"
  slug          = "acc-ci-import"
  schedule_kind = "simple"
  period_s      = 86400
  grace_s       = 3600
  ci_provider   = "gitlab"
  ci_workflow   = "nightly"
  ci_branch     = "main"
}`

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_monitor.ciimp", "ci_workflow", "nightly"),
				),
			},
			{
				// A refresh must not drop what Terraform last wrote, even
				// though the response says nothing about it.
				Config:   config,
				PlanOnly: true,
			},
			{
				ResourceName:            "lastping_monitor.ciimp",
				ImportState:             true,
				ImportStateId:           "acc-ci-import",
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"ci_workflow", "ci_branch"},
			},
		},
	})
}

// TestAccMonitor_ciProviderChangeForcesReplacement: the API treats the CI
// binding as create-only and ignores ci_provider on PATCH entirely, so an
// in-place update would apply cleanly, change nothing, and leave the same diff
// on every subsequent plan. RequiresReplace makes the real cost — a new monitor
// id, a new webhook URL and a new signing secret — visible at plan time.
func TestAccMonitor_ciProviderChangeForcesReplacement(t *testing.T) {
	const onGitHub = `
resource "lastping_monitor" "swap" {
  name          = "acc-ci-swap"
  slug          = "acc-ci-swap"
  schedule_kind = "simple"
  period_s      = 86400
  grace_s       = 3600
  ci_provider   = "github"
}`
	const onGitLab = `
resource "lastping_monitor" "swap" {
  name          = "acc-ci-swap"
  slug          = "acc-ci-swap"
  schedule_kind = "simple"
  period_s      = 86400
  grace_s       = 3600
  ci_provider   = "gitlab"
}`

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: onGitHub,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_monitor.swap", "ci_provider", "github"),
				),
			},
			{
				Config: onGitLab,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("lastping_monitor.swap",
							plancheck.ResourceActionDestroyBeforeCreate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_monitor.swap", "ci_provider", "gitlab"),
				),
			},
		},
	})
}

// TestAccMonitor_ciFilterWithoutProviderRejected: the API accepts ci_workflow
// on a monitor with no CI binding and silently discards it, and because no
// response reports the filter back, the provider would write the configured
// value into state unchallenged — an apply that succeeds, plans clean forever,
// and does not do what the configuration says. Plan time is the only place that
// is still visible.
func TestAccMonitor_ciFilterWithoutProviderRejected(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: `
resource "lastping_monitor" "orphan" {
  name          = "acc-ci-orphan"
  slug          = "acc-ci-orphan"
  schedule_kind = "simple"
  period_s      = 3600
  grace_s       = 600
  ci_workflow   = "ci.yml"
}`,
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(`ci_workflow\s+has\s+no\s+effect\s+without\s+ci_provider`),
		}},
	})
}
