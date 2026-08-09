package provider

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	dschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	eschema "github.com/hashicorp/terraform-plugin-framework/ephemeral/schema"
	fwprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// specPath is the vendored copy of the public OpenAPI document, refreshed with
// `make sync-openapi`. It lives at the repository root rather than under this
// package's own testdata/ because it is the project's single vendored copy of
// the API contract, not a fixture private to one test.
const specPath = "../../testdata/openapi.yaml"

// ─────────────────────────────────────────────────────────────────────────────
// What this file is for
//
// The provider and the API are two codebases with one shared vocabulary. When
// the API renames or drops a field, nothing in the provider's own test suite
// notices: the Go types still compile, the unit tests still pass, and the
// failure lands on a user's `terraform apply` as an unexplained 400 or a
// silently-ignored attribute.
//
// These tests read the published spec and check both directions of the same
// relationship. TestOpenAPIContract asserts that every attribute the provider
// sends exists as a request property, and every attribute it reads back
// exists as a response property — this catches the provider claiming a field
// the API does not have. TestOpenAPIContract_SpecCoverage asserts the mirror:
// that every request/response property the spec declares (on a schema an
// existing case already checks) is modelled by the provider — this catches
// the API growing a field the provider silently ignores, which is the more
// common direction of drift since the API leads and Terraform follows.
//
// Deliberate mismatches are declared, not tolerated in silence, in both
// directions:
//   - sendExempt/readExempt (per case): a provider attribute has no spec
//     property, and never should — TestOpenAPIContract skips it.
//   - knownSpecGaps (below): the API genuinely implements a provider
//     attribute the *spec* omits — a documentation bug, not tolerated
//     forever: it fails once the spec is fixed so the entry cannot outlive it.
//   - deliberatelyUnmodelled (below): a spec property has no provider
//     attribute, and never should — TestOpenAPIContract_SpecCoverage skips it.
//   - knownModellingGaps (below): the provider genuinely omits a spec
//     property that probably SHOULD be modelled — a real, temporary gap, not
//     a design decision, that fails once someone models it (or reclassifies
//     it into deliberatelyUnmodelled) so the entry cannot outlive it either.
// ─────────────────────────────────────────────────────────────────────────────

// knownSpecGaps are provider attributes backed by real API behaviour that the
// *published spec* does not document. They are bugs in the monorepo's
// api/swagger/openapi.yaml, not in the provider, and they are listed here so
// the contract test can stay green without pretending the gap is fine.
//
// Each entry is asserted to STILL be missing. When the spec is fixed and
// `make sync-openapi` pulls the new copy, the entry fails and must be deleted —
// so this map cannot quietly become a dumping ground.
//
// Keys are "<case name>.<attribute>".
//
// Empty is the healthy state, and the map is kept rather than deleted so the
// next gap is recorded here — visibly, with a reason — instead of being waved
// through an exempt map. The last three entries (maintenance_until on the
// monitor resource and both monitor data sources) went when the monorepo added
// the property to components.schemas.Check.
//
// step_timeout_s did NOT need an entry, and the near miss is worth recording:
// the deployed https://app.lastping.dev/openapi.yaml — what `make sync-openapi`
// fetches — was behind the monorepo and omitted the property, while
// api/swagger/openapi.yaml on main declared it on CheckCreate, Check and
// CheckPatch all along. A deployment lag reads exactly like a spec gap from
// here, so confirm against the monorepo's file, not the served document, before
// adding an entry.
//
// notify_min_run_s was the same deployment-lag shape: three entries covering
// resource.lastping_monitor, data.lastping_monitor and
// data.lastping_monitors.monitors went once a resync picked up a deployment
// that carried the property on CheckCreate, Check and CheckPatch.
var knownSpecGaps = map[string]string{}

