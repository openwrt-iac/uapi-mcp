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
	if len(c.Endpoints) != 266 {
		t.Errorf("got %d endpoints, want 266", len(c.Endpoints))
	}
	if c.UAPIVersion != "3.3.0" {
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
		// Adopt takes the id and nothing else.
		adopt := strings.HasSuffix(ep.Tool, "_adopt")
		switch ep.Method {
		case "GET", "DELETE":
			if ep.BodyKind == "flat" || ep.BodyKind == "wrapped" {
				t.Errorf("%s: %s carries a body", ep.Tool, ep.Method)
			}
		case "POST":
			if adopt && ep.BodyKind != "" {
				t.Errorf("%s: adopt carries a body", ep.Tool)
			}
			if !adopt && ep.BodyKind != "flat" && ep.BodyKind != "wrapped" {
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
	if byTool["tokens_list"].Paginated {
		t.Error("tokens_list must not be paginated: GET /tokens answers an object")
	}
	if !byTool["firewall_rules_list"].Paginated {
		t.Error("firewall_rules_list must be paginated: GET /firewall/rules answers an array")
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
