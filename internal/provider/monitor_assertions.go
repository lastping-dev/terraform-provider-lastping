package provider

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

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

// maxAssertionsPerMonitor mirrors the API's cap (api/api_assertions.go). It is
// checked at plan time so hitting it names the monitor and costs nothing,
// instead of arriving as a 400 partway through an apply.
const maxAssertionsPerMonitor = 20

// maxAssertionPatternBytes mirrors core/assertion's cap on a `matches` pattern.
const maxAssertionPatternBytes = 1000

// assertionKinds and assertionOps are the accepted enum values, in the order
// the documentation lists them.
var (
	assertionKinds = []string{"contains", "not_contains", "matches", "json_path"}
	assertionOps   = []string{"eq", "ne", "gt", "gte", "lt", "lte"}
)

// monitorAssertionModel is one `assertion` block.
//
// There is deliberately no `id` attribute even though the API assigns one. The
// endpoint replaces the entire set on every write, so an assertion's id is not
// stable across an apply that touches any assertion — it is not an identity a
// practitioner could reference, and a Computed attribute inside a set element
// would make the whole element unknown at plan time, which is precisely what a
// set cannot tolerate.
type monitorAssertionModel struct {
	Name  types.String `tfsdk:"name"`
	Kind  types.String `tfsdk:"kind"`
	Value types.String `tfsdk:"value"`
	Path  types.String `tfsdk:"path"`
	Op    types.String `tfsdk:"op"`
}

// assertionObjectType is the object type of one set element. It must agree with
// monitorAssertionModel field-for-field; monitorAssertionSetNull and the
// round-trip test below both depend on it.
func assertionObjectType() types.ObjectType {
	return types.ObjectType{AttrTypes: map[string]attr.Type{
		"name":  types.StringType,
		"kind":  types.StringType,
		"value": types.StringType,
		"path":  types.StringType,
		"op":    types.StringType,
	}}
}

// monitorAssertionBlock is the `assertion` block's schema.
//
// It is a SET, not a list, and that is a decision about the API rather than a
// stylistic one. The endpoint has no per-assertion identity — it replaces the
// whole collection — and it returns rows ordered by (created_at, id), where
// every row written by one request shares a created_at and the tie-break is a
// random UUID. A list would therefore show a permanent, un-appliable diff
// whenever the server handed the same assertions back in a different order. A
// set has no order to disagree about.
func monitorAssertionBlock() schema.SetNestedBlock {
	return schema.SetNestedBlock{
		MarkdownDescription: "An **output assertion**: a condition the ping body of a *successful* run must " +
			"satisfy. Without one, a job that runs on schedule and exits zero is a green monitor even when it " +
			"did nothing — nobody ever looked at what it reported. When an assertion does not hold, the success " +
			"ping opens an incident with cause `assertion`, naming the assertion that failed, exactly as a real " +
			"failure would.\n\n" +
			"The block is repeatable and **replaces the whole set**: the assertions in the configuration are the " +
			"monitor's complete set after an apply, and removing every block removes every assertion. At most " +
			fmt.Sprintf("%d", maxAssertionsPerMonitor) + " per monitor.\n\n" +
			"~> **Assertions read the ping body, so the job has to send one** — for example " +
			"`curl -d '{\"rows_processed\":128}' \"$PING_URL\"`. A monitor with `monitor_type = \"http\"` has no " +
			"ping body at all (the prober makes the request), so assertions configured on one are never " +
			"evaluated; the provider rejects them at plan time rather than storing a setting that cannot fire.",
		NestedObject: schema.NestedBlockObject{
			Attributes: map[string]schema.Attribute{
				"name": schema.StringAttribute{
					Required: true,
					MarkdownDescription: "Human-readable label for this assertion. It appears in the incident " +
						"and in the alert, so it should read as the thing that was supposed to be true — " +
						"`\"rows were written\"`, not `\"assertion 3\"`.",
					Validators: []validator.String{stringvalidator.LengthAtLeast(1)},
				},
				"kind": schema.StringAttribute{
					Required: true,
					MarkdownDescription: "How the body is inspected:\n\n" +
						"* `contains` — the body contains `value` as a substring.\n" +
						"* `not_contains` — the body does not contain `value`.\n" +
						"* `matches` — the body matches `value` as a Go RE2 regular expression " +
						"(no backtracking; patterns are capped at " + fmt.Sprintf("%d", maxAssertionPatternBytes) +
						" bytes).\n" +
						"* `json_path` — the body is parsed as JSON, the value at `path` is read, and compared " +
						"against `value` using `op`.",
					Validators: []validator.String{stringvalidator.OneOf(assertionKinds...)},
				},
				"value": schema.StringAttribute{
					Optional: true,
					MarkdownDescription: "The substring (`contains`, `not_contains`), the regular expression " +
						"(`matches`), or the expected value (`json_path`). Required for every kind but " +
						"`json_path`, where it may be omitted to compare against the empty string.\n\n" +
						"~> **Comparison for `json_path` is numeric only when both sides are numbers.** When the " +
						"value read from the body and this value both parse as numbers the comparison is " +
						"numeric; otherwise both are compared as strings. So `op = \"gt\"` with `value = \"3\"` " +
						"beats `\"12.5\"` lexically but loses to it numerically — and `rows_processed > 0` means " +
						"what it looks like it means.",
					Validators: []validator.String{stringvalidator.LengthAtLeast(1)},
				},
				"path": schema.StringAttribute{
					Optional: true,
					MarkdownDescription: "Dotted path into the body parsed as JSON, for example " +
						"`result.rows_processed`. Required for `kind = \"json_path\"` and meaningless for every " +
						"other kind.\n\n" +
						"~> **This is a dotted path, not JSONPath.** Wildcards, filters and unions (`[`, `*`, " +
						"`$`) are rejected: the syntax a real JSONPath implementation buys is exactly the syntax " +
						"this model does not want, since it turns a one-line assertion into something that needs " +
						"its own review.",
					Validators: []validator.String{stringvalidator.LengthAtLeast(1)},
				},
				"op": schema.StringAttribute{
					Optional: true,
					MarkdownDescription: "Comparison operator for `kind = \"json_path\"`: `eq`, `ne`, `gt`, " +
						"`gte`, `lt`, or `lte`. Required for that kind and meaningless for every other one.",
					Validators: []validator.String{stringvalidator.OneOf(assertionOps...)},
				},
			},
		},
	}
}

