package provider

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/lastping-dev/terraform-provider-lastping/internal/client"
)

// maxGuardsPerMonitor and maxGuardWindowSeconds mirror the API's caps
// (core/metricguard). They are checked at plan time so hitting either names the
// offending guard and costs nothing, instead of arriving as a 400 partway
// through an apply.
const (
	maxGuardsPerMonitor   = 5
	maxGuardWindowSeconds = 604800
)

// guardAggregations are the accepted aggregation values, in the order the
// documentation lists them.
var guardAggregations = []string{"sum", "max", "avg"}

// monitorGuardModel is one `metric_guard` block.
//
// There is deliberately no `id` attribute, for the same reason
// monitorAssertionModel has none: the endpoint replaces the whole set on every
// write, so a guard's id is not stable across an apply that touches any guard,
// and a Computed attribute inside a set element would make the whole element
// unknown at plan time.
type monitorGuardModel struct {
	Name        types.String  `tfsdk:"name"`
	Path        types.String  `tfsdk:"path"`
	WindowS     types.Int64   `tfsdk:"window_s"`
	Ceiling     types.Float64 `tfsdk:"ceiling"`
	Aggregation types.String  `tfsdk:"aggregation"`
}

// guardObjectType is the object type of one set element. It must agree with
// monitorGuardModel field-for-field.
func guardObjectType() types.ObjectType {
	return types.ObjectType{AttrTypes: map[string]attr.Type{
		"name":        types.StringType,
		"path":        types.StringType,
		"window_s":    types.Int64Type,
		"ceiling":     types.Float64Type,
		"aggregation": types.StringType,
	}}
}

// monitorGuardBlock is the `metric_guard` block's schema.
//
// A SET rather than a list, for the same reason the assertion block is one: the
// endpoint has no per-guard identity, and it returns rows ordered by
// (created_at, id) where every row written by one request shares a created_at
// and the tie-break is a random UUID. A list would show a permanent,
// un-appliable diff whenever the server handed the same guards back in a
// different order.
func monitorGuardBlock() schema.SetNestedBlock {
	return schema.SetNestedBlock{
		MarkdownDescription: "A **metric guard**: a ceiling on a number the job reports about itself. " +
			"An `assertion` catches a run that did nothing; a guard catches the opposite — an agent that " +
			"loops, retries and burns money. That failure is invisible to a ping-rate ceiling, because an " +
			"agent spending $400 in one call is not pinging fast.\n\n" +
			"The guard reads one number out of the ping body at `path`, rolls it up across the trailing " +
			"`window_s` seconds using `aggregation`, and opens an incident when the result **exceeds** " +
			"`ceiling`. Equality does not trip: a ceiling is the most you are allowed to spend, not the " +
			"first amount that is too much. There is no new incident cause — a tripped guard opens a " +
			"`runaway` incident, the same cause the fixed pings-per-hour `runaway_ceiling` uses, and the " +
			"guard's `name` is written to the incident's detail so the two are distinguishable in the alert.\n\n" +
			"Pings whose body is absent, is not JSON, or holds nothing numeric at `path` are **skipped**, " +
			"not counted as zero — so a `start` ping never drags an average down.\n\n" +
			"The block is repeatable and **replaces the whole set**: the guards in the configuration are the " +
			"monitor's complete set after an apply, and removing every block removes every guard. At most " +
			strconv.Itoa(maxGuardsPerMonitor) + " per monitor — a tighter cap than `assertion`'s 20, because " +
			"an assertion is a string match over a set already in hand while each guard is its own query " +
			"for its own window, so N guards multiply the per-ping cost by N.\n\n" +
			"~> **Guards read the ping body, so the job has to send one** — for example " +
			"`curl -d '{\"cost\":{\"usd\":0.42}}' \"$PING_URL\"`. A monitor with `monitor_type = \"http\"` has " +
			"no ping body at all (the prober makes the request), so guards configured on one are never " +
			"evaluated; the provider rejects them at plan time rather than storing a setting that cannot fire.",
		NestedObject: schema.NestedBlockObject{
			Attributes: map[string]schema.Attribute{
				"name": schema.StringAttribute{
					Required: true,
					MarkdownDescription: "Human-readable label for this guard. It is written to the incident's " +
						"detail when the guard trips, and is the only thing that tells it apart from a " +
						"ping-rate runaway in the alert — so it should name the thing being capped, " +
						"`\"daily spend\"`, not `\"guard 2\"`.",
					Validators: []validator.String{stringvalidator.LengthAtLeast(1)},
				},
				"path": schema.StringAttribute{
					Required: true,
					MarkdownDescription: "Dotted path into the ping body parsed as JSON, for example " +
						"`cost.usd`. Identical in meaning to an `assertion` block's `path` and read by the " +
						"same code, including the rule that a numeric string counts as a number.\n\n" +
						"~> **This is a dotted path, not JSONPath.** Wildcards, filters and unions (`[`, " +
						"`*`, `$`) are rejected.",
					Validators: []validator.String{stringvalidator.LengthAtLeast(1)},
				},
				"window_s": schema.Int64Attribute{
					Required: true,
					MarkdownDescription: "Trailing window in seconds — how far back the guard looks when it " +
						"rolls the number up. Capped at " + strconv.Itoa(maxGuardWindowSeconds) +
						" (7 days).\n\n" +
						"~> **The cap is measured cost, not policy.** A guard re-reads every ping body in " +
						"its window on every ping, so the work is linear in the window: 4.2 ms per ping at " +
						"a one-hour window against 390 ms at thirty days. Seven days is also comfortably " +
						"inside the 90-day ping retention, past which a guard would aggregate over rows " +
						"that have already been pruned and quietly report a number lower than the truth.",
					Validators: []validator.Int64{
						int64validator.AtLeast(1),
						int64validator.AtMost(maxGuardWindowSeconds),
					},
				},
				"ceiling": schema.Float64Attribute{
					Required: true,
					MarkdownDescription: "The value the aggregate must not exceed. Exceeding it opens the " +
						"incident; equalling it does not. Zero and negative values are both accepted.",
				},
				"aggregation": schema.StringAttribute{
					Required: true,
					MarkdownDescription: "How the numbers found in the window are rolled up:\n\n" +
						"* `sum` — total them. The usual choice for a budget.\n" +
						"* `max` — take the largest single reported value. The choice for \"no one run may " +
						"cost more than this\".\n" +
						"* `avg` — average them over the pings that actually reported a number at `path`.",
					Validators: []validator.String{stringvalidator.OneOf(guardAggregations...)},
				},
			},
		},
	}
}

