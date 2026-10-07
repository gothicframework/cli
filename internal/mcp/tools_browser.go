package mcp

// tools_browser.go — the page-driving tools: navigate, act, probe,
// browser_mode. nil machinery is a clean tool error, never a panic.

import (
	"context"
	"fmt"
	"strings"

	"github.com/gothicframework/cli/v4/internal/browser"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ── navigate ───────────────────────────────────────────────────────────────

type navigateIn struct {
	URL string `json:"url" jsonschema:"the dev-origin URL to open (allowlist-gated)"`
}

type navigateOut struct {
	OK  bool   `json:"ok"`
	URL string `json:"url"`
}

func (s *surface) navigateTool(ctx context.Context, _ *mcp.CallToolRequest, in navigateIn) (*mcp.CallToolResult, navigateOut, error) {
	c, err := s.currentCap()
	if err != nil {
		return nil, navigateOut{}, err
	}
	b, err := c.needBrowser()
	if err != nil {
		return nil, navigateOut{}, err
	}
	if strings.TrimSpace(in.URL) == "" {
		return nil, navigateOut{}, fmt.Errorf("url is required")
	}
	if err := b.Open(in.URL); err != nil {
		return nil, navigateOut{}, fmt.Errorf("navigate to %s: %w", in.URL, err)
	}
	return nil, navigateOut{OK: true, URL: in.URL}, nil
}

// ── act ────────────────────────────────────────────────────────────────────

type actStep struct {
	Action   string `json:"action" jsonschema:"one of goto, click, fill, hover, scroll, key, select"`
	Selector string `json:"selector,omitempty" jsonschema:"CSS selector for click/fill/hover/select"`
	Value    string `json:"value,omitempty" jsonschema:"payload: text (fill), URL (goto), key names (key), option values comma-separated (select), scroll form (scroll: top|up|down|px)"`
}

type actIn struct {
	Steps []actStep `json:"steps" jsonschema:"actions to run in order"`
}

type actOut struct {
	OK    bool `json:"ok"`
	Steps int  `json:"steps"`
}

func (s *surface) actTool(ctx context.Context, _ *mcp.CallToolRequest, in actIn) (*mcp.CallToolResult, actOut, error) {
	c, err := s.currentCap()
	if err != nil {
		return nil, actOut{}, err
	}
	b, err := c.needBrowser()
	if err != nil {
		return nil, actOut{}, err
	}
	if len(in.Steps) == 0 {
		return nil, actOut{}, fmt.Errorf("steps is required (at least one action)")
	}
	steps := make([]browser.Step, 0, len(in.Steps))
	for _, st := range in.Steps {
		steps = append(steps, browser.Step{Action: st.Action, Selector: st.Selector, Value: st.Value})
	}
	if err := b.Act(ctx, steps); err != nil {
		return nil, actOut{}, err
	}
	return nil, actOut{OK: true, Steps: len(steps)}, nil
}

// ── probe ──────────────────────────────────────────────────────────────────

type probeIn struct {
	Expr string `json:"expr,omitempty" jsonschema:"a rod-style JS function expression (\"() => …\") returning a JSON-serializable value; empty = the standard page-state probe"`
}

type probeOut struct {
	OK    bool   `json:"ok"`
	Value string `json:"value,omitempty" jsonschema:"the JSON-serialized result"`
}

// probeDefaultJS is the standard page-state probe: identity, readiness, and
// the DOM shape an agent needs before deciding what to act on.
const probeDefaultJS = `() => JSON.stringify({
  url: location.href,
  title: document.title,
  readyState: document.readyState,
  scroll: [window.scrollX, window.scrollY],
  viewport: [window.innerWidth, window.innerHeight],
  counts: {
    elements: document.querySelectorAll("*").length,
    buttons: document.querySelectorAll("button, [role=button]").length,
    inputs: document.querySelectorAll("input, textarea, select").length,
    links: document.querySelectorAll("a[href]").length,
    images: document.querySelectorAll("img").length
  }
})`

func (s *surface) probeTool(ctx context.Context, _ *mcp.CallToolRequest, in probeIn) (*mcp.CallToolResult, probeOut, error) {
	c, err := s.currentCap()
	if err != nil {
		return nil, probeOut{}, err
	}
	b, err := c.needBrowser()
	if err != nil {
		return nil, probeOut{}, err
	}
	expr := strings.TrimSpace(in.Expr)
	if expr == "" {
		expr = probeDefaultJS
	}
	if !strings.HasPrefix(expr, "() =>") {
		return nil, probeOut{}, fmt.Errorf("expr must be a rod-style function expression starting with \"() =>\"")
	}
	val, err := b.Eval(expr)
	if err != nil {
		return nil, probeOut{}, fmt.Errorf("probe: %w", err)
	}
	return nil, probeOut{OK: true, Value: val}, nil
}

// ── browser_mode ───────────────────────────────────────────────────────────

type browserModeIn struct {
	Headless bool `json:"headless" jsonschema:"true = headless, false = headful (with a window)"`
}

type browserModeOut struct {
	OK       bool `json:"ok"`
	Headless bool `json:"headless"`
	Running  bool `json:"running"`
}

func (s *surface) browserModeTool(ctx context.Context, _ *mcp.CallToolRequest, in browserModeIn) (*mcp.CallToolResult, browserModeOut, error) {
	c, err := s.currentCap()
	if err != nil {
		return nil, browserModeOut{}, err
	}
	b, err := c.needBrowser()
	if err != nil {
		return nil, browserModeOut{}, err
	}
	if err := b.SetHeadless(in.Headless); err != nil {
		return nil, browserModeOut{}, err
	}
	return nil, browserModeOut{OK: true, Headless: in.Headless, Running: b.Running()}, nil
}

// ── set_viewport ───────────────────────────────────────────────────────────

type setViewportIn struct {
	Width  int  `json:"width,omitempty" jsonschema:"viewport width in pixels; 0 with clear leaves the browser unpinned"`
	Height int  `json:"height,omitempty" jsonschema:"viewport height in pixels; 0 keeps the page's current height"`
	Mobile bool `json:"mobile,omitempty" jsonschema:"emulate a mobile device (viewport meta tag, overlay scrollbars, text autosizing)"`
	Clear  bool `json:"clear,omitempty" jsonschema:"true = clear any pinned viewport"`
}

type setViewportOut struct {
	OK     bool   `json:"ok"`
	Pinned string `json:"pinned,omitempty" jsonschema:"the pinned override (e.g. 375x812 mobile), or the clear marker"`
}

func (s *surface) setViewportTool(ctx context.Context, _ *mcp.CallToolRequest, in setViewportIn) (*mcp.CallToolResult, setViewportOut, error) {
	c, err := s.currentCap()
	if err != nil {
		return nil, setViewportOut{}, err
	}
	b, err := c.needBrowser()
	if err != nil {
		return nil, setViewportOut{}, err
	}
	if in.Clear || in.Width <= 0 {
		if err := b.ClearViewport(); err != nil {
			return nil, setViewportOut{}, err
		}
		return nil, setViewportOut{OK: true, Pinned: "(cleared)"}, nil
	}
	if err := b.SetViewport(in.Width, in.Height, in.Mobile); err != nil {
		return nil, setViewportOut{}, err
	}
	label := fmt.Sprintf("%dx%d", in.Width, in.Height)
	if in.Mobile {
		label += " mobile"
	}
	return nil, setViewportOut{OK: true, Pinned: label}, nil
}
