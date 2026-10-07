package mcp

// The in-memory round trip tests: every contract tool is called once through
// a real MCP client over NewInMemoryTransports, against fake backends — no
// browser, no build, no HTTP server. The resource plane reads the embedded
// skill documents the same way.

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gothicframework/cli/v4/internal/browser"
	"github.com/gothicframework/cli/v4/internal/buildctl"
	"github.com/gothicframework/cli/v4/internal/skills"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ── fakes ──────────────────────────────────────────────────────────────────

// fakeTake implements TakePort with a canned manifest. Like the real Take,
// it deactivates itself once stopped so ActiveTake-driven flows behave.
type fakeTake struct {
	manifest browser.TakeManifest
	stopped  bool
	dropped  bool
}

func (t *fakeTake) Stop(discard bool) (browser.TakeManifest, error) {
	t.stopped = true
	t.dropped = discard
	m := t.manifest
	m.Discarded = discard
	return m, nil
}

func (t *fakeTake) Wait() (browser.TakeManifest, error) { return t.manifest, nil }

func (t *fakeTake) Done() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// TakeID/TakeDir name the fake for the tools' id/dir unwrapping.
func (t *fakeTake) TakeID() string { return t.manifest.ID }
func (t *fakeTake) TakeDir() string {
	return "rec_" + t.manifest.ID
}

// fakeBrowser implements BrowserPort: canned stills (real PNG + manifest
// files in a temp dir), a canned take, recorded calls, and an Eval dispatch.
type fakeBrowser struct {
	calls []string

	stillErr  error
	recordErr error

	take   *fakeTake
	active *fakeTake

	headless   bool
	running    bool
	projDir    string
	captureNum int
	// pageURL is the URL written into every captured manifest (the test rigs
	// point it at a local test server so the audit's HTML fetch works).
	pageURL string

	// evalRules dispatch Eval by substring, first match wins.
	evalRules []fakeEvalRule

	// viewportErr makes SetViewport/ClearViewport fail; vpW/vpH/mobile
	// record the last pinned override.
	viewportErr error
	vpW, vpH    int
	mobile      bool

	// console is the retained page console, newest first (the Manager's
	// order).
	console []browser.ConsoleEntry
}

type fakeEvalRule struct{ needle, result string }

func (f *fakeBrowser) Open(url string) error {
	f.calls = append(f.calls, "open:"+url)
	return nil
}

func (f *fakeBrowser) Act(_ context.Context, steps []browser.Step) error {
	for _, s := range steps {
		f.calls = append(f.calls, "act:"+s.Action)
	}
	return nil
}

func (f *fakeBrowser) CaptureStill(_ context.Context, opts browser.StillOptions) ([]browser.StillResult, error) {
	if f.stillErr != nil {
		return nil, f.stillErr
	}
	f.calls = append(f.calls, fmt.Sprintf("still:%v", opts.Widths))
	f.captureNum++
	widths := opts.Widths
	if len(widths) == 0 {
		widths = []int{800}
	}
	out := make([]browser.StillResult, 0, len(widths))
	for i, w := range widths {
		// One raster shape per still: alternate widths inside one capture
		// (the compare tool), and alternate captures for a single width
		// (the diff tool).
		var alternate bool
		if len(widths) > 1 {
			alternate = i%2 == 1
		} else {
			alternate = f.captureNum%2 == 0
		}
		pngPath := filepath.Join(f.projDir, fmt.Sprintf("still_%d_%d.png", w, f.captureNum))
		if err := writeTestPNG(pngPath, alternate); err != nil {
			return nil, err
		}
		manifestPath := filepath.Join(f.projDir, fmt.Sprintf("still_%d_%d.manifest.json", w, f.captureNum))
		if err := os.WriteFile(manifestPath, []byte(testManifestAt(f.pageURL)), 0o644); err != nil {
			return nil, err
		}
		out = append(out, browser.StillResult{
			Width:        w,
			Height:       60,
			PNGPath:      pngPath,
			ManifestPath: manifestPath,
			URL:          f.pageURL,
			Scroll:       [2]float64{0, 0},
		})
	}
	return out, nil
}

func (f *fakeBrowser) StartRecording(_ browser.RecordOptions) (TakePort, error) {
	if f.recordErr != nil {
		return nil, f.recordErr
	}
	f.calls = append(f.calls, "record:start")
	f.active = f.take
	return f.take, nil
}

func (f *fakeBrowser) ActiveTake() TakePort {
	if f.active == nil || f.active.stopped {
		return nil
	}
	return f.active
}