// monitorAssertionSetNull is the "no assertion blocks configured" value.
func monitorAssertionSetNull() types.Set {
	return types.SetNull(assertionObjectType())
}

// assertionsFromModel converts the configured blocks into the API's payload.
// It returns a non-nil slice so an empty set is sent as `[]` (clear the set)
// rather than as null.
func assertionsFromModel(ctx context.Context, set types.Set) ([]client.Assertion, error) {
	out := []client.Assertion{}
	if set.IsNull() || set.IsUnknown() {
		return out, nil
	}
	var models []monitorAssertionModel
	if diags := set.ElementsAs(ctx, &models, false); diags.HasError() {
		return nil, fmt.Errorf("read assertion blocks: %v", diags)
	}
	for _, m := range models {
		out = append(out, client.Assertion{
			Name:  m.Name.ValueString(),
			Kind:  m.Kind.ValueString(),
			Value: m.Value.ValueString(),
			Path:  m.Path.ValueString(),
			Op:    m.Op.ValueString(),
		})
	}
	return out, nil
}

// assertionsToModel converts the API's answer back into a set.
//
// prior exists for one case only: the API always answers with an array, so a
// monitor with no assertions comes back as `[]`, which has to map onto whichever
// empty form the configuration used — null when no blocks are written, and the
// same null when the practitioner has removed them all. Terraform has no
// "explicit empty set of blocks" to distinguish from "no blocks", so an empty
// answer is always null here; prior is kept in the signature so this reads the
// same way as tagsValue and so a future explicit-empty form has somewhere to go.
//
// The API's empty string for an unset value/path/op maps back to null. That is
// safe precisely because the schema forbids an explicitly empty string for
// those three (LengthAtLeast(1)): there is no configuration whose "" would come
// back as null and fail the apply as an inconsistent result.
func assertionsToModel(ctx context.Context, in []client.Assertion, prior types.Set) (types.Set, diag.Diagnostics) {
	if len(in) == 0 {
		return monitorAssertionSetNull(), nil
	}
	models := make([]monitorAssertionModel, 0, len(in))
	for _, a := range in {
		models = append(models, monitorAssertionModel{
			Name:  types.StringValue(a.Name),
			Kind:  types.StringValue(a.Kind),
			Value: stringOrNull(a.Value),
			Path:  stringOrNull(a.Path),
			Op:    stringOrNull(a.Op),
		})
	}
	return types.SetValueFrom(ctx, assertionObjectType(), models)
}