// deliberatelyUnmodelled records spec properties that live on a schema an
// existing contractCase already checks, that the provider does NOT model, and
// that a human has decided it never should — a permanent design decision, the
// reverse-direction mirror of a case's sendExempt/readExempt map rather than
// of knownSpecGaps.
//
// This is NOT where a genuinely missing attribute goes. Each entry is a claim
// that the property has no business being Terraform state, and a wrong claim
// hides a real gap exactly as effectively as a bug — see knownModellingGaps
// for where a real, not-yet-fixed gap belongs instead. Do not add an entry
// here just to silence TestOpenAPIContract_SpecCoverage.
//
// Keys are "<case name>[.<nested attribute>].<spec property name>", matching
// knownSpecGaps' key shape — see splitGapKey.
var deliberatelyUnmodelled = map[string]string{
	// last_used_at and last_used_surface (ApiKey, inherited by ApiKeyCreated)
	// are REST/MCP/UI-only by design — see the monorepo's api/apikeys_api.go,
	// the comment on apiKeyResponse: last_used_at changes on every
	// authenticated request, so a computed Terraform attribute tracking it
	// would produce a permanent, meaningless diff on every `terraform plan`.
	// last_used_surface is derived from the caller-controlled, spoofable
	// User-Agent header — best-effort client self-identification, never
	// authorization, and not state a config-as-code tool should converge on.
	"resource.lastping_api_key.last_used_at":       apiKeyTelemetryExempt,
	"resource.lastping_api_key.last_used_surface":  apiKeyTelemetryExempt,
	"ephemeral.lastping_api_key.last_used_at":      apiKeyTelemetryExempt,
	"ephemeral.lastping_api_key.last_used_surface": apiKeyTelemetryExempt,

	// The ephemeral resource never surfaces created_at or expires_at (only the
	// managed lastping_api_key resource does). This mirrors the same map's
	// ttl-vs-expires_at split the *forward* direction already exempts: the
	// ephemeral resource's whole point, spelled out in its own schema
	// description, is that nothing about it is written to plan or state — the
	// caller configures a relative ttl and gets back only what using the key
	// requires (id, prefix, key). Absolute timestamps that only matter for a
	// value meant to be reconciled across runs add nothing for a value that
	// lives and dies within one.
	"ephemeral.lastping_api_key.created_at": "ephemeral.lastping_api_key deliberately keeps no " +
		"persisted-looking fields — see the resource's own schema description. created_at describes " +
		"a record that is never refreshed or diffed here, so exposing it would suggest state this " +
		"resource explicitly does not keep.",
	"ephemeral.lastping_api_key.expires_at": "ephemeral.lastping_api_key models its lifetime as the " +
		"caller-supplied ttl (a duration), not the server's absolute expires_at — the same asymmetry " +
		"ttl's sendExempt/readExempt entries already document. The resource is consumed and gone " +
		"within one run, so there is nothing to reconcile an absolute timestamp against.",

	// ci_configured is `true` exactly when ci_provider is set and omitted
	// otherwise (see its spec description) — a boolean mirror of information
	// the provider already exposes via ci_provider's presence/absence. A
	// second attribute that is always derivable from the first would be
	// redundant state, not a new capability.
	"resource.lastping_monitor.ci_configured":       ciConfiguredExempt,
	"data.lastping_monitor.ci_configured":           ciConfiguredExempt,
	"data.lastping_monitors.monitors.ci_configured": ciConfiguredExempt,

	// ChannelCreate.config is the API's single free-form request object; the
	// provider flattens it into named, kind-specific attributes instead of
	// exposing "config" itself. This is the same fact destinationConfigExempt
	// already records for the *forward* direction (why none of the flattened
	// attributes has a spec property of its own) — this entry is what closes
	// the loop from the spec's side: "config" itself has no flattened
	// counterpart to point at, because it is not one property but a bag of
	// them, declared additionalProperties: true with no named members.
	"resource.lastping_destination.config": destinationConfigExempt,

	// Route's channel_ids is exactly resource_route.go's destination_ids: the
	// provider says "destination" everywhere the API says "channel", so the
	// attribute exists under a different name rather than not at all. The
	// *forward* direction already exempts destination_ids for the identical
	// reason (sendExempt/readExempt on "resource.lastping_route"); this is
	// that same rename viewed from the spec's side.
	"resource.lastping_route.channel_ids": "provider-side name for the API's channel_ids; the " +
		"provider says \"destination\" everywhere the API says \"channel\" (see destination_ids in " +
		"this case's sendExempt/readExempt)",
}

// apiKeyTelemetryExempt is shared by the managed and ephemeral api_key cases:
// both omit last_used_at/last_used_surface for the same reason.
const apiKeyTelemetryExempt = "REST/MCP/UI-only by design (monorepo api/apikeys_api.go, comment on " +
	"apiKeyResponse): last_used_at changes on every authenticated request, so a computed Terraform " +
	"attribute tracking it would produce a permanent, meaningless diff on every `terraform plan`. " +
	"last_used_surface is derived from the spoofable User-Agent header — best-effort client " +
	"self-identification, never authorization, not state to converge on."

// ciConfiguredExempt is shared by all three monitor-reading cases.
const ciConfiguredExempt = "always exactly (ci_provider != null) — a derived boolean mirror of " +
	"information ci_provider already carries, not a new capability"

// knownModellingGaps names spec properties that a schema an existing contract
// case checks genuinely declares, that the provider does not model, and that
// — unlike a deliberatelyUnmodelled entry — probably SHOULD be modelled. They
// are real gaps this audit found, not design decisions: an entry here is not
// a way to make TestOpenAPIContract_SpecCoverage stop mentioning a property,
// it is a promise that someone still owes Terraform that attribute.
//
// This is the mirror of knownSpecGaps (the API implements something the spec
// omits) for the direction this file exists to catch (the spec declares
// something the provider omits). Exactly like knownSpecGaps,
// TestOpenAPIContract_KnownModellingGapsStillExist asserts every entry is
// STILL missing, so a gap that gets filled fails loudly instead of leaving a
// stale, misleading entry behind — and so this map cannot quietly become a
// dumping ground either.
//
// Empty is the healthy state, same as knownSpecGaps: ci_webhook_url,
// ci_secret and next_probe_at were the three entries this test found on
// components.schemas.Check on 2026-08-09, and all three are now modelled —
// ci_webhook_url and next_probe_at as ordinary Computed attributes
// (resource.lastping_monitor, data.lastping_monitor and
// data.lastping_monitors.monitors), ci_secret the same way but Sensitive and
// carried forward from the create response via writeOnlyString, since no
// later response ever repeats it.
//
// Keys are "<case name>[.<nested attribute>].<spec property name>".
var knownModellingGaps = map[string]string{}

