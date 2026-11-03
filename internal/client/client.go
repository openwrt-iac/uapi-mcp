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
// X-Next-Cursor header.
const (
	pageLimit = 500
	maxPages  = 1000
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
// its path, for example https://192.168.1.1/api/v3. insecure skips TLS
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
// payload plus the advisory headers uapi attaches to curated writes.
type Response struct {
	Status          int
	Body            []byte
	NextCursor      string
	MgmtPathWarning string
	ReloadStatus    string
	ReloadServices  string
	KernelStatus    string
	KernelApplied   string
}

type FieldError struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Footer renders the advisory headers uapi attaches to a write, which say
// what the transaction did beyond committing uci: whether the init scripts
// reloaded, whether the kernel applied, and whether the caller's own
// management path moved.
func (r *Response) Footer() string {
	var lines []string
	for _, kv := range [][2]string{
		{"X-Mgmt-Path-Warning", r.MgmtPathWarning},
		{"X-Reload-Status", r.ReloadStatus},
		{"X-Reload-Services", r.ReloadServices},
		{"X-Kernel-Status", r.KernelStatus},
		{"X-Kernel-Applied", r.KernelApplied},
	} {
		if kv[1] != "" {
			lines = append(lines, kv[0]+": "+kv[1])
		}
	}
	return strings.Join(lines, "\n")
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
}

func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s: uapi %d %s: %s", e.Method, e.Path, e.Status, e.Code, e.Message)
	for _, fe := range e.Errors {
		fmt.Fprintf(&b, "\n  - %s: %s (%s)", fe.Field, fe.Message, fe.Code)
	}
	if e.RequestID != "" {
		fmt.Fprintf(&b, "\nrequest_id: %s", e.RequestID)
	}
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
// carries user-facing filter parameters; transport mechanics (idempotency
// keys, pagination cursors) are added here.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, body []byte) (*Response, error) {
	idemKey := ""
	if method == http.MethodPost {
		idemKey = newIdempotencyKey()
	}
	q := query.Clone()
	if q == nil {
		q = url.Values{}
	}
	if idemKey != "" {
		// Sent as a query parameter because uhttpd's CGI layer forwards a
		// fixed header allowlist the Idempotency-Key header is not on, so
		// the header form alone never reaches uapi. The header rides along
		// for deployments that do not front uapi with uhttpd.
		q.Set("idempotency_key", idemKey)
	}
	target := c.baseURL + path
	if len(q) > 0 {
		target += "?" + q.Encode()
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
			Status:          resp.StatusCode,
			Body:            raw,
			NextCursor:      resp.Header.Get("X-Next-Cursor"),
			MgmtPathWarning: resp.Header.Get("X-Mgmt-Path-Warning"),
			ReloadStatus:    resp.Header.Get("X-Reload-Status"),
			ReloadServices:  resp.Header.Get("X-Reload-Services"),
			KernelStatus:    resp.Header.Get("X-Kernel-Status"),
			KernelApplied:   resp.Header.Get("X-Kernel-Applied"),
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return out, nil
		}
		return out, decodeError(method, path, resp.StatusCode, raw)
	}
}

// List walks a collection GET through its cursor pagination and returns
// every item as one JSON array.
func (c *Client) List(ctx context.Context, path string, query url.Values) ([]byte, error) {
	var all []json.RawMessage
	next := ""
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
			return nil, err
		}
		var items []json.RawMessage
		if err := json.Unmarshal(resp.Body, &items); err != nil {
			return nil, fmt.Errorf("GET %s: decoding page: %w", path, err)
		}
		all = append(all, items...)
		if resp.NextCursor == "" {
			return json.Marshal(all)
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
		return apiErr
	}
	apiErr.Code = "unknown"
	apiErr.Message = strings.TrimSpace(string(body))
	if apiErr.Message == "" {
		apiErr.Message = http.StatusText(status)
	}
	return apiErr
}
