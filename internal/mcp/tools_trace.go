package mcp

// tools_trace.go — trace and logs: the two read views of the session's
// observation timeline. trace serves the app's server-side request trace
// (reads the middlewares tracer's endpoint over HTTP);
// logs serves the dev bus (the proxy's decoded topic/durable/control/mark
// traffic) plus the managed page's retained console errors/warnings.
// Both are newest-first; the bus and trace planes answer from the same
// TimelinePort.

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ── trace ──────────────────────────────────────────────────────────────────

type traceIn struct {
	Last int `json:"last,omitempty" jsonschema:"how many records (default 20, capped at 200)"`
}

type traceOut struct {
	OK      bool            `json:"ok"`
	Count   int             `json:"count"`
	Records []timelineEntry `json:"records" jsonschema:"trace records, newest first"`
	Note    string          `json:"note,omitempty"`
}

func (s *surface) traceTool(ctx context.Context, _ *mcp.CallToolRequest, in traceIn) (*mcp.CallToolResult, traceOut, error) {
	c, err := s.currentCap()
	if err != nil {
		return nil, traceOut{}, err
	}
	tl, err := c.needTimeline()
	if err != nil {
		return nil, traceOut{}, err
	}
	last := boundedLast(in.Last, 20)
	records, err := tl.Trace(ctx, last)
	if err != nil {
		return nil, traceOut{}, fmt.Errorf("read the request trace: %w", err)
	}
	out := traceOut{OK: true, Count: len(records)}
	for _, rec := range records {
		out.Records = append(out.Records, timelineEntry{Source: "trace", Record: rec})
	}
	return nil, out, nil
}

// ── logs ───────────────────────────────────────────────────────────────────

type logsIn struct {
	Last int `json:"last,omitempty" jsonschema:"how many records (default 20, capped at 200)"`
}

type logsOut struct {
	OK      bool            `json:"ok"`
	Count   int             `json:"count"`
	Records []timelineEntry `json:"records" jsonschema:"dev-bus records (topic|durable|control|mark), newest first"`
	// Console is the managed page's retained error/warning console messages,
	// newest first; absent when the session has no browser or the page
	// emitted none.
	Console []consoleRecord `json:"console,omitempty"`
	Note    string          `json:"note,omitempty"`
}

// consoleRecord is one retained page-console message.
type consoleRecord struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	TimestampMs int64  `json:"timestamp_ms"`
}

func (s *surface) logsTool(_ context.Context, _ *mcp.CallToolRequest, in logsIn) (*mcp.CallToolResult, logsOut, error) {
	c, err := s.currentCap()
	if err != nil {
		return nil, logsOut{}, err
	}
	tl, err := c.needTimeline()
	if err != nil {
		return nil, logsOut{}, err
	}
	last := boundedLast(in.Last, 20)
	records, err := tl.Bus(last)
	if err != nil {
		return nil, logsOut{}, fmt.Errorf("read the dev bus: %w", err)
	}
	out := logsOut{OK: true, Count: len(records)}
	for _, rec := range records {
		out.Records = append(out.Records, timelineEntry{Source: "bus", Record: rec})
	}
	// The page's retained console messages ride along when a managed browser
	// exists; a session without one simply omits the plane.
	if b, err := c.needBrowser(); err == nil {
		for _, e := range b.Console(last) {
			out.Console = append(out.Console, consoleRecord{Type: e.Type, Text: e.Text, TimestampMs: e.TimestampMs})
		}
	}
	return nil, out, nil
}

// boundedLast clamps a ?last-style count: 0 → default, negatives → default,
// everything above the cap → the cap.
func boundedLast(n, def int) int {
	if n <= 0 {
		return def
	}
	const cap = 200
	if n > cap {
		return cap
	}
	return n
}

// timelineEntry is one record of the observation timeline with its plane
// named (so a merged read is self-describing).
type timelineEntry struct {
	Source string         `json:"source"` // "trace" | "bus"
	Record map[string]any `json:"record"`
}