// destinationConfigExempt is the reason every per-kind credential attribute on
// lastping_destination has no spec property of its own. They are flattened by
// the provider out of the API's single free-form `config` object, which the
// spec declares as `additionalProperties: true` with no named properties — so
// there is nothing for a property check to match against, in either direction.
const destinationConfigExempt = "flattened out of the API's free-form `config` object " +
	"(ChannelCreate.config is additionalProperties, and the API never returns config at all)"

// ciFilterWriteOnly is the reason ci_workflow and ci_branch are checked in the
// send direction only.
//
// This is NOT a knownSpecGaps entry, and the distinction is the whole point of
// keeping the two mechanisms apart: the spec is accurate here. CheckCreate and
// CheckPatch both declare the properties, components.schemas.Check deliberately
// does not, and the API agrees — api/checks.go's checkResponse has no field for
// either, so rowToDTO could not populate them if it wanted to. The filters are
// genuinely write-only, and an entry in knownSpecGaps would be asserting that a
// correct spec is wrong.
//
// If the API ever starts returning them, this exemption is what has to go:
// unlike knownSpecGaps it will not fail on its own when that happens, so the
// change lands with the read-direction fix in modelFromMonitor's
// writeOnlyString rather than before it.
const ciFilterWriteOnly = "write-only: accepted on CheckCreate and CheckPatch, but no API response " +
	"carries it (checkResponse has no such field), so the provider carries the prior state forward " +
	"instead of refreshing"

// destinationConfigAttrs is that set, kept in one place so a new credential
// attribute has to be added deliberately.
func destinationConfigExemptions() map[string]string {
	out := map[string]string{}
	for _, a := range allDestinationConfigAttrs {
		out[a] = destinationConfigExempt
	}
	return out
}

// nestedCase describes a nested attribute whose object should be checked
// against a schema of its own — the elements of a list data source, say.
type nestedCase struct {
	response   []string
	readExempt map[string]string
}

// contractCase binds one provider surface to the spec.
//
// request is the schema for what the provider SENDS; only non-computed
// attributes are checked against it, since a computed attribute is by
// definition never part of a request. response is the schema for what the
// provider READS; every attribute is checked against it.
//
// A nil request or response means "there is nothing to check", and skipReason
// must say why — an unexplained nil is how a surface silently stops being
// covered.
type contractCase struct {
	name       string
	request    []string
	response   []string
	skipReason string
	nested     map[string]nestedCase
	sendExempt map[string]string
	readExempt map[string]string
}

