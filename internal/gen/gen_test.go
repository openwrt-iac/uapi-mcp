package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type catalog struct {
	UAPIVersion string     `json:"uapi_version"`
	Endpoints   []endpoint `json:"endpoints"`
}

func generateCatalog(t *testing.T) catalog {
	t.Helper()
	raw, err := os.ReadFile("openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	out, err := generate(raw)
	if err != nil {
		t.Fatal(err)
	}
	var c catalog
	if err := json.Unmarshal(out, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// The vendored spec's operation count, /raw excluded. Update this number
// deliberately when bumping the vendored spec: it is the visible size of the
// tool surface.
func TestEndpointCount(t *testing.T) {
	c := generateCatalog(t)
	if len(c.Endpoints) != 258 {
		t.Errorf("got %d endpoints, want 258", len(c.Endpoints))
	}
	if c.UAPIVersion != "4.0.0" {
		t.Errorf("uapi_version = %q", c.UAPIVersion)
	}
}

func TestToolNames(t *testing.T) {
	c := generateCatalog(t)
	seen := map[string]bool{}
	for _, ep := range c.Endpoints {
		if seen[ep.Tool] {
			t.Errorf("duplicate tool name %s", ep.Tool)
		}
		seen[ep.Tool] = true
		if strings.HasPrefix(ep.Tool, "raw") {
			t.Errorf("raw endpoint leaked: %s", ep.Tool)
		}
		if strings.HasPrefix(ep.Path, "/raw") {
			t.Errorf("raw path leaked: %s %s", ep.Tool, ep.Path)
		}
		for _, r := range ep.Tool {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
				t.Errorf("tool name %q holds a character outside [a-z0-9_]", ep.Tool)
				break
			}
		}
	}
	// The names the founding document names as the pattern.
	for _, name := range []string{"network_interfaces_list", "network_rule_replace", "firewall_rule_delete"} {
		if !seen[name] {
			t.Errorf("expected tool %s in the catalog", name)
		}
	}
}

// No input schema may keep validation the server owns: uapi may add enum
// values, loosen bounds or widen types within a major, and a client that
// kept them would reject payloads the server accepts.
func TestInputsAreSoftened(t *testing.T) {
	banned := map[string]bool{
		"enum": true, "const": true, "pattern": true,
		"minLength": true, "maxLength": true,
		"minimum": true, "maximum": true,
		"exclusiveMinimum": true, "exclusiveMaximum": true,
		"minItems": true, "maxItems": true,
		"multipleOf": true, "uniqueItems": true,
		"format": true, "default": true,
		"examples": true, "example": true,
		"allOf": true, "anyOf": true, "oneOf": true,
		"not": true, "if": true, "then": true, "else": true,
		"writeOnly": true, "readOnly": true, "title": true,
	}
	var walk func(v any, where string)
	walk = func(v any, where string) {
		switch val := v.(type) {
		case map[string]any:
			for k, sub := range val {
				if banned[k] || strings.HasPrefix(k, "x-") {
					t.Errorf("%s: forbidden key %q survived softening", where, k)
				}
				walk(sub, where+"/"+k)
			}
		case []any:
			for _, sub := range val {
				walk(sub, where)
			}
		}
	}
	c := generateCatalog(t)
	for _, ep := range c.Endpoints {
		walk(ep.Input, ep.Tool)
	}
}

func TestInputsArePermissiveObjects(t *testing.T) {
	c := generateCatalog(t)
	for _, ep := range c.Endpoints {
		if ep.Input["type"] != "object" {
			t.Errorf("%s: input type = %v, want object", ep.Tool, ep.Input["type"])
		}
		if ep.Input["additionalProperties"] != true {
			t.Errorf("%s: input is not additionalProperties: true", ep.Tool)
		}
		if _, ok := ep.Input["properties"].(map[string]any); !ok {
			t.Errorf("%s: input has no properties map", ep.Tool)
		}
		for _, name := range ep.PathParams {
			if _, ok := ep.Input["properties"].(map[string]any)[name]; !ok {
				t.Errorf("%s: path param %q missing from input", ep.Tool, name)
			}
		}
	}
}

func TestWriteToolsCarryBodies(t *testing.T) {
	c := generateCatalog(t)
	for _, ep := range c.Endpoints {
		// An action, such as adopt or recover, takes the id and nothing else.
		act := action(splitPath(ep.Path)) != ""
		switch ep.Method {
		case "GET", "DELETE":
			if ep.BodyKind == "flat" || ep.BodyKind == "wrapped" {
				t.Errorf("%s: %s carries a body", ep.Tool, ep.Method)
			}
		case "POST":
			if act && ep.BodyKind != "" {
				t.Errorf("%s: action carries a body", ep.Tool)
			}
			if !act && ep.BodyKind != "flat" && ep.BodyKind != "wrapped" {
				t.Errorf("%s: create has no body kind", ep.Tool)
			}
		case "PUT", "PATCH":
			if ep.BodyKind != "flat" && ep.BodyKind != "wrapped" {
				t.Errorf("%s: %s has no body kind", ep.Tool, ep.Method)
			}
		default:
			t.Errorf("%s: unknown method %q", ep.Tool, ep.Method)
		}
	}
}

func TestMergeDescriptionsDisclaimJSONPatch(t *testing.T) {
	c := generateCatalog(t)
	merges := 0
	for _, ep := range c.Endpoints {
		if ep.Method == "PATCH" {
			merges++
			if !strings.Contains(ep.Summary, "RFC 7396 merge-patch") {
				t.Errorf("%s: merge description does not name the merge-patch semantics", ep.Tool)
			}
		}
	}
	if merges == 0 {
		t.Fatal("no PATCH endpoints in the catalog")
	}
}

// Pagination belongs to collection GETs that answer a top-level array.
// Object-shaped collections, such as /schema and /tokens, pass through like
// any other read; walking them as pages would fail to decode.
func TestPaginatedToolsAnswerArrays(t *testing.T) {
	c := generateCatalog(t)
	for _, ep := range c.Endpoints {
		if ep.Paginated {
			if ep.Method != "GET" || len(ep.PathParams) > 0 {
				t.Errorf("%s: paginated, but not a parameterless GET", ep.Tool)
			}
		}
	}
	byTool := map[string]endpoint{}
	for _, ep := range c.Endpoints {
		byTool[ep.Tool] = ep
	}
	if byTool["schema_list"].Paginated {
		t.Error("schema_list must not be paginated: GET /schema answers an object")
	}
	if byTool["uapi_tokens_list"].Paginated {
		t.Error("uapi_tokens_list must not be paginated: GET /uapi/tokens answers an object")
	}
	if !byTool["firewall_rules_list"].Paginated {
		t.Error("firewall_rules_list must be paginated: GET /firewall/rules answers an array")
	}
}

// Since uapi 3.5.0 every list declares limit and cursor. The client walks
// every page itself, so neither may reach the agent: an agent cursor would
// answer a partial list that reads as the whole collection.
func TestClientMechanicsStayHidden(t *testing.T) {
	c := generateCatalog(t)
	for _, ep := range c.Endpoints {
		for _, q := range ep.QueryParams {
			if hiddenQueryParams[q] {
				t.Errorf("%s: exposes the client mechanic %s", ep.Tool, q)
			}
		}
		if !ep.Paginated {
			continue
		}
		props := ep.Input["properties"].(map[string]any)
		for _, name := range []string{"limit", "cursor"} {
			if _, ok := props[name]; ok {
				t.Errorf("%s: input carries %s", ep.Tool, name)
			}
		}
	}
}

// A literal segment after an item parameter is the verb of a POST, as adopt
// always was: POST /uapi/services/{name}/recover is uapi_service_recover.
func TestActionTools(t *testing.T) {
	c := generateCatalog(t)
	byTool := map[string]endpoint{}
	for _, ep := range c.Endpoints {
		byTool[ep.Tool] = ep
		if ep.Method == "POST" && action(splitPath(ep.Path)) != "" && strings.HasSuffix(ep.Tool, "_create") {
			t.Errorf("%s: an action named as a create", ep.Tool)
		}
	}
	for tool, path := range map[string]string{
		"uapi_service_recover": "/uapi/services/{name}/recover",
		"firewall_rule_adopt":  "/firewall/rules/{id}/adopt",
	} {
		if byTool[tool].Path != path {
			t.Errorf("%s: path = %q, want %q", tool, byTool[tool].Path, path)
		}
	}
}

// The spec leaves the description of a Patch field to its Request field. A
// merge field must read as its replace field, and its own enum, which can
// add null to remove the value, folds after the borrowed text.
func TestMergeFieldsCarryReplaceDescriptions(t *testing.T) {
	c := generateCatalog(t)
	byTool := map[string]endpoint{}
	for _, ep := range c.Endpoints {
		byTool[ep.Tool] = ep
	}
	withoutFold := func(d string) string {
		if i := strings.Index(d, "One of (may grow):"); i >= 0 {
			d = d[:i]
		}
		return strings.TrimSpace(d)
	}
	bodyProps := func(ep endpoint) map[string]any {
		props := ep.Input["properties"].(map[string]any)
		if body, ok := props["body"].(map[string]any); ok {
			props, _ = body["properties"].(map[string]any)
		}
		return props
	}
	checked := 0
	for _, merge := range c.Endpoints {
		if merge.Method != "PATCH" {
			continue
		}
		replace, ok := byTool[strings.TrimSuffix(merge.Tool, "_merge")+"_replace"]
		if !ok {
			continue
		}
		rp := bodyProps(replace)
		for field, m := range bodyProps(merge) {
			r, ok := rp[field].(map[string]any)
			if !ok {
				continue
			}
			md, _ := m.(map[string]any)["description"].(string)
			rd, _ := r["description"].(string)
			if withoutFold(md) != withoutFold(rd) {
				t.Errorf("%s.%s: description %q, replace has %q", merge.Tool, field, md, rd)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no merge field checked")
	}
	ovpn := bodyProps(byTool["network_interface_merge"])["ovpnproto"].(map[string]any)["description"].(string)
	if !strings.HasPrefix(ovpn, "proto openvpn only.") {
		t.Errorf("network_interface_merge.ovpnproto = %q", ovpn)
	}
}

// A schema default is the value that a field reads while its uci option is
// unset. The softening drops the keyword, so the value must reach the agent
// in the description, on the merge tool too, whose Patch field has none.
func TestDefaultsFoldIntoDescriptions(t *testing.T) {
	c := generateCatalog(t)
	byTool := map[string]endpoint{}
	for _, ep := range c.Endpoints {
		byTool[ep.Tool] = ep
	}
	field := func(tool string, path ...string) string {
		node := byTool[tool].Input
		for _, name := range path {
			node, _ = node["properties"].(map[string]any)[name].(map[string]any)
		}
		d, _ := node["description"].(string)
		return d
	}
	for _, tc := range []struct {
		desc string
		want string
	}{
		{field("firewall_rules_create", "enabled"), "Reads true when unset."},
		{field("firewall_rule_merge", "body", "enabled"), "Reads true when unset."},
		{field("firewall_rules_create", "match", "family"), `Reads "any" when unset.`},
	} {
		if !strings.HasSuffix(tc.desc, tc.want) {
			t.Errorf("description %q does not end with %q", tc.desc, tc.want)
		}
	}
}

// A parameter that the spec shares through components/parameters reaches
// the tool as an inline one does.
func TestRefParametersResolve(t *testing.T) {
	raw := []byte(`{
		"info": {"version": "0"},
		"paths": {"/dhcp/hosts": {"get": {
			"summary": "List",
			"parameters": [{"$ref": "#/components/parameters/Managed"}, {"$ref": "#/components/parameters/RequestId"}],
			"responses": {"200": {"content": {"application/json": {"schema": {"type": "array"}}}}}
		}}},
		"components": {"parameters": {
			"Managed": {"name": "managed", "in": "query", "description": "Managed only", "schema": {"type": "string", "enum": ["true", "false"]}},
			"RequestId": {"name": "request_id", "in": "query", "schema": {"type": "string"}}
		}}
	}`)
	out, err := generate(raw)
	if err != nil {
		t.Fatal(err)
	}
	var c catalog
	if err := json.Unmarshal(out, &c); err != nil {
		t.Fatal(err)
	}
	ep := c.Endpoints[0]
	if len(ep.QueryParams) != 1 || ep.QueryParams[0] != "managed" {
		t.Errorf("query params = %v, want [managed]", ep.QueryParams)
	}
	if _, ok := ep.Input["properties"].(map[string]any)["managed"]; !ok {
		t.Error("managed missing from the input")
	}
}

func TestDeterministicOutput(t *testing.T) {
	raw, err := os.ReadFile("openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	first, err := generate(raw)
	if err != nil {
		t.Fatal(err)
	}
	second, err := generate(raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Error("generate is not deterministic across runs")
	}
}
