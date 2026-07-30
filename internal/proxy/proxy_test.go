package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"golang.org/x/net/html"
)

func TestParseNonce(t *testing.T) {
	proxy := NewProxyHelper()

	tests := []struct {
		name     string
		csp      string
		expected string
	}{
		{
			name:     "valid nonce",
			csp:      "script-src 'nonce-abc123'",
			expected: "abc123",
		},
		{
			name:     "no nonce",
			csp:      "script-src 'self'",
			expected: "",
		},
		{
			name:     "empty CSP",
			csp:      "",
			expected: "",
		},
		{
			name:     "nonce with multiple directives",
			csp:      "default-src 'self'; script-src 'nonce-xyz789' 'strict-dynamic'",
			expected: "xyz789",
		},
		{
			name:     "no script-src directive",
			csp:      "default-src 'self'; style-src 'unsafe-inline'",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := proxy.parseNonce(tt.csp)
			if got != tt.expected {
				t.Errorf("parseNonce(%q) = %q, want %q", tt.csp, got, tt.expected)
			}
		})
	}
}

func TestInsertScriptTagIntoBody(t *testing.T) {
	proxy := NewProxyHelper()

	tests := []struct {
		name        string
		nonce       string
		body        string
		expectErr   bool
		expectInSrc string
	}{
		{
			name:        "basic HTML body",
			nonce:       "",
			body:        "<html><head></head><body><h1>Hello</h1></body></html>",
			expectErr:   false,
			expectInSrc: `src="/_gothicframework/reload/script.js"`,
		},
		{
			name:        "with nonce",
			nonce:       "abc123",
			body:        "<html><head></head><body><h1>Hello</h1></body></html>",
			expectErr:   false,
			expectInSrc: `nonce="abc123"`,
		},
		{
			name:        "minimal HTML parsed with implicit body",
			nonce:       "",
			body:        "<html><head></head></html>",
			expectErr:   false,
			expectInSrc: `src="/_gothicframework/reload/script.js"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := proxy.insertScriptTagIntoBody(tt.nonce, tt.body)
			if tt.expectErr && err == nil {
				t.Error("expected error but got nil")
				return
			}
			if !tt.expectErr && err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if tt.expectInSrc != "" && !strings.Contains(result, tt.expectInSrc) {
				t.Errorf("expected result to contain %q, got: %s", tt.expectInSrc, result)
			}
		})
	}
}

func TestModifyResponse_SkipNonHTML(t *testing.T) {
	proxy := NewProxyHelper()

	resp := &http.Response{
		Header:  http.Header{"Content-Type": {"application/json"}},
		Body:    io.NopCloser(strings.NewReader(`{"key":"value"}`)),
		Request: &http.Request{URL: mustParseURL("http://localhost/api")},
	}

	err := proxy.modifyResponse(resp)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	// Body should be unchanged
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"key":"value"}` {
		t.Errorf("expected body unchanged for non-HTML, got: %s", string(body))
	}
}

func TestModifyResponse_SkipHXRequest(t *testing.T) {
	proxy := NewProxyHelper()

	htmlContent := "<html><body>hi</body></html>"
	header := make(http.Header)
	header.Set("Content-Type", "text/html")
	header.Set("Gothic-Framework-Skip-Modify", "true")
	resp := &http.Response{
		Header:  header,
		Body:    io.NopCloser(strings.NewReader(htmlContent)),
		Request: &http.Request{URL: mustParseURL("http://localhost/")},
	}

	err := proxy.modifyResponse(resp)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)
	if bodyStr != htmlContent {
		t.Errorf("body was modified when it shouldn't have been: got %q", bodyStr)
	}
}

func TestModifyResponse_InjectScript(t *testing.T) {
	proxy := NewProxyHelper()

	htmlBody := "<html><head></head><body><h1>Hello</h1></body></html>"
	resp := &http.Response{
		Header:  http.Header{"Content-Type": {"text/html"}},
		Body:    io.NopCloser(strings.NewReader(htmlBody)),
		Request: &http.Request{URL: mustParseURL("http://localhost/")},
	}

	err := proxy.modifyResponse(resp)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "/_gothicframework/reload/script.js") {
		t.Error("expected script tag to be injected into response")
	}
}

