// Package mcp hosts the Gothic dev session's MCP surface over Streamable HTTP.
//
// The dev proxy process serves it at /_gothicframework/mcp (default-on; the
// hot-reload command mounts it and --no-mcp opts out). The surface is the
// machine port of the framework's own tooling: the managed browser (view,
// act, record), the build pipeline (build_*, sync, build_status), the request
// trace and the dev bus (trace, logs), and the bundled skill documents
// (skillsearch/skillinfo/skill, also reachable as gothic-skill:// resources).
//
// Results stay small: heavy content (skill document bodies, page-view grids,
// take frame lists) is returned only on an explicit named request.
//
// Concurrency: every tool serializes on the machinery's own locks (the
// browser Manager, the build Controller); the surface adds one mutex only for
// its own small per-session state (the diff baseline, the last take).
package mcp

import (
	"fmt"
	"net/http"
	"sync"

	"github.com/gothicframework/cli/v4/internal/browser"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPEndpointPath is where the dev proxy mux serves the MCP endpoint.
const MCPEndpointPath = "/_gothicframework/mcp"

// The contract surface's tool names. Tests assert on this exact set.
const (
	toolNavigate     = "navigate"
	toolAct          = "act"
	toolView         = "view"
	toolZoom         = "zoom"
	toolDiff         = "diff"
	toolCompare      = "compare"
	toolRecordStart  = "record_start"
	toolRecordStop   = "record_stop"
	toolRecordMark   = "record_mark"
	toolRecordFrames = "record_frames"
	toolProbe        = "probe"
	toolTrace        = "trace"
	toolLogs         = "logs"
	toolAudit        = "audit"
	toolBuildTempl   = "build_templ"
	toolBuildCSS     = "build_css"
	toolBuildWasm    = "build_wasm"
	toolBuildGo      = "build_go"
	toolSync         = "sync"
	toolBuildStatus  = "build_status"
	toolSkillSearch  = "skillsearch"
	toolSkillInfo    = "skillinfo"
	toolSkill        = "skill"
	toolBrowserMode  = "browser_mode"
	toolSetViewport  = "set_viewport"
)

// Capability is the machinery one dev session exposes to MCP clients. The
// hot-reload command builds it once over its real subsystems; tests build
// fakes. Nil members make the matching tools fail with a clean tool error
// instead of a panic.
type Capability struct {
	// Browser is the managed browser (nil when the session has none).
	Browser BrowserPort

	// Build is the build controller (nil when the session has none).
	Build BuildPort

	// Timeline reads the observation timeline (server trace + dev bus).
	Timeline TimelinePort

	// Skills serves the bundled skill documents.
	Skills SkillsPort

	// MarkSink injects a named mark into the dev bus (nil disables record_mark).
	MarkSink MarkSink

	// Marks pulls the act marks out of the dev bus ring (newest first) — the
	// closure the recording takes run on. Production binds it to
	// browser.MarksFromRecords over the proxy's ring; nil falls back to the
	// Timeline port's Bus read.
	Marks func() []browser.TakeMark

	// Engine is the managed browser's engine label reported in page-view
	// payloads (e.g. "chromium 131"); empty when unknown.
	Engine string

	// Version is the CLI version reported as the MCP server's version
	// (passed in — the cmd package owns the constant).
	Version string
}

// surface is one MCP server instance: the capability factory plus the small
// per-session state the diff/record tools keep.
type surface struct {
	factory func() *Capability

	mu           sync.Mutex
	baseline     *baselineState        // the diff tool's stored still
	lastTake     TakePort              // the record tools' last handle
	lastManifest *browser.TakeManifest // the last finalized take manifest
}

// buildServer constructs the MCP server instance (tools + resources). The
// HTTP handler and the in-memory test transports both build through it.
func buildServer(factory func() *Capability) *mcp.Server {
	s := &surface{factory: factory}
	srv := mcp.NewServer(
		&mcp.Implementation{Name: "gothic", Version: factory().Version},
		&mcp.ServerOptions{
			Instructions: "Gothic Framework dev session. view/zoom = the page as " +
				"a text map; act/navigate = drive the page; record_* = screencast " +
				"takes; build_* = the build pipeline; skillsearch/skillinfo/skill = " +
				"bundled framework docs; trace/logs = the observation timeline.",
		},
	)
	s.registerTools(srv)
	s.registerResources(srv)
	return srv
}

// Handler builds the whole MCP surface as an http.Handler: one server, every
// contract tool, the skill resources, and Streamable HTTP with plain-JSON
// responses (the dev server's own traffic pattern: one request, one answer).
func Handler(factory func() *Capability) http.Handler {
	srv := buildServer(factory)
	// StreamableHTTPHandler applies origin verification only behind an env
	// opt-in; the dev proxy sets no CORS headers on this path, so without an
	// explicit protector a DNS-rebounded web page could same-origin-POST the
	// loopback dev session. The standard protector allows same-origin and
	// non-browser clients, exactly the endpoint's legitimate callers.
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return srv
	}, &mcp.StreamableHTTPOptions{
		JSONResponse:          true,
		CrossOriginProtection: http.NewCrossOriginProtection(),
	})
}

// currentCap resolves the session's machinery for one request. A nil factory
// result is a clean error; nil members are checked per tool.
func (s *surface) currentCap() (*Capability, error) {
	if s.factory == nil {
		return nil, fmt.Errorf("the dev session's MCP capability is not available")
	}
	c := s.factory()
	if c == nil {
		return nil, fmt.Errorf("the dev session's MCP capability is not available")
	}
	return c, nil
}

