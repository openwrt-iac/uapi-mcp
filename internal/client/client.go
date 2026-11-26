// Package client is a thin HTTP client for the uapi REST API, sharing the
// forward-compatibility discipline of the Terraform provider: responses are
// passed through as received, no client-side enum or schema validation, and
// control flow branches on HTTP status only.
package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mrand "math/rand"
	"net/http"
	"net/url"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Retry bounds, by wall clock, how long one request retries a retryable
// failure: the throttling statuses 423 (locked) and 429 (rate limited), and,
// on a POST, a transport error, where the idempotency key makes each replay
// safe. maxAttempts is a runaway backstop.
const (
	retryBudget = 45 * time.Second
	maxAttempts = 50
)

// Pagination bounds: uapi answers collections page by page through the
// X-Next-Cursor header. maxRestarts bounds how often one walk starts again
// from the first page after a stale cursor.
const (
	pageLimit   = 500
	maxPages    = 1000
	maxRestarts = 3
)

func newIdempotencyKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

type Client struct {
	baseURL   string
	token     string
	userAgent string
	http      *http.Client
}

// New builds a client for one router. endpoint carries the API version in
// its path, for example https://192.168.1.1/api/v4. insecure skips TLS
// verification, needed against uapi's default self-signed certificate.
func New(endpoint, token, userAgent string, insecure bool) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if insecure {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &Client{
		baseURL:   strings.TrimRight(endpoint, "/"),
		token:     token,
		userAgent: userAgent,
		http:      &http.Client{Timeout: 60 * time.Second, Transport: transport},
	}
}

// UserAgent is the canonical User-Agent for this binary.
func UserAgent(version string) string {
	if version == "" {
		version = "dev"
	}
	return fmt.Sprintf("uapi-mcp/%s (%s/%s)", version, runtime.GOOS, runtime.GOARCH)
}

// Response carries what the tool layer needs from one HTTP exchange: the raw
// payload plus the advisory headers uapi attaches to curated writes and to
// the wireguard kernel peer reads.
type Response struct {
	Status     int
	Body       []byte
	NextCursor string
	Advisory   http.Header
}

// advisory keeps every X-Reload-* and X-Kernel-* header and
// X-Mgmt-Path-Warning. They say what a transaction did beyond its commit, and
// a uapi 4.x minor can add one, which then reaches the agent with no release
// of uapi-mcp.
func advisory(h http.Header) http.Header {
	out := http.Header{}
	for name, values := range h {
		if strings.HasPrefix(name, "X-Reload-") || strings.HasPrefix(name, "X-Kernel-") || name == "X-Mgmt-Path-Warning" {
			out[name] = values
		}
	}
	return out
}

type FieldError struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Footer renders the advisory headers uapi attaches to a write, sorted by
// name, which say what the transaction did beyond committing uci: whether the
// init scripts reloaded and their daemons run, whether the kernel applied,
// and whether the caller's own management path moved. On a wireguard kernel
// peer read, X-Kernel-Unread names the interfaces the payload does not cover,
// so an empty list next to it is not a kernel without drift.
func (r *Response) Footer() string {
	lines := []string{}
	for _, name := range sortedNames(r.Advisory) {
		if value := strings.Join(r.Advisory.Values(name), ", "); value != "" {
			lines = append(lines, name+": "+value)
		}
	}
	return strings.Join(lines, "\n")
}

func sortedNames(h http.Header) []string {
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// APIError is uapi's error envelope, plus the endpoint that produced it. The
// tool layer surfaces it verbatim so the agent reads what uapi said.
type APIError struct {
	Method    string
	Path      string
	Status    int
	Code      string       `json:"code"`
	Message   string       `json:"message"`
	RequestID string       `json:"request_id"`
	Errors    []FieldError `json:"errors"`
	// Extra holds the other top-level fields, such as reload_error, the only
	// cause of a failed reload. uapi adds such fields within a major.
	Extra map[string]json.RawMessage `json:"-"`
}

func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s: uapi %d %s: %s", e.Method, e.Path, e.Status, e.Code, e.Message)
	for _, fe := range e.Errors {
		fmt.Fprintf(&b, "\n  - %s: %s (%s)", fe.Field, fe.Message, fe.Code)
	}
	names := make([]string, 0, len(e.Extra))
	for name := range e.Extra {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&b, "\n%s: %s", name, strings.ReplaceAll(extraText(e.Extra[name]), "\n", "\n  "))
	}
	if e.RequestID != "" {
		fmt.Fprintf(&b, "\nrequest_id: %s", e.RequestID)
	}
	return b.String()
}

// extraText renders a string as its text and any other value as compact JSON.
func extraText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var b bytes.Buffer
	_ = json.Compact(&b, raw)
	return b.String()
}

// IsNotFound reports whether err is an API 404.
func IsNotFound(err error) bool { return statusIs(err, http.StatusNotFound) }

func statusIs(err error, code int) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status == code
	}
	return false
}