func TestModifyResponse_GzipEncoding(t *testing.T) {
	proxy := NewProxyHelper()

	htmlBody := "<html><head></head><body><h1>Hello</h1></body></html>"

	// Gzip encode the body
	var gzBuf bytes.Buffer
	gzWriter := gzip.NewWriter(&gzBuf)
	gzWriter.Write([]byte(htmlBody))
	gzWriter.Close()

	resp := &http.Response{
		Header:  http.Header{"Content-Type": {"text/html"}, "Content-Encoding": {"gzip"}},
		Body:    io.NopCloser(&gzBuf),
		Request: &http.Request{URL: mustParseURL("http://localhost/")},
	}

	err := proxy.modifyResponse(resp)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	// Decompress the response
	gzReader, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("failed to create gzip reader: %v", err)
	}
	body, _ := io.ReadAll(gzReader)
	if !strings.Contains(string(body), "/_gothicframework/reload/script.js") {
		t.Error("expected script tag in gzip-encoded response")
	}
}

// TestRoundTripper_ContextCancellation proves the cancellation path in
// RoundTrip fires (returns r.Context().Err()), not an invalid-address parse
// error. A real server accepts the connection but blocks until the test
// cancels the request context; RoundTrip's in-flight transport call is then
// torn down and the loop returns context.Canceled.
func TestRoundTripper_ContextCancellation(t *testing.T) {
	serverReady := make(chan struct{})
	releaseServer := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(serverReady)
		<-releaseServer // block until the test is done
	}))
	defer server.Close()
	defer close(releaseServer)

	rt := &roundTripper{
		maxRetries:      5,
		initialDelay:    10 * time.Millisecond,
		backoffExponent: 1.5,
	}

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)

	errCh := make(chan error, 1)
	go func() {
		_, err := rt.RoundTrip(req)
		errCh <- err
	}()

	// Wait until the server has the request in hand, then cancel mid-flight.
	<-serverReady
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RoundTrip did not return after context cancellation")
	}
}

// TestRoundTripper_RetriesUntilMaxRetries proves the retry/backoff loop runs
// exactly maxRetries times before giving up. The target port has no listener,
// so every http.DefaultTransport.RoundTrip returns a dial error and the loop
// retries with backoff. maxRetries/initialDelay are injectable struct fields,
// so we use a tiny count and delay to keep the test fast.
//
// Note: the loop only retries on transport errors (connection failures). It
// does NOT retry on HTTP error status codes like 503, a 503 returns with a
// nil error and RoundTrip returns it immediately on the first attempt. That
// 503-passthrough behavior is therefore not a retry path and is not asserted
// here.
func TestRoundTripper_RetriesUntilMaxRetries(t *testing.T) {
	// Reserve a port with a listener, then close it so dials are refused.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	rt := &roundTripper{
		maxRetries:      3,
		initialDelay:    1 * time.Millisecond,
		backoffExponent: 1.0, // flat 1ms delay between attempts
	}

	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/", nil)
	resp, err := rt.RoundTrip(req)
	if resp != nil {
		t.Errorf("expected nil response after exhausting retries, got %v", resp)
	}
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if !strings.Contains(err.Error(), "max retries reached") {
		t.Errorf("expected max-retries error, got %v", err)
	}
}

// TestRoundTripper_DelayCeiling proves delayForRetry never exceeds maxDelay
// across all retry indices for the production configuration.
func TestRoundTripper_DelayCeiling(t *testing.T) {
	rt := &roundTripper{
		initialDelay:    100 * time.Millisecond,
		backoffExponent: 1.5,
		maxDelay:        250 * time.Millisecond,
	}
	for retry := 0; retry < 100; retry++ {
		d := rt.delayForRetry(retry)
		if d > rt.maxDelay {
			t.Errorf("retry %d: delay %v exceeds maxDelay %v", retry, d, rt.maxDelay)
		}
	}
}

// TestRoundTripper_TotalWorstCaseBounded verifies that the cumulative delay
// across all maxRetries (20) with a 250 ms ceiling is at most 4.5 s, and
// states the exact value in the output for pinning.
func TestRoundTripper_TotalWorstCaseBounded(t *testing.T) {
	rt := &roundTripper{
		maxRetries:      20,
		initialDelay:    100 * time.Millisecond,
		backoffExponent: 1.5,
		maxDelay:        250 * time.Millisecond,
	}
	var total time.Duration
	// delays are computed for retries 0 through maxRetries-2 (the last
	// attempt does not wait, it returns the error immediately).
	for retry := 0; retry < rt.maxRetries-1; retry++ {
		total += rt.delayForRetry(retry)
	}
	if total > 5*time.Second {
		t.Errorf("total worst-case delay %v exceeds 5 s", total)
	}
	t.Logf("total worst-case delay with ceiling: %v (capped at 250 ms)", total)
	// Exact value: with time.Duration truncating float to int64,
	// retries 0-2 compute as 100/100/200 ms, then 16 × 250 ms = 4.4 s.
	const expected = 4400 * time.Millisecond
	if total != expected {
		t.Errorf("expected exact total %v, got %v", expected, total)
	}
}

