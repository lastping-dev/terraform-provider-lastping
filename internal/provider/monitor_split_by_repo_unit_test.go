package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/stretchr/testify/require"

	"github.com/lastping-dev/terraform-provider-lastping/internal/client"
)

// split_by_repo: whether hook runs are filed under one monitor per git
// repository. The server defaults it to true and every existing monitor
// already carries true, so the provider imposes no default of its own: a
// provider-side Default would plan a write on a bare provider upgrade.

func splitByRepoBase() monitorResourceModel {
	return monitorResourceModel{
		Name:         types.StringValue("acc"),
		ScheduleKind: types.StringValue("on_demand"),
		Tags:         types.SetNull(types.StringType),
	}
}

// TestMonitorSplitByRepoIsOptionalComputedWithoutDefault pins the schema
// shape and the plan of an unconfigured value.
func TestMonitorSplitByRepoIsOptionalComputedWithoutDefault(t *testing.T) {
	s := monitorSchema(t)
	attr, ok := s.Attributes["split_by_repo"].(schema.BoolAttribute)
	require.True(t, ok, "lastping_monitor has no split_by_repo bool attribute")
	require.True(t, attr.IsOptional(), "split_by_repo must be configurable")
	require.True(t, attr.IsComputed(), "split_by_repo must be Computed: the server supplies it")
	require.Nil(t, attr.Default, "split_by_repo must carry NO provider-side Default")
	require.Len(t, attr.PlanModifiers, 1, "split_by_repo needs UseStateForUnknown")

	m := splitByRepoBase()
	m.SplitByRepo = types.BoolValue(false)
	raw := monitorRawValue(t, m)
	resp := &planmodifier.BoolResponse{PlanValue: types.BoolUnknown()}
	attr.PlanModifiers[0].PlanModifyBool(context.Background(), planmodifier.BoolRequest{
		State:       tfsdk.State{Raw: raw, Schema: s},
		Plan:        tfsdk.Plan{Raw: raw, Schema: s},
		StateValue:  types.BoolValue(false),
		PlanValue:   types.BoolUnknown(),
		ConfigValue: types.BoolNull(),
	}, resp)
	require.Equal(t, types.BoolValue(false), resp.PlanValue,
		"an omitted split_by_repo must plan the STORED value, not a change")
}

// TestMonitorSplitByRepoPatch: the PATCH document carries the field only
// when the plan knows it, and a null plan value sends nothing.
func TestMonitorSplitByRepoPatch(t *testing.T) {
	ctx := context.Background()
	for _, want := range []bool{false, true} {
		desired := splitByRepoBase()
		desired.SplitByRepo = types.BoolValue(want)
		patch, err := monitorPatchFromModel(ctx, desired, desired)
		require.NoError(t, err)
		require.Equal(t, want, patch["split_by_repo"])
	}
	for name, v := range map[string]types.Bool{"null": types.BoolNull(), "unknown": types.BoolUnknown()} {
		desired := splitByRepoBase()
		desired.SplitByRepo = v
		patch, err := monitorPatchFromModel(ctx, desired, desired)
		require.NoError(t, err)
		_, has := patch["split_by_repo"]
		require.False(t, has, "a %s split_by_repo must be absent from the patch", name)
		require.Equal(t, "acc", patch["name"], "the patch was still built")
	}
}

// TestMonitorSplitByRepoPatchNeeded: a changed value is a real diff, the
// same value is not, and an unknown or null plan value never is.
func TestMonitorSplitByRepoPatchNeeded(t *testing.T) {
	ctx := context.Background()
	state := splitByRepoBase()
	state.SplitByRepo = types.BoolValue(true)

	same, err := monitorPatchNeeded(ctx, state, state)
	require.NoError(t, err)
	require.False(t, same)

	plan := state
	plan.SplitByRepo = types.BoolValue(false)
	changed, err := monitorPatchNeeded(ctx, plan, state)
	require.NoError(t, err)
	require.True(t, changed, "true -> false must reach PATCH")

	plan.SplitByRepo = types.BoolNull()
	none, err := monitorPatchNeeded(ctx, plan, state)
	require.NoError(t, err)
	require.False(t, none)
}

// TestMonitorSplitByRepoRead: the server's value is read into state; a
// response without the field reads as null.
func TestMonitorSplitByRepoRead(t *testing.T) {
	ctx := context.Background()
	prior := monitorResourceModel{Tags: types.SetNull(types.StringType)}
	mon := &client.Monitor{
		ID: "3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f", Name: "acc",
		MonitorType: "heartbeat", ScheduleKind: "on_demand", TZ: "UTC", GraceS: 600,
	}
	got, err := modelFromMonitor(ctx, mon, prior)
	require.NoError(t, err)
	require.True(t, got.SplitByRepo.IsNull())

	for _, v := range []bool{true, false} {
		mon.SplitByRepo = &v
		got, err = modelFromMonitor(ctx, mon, prior)
		require.NoError(t, err)
		require.Equal(t, types.BoolValue(v), got.SplitByRepo)
	}
}