// monitorGuardSetNull is the "no metric_guard blocks configured" value.
func monitorGuardSetNull() types.Set {
	return types.SetNull(guardObjectType())
}

// guardsFromModel converts the configured blocks into the API's payload. It
// returns a non-nil slice so an empty set is sent as `[]` (clear the set)
// rather than as null.
func guardsFromModel(ctx context.Context, set types.Set) ([]client.MetricGuard, error) {
	out := []client.MetricGuard{}
	if set.IsNull() || set.IsUnknown() {
		return out, nil
	}
	var models []monitorGuardModel
	if diags := set.ElementsAs(ctx, &models, false); diags.HasError() {
		return nil, fmt.Errorf("read metric_guard blocks: %v", diags)
	}
	for _, m := range models {
		out = append(out, client.MetricGuard{
			Name:        m.Name.ValueString(),
			Path:        m.Path.ValueString(),
			WindowS:     m.WindowS.ValueInt64(),
			Ceiling:     m.Ceiling.ValueFloat64(),
			Aggregation: m.Aggregation.ValueString(),
		})
	}
	return out, nil
}

// guardsToModel converts the API's answer back into a set.
//
// prior exists for the same reason it does on assertionsToModel: the API always
// answers with an array, so a monitor with no guards comes back as `[]`, which
// has to map onto whichever empty form the configuration used. Terraform has no
// "explicit empty set of blocks" distinct from "no blocks", so an empty answer
// is always null here.
//
// Every attribute is Required, so none of them can be null in a configuration
// and none needs the stringOrNull treatment assertionsToModel gives value/path/op.
func guardsToModel(ctx context.Context, in []client.MetricGuard, prior types.Set) (types.Set, diag.Diagnostics) {
	if len(in) == 0 {
		return monitorGuardSetNull(), nil
	}
	models := make([]monitorGuardModel, 0, len(in))
	for _, g := range in {
		models = append(models, monitorGuardModel{
			Name:        types.StringValue(g.Name),
			Path:        types.StringValue(g.Path),
			WindowS:     types.Int64Value(g.WindowS),
			Ceiling:     types.Float64Value(g.Ceiling),
			Aggregation: types.StringValue(g.Aggregation),
		})
	}
	return types.SetValueFrom(ctx, guardObjectType(), models)
}

// guardsEqual reports whether two API guard sets describe the same collection,
// ignoring order and server-assigned ids.
//
// Order is ignored because the endpoint's read order is not the write order:
// rows come back by (created_at, id) and every row written by one request
// shares a created_at, so the ordering falls through to a random UUID. An
// order-sensitive comparison would issue a pointless replace-the-set PUT on
// most applies that changed nothing.
func guardsEqual(a, b []client.MetricGuard) bool {
	if len(a) != len(b) {
		return false
	}
	key := func(x client.MetricGuard) string {
		// Ceiling is formatted with 'g'/-1: the shortest text that parses back
		// to the same float64, so two ceilings compare equal exactly when the
		// stored doubles are equal. Formatting at a fixed precision would make
		// 0.1 and 0.10000000000000002 look identical and suppress a real diff.
		// \x00 is not producible in name, path or aggregation through
		// Terraform, so it cannot make two different guards collide on one key.
		return strings.Join([]string{
			x.Name, x.Path, x.Aggregation,
			strconv.FormatInt(x.WindowS, 10),
			strconv.FormatFloat(x.Ceiling, 'g', -1, 64),
		}, "\x00")
	}
	ka := make([]string, 0, len(a))
	kb := make([]string, 0, len(b))
	for _, x := range a {
		ka = append(ka, key(x))
	}
	for _, x := range b {
		kb = append(kb, key(x))
	}
	sort.Strings(ka)
	sort.Strings(kb)
	for i := range ka {
		if ka[i] != kb[i] {
			return false
		}
	}
	return true
}

