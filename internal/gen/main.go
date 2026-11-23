// Command gen generates internal/tools/catalog.json from the uapi OpenAPI
// spec vendored at internal/gen/openapi.json. Run it from the repo root:
//
//	go run ./internal/gen
//
// The catalog is the complete tool surface: one entry per spec operation,
// minus /raw. Input schemas are the spec's request schemas, softened so the
// client never rejects a payload the server accepts: enums fold into property
// descriptions, patterns and bounds are dropped, objects accept unknown
// properties, and allOf conditionals are dropped outright. Regenerate after
// every uapi spec bump; the uapi-mcp minor follows the uapi minor.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

const (
	specPath    = "internal/gen/openapi.json"
	catalogPath = "internal/tools/catalog.json"
)

// hiddenQueryParams are client mechanics, not agent inputs. The client sends
// idempotency keys itself on POST and walks every page of a list itself, so
// an agent cursor would silently answer a partial list; ETag preconditions
// are not part of the tool surface, and request_id only names the request in
// uapi's logs, which uapi does by itself.
var hiddenQueryParams = map[string]bool{
	"idempotency_key": true,
	"if_match":        true,
	"if_none_match":   true,
	"limit":           true,
	"cursor":          true,
	"request_id":      true,
}

type spec struct {
	Info struct {
		Version string `json:"version"`
	} `json:"info"`
	Paths      map[string]*pathItem `json:"paths"`
	Components struct {
		Schemas    map[string]json.RawMessage `json:"schemas"`
		Parameters map[string]parameter       `json:"parameters"`
	} `json:"components"`
}

type pathItem struct {
	Parameters []parameter `json:"parameters"`
	Get        *operation  `json:"get"`
	Post       *operation  `json:"post"`
	Put        *operation  `json:"put"`
	Patch      *operation  `json:"patch"`
	Delete     *operation  `json:"delete"`
}