// Do performs one request. body is the exact JSON to send, or nil. query
// carries the query parameters; Do adds the Idempotency-Key header of a POST.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, body []byte) (*Response, error) {
	idemKey := ""
	if method == http.MethodPost {
		idemKey = newIdempotencyKey()
	}
	target := c.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	var retryDeadline time.Time
	for attempt := 0; ; attempt++ {
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, target, reader)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", c.userAgent)
		if idemKey != "" {
			req.Header.Set("Idempotency-Key", idemKey)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.http.Do(req)
		if err != nil {
			if replayPost(ctx, method, err, &retryDeadline, attempt) {
				continue
			}
			return nil, fmt.Errorf("%s %s: %w", method, path, err)
		}
		raw, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			// A lost read of a POST response is the same failure class as a
			// transport error: uapi may have committed, and the replay
			// retrieves the cached answer.
			if replayPost(ctx, method, readErr, &retryDeadline, attempt) {
				continue
			}
			return nil, fmt.Errorf("%s %s: reading response body: %w", method, path, readErr)
		}

		if resp.StatusCode == http.StatusLocked || resp.StatusCode == http.StatusTooManyRequests {
			if retryDeadline.IsZero() {
				retryDeadline = time.Now().Add(retryBudget)
			}
			if attempt < maxAttempts && time.Now().Before(retryDeadline) {
				wait := retryWait(resp.Header.Get("Retry-After"), attempt)
				select {
				case <-ctx.Done():
					return nil, fmt.Errorf("%s %s: %w", method, path, ctx.Err())
				case <-time.After(wait):
				}
				continue
			}
		}

		out := &Response{
			Status:     resp.StatusCode,
			Body:       raw,
			NextCursor: resp.Header.Get("X-Next-Cursor"),
			Advisory:   advisory(resp.Header),
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return out, nil
		}
		return out, decodeError(method, path, resp.StatusCode, raw)
	}
}

// List walks a collection GET through its cursor pagination and returns
// every item as one JSON array in Body, with X-Kernel-Unread merged over
// every page, once per interface.
func (c *Client) List(ctx context.Context, path string, query url.Values) (*Response, error) {
	// An empty collection must marshal to [], not null: a nil slice would
	// render null and hand the agent a JSON null instead of an empty list.
	all := []json.RawMessage{}
	var unread []string
	seen := map[string]bool{}
	next := ""
	restarts := 0
	for page := 0; page < maxPages; page++ {
		q := query.Clone()
		if q == nil {
			q = url.Values{}
		}
		q.Set("limit", strconv.Itoa(pageLimit))
		if next != "" {
			q.Set("cursor", next)
		}
		resp, err := c.Do(ctx, http.MethodGet, path, q, nil)
		if err != nil {
			// uapi answers 400 to a cursor when the list changed under it, and
			// the items already read may then miss one or hold it twice.
			if next != "" && statusIs(err, http.StatusBadRequest) && restarts < maxRestarts {
				restarts++
				all, unread, seen, next = []json.RawMessage{}, nil, map[string]bool{}, ""
				continue
			}
			return nil, err
		}
		var items []json.RawMessage
		if err := json.Unmarshal(resp.Body, &items); err != nil {
			return nil, fmt.Errorf("GET %s: decoding page: %w", path, err)
		}
		all = append(all, items...)
		for _, name := range strings.Split(strings.Join(resp.Advisory.Values("X-Kernel-Unread"), ","), ",") {
			if name = strings.TrimSpace(name); name != "" && !seen[name] {
				seen[name] = true
				unread = append(unread, name)
			}
		}
		if resp.NextCursor == "" {
			body, err := json.Marshal(all)
			if err != nil {
				return nil, err
			}
			merged := http.Header{}
			if len(unread) > 0 {
				merged.Set("X-Kernel-Unread", strings.Join(unread, ","))
			}
			return &Response{Status: resp.Status, Body: body, Advisory: merged}, nil
		}
		next = resp.NextCursor
	}
	return nil, fmt.Errorf("GET %s: truncated at %d pages with more items left", path, maxPages)
}

// replayPost waits out the backoff and reports whether a failed POST attempt
// can be replayed. Only POST: its idempotency key makes the replay safe, and
// when uapi committed before the answer was lost, the replay retrieves the
// cached answer instead of double-applying. Other methods fail at once: a
// committed PUT or PATCH moves the ETag, so its guarded replay would answer
// 412. Returns false when the context ended or the shared retry budget ran
// out; the caller then reports the original failure.
func replayPost(ctx context.Context, method string, err error, deadline *time.Time, attempt int) bool {
	if method != http.MethodPost ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if deadline.IsZero() {
		*deadline = time.Now().Add(retryBudget)
	}
	if attempt >= maxAttempts || !time.Now().Before(*deadline) {
		return false
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(retryWait("", attempt)):
		return true
	}
}

// retryWait honors a server Retry-After when present; otherwise it falls back
// to exponential backoff with jitter, so concurrent clients retrying a 423
// or 429 do not synchronize into a thundering herd.
func retryWait(header string, attempt int) time.Duration {
	if header != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second
		}
	}
	const (
		base    = 200 * time.Millisecond
		maxWait = 5 * time.Second
	)
	// Cap the shift exponent: 200ms<<5 already exceeds maxWait, and a large
	// attempt would overflow int64 to a negative the cap below would not catch.
	backoff := maxWait
	if attempt < 5 {
		backoff = base << attempt
	}
	return backoff + time.Duration(mrand.Int63n(int64(base)))
}

func decodeError(method, path string, status int, body []byte) error {
	apiErr := &APIError{Method: method, Path: path, Status: status}
	if len(body) > 0 && json.Unmarshal(body, apiErr) == nil && apiErr.Code != "" {
		_ = json.Unmarshal(body, &apiErr.Extra)
		for name, raw := range apiErr.Extra {
			switch name {
			case "code", "message", "request_id", "errors":
				delete(apiErr.Extra, name)
			default:
				if string(raw) == "null" {
					delete(apiErr.Extra, name)
				}
			}
		}
		return apiErr
	}
	apiErr.Code = "unknown"
	apiErr.Message = strings.TrimSpace(string(body))
	if apiErr.Message == "" {
		apiErr.Message = http.StatusText(status)
	}
	return apiErr
}