// TestMonitorSplitByRepoUpgradePlansNoChange is the regression the no-Default
// rule exists for: state written by a provider that did not know the
// attribute holds null; refresh takes the server's true; an unconfigured
// value then plans as that true, and no PATCH is needed.
func TestMonitorSplitByRepoUpgradePlansNoChange(t *testing.T) {
	ctx := context.Background()
	old := splitByRepoBase()
	old.SplitByRepo = types.BoolNull()

	yes := true
	mon := &client.Monitor{
		ID: "3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f", Name: "acc",
		MonitorType: "heartbeat", ScheduleKind: "on_demand", TZ: "UTC", GraceS: 600,
		SplitByRepo: &yes,
	}
	refreshed, err := modelFromMonitor(ctx, mon, old)
	require.NoError(t, err)
	require.Equal(t, types.BoolValue(true), refreshed.SplitByRepo)

	s := monitorSchema(t)
	attr := s.Attributes["split_by_repo"].(schema.BoolAttribute)
	raw := monitorRawValue(t, refreshed)
	resp := &planmodifier.BoolResponse{PlanValue: types.BoolUnknown()}
	attr.PlanModifiers[0].PlanModifyBool(ctx, planmodifier.BoolRequest{
		State:       tfsdk.State{Raw: raw, Schema: s},
		Plan:        tfsdk.Plan{Raw: raw, Schema: s},
		StateValue:  refreshed.SplitByRepo,
		PlanValue:   types.BoolUnknown(),
		ConfigValue: types.BoolNull(),
	}, resp)
	require.Equal(t, types.BoolValue(true), resp.PlanValue)

	plan := refreshed
	plan.SplitByRepo = resp.PlanValue
	needed, err := monitorPatchNeeded(ctx, plan, refreshed)
	require.NoError(t, err)
	require.False(t, needed, "a bare provider upgrade must not plan a PATCH")
}

// TestMonitorSplitByRepoNotInCreateBody: POST does not take the field.
func TestMonitorSplitByRepoNotInCreateBody(t *testing.T) {
	m := splitByRepoBase()
	m.SplitByRepo = types.BoolValue(false)
	payload, err := monitorFromModel(context.Background(), m)
	require.NoError(t, err)
	b, err := json.Marshal(payload)
	require.NoError(t, err)
	require.NotContains(t, string(b), "split_by_repo")
}

// splitByRepoFakeAPI answers the monitor create, PATCH and the empty
// assertion and guard reads, and records every request.
type splitByRepoFakeAPI struct {
	mu        sync.Mutex
	requests  []string
	bodies    []string
	stored    bool
	patchCode string
}

