package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/openwrt-iac/uapi-mcp/internal/client"
)

func call(t *testing.T, c *client.Client, ep Endpoint, arguments string) *mcp.CallToolResult {
	t.Helper()
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: []byte(arguments)}}
	res, err := handler(c, ep)(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned a protocol error: %v", err)
	}
	return res
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) != 1 {
		t.Fatalf("expected one content block, got %d", len(res.Content))
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", res.Content[0])
	}
	return text.Text
}

func readEp() Endpoint {
	return Endpoint{
		Tool:       "firewall_rule_read",
		Summary:    "Get one rules",
		Method:     http.MethodGet,
		Path:       "/firewall/rules/{id}",
		PathParams: []string{"id"},
		BodyKind:   bodyNone,
		Input:      map[string]any{"type": "object"},
	}
}

func TestHandlerReadSubstitutesPath(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"id":"r_1","target":"ACCEPT"}`)
	}))
	defer srv.Close()

	c := client.New(srv.URL, "tok", "uapi-mcp/test", false)
	res := call(t, c, readEp(), `{"id":"r_1"}`)
	if res.IsError {
		t.Fatal("unexpected error result")
	}
	if gotPath != "/firewall/rules/r_1" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if text := resultText(t, res); text != `{"id":"r_1","target":"ACCEPT"}` {
		t.Errorf("text = %q", text)
	}
}

func TestHandlerReadMissingPathParam(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should reach the server")
	}))
	defer srv.Close()

	c := client.New(srv.URL, "tok", "uapi-mcp/test", false)
	res := call(t, c, readEp(), `{}`)
	if !res.IsError {
		t.Fatal("expected an error result")
	}
	if text := resultText(t, res); !strings.Contains(text, "missing required string argument id") {
		t.Errorf("text = %q", text)
	}
}

func TestHandlerListPaginates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("managed") != "true" {
			t.Errorf("managed = %q", r.URL.Query().Get("managed"))
		}
		if r.URL.Query().Get("cursor") == "" {
			w.Header().Set("X-Next-Cursor", "c_2")
			_, _ = io.WriteString(w, `[{"id":"r_1"},{"id":"r_2"}]`)
			return
		}
		_, _ = io.WriteString(w, `[{"id":"r_3"}]`)
	}))
	defer srv.Close()

	ep := Endpoint{
		Tool:        "firewall_rules_list",
		Method:      http.MethodGet,
		Path:        "/firewall/rules",
		QueryParams: []string{"managed"},
		Paginated:   true,
		BodyKind:    bodyNone,
	}
	c := client.New(srv.URL, "tok", "uapi-mcp/test", false)
	res := call(t, c, ep, `{"managed":"true"}`)
	if res.IsError {
		t.Fatal("unexpected error result")
	}
	var items []map[string]any
	if err := json.Unmarshal([]byte(resultText(t, res)), &items); err != nil {
		t.Fatalf("merged list is not JSON: %v", err)
	}
	if len(items) != 3 {
		t.Errorf("merged list holds %d items", len(items))
	}
}

func TestHandlerCreateSendsFlatBody(t *testing.T) {
	var gotMethod string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = io.WriteString(w, `{"id":"r_1"}`)
	}))
	defer srv.Close()

	ep := Endpoint{
		Tool:     "firewall_rules_create",
		Method:   http.MethodPost,
		Path:     "/firewall/rules",
		BodyKind: bodyFlat,
	}
	c := client.New(srv.URL, "tok", "uapi-mcp/test", false)
	res := call(t, c, ep, `{"target":"ACCEPT","name":"allow web"}`)
	if res.IsError {
		t.Fatal("unexpected error result")
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q", gotMethod)
	}
	if gotBody["target"] != "ACCEPT" || gotBody["name"] != "allow web" {
		t.Errorf("body = %v", gotBody)
	}
}

func TestHandlerReplaceSendsWrappedBody(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("X-Reload-Status", "ok")
		w.Header().Set("X-Reload-Services", "firewall")
		_, _ = io.WriteString(w, `{"id":"r_1","target":"DROP"}`)
	}))
	defer srv.Close()

	ep := Endpoint{
		Tool:       "firewall_rule_replace",
		Method:     http.MethodPut,
		Path:       "/firewall/rules/{id}",
		PathParams: []string{"id"},
		BodyKind:   bodyWrapped,
	}
	c := client.New(srv.URL, "tok", "uapi-mcp/test", false)
	res := call(t, c, ep, `{"id":"r_1","body":{"target":"DROP"}}`)
	if res.IsError {
		t.Fatal("unexpected error result")
	}
	if gotMethod != http.MethodPut || gotPath != "/firewall/rules/r_1" {
		t.Errorf("%s %s", gotMethod, gotPath)
	}
	if gotBody["target"] != "DROP" {
		t.Errorf("body = %v", gotBody)
	}
	want := `{"id":"r_1","target":"DROP"}` + "\n\nX-Reload-Status: ok\nX-Reload-Services: firewall"
	if text := resultText(t, res); text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
}

func TestHandlerFlatBodyRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should reach the server")
	}))
	defer srv.Close()

	// A singleton merge: the fields are the input, not a body object.
	ep := Endpoint{
		Tool:     "system_merge",
		Method:   http.MethodPatch,
		Path:     "/system",
		BodyKind: bodyFlat,
	}
	c := client.New(srv.URL, "tok", "uapi-mcp/test", false)
	res := call(t, c, ep, `{"body":{"hostname":"router"}}`)
	if !res.IsError {
		t.Fatal("expected an error result: a dropped body object would be a silent no-op")
	}
	if text := resultText(t, res); !strings.Contains(text, "not a body object") {
		t.Errorf("text = %q", text)
	}
}

func TestHandlerWrappedBodyRequired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should reach the server")
	}))
	defer srv.Close()

	ep := Endpoint{
		Tool:       "firewall_rule_replace",
		Method:     http.MethodPut,
		Path:       "/firewall/rules/{id}",
		PathParams: []string{"id"},
		BodyKind:   bodyWrapped,
	}
	c := client.New(srv.URL, "tok", "uapi-mcp/test", false)
	res := call(t, c, ep, `{"id":"r_1"}`)
	if !res.IsError {
		t.Fatal("expected an error result")
	}
	if text := resultText(t, res); !strings.Contains(text, "missing required argument body") {
		t.Errorf("text = %q", text)
	}
}

func TestHandlerDeleteNotFoundIsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"code":"not_found","message":"no such section"}`)
	}))
	defer srv.Close()

	ep := Endpoint{
		Tool:       "firewall_rule_delete",
		Method:     http.MethodDelete,
		Path:       "/firewall/rules/{id}",
		PathParams: []string{"id"},
		BodyKind:   bodyNone,
	}
	c := client.New(srv.URL, "tok", "uapi-mcp/test", false)
	res := call(t, c, ep, `{"id":"r_gone"}`)
	if res.IsError {
		t.Fatal("a 404 on delete is success, not an error")
	}
	if text := resultText(t, res); !strings.Contains(text, "already absent") {
		t.Errorf("text = %q", text)
	}
}