func contractCases() map[string]contractCase {
	schema := func(name string) []string { return []string{"components", "schemas", name} }
	reqBody := func(path, method string) []string {
		return []string{"paths", path, method, "requestBody", "content", "application/json", "schema"}
	}
	respBody := func(path, method, status string) []string {
		return []string{"paths", path, method, "responses", status, "content", "application/json", "schema"}
	}

	cases := []contractCase{
		{
			name:     "resource.lastping_monitor",
			request:  schema("CheckCreate"),
			response: schema("Check"),
			sendExempt: map[string]string{
				"paused": "maps onto POST /checks/{id}/pause and /resume, not a body field",
			},
			readExempt: map[string]string{
				"ci_workflow": ciFilterWriteOnly,
				"ci_branch":   ciFilterWriteOnly,
			},
		},
		{
			name:       "resource.lastping_destination",
			request:    schema("ChannelCreate"),
			response:   schema("Channel"),
			sendExempt: destinationConfigExemptions(),
			readExempt: destinationConfigExemptions(),
		},
		{
			name:     "resource.lastping_route",
			request:  reqBody("/api/v1/checks/{id}/routes/{event_type}", "put"),
			response: schema("Route"),
			sendExempt: map[string]string{
				"monitor_id": "path parameter of /api/v1/checks/{id}/routes/{event_type}, not a body field",
				"event_type": "path parameter of /api/v1/checks/{id}/routes/{event_type}, not a body field",
				"destination_ids": "provider-side name for the API's channel_ids; the provider says " +
					"\"destination\" everywhere the API says \"channel\"",
			},
			readExempt: map[string]string{
				"monitor_id": "path parameter; the Route response carries only event_type and channel_ids",
				"destination_ids": "provider-side name for the API's channel_ids; the provider says " +
					"\"destination\" everywhere the API says \"channel\"",
			},
		},
		{
			name:     "resource.lastping_alert_template",
			request:  reqBody("/api/v1/checks/{id}/templates", "put"),
			response: schema("AlertTemplatesResponse"),
			sendExempt: map[string]string{
				"monitor_id": "path parameter of /api/v1/checks/{id}/templates, not a body field",
			},
			readExempt: map[string]string{
				"monitor_id": "path parameter; the response carries only the templates map",
			},
		},
		{
			name:     "resource.lastping_status_page",
			request:  schema("StatusPageCreate"),
			response: schema("StatusPage"),
		},
		{
			name:     "resource.lastping_api_key",
			request:  schema("ApiKeyCreate"),
			response: schema("ApiKeyCreated"),
		},
		{
			// AgentCreate carries only name and description; slug is derived
			// server-side and is not a request property, which the send
			// direction gets right for free because `slug` is computed-only.
			name:     "resource.lastping_agent",
			request:  schema("AgentCreate"),
			response: schema("Agent"),
		},
		{
			name:     "ephemeral.lastping_api_key",
			request:  schema("ApiKeyCreate"),
			response: schema("ApiKeyCreated"),
			sendExempt: map[string]string{
				"ttl": "provider-side concept: a duration the provider converts into the API's " +
					"absolute expires_at before sending",
			},
			readExempt: map[string]string{
				"ttl": "provider-side concept; the API reports absolute expires_at, never a duration",
			},
		},

		// Data sources send no body. Their lookup arguments are path or query
		// parameters, so only the read direction is meaningful — and every
		// attribute, argument or not, is checked against the response schema.
		{
			name:     "data.lastping_monitor",
			response: schema("Check"),
		},
		{
			name:     "data.lastping_monitors",
			response: schema("Check"),
			nested: map[string]nestedCase{
				"monitors": {response: schema("Check")},
			},
			readExempt: map[string]string{
				"tag":      "query parameter of GET /api/v1/checks, not a response field",
				"monitors": "the result-set envelope; its elements are checked against Check",
			},
		},
		{
			name:     "data.lastping_destination",
			response: schema("Channel"),
		},
		{
			name:     "data.lastping_incidents",
			response: schema("Incident"),
			nested: map[string]nestedCase{
				"incidents": {response: schema("Incident")},
			},
			readExempt: map[string]string{
				"monitor_id": "path parameter of GET /api/v1/checks/{id}/incidents, not a response field",
				"limit":      "query parameter of GET /api/v1/checks/{id}/incidents, not a response field",
				"incidents":  "the result-set envelope; its elements are checked against Incident",
			},
		},
		{
			name: "data.lastping_metrics",
			skipReason: "GET /api/v1/metrics answers in Prometheus text exposition format, not JSON; " +
				"the spec types its 200 body as a bare string with no properties to check",
		},
		{
			name:     "data.lastping_project",
			response: respBody("/api/v1/whoami", "get", "200"),
		},
	}

	out := make(map[string]contractCase, len(cases))
	for _, c := range cases {
		out[c.name] = c
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// Spec parsing
// ─────────────────────────────────────────────────────────────────────────────

func loadSpec(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(specPath)
	require.NoError(t, err, "read %s — run `make sync-openapi`", specPath)

	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &doc), "parse %s as YAML", specPath)
	require.Contains(t, doc, "openapi", "%s is not an OpenAPI document — "+
		"`make sync-openapi` may have saved an error page", specPath)
	return doc
}

// resolveNode walks a locator such as
// {"components","schemas","Check"} down the parsed document.
func resolveNode(doc map[string]any, locator []string) (map[string]any, error) {
	node := any(doc)
	for i, key := range locator {
		m, ok := node.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: not a mapping", strings.Join(locator[:i], "."))
		}
		node, ok = m[key]
		if !ok {
			return nil, fmt.Errorf("%s: no such key", strings.Join(locator[:i+1], "."))
		}
	}
	m, ok := node.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: not a mapping", strings.Join(locator, "."))
	}
	return m, nil
}

// schemaProperties returns the property names of an OpenAPI schema node,
// following $ref and flattening allOf so a composed schema such as
// ApiKeyCreated reports its inherited properties too.
func schemaProperties(doc map[string]any, node map[string]any, depth int) (map[string]bool, error) {
	if depth > 10 {
		return nil, fmt.Errorf("schema nesting deeper than 10 levels: probable $ref cycle")
	}

	if ref, ok := node["$ref"].(string); ok {
		locator, err := refLocator(ref)
		if err != nil {
			return nil, err
		}
		target, err := resolveNode(doc, locator)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", ref, err)
		}
		return schemaProperties(doc, target, depth+1)
	}

	out := map[string]bool{}
	if all, ok := node["allOf"].([]any); ok {
		for _, member := range all {
			m, ok := member.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("allOf member is not a mapping")
			}
			props, err := schemaProperties(doc, m, depth+1)
			if err != nil {
				return nil, err
			}
			for k := range props {
				out[k] = true
			}
		}
	}
	if props, ok := node["properties"].(map[string]any); ok {
		for k := range props {
			out[k] = true
		}
	}
	return out, nil
}