// needBrowser/needBuild/needSkills/needTimeline unwrap the capability member
// with the matching tool error.
func (c *Capability) needBrowser() (BrowserPort, error) {
	if c.Browser == nil {
		return nil, fmt.Errorf("the managed browser is not available in this session")
	}
	return c.Browser, nil
}

func (c *Capability) needBuild() (BuildPort, error) {
	if c.Build == nil {
		return nil, fmt.Errorf("the build controller is not available in this session")
	}
	return c.Build, nil
}

func (c *Capability) needSkills() (SkillsPort, error) {
	if c.Skills == nil {
		return nil, fmt.Errorf("the skill index is not available in this session")
	}
	return c.Skills, nil
}

func (c *Capability) needTimeline() (TimelinePort, error) {
	if c.Timeline == nil {
		return nil, fmt.Errorf("the observation timeline is not available in this session")
	}
	return c.Timeline, nil
}

// registerTools registers every contract tool with its annotations.
func (s *surface) registerTools(srv *mcp.Server) {
	// ── browser interaction (drive the page) ────────────────────────────
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolNavigate,
		Description: "Navigate the managed dev browser to a URL on the dev origin (allowlist-gated).",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: false, DestructiveHint: boolPtr(false)},
	}, s.navigateTool)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolAct,
		Description: "Run scripted actions on the page — click/fill/hover/scroll/key/select/goto, in order, by CSS selector.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: false, DestructiveHint: boolPtr(false)},
	}, s.actTool)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolProbe,
		Description: "Run one read-style JS expression in the page (default: a page-state probe) and return the JSON result. Only read-style expressions belong here; act/record_* exist for changes.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.probeTool)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolBrowserMode,
		Description: "Switch the managed browser between headless and headful (relaunches it; the profile survives).",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: boolPtr(false)},
	}, s.browserModeTool)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolSetViewport,
		Description: "Pin the managed browser's viewport (e.g. a 375px mobile width) for every subsequent probe/act/view, or clear the pin. The pin holds across navigations until cleared.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: boolPtr(false)},
	}, s.setViewportTool)

	// ── page observation (read-only) ────────────────────────────────────
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolView,
		Description: "Screenshot the page at one or more viewport widths and encode the page-view text map. Paths of the on-disk artifacts are included; the grid payload only when you pass include_text (it is the heavy form).",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.viewTool)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolZoom,
		Description: "Zoom into one region of the page by CSS selector: the crop rastered at full resolution as a page-view payload.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.zoomTool)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolDiff,
		Description: "Diff the current render against the last stored baseline. The first call stores the baseline; later calls report the changed cells.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
	}, s.diffTool)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolCompare,
		Description: "Capture the page at two viewport widths and compare the two text maps cell by cell.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.compareTool)

	// ── recording ───────────────────────────────────────────────────────
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolRecordStart,
		Description: "Start a screencast take over the managed browser (auto-stops on settle or after the budget). Returns the take id.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: false, DestructiveHint: boolPtr(false)},
	}, s.recordStartTool)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolRecordStop,
		Description: "Stop the active take and return its timeline manifest summary.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: false, DestructiveHint: boolPtr(false)},
	}, s.recordStopTool)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolRecordMark,
		Description: "Inject a named mark into the dev bus, timestamped into the active take's timeline.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: false, DestructiveHint: boolPtr(false)},
	}, s.recordMarkTool)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolRecordFrames,
		Description: "Read the last take's timeline: frames, deltas, keyframes, marks. Frame file paths only on request (the heavy form).",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.recordFramesTool)

	// ── observation timeline (read-only) ────────────────────────────────
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolTrace,
		Description: "Read the framework's server-side request trace (per-request stages, redacted), newest first.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.traceTool)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolLogs,
		Description: "Read the dev bus — topic traffic, durable state, control-plane events, act marks — newest first.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.logsTool)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolAudit,
		Description: "Run the bounded audit: web-vitals metrics (FCP/LCP/CLS/TBT/INP), DOM/SEO checks, and axe accessibility rules. Findings only.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.auditTool)

	// ── build pipeline ──────────────────────────────────────────────────
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolBuildTempl,
		Description: "Run the templ generate stage (all dirty files, or one named file).",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: boolPtr(false)},
	}, s.buildTemplTool)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolBuildCSS,
		Description: "Run the Tailwind CSS build.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: boolPtr(false)},
	}, s.buildCSSTool)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolBuildWasm,
		Description: "Run the WASM stage (all dirty units; an optional target scopes the report).",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: boolPtr(false)},
	}, s.buildWasmTool)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolBuildGo,
		Description: "Compile the app server binary.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: boolPtr(false)},
	}, s.buildGoTool)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolSync,
		Description: "Bring every generated artifact up to date in pipeline order (no app build).",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: boolPtr(false)},
	}, s.syncTool)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolBuildStatus,
		Description: "Report fresh/stale/mid-rebuild per build artifact.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.buildStatusTool)

	// ── skills (read-only; heavy content only on skill) ─────────────────
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolSkillSearch,
		Description: "Search the bundled Gothic skill documents by name/description substring.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.skillSearchTool)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolSkillInfo,
		Description: "Front matter (name, description, applies_to version contract) of the named skills.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.skillInfoTool)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolSkill,
		Description: "Load full skill documents by name. Without names: only the name list. This is the one heavy-content skill request.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.skillTool)
}

// boolPtr is a helper for the pointer-typed MCP hints.
func boolPtr(b bool) *bool { return &b }

// summary is the small envelope every tool result uses: ok, plus a machine
// and a human summary. Tools embed it as their first fields.
type summary struct {
	OK      bool   `json:"ok"`
	Summary string `json:"summary,omitempty"`
}
