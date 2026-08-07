package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/stretchr/testify/require"

	"github.com/lastping-dev/terraform-provider-lastping/internal/client"
)

// assertionObj builds one `assertion` block element. Empty strings become null,
// which is what the schema's LengthAtLeast(1) validators force a practitioner
// to write anyway.
func assertionObj(name, kind, value, path, op string) attr.Value {
	str := func(s string) attr.Value {
		if s == "" {
			return types.StringNull()
		}
		return types.StringValue(s)
	}
	return types.ObjectValueMust(assertionObjectType().AttrTypes, map[string]attr.Value{
		"name":  str(name),
		"kind":  str(kind),
		"value": str(value),
		"path":  str(path),
		"op":    str(op),
	})
}

func assertionSet(elems ...attr.Value) types.Set {
	return types.SetValueMust(assertionObjectType(), elems)
}

// A null set is "no assertion blocks configured", and it has to leave the API
// with an EMPTY ARRAY rather than a null: the endpoint is replace-the-set, so
// `[]` is the only thing that means "this monitor now has none". A nil slice
// would marshal to `null` and read as "no assertions key", which is not the
// same instruction.
func TestAssertionsFromModel_NullSetIsAnEmptyArrayNotNil(t *testing.T) {
	got, err := assertionsFromModel(context.Background(), monitorAssertionSetNull())
	require.NoError(t, err)
	require.NotNil(t, got, "a null set must produce [] so the PUT clears the stored assertions")
	require.Empty(t, got)
}

// The blocks a practitioner writes must reach the wire verbatim, including the
// empty strings the API uses for "unset" on the kinds that ignore path/op.
func TestAssertionsFromModel_MapsEveryField(t *testing.T) {
	set := assertionSet(
		assertionObj("rows written", "json_path", "0", "result.rows_processed", "gt"),
		assertionObj("no traceback", "not_contains", "Traceback", "", ""),
	)

	got, err := assertionsFromModel(context.Background(), set)
	require.NoError(t, err)
	require.Len(t, got, 2)

	// A set has no order, so index into it by name.
	byName := map[string]client.Assertion{}
	for _, a := range got {
		byName[a.Name] = a
	}
	require.Equal(t, client.Assertion{
		Name: "rows written", Kind: "json_path", Value: "0",
		Path: "result.rows_processed", Op: "gt",
	}, byName["rows written"])
	require.Equal(t, client.Assertion{
		Name: "no traceback", Kind: "not_contains", Value: "Traceback",
	}, byName["no traceback"])
}

// The round trip is what the plan/apply consistency check compares, so a field
// dropped or reshaped in either direction is an apply-time "provider produced
// inconsistent result", not a quiet cosmetic difference.
func TestAssertionsRoundTripThroughTheAPIShape(t *testing.T) {
	ctx := context.Background()
	set := assertionSet(
		assertionObj("rows written", "json_path", "0", "result.rows_processed", "gt"),
		assertionObj("no traceback", "not_contains", "Traceback", "", ""),
		assertionObj("completion line", "matches", "^done: [0-9]+$", "", ""),
	)

	wire, err := assertionsFromModel(ctx, set)
	require.NoError(t, err)

	// The server echoes the set back with ids assigned. Those must not leak
	// into state — there is no id attribute for them to land in.
	for i := range wire {
		wire[i].ID = "11111111-2222-4333-8444-55555555555" + string(rune('0'+i))
	}

	back, diags := assertionsToModel(ctx, wire, set)
	require.False(t, diags.HasError(), "%v", diags)
	require.True(t, set.Equal(back),
		"the set read back must equal the set configured\nwant: %s\ngot:  %s", set, back)
}

// An API answer of `[]` has to become a null set, because that is the only
// empty form the configuration can express — there is no way to write "zero
// assertion blocks, explicitly". Returning an empty non-null set instead would
// be a permanent diff against every monitor that has none.
func TestAssertionsToModel_EmptyAnswerIsNull(t *testing.T) {
	got, diags := assertionsToModel(context.Background(), []client.Assertion{}, monitorAssertionSetNull())
	require.False(t, diags.HasError(), "%v", diags)
	require.True(t, got.IsNull())

	// Same answer when the prior state DID hold assertions: an assertion set
	// cleared out of band must surface as null, not be masked by the stale
	// value.
	prior := assertionSet(assertionObj("gone", "contains", "x", "", ""))
	got, diags = assertionsToModel(context.Background(), nil, prior)
	require.False(t, diags.HasError(), "%v", diags)
	require.True(t, got.IsNull(), "assertions deleted out of band must show as drift, not be hidden")
}