func (f *fakeBrowser) SetHeadless(headless bool) error {
	f.calls = append(f.calls, fmt.Sprintf("headless:%v", headless))
	f.headless = headless
	return nil
}

func (f *fakeBrowser) Running() bool { return f.running }

func (f *fakeBrowser) Eval(expr string) (string, error) {
	for _, r := range f.evalRules {
		if strings.Contains(expr, r.needle) {
			return r.result, nil
		}
	}
	return testProbeJSON, nil
}

func (f *fakeBrowser) SetViewport(width, height int, mobile bool) error {
	f.calls = append(f.calls, fmt.Sprintf("viewport:%dx%d%v", width, height, mobile))
	f.vpW, f.vpH, f.mobile = width, height, mobile
	return f.viewportErr
}

func (f *fakeBrowser) ClearViewport() error {
	f.calls = append(f.calls, "viewport:clear")
	f.vpW, f.vpH, f.mobile = 0, 0, false
	return f.viewportErr
}

// Console returns the retained page console, newest first; last <= 0 returns
// the whole ring.
func (f *fakeBrowser) Console(last int) []browser.ConsoleEntry {
	if last > 0 && last < len(f.console) {
		return f.console[:last]
	}
	return f.console
}

// fakeBuilder implements BuildPort, recording which stage ran.
type fakeBuilder struct {
	last       string
	templFile  string
	wasmTarget string
}

func (f *fakeBuilder) BuildTempl(file string) buildctl.BuildResult {
	f.last = buildctl.StageTempl
	f.templFile = file
	return buildctl.BuildResult{OK: true, Stage: buildctl.StageTempl, MS: 5,
		Recompiled: []buildctl.Recompiled{{Path: "src/pages/index_templ.go", Bytes: 100}},
		NowStale:   []string{buildctl.StageRoutesGen},
	}
}

func (f *fakeBuilder) BuildRoutes() buildctl.BuildResult {
	f.last = buildctl.StageRoutesGen
	return buildctl.BuildResult{OK: true, Stage: buildctl.StageRoutesGen}
}

func (f *fakeBuilder) BuildTopics() buildctl.BuildResult {
	f.last = buildctl.StageTopicGen
	return buildctl.BuildResult{OK: true, Stage: buildctl.StageTopicGen}
}

func (f *fakeBuilder) BuildWasm(target string) buildctl.BuildResult {
	f.last = buildctl.StageWasm
	f.wasmTarget = target
	return buildctl.BuildResult{OK: true, Stage: buildctl.StageWasm, Counts: &buildctl.StageCounts{Rebuilt: 1}}
}

func (f *fakeBuilder) BuildGo() buildctl.BuildResult {
	f.last = buildctl.StageGo
	return buildctl.BuildResult{OK: true, Stage: buildctl.StageGo}
}

func (f *fakeBuilder) BuildCSS(page string) buildctl.BuildResult {
	f.last = buildctl.StageCSS
	return buildctl.BuildResult{OK: true, Stage: buildctl.StageCSS}
}

func (f *fakeBuilder) Sync() buildctl.BuildResult {
	f.last = buildctl.StageSync
	return buildctl.BuildResult{OK: true, Stage: buildctl.StageSync}
}

func (f *fakeBuilder) BuildStatus() buildctl.StatusResult {
	f.last = buildctl.StageSync
	return buildctl.StatusResult{Artifacts: []buildctl.StatusArtifact{
		{Name: buildctl.StageTempl, State: buildctl.StatusFresh, Detail: "all up to date"},
	}}
}

// fakeTimeline implements TimelinePort with one canned record per plane.
type fakeTimeline struct {
	traces []map[string]any
	recs   []map[string]any
	err    error
}

func (f *fakeTimeline) Trace(_ context.Context, _ int) ([]map[string]any, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.traces, nil
}

func (f *fakeTimeline) Bus(_ int) ([]map[string]any, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.recs, nil
}

// fakeMarkSink captures record_mark injections.
type fakeMarkSink struct{ events []string }

func (f *fakeMarkSink) Mark(event string, _ map[string]any) error {
	f.events = append(f.events, event)
	return nil
}

// ── shared fixtures ────────────────────────────────────────────────────────

const testPageURL = "http://127.0.0.1:60714/"