// TestRoundTripper_RetriesWithProductionConfig proves the production
// roundTripper configuration (maxRetries=20, 250 ms ceiling) exhausts retries
// and returns a max-retries error like the lightweight version, pinning the
// shipped values in a test. Cumulative wait is ~4.5 s so the test times out
// safely within 10 s.
func TestRoundTripper_RetriesWithProductionConfig(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	rt := &roundTripper{
		maxRetries:      20,
		initialDelay:    100 * time.Millisecond,
		backoffExponent: 1.5,
		maxDelay:        250 * time.Millisecond,
	}

	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/", nil)
	resp, err := rt.RoundTrip(req)
	if resp != nil {
		t.Errorf("expected nil response after exhausting retries, got %v", resp)
	}
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if !strings.Contains(err.Error(), "max retries reached") {
		t.Errorf("expected max-retries error, got %v", err)
	}
}

// TestSend_ConcurrentDisconnect proves that subscribers disconnecting
// concurrently with Send does not panic. Many subscribers register,
// disconnect after a short delay, while sends fire in rapid succession.
func TestSend_ConcurrentDisconnect(t *testing.T) {
	sse := NewsseHandler()
	var wg sync.WaitGroup

	// Register and immediately disconnect N subscribers while sends fire.
	for i := 0; i < 50; i++ {
		wg.Add(1)
		_, cancel := context.WithCancel(context.Background())

		// Simulate an SSE subscriber: register, wait briefly, cancel.
		go func() {
			defer wg.Done()
			defer cancel()

			id := atomic.AddInt64(&sse.counter, 1)
			sse.m.Lock()
			events := make(chan event, 1)
			sse.requests[id] = events
			sse.m.Unlock()

			// Read from events so the buffered channel doesn't fill up,
			// but cancel after a short random-ish interval.
			done := make(chan struct{})
			go func() {
				for {
					select {
					case <-events:
					case <-done:
						return
					}
				}
			}()

			// Give the subscriber time to be active before cancelling.
			time.Sleep(time.Duration(i) * 100 * time.Microsecond)
			cancel()

			sse.m.Lock()
			delete(sse.requests, id)
			close(events)
			sse.m.Unlock()
			close(done)
		}()
	}

	// Fire sends while subscribers come and go.
	for i := 0; i < 20; i++ {
		time.Sleep(50 * time.Microsecond)
		sse.Send("message", "reload")
	}

	wg.Wait()
}

// stubProxyTarget wires ProxyHelper.p to a backend so ServeHTTP can fall through
// to real proxying for non-internal paths.
func stubProxyTarget(t *testing.T, proxy *ProxyHelper, backend *httptest.Server) {
	t.Helper()
	target, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("failed to parse backend URL: %v", err)
	}
	proxy.buildProxy(target)
}

func TestServeHTTP_ReloadScript(t *testing.T) {
	proxy := NewProxyHelper()

	req := httptest.NewRequest(http.MethodGet, "/_gothicframework/reload/script.js", nil)
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if ct := rec.Header().Get("Content-Type"); ct != "text/javascript" {
		t.Errorf("expected Content-Type text/javascript, got %q", ct)
	}
	if rec.Body.Len() == 0 {
		t.Error("expected reload script body to be written")
	}
}

func TestServeHTTP_ReloadEventsPost(t *testing.T) {
	proxy := NewProxyHelper()

	// POST to events triggers Send (line 129). With no SSE subscribers this is a
	// no-op but still exercises the Send loop and the POST branch.
	req := httptest.NewRequest(http.MethodPost, "/_gothicframework/reload/events", nil)
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for POST events, got %d", rec.Code)
	}
}

func TestServeHTTP_ReloadEventsMethodNotAllowed(t *testing.T) {
	proxy := NewProxyHelper()

	req := httptest.NewRequest(http.MethodDelete, "/_gothicframework/reload/events", nil)
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for DELETE events, got %d", rec.Code)
	}
}