// assertionsEqual decides whether an apply issues a replace-the-set PUT at all.
// It must ignore order, because the endpoint returns rows by (created_at, id)
// and every row written by one request shares a created_at — the tie-break is a
// random UUID, so the read order is not the write order. It must ignore ids for
// the same reason. And it must NOT ignore anything else.
func TestAssertionsEqual(t *testing.T) {
	a := client.Assertion{Name: "rows", Kind: "json_path", Value: "0", Path: "result.rows", Op: "gt"}
	b := client.Assertion{Name: "no traceback", Kind: "not_contains", Value: "Traceback"}

	// Same two assertions, opposite order, different server ids.
	withIDs := []client.Assertion{
		{Name: b.Name, Kind: b.Kind, Value: b.Value, ID: "id-b"},
		{Name: a.Name, Kind: a.Kind, Value: a.Value, Path: a.Path, Op: a.Op, ID: "id-a"},
	}
	require.True(t, assertionsEqual([]client.Assertion{a, b}, withIDs),
		"order and server ids must not count as a difference")

	// Every field that is NOT ignored, one at a time. Each differs from `a` in
	// exactly one place, so a comparison that dropped that field would pass.
	for _, tc := range []struct {
		name string
		mod  client.Assertion
	}{
		{"name", client.Assertion{Name: "ROWS", Kind: a.Kind, Value: a.Value, Path: a.Path, Op: a.Op}},
		{"kind", client.Assertion{Name: a.Name, Kind: "contains", Value: a.Value, Path: a.Path, Op: a.Op}},
		{"value", client.Assertion{Name: a.Name, Kind: a.Kind, Value: "1", Path: a.Path, Op: a.Op}},
		{"path", client.Assertion{Name: a.Name, Kind: a.Kind, Value: a.Value, Path: "result.other", Op: a.Op}},
		{"op", client.Assertion{Name: a.Name, Kind: a.Kind, Value: a.Value, Path: a.Path, Op: "gte"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.False(t, assertionsEqual([]client.Assertion{a}, []client.Assertion{tc.mod}),
				"a difference in %s must be detected", tc.name)
		})
	}

	// Different lengths.
	require.False(t, assertionsEqual([]client.Assertion{a}, []client.Assertion{a, b}))
	// Both empty.
	require.True(t, assertionsEqual(nil, []client.Assertion{}))
}

// validateAssertions mirrors core/assertion.Validate at plan time. Every case
// here is a configuration the API would reject with a 400 partway through an
// apply; catching them in the plan is the difference between an error that
// names the assertion and one that arrives after Terraform has started
// changing things.
func TestValidateAssertionsRejectsWhatTheAPIWouldReject(t *testing.T) {
	base := monitorResourceModel{
		Name:         types.StringValue("acc"),
		MonitorType:  types.StringValue("heartbeat"),
		ScheduleKind: types.StringValue("simple"),
		PeriodS:      types.Int64Value(3600),
		Tags:         types.SetNull(types.StringType),
		Assertions:   monitorAssertionSetNull(),
	}

	for _, tc := range []struct {
		name      string
		set       types.Set
		wantError string // "" means the configuration must be accepted
	}{
		{
			name: "valid json_path",
			set:  assertionSet(assertionObj("rows", "json_path", "0", "result.rows", "gt")),
		},
		{
			name: "valid contains",
			set:  assertionSet(assertionObj("done", "contains", "done", "", "")),
		},
		{
			name: "valid matches",
			set:  assertionSet(assertionObj("done", "matches", "^done: [0-9]+$", "", "")),
		},
		{
			name:      "contains without value",
			set:       assertionSet(assertionObj("empty", "contains", "", "", "")),
			wantError: "missing value",
		},
		{
			name:      "matches with an uncompilable pattern",
			set:       assertionSet(assertionObj("broken", "matches", "(unclosed", "", "")),
			wantError: "not a valid regular expression",
		},
		{
			name: "matches with an over-long pattern",
			set: assertionSet(assertionObj("huge", "matches",
				strings.Repeat("a", maxAssertionPatternBytes+1), "", "")),
			wantError: "too long",
		},
		{
			// Exactly at the cap is accepted; one over is the case above. Both
			// sides are pinned so an off-by-one fails here either way.
			name: "matches at exactly the pattern cap",
			set: assertionSet(assertionObj("big", "matches",
				strings.Repeat("a", maxAssertionPatternBytes), "", "")),
		},
		{
			name:      "json_path without path",
			set:       assertionSet(assertionObj("nopath", "json_path", "0", "", "gt")),
			wantError: "missing path",
		},
		{
			name:      "json_path without op",
			set:       assertionSet(assertionObj("noop", "json_path", "0", "result.rows", "")),
			wantError: "missing op",
		},
		{
			name:      "json_path with query syntax in the path",
			set:       assertionSet(assertionObj("query", "json_path", "1", "items[*].id", "eq")),
			wantError: "query, not a dotted path",
		},
		{
			name:      "path on a kind that ignores it",
			set:       assertionSet(assertionObj("strays", "contains", "done", "result.rows", "")),
			wantError: "ignores",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			cfg.Assertions = tc.set
			diags := monitorValidateConfig(t, cfg)
			if tc.wantError == "" {
				require.False(t, diags.HasError(), "%v", diags)
				return
			}
			require.True(t, diags.HasError(), "this configuration must be refused at plan time")
			joined := diags.Errors()[0].Summary() + " " + diags.Errors()[0].Detail()
			require.Contains(t, joined, tc.wantError)
		})
	}
}