// refLocator converts a local JSON pointer such as
// "#/components/schemas/Check" into a locator.
func refLocator(ref string) ([]string, error) {
	if !strings.HasPrefix(ref, "#/") {
		return nil, fmt.Errorf("only local $refs are supported, got %q", ref)
	}
	return strings.Split(strings.TrimPrefix(ref, "#/"), "/"), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Provider schema introspection
// ─────────────────────────────────────────────────────────────────────────────

// attrInfo is one provider attribute reduced to what the contract needs:
// whether the provider can send it, and its nested shape if it has one.
type attrInfo struct {
	computed bool
	nested   map[string]attrInfo
}

// providerSurfaces walks every resource, ephemeral resource and data source the
// provider registers and reduces each to its attribute set. Doing it through
// the provider's own registration lists (rather than a hand-written inventory)
// is what makes a newly-added surface with no contract case a test failure.
func providerSurfaces(t *testing.T) map[string]map[string]attrInfo {
	t.Helper()
	ctx := context.Background()
	p := New("test")()

	out := map[string]map[string]attrInfo{}

	for _, f := range p.Resources(ctx) {
		r := f()
		out["resource."+typeNameOfResource(ctx, r)] = resourceAttrs(t, resourceSchema(t, ctx, r))
	}
	for _, f := range p.(fwprovider.ProviderWithEphemeralResources).EphemeralResources(ctx) {
		e := f()
		out["ephemeral."+typeNameOfEphemeral(ctx, e)] = ephemeralAttrs(t, ephemeralSchema(t, ctx, e))
	}
	for _, f := range p.DataSources(ctx) {
		d := f()
		out["data."+typeNameOfDataSource(ctx, d)] = dataSourceAttrs(t, dataSourceSchema(t, ctx, d))
	}
	return out
}

func typeNameOfResource(ctx context.Context, r resource.Resource) string {
	resp := &resource.MetadataResponse{}
	r.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: "lastping"}, resp)
	return resp.TypeName
}

func typeNameOfEphemeral(ctx context.Context, e ephemeral.EphemeralResource) string {
	resp := &ephemeral.MetadataResponse{}
	e.Metadata(ctx, ephemeral.MetadataRequest{ProviderTypeName: "lastping"}, resp)
	return resp.TypeName
}

func typeNameOfDataSource(ctx context.Context, d datasource.DataSource) string {
	resp := &datasource.MetadataResponse{}
	d.Metadata(ctx, datasource.MetadataRequest{ProviderTypeName: "lastping"}, resp)
	return resp.TypeName
}

func resourceSchema(t *testing.T, ctx context.Context, r resource.Resource) map[string]rschema.Attribute {
	t.Helper()
	resp := &resource.SchemaResponse{}
	r.Schema(ctx, resource.SchemaRequest{}, resp)
	require.False(t, resp.Diagnostics.HasError(), "%v", resp.Diagnostics)
	return resp.Schema.Attributes
}

func ephemeralSchema(t *testing.T, ctx context.Context, e ephemeral.EphemeralResource) map[string]eschema.Attribute {
	t.Helper()
	resp := &ephemeral.SchemaResponse{}
	e.Schema(ctx, ephemeral.SchemaRequest{}, resp)
	require.False(t, resp.Diagnostics.HasError(), "%v", resp.Diagnostics)
	return resp.Schema.Attributes
}

func dataSourceSchema(t *testing.T, ctx context.Context, d datasource.DataSource) map[string]dschema.Attribute {
	t.Helper()
	resp := &datasource.SchemaResponse{}
	d.Schema(ctx, datasource.SchemaRequest{}, resp)
	require.False(t, resp.Diagnostics.HasError(), "%v", resp.Diagnostics)
	return resp.Schema.Attributes
}

func resourceAttrs(t *testing.T, in map[string]rschema.Attribute) map[string]attrInfo {
	t.Helper()
	out := make(map[string]attrInfo, len(in))
	for name, a := range in {
		info := attrInfo{computed: a.IsComputed() && !a.IsOptional() && !a.IsRequired()}
		switch n := a.(type) {
		case rschema.ListNestedAttribute:
			info.nested = resourceAttrs(t, n.NestedObject.Attributes)
		case rschema.SetNestedAttribute:
			info.nested = resourceAttrs(t, n.NestedObject.Attributes)
		case rschema.SingleNestedAttribute:
			info.nested = resourceAttrs(t, n.Attributes)
		}
		out[name] = info
	}
	return out
}

func ephemeralAttrs(t *testing.T, in map[string]eschema.Attribute) map[string]attrInfo {
	t.Helper()
	out := make(map[string]attrInfo, len(in))
	for name, a := range in {
		info := attrInfo{computed: a.IsComputed() && !a.IsOptional() && !a.IsRequired()}
		switch n := a.(type) {
		case eschema.ListNestedAttribute:
			info.nested = ephemeralAttrs(t, n.NestedObject.Attributes)
		case eschema.SetNestedAttribute:
			info.nested = ephemeralAttrs(t, n.NestedObject.Attributes)
		case eschema.SingleNestedAttribute:
			info.nested = ephemeralAttrs(t, n.Attributes)
		}
		out[name] = info
	}
	return out
}