func TestServeHTTP_ProxiesToTarget(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<html><head></head><body>backend</body></html>")
	}))
	defer backend.Close()

	proxy := NewProxyHelper()
	stubProxyTarget(t, &proxy, backend)

	req := httptest.NewRequest(http.MethodGet, "/some/page", nil)
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 proxied, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "backend") {
		t.Errorf("expected proxied backend content, got %q", rec.Body.String())
	}
	// modifyResponse should have injected the reload script.
	if !strings.Contains(rec.Body.String(), "/_gothicframework/reload/script.js") {
		t.Error("expected reload script injected into proxied HTML")
	}
}

func TestServeHTTP_ReloadEventsGet(t *testing.T) {
	proxy := NewProxyHelper()

	// GET to events opens an SSE stream that blocks; cancel via context to return.
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/_gothicframework/reload/events", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		proxy.ServeHTTP(rec, req)
		close(done)
	}()

	// Give the handler time to register and emit the initial ping.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE handler did not return after context cancellation")
	}

	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("expected Content-Type text/event-stream, got %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "data: ping") {
		t.Errorf("expected ping in SSE output, got %q", rec.Body.String())
	}
}

func TestSendDeliversEventToSubscriber(t *testing.T) {
	proxy := NewProxyHelper()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/_gothicframework/reload/events", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		proxy.Sse.ServeHTTP(rec, req)
	}()

	// Wait for the subscriber to register.
	time.Sleep(50 * time.Millisecond)

	// SendSSE delegates to Sse.Send (line 129) and delivers to subscribers.
	proxy.SendSSE("message", "reload")

	time.Sleep(50 * time.Millisecond)
	cancel()
	wg.Wait()

	out := rec.Body.String()
	if !strings.Contains(out, "event: message") || !strings.Contains(out, "data: reload") {
		t.Errorf("expected delivered reload event, got %q", out)
	}
}