// assertionsEqual reports whether two API assertion sets describe the same
// collection, ignoring order and server-assigned ids.
//
// Order is ignored because the endpoint's read order is not the write order:
// rows are returned by (created_at, id) and every row written by one request
// shares a created_at, so the ordering falls through to a random UUID. An
// order-sensitive comparison would issue a pointless replace-the-set PUT on
// most applies that changed nothing.
func assertionsEqual(a, b []client.Assertion) bool {
	if len(a) != len(b) {
		return false
	}
	key := func(x client.Assertion) string {
		// \x00 is not producible in any of these fields through Terraform, so it
		// cannot make two different assertions collide on one key.
		return strings.Join([]string{x.Name, x.Kind, x.Value, x.Path, x.Op}, "\x00")
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

// validateAssertions mirrors core/assertion.Validate — the function the API
// applies at write time and the processor's evaluator is built on — at plan
// time, so a malformed assertion names the offending block instead of arriving
// as a 400 partway through an apply.
//
// It also refuses two things the API tolerates but which are always mistakes in
// a configuration: assertions on an http monitor (which has no ping body, so
// they could never be evaluated) and path/op on a kind that ignores them.
func validateAssertions(ctx context.Context, cfg monitorResourceModel, resp *resource.ValidateConfigResponse) {
	set := cfg.Assertions
	if set.IsNull() || set.IsUnknown() {
		return
	}

	var models []monitorAssertionModel
	if diags := set.ElementsAs(ctx, &models, false); diags.HasError() {
		resp.Diagnostics.Append(diags...)
		return
	}

	if len(models) > maxAssertionsPerMonitor {
		resp.Diagnostics.AddAttributeError(path.Root("assertion"), "Too many assertions",
			fmt.Sprintf("A monitor may carry at most %d output assertions; this configuration has %d. "+
				"The API rejects the whole write above that cap.",
				maxAssertionsPerMonitor, len(models)))
		return
	}

	if !cfg.MonitorType.IsUnknown() && cfg.MonitorType.ValueString() == "http" && len(models) > 0 {
		resp.Diagnostics.AddAttributeError(path.Root("assertion"),
			"assertion is not supported on an http monitor",
			"Output assertions inspect the body of a ping sent by the job itself. A monitor with "+
				"monitor_type = \"http\" is probed by LastPing — there is no ping body to inspect — so an "+
				"assertion configured here would be stored and never evaluated.\n\nRemove the assertion "+
				"blocks. To assert on a probe's response body, use probe_expected_body.")
		return
	}

	for _, m := range models {
		if m.Name.IsUnknown() || m.Kind.IsUnknown() || m.Value.IsUnknown() ||
			m.Path.IsUnknown() || m.Op.IsUnknown() {
			// A value only known after apply cannot be checked here; the API
			// still enforces the same rules on the write.
			continue
		}
		validateOneAssertion(m, resp)
	}
}

// validateOneAssertion checks a single block. The messages name the assertion
// so a diagnostic on a set-typed block — which has no index to point at — is
// still actionable.
func validateOneAssertion(m monitorAssertionModel, resp *resource.ValidateConfigResponse) {
	name := m.Name.ValueString()
	kind := m.Kind.ValueString()
	hasValue := !m.Value.IsNull() && m.Value.ValueString() != ""
	hasPath := !m.Path.IsNull() && m.Path.ValueString() != ""
	hasOp := !m.Op.IsNull() && m.Op.ValueString() != ""

	addErr := func(summary, detail string) {
		resp.Diagnostics.AddAttributeError(path.Root("assertion"), summary,
			fmt.Sprintf("assertion %q: %s", name, detail))
	}

	switch kind {
	case "contains", "not_contains", "matches":
		if !hasValue {
			addErr("Assertion is missing value",
				fmt.Sprintf("kind = %q requires value.", kind))
			return
		}
		if hasPath || hasOp {
			addErr("Assertion sets an attribute its kind ignores",
				fmt.Sprintf("path and op are only meaningful for kind = \"json_path\"; kind is %q here. "+
					"Remove them so the configuration says what the monitor actually does.", kind))
		}
		if kind != "matches" {
			return
		}
		pattern := m.Value.ValueString()
		if len(pattern) > maxAssertionPatternBytes {
			addErr("Assertion pattern is too long",
				fmt.Sprintf("a matches pattern may be at most %d bytes; this one is %d.",
					maxAssertionPatternBytes, len(pattern)))
			return
		}
		if _, err := regexp.Compile(pattern); err != nil {
			addErr("Assertion pattern is not a valid regular expression",
				fmt.Sprintf("value is compiled as a Go RE2 regular expression, and this one does not "+
					"compile: %s", err))
		}

	case "json_path":
		if !hasPath {
			addErr("Assertion is missing path", "kind = \"json_path\" requires path, for example "+
				"\"result.rows_processed\".")
			return
		}
		if strings.ContainsAny(m.Path.ValueString(), "[*$") {
			addErr("Assertion path is a query, not a dotted path",
				fmt.Sprintf("path %q contains query syntax ('[', '*' or '$'). Only a dotted path "+
					"(\"a.b.c\") is accepted.", m.Path.ValueString()))
		}
		if !hasOp {
			addErr("Assertion is missing op",
				fmt.Sprintf("kind = \"json_path\" requires op (one of %s).", strings.Join(assertionOps, ", ")))
		}
	}
}

// syncAssertions writes the desired assertion set for a monitor when it differs
// from what the server already holds, and returns the resulting set as state.
//
// The read-then-compare is not an optimisation for its own sake: the endpoint is
// replace-the-set, so an unconditional PUT would delete and re-insert every
// assertion — new ids, new created_at — on every single apply, including the
// ones that changed nothing about them.
//
// current may be nil, which means "not read yet, fetch it".
func (r *monitorResource) syncAssertions(
	ctx context.Context, monitorID string, desired []client.Assertion, current []client.Assertion,
) ([]client.Assertion, error) {
	if current == nil {
		got, err := r.client.GetAssertions(ctx, monitorID)
		if err != nil {
			return nil, fmt.Errorf("read assertions: %w", err)
		}
		current = got
	}
	if assertionsEqual(desired, current) {
		return current, nil
	}
	out, err := r.client.PutAssertions(ctx, monitorID, desired)
	if err != nil {
		return nil, fmt.Errorf("write assertions: %w", err)
	}
	return out, nil
}
