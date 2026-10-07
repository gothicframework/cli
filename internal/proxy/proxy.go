package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/gothicframework/cli/v4/internal/output"
	"golang.org/x/net/html"

	_ "embed"
)

//go:embed script.js
var reloadScriptJS string

var errBodyNotFound = fmt.Errorf("body not found")

// ── Dev bus: the browser-side observer's ingest + read endpoint ──
//
// The dev script's observer decodes the page's topic data-plane and core
// control-plane traffic in the browser and POSTs decoded records here in
// batches. This ring keeps the last busRingCapacity records so a developer (or
// a future inspector) can read what a page actually announced, newest-first.

const busRingCapacity = 500

const busDefaultLast = 100 // GET ?last= default when the parameter is absent

const busBodyLimit = 8 << 20 // 8 MB — a 50-record batch can carry large strings

// BusVerbose turns on one deduped terminal line per ingested record. The
// hot-reload command wires its --verbose flag here; off by default.
var BusVerbose bool

// busRecord is one observed bus event. Shape (the dev-side contract):
//
//	t (ms epoch), kind (topic|durable|control|mark), dir (pub|bcast|act),
//	event, topic, field, payload (a decoded object, or {raw_hex, schema} when
//	the frame is undecodable, or null).
//
// kind:"mark" records are the capture timeline's act layer: the injected
// script emits one per user/agent input (click, input, keydown, scroll, …)
// plus a debounced "settle" signal when the page goes quiet. dir is always
// "act"; event is the input type; payload carries {sel,x,y,...} identifying
// what was touched. They ride the same batching and ring as every other kind
// — this endpoint accepts them without special handling by design.
type busRecord = map[string]any

type busRing struct {
	mu   sync.Mutex
	ents []busRecord
}

// add appends a batch, evicting the oldest entries beyond the ring capacity.
func (b *busRing) add(records []busRecord) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, r := range records {
		if len(b.ents) >= busRingCapacity {
			copy(b.ents, b.ents[1:])
			b.ents[len(b.ents)-1] = r
		} else {
			b.ents = append(b.ents, r)
		}
	}
}

// newestFirst returns a copy of the ring, newest first. With a non-negative
// limit, only the newest `limit` records; with a negative limit, all of them.
func (b *busRing) newestFirst(limit int) []busRecord {
	b.mu.Lock()
	defer b.mu.Unlock()
	total := len(b.ents)
	max := total
	if limit >= 0 && limit < max {
		max = limit
	}
	out := make([]busRecord, 0, max)
	for i := 0; i < max; i++ {
		out = append(out, b.ents[total-1-i])
	}
	return out
}

func (b *busRing) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.ents)
}

// busVerboseKey/Line render the one --verbose line per record. The key carries
// the record identity, so output.Print's consecutive dedup collapses identical
// consecutive traffic (e.g. repeated identical broadcasts) with an (xN) suffix.
func busVerboseKey(rec busRecord) string {
	return fmt.Sprint(rec["kind"], "|", rec["dir"], "|", rec["event"],
		"|", rec["topic"], "|", rec["field"])
}

func busVerboseLine(rec busRecord) string {
	return fmt.Sprintf("%s bus %s %s topic=%v field=%v payload=%v",
		output.Tag("Observed"), rec["dir"], rec["event"], rec["topic"], rec["field"], rec["payload"])
}