func dataSourceAttrs(t *testing.T, in map[string]dschema.Attribute) map[string]attrInfo {
	t.Helper()
	out := make(map[string]attrInfo, len(in))
	for name, a := range in {
		info := attrInfo{computed: a.IsComputed() && !a.IsOptional() && !a.IsRequired()}
		switch n := a.(type) {
		case dschema.ListNestedAttribute:
			info.nested = dataSourceAttrs(t, n.NestedObject.Attributes)
		case dschema.SetNestedAttribute:
			info.nested = dataSourceAttrs(t, n.NestedObject.Attributes)
		case dschema.SingleNestedAttribute:
			info.nested = dataSourceAttrs(t, n.Attributes)
		}
		out[name] = info
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────────────

// TestOpenAPIContract_EverySurfaceIsCovered fails when a resource or data
// source is registered without a contract case, so new surfaces cannot opt out
// of drift detection by omission.
func TestOpenAPIContract_EverySurfaceIsCovered(t *testing.T) {
	t.Parallel()
	surfaces := providerSurfaces(t)
	cases := contractCases()

	for name := range surfaces {
		require.Contains(t, cases, name,
			"%s has no entry in contractCases(): add one so its attributes are checked "+
				"against the OpenAPI spec", name)
	}
	for name := range cases {
		require.Contains(t, surfaces, name,
			"contractCases() has a stale entry for %s, which the provider no longer registers", name)
	}
}

// TestOpenAPIContract asserts the provider and the published spec agree on
// field names, in both directions.
func TestOpenAPIContract(t *testing.T) {
	t.Parallel()
	doc := loadSpec(t)
	surfaces := providerSurfaces(t)
	cases := contractCases()

	for name, attrs := range surfaces {
		c := cases[name]
		t.Run(name, func(t *testing.T) {
			if c.request == nil && c.response == nil {
				require.NotEmpty(t, c.skipReason,
					"%s checks nothing and gives no reason", name)
				t.Skip(c.skipReason)
			}

			if c.request != nil {
				props := propsAt(t, doc, c.request)
				assertAttrsInSchema(t, name, attrs, props, c.sendExempt, sendDirection)
			}
			if c.response != nil {
				props := propsAt(t, doc, c.response)
				assertAttrsInSchema(t, name, attrs, props, c.readExempt, readDirection)
			}
			for attrName, nc := range c.nested {
				info, ok := attrs[attrName]
				require.True(t, ok, "%s: nested case for unknown attribute %q", name, attrName)
				require.NotNil(t, info.nested, "%s.%s is not a nested attribute", name, attrName)
				props := propsAt(t, doc, nc.response)
				assertAttrsInSchema(t, name+"."+attrName, info.nested, props, nc.readExempt, readDirection)
			}
		})
	}
}

// TestOpenAPIContract_KnownSpecGapsStillExist keeps knownSpecGaps honest. Every
// entry must name a real provider attribute that is still absent from the spec;
// once the monorepo publishes the missing property, this fails and the entry has
// to go — which is how the workaround gets cleaned up instead of accumulating.
func TestOpenAPIContract_KnownSpecGapsStillExist(t *testing.T) {
	t.Parallel()
	doc := loadSpec(t)
	surfaces := providerSurfaces(t)
	cases := contractCases()

	for key, why := range knownSpecGaps {
		caseName, attrPath, ok := splitGapKey(key, cases)
		require.True(t, ok, "knownSpecGaps key %q does not name a known contract case", key)

		c := cases[caseName]
		attrs := surfaces[caseName]
		locator := c.response

		// A nested gap ("data.lastping_monitors.monitors.x") is checked against
		// the nested object's own schema.
		if idx := strings.Index(attrPath, "."); idx >= 0 {
			parent, child := attrPath[:idx], attrPath[idx+1:]
			nc, ok := c.nested[parent]
			require.True(t, ok, "knownSpecGaps key %q: %s has no nested case %q", key, caseName, parent)
			attrs, locator, attrPath = attrs[parent].nested, nc.response, child
		}

		require.Contains(t, attrs, attrPath,
			"knownSpecGaps key %q names an attribute the provider no longer has", key)

		props := propsAt(t, doc, locator)
		require.NotContains(t, props, attrPath,
			"%s is now present in the spec (%s) — the gap is fixed, so delete this "+
				"knownSpecGaps entry.\nOriginal reason: %s", key, strings.Join(locator, "."), why)

		t.Logf("KNOWN SPEC GAP still open: %s — %s", key, why)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Reverse coverage: does the provider model everything the spec declares?
//
// Scope: TestOpenAPIContract_SpecCoverage checks exactly the schemas
// contractCases() already binds to an existing resource, ephemeral resource
// or data source — the same request/response locators TestOpenAPIContract
// uses, walked in the other direction. That set is deliberately the whole
// boundary: contractCases() already enumerates the provider's entire public
// surface (TestOpenAPIContract_EverySurfaceIsCovered fails if a registered
// resource or data source has no case), so a schema that backs no case backs
// nothing the provider could model in the first place. This file does not
// walk the other ~150KB of components.schemas that have no Terraform surface
// to compare against — there is nothing there for "the provider omits this"
// to mean. A case with a skipReason (currently only data.lastping_metrics,
// whose 200 body is Prometheus text with no JSON properties to walk) is
// skipped here for the identical reason it is skipped in the forward test.
// ─────────────────────────────────────────────────────────────────────────────

// TestOpenAPIContract_SpecCoverage is TestOpenAPIContract in reverse: for
// every request/response property the spec declares on a schema an existing
// case checks, some provider attribute must model it — unless the omission is
// recorded, with a reason, in deliberatelyUnmodelled or knownModellingGaps.
//
// An unrecorded miss is exactly the failure mode this file exists to catch:
// the API grew a field and nothing noticed. See the file-level comment for
// why this is the more common direction of drift than the one
// TestOpenAPIContract already guards.
func TestOpenAPIContract_SpecCoverage(t *testing.T) {
	t.Parallel()
	doc := loadSpec(t)
	surfaces := providerSurfaces(t)
	cases := contractCases()

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if c.request == nil && c.response == nil {
				require.NotEmpty(t, c.skipReason, "%s checks nothing and gives no reason", name)
				t.Skip(c.skipReason)
			}

			attrs := surfaces[name]
			if c.request != nil {
				assertSpecPropsModelled(t, name, propsAt(t, doc, c.request), attrs, c.request)
			}
			// A case's own top-level response is skipped when a nested case
			// already points at the identical schema (data.lastping_monitors and
			// data.lastping_incidents: response and nested["monitors"/"incidents"]
			// both resolve to Check/Incident). There the top-level attrs are only
			// the envelope — a filter argument plus the list field itself, always
			// exempted in the forward direction — so walking the schema's
			// properties against them a second time would flag every real
			// property as "missing from the envelope", which the nested walk
			// against the actual element attrs already checks correctly.
			if c.response != nil && !responseSharedWithNested(c) {
				assertSpecPropsModelled(t, name, propsAt(t, doc, c.response), attrs, c.response)
			}
			for attrName, nc := range c.nested {
				info, ok := attrs[attrName]
				require.True(t, ok, "%s: nested case for unknown attribute %q", name, attrName)
				require.NotNil(t, info.nested, "%s.%s is not a nested attribute", name, attrName)
				props := propsAt(t, doc, nc.response)
				assertSpecPropsModelled(t, name+"."+attrName, props, info.nested, nc.response)
			}
		})
	}
}

// assertSpecPropsModelled is the reverse-direction check: every spec
// property must correspond to a provider attribute of the same name, unless
// it is recorded as a deliberate decision or a known, temporary gap.
func assertSpecPropsModelled(
	t *testing.T,
	caseName string,
	props map[string]bool,
	attrs map[string]attrInfo,
	locator []string,
) {
	t.Helper()

	for _, prop := range sortedKeys(props) {
		if _, ok := attrs[prop]; ok {
			continue
		}
		if why, ok := deliberatelyUnmodelled[caseName+"."+prop]; ok {
			require.NotEmpty(t, why, "%s.%s is in deliberatelyUnmodelled with no reason given", caseName, prop)
			continue
		}
		if why, ok := knownModellingGaps[caseName+"."+prop]; ok {
			require.NotEmpty(t, why, "%s.%s is in knownModellingGaps with no reason given", caseName, prop)
			continue
		}

		t.Errorf("the OpenAPI spec's %s declares %q, but no %s attribute models it.\n"+
			"Provider attributes: %s\n\n"+
			"If this should be a Terraform attribute, that is a real gap: report it and add it to "+
			"knownModellingGaps with a reason, but do not implement it as part of a change whose job "+
			"is only to detect drift. If it deliberately should never be a Terraform attribute, add "+
			"%q to deliberatelyUnmodelled with a comment saying why.",
			strings.Join(locator, "."), prop, caseName, strings.Join(sortedAttrNames(attrs), ", "), prop)
	}
}

// sortedAttrNames lists a provider attribute set's names, for error messages.
func sortedAttrNames(attrs map[string]attrInfo) []string {
	out := make([]string, 0, len(attrs))
	for name := range attrs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// responseSharedWithNested reports whether c's own top-level response resolves
// to the identical schema as one of its nested cases — see the comment where
// this is called in TestOpenAPIContract_SpecCoverage.
func responseSharedWithNested(c contractCase) bool {
	for _, nc := range c.nested {
		if locatorEqual(c.response, nc.response) {
			return true
		}
	}
	return false
}

func locatorEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// contractCaseLocators returns every top-level schema location a case checks
// (request and/or response, in that order). Unlike TestOpenAPIContract_
// KnownSpecGapsStillExist's single c.response, a deliberatelyUnmodelled or
// knownModellingGaps entry can legitimately name a request-only property
// (ChannelCreate's config, say), so the stale-entry checks below cannot
// assume the response schema is the right — or only — place to look.
func contractCaseLocators(c contractCase) [][]string {
	var out [][]string
	if c.request != nil {
		out = append(out, c.request)
	}
	if c.response != nil {
		out = append(out, c.response)
	}
	return out
}

// resolveReverseGapKey splits a deliberatelyUnmodelled/knownModellingGaps key
// into the attribute set to check it against and the schema locations that
// might still declare it, following the same case-name-then-optional-nested-
// attribute shape splitGapKey and TestOpenAPIContract_KnownSpecGapsStillExist
// use for knownSpecGaps.
func resolveReverseGapKey(
	t *testing.T,
	key string,
	cases map[string]contractCase,
	surfaces map[string]map[string]attrInfo,
) (attrs map[string]attrInfo, locators [][]string, propName string) {
	t.Helper()

	caseName, propPath, ok := splitGapKey(key, cases)
	require.True(t, ok, "key %q does not name a known contract case", key)

	c := cases[caseName]
	attrs = surfaces[caseName]
	locators = contractCaseLocators(c)
	propName = propPath

	if idx := strings.Index(propPath, "."); idx >= 0 {
		parent, child := propPath[:idx], propPath[idx+1:]
		nc, ok := c.nested[parent]
		require.True(t, ok, "key %q: %s has no nested case %q", key, caseName, parent)
		attrs = attrs[parent].nested
		locators = nil
		if nc.response != nil {
			locators = [][]string{nc.response}
		}
		propName = child
	}
	return attrs, locators, propName
}

// TestOpenAPIContract_DeliberatelyUnmodelledStaysValid keeps
// deliberatelyUnmodelled honest: every entry must name a property the
// provider still does not model, that still exists in the spec. A property
// the provider has since grown means the decision is moot; a property that
// has vanished from the spec means the decision no longer applies to
// anything. Either way the entry is stale and must be deleted, rather than
// left behind to silently cover for something else later.
func TestOpenAPIContract_DeliberatelyUnmodelledStaysValid(t *testing.T) {
	t.Parallel()
	doc := loadSpec(t)
	surfaces := providerSurfaces(t)
	cases := contractCases()

	for key, why := range deliberatelyUnmodelled {
		require.NotEmpty(t, why, "deliberatelyUnmodelled key %q has no reason", key)
		attrs, locators, prop := resolveReverseGapKey(t, key, cases, surfaces)

		require.NotContains(t, attrs, prop,
			"deliberatelyUnmodelled key %q names %q, which the provider now models — the decision is "+
				"moot, so delete this entry.\nOriginal reason: %s", key, prop, why)

		found := false
		for _, loc := range locators {
			if propsAt(t, doc, loc)[prop] {
				found = true
				break
			}
		}
		require.True(t, found, "deliberatelyUnmodelled key %q: %q is no longer present in any schema "+
			"this case checks — delete this entry.\nOriginal reason: %s", key, prop, why)
	}
}

// TestOpenAPIContract_KnownModellingGapsStillExist is knownModellingGaps'
// mirror of TestOpenAPIContract_KnownSpecGapsStillExist: every entry must
// still name a real, still-missing gap, so a gap that gets filled fails
// loudly here instead of leaving a stale entry that quietly hides whatever
// happens to collide with its name next.
func TestOpenAPIContract_KnownModellingGapsStillExist(t *testing.T) {
	t.Parallel()
	doc := loadSpec(t)
	surfaces := providerSurfaces(t)
	cases := contractCases()

	for key, why := range knownModellingGaps {
		require.NotEmpty(t, why, "knownModellingGaps key %q has no reason", key)
		attrs, locators, prop := resolveReverseGapKey(t, key, cases, surfaces)

		require.NotContains(t, attrs, prop,
			"knownModellingGaps key %q names %q, which the provider now models — the gap is closed, "+
				"so delete this entry.\nOriginal reason: %s", key, prop, why)

		found := false
		for _, loc := range locators {
			if propsAt(t, doc, loc)[prop] {
				found = true
				break
			}
		}
		require.True(t, found, "knownModellingGaps key %q: %q is no longer present in any schema this "+
			"case checks — the gap is gone (or the spec regressed); either way, delete this entry.\n"+
			"Original reason: %s", key, prop, why)

		t.Logf("KNOWN MODELLING GAP still open: %s — %s", key, why)
	}
}

// splitGapKey splits "resource.lastping_monitor.maintenance_until" into its
// case name and attribute path by matching the longest case-name prefix, since
// case names themselves contain dots.
func splitGapKey(key string, cases map[string]contractCase) (string, string, bool) {
	for name := range cases {
		if strings.HasPrefix(key, name+".") {
			return name, strings.TrimPrefix(key, name+"."), true
		}
	}
	return "", "", false
}

type direction int

const (
	sendDirection direction = iota
	readDirection
)

// assertAttrsInSchema is the actual contract check.
func assertAttrsInSchema(
	t *testing.T,
	caseName string,
	attrs map[string]attrInfo,
	props map[string]bool,
	exempt map[string]string,
	dir direction,
) {
	t.Helper()

	names := make([]string, 0, len(attrs))
	for name := range attrs {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		// A computed-only attribute is never part of a request body.
		if dir == sendDirection && attrs[name].computed {
			continue
		}
		if props[name] {
			continue
		}
		if why, ok := exempt[name]; ok {
			require.NotEmpty(t, why, "%s.%s is exempt with no reason given", caseName, name)
			continue
		}
		if _, ok := knownSpecGaps[caseName+"."+name]; ok {
			continue
		}

		verb := "sends"
		if dir == readDirection {
			verb = "reads"
		}
		t.Errorf("%s %s %q, but the OpenAPI spec's schema has no such property.\n"+
			"Spec properties: %s\n\n"+
			"Either the provider attribute is wrong, or the spec is out of date "+
			"(refresh it with `make sync-openapi`). If the mismatch is deliberate, add %q to the "+
			"case's exempt map with a comment saying why.",
			caseName, verb, name, strings.Join(sortedKeys(props), ", "), name)
	}
}

func propsAt(t *testing.T, doc map[string]any, locator []string) map[string]bool {
	t.Helper()
	node, err := resolveNode(doc, locator)
	require.NoError(t, err, "locate %s in the spec", strings.Join(locator, "."))
	props, err := schemaProperties(doc, node, 0)
	require.NoError(t, err, "read properties of %s", strings.Join(locator, "."))
	require.NotEmpty(t, props, "%s has no properties — the locator is probably wrong",
		strings.Join(locator, "."))
	return props
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