func TestHandlerDelete204ReportsStatus(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	ep := Endpoint{
		Tool:       "firewall_rule_delete",
		Method:     http.MethodDelete,
		Path:       "/firewall/rules/{id}",
		PathParams: []string{"id"},
		BodyKind:   bodyNone,
	}
	c := client.New(srv.URL, "tok", "uapi-mcp/test", false)
	res := call(t, c, ep, `{"id":"r_1"}`)
	if res.IsError {
		t.Fatal("unexpected error result")
	}
	if text := resultText(t, res); text != "DELETE /firewall/rules/r_1: 204 No Content" {
		t.Errorf("text = %q", text)
	}
}

func TestHandlerSurfacesErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"code":"validation_failed","message":"bad","request_id":"01HXT","errors":[{"field":"target","code":"required","message":"is required"}]}`)
	}))
	defer srv.Close()

	ep := Endpoint{
		Tool:       "firewall_rule_replace",
		Method:     http.MethodPut,
		Path:       "/firewall/rules/{id}",
		PathParams: []string{"id"},
		BodyKind:   bodyWrapped,
	}
	c := client.New(srv.URL, "tok", "uapi-mcp/test", false)
	res := call(t, c, ep, `{"id":"r_1","body":{}}`)
	if !res.IsError {
		t.Fatal("expected an error result")
	}
	text := resultText(t, res)
	for _, want := range []string{
		"PUT /firewall/rules/r_1",
		"uapi 422 validation_failed",
		"- target: is required (required)",
		"request_id: 01HXT",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text %q does not contain %q", text, want)
		}
	}
}

func TestAnnotations(t *testing.T) {
	for method, want := range map[string][4]bool{
		http.MethodGet:    {true, true, false, false},
		http.MethodPost:   {false, false, false, false},
		http.MethodPut:    {false, true, true, false},
		http.MethodPatch:  {false, true, false, false},
		http.MethodDelete: {false, true, true, false},
	} {
		a := annotationsFor(method)
		if a.ReadOnlyHint != want[0] || a.IdempotentHint != want[1] ||
			*a.DestructiveHint != want[2] || *a.OpenWorldHint != want[3] {
			t.Errorf("%s: got readOnly=%v idempotent=%v destructive=%v openWorld=%v",
				method, a.ReadOnlyHint, a.IdempotentHint, *a.DestructiveHint, *a.OpenWorldHint)
		}
	}
}

func TestEmbeddedCatalog(t *testing.T) {
	var catalog struct {
		UAPIVersion string     `json:"uapi_version"`
		Endpoints   []Endpoint `json:"endpoints"`
	}
	if err := json.Unmarshal(catalogJSON, &catalog); err != nil {
		t.Fatal(err)
	}
	if catalog.UAPIVersion != "3.3.0" {
		t.Errorf("uapi_version = %q", catalog.UAPIVersion)
	}
	if len(catalog.Endpoints) != 266 {
		t.Errorf("endpoints = %d, want 266", len(catalog.Endpoints))
	}
	seen := map[string]bool{}
	for _, ep := range catalog.Endpoints {
		if seen[ep.Tool] {
			t.Errorf("duplicate tool name %s", ep.Tool)
		}
		seen[ep.Tool] = true
		if ep.Input["additionalProperties"] != true {
			t.Errorf("%s: input is not permissive", ep.Tool)
		}
		switch ep.BodyKind {
		case bodyNone, bodyFlat, bodyWrapped:
		default:
			t.Errorf("%s: unknown body kind %q", ep.Tool, ep.BodyKind)
		}
	}
	for _, name := range []string{"network_interfaces_list", "network_rule_replace", "firewall_rule_delete"} {
		if !seen[name] {
			t.Errorf("expected tool %s in the embedded catalog", name)
		}
	}
}