// The cap is 20. Twenty is accepted and twenty-one is refused, so an off-by-one
// in either direction fails here rather than becoming a 400 mid-apply.
func TestValidateAssertionsCapIsTwenty(t *testing.T) {
	base := monitorResourceModel{
		Name:         types.StringValue("acc"),
		MonitorType:  types.StringValue("heartbeat"),
		ScheduleKind: types.StringValue("simple"),
		PeriodS:      types.Int64Value(3600),
		Tags:         types.SetNull(types.StringType),
		Assertions:   monitorAssertionSetNull(),
	}
	build := func(n int) types.Set {
		elems := make([]attr.Value, 0, n)
		for i := range n {
			// Distinct names: a set silently collapses duplicates, which would
			// make an n-element fixture assert against fewer than n.
			elems = append(elems, assertionObj("assertion-"+string(rune('a'+i)), "contains", "x", "", ""))
		}
		return assertionSet(elems...)
	}

	require.Equal(t, maxAssertionsPerMonitor, len(build(maxAssertionsPerMonitor).Elements()),
		"the fixture must really hold 20 distinct assertions")

	cfg := base
	cfg.Assertions = build(maxAssertionsPerMonitor)
	require.False(t, monitorValidateConfig(t, cfg).HasError(), "the cap itself must be accepted")

	cfg.Assertions = build(maxAssertionsPerMonitor + 1)
	diags := monitorValidateConfig(t, cfg)
	require.True(t, diags.HasError(), "one over the cap must be refused")
	require.Contains(t, diags.Errors()[0].Summary(), "Too many assertions")
}

// An http monitor is probed by LastPing; there is no ping body for an assertion
// to inspect. The API stores them happily, which is worse than refusing them —
// the practitioner gets a monitor with assertions that can never fire and no
// signal that anything is wrong.
func TestValidateAssertionsRejectedOnHTTPMonitor(t *testing.T) {
	base := monitorResourceModel{
		Name:        types.StringValue("acc"),
		MonitorType: types.StringValue("http"),
		ProbeURL:    types.StringValue("https://example.com/"),
		Tags:        types.SetNull(types.StringType),
		Assertions:  monitorAssertionSetNull(),
	}

	// No assertions on an http monitor: fine.
	require.False(t, monitorValidateConfig(t, base).HasError())

	withAssertion := base
	withAssertion.Assertions = assertionSet(assertionObj("rows", "contains", "done", "", ""))
	diags := monitorValidateConfig(t, withAssertion)
	require.True(t, diags.HasError(), "assertions on an http monitor must be refused at plan time")
	require.Contains(t, diags.Errors()[0].Summary(), "http monitor")

	// The same assertion on a heartbeat is accepted, so it is the monitor type
	// being refused and not the assertion itself.
	heartbeat := withAssertion
	heartbeat.MonitorType = types.StringValue("heartbeat")
	heartbeat.ProbeURL = types.StringNull()
	require.False(t, monitorValidateConfig(t, heartbeat).HasError())
}