// testManifestAt renders the fixture manifest for one page URL.
func testManifestAt(pageURL string) string {
	return fmt.Sprintf(`{
 "scroll": [0, 0],
 "url": %q,
 "viewport": [256, 64],
 "rects": [
  {"sel": "div#hero", "x": 10, "y": 5, "w": 100, "h": 30, "bg": "rgb(0, 0, 0)", "fg": "rgb(255, 255, 255)", "overflows": false},
  {"sel": "p.lede", "x": 20, "y": 40, "w": 60, "h": 12, "bg": "", "fg": "rgb(10, 10, 10)", "overflows": false}
 ]
}`, pageURL)
}

// testAuditHTML is the page the rig's test HTTP server serves: enough
// structure for every DOM/SEO check to have a definite answer (one h1 FAILS
// the single-h1 rule by there being two, imgs lack alt).
const testAuditHTML = `<!doctype html>
<html lang="en">
<head>
 <meta charset="utf-8">
 <meta name="viewport" content="width=device-width, initial-scale=1">
 <meta name="description" content="The audit fixture page">
 <link rel="canonical" href="http://127.0.0.1:60714/">
 <title>Audit Fixture</title>
</head>
<body>
 <h1>Audit Fixture</h1>
 <h1>Duplicate heading</h1>
 <img src="/static/pic.png">
 <p class="lede">Text for the capture.</p>
</body>
</html>`

const testProbeJSON = `{"url":"http://127.0.0.1:60714/","title":"Home","readyState":"complete","counts":{"buttons":1}}`

// writeTestPNG renders one of two rasters: alternate=true paints the whole
// raster bright so a diff against the dark raster reports change.
func writeTestPNG(path string, alternate bool) error {
	img := image.NewNRGBA(image.Rect(0, 0, 256, 64))
	fg := color.RGBA{0x10, 0x10, 0x10, 0xff}
	if alternate {
		fg = color.RGBA{0xf0, 0xf0, 0xf0, 0xff}
	}
	for y := 0; y < 64; y++ {
		for x := 0; x < 256; x++ {
			img.Set(x, y, fg)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

// ── the test rig ───────────────────────────────────────────────────────────

// rig is one MCP server + connected in-memory client over fakes.
type rig struct {
	browser  *fakeBrowser
	builder  *fakeBuilder
	timeline *fakeTimeline
	marks    *fakeMarkSink
	skills   SkillsPort
	client   *mcp.ClientSession
}

// connect builds the server with the rig's fakes and wires a real client to
// it over in-memory transports.
func newRig(t *testing.T) *rig {
	t.Helper()
	dir := t.TempDir()

	b := &fakeBuilder{}
	fb := &fakeBrowser{
		projDir: dir,
		running: true,
		take: &fakeTake{manifest: browser.TakeManifest{
			ID:         "rec_20260102-150405-aa11",
			URL:        testPageURL,
			DurationMs: 2200,
			EndReason:  "settled",
			Settled:    true,
			Frames: []browser.FrameInfo{
				{File: "rec_1/frame_00001.jpg", T: 100, Delta: 3.5},
				{File: "rec_1/frame_00002.jpg", T: 300, Delta: 12.0, Keyframe: true},
			},
			Keyframes: []string{"frame_00002.jpg"},
			Marks: []browser.TakeMark{
				{T: 250, Seq: 1, Event: "mcp:before-submit", Dir: "act"},
			},
		}},
		evalRules: []fakeEvalRule{
			// The axe injector runs document.createElement; the axe run
			// calls axe.run; the vitals collector uses PerformanceObserver.
			{needle: "createElement", result: `"loaded"`},
			{needle: "axe.run", result: `[{"id":"button-name","impact":"serious","help":"Buttons must have discernible text","nodes":1,"first":["button#submit"]}]`},
			{needle: "PerformanceObserver", result: `{"fcp":900,"lcp":1400,"cls":0.02,"tbt":40,"inp":80}`},
		},
	}
	fk := &fakeMarkSink{}
	ft := &fakeTimeline{
		traces: []map[string]any{{"t": 1000, "seq": 1, "method": "GET", "path": "/"}},
		recs:   []map[string]any{{"t": 1100, "kind": "topic", "dir": "bcast", "topic": "counter", "field": "count"}},
	}
	sk := skills.New(nil) // the real embedded skill set, no version pins

	// A local test server backs the captured page: the audit's HTML fetch
	// (and any tool asserting on the page URL) reads real, deterministic
	// markup over loopback.
	pageSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, testAuditHTML)
	}))
	t.Cleanup(pageSrv.Close)
	fb.pageURL = pageSrv.URL

	srv := buildServer(func() *Capability {
		return &Capability{
			Browser:  fb,
			Build:    b,
			Timeline: ft,
			Skills:   sk,
			MarkSink: fk.Mark,
			Engine:   "chromium test",
			Version:  "v3-test",
		}
	})

	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(context.Background(), st, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	cl := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := cl.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	return &rig{browser: fb, builder: b, timeline: ft, marks: fk, skills: sk, client: cs}
}

