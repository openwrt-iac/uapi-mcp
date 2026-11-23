package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/openwrt-iac/uapi-mcp/internal/client"
)

// The tests in this file look at the server the way an MCP host does: an SDK
// client over an in-memory transport, asserting the results the wire carries.
// The unit tests in internal/tools assert handler returns; these assert what
// a host receives, which is where the footer-glued-to-payload defect hid.

// stubRouter answers the endpoints the tests call, the way uapi does.
func stubRouter(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/firewall/rules":
			_, _ = io.WriteString(w, `[{"id":"r_1","target":"ACCEPT"}]`)
		case r.Method == http.MethodGet && r.URL.Path == "/firewall/rules/r_1":
			_, _ = io.WriteString(w, `{"id":"r_1","target":"ACCEPT"}`)
		case r.Method == http.MethodPut && r.URL.Path == "/firewall/rules/r_1":
			w.Header().Set("X-Reload-Status", "ok")
			w.Header().Set("X-Reload-Services", "firewall")
			_, _ = io.WriteString(w, `{"id":"r_1","target":"DROP"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/firewall/rules":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"code":"validation_failed","message":"bad","request_id":"01HXT","errors":[{"field":"target","code":"required","message":"is required"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/network/wireguard_kernel_peers":
			_, _ = io.WriteString(w, `[]`)
		case r.Method == http.MethodDelete && r.URL.Path == "/dhcp/hosts/h_1":
			w.Header().Set("X-Reload-Status", "stopped")
			w.Header().Set("X-Reload-Services", "dnsmasq")
			w.Header().Set("X-Reload-Stopped", "dnsmasq/cfg01411c")
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func hostSession(t *testing.T) *mcp.ClientSession {
	t.Helper()
	return hostSessionTo(t, stubRouter(t).URL)
}

func hostSessionTo(t *testing.T, routerURL string) *mcp.ClientSession {
	t.Helper()
	c := client.New(routerURL, "tok", "uapi-mcp/test", false)
	s, err := New("test", c)
	if err != nil {
		t.Fatalf("building server: %v", err)
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := s.Connect(context.Background(), serverTransport, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	host := mcp.NewClient(&mcp.Implementation{Name: "host-eye", Version: "0"}, nil)
	session, err := host.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func call(t *testing.T, session *mcp.ClientSession, name string, arguments map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", name, err)
	}
	return res
}

func textBlocks(t *testing.T, res *mcp.CallToolResult) []string {
	t.Helper()
	var out []string
	for _, c := range res.Content {
		text, ok := c.(*mcp.TextContent)
		if !ok {
			t.Fatalf("expected *TextContent, got %T", c)
		}
		out = append(out, text.Text)
	}
	return out
}

func TestHostSeesTheWholeCatalog(t *testing.T) {
	session := hostSession(t)
	listed, err := session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(listed.Tools) != 258 {
		t.Errorf("host sees %d tools, want 258", len(listed.Tools))
	}
	byName := map[string]*mcp.Tool{}
	for _, tool := range listed.Tools {
		byName[tool.Name] = tool
	}
	if got := byName["firewall_rule_delete"]; got == nil {
		t.Error("firewall_rule_delete missing from the catalog")
	} else if got.Annotations == nil || got.Annotations.DestructiveHint == nil || !*got.Annotations.DestructiveHint {
		t.Errorf("firewall_rule_delete annotations = %+v", got.Annotations)
	}
	replace := byName["firewall_rule_replace"]
	if replace == nil {
		t.Fatal("firewall_rule_replace missing from the catalog")
	}
	schema, ok := replace.InputSchema.(map[string]any)
	if !ok {
		t.Fatalf("input schema type %T", replace.InputSchema)
	}
	// Over the wire the required list arrives as a JSON array, not []string.
	if req, ok := schema["required"].([]any); !ok || len(req) != 2 || req[0] != "id" || req[1] != "body" {
		t.Errorf("firewall_rule_replace required = %#v, want [id body]", schema["required"])
	}
}

func TestHostReadsAPayload(t *testing.T) {
	session := hostSession(t)
	res := call(t, session, "firewall_rule_read", map[string]any{"id": "r_1"})
	if res.IsError {
		t.Fatal("unexpected error result")
	}
	blocks := textBlocks(t, res)
	if len(blocks) != 1 {
		t.Fatalf("read carries %d blocks, want 1", len(blocks))
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(blocks[0]), &payload); err != nil {
		t.Fatalf("read payload is not JSON: %v (text %q)", err, blocks[0])
	}
	if payload["id"] != "r_1" {
		t.Errorf("payload = %v", payload)
	}
}

func TestHostWritesPayloadThenFooter(t *testing.T) {
	session := hostSession(t)
	res := call(t, session, "firewall_rule_replace", map[string]any{
		"id":   "r_1",
		"body": map[string]any{"target": "DROP"},
	})
	if res.IsError {
		t.Fatal("unexpected error result")
	}
	blocks := textBlocks(t, res)
	if len(blocks) != 2 {
		t.Fatalf("write with transaction headers carries %d blocks, want payload then footer", len(blocks))
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(blocks[0]), &payload); err != nil {
		t.Fatalf("write payload is not JSON: %v (text %q)", err, blocks[0])
	}
	if payload["target"] != "DROP" {
		t.Errorf("payload = %v", payload)
	}
	if !strings.Contains(blocks[1], "X-Reload-Status: ok") {
		t.Errorf("footer = %q", blocks[1])
	}
}

func TestHostSeesTheErrorEnvelope(t *testing.T) {
	session := hostSession(t)
	res := call(t, session, "firewall_rules_create", map[string]any{"name": "bogus"})
	if !res.IsError {
		t.Fatal("expected an error result")
	}
	blocks := textBlocks(t, res)
	if len(blocks) != 1 {
		t.Fatalf("error carries %d blocks, want 1", len(blocks))
	}
	text := blocks[0]
	for _, want := range []string{
		"POST /firewall/rules",
		"uapi 422 validation_failed",
		"- target: is required (required)",
		"request_id: 01HXT",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("error text %q does not contain %q", text, want)
		}
	}
}

func TestHostSeesAnEmptyListAsAnArray(t *testing.T) {
	session := hostSession(t)
	res := call(t, session, "network_wireguard_kernel_peers_list", map[string]any{})
	if res.IsError {
		t.Fatal("unexpected error result")
	}
	blocks := textBlocks(t, res)
	if len(blocks) != 1 {
		t.Fatalf("empty list carries %d blocks, want 1", len(blocks))
	}
	var items []any
	if err := json.Unmarshal([]byte(blocks[0]), &items); err != nil {
		t.Fatalf("empty list is not a JSON array: %v (text %q)", err, blocks[0])
	}
	if len(items) != 0 {
		t.Errorf("empty list = %v", items)
	}
}

// The case uapi 3.5.0 added X-Kernel-Unread for: no peer listed because the
// kernel of wg0 could not be read. Without the footer the host sees [] and
// reads a kernel without drift.
func TestHostSeesUnreadKernelBesideAnEmptyList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/network/wireguard_kernel_peers" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Kernel-Unread", "wg0")
		_, _ = io.WriteString(w, `[]`)
	}))
	t.Cleanup(srv.Close)

	res := call(t, hostSessionTo(t, srv.URL), "network_wireguard_kernel_peers_list", map[string]any{})
	if res.IsError {
		t.Fatal("unexpected error result")
	}
	blocks := textBlocks(t, res)
	if len(blocks) != 2 {
		t.Fatalf("list with X-Kernel-Unread carries %d blocks, want payload then footer", len(blocks))
	}
	var items []any
	if err := json.Unmarshal([]byte(blocks[0]), &items); err != nil || len(items) != 0 {
		t.Fatalf("payload = %q, want []", blocks[0])
	}
	if blocks[1] != "X-Kernel-Unread: wg0" {
		t.Errorf("footer = %q", blocks[1])
	}
}

// A DELETE answers 204 with no body, and its reload headers say whether the
// daemon still runs. The host must get them as the footer block.
func TestHostSeesTheFooterOfADelete(t *testing.T) {
	res := call(t, hostSession(t), "dhcp_host_delete", map[string]any{"id": "h_1"})
	if res.IsError {
		t.Fatal("unexpected error result")
	}
	blocks := textBlocks(t, res)
	if len(blocks) != 2 {
		t.Fatalf("delete with reload headers carries %d blocks, want status then footer", len(blocks))
	}
	if blocks[0] != "DELETE /dhcp/hosts/h_1: 204 No Content" {
		t.Errorf("status block = %q", blocks[0])
	}
	if !strings.Contains(blocks[1], "X-Reload-Stopped: dnsmasq/cfg01411c") {
		t.Errorf("footer = %q", blocks[1])
	}
}

// The instructions send the agent to tools by name. Each name must exist.
func TestInstructionsNameRealTools(t *testing.T) {
	session := hostSession(t)
	listed, err := session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := map[string]bool{}
	for _, tool := range listed.Tools {
		names[tool.Name] = true
	}
	for _, word := range strings.FieldsFunc(instructions, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_'
	}) {
		if strings.Count(word, "_") >= 2 && !names[word] {
			t.Errorf("instructions name %s, which is not a tool", word)
		}
	}
	if !strings.Contains(session.InitializeResult().Instructions, "uapi_service_recover") {
		t.Error("the host does not get the instructions")
	}
}