func (f *splitByRepoFakeAPI) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	f.bodies = append(f.bodies, string(body))
	w.Header().Set("Content-Type", "application/json")
	monitor := func() {
		out, _ := json.Marshal(map[string]any{
			"id": "3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f", "name": "acc", "slug": "acc",
			"monitor_type": "heartbeat", "schedule_kind": "on_demand", "tz": "UTC", "grace_s": 600,
			"failure_threshold": 1, "trace_content": "dropped", "split_by_repo": f.stored,
		})
		_, _ = w.Write(out)
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/checks":
		f.stored = true
		w.WriteHeader(http.StatusCreated)
		monitor()
	case r.Method == http.MethodPatch:
		if f.patchCode != "" {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"title":"Bad Request","status":400,"detail":"refused","code":"` + f.patchCode + `"}`))
			return
		}
		var p map[string]any
		_ = json.Unmarshal(body, &p)
		if v, ok := p["split_by_repo"].(bool); ok {
			f.stored = v
		}
		monitor()
	default:
		_, _ = w.Write([]byte(`[]`))
	}
}

func runSplitByRepoCreate(t *testing.T, f *splitByRepoFakeAPI, planned types.Bool) *resource.CreateResponse {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	s := monitorSchema(t)
	m := splitByRepoBase()
	m.Slug = types.StringValue("acc")
	m.GraceS = types.Int64Value(600)
	m.SplitByRepo = planned
	plan := tfsdk.Plan{Schema: s, Raw: monitorRawValue(t, m)}
	resp := &resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	r := &monitorResource{client: client.New(srv.URL, "lp_test", "unit")}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, resp)
	return resp
}

// TestMonitorSplitByRepoCreateFollowsWithPatch drives Create: a planned
// value that differs from the server's default is written with a PATCH, and
// the PATCH response is what lands in state.
func TestMonitorSplitByRepoCreateFollowsWithPatch(t *testing.T) {
	f := &splitByRepoFakeAPI{}
	resp := runSplitByRepoCreate(t, f, types.BoolValue(false))
	require.False(t, resp.Diagnostics.HasError(), "%v", resp.Diagnostics)

	require.Equal(t, "POST /api/v1/checks", f.requests[0])
	require.NotContains(t, f.bodies[0], "split_by_repo", "POST does not take the field")
	require.Equal(t, "PATCH /api/v1/checks/3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f", f.requests[1])
	require.JSONEq(t, `{"split_by_repo":false}`, f.bodies[1])

	var got monitorResourceModel
	require.False(t, resp.State.Get(context.Background(), &got).HasError())
	require.Equal(t, types.BoolValue(false), got.SplitByRepo)
}

// TestMonitorSplitByRepoCreateWithoutPatch: a planned value equal to the
// server's default, and an unknown one, need no PATCH.
func TestMonitorSplitByRepoCreateWithoutPatch(t *testing.T) {
	for name, v := range map[string]types.Bool{"default": types.BoolValue(true), "unknown": types.BoolUnknown()} {
		t.Run(name, func(t *testing.T) {
			f := &splitByRepoFakeAPI{}
			resp := runSplitByRepoCreate(t, f, v)
			require.False(t, resp.Diagnostics.HasError(), "%v", resp.Diagnostics)
			for _, req := range f.requests {
				require.NotContains(t, req, "PATCH")
			}
			var got monitorResourceModel
			require.False(t, resp.State.Get(context.Background(), &got).HasError())
			require.Equal(t, types.BoolValue(true), got.SplitByRepo)
		})
	}
}

// TestMonitorSplitByRepoDiagnostics: both refusals become a diagnostic on
// the attribute that names it, and the created monitor stays in state.
func TestMonitorSplitByRepoDiagnostics(t *testing.T) {
	for _, code := range []string{"SPLIT_BY_REPO_ON_CHILD", "SPLIT_BY_REPO_NOT_ON_DEMAND"} {
		t.Run(code, func(t *testing.T) {
			f := &splitByRepoFakeAPI{patchCode: code}
			resp := runSplitByRepoCreate(t, f, types.BoolValue(false))
			require.True(t, resp.Diagnostics.HasError())
			d := resp.Diagnostics.Errors()[0]
			require.Contains(t, d.Summary(), "split_by_repo")
			require.Contains(t, d.Summary(), code)

			var got monitorResourceModel
			require.False(t, resp.State.Get(context.Background(), &got).HasError())
			require.Equal(t, "3f7c1f5a-1a2b-4c3d-8e9f-0a1b2c3d4e5f", got.ID.ValueString(),
				"the created monitor must stay in state")
			require.Equal(t, types.BoolValue(true), got.SplitByRepo, "state holds the server's value")
		})
	}
}

// splitByRepoError returns the split_by_repo diagnostic of a validated
// config, or nil.
func splitByRepoError(t *testing.T, m monitorResourceModel) diag.Diagnostic {
	t.Helper()
	for _, d := range monitorValidateConfig(t, m).Errors() {
		if wp, ok := d.(diag.DiagnosticWithPath); ok && wp.Path().Equal(path.Root("split_by_repo")) {
			return d
		}
	}
	return nil
}

// TestMonitorSplitByRepoValidateSchedule: false is refused at plan time on
// any monitor that is not on_demand, which is what stops a create from
// failing on its follow-up PATCH and being replaced on every apply.
func TestMonitorSplitByRepoValidateSchedule(t *testing.T) {
	cfg := func(kind types.String, split types.Bool) monitorResourceModel {
		m := monitorResourceModel{Name: types.StringValue("acc"), ScheduleKind: kind, SplitByRepo: split}
		switch kind.ValueString() {
		case "cron":
			m.CronExpr = types.StringValue("0 3 * * *")
		case "simple":
			m.PeriodS = types.Int64Value(3600)
		}
		return m
	}

	for name, kind := range map[string]types.String{
		"cron": types.StringValue("cron"), "simple": types.StringValue("simple"), "null": types.StringNull(),
	} {
		t.Run("false refused on "+name, func(t *testing.T) {
			d := splitByRepoError(t, cfg(kind, types.BoolValue(false)))
			require.NotNil(t, d, "split_by_repo = false on %s must be a plan-time error", name)
			require.Contains(t, d.Summary(), "on_demand")
			require.Contains(t, d.Detail(), "Remove the split_by_repo line")
		})
	}

	accepted := map[string]monitorResourceModel{
		"false on on_demand":      cfg(types.StringValue("on_demand"), types.BoolValue(false)),
		"true on cron":            cfg(types.StringValue("cron"), types.BoolValue(true)),
		"true on null schedule":   cfg(types.StringNull(), types.BoolValue(true)),
		"null on cron":            cfg(types.StringValue("cron"), types.BoolNull()),
		"unknown on cron":         cfg(types.StringValue("cron"), types.BoolUnknown()),
		"false on unknown kind":   cfg(types.StringUnknown(), types.BoolValue(false)),
		"unknown on unknown kind": cfg(types.StringUnknown(), types.BoolUnknown()),
	}
	for name, m := range accepted {
		t.Run(name, func(t *testing.T) {
			require.Nil(t, splitByRepoError(t, m))
		})
	}
}