// structured decodes a call's structured output into a generic map.
func structured(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	if res == nil {
		t.Fatal("nil tool result")
	}
	if res.IsError {
		t.Fatalf("tool returned an error result: %v", res.Content)
	}
	m, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content is not an object: %T (%v)", res.StructuredContent, res.StructuredContent)
	}
	return m
}

// asserting decodes a structured result into a typed out struct via JSON.
func asserting[Out any](t *testing.T, res *mcp.CallToolResult) Out {
	t.Helper()
	var out Out
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode structured content into %T: %v\nraw: %s", &out, err, raw)
	}
	return out
}

// ── tool list ──────────────────────────────────────────────────────────────

func TestToolListMatchesContract(t *testing.T) {
	r := newRig(t)
	res, err := r.client.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	want := map[string]bool{
		toolNavigate: true, toolAct: true, toolProbe: true, toolBrowserMode: true,
		toolSetViewport: true,
		toolView:        true, toolZoom: true, toolDiff: true, toolCompare: true,
		toolRecordStart: true, toolRecordStop: true, toolRecordMark: true, toolRecordFrames: true,
		toolTrace: true, toolLogs: true, toolAudit: true,
		toolBuildTempl: true, toolBuildCSS: true, toolBuildWasm: true, toolBuildGo: true,
		toolSync: true, toolBuildStatus: true,
		toolSkillSearch: true, toolSkillInfo: true, toolSkill: true,
	}
	got := make(map[string]bool, len(res.Tools))
	for _, tl := range res.Tools {
		got[tl.Name] = true
	}
	if len(got) != len(want) {
		t.Errorf("tool list has %d tools, want %d list = %v", len(got), len(want), keysOfTools(got))
	}
	for name := range want {
		if !got[name] {
			t.Errorf("tool %q missing from the list", name)
		}
	}
}

// keysOfTools lists a set of tool names sorted, for failure messages.
func keysOfTools(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestServerIdentityPinsNameAndVersion pins the identity the MCP server
// advertises to clients on initialize.
func TestServerIdentityPinsNameAndVersion(t *testing.T) {
	r := newRig(t)
	info := r.client.InitializeResult().ServerInfo
	if info == nil {
		t.Fatal("initialize result carries no serverInfo")
	}
	if info.Name != "gothic" {
		t.Errorf("server name = %q, want %q", info.Name, "gothic")
	}
	if info.Version != "v3-test" {
		t.Errorf("server version = %q, want %q", info.Version, "v3-test")
	}
}

// ── per-tool round trips ───────────────────────────────────────────────────

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s returned an error result: %v", name, res.Content)
	}
	return res
}

func TestRoundTripNavigate(t *testing.T) {
	r := newRig(t)
	out := asserting[navigateOut](t, callTool(t, r.client, toolNavigate, map[string]any{
		"url": testPageURL,
	}))
	if !out.OK || out.URL != testPageURL {
		t.Errorf("navigateOut = %+v", out)
	}
	if len(r.browser.calls) == 0 || r.browser.calls[0] != "open:"+testPageURL {
		t.Errorf("browser open not called: %v", r.browser.calls)
	}
}

func TestRoundTripAct(t *testing.T) {
	r := newRig(t)
	out := asserting[actOut](t, callTool(t, r.client, toolAct, map[string]any{
		"steps": []map[string]any{
			{"action": "click", "selector": "#submit"},
			{"action": "fill", "selector": "#q", "value": "gothic"},
		},
	}))
	if !out.OK || out.Steps != 2 {
		t.Errorf("actOut = %+v", out)
	}
	if got := strings.Join(r.browser.calls, ","); !strings.Contains(got, "act:click") || !strings.Contains(got, "act:fill") {
		t.Errorf("browser actions not driven: %v", r.browser.calls)
	}
}

