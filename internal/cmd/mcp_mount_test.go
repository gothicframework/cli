package cmd

// mcp_mount_test.go — the mount behavior: the endpoint answers with the real
// MCP server by default, answers a deterministic 404 with --no-mcp, and the
// capability wires the take marks to the dev bus ring (one timeline for the
// injected script's inputs and tool-injected marks).

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gothic_cli "github.com/gothicframework/cli/v4/internal/cli"
	"github.com/gothicframework/cli/v4/internal/mcp"
	"github.com/gothicframework/cli/v4/internal/proxy"
)

// mcpRequest is the shared JSON-RPC shape for the streamable HTTP endpoint.
func mcpRequestBody(method string, params any) []byte {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	})
	return body
}

func doMCPRequest(t *testing.T, px *proxy.ProxyHelper, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, mcp.MCPEndpointPath, bytes.NewReader(body))
	// The MCP streamable transport negotiates by Accept: both media types are
	// mandatory per the transport's request validation.
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rr := httptest.NewRecorder()
	px.ServeHTTP(rr, req)
	return rr
}

func TestMountMCPSurfaceOn(t *testing.T) {
	cli := gothic_cli.NewCli()
	command := newHotReloadCommandCli(&cli)
	command.noMCP = false
	command.mountMCPSurface()

	rr := doMCPRequest(t, &cli.Proxy, mcpRequestBody("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "gothic-mcp-test", "version": "0"},
	}))
	if rr.Code != http.StatusOK {
		t.Fatalf("initialize answered HTTP %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "\"gothic\"") {
		t.Errorf("initialize response does not name the gothic dev server: %s", rr.Body.String())
	}
}

func TestMountMCPSurfaceOffAnswers404(t *testing.T) {
	cli := gothic_cli.NewCli()
	command := newHotReloadCommandCli(&cli)
	command.noMCP = true
	command.mountMCPSurface()

	// The network/transport layer of MCP expects POST; either way, the
	// endpoint must answer 404 WITHOUT reaching any backend.
	rr := doMCPRequest(t, &cli.Proxy, mcpRequestBody("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "gothic-mcp-test", "version": "0"},
	}))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("--no-mcp must answer 404, got HTTP %d: %s", rr.Code, rr.Body.String())
	}

	// A GET behaves the same.
	req := httptest.NewRequest(http.MethodGet, mcp.MCPEndpointPath, nil)
	rr2 := httptest.NewRecorder()
	cli.Proxy.ServeHTTP(rr2, req)
	if rr2.Code != http.StatusNotFound {
		t.Errorf("--no-mcp GET must answer 404, got HTTP %d", rr2.Code)
	}
}

// TestCapabilityMarksFromProxyBus pins the wiring of the take's act marks to
// the dev bus ring: the capability's Marks puller reads the ring, and the
// ring is shared by the injected script's POSTs AND tool-injected marks.
func TestCapabilityMarksFromProxyBus(t *testing.T) {
	cli := gothic_cli.NewCli()
	command := newHotReloadCommandCli(&cli)
	command.noMCP = false

	// A mark observed by the injected script rides the bus ingest endpoint
	// handler; the direct ingest is the same ring the endpoint writes.
	cli.Proxy.Ingest([]map[string]any{{
		"t": float64(1000), "kind": "mark", "dir": "act",
		"event": "click", "payload": map[string]any{"sel": "#save"},
	}})

	cap := command.mcpCapability()
	if cap.Marks == nil {
		t.Fatal("capability.Marks is nil; take marks would never reach RecordOptions")
	}
	marks := cap.Marks()
	if len(marks) != 1 || marks[0].Event != "click" {
		t.Fatalf("Marks() = %+v, want the one bus mark", marks)
	}
	if marks[0].T != 1000 {
		t.Errorf("mark t = %d, want 1000 (the bus record's timestamp)", marks[0].T)
	}

	// A tool-injected mark lands on the same ring (newest first) and is
	// readable exactly like a script-observed input.
	if err := cap.MarkSink("mcp:before-submit", map[string]any{"sel": "#save"}); err != nil {
		t.Fatalf("MarkSink: %v", err)
	}
	marks = cap.Marks()
	var seen bool
	var prevT int64 = -1
	for _, m := range marks {
		if m.Event == "mcp:before-submit" {
			seen = true
		}
		if prevT >= 0 && m.T > prevT {
			t.Errorf("marks not newest first: %v", marks)
		}
		prevT = m.T
	}
	if len(marks) != 2 || !seen {
		t.Errorf("Marks() after a tool-injected mark = %+v", marks)
	}
}