type parameter struct {
	Ref         string          `json:"$ref"`
	Name        string          `json:"name"`
	In          string          `json:"in"`
	Required    bool            `json:"required"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}

type operation struct {
	Summary     string      `json:"summary"`
	Description string      `json:"description"`
	Parameters  []parameter `json:"parameters"`
	RequestBody *struct {
		Required bool `json:"required"`
		Content  map[string]struct {
			Schema json.RawMessage `json:"schema"`
		} `json:"content"`
	} `json:"requestBody"`
	Responses map[string]struct {
		Content map[string]struct {
			Schema json.RawMessage `json:"schema"`
		} `json:"content"`
	} `json:"responses"`
}

// endpoint is one catalog entry: one spec operation, in the shape the tool
// layer wires into an MCP tool.
type endpoint struct {
	Tool        string         `json:"tool"`
	Summary     string         `json:"summary"`
	Method      string         `json:"method"`
	Path        string         `json:"path"`
	PathParams  []string       `json:"pathParams,omitempty"`
	QueryParams []string       `json:"queryParams,omitempty"`
	Paginated   bool           `json:"paginated,omitempty"`
	BodyKind    string         `json:"bodyKind,omitempty"`
	Input       map[string]any `json:"input"`
}

type softener struct {
	schemas map[string]json.RawMessage
	params  map[string]parameter
}

// responseIsArray reports whether the 200 response is a top-level JSON array,
// which is the only shape cursor pagination walks. Collections that answer an
// object, such as /schema, pass through like any other read.
func (so *softener) responseIsArray(o *operation) bool {
	resp, ok := o.Responses["200"]
	if !ok {
		return false
	}
	content, ok := resp.Content["application/json"]
	if !ok {
		return false
	}
	var s map[string]any
	if err := json.Unmarshal(content.Schema, &s); err != nil {
		return false
	}
	if ref, ok := s["$ref"].(string); ok {
		raw, ok := so.schemas[refName(ref)]
		if !ok {
			return false
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			return false
		}
	}
	t, _ := s["type"].(string)
	return t == "array"
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}

func run() error {
	raw, err := os.ReadFile(specPath)
	if err != nil {
		return err
	}
	out, err := generate(raw)
	if err != nil {
		return err
	}
	return os.WriteFile(catalogPath, out, 0o644)
}

func generate(raw []byte) ([]byte, error) {
	var s spec
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", specPath, err)
	}

	// Collection parents: every path whose last segment is a parameter points
	// at the collection it belongs to. A parameterless path in that set is a
	// collection (GET lists, and the list is cursor-paginated); any other
	// parameterless path is a singleton.
	parents := map[string]bool{}
	for path := range s.Paths {
		segments := strings.Split(strings.Trim(path, "/"), "/")
		if len(segments) > 0 && isParam(segments[len(segments)-1]) {
			parents["/"+strings.Join(segments[:len(segments)-1], "/")] = true
		}
	}

	so := &softener{schemas: s.Components.Schemas, params: s.Components.Parameters}
	var endpoints []endpoint
	for _, path := range sortedKeys(s.Paths) {
		if path == "/raw" || strings.HasPrefix(path, "/raw/") {
			continue
		}
		item := s.Paths[path]
		for _, op := range []struct {
			method string
			o      *operation
		}{
			{"GET", item.Get},
			{"POST", item.Post},
			{"PUT", item.Put},
			{"PATCH", item.Patch},
			{"DELETE", item.Delete},
		} {
			if op.o == nil {
				continue
			}
			ep, err := so.endpoint(op.method, path, item, op.o, parents)
			if err != nil {
				return nil, fmt.Errorf("%s %s: %w", op.method, path, err)
			}
			endpoints = append(endpoints, ep)
		}
	}
	sort.Slice(endpoints, func(i, j int) bool { return endpoints[i].Tool < endpoints[j].Tool })

	seen := map[string]bool{}
	for _, ep := range endpoints {
		if seen[ep.Tool] {
			return nil, fmt.Errorf("duplicate tool name %s", ep.Tool)
		}
		seen[ep.Tool] = true
	}
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("spec has no operations")
	}

	out, err := json.MarshalIndent(map[string]any{
		"uapi_version": s.Info.Version,
		"endpoints":    endpoints,
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

func (so *softener) endpoint(method, path string, item *pathItem, o *operation, parents map[string]bool) (endpoint, error) {
	params := append([]parameter{}, item.Parameters...)
	params = append(params, o.Parameters...)

	var pathParams []parameter
	var queryParams []parameter
	for _, p := range params {
		if p.Ref != "" {
			shared, ok := so.params[refName(p.Ref)]
			if !ok {
				return endpoint{}, fmt.Errorf("unknown parameter %q", p.Ref)
			}
			p = shared
		}
		switch {
		case p.In == "path":
			pathParams = append(pathParams, p)
		case p.In == "query" && !hiddenQueryParams[p.Name]:
			queryParams = append(queryParams, p)
		}
	}

	properties := map[string]any{}
	required := []string{}
	for _, p := range pathParams {
		properties[p.Name] = map[string]any{"type": "string", "description": p.Description}
		required = append(required, p.Name)
	}
	for _, p := range queryParams {
		s, err := so.soften(p.Schema, nil)
		if err != nil {
			return endpoint{}, fmt.Errorf("query param %s: %w", p.Name, err)
		}
		s.(map[string]any)["description"] = p.Description
		properties[p.Name] = s
	}

	bodyKind := ""
	if o.RequestBody != nil {
		content, ok := o.RequestBody.Content["application/json"]
		if !ok {
			return endpoint{}, fmt.Errorf("no application/json body")
		}
		body, err := so.requestBody(content.Schema)
		if err != nil {
			return endpoint{}, err
		}
		if len(pathParams) == 0 {
			// No path to distinguish the resource from its fields, so the
			// body fields are the input.
			bodyKind = "flat"
			for k, v := range body["properties"].(map[string]any) {
				properties[k] = v
			}
			required = append(required, stringSlice(body["required"])...)
		} else {
			bodyKind = "wrapped"
			properties["body"] = body
			required = append(required, "body")
		}
	}

	input := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": true,
	}
	if len(required) > 0 {
		input["required"] = required
	}

	segments := splitPath(path)
	paginated := method == "GET" && len(pathParams) == 0 && parents[path] && so.responseIsArray(o)
	verb := verbFor(method, segments, len(pathParams) > 0, parents[path])
	name, err := toolName(segments, pathParams, verb)
	if err != nil {
		return endpoint{}, err
	}

	description := o.Description
	if description == "" {
		description = o.Summary
	}
	if method == "PATCH" {
		// The spec description also documents the RFC 6902 content type this
		// tool cannot send, so say what the tool does.
		description += "\n\nThis tool sends the body as application/json (RFC 7396 merge-patch); it cannot send RFC 6902 JSON Patch."
	}
	return endpoint{
		Tool:        name,
		Summary:     fmt.Sprintf("%s\n\nHTTP %s %s", description, method, path),
		Method:      method,
		Path:        path,
		PathParams:  paramNames(pathParams),
		QueryParams: paramNames(queryParams),
		Paginated:   paginated,
		BodyKind:    bodyKind,
		Input:       input,
	}, nil
}

// requestBody resolves and softens a request-body schema. A Patch field takes
// the description and the deprecation that the spec leaves to the Request
// half before softening, so that the enum of the Patch field, null included,
// folds after the borrowed text instead of standing in for it.
func (so *softener) requestBody(ref json.RawMessage) (map[string]any, error) {
	var probe struct {
		Ref string `json:"$ref"`
	}
	_ = json.Unmarshal(ref, &probe)
	name := refName(probe.Ref)
	node, resolving := ref, []string(nil)
	if patchOf := strings.TrimSuffix(name, "Patch"); patchOf != name {
		if req, ok := so.schemas[patchOf+"Request"]; ok {
			merged, err := withRequestDocs(so.schemas[name], req)
			if err != nil {
				return nil, err
			}
			node, resolving = merged, []string{name}
		}
	}
	s, err := so.soften(node, resolving)
	if err != nil {
		return nil, err
	}
	body, ok := s.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("request body is not an object schema")
	}
	return body, nil
}

// withRequestDocs copies description and deprecated from each Request field
// to the Patch field of the same name that lacks them.
func withRequestDocs(patchRaw, reqRaw json.RawMessage) (json.RawMessage, error) {
	var patch, req map[string]any
	if err := json.Unmarshal(patchRaw, &patch); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(reqRaw, &req); err != nil {
		return nil, err
	}
	reqProps, _ := req["properties"].(map[string]any)
	props, _ := patch["properties"].(map[string]any)
	for field, p := range props {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		rm, ok := reqProps[field].(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"description", "deprecated"} {
			if _, has := pm[key]; !has {
				if v, ok := rm[key]; ok {
					pm[key] = v
				}
			}
		}
	}
	return json.Marshal(patch)
}

// soften rewrites one schema node into the permissive form the tool surface
// ships. Dropped keys are validation uapi may tighten or widen within a major;
// kept keys are documentation and structure.
func (so *softener) soften(node json.RawMessage, resolving []string) (any, error) {
	var v any
	if err := json.Unmarshal(node, &v); err != nil {
		return nil, err
	}
	return so.softenValue(v, resolving)
}

func (so *softener) softenValue(v any, resolving []string) (any, error) {
	switch val := v.(type) {
	case bool, float64, string, nil:
		return val, nil
	case []any:
		var out []any
		for _, item := range val {
			s, err := so.softenValue(item, resolving)
			if err != nil {
				return nil, err
			}
			out = append(out, s)
		}
		return out, nil
	case map[string]any:
		if ref, ok := val["$ref"].(string); ok {
			name := refName(ref)
			for _, r := range resolving {
				if r == name {
					return nil, fmt.Errorf("schema cycle at %s", name)
				}
			}
			raw, ok := so.schemas[name]
			if !ok {
				return nil, fmt.Errorf("unknown schema %q", ref)
			}
			return so.soften(raw, append(resolving, name))
		}
		out := map[string]any{}
		for k, sub := range val {
			switch k {
			case "allOf", "anyOf", "oneOf", "not", "if", "then", "else",
				"pattern", "minLength", "maxLength", "minimum", "maximum",
				"exclusiveMinimum", "exclusiveMaximum", "minItems", "maxItems",
				"multipleOf", "uniqueItems", "format", "default", "examples",
				"example", "title", "writeOnly", "readOnly", "const", "enum":
				continue
			}
			if strings.HasPrefix(k, "x-") {
				continue
			}
			s, err := so.softenValue(sub, resolving)
			if err != nil {
				return nil, err
			}
			out[k] = s
		}
		// Enum values are the only documentation some fields carry, so they
		// fold into the description instead of surviving as validation.
		if enum, ok := val["enum"].([]any); ok && len(enum) > 0 {
			parts := make([]string, len(enum))
			for i, e := range enum {
				switch e {
				case nil:
					parts[i] = "null"
				default:
					parts[i] = fmt.Sprintf("%v", e)
				}
			}
			fold := fmt.Sprintf("One of (may grow): %s.", strings.Join(parts, ", "))
			if d, ok := out["description"].(string); ok && d != "" {
				out["description"] = d + " " + fold
			} else {
				out["description"] = fold
			}
		}
		if isObject(val) {
			out["additionalProperties"] = true
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported schema value %T", v)
	}
}

func isObject(v map[string]any) bool {
	t, ok := v["type"].(string)
	if ok {
		return t == "object"
	}
	types, ok := v["type"].([]any)
	if ok {
		for _, ty := range types {
			if ty == "object" {
				return true
			}
		}
	}
	_, hasProps := v["properties"]
	return hasProps
}

// refName gives the component name of a local $ref, a schema or a parameter.
func refName(ref string) string {
	return ref[strings.LastIndex(ref, "/")+1:]
}

func verbFor(method string, segments []string, hasParams, isCollection bool) string {
	switch method {
	case "GET":
		if !hasParams && isCollection {
			return "list"
		}
		return "read"
	case "POST":
		if a := action(segments); a != "" {
			return a
		}
		return "create"
	case "PUT":
		return "replace"
	case "PATCH":
		return "merge"
	case "DELETE":
		return "delete"
	}
	return strings.ToLower(method)
}

// action returns the literal segment that ends a path after an item
// parameter, such as adopt in /firewall/rules/{id}/adopt or recover in
// /uapi/services/{name}/recover: a POST on it is that action, not a create.
func action(segments []string) string {
	n := len(segments)
	if n >= 2 && isParam(segments[n-2]) && !isParam(segments[n-1]) {
		return segments[n-1]
	}
	return ""
}

func toolName(segments []string, pathParams []parameter, verb string) (string, error) {
	// Param segments and a trailing action verb are not part of the
	// resource name.
	if verb == action(segments) {
		segments = segments[:len(segments)-1]
	}
	base := make([]string, 0, len(segments))
	for _, seg := range segments {
		if !isParam(seg) {
			base = append(base, seg)
		}
	}
	if len(base) == 0 {
		return "", fmt.Errorf("path has no segments")
	}

	var name string
	switch {
	case len(pathParams) == 0:
		name = strings.Join(base, "_")
	case len(pathParams) == 1 && (pathParams[0].Name == "id" || pathParams[0].Name == "name"):
		name = strings.Join(base[:len(base)-1], "_") + "_" + singular(base[len(base)-1])
		if len(base) == 1 {
			name = singular(base[0])
		}
	default:
		name = strings.Join(base, "_") + "_" + strings.Join(paramNames(pathParams), "_")
	}

	parts := make([]string, 0, 8)
	for _, seg := range strings.Split(name+"_"+verb, "_") {
		if seg != "" {
			parts = append(parts, seg)
		}
	}
	full := strings.Join(parts, "_")
	for _, seg := range parts {
		for _, r := range seg {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
				return "", fmt.Errorf("tool name %q has a non [a-z0-9_] segment", full)
			}
		}
	}
	return full, nil
}

// singular covers the plurals that occur in uapi's path segments: policies,
// accesses, leases6, com2secs and the regular ones.
func singular(s string) string {
	switch {
	case strings.HasSuffix(s, "ies") && len(s) > 3:
		return s[:len(s)-3] + "y"
	case strings.HasSuffix(s, "sses"), strings.HasSuffix(s, "ches"),
		strings.HasSuffix(s, "shes"), strings.HasSuffix(s, "xes"):
		return s[:len(s)-2]
	case strings.HasSuffix(s, "s") && !strings.HasSuffix(s, "ss"):
		return s[:len(s)-1]
	}
	return s
}

func splitPath(path string) []string {
	var out []string
	for _, seg := range strings.Split(path, "/") {
		if seg != "" {
			out = append(out, seg)
		}
	}
	// /openapi.json sanitizes to openapi_json
	for i, seg := range out {
		out[i] = sanitizeSegment(seg)
	}
	return out
}

func sanitizeSegment(seg string) string {
	if isParam(seg) {
		return seg
	}
	var b strings.Builder
	for _, r := range strings.ToLower(seg) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func isParam(seg string) bool {
	return strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}")
}

func paramNames(params []parameter) []string {
	var out []string
	for _, p := range params {
		out = append(out, p.Name)
	}
	return out
}

// stringSlice converts a JSON-decoded required list, which arrives as []any,
// to the []string the catalog carries.
func stringSlice(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