func TestRoundTripProbe(t *testing.T) {
	r := newRig(t)
	out := asserting[probeOut](t, callTool(t, r.client, toolProbe, map[string]any{}))
	if !out.OK || !strings.Contains(out.Value, `"readyState":"complete"`) {
		t.Errorf("probeOut = %+v", out)
	}
	// A custom expression rides through as-is.
	out = asserting[probeOut](t, callTool(t, r.client, toolProbe, map[string]any{
		"expr": probeDefaultJS,
	}))
	if !out.OK || !strings.Contains(out.Value, `"title":"Home"`) {
		t.Errorf("custom probeOut = %+v", out)
	}
	// A non-function expression is refused (read-shaped by protocol).
	res, err := r.client.CallTool(context.Background(), &mcp.CallToolParams{
		Name: toolProbe, Arguments: map[string]any{"expr": "document.cookie"},
	})
	if err != nil {
		t.Fatalf("probe refusal: %v", err)
	}
	if !res.IsError {
		t.Errorf("expr without the () => shape must be an error result")
	}
}

func TestRoundTripBrowserMode(t *testing.T) {
	r := newRig(t)
	out := asserting[browserModeOut](t, callTool(t, r.client, toolBrowserMode, map[string]any{
		"headless": false,
	}))
	if !out.OK || out.Headless || !out.Running {
		t.Errorf("browserModeOut = %+v", out)
	}
	if r.browser.headless {
		t.Errorf("SetHeadless(false) not recorded")
	}
}

func TestRoundTripSetViewport(t *testing.T) {
	r := newRig(t)
	out := asserting[setViewportOut](t, callTool(t, r.client, toolSetViewport, map[string]any{
		"width": 375, "height": 667, "mobile": true,
	}))
	if !out.OK || out.Pinned != "375x667 mobile" {
		t.Fatalf("setViewportOut = %+v", out)
	}
	if r.browser.vpW != 375 || r.browser.vpH != 667 || !r.browser.mobile {
		t.Errorf("fake pin = %dx%d mobile=%v, want 375x667 mobile=true", r.browser.vpW, r.browser.vpH, r.browser.mobile)
	}

	// A desktop pin replaces the mobile one.
	out = asserting[setViewportOut](t, callTool(t, r.client, toolSetViewport, map[string]any{
		"width": 900, "height": 600,
	}))
	if !out.OK || out.Pinned != "900x600" || r.browser.mobile {
		t.Errorf("desktop pin = %+v (fake mobile=%v)", out, r.browser.mobile)
	}

	// Clear lifts the pin; a zero width without clear takes the same path.
	out = asserting[setViewportOut](t, callTool(t, r.client, toolSetViewport, map[string]any{"clear": true}))
	if !out.OK || out.Pinned != "(cleared)" || r.browser.vpW != 0 {
		t.Errorf("clear = %+v (fake vpW=%d)", out, r.browser.vpW)
	}

	// A pin error is a clean tool error.
	r.browser.viewportErr = fmt.Errorf("no browser")
	res, err := r.client.CallTool(context.Background(), &mcp.CallToolParams{
		Name: toolSetViewport, Arguments: map[string]any{"width": 400, "height": 400},
	})
	if err != nil {
		t.Fatalf("set_viewport error path: %v", err)
	}
	if !res.IsError {
		t.Errorf("viewport error must be an error result, got %v", res.StructuredContent)
	}
}

func TestRoundTripView(t *testing.T) {
	r := newRig(t)
	out := asserting[viewOut](t, callTool(t, r.client, toolView, map[string]any{
		"widths":       []any{390},
		"include_text": true,
	}))
	if !out.OK || len(out.Views) != 1 {
		t.Fatalf("viewOut = %+v", out)
	}
	v := out.Views[0]
	if v.Width != 390 || v.PNG == "" || v.ManifestPath == "" || !strings.Contains(v.Text, "=== GOTHIC PAGE VIEW ===") {
		t.Errorf("view entry = %+v", v)
	}
	// The light form omits the payload text.
	out = asserting[viewOut](t, callTool(t, r.client, toolView, map[string]any{}))
	if len(out.Views) != 1 || out.Views[0].Text != "" {
		t.Errorf("light view entry = %+v", out.Views)
	}
}

func TestRoundTripZoom(t *testing.T) {
	r := newRig(t)
	out := asserting[zoomOut](t, callTool(t, r.client, toolZoom, map[string]any{
		"selector": "div#hero",
	}))
	if !out.OK || out.Width != 100 || out.Height != 30 {
		t.Fatalf("zoomOut = %+v", out)
	}
	if !strings.Contains(out.Rect, "x=10") || out.PNG == "" || !strings.Contains(out.Text, "=== GOTHIC PAGE VIEW ===") {
		t.Errorf("zoom payload = %+v", out)
	}
	// An unknown selector is a clean error result.
	res, err := r.client.CallTool(context.Background(), &mcp.CallToolParams{
		Name: toolZoom, Arguments: map[string]any{"selector": "nope"},
	})
	if err != nil {
		t.Fatalf("zoom unknown selector: %v", err)
	}
	if !res.IsError {
		t.Errorf("unknown selector must be an error result, got %v", res.StructuredContent)
	}
}

