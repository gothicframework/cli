package mcp

// ports.go — the seams between the MCP layer and the machinery it drives.
//
// The machinery packages (browser, buildctl, skills, the dev-proxy bus, the
// app's trace endpoint) define their own concrete types; the MCP layer
// consumes small interfaces so tests can stub the whole backend without
// launching a browser or running a build. The concrete packages satisfy these
// interfaces implicitly — browser's Step/StillOptions/RecordOptions are plain
// structs, safe to name in tests without launching rod.

import (
	"context"

	"github.com/gothicframework/cli/v3/internal/browser"
	"github.com/gothicframework/cli/v3/internal/buildctl"
	"github.com/gothicframework/cli/v3/internal/skills"
)

// BrowserPort is the managed-browser surface the tools use.
// NewBrowserPort adapts cli/internal/browser.Manager (whose StartRecording
// returns the concrete *Take) to the interface.
type BrowserPort interface {
	Open(url string) error
	Act(ctx context.Context, steps []browser.Step) error
	CaptureStill(ctx context.Context, opts browser.StillOptions) ([]browser.StillResult, error)
	StartRecording(opts browser.RecordOptions) (TakePort, error)
	ActiveTake() TakePort
	SetHeadless(headless bool) error
	// SetViewport pins a device-metrics override on the managed page until
	// ClearViewport; height 0 keeps the page's current height.
	SetViewport(width, height int, mobile bool) error
	ClearViewport() error
	// Console returns the page's retained console errors/warnings, newest
	// first; last <= 0 returns the whole ring.
	Console(last int) []browser.ConsoleEntry
	Running() bool
	// Eval runs one rod-style JS function expression ("() => …") in the page
	// and returns its value serialized. Promise results are awaited.
	Eval(js string) (string, error)
}

// TakePort is the recording handle the record tools drive. *browser.Take
// implements it exactly.
type TakePort interface {
	Stop(discard bool) (browser.TakeManifest, error)
	Wait() (browser.TakeManifest, error)
	Done() <-chan struct{}
}

// BuildPort is the build-pipeline surface the tools use.
// cli/internal/buildctl.Controller implements it exactly.
type BuildPort interface {
	BuildTempl(file string) buildctl.BuildResult
	BuildRoutes() buildctl.BuildResult
	BuildTopics() buildctl.BuildResult
	BuildWasm(target string) buildctl.BuildResult
	BuildGo() buildctl.BuildResult
	BuildCSS(page string) buildctl.BuildResult
	Sync() buildctl.BuildResult
	BuildStatus() buildctl.StatusResult
}

// SkillsPort serves the bundled skill documents.
// cli/internal/skills.Set implements it exactly.
type SkillsPort interface {
	Search(query string) []skills.Ref
	Info(names []string) []skills.SkillInfo
	Load(names []string) []skills.Document
	Names() []string
}

// TimelinePort reads the session's observation timeline from both planes: the
// app's server-side request trace and the dev bus (topic/durable/control/mark
// traffic decoded by the injected observer). Production wires both planes to
// HTTP reads over the dev proxy; tests stub them in memory.
type TimelinePort interface {
	// Trace returns the newest N server-side trace records.
	Trace(ctx context.Context, last int) ([]map[string]any, error)
	// Bus returns the newest N dev-bus records, newest first.
	Bus(last int) ([]map[string]any, error)
}

// MarkSink injects one named mark into the dev bus (a kind:"mark" record), so
// the active take's marks poller and every record reader see it on the same
// timeline. Production posts to the proxy's bus ingest endpoint.
type MarkSink func(event string, payload map[string]any) error

// browserPort adapts *browser.Manager to BrowserPort: the adapter exists only
// because Manager.StartRecording returns the concrete *Take type.
type browserPort struct{ m *browser.Manager }

// NewBrowserPort adapts a managed browser Manager to the BrowserPort.
func NewBrowserPort(m *browser.Manager) BrowserPort { return browserPort{m: m} }

func (a browserPort) Open(url string) error { return a.m.Open(url) }

func (a browserPort) Act(ctx context.Context, steps []browser.Step) error {
	return a.m.Act(ctx, steps)
}

func (a browserPort) CaptureStill(ctx context.Context, opts browser.StillOptions) ([]browser.StillResult, error) {
	return a.m.CaptureStill(ctx, opts)
}

func (a browserPort) StartRecording(opts browser.RecordOptions) (TakePort, error) {
	return a.m.StartRecording(opts)
}

// ActiveTake resolves the manager's concrete *Take to the interface, with
// the nil *Take unwrapped to a nil interface so callers' nil checks work.
func (a browserPort) ActiveTake() TakePort {
	t := a.m.ActiveTake()
	if t == nil {
		return nil
	}
	return t
}

func (a browserPort) SetHeadless(headless bool) error { return a.m.SetHeadless(headless) }

func (a browserPort) SetViewport(width, height int, mobile bool) error {
	return a.m.SetViewport(width, height, mobile)
}

func (a browserPort) ClearViewport() error { return a.m.ClearViewport() }

func (a browserPort) Console(last int) []browser.ConsoleEntry { return a.m.Console(last) }

func (a browserPort) Running() bool { return a.m.Running() }

func (a browserPort) Eval(js string) (string, error) { return a.m.Eval(js) }
