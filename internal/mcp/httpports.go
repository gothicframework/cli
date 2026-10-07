package mcp

// httpports.go — the production TimelinePort: both observation planes are
// HTTP endpoints served by the dev processes (the app server's trace endpoint
// and the proxy's dev-bus endpoint), reached through the dev proxy's own
// origin. The MCP handler runs inside the CLI process, so a loopback read of
// its own proxy is the natural reader — the ring's internals stay in the
// proxy package.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// HTTPBaseTimeline reads the observation timeline over the dev proxy's own
// HTTP surface. Construct it with the proxy's base URL
// (e.g. "http://127.0.0.1:3000").
type HTTPBaseTimeline struct {
	// Base is the dev proxy's origin.
	Base string

	// Client is the HTTP client (zero value = a 5s-timeout default).
	Client *http.Client
}

func (t HTTPBaseTimeline) client() *http.Client {
	if t.Client != nil {
		return t.Client
	}
	return &http.Client{Timeout: 5 * time.Second}
}

// TracePort — GET /_gothicframework/trace?last=N (the app's tracer endpoint,
// passed through by the proxy).
func (t HTTPBaseTimeline) Trace(ctx context.Context, last int) ([]map[string]any, error) {
	raw, err := t.get(ctx, "/_gothicframework/trace?last="+strconv.Itoa(last))
	if err != nil {
		return nil, err
	}
	var records []map[string]any
	if err := json.Unmarshal(raw, &records); err != nil {
		return nil, fmt.Errorf("decoding the trace endpoint's JSON: %w", err)
	}
	return records, nil
}

// LogsPort — GET /_gothicframework/reload/bus?last=N (the dev bus ring's
// read endpoint).
func (t HTTPBaseTimeline) Bus(last int) ([]map[string]any, error) {
	raw, err := t.get(context.Background(), "/_gothicframework/reload/bus?last="+strconv.Itoa(last))
	if err != nil {
		return nil, err
	}
	var payload struct {
		Records []map[string]any `json:"records"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("decoding the bus endpoint's JSON: %w", err)
	}
	return payload.Records, nil
}

// get performs one bounded GET against the dev proxy.
func (t HTTPBaseTimeline) get(ctx context.Context, path string) ([]byte, error) {
	base := strings.TrimSuffix(t.Base, "/")
	if _, err := url.Parse(base + path); err != nil {
		return nil, fmt.Errorf("invalid timeline URL %q: %w", base+path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := t.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("%s %s: HTTP %d (%s)", http.MethodGet, path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return data, nil
}