func TestRoundTripDiff(t *testing.T) {
	r := newRig(t)
	// First call stores the baseline.
	out := asserting[diffOut](t, callTool(t, r.client, toolDiff, map[string]any{}))
	if !out.OK || !out.BaselineSet {
		t.Fatalf("baseline diffOut = %+v", out)
	}
	// Second call compares and reports change.
	out = asserting[diffOut](t, callTool(t, r.client, toolDiff, map[string]any{}))
	if !out.OK || out.Total != 32*16 || out.Changed == 0 {
		t.Errorf("diffOut = %+v (want some change, total 512)", out)
	}
}

func TestRoundTripCompare(t *testing.T) {
	r := newRig(t)
	out := asserting[compareOut](t, callTool(t, r.client, toolCompare, map[string]any{
		"widths": []any{390, 768},
	}))
	if !out.OK || len(out.Widths) != 2 || out.Total != 512 || len(out.Views) != 2 {
		t.Fatalf("compareOut = %+v", out)
	}
	if out.Changed == 0 {
		t.Errorf("the two widths' rasters share the fake's shape; expected some change")
	}
	// The heavy form includes both payload texts.
	out = asserting[compareOut](t, callTool(t, r.client, toolCompare, map[string]any{
		"widths": []any{390, 768}, "include_text": true,
	}))
	if len(out.Views) != 2 || !strings.Contains(out.Views[0].Text, "=== GOTHIC PAGE VIEW ===") {
		t.Errorf("compare heavy form = %+v", out.Views)
	}
	// A width-count error.
	res, err := r.client.CallTool(context.Background(), &mcp.CallToolParams{
		Name: toolCompare, Arguments: map[string]any{"widths": []any{390}},
	})
	if err != nil {
		t.Fatalf("compare one width: %v", err)
	}
	if !res.IsError {
		t.Errorf("one width must be an error result")
	}
}