// handleBusPOST ingests one batch. Body shape: {"records": [...]} (a bare
// JSON array is accepted too — the dev contract is deliberately loose).
// Same-origin only: the endpoint is loopback dev tooling, and a simple
// cross-origin POST (text/plain content-type skips the CORS preflight) could
// otherwise plant fabricated records in an agent's observation timeline.
func (proxy *ProxyHelper) handleBusPOST(w http.ResponseWriter, r *http.Request) {
	if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host {
		http.Error(w, "cross-origin bus post", http.StatusForbidden)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, busBodyLimit))
	if err != nil {
		http.Error(w, "cannot read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var payload struct {
		Records []busRecord `json:"records"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		var arr []busRecord
		if err2 := json.Unmarshal(body, &arr); err2 != nil {
			http.Error(w, "invalid bus payload: "+err.Error(), http.StatusBadRequest)
			return
		}
		payload.Records = arr
	}
	if len(payload.Records) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if BusVerbose {
		for _, rec := range payload.Records {
			output.Print(busVerboseKey(rec), busVerboseLine(rec))
		}
	}
	proxy.Bus.add(payload.Records)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]int{"stored": len(payload.Records)}); err != nil {
		output.Errorln("cannot write the bus response: %v", err)
	}
}

// handleBusGET serves the ring newest-first; ?last=N trims to the newest N.
func (proxy *ProxyHelper) handleBusGET(w http.ResponseWriter, r *http.Request) {
	limit := busDefaultLast
	if raw := strings.TrimSpace(r.URL.Query().Get("last")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			http.Error(w, "invalid last parameter", http.StatusBadRequest)
			return
		}
		limit = n
	}
	recs := proxy.Bus.newestFirst(limit)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"records": recs}); err != nil {
		output.Errorln("cannot write the bus log: %v", err)
	}
}

// proxyStartedAt is a per-process timestamp used as a cache-buster for public
// assets in the proxy-injected HTML. Every `make dev` restart produces a new
// value, forcing the browser to fetch fresh CSS/JS instead of serving a stale
// cached version whose URL hasn't changed.
var proxyStartedAt = strconv.FormatInt(time.Now().UnixNano(), 10)

type ProxyHelper struct {
	URL    string
	Target *url.URL
	p      *httputil.ReverseProxy
	Sse    *sseHandler
	// Bus holds the dev observers' decoded traffic (POST ingest, GET read).
	Bus *busRing
	// DevHandlers serves dev-tooling endpoints by exact request path, before
	// proxying (the dev command binds the MCP surface here; --no-mcp binds
	// http.NotFound to keep the path answering 404 without a backend round
	// trip). Unmatched paths (e.g. the app's /_gothicframework/trace) fall
	// through to the app server as before.
	DevHandlers map[string]http.Handler
}

// RoundTripper with retries and capped exponential backoff. The delay ceiling
// keeps the transport suitable as a readiness mechanism, unbounded growth
// buys nothing when the goal is to wait for the backend to start.
type roundTripper struct {
	maxRetries      int
	initialDelay    time.Duration
	backoffExponent float64
	maxDelay        time.Duration // per-attempt ceiling; 0 = no cap
}

// SSE event and handler (migrated from the sse package)

type event struct {
	Type string
	Data string
}

type sseHandler struct {
	m        *sync.Mutex
	counter  int64
	requests map[int64]chan event
	// buildState is the latest badge-worthy event (building or builderror), or
	// nil once the build settles. It is replayed to every new subscriber so a
	// page that reloads mid-compile knows a compile is still running.
	buildState *event
}

func NewProxyHelper() ProxyHelper {
	return ProxyHelper{
		Sse: NewsseHandler(),
		Bus: &busRing{},
	}
}

func NewsseHandler() *sseHandler {
	return &sseHandler{
		m:        new(sync.Mutex),
		requests: make(map[int64]chan event),
	}
}

// buildProxy configures the underlying reverse proxy (transport with retries and
// response modification) for the given target. Split out from RunProxy so the
// proxy can be wired up without binding a listener (e.g. in tests).
func (proxy *ProxyHelper) buildProxy(target *url.URL) {
	p := httputil.NewSingleHostReverseProxy(target)
	p.ErrorLog = log.New(os.Stderr, "Proxy error: ", 0)
	p.Transport = &roundTripper{
		maxRetries:      20,
		initialDelay:    100 * time.Millisecond,
		backoffExponent: 1.5,
		maxDelay:        250 * time.Millisecond,
	}

	proxy.Target = target
	proxy.p = p
	proxy.p.ModifyResponse = proxy.modifyResponse
}

// RunProxy configures and starts the proxy server with bind, port, and target
func (proxy *ProxyHelper) RunProxy(bind string, port int, target *url.URL) error {
	proxy.URL = fmt.Sprintf("http://%s:%d", bind, port)

	proxy.buildProxy(target)

	output.Println("%s %s -> %s", output.Tag("PROXY"), output.Link(proxy.URL), output.Link(fmt.Sprint(target)))

	if err := http.ListenAndServe(fmt.Sprintf("%s:%d", bind, port), proxy); err != nil {
		return fmt.Errorf("failed to start proxy server: %w", err)
	}
	return nil
}

// ServeHTTP handles internal routes and normal proxying
func (proxy *ProxyHelper) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/_gothicframework/reload/script.js":
		w.Header().Add("Content-Type", "text/javascript")
		// This route answers directly and never reaches modifyResponse, which is
		// what puts no-store on everything else. Without the header the script has
		// no Cache-Control, no ETag and no Last-Modified, so a browser is free to
		// keep a copy from an older CLI: the developer upgrades, and the tab goes
		// on running the previous dev script with no way to tell.
		w.Header().Set("Cache-Control", "no-store, must-revalidate")
		_, err := io.WriteString(w, reloadScriptJS)
		if err != nil {
			output.Errorln("cannot write the reload script: %v", err)
		}
		return

	case "/_gothicframework/reload/events":
		switch r.Method {
		case http.MethodGet:
			proxy.Sse.ServeHTTP(w, r)
		case http.MethodPost:
			proxy.Sse.Send("message", "reload")
		default:
			http.Error(w, "only GET or POST method allowed", http.StatusMethodNotAllowed)
		}
		return

	case "/_gothicframework/reload/bus":
		switch r.Method {
		case http.MethodPost:
			proxy.handleBusPOST(w, r)
		case http.MethodGet:
			proxy.handleBusGET(w, r)
		default:
			http.Error(w, "only GET or POST method allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	// Dev-tooling endpoints bound by the dev command (exact path match).
	if h := proxy.DevHandlers[r.URL.Path]; h != nil {
		h.ServeHTTP(w, r)
		return
	}

	proxy.p.ServeHTTP(w, r)
}

// SSE methods

// Send delivers an event to every live subscriber. The per-subscriber channel
// is buffered and Send writes synchronously while holding the lock, so a
// sender either sees the subscriber in the map or does not, there is no
// goroutine in flight that could send on a closed channel.
//
// The buffer holds a short burst because a cycle can emit two events back to
// back (builddone then reload). With a single slot the second one was dropped
// and the page never reloaded.
func (s *sseHandler) Send(eventType string, data string) {
	s.m.Lock()
	defer s.m.Unlock()
	e := event{Type: eventType, Data: data}

	// Remember the badge state so a subscriber that connects mid-build is told
	// what is happening. Without this, the reload that repaints the page also
	// disconnects the client, and a build finishing in that window leaves the
	// new document with no idea a compile is still running.
	switch eventType {
	case "building", "builderror":
		s.buildState = &e
	case "builddone":
		s.buildState = nil
	}

	for _, ch := range s.requests {
		select {
		case ch <- e:
		default:
			// Subscriber buffer full; skip. The subscriber will get the next
			// event or the periodic ping.
		}
	}
}

// encodeEvent renders one SSE frame. A payload is emitted as one "data:" line
// per source line, which is what the protocol requires and what EventSource
// rejoins with newlines on the client. A raw multi-line payload would end the
// frame at its first newline, so a compiler error reached the browser as its
// header alone with the diagnostic stripped off.
func encodeEvent(e event) string {
	var b strings.Builder
	b.WriteString("event: ")
	b.WriteString(e.Type)
	b.WriteString("\n")
	for _, line := range strings.Split(e.Data, "\n") {
		b.WriteString("data: ")
		b.WriteString(strings.TrimRight(line, "\r"))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	return b.String()
}

// Subscribers reports how many browser tabs are currently listening. A tab
// left open from a previous session reconnects on its own, so a non-zero count
// means somebody is already watching and the session does not need to open
// another one.
func (s *sseHandler) Subscribers() int {
	s.m.Lock()
	defer s.m.Unlock()
	return len(s.requests)
}

func (s *sseHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Tell the browser how fast to reconnect after the stream drops. Every
	// rebuild restarts the app, and the default retry (seconds, browser-defined)
	// leaves an open tab disconnected long enough that the session cannot tell
	// it is there.
	if _, err := io.WriteString(w, "retry: 500\n\n"); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.(http.Flusher).Flush()

	id := atomic.AddInt64(&s.counter, 1)
	s.m.Lock()
	events := make(chan event, 4)
	s.requests[id] = events
	pending := s.buildState
	s.m.Unlock()

	// Replay the current badge state before entering the loop, so a page that
	// reloaded mid-compile picks the badge back up instead of looking finished
	// while the old binary is still live. Written directly rather than queued:
	// the keepalive timer is already armed, and the loop's select would pick
	// between the two at random.
	if pending != nil {
		if _, err := io.WriteString(w, encodeEvent(*pending)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.(http.Flusher).Flush()
	}

	defer func() {
		s.m.Lock()
		defer s.m.Unlock()
		delete(s.requests, id)
		close(events)
	}()

	timer := time.NewTimer(0)
loop:
	for {
		select {
		case <-timer.C:
			if _, err := fmt.Fprintf(w, "event: message\ndata: ping\n\n"); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			timer.Reset(time.Second * 5)
		case e := <-events:
			if _, err := io.WriteString(w, encodeEvent(e)); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		case <-r.Context().Done():
			break loop
		}
		w.(http.Flusher).Flush()
	}
}

// NotifyProxy helper to send reload via POST
func NotifyProxy(host string, port int) error {
	urlStr := fmt.Sprintf("http://%s:%d/_gothicframework/reload/events", host, port)
	req, err := http.NewRequest(http.MethodPost, urlStr, nil)
	if err != nil {
		return err
	}
	_, err = http.DefaultClient.Do(req)
	return err
}

// SSE send via ProxyHelper
func (proxy *ProxyHelper) SendSSE(eventType, data string) {
	proxy.Sse.Send(eventType, data)
}

// RoundTripper with retry and exponential backoff
// delayForRetry returns the exponential-backoff delay for the given retry
// index, capped at maxDelay when set. Exported as a method so tests assert
// on the delay function directly.
func (rt *roundTripper) delayForRetry(retries int) time.Duration {
	delay := rt.initialDelay * time.Duration(math.Pow(rt.backoffExponent, float64(retries)))
	if rt.maxDelay > 0 && delay > rt.maxDelay {
		delay = rt.maxDelay
	}
	return delay
}

func (rt *roundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	var bodyBytes []byte
	if r.Body != nil && r.Body != http.NoBody {
		var err error
		bodyBytes, err = io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		r.Body.Close()
	}

	var resp *http.Response
	var err error
	for retries := 0; retries < rt.maxRetries; retries++ {
		if r.Context().Err() != nil {
			return nil, r.Context().Err()
		}
		req := r.Clone(r.Context())
		if bodyBytes != nil {
			req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}
		resp, err = http.DefaultTransport.RoundTrip(req)
		if err != nil {
			delay := rt.delayForRetry(retries)
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
			continue
		}
		rt.setShouldSkipResponseModificationHeader(r, resp)
		return resp, nil
	}

	return nil, fmt.Errorf("max retries reached for URL: %q", r.URL.String())
}

func (rt *roundTripper) setShouldSkipResponseModificationHeader(r *http.Request, resp *http.Response) {
	if r.Header.Get("HX-Request") == "true" {
		resp.Header.Set("gothic-framework-skip-modify", "true")
	}
}

// Modify response to inject script and handle encoding
func (proxy *ProxyHelper) modifyResponse(r *http.Response) error {
	// Disable caching for all dev proxy responses, same effect as DevTools "Disable cache".
	r.Header.Set("Cache-Control", "no-store, must-revalidate")
	r.Header.Del("ETag")
	r.Header.Del("Last-Modified")

	urlStr := r.Request.URL.String()

	if r.Header.Get("gothic-framework-skip-modify") == "true" {
		return nil
	}

	if !strings.HasPrefix(r.Header.Get("Content-Type"), "text/html") {
		return nil
	}

	newReader := func(in io.Reader) (io.Reader, error) { return in, nil }
	newWriter := func(out io.Writer) io.WriteCloser { return passthroughWriteCloser{out} }

	switch r.Header.Get("Content-Encoding") {
	case "gzip":
		newReader = func(in io.Reader) (io.Reader, error) { return gzip.NewReader(in) }
		newWriter = func(out io.Writer) io.WriteCloser { return gzip.NewWriter(out) }
	case "br":
		newReader = func(in io.Reader) (io.Reader, error) { return brotli.NewReader(in), nil }
		newWriter = func(out io.Writer) io.WriteCloser { return brotli.NewWriter(out) }
	}

	encr, err := newReader(r.Body)
	if err != nil {
		return err
	}
	defer r.Body.Close()

	body, err := io.ReadAll(encr)
	if err != nil {
		return err
	}

	csp := r.Header.Get("Content-Security-Policy")
	updated, err := proxy.insertScriptTagIntoBody(proxy.parseNonce(csp), string(body))
	if err != nil {
		output.Errorln("cannot insert the reload script for %s: %v", urlStr, err)
		updated = string(body)
	}

	var buf bytes.Buffer
	writer := newWriter(&buf)
	_, err = writer.Write([]byte(updated))
	if err != nil {
		return err
	}
	if err = writer.Close(); err != nil {
		return err
	}

	r.Body = io.NopCloser(&buf)
	r.ContentLength = int64(buf.Len())
	r.Header.Set("Content-Length", strconv.Itoa(buf.Len()))

	return nil
}

// passthrough helper to write without closing the Writer
type passthroughWriteCloser struct {
	io.Writer
}

func (pwc passthroughWriteCloser) Close() error {
	return nil
}

// Helpers to manipulate HTML and inject script

func (proxy *ProxyHelper) parseNonce(csp string) string {
	for _, raw := range strings.Split(csp, ";") {
		parts := strings.Fields(raw)
		if len(parts) < 2 || parts[0] != "script-src" {
			continue
		}
		for _, part := range parts[1:] {
			part = strings.Trim(part, "'")
			if strings.HasPrefix(part, "nonce-") {
				return part[6:]
			}
		}
	}
	return ""
}

func (proxy *ProxyHelper) insertScriptTagIntoBody(nonce, body string) (string, error) {
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		return body, err
	}
	bodyNodes := proxy.all(doc, proxy.element("body"))
	if len(bodyNodes) == 0 {
		return body, errBodyNotFound
	}
	bodyNodes[0].AppendChild(proxy.newReloadScriptNode(nonce))
	proxy.bustPublicAssetCache(doc)

	var buf bytes.Buffer
	if err := html.Render(&buf, doc); err != nil {
		return body, err
	}
	return buf.String(), nil
}

// bustableAssetPrefixes are the URL prefixes whose href/src the dev proxy
// cache-busts on each server restart: the user's /public/ static files and the
// framework's /_gothic/ runtime assets (served from the framework embed).
var bustableAssetPrefixes = []string{"/public/", "/_gothic/"}

// bustAssetURL appends a per-process version query parameter to url when it
// points at a bustable asset, leaving every other URL untouched. It is
// query-aware: the /_gothic/ runtime assets already carry a ?v=<hash>, so it
// joins with & rather than a second ? that would corrupt the URL.
func bustAssetURL(url string) (string, bool) {
	for _, p := range bustableAssetPrefixes {
		if strings.HasPrefix(url, p) {
			sep := "?"
			if strings.Contains(url, "?") {
				sep = "&"
			}
			return url + sep + "v=" + proxyStartedAt, true
		}
	}
	return url, false
}

// bustPublicAssetCache rewrites href/src attributes that point to a bustable
// asset (/public/ or /_gothic/) to include a per-process version query
// parameter. This forces the browser to treat each server restart as a new URL,
// bypassing stale cached assets.
func (proxy *ProxyHelper) bustPublicAssetCache(doc *html.Node) {
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			var key string
			switch n.Data {
			case "link":
				key = "href"
			case "script":
				key = "src"
			}
			if key != "" {
				for i, a := range n.Attr {
					if a.Key == key {
						if busted, ok := bustAssetURL(a.Val); ok {
							n.Attr[i].Val = busted
						}
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
}

func (proxy *ProxyHelper) newReloadScriptNode(nonce string) *html.Node {
	script := &html.Node{
		Type: html.ElementNode,
		Data: "script",
		Attr: []html.Attribute{
			{Key: "src", Val: "/_gothicframework/reload/script.js"},
		},
	}
	if nonce != "" {
		script.Attr = append(script.Attr, html.Attribute{Key: "nonce", Val: nonce})
	}
	return script
}

type matcher func(*html.Node) bool

type attribute struct {
	Name, Value string
}

func (proxy *ProxyHelper) element(name string, attrs ...attribute) matcher {
	return func(n *html.Node) bool {
		if n.Type != html.ElementNode || n.Data != name {
			return false
		}
		for _, a := range attrs {
			if proxy.getAttrValue(n, a.Name) != a.Value {
				return false
			}
		}
		return true
	}
}

func (proxy *ProxyHelper) all(n *html.Node, f matcher) []*html.Node {
	var nodes []*html.Node
	if f(n) {
		nodes = append(nodes, n)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		nodes = append(nodes, proxy.all(c, f)...)
	}
	return nodes
}

func (proxy *ProxyHelper) getAttrValue(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}
