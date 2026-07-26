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
// These tests read the published spec and assert that every attribute the
// provider sends exists as a request property, and every attribute it reads
// back exists as a response property. Drift becomes a failing PR check.
//
// Deliberate mismatches are declared, not tolerated in silence: each entry in a
// case's sendExempt/readExempt map carries the reason it cannot be a spec
// property. knownSpecGaps (below) is separate and stricter — it names fields
// the API genuinely implements but the spec omits, and it fails once the spec
// is fixed so the entry cannot outlive the bug.
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
var knownSpecGaps = map[string]string{
	// maintenance_until is returned by the API — api/checks.go, checkResponse
	// has `MaintenanceUntil *time.Time \`json:"maintenance_until,omitempty"\`` —
	// and drives the dashboard's snooze display, but the Check schema in the
	// published spec never lists it. MONOREPO BUG: add maintenance_until to
	// components.schemas.Check, then delete these three entries.
	"resource.lastping_monitor.maintenance_until":       "api/checks.go checkResponse returns it; components.schemas.Check omits it",
	"data.lastping_monitor.maintenance_until":           "api/checks.go checkResponse returns it; components.schemas.Check omits it",
	"data.lastping_monitors.monitors.maintenance_until": "api/checks.go checkResponse returns it; components.schemas.Check omits it",
}

// destinationConfigExempt is the reason every per-kind credential attribute on
// lastping_destination has no spec property of its own. They are flattened by
// the provider out of the API's single free-form `config` object, which the
// spec declares as `additionalProperties: true` with no named properties — so
// there is nothing for a property check to match against, in either direction.
const destinationConfigExempt = "flattened out of the API's free-form `config` object " +
	"(ChannelCreate.config is additionalProperties, and the API never returns config at all)"

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