// validateGuards mirrors core/metricguard.Validate — the function the API
// applies at write time — at plan time, so a malformed guard names the
// offending block instead of arriving as a 400 partway through an apply.
//
// It also refuses guards on an http monitor, which the API tolerates but which
// is always a mistake in a configuration: a probe has no ping body, so the
// guard could never be evaluated.
func validateGuards(ctx context.Context, cfg monitorResourceModel, resp *resource.ValidateConfigResponse) {
	set := cfg.Guards
	if set.IsNull() || set.IsUnknown() {
		return
	}

	var models []monitorGuardModel
	if diags := set.ElementsAs(ctx, &models, false); diags.HasError() {
		resp.Diagnostics.Append(diags...)
		return
	}

	if len(models) > maxGuardsPerMonitor {
		resp.Diagnostics.AddAttributeError(path.Root("metric_guard"), "Too many metric guards",
			fmt.Sprintf("A monitor may carry at most %d metric guards; this configuration has %d. "+
				"Each guard re-aggregates the ping bodies in its window on every ping, so the per-ping "+
				"cost is linear in the number of guards, and the API rejects the whole write above that cap.",
				maxGuardsPerMonitor, len(models)))
		return
	}

	if !cfg.MonitorType.IsUnknown() && cfg.MonitorType.ValueString() == "http" && len(models) > 0 {
		resp.Diagnostics.AddAttributeError(path.Root("metric_guard"),
			"metric_guard is not supported on an http monitor",
			"Metric guards read a number out of the body of a ping sent by the job itself. A monitor "+
				"with monitor_type = \"http\" is probed by LastPing — there is no ping body to read — so a "+
				"guard configured here would be stored and never evaluated.\n\nRemove the metric_guard "+
				"blocks.")
		return
	}

	for _, m := range models {
		if m.Name.IsUnknown() || m.Path.IsUnknown() || m.WindowS.IsUnknown() ||
			m.Ceiling.IsUnknown() || m.Aggregation.IsUnknown() {
			// A value only known after apply cannot be checked here; the API
			// still enforces the same rules on the write.
			continue
		}
		validateOneGuard(m, resp)
	}
}

// validateOneGuard checks a single block. The messages name the guard so a
// diagnostic on a set-typed block — which has no index to point at — is still
// actionable.
func validateOneGuard(m monitorGuardModel, resp *resource.ValidateConfigResponse) {
	name := m.Name.ValueString()

	addErr := func(summary, detail string) {
		resp.Diagnostics.AddAttributeError(path.Root("metric_guard"), summary,
			fmt.Sprintf("metric_guard %q: %s", name, detail))
	}

	if strings.ContainsAny(m.Path.ValueString(), "[*$") {
		addErr("Guard path is a query, not a dotted path",
			fmt.Sprintf("path %q contains query syntax ('[', '*' or '$'). Only a dotted path "+
				"(\"a.b.c\") is accepted.", m.Path.ValueString()))
	}

	// window_s is already bounded by the schema validators; this catches the
	// case where those are somehow bypassed and keeps the message consistent.
	if w := m.WindowS.ValueInt64(); w <= 0 || w > maxGuardWindowSeconds {
		addErr("Guard window is out of range",
			fmt.Sprintf("window_s must be between 1 and %d seconds (7 days); this one is %d. "+
				"A guard re-reads every ping body in its window on every ping, so the cost is linear "+
				"in the window, and a window past the 90-day ping retention would aggregate over rows "+
				"that have already been pruned.", maxGuardWindowSeconds, w))
	}

	// There is deliberately no finite-ceiling check here, unlike
	// core/metricguard.Validate. Terraform's number type is a big.Float, which
	// has no NaN and no infinity, and terraform-plugin-framework panics if a
	// provider tries to construct one — so a non-finite ceiling cannot reach
	// this function at all. The API keeps its own check because JSON can carry
	// 1e400, which decodes to +Inf.
}

// syncGuards writes the desired guard set for a monitor when it differs from
// what the server already holds, and returns the resulting set as state.
//
// The read-then-compare is not an optimisation for its own sake: the endpoint
// is replace-the-set, so an unconditional PUT would delete and re-insert every
// guard — new ids, new created_at — on every single apply, including the ones
// that changed nothing about them.
//
// current may be nil, which means "not read yet, fetch it".
func (r *monitorResource) syncGuards(
	ctx context.Context, monitorID string, desired []client.MetricGuard, current []client.MetricGuard,
) ([]client.MetricGuard, error) {
	if current == nil {
		got, err := r.client.GetMetricGuards(ctx, monitorID)
		if err != nil {
			return nil, fmt.Errorf("read metric guards: %w", err)
		}
		current = got
	}
	if guardsEqual(desired, current) {
		return current, nil
	}
	out, err := r.client.PutMetricGuards(ctx, monitorID, desired)
	if err != nil {
		return nil, fmt.Errorf("write metric guards: %w", err)
	}
	return out, nil
}
