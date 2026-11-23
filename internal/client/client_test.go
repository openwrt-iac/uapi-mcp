package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func testClient(serverURL string) *Client {
	return New(serverURL, "tok", "uapi-mcp/test", false)
}

func TestSendsAuthHeaders(t *testing.T) {
	var gotUA, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r_1"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "tok", UserAgent("test"), false)
	_, err := c.Do(context.Background(), http.MethodGet, "/firewall/rules/r_1", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(gotUA, "uapi-mcp/test ") {
		t.Errorf("User-Agent = %q, want uapi-mcp/test prefix", gotUA)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q", gotAuth)
	}
}

// uapi 4.x reads the Idempotency-Key header, so the key no longer rides in
// the query as well.
func TestPostSendsIdempotencyKey(t *testing.T) {
	var gotQuery, gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		gotHeader = r.Header.Get("Idempotency-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r_1"}`))
	}))
	defer srv.Close()

	body := []byte(`{"target":"ACCEPT"}`)
	if _, err := testClient(srv.URL).Do(context.Background(), http.MethodPost, "/firewall/rules", nil, body); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gotHeader) != 32 {
		t.Errorf("Idempotency-Key header = %q, want 32 hex chars", gotHeader)
	}
	if gotQuery != "" {
		t.Errorf("query = %q, want none", gotQuery)
	}
}

func TestPutSendsNoIdempotencyKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Idempotency-Key") != "" {
			t.Errorf("unexpected Idempotency-Key on PUT")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r_1"}`))
	}))
	defer srv.Close()

	_, err := testClient(srv.URL).Do(context.Background(), http.MethodPut, "/firewall/rules/r_1", nil, []byte(`{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRetriesLocked(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusLocked)
			_, _ = w.Write([]byte(`{"code":"locked","message":"busy"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"r_1"}`))
	}))
	defer srv.Close()

	resp, err := testClient(srv.URL).Do(context.Background(), http.MethodPut, "/firewall/rules/r_1", nil, []byte(`{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d", resp.Status)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("expected 3 calls (2 locked + 1 ok), got %d", got)
	}
}

func TestRetriesRateLimited(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 2 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"code":"too_many_requests","message":"slow down"}`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	_, err := testClient(srv.URL).Do(context.Background(), http.MethodGet, "/firewall/rules", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("expected 2 calls, got %d", got)
	}
}

func TestLockedExhaustsRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusLocked)
		_, _ = w.Write([]byte(`{"code":"locked","message":"busy"}`))
	}))
	defer srv.Close()

	_, err := testClient(srv.URL).Do(context.Background(), http.MethodPut, "/firewall/rules/r_1", nil, []byte(`{}`))
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.Status != http.StatusLocked || apiErr.Code != "locked" {
		t.Errorf("expected locked APIError, got %v", err)
	}
}

func TestValidationErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"code":"validation_failed","message":"bad","request_id":"01HXTEST","errors":[{"field":"target","code":"required","message":"is required"}]}`))
	}))
	defer srv.Close()

	_, err := testClient(srv.URL).Do(context.Background(), http.MethodPost, "/firewall/rules", nil, []byte(`{}`))
	if err == nil {
		t.Fatal("expected error")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if apiErr.Code != "validation_failed" || apiErr.RequestID != "01HXTEST" {
		t.Errorf("envelope not parsed: %+v", apiErr)
	}
	if len(apiErr.Errors) != 1 || apiErr.Errors[0].Field != "target" {
		t.Errorf("field errors not parsed: %+v", apiErr.Errors)
	}
	want := "POST /firewall/rules: uapi 422 validation_failed: bad\n  - target: is required (required)\nrequest_id: 01HXTEST"
	if apiErr.Error() != want {
		t.Errorf("Error() =\n%q\nwant\n%q", apiErr.Error(), want)
	}
}

func TestErrorFallbackOnNonJSONBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("gateway exploded"))
	}))
	defer srv.Close()

	_, err := testClient(srv.URL).Do(context.Background(), http.MethodGet, "/healthz", nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.Code != "unknown" || apiErr.Message != "gateway exploded" {
		t.Errorf("expected fallback envelope, got %v", err)
	}
}

func TestWriteHeadersCarriedOnResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Mgmt-Path-Warning", "interface=wan changed=proto,ipaddrs")
		w.Header().Set("X-Reload-Status", "stopped")
		w.Header().Set("X-Reload-Services", "dnsmasq,odhcpd")
		w.Header().Set("X-Reload-Stopped", "dnsmasq/cfg01411c")
		w.Header().Set("X-Kernel-Status", "blocked")
		w.Header().Set("X-Kernel-Blocked", "wg0/peer1")
		w.Header().Set("X-Request-Id", "01hx0000000000000000000000")
		w.Header().Set("ETag", `"abc"`)
		_, _ = w.Write([]byte(`{"id":"wan"}`))
	}))
	defer srv.Close()

	resp, err := testClient(srv.URL).Do(context.Background(), http.MethodPut, "/network/interfaces/wan", nil, []byte(`{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "X-Kernel-Blocked: wg0/peer1\nX-Kernel-Status: blocked\nX-Mgmt-Path-Warning: interface=wan changed=proto,ipaddrs\n" +
		"X-Reload-Services: dnsmasq,odhcpd\nX-Reload-Status: stopped\nX-Reload-Stopped: dnsmasq/cfg01411c"
	if resp.Footer() != want {
		t.Errorf("Footer() = %q, want %q", resp.Footer(), want)
	}
}

// A header that a uapi 4.x minor adds under one of the advisory prefixes
// reaches the footer with no uapi-mcp release.
func TestFooterPassesNewAdvisoryHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Reload-Future", "a")
		w.Header().Set("X-Kernel-Future", "b")
		w.Header().Set("X-Future", "c")
		_, _ = w.Write([]byte(`{"id":"wan"}`))
	}))
	defer srv.Close()

	resp, err := testClient(srv.URL).Do(context.Background(), http.MethodPut, "/network/interfaces/wan", nil, []byte(`{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "X-Kernel-Future: b\nX-Reload-Future: a"; resp.Footer() != want {
		t.Errorf("Footer() = %q, want %q", resp.Footer(), want)
	}
}

func TestListFollowsCursor(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.URL.Query().Get("limit") != "500" {
			t.Errorf("limit = %q, want 500", r.URL.Query().Get("limit"))
		}
		if r.URL.Query().Get("cursor") == "" {
			w.Header().Set("X-Next-Cursor", "c_2")
			_, _ = w.Write([]byte(`[{"id":"r_1"},{"id":"r_2"}]`))
			return
		}
		_, _ = w.Write([]byte(`[{"id":"r_3"}]`))
	}))
	defer srv.Close()

	resp, err := testClient(srv.URL).List(context.Background(), "/firewall/rules", url.Values{"managed": {"true"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var items []map[string]any
	if err := json.Unmarshal(resp.Body, &items); err != nil {
		t.Fatalf("decoding merged list: %v", err)
	}
	if len(items) != 3 || items[0]["id"] != "r_1" || items[2]["id"] != "r_3" {
		t.Errorf("merged list = %s", resp.Body)
	}
	if resp.Footer() != "" {
		t.Errorf("Footer() = %q, want none", resp.Footer())
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("expected 2 pages, got %d", got)
	}
}

func TestListEmptyCollection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	resp, err := testClient(srv.URL).List(context.Background(), "/network/wireguard_kernel_peers", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(resp.Body) != "[]" {
		t.Errorf("empty collection = %q, want []", resp.Body)
	}
}

func TestListMergesKernelUnread(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") == "" {
			w.Header().Set("X-Kernel-Unread", "wg0")
			w.Header().Set("X-Next-Cursor", "c_2")
			_, _ = w.Write([]byte(`[{"id":"p_1"}]`))
			return
		}
		w.Header().Set("X-Kernel-Unread", "wg0, wg1")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	resp, err := testClient(srv.URL).List(context.Background(), "/network/wireguard_kernel_peers", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Footer() != "X-Kernel-Unread: wg0,wg1" {
		t.Errorf("Footer() = %q", resp.Footer())
	}
}

// uapi answers 400 invalid_cursor when the list changed under a cursor. The
// walk reads the list again from the first page and returns that reading
// only, with no item of the stale walk.
func TestListRestartsAfterStaleCursor(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		switch {
		case r.URL.Query().Get("cursor") == "" && n == 1:
			w.Header().Set("X-Next-Cursor", "c_old")
			_, _ = w.Write([]byte(`[{"id":"stale"}]`))
		case r.URL.Query().Get("cursor") == "c_old":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":"invalid_cursor","message":"the list changed"}`))
		case r.URL.Query().Get("cursor") == "":
			w.Header().Set("X-Next-Cursor", "c_new")
			_, _ = w.Write([]byte(`[{"id":"r_1"}]`))
		default:
			_, _ = w.Write([]byte(`[{"id":"r_2"}]`))
		}
	}))
	defer srv.Close()

	resp, err := testClient(srv.URL).List(context.Background(), "/dhcp/hosts", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(resp.Body) != `[{"id":"r_1"},{"id":"r_2"}]` {
		t.Errorf("list = %s, want the second walk only", resp.Body)
	}
}

func TestListStopsAfterRestarts(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.URL.Query().Get("cursor") == "" {
			w.Header().Set("X-Next-Cursor", "c_1")
			_, _ = w.Write([]byte(`[{"id":"r_1"}]`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"invalid_cursor","message":"the list changed"}`))
	}))
	defer srv.Close()

	_, err := testClient(srv.URL).List(context.Background(), "/dhcp/hosts", nil)
	if !statusIs(err, http.StatusBadRequest) {
		t.Fatalf("err = %v, want the last 400", err)
	}
	if got, want := atomic.LoadInt32(&calls), int32(2*(maxRestarts+1)); got != want {
		t.Errorf("requests = %d, want %d", got, want)
	}
}

// A 400 on the first page is the request itself, such as a managed value
// outside its set: reading again would answer the same.
func TestListDoesNotRestartOnFirstPage(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"bad_request","message":"managed"}`))
	}))
	defer srv.Close()

	_, err := testClient(srv.URL).List(context.Background(), "/dhcp/hosts", url.Values{"managed": {"yes"}})
	if !statusIs(err, http.StatusBadRequest) {
		t.Fatalf("err = %v, want 400", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("requests = %d, want 1", got)
	}
}

func TestQueryParamsForwarded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("managed") != "true" {
			t.Errorf("managed = %q", r.URL.Query().Get("managed"))
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	if _, err := testClient(srv.URL).List(context.Background(), "/dhcp/hosts", url.Values{"managed": {"true"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestIsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"not_found","message":"nope"}`))
	}))
	defer srv.Close()

	_, err := testClient(srv.URL).Do(context.Background(), http.MethodGet, "/firewall/rules/r_x", nil, nil)
	if !IsNotFound(err) {
		t.Errorf("expected IsNotFound, got %v", err)
	}
}
