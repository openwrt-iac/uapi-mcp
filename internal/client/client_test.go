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

func TestPostSendsIdempotencyKey(t *testing.T) {
	var gotQuery, gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("idempotency_key")
		gotHeader = r.Header.Get("Idempotency-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r_1"}`))
	}))
	defer srv.Close()

	body := []byte(`{"target":"ACCEPT"}`)
	if _, err := testClient(srv.URL).Do(context.Background(), http.MethodPost, "/firewall/rules", nil, body); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gotQuery) != 32 {
		t.Errorf("idempotency_key query = %q, want 32 hex chars", gotQuery)
	}
	if gotHeader != gotQuery {
		t.Errorf("Idempotency-Key header = %q, query = %q", gotHeader, gotQuery)
	}
}

func TestPutSendsNoIdempotencyKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("idempotency_key") != "" {
			t.Errorf("unexpected idempotency_key on PUT")
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
		w.Header().Set("X-Reload-Status", "ok")
		w.Header().Set("X-Reload-Services", "network,odhcpd")
		w.Header().Set("X-Kernel-Status", "no_kernel")
		_, _ = w.Write([]byte(`{"id":"wan"}`))
	}))
	defer srv.Close()

	resp, err := testClient(srv.URL).Do(context.Background(), http.MethodPut, "/network/interfaces/wan", nil, []byte(`{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "X-Mgmt-Path-Warning: interface=wan changed=proto,ipaddrs\nX-Reload-Status: ok\nX-Reload-Services: network,odhcpd\nX-Kernel-Status: no_kernel"
	if resp.Footer() != want {
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

	raw, err := testClient(srv.URL).List(context.Background(), "/firewall/rules", url.Values{"managed": {"true"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatalf("decoding merged list: %v", err)
	}
	if len(items) != 3 || items[0]["id"] != "r_1" || items[2]["id"] != "r_3" {
		t.Errorf("merged list = %s", raw)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("expected 2 pages, got %d", got)
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