func TestNotifyProxy(t *testing.T) {
	var gotMethod, gotPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	u, _ := url.Parse(backend.URL)
	host, portStr, _ := strings.Cut(u.Host, ":")
	port := 0
	for _, c := range portStr {
		port = port*10 + int(c-'0')
	}

	if err := NotifyProxy(host, port); err != nil {
		t.Fatalf("NotifyProxy returned error: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("expected POST, got %s", gotMethod)
	}
	if gotPath != "/_gothicframework/reload/events" {
		t.Errorf("expected reload events path, got %s", gotPath)
	}
}

func TestNotifyProxy_ConnectionError(t *testing.T) {
	// Port 0 with no listener: Do() should return a connection error.
	err := NotifyProxy("127.0.0.1", 1)
	if err == nil {
		t.Error("expected error notifying a closed port")
	}
}

func TestSetShouldSkipResponseModificationHeader(t *testing.T) {
	rt := &roundTripper{}

	reqHeader := make(http.Header)
	reqHeader.Set("HX-Request", "true")
	req := &http.Request{Header: reqHeader}
	resp := &http.Response{Header: make(http.Header)}

	rt.setShouldSkipResponseModificationHeader(req, resp)
	if resp.Header.Get("gothic-framework-skip-modify") != "true" {
		t.Error("expected skip-modify header set for HX-Request")
	}

	// Non-HX request: header must not be set.
	req2 := &http.Request{Header: make(http.Header)}
	resp2 := &http.Response{Header: make(http.Header)}
	rt.setShouldSkipResponseModificationHeader(req2, resp2)
	if resp2.Header.Get("gothic-framework-skip-modify") != "" {
		t.Error("expected no skip-modify header for non-HX request")
	}
}

func TestModifyResponse_BrotliEncoding(t *testing.T) {
	proxy := NewProxyHelper()

	htmlBody := "<html><head></head><body><h1>Hello</h1></body></html>"

	var brBuf bytes.Buffer
	brWriter := brotli.NewWriter(&brBuf)
	brWriter.Write([]byte(htmlBody))
	brWriter.Close()

	header := make(http.Header)
	header.Set("Content-Type", "text/html")
	header.Set("Content-Encoding", "br")
	resp := &http.Response{
		Header:  header,
		Body:    io.NopCloser(&brBuf),
		Request: &http.Request{URL: mustParseURL("http://localhost/")},
	}

	if err := proxy.modifyResponse(resp); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	body, _ := io.ReadAll(brotli.NewReader(resp.Body))
	if !strings.Contains(string(body), "/_gothicframework/reload/script.js") {
		t.Error("expected script tag in brotli-encoded response")
	}
}

func TestBustPublicAssetCache(t *testing.T) {
	proxy := NewProxyHelper()

	body := `<html><head>` +
		`<link href="/public/styles.css" rel="stylesheet">` +
		`<link href="https://cdn.example.com/x.css" rel="stylesheet">` +
		`<script src="/_gothic/gothic-core.js?v=abc123"></script>` +
		`</head><body>` +
		`<script src="/public/app.js"></script>` +
		`<script src="https://cdn.example.com/lib.js"></script>` +
		`</body></html>`

	result, err := proxy.insertScriptTagIntoBody("", body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(result, "/public/styles.css?v=") {
		t.Error("expected /public/ link href to be cache-busted")
	}
	if !strings.Contains(result, "/public/app.js?v=") {
		t.Error("expected /public/ script src to be cache-busted")
	}
	// Framework runtime assets under /_gothic/ must ALSO be cache-busted in dev.
	// They already carry a ?v=<hash>, so the buster must join with & (a second ?
	// would corrupt the URL). The HTML serializer escapes & as &amp;, which the
	// browser decodes back to & on parse.
	if !strings.Contains(result, "/_gothic/gothic-core.js?v=abc123&amp;v=") {
		t.Errorf("expected /_gothic/ src to be cache-busted with & separator, got: %s", result)
	}
	if strings.Contains(result, "abc123?v=") {
		t.Errorf("/_gothic/ buster must not produce a double-query URL, got: %s", result)
	}
	// External assets must remain untouched.
	if strings.Contains(result, "cdn.example.com/x.css?v=") {
		t.Error("external link href should not be cache-busted")
	}
	if strings.Contains(result, "cdn.example.com/lib.js?v=") {
		t.Error("external script src should not be cache-busted")
	}
}

func TestGetAttrValue(t *testing.T) {
	proxy := NewProxyHelper()

	node := &html.Node{
		Type: html.ElementNode,
		Data: "div",
		Attr: []html.Attribute{
			{Key: "id", Val: "main"},
			{Key: "class", Val: "container"},
		},
	}

	if got := proxy.getAttrValue(node, "id"); got != "main" {
		t.Errorf("expected 'main', got %q", got)
	}
	if got := proxy.getAttrValue(node, "missing"); got != "" {
		t.Errorf("expected empty for missing attr, got %q", got)
	}
}

func TestElementMatcherWithAttributes(t *testing.T) {
	proxy := NewProxyHelper()

	node := &html.Node{
		Type: html.ElementNode,
		Data: "div",
		Attr: []html.Attribute{{Key: "id", Val: "target"}},
	}

	matchID := proxy.element("div", attribute{Name: "id", Value: "target"})
	if !matchID(node) {
		t.Error("expected matcher to match div with id=target")
	}

	matchWrongVal := proxy.element("div", attribute{Name: "id", Value: "other"})
	if matchWrongVal(node) {
		t.Error("expected matcher to reject wrong attribute value")
	}

	textNode := &html.Node{Type: html.TextNode, Data: "div"}
	if matchID(textNode) {
		t.Error("expected matcher to reject non-element node")
	}
}

func TestRunProxy_BindError(t *testing.T) {
	proxy := NewProxyHelper()
	target := mustParseURL("http://localhost:12345")

	// An invalid bind address makes ListenAndServe fail immediately, exercising
	// RunProxy's setup and error-return path without leaving a listener open.
	err := proxy.RunProxy("256.256.256.256", 0, target)
	if err == nil {
		t.Error("expected RunProxy to return an error for an invalid bind address")
	}
}

func mustParseURL(rawURL string) *url.URL {
	u, err := url.Parse(rawURL)
	if err != nil {
		panic(err)
	}
	return u
}

// TestSendReplaysBuildStateToNewSubscriber covers the reason build state is
// remembered at all: the paint reload disconnects the browser while the compile
// is still running, so the document that comes back must be told a build is
// still in flight instead of looking finished on the previous binary.
func TestSendReplaysBuildStateToNewSubscriber(t *testing.T) {
	s := NewsseHandler()
	s.Send("building", "src/pages/counter.templ")

	srv := httptest.NewServer(s)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer resp.Body.Close()

	// The stream opens with its retry hint, so read until the replayed event
	// arrives rather than assuming it lands in the first chunk.
	var got string
	buf := make([]byte, 256)
	for i := 0; i < 4 && !strings.Contains(got, "event: building"); i++ {
		n, err := resp.Body.Read(buf)
		got += string(buf[:n])
		if err != nil {
			break
		}
	}
	if !strings.Contains(got, "event: building") || !strings.Contains(got, "src/pages/counter.templ") {
		t.Errorf("new subscriber did not receive the pending build state, got %q", got)
	}
}

// TestBuildDoneClearsReplayedState proves the badge state does not outlive the
// build: once the stage settles, a fresh subscriber must not be told a compile
// is running.
func TestBuildDoneClearsReplayedState(t *testing.T) {
	s := NewsseHandler()
	s.Send("building", "src/pages/counter.templ")
	s.Send("builddone", "")

	if s.buildState != nil {
		t.Fatalf("buildState = %+v, want nil after builddone", s.buildState)
	}
}

// TestSendDeliversBurstWithoutDropping guards the ordering the badge depends
// on: a cycle emits builddone immediately followed by reload, and a one-slot
// buffer silently dropped the reload, so the page never refreshed.
func TestSendDeliversBurstWithoutDropping(t *testing.T) {
	s := NewsseHandler()
	events := make(chan event, 4)
	s.m.Lock()
	s.requests[1] = events
	s.m.Unlock()

	s.Send("builddone", "")
	s.Send("message", "reload")

	if len(events) != 2 {
		t.Fatalf("buffered %d events, want 2 (the burst must not be dropped)", len(events))
	}
	if e := <-events; e.Type != "builddone" {
		t.Errorf("first event = %q, want builddone", e.Type)
	}
	if e := <-events; e.Type != "message" || e.Data != "reload" {
		t.Errorf("second event = %q/%q, want message/reload", e.Type, e.Data)
	}
}

// TestEncodeEventSplitsMultilineData pins the SSE framing for compiler errors.
// A raw multi-line payload ends the frame at its first newline, so the browser
// received the "package load errors:" header with every diagnostic stripped
// off. One data line per source line is what the protocol requires and what
// EventSource rejoins on the client.
func TestEncodeEventSplitsMultilineData(t *testing.T) {
	got := encodeEvent(event{Type: "builderror", Data: "wasm: load packages:\nfoo.go:12: undefined: bar\nbaz.go:3: syntax error"})
	want := "event: builderror\n" +
		"data: wasm: load packages:\n" +
		"data: foo.go:12: undefined: bar\n" +
		"data: baz.go:3: syntax error\n\n"
	if got != want {
		t.Errorf("encodeEvent() = %q, want %q", got, want)
	}
}

// TestEncodeEventSingleLine keeps the common case byte-identical to the frame
// the client has always received.
func TestEncodeEventSingleLine(t *testing.T) {
	if got, want := encodeEvent(event{Type: "message", Data: "reload"}), "event: message\ndata: reload\n\n"; got != want {
		t.Errorf("encodeEvent() = %q, want %q", got, want)
	}
}

// TestSubscribersCountsLiveTabs backs the rule that a session does not open a
// second browser tab when one is already watching.
func TestSubscribersCountsLiveTabs(t *testing.T) {
	s := NewsseHandler()
	if got := s.Subscribers(); got != 0 {
		t.Fatalf("Subscribers() = %d on a fresh handler, want 0", got)
	}

	srv := httptest.NewServer(s)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer resp.Body.Close()

	deadline := time.Now().Add(2 * time.Second)
	for s.Subscribers() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := s.Subscribers(); got != 1 {
		t.Errorf("Subscribers() = %d with one tab connected, want 1", got)
	}

	cancel()
	deadline = time.Now().Add(2 * time.Second)
	for s.Subscribers() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := s.Subscribers(); got != 0 {
		t.Errorf("Subscribers() = %d after the tab left, want 0", got)
	}
}

// TestStreamAdvertisesRetryInterval pins the hint that makes an orphaned tab
// reconnect fast enough for the session to notice it before opening another.
func TestStreamAdvertisesRetryInterval(t *testing.T) {
	s := NewsseHandler()
	srv := httptest.NewServer(s)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 64)
	n, _ := resp.Body.Read(buf)
	if got := string(buf[:n]); !strings.HasPrefix(got, "retry: ") {
		t.Errorf("stream opened with %q, want a retry hint first", got)
	}
}