func TestRoundTripRecordStartStopFramesMark(t *testing.T) {
	r := newRig(t)
	start := asserting[recordStartOut](t, callTool(t, r.client, toolRecordStart, map[string]any{
		"max_seconds": 30,
	}))
	if !start.OK || start.TakeID != "rec_20260102-150405-aa11" {
		t.Fatalf("recordStartOut = %+v", start)
	}
	if !strings.Contains(strings.Join(r.browser.calls, ","), "record:start") {
		t.Errorf("StartRecording not driven: %v", r.browser.calls)
	}

	// record_mark injects into the bus sink.
	mark := asserting[recordMarkOut](t, callTool(t, r.client, toolRecordMark, map[string]any{
		"name": "before-submit", "payload": map[string]any{"sel": "#submit"},
	}))
	if !mark.OK || mark.Event != "mcp:before-submit" {
		t.Errorf("recordMarkOut = %+v", mark)
	}
	if len(r.marks.events) != 1 || r.marks.events[0] != "mcp:before-submit" {
		t.Errorf("mark sink events = %v", r.marks.events)
	}

	// record_frames while active is honest about the running take.
	frames := asserting[recordFramesOut](t, callTool(t, r.client, toolRecordFrames, map[string]any{}))
	if !frames.OK || !frames.Active {
		t.Errorf("active recordFramesOut = %+v", frames)
	}

	// record_stop returns the manifest summary.
	stop := asserting[recordStopOut](t, callTool(t, r.client, toolRecordStop, map[string]any{}))
	if !stop.OK || !stop.Stopped || !stop.Settled || stop.Frames != 2 || stop.Marks != 1 || stop.TakeID != "rec_20260102-150405-aa11" {
		t.Errorf("recordStopOut = %+v", stop)
	}
	// After the stop, record_frames reads the finalized manifest (heavy
	// form on request).
	stopHeavy := asserting[recordFramesOut](t, callTool(t, r.client, toolRecordFrames, map[string]any{
		"include_frames": true,
	}))
	if !stopHeavy.OK || stopHeavy.Active || stopHeavy.Frames != 2 || !stopHeavy.Settled || len(stopHeavy.FrameLines) != 2 {
		t.Errorf("recordFramesOut after stop = %+v", stopHeavy)
	}
	// Stopping again (no active take) is refused as an error result.
	res, err := r.client.CallTool(context.Background(), &mcp.CallToolParams{
		Name: toolRecordStop, Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("stop without take: %v", err)
	}
	if !res.IsError {
		t.Errorf("stop without active take must be an error result")
	}
}

func TestRoundTripTraceAndLogs(t *testing.T) {
	r := newRig(t)
	trace := asserting[traceOut](t, callTool(t, r.client, toolTrace, map[string]any{"last": 5}))
	if !trace.OK || trace.Count != 1 || len(trace.Records) != 1 || trace.Records[0].Source != "trace" {
		t.Fatalf("traceOut = %+v", trace)
	}
	if got, _ := trace.Records[0].Record["path"].(string); got != "/" {
		t.Errorf("trace record path = %v", trace.Records[0].Record)
	}
	r.browser.console = []browser.ConsoleEntry{
		{Type: "error", Text: "Uncaught Error: boom", TimestampMs: 1200},
		{Type: "warning", Text: "deprecated API", TimestampMs: 900},
	}
	logs := asserting[logsOut](t, callTool(t, r.client, toolLogs, map[string]any{}))
	if !logs.OK || logs.Count != 1 || len(logs.Records) != 1 || logs.Records[0].Source != "bus" {
		t.Fatalf("logsOut = %+v", logs)
	}
	if got, _ := logs.Records[0].Record["topic"].(string); got != "counter" {
		t.Errorf("bus record topic = %v", logs.Records[0].Record)
	}
	// The retained page console rides along, newest first.
	if len(logs.Console) != 2 || logs.Console[0].Type != "error" || logs.Console[0].Text != "Uncaught Error: boom" {
		t.Errorf("logs console = %+v", logs.Console)
	}

	// last bounds both planes.
	logs = asserting[logsOut](t, callTool(t, r.client, toolLogs, map[string]any{"last": 1}))
	if len(logs.Console) != 1 || logs.Console[0].Type != "error" {
		t.Errorf("logs console last=1 = %+v", logs.Console)
	}
}

func TestRoundTripAudit(t *testing.T) {
	r := newRig(t)
	out := asserting[auditOut](t, callTool(t, r.client, toolAudit, map[string]any{
		"include_a11y": true, "include_vitals": true,
	}))
	if !out.OK || out.Fail == 0 {
		t.Fatalf("auditOut = %+v", out)
	}
	checks := map[string]auditFinding{}
	for _, f := range out.Findings {
		checks[f.Check] = f
	}
	// Every audit plane produced findings.
	for _, want := range []string{"seo-title", "vitals-fcp", "a11y:button-name", "console-errors"} {
		if _, ok := checks[want]; !ok {
			t.Errorf("audit finding %q missing; got %v", want, keys(checks))
		}
	}
	// The console plane runs from the retained ring: clean here (the fake
	// kept no console messages).
	if f := checks["console-errors"]; f.Status != "pass" {
		t.Errorf("console-errors = %+v", f)
	}

	// With retained errors the plane fails and reports them.
	r.browser.console = []browser.ConsoleEntry{
		{Type: "error", Text: "Uncaught Error: boom", TimestampMs: 10},
		{Type: "warning", Text: "old warning", TimestampMs: 5},
	}
	out = asserting[auditOut](t, callTool(t, r.client, toolAudit, map[string]any{}))
	for _, f := range out.Findings {
		if f.Check == "console-errors" {
			if f.Status != "fail" || !strings.Contains(f.Detail, "2 console") || !strings.Contains(f.Detail, "Uncaught Error: boom") {
				t.Errorf("console-errors with entries = %+v", f)
			}
		}
	}
	// The axe finding reports node detail.
	if f := checks["a11y:button-name"]; f.Impact != "serious" || !strings.Contains(f.Detail, "button#submit") {
		t.Errorf("axe finding = %+v", f)
	}
}

func TestRoundTripBuildTools(t *testing.T) {
	r := newRig(t)

	templ := asserting[buildOut](t, callTool(t, r.client, toolBuildTempl, map[string]any{
		"file": "src/pages/index.templ",
	}))
	if !templ.OK || templ.Stage != buildctl.StageTempl || len(templ.Recompiled) != 1 {
		t.Errorf("build_templ out = %+v", templ)
	}
	if r.builder.templFile != "src/pages/index.templ" {
		t.Errorf("templ file = %q", r.builder.templFile)
	}

	css := asserting[buildOut](t, callTool(t, r.client, toolBuildCSS, map[string]any{}))
	if !css.OK || css.Stage != buildctl.StageCSS {
		t.Errorf("build_css out = %+v", css)
	}

	wasm := asserting[buildOut](t, callTool(t, r.client, toolBuildWasm, map[string]any{
		"target": "/counter",
	}))
	if !wasm.OK || wasm.Stage != buildctl.StageWasm || r.builder.wasmTarget != "/counter" {
		t.Errorf("build_wasm out = %+v", wasm)
	}

	go_ := asserting[buildOut](t, callTool(t, r.client, toolBuildGo, map[string]any{}))
	if !go_.OK || go_.Stage != buildctl.StageGo {
		t.Errorf("build_go out = %+v", go_)
	}

	sync := asserting[buildOut](t, callTool(t, r.client, toolSync, map[string]any{}))
	if !sync.OK || sync.Stage != buildctl.StageSync {
		t.Errorf("sync out = %+v", sync)
	}

	status := asserting[buildctl.StatusResult](t, callTool(t, r.client, toolBuildStatus, map[string]any{}))
	if len(status.Artifacts) != 1 || status.Artifacts[0].State != buildctl.StatusFresh {
		t.Errorf("build_status out = %+v", status)
	}
}

func TestRoundTripSkillTools(t *testing.T) {
	r := newRig(t)
	search := asserting[skillSearchOut](t, callTool(t, r.client, toolSkillSearch, map[string]any{
		"query": "routing",
	}))
	if !search.OK || len(search.Hits) == 0 || search.Hits[0].Name != "gothic-routing" {
		t.Fatalf("skillsearch out = %+v", search)
	}

	info := asserting[skillInfoOut](t, callTool(t, r.client, toolSkillInfo, map[string]any{
		"names": []any{"gothic-routing"},
	}))
	if !info.OK || len(info.Skills) != 1 || info.Skills[0].AppliesTo == "" {
		t.Errorf("skillinfo out = %+v", info)
	}

	// The light form: the name list only.
	light := asserting[skillOut](t, callTool(t, r.client, toolSkill, map[string]any{}))
	if !light.OK || len(light.Names) == 0 || len(light.Docs) != 0 {
		t.Errorf("skill light form = %+v", light)
	}
	// The heavy form: full documents for named skills only.
	heavy := asserting[skillOut](t, callTool(t, r.client, toolSkill, map[string]any{
		"names": []any{"gothic-routing"},
	}))
	if !heavy.OK || len(heavy.Docs) != 1 || !strings.Contains(heavy.Docs[0].Body, "RouteConfig") {
		t.Errorf("skill heavy form = %+v", heavy.Docs)
	}
	// An unknown skill name is refused with the known list in the error.
	res, err := r.client.CallTool(context.Background(), &mcp.CallToolParams{
		Name: toolSkill, Arguments: map[string]any{"names": []any{"nope"}},
	})
	if err != nil {
		t.Fatalf("skill unknown: %v", err)
	}
	if !res.IsError {
		t.Errorf("unknown skill name must be an error result")
	}
}

// keys reports the map's keys sorted for failure messages.
func keys(m map[string]auditFinding) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ── resources ──────────────────────────────────────────────────────────────

func TestReadSkillResource(t *testing.T) {
	r := newRig(t)
	res, err := r.client.ReadResource(context.Background(), &mcp.ReadResourceParams{
		URI: "gothic-skill://gothic-routing",
	})
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if len(res.Contents) != 1 {
		t.Fatalf("resources read returned %d contents", len(res.Contents))
	}
	c := res.Contents[0]
	if c.MIMEType != "text/markdown" || !strings.Contains(c.Text, "RouteConfig") {
		t.Errorf("resource content = %+v (%d bytes)", c.MIMEType, len(c.Text))
	}
	// An unknown skill resource is the SDK's not-found error.
	if _, err := r.client.ReadResource(context.Background(), &mcp.ReadResourceParams{
		URI: "gothic-skill://no-such-skill",
	}); err == nil {
		t.Errorf("unknown skill resource must be an error")
	}
}

// ── compile-time assertions: the concrete machinery satisfies the ports ─────

var (
	_ BrowserPort = browserPort{}
	_ TakePort    = (*browser.Take)(nil)
	_ BuildPort   = (*buildctl.Controller)(nil)
	_ SkillsPort  = (*skills.Set)(nil)
)
