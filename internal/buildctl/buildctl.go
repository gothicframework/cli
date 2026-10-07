// Package buildctl exposes the Gothic build pipeline as a structured Go API:
// each tool runs one stage (templ, routes_gen, topic_gen, wasm, css, the app
// build), is incremental against the on-disk caches the pipeline already
// keeps, and returns a BuildResult carrying what it recompiled, how long it
// took, translated diagnostics for any failure, and the artifacts its output
// just invalidated ("now stale: routes_gen, wasm:/counter").
//
// The tool layer never prints. Callers (the hot-reload command today, the MCP
// in the future) render results to their own output contract; the narration
// the underlying helpers print (Recompiling..., size lines) is theirs.
//
// Concurrency: the generator stages and the app build share one build lock,
// which is also what an edit session holds across a multi-file edit; the WASM
// stage serialises on its own lock because the hot-reload session compiles it
// in the background while the foreground build proceeds.
package buildctl

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	helpers "github.com/gothicframework/cli/v4/internal/build"
	buildtools "github.com/gothicframework/cli/v4/internal/buildtools"
	gothic_cli "github.com/gothicframework/cli/v4/internal/cli"
	templcache "github.com/gothicframework/core/render"
)

// Stage names — the stage field of every BuildResult.
const (
	StageTempl     = "templ"
	StageRoutesGen = "routes_gen"
	StageTopicGen  = "topic_gen"
	StageWasm      = "wasm"
	StageCSS       = "css"
	StageGo        = "go"
	StageSync      = "sync"
)

// Sub-step names a stage can fail at (the routes stage reads the config first
// and syncs the embedded public file last, and callers want to know which of
// the three failed without parsing the error text).
const (
	StepConfig   = "config"
	StepEmbedded = "embedded"
)

// On-disk artifact paths, all relative to the project root (the CLI's cwd).
const (
	routesGenPath = "src/routes/routes_gen.go"
	topicGenPath  = "src/topics/topic_gen.go"
	cssOutPath    = "public/styles.css"
	wasmOutDir    = "public/wasm"
	pagesDir      = "src/pages"
	componentsDir = "src/components"
)

// Recompiled is one artifact a build tool actually wrote.
type Recompiled struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// Diagnostic is a translated build failure. Raw carries the tool's own
// compiler/generator text verbatim; Diagnosis and Fix are the plain-language
// translation; Skill, when set, names a bundled skill document covering the
// area (see internal/skills).
type Diagnostic struct {
	Raw       string `json:"raw"`
	Diagnosis string `json:"diagnosis"`
	Fix       string `json:"fix"`
	Skill     string `json:"skill,omitempty"`
}

// StageCounts mirror the WASM helper's own counters after a stage.
type StageCounts struct {
	Rebuilt  int `json:"rebuilt"`
	UpToDate int `json:"upToDate"`
}

// BuildResult is the structured outcome of one build tool. OK is false only
// when a stage failed; Suppressed marks a call that ran during an edit
// session, which performs no work by design.
type BuildResult struct {
	OK          bool         `json:"ok"`
	Stage       string       `json:"stage"`
	Step        string       `json:"step,omitempty"`
	Target      string       `json:"target,omitempty"`
	Recompiled  []Recompiled `json:"recompiled,omitempty"`
	MS          int64        `json:"ms"`
	Raw         string       `json:"raw,omitempty"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
	// NowStale lists the artifacts this build's output just invalidated, in
	// pipeline order: e.g. "routes_gen", "wasm:/counter". The names are the
	// stage/artifact names; a wasm entry may be per-route ("wasm:/counter")
	// or the whole inventory ("wasm").
	NowStale   []string     `json:"nowStale,omitempty"`
	Counts     *StageCounts `json:"counts,omitempty"`
	Suppressed bool         `json:"suppressed,omitempty"`
	// Err is the raw error text of a failed stage ("" when OK).
	Err string `json:"err,omitempty"`
}

// Options wire the controller to its host. Every field is optional.
type Options struct {
	// MainBinary is the app build target ("tmp/main"); derived from the CLI
	// runtime when empty.
	MainBinary string
	// SyncEmbedded syncs the EMBEDDED-mode gothic_embed_gen.go — a host-layer
	// concern (the file's shape is decided by the command layer). Nil skips
	// the step.
	SyncEmbedded func(config *gothic_cli.Config) error
	// WasmLogf / WasmErrorf narrate the WASM stage the way the hosting
	// command's stream expects. No-ops when nil.
	WasmLogf   func(format string, args ...any)
	WasmErrorf func(format string, args ...any)
	// WasmInventory fires right before the WASM generate phase with the scan
	// inventory (pages, components, topics); the host prints its own line.
	WasmInventory func(pages, components, topics int)
	// BeginEdit suspends the host's watcher-driven rebuilds (stops arming
	// them) for the duration of an edit session; the returned function is
	// called when the session ends.
	BeginEdit func() (end func())
	// AfterSync fires after EditEnd's one Sync, so the host can re-arm its
	// rebuild cycle for the app binary and the reload.
	AfterSync func(res BuildResult)
}

// Controller owns the build pipeline for one session (a hot-reload command
// today; the MCP server in the future). All build calls are serialised against
// the caches they share.
type Controller struct {
	cli  *gothic_cli.GothicCli
	opts Options

	mu     sync.Mutex // the build lock: generator stages, the app build, and edit sessions
	wasmMu sync.Mutex // the WASM stage; separate so a background compile never blocks first paint

	busy    atomic.Int32 // per-stage mid-rebuild bits for BuildStatus
	editing atomic.Bool  // an edit session holds the build lock
	editMu  sync.Mutex   // serialises Begin/End
	editEnd func()       // host hook to resume the watcher

	// Executor seams. Nil = the production default bound over the CLI helpers;
	// tests replace them with fakes.
	TemplRun  func(file string) (string, error) // "" = all dirty files
	RoutesRun func(goModName string) error
	CssRun    func() error
	GoRun     func(binary string) (string, error)                     // combined captured output
	WasmRun   func(target string, snap Snapshot) (BuildResult, error) // the WASM stage incl. gate

	// lastOK records the completion time of each stage's last successful run,
	// for the mtime-based freshness signals in BuildStatus. Indexed by stage.
	lastOK [stageCount]atomic.Int64
}

// stageCount is one slot per stage name plus a sync slot.
const stageCount = 8

// New builds a Controller over the CLI's build helpers.
func New(cli *gothic_cli.GothicCli, opts Options) *Controller {
	c := &Controller{cli: cli, opts: opts}
	if c.opts.MainBinary == "" {
		c.opts.MainBinary = "tmp/main"
		if cli != nil && cli.Runtime == "windows" {
			c.opts.MainBinary += ".exe"
		}
	}
	if c.WasmRun == nil {
		c.WasmRun = c.realWasmStage
	}
	return c
}

// Editing reports whether an edit session currently holds the build lock.
// The hot-reload watcher consults this to suspend its own rebuilds.
func (c *Controller) Editing() bool { return c.editing.Load() }

// EditBegin suspends watcher-driven rebuilds and holds the build lock across a
// multi-file edit sequence. While the session is open, every build call
// short-circuits with a Suppressed result instead of racing the edit. A
// second Begin while a session is open is a no-op.
func (c *Controller) EditBegin() {
	c.editMu.Lock()
	defer c.editMu.Unlock()
	if !c.editing.CompareAndSwap(false, true) {
		return
	}
	if c.opts.BeginEdit != nil {
		c.editEnd = c.opts.BeginEdit()
	}
	// Hold the build lock for the whole session: a rebuild that was already
	// mid-stage finishes, and everything after it waits for EditEnd.
	c.mu.Lock()
}

// EditEnd closes the edit session and runs exactly one Sync over the changed
// state. Returns the sync's BuildResult, and fires the host's AfterSync hook
// so it can re-arm its own rebuild cycle for the app binary and the reload.
// When no session is open, it is a no-op.
func (c *Controller) EditEnd() BuildResult {
	c.editMu.Lock()
	defer c.editMu.Unlock()
	if !c.editing.CompareAndSwap(true, false) {
		return BuildResult{OK: true, Stage: StageSync}
	}
	end := c.editEnd
	c.editEnd = nil
	// Release the edit-session hold first, then sync with the flag already
	// down so the sync is not suppressed against itself. A watcher rebuild
	// that slips in between serialises on the same lock and simply re-runs
	// afterwards at cache speed.
	c.mu.Unlock()
	if end != nil {
		end()
	}
	res := c.Sync()
	if c.opts.AfterSync != nil {
		c.opts.AfterSync(res)
	}
	return res
}

// ── stage bodies ──────────────────────────────────────────────────────────

// Per-stage busy bits: BuildStatus reports the artifact whose stage is
// currently running as mid-rebuild.
const (
	busyTempl int32 = 1 << iota
	busyRoutesGen
	busyTopicGen
	busyWasm
	busyCSS
	busyGo
)

func stageBit(stage string) int32 {
	switch stage {
	case StageTempl:
		return busyTempl
	case StageRoutesGen:
		return busyRoutesGen
	case StageTopicGen:
		return busyTopicGen
	case StageWasm:
		return busyWasm
	case StageCSS:
		return busyCSS
	case StageGo:
		return busyGo
	}
	return 0
}

// BuildTempl runs the templ generate stage. An empty file compiles every
// dirty .templ file (the incremental cached path); a named file regenerates
// just that one. Templ output becomes the pipeline's routes_gen and WASM
// inputs, so a run that compiled anything declares them stale.
func (c *Controller) BuildTempl(file string) BuildResult {
	return c.run(&c.mu, StageTempl, file, c.bodyTempl)
}

// BuildRoutes runs the routes_gen stage: config read, the file-based route
// registry, and (when wired) the EMBEDDED-mode embed file. The stage's output
// feeds the app build, so a rewrite declares it stale.
func (c *Controller) BuildRoutes() BuildResult {
	return c.run(&c.mu, StageRoutesGen, "", c.bodyRoutes)
}

// BuildTopics regenerates src/topics/topic_gen.go from the topic
// declarations. The generated accessors are inlined into every page's WASM
// entry point and compiled into the server, so a rewrite declares both stale.
func (c *Controller) BuildTopics() BuildResult {
	return c.run(&c.mu, StageTopicGen, "", c.bodyTopics)
}

// BuildWasm runs the WASM stage: the digest gate, topic-stub ordering
// (stubs before the scan), the page scan, and the incremental per-symbol
// compile. A target (route path, component path, or topic name) scopes the
// reporting to the matching units; the stage itself always runs the full
// inventory because the per-symbol cache makes everything else instant.
func (c *Controller) BuildWasm(target string) BuildResult {
	return c.run(&c.wasmMu, StageWasm, target, func(target string) (BuildResult, error) {
		res, err := c.bodyWasm(target)
		if err != nil || target == "" {
			return res, err
		}
		return c.narrowToTarget(res, target), nil
	})
}

// narrowToTarget scopes a WASM result's reporting to the units the target
// names. A target is a route path ("/counter"), a component path
// ("/components/counter"), or an output name ("counter"); "topic:<key>" and
// topic keys keep the full report — a topic's definitions inlined into every
// page that uses it. When nothing matches, the report is kept whole: the
// stage did run everything, and reporting zero rebuilt units would mislead.
func (c *Controller) narrowToTarget(res BuildResult, target string) BuildResult {
	if strings.HasPrefix(target, "topic:") {
		return res
	}
	name := strings.TrimPrefix(target, "/")
	named := func(path string) bool {
		return path == target || path == name ||
			strings.TrimPrefix(path, "/") == name ||
			strings.HasPrefix(path+"/", target+"/") ||
			strings.HasPrefix(path, target+"/")
	}
	var matched []Recompiled
	for _, r := range res.Recompiled {
		if !strings.HasPrefix(r.Path, "wasm:") {
			matched = append(matched, r)
			continue
		}
		if named(strings.TrimPrefix(r.Path, "wasm:")) {
			matched = append(matched, r)
		}
	}
	if len(matched) == 0 {
		return res
	}
	res.Recompiled = matched
	return res
}

// BuildCSS compiles the Tailwind output. Tailwind scans every template for
// classes, so the build is project-wide; an optional page name is recorded as
// the target. The output file's mtime gates the build: fresh CSS is a no-op.
func (c *Controller) BuildCSS(page string) BuildResult {
	return c.run(&c.mu, StageCSS, page, c.bodyCSS)
}

// BuildGo compiles the app server binary, capturing its output for
// diagnostics while echoing it to the terminal.
func (c *Controller) BuildGo() BuildResult {
	return c.run(&c.mu, StageGo, "", c.bodyGo)
}

// Sync brings every generated artifact up to date in pipeline order —
// templ → routes_gen → topic_gen → wasm → css — incrementally and
// idempotently: when nothing changed, no stage recompiles and the call
// returns immediately. It does NOT run BuildGo; the caller compiles the app
// when it wants the server binary fresh.
func (c *Controller) Sync() BuildResult {
	if c.editing.Load() {
		return BuildResult{OK: true, Stage: StageSync, Suppressed: true}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	start := time.Now()
	out := c.runSync()
	out.MS = time.Since(start).Milliseconds()
	return out
}

// runSync executes the pipeline in order. Called with c.mu held (Sync) or
// from EditEnd, which released the lock it held before calling Sync.
// Stops at the first failing stage: a later stage would compile inputs the
// failed stage never produced.
func (c *Controller) runSync() BuildResult {
	out := BuildResult{OK: true, Stage: StageSync}
	merge := func(res BuildResult) {
		if !res.OK && out.OK {
			out.OK = false
			out.Stage = res.Stage
			out.Step = res.Step
			out.Err = res.Err
			out.Raw = res.Raw
			out.Diagnostics = res.Diagnostics
		}
		out.Recompiled = append(out.Recompiled, res.Recompiled...)
		out.NowStale = mergeUnique(out.NowStale, res.NowStale)
		if res.Counts != nil {
			out.Counts = res.Counts
		}
	}
	// Each stage body runs directly (the lock is already held); fillRun does
	// the busy-bit bookkeeping so BuildStatus reports the right artifact as
	// mid-rebuild.
	for _, s := range []struct {
		stage string
		body  func() (BuildResult, error)
	}{
		{StageTempl, func() (BuildResult, error) { return c.bodyTempl("") }},
		{StageRoutesGen, func() (BuildResult, error) { return c.bodyRoutes("") }},
		{StageTopicGen, func() (BuildResult, error) { return c.bodyTopics("") }},
	} {
		merge(c.fillRun(s.stage, "", s.body))
		if !out.OK {
			return out
		}
	}
	// The WASM stage takes its own lock inside the runner; run it through the
	// public method so the locking and the busy bit stay in one place.
	merge(c.BuildWasm(""))
	if !out.OK {
		return out
	}
	merge(c.fillRun(StageCSS, "", func() (BuildResult, error) { return c.bodyCSS("") }))
	return out
}

// fillRun runs one stage body and fills the result envelope. No locking: the
// caller holds the stage's lock.
func (c *Controller) fillRun(stage, target string, body func() (BuildResult, error)) BuildResult {
	bit := stageBit(stage)
	c.busy.Add(bit)
	defer c.busy.Add(-bit)
	start := time.Now()
	res, err := body()
	res.Stage = stage
	res.Target = target
	res.MS = time.Since(start).Milliseconds()
	if err != nil {
		res.OK = false
		if res.Err == "" {
			res.Err = err.Error()
		}
		if res.Raw == "" {
			res.Raw = res.Err
		}
		if len(res.Diagnostics) == 0 {
			res.Diagnostics = DiagnoseAll(res.Raw)
		}
	} else {
		res.OK = true
		c.markOK(stage)
	}
	return res
}

// run is the public-call envelope: edit-session suppression, stage locking,
// busy-bit bookkeeping, and result filling.
func (c *Controller) run(mu *sync.Mutex, stage, target string, body func(target string) (BuildResult, error)) BuildResult {
	if c.editing.Load() {
		return BuildResult{OK: true, Stage: stage, Target: target, Suppressed: true}
	}
	mu.Lock()
	defer mu.Unlock()
	return c.fillRun(stage, target, func() (BuildResult, error) { return body(target) })
}

// ── templ stage ───────────────────────────────────────────────────────────

func (c *Controller) bodyTempl(file string) (BuildResult, error) {
	var res BuildResult
	dirty, dirtyErr := templDirtyFiles()
	var out string
	var err error
	switch {
	case c.TemplRun != nil:
		out, err = c.TemplRun(file)
	case file == "":
		err = c.cli.Templ.Render()
		out = buildtools.LastTemplOutput()
	default:
		out, err = c.cli.Templ.RenderFile(file)
	}
	res.Raw = out
	if err != nil {
		return res, err
	}
	if dirtyErr != nil {
		// The dirty list is best-effort metadata: the compile itself is the
		// authority and it succeeded, so surface nothing but keep the result
		// free of invented recompiled entries.
		return res, nil
	}
	// Only files that were actually dirty got compiled; the rest of the
	// project is untouched.
	res.Recompiled = recompiledTempl(dirty)
	if len(dirty) > 0 {
		res.NowStale = c.templNowStale(dirty)
	}
	return res, nil
}

// recompiledTempl turns the dirty-file list into recompiled entries: the
// generated counterpart of each dirty .templ source, with its on-disk size.
func recompiledTempl(dirty []string) []Recompiled {
	var out []Recompiled
	for _, f := range dirty {
		gen := templCounterpart(f)
		size, err := fileSize(gen)
		if err != nil {
			continue // failed counterpart; the stage's error surfaces separately
		}
		out = append(out, Recompiled{Path: gen, Bytes: size})
	}
	return out
}

// templNowStale declares what a templ compile just invalidated, in pipeline
// order: the route registry when a routed source changed, one WASM unit per
// dirty source whose generated file carries a ClientSideState, the CSS (new
// classes), and the app binary (the compiled server sources changed).
func (c *Controller) templNowStale(dirty []string) []string {
	var out []string
	for _, f := range dirty {
		if !isRouteSource(f) {
			continue
		}
		out = append(out, StageRoutesGen)
		break
	}
	seen := map[string]bool{StageRoutesGen: true}
	for _, f := range dirty {
		if !isRouteSource(f) {
			continue
		}
		gen := templCounterpart(f)
		data, err := os.ReadFile(gen)
		if err != nil || !strings.Contains(string(data), "ClientSideState") {
			continue
		}
		unit := "wasm:" + c.httpPathFor(gen)
		if seen[unit] {
			continue
		}
		seen[unit] = true
		out = append(out, unit)
	}
	out = append(out, StageCSS, StageGo)
	return out
}

// templCounterpart returns the generated _templ.go path of a .templ source.
func templCounterpart(templPath string) string {
	return strings.TrimSuffix(filepath.ToSlash(templPath), ".templ") + "_templ.go"
}

// isRouteSource reports whether a .templ source lives in the routed folders
// (pages and components; layouts are not routed).
func isRouteSource(path string) bool {
	p := filepath.ToSlash(path)
	return strings.HasPrefix(p, pagesDir+"/") || strings.HasPrefix(p, componentsDir+"/")
}

// templDirtyFiles lists the .templ files whose cached hash differs from their
// content or whose generated counterpart is missing. The cache is content
// based, so this is the authoritative templ freshness signal.
func templDirtyFiles() ([]string, error) {
	cache := templcache.Load()
	files, err := templcache.ScanTemplFiles(".")
	if err != nil {
		return nil, err
	}
	return templcache.DirtyFiles(cache, files), nil
}

// httpPathFor maps a generated page file to the HTTP route it serves, through
// the scanner's own rules.
func (c *Controller) httpPathFor(genPath string) string {
	if c.cli != nil {
		return c.cli.Wasm.HttpPathForSource(genPath)
	}
	return genPath
}

// ── routes stage ──────────────────────────────────────────────────────────

func (c *Controller) bodyRoutes(_ string) (BuildResult, error) {
	var res BuildResult
	before := statFile(routesGenPath)
	config, err := c.cli.GetConfig()
	if err != nil {
		res.Step = StepConfig
		return res, err
	}
	var renderErr error
	if c.RoutesRun != nil {
		renderErr = c.RoutesRun(config.GoModName)
	} else {
		renderErr = c.cli.FileBasedRouter.Render(config.GoModName)
	}
	if renderErr != nil {
		res.Step = StageRoutesGen
		return res, renderErr
	}
	if c.opts.SyncEmbedded != nil {
		if err := c.opts.SyncEmbedded(&config); err != nil {
			res.Step = StepEmbedded
			return res, err
		}
	}
	if rec, changed := fileChanged(routesGenPath, before); changed {
		res.Recompiled = append(res.Recompiled, rec)
		res.NowStale = []string{StageGo}
	}
	return res, nil
}

// ── topic_gen stage ───────────────────────────────────────────────────────

func (c *Controller) bodyTopics(_ string) (BuildResult, error) {
	var res BuildResult
	before := statFile(topicGenPath)
	c.cli.Wasm.PregenerateTopicStubs()
	if rec, changed := fileChanged(topicGenPath, before); changed {
		res.Recompiled = append(res.Recompiled, rec)
		res.NowStale = []string{StageWasm, StageGo}
	}
	return res, nil
}

// ── css stage ─────────────────────────────────────────────────────────────

// cssFresh reports whether the compiled CSS is newer than every source it
// could depend on: Tailwind scans the whole source tree for classes.
func (c *Controller) cssFresh() bool {
	fi := statFile(cssOutPath)
	if fi == nil {
		return false
	}
	newest := newestMtime([]string{"src"}, ".css", ".templ", ".go", ".html")
	return !newest.After(maxTime(fi.ModTime(), c.lastOKTime(StageCSS)))
}

func (c *Controller) bodyCSS(page string) (BuildResult, error) {
	if page == "" && c.cssFresh() {
		// Fresh CSS: no build, no recompiled entry — the instant no-op.
		return BuildResult{}, nil
	}
	if c.CssRun != nil {
		if err := c.CssRun(); err != nil {
			return BuildResult{}, err
		}
	} else if err := c.cli.Tailwind.Build(); err != nil {
		return BuildResult{}, err
	}
	size, err := fileSize(cssOutPath)
	if err != nil {
		return BuildResult{}, fmt.Errorf("stat %s after build: %w", cssOutPath, err)
	}
	return BuildResult{Recompiled: []Recompiled{{Path: cssOutPath, Bytes: size}}}, nil
}

// ── go stage ──────────────────────────────────────────────────────────────

func (c *Controller) bodyGo(_ string) (BuildResult, error) {
	var res BuildResult
	var raw string
	var err error
	switch {
	case c.GoRun != nil:
		raw, err = c.GoRun(c.opts.MainBinary)
	default:
		raw, err = defaultGoBuild(c.opts.MainBinary)
	}
	res.Raw = raw
	if err != nil {
		return res, err
	}
	size, _ := fileSize(c.opts.MainBinary)
	res.Recompiled = []Recompiled{{Path: c.opts.MainBinary, Bytes: size}}
	return res, nil
}

// defaultGoBuild compiles the app package, teeing the compiler's stdout and
// stderr into the terminal and into a buffer: the output contract keeps its
// live echo, and the captured text becomes the diagnostics' raw material.
func defaultGoBuild(binary string) (string, error) {
	var buf bytes.Buffer
	cmd := exec.Command("go", "build", "-o", binary, ".")
	cmd.Stdout = io.MultiWriter(os.Stdout, &buf)
	cmd.Stderr = io.MultiWriter(os.Stderr, &buf)
	err := cmd.Run()
	return buf.String(), err
}

// ── freshness helpers ─────────────────────────────────────────────────────

// stageIndex maps a stage name to its lastOK slot.
func stageIndex(stage string) int {
	switch stage {
	case StageTempl:
		return 0
	case StageRoutesGen:
		return 1
	case StageTopicGen:
		return 2
	case StageWasm:
		return 3
	case StageCSS:
		return 4
	case StageGo:
		return 5
	}
	return 6
}

// verifiedDir holds one persisted marker per stage, beside the other
// .gothicCli session state.
const verifiedDir = ".gothicCli/verified"

// markOK records the in-session completion time and persists it as a marker
// file. The in-memory times are lost when the process restarts, and the
// write-if-changed artifacts a verified stage did not rewrite keep their old
// mtime on disk — the marker is what keeps the mtime-based freshness signals
// in BuildStatus honest across restarts. Best effort: a failed write leaves
// the signal exactly as it was (fail toward stale, the documented default).
func (c *Controller) markOK(stage string) {
	now := time.Now()
	c.lastOK[stageIndex(stage)].Store(now.UnixNano())
	if err := os.MkdirAll(verifiedDir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(verifiedMarker(stage), []byte(now.Format(time.RFC3339)+"\n"), 0o644)
}

// verifiedMarker is the stage's persisted verification marker path.
func verifiedMarker(stage string) string {
	return filepath.Join(verifiedDir, stage+".ok")
}

// verifiedTime returns the persisted completion time of the stage's last
// successful run (any process); the zero time when the marker is absent.
func verifiedTime(stage string) time.Time {
	fi, err := os.Stat(verifiedMarker(stage))
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// freshSince reports whether the last successful run of stage completed after
// the given time. Zero lastOK (never ran this session) never reports fresh.
func (c *Controller) freshSince(stage string, t time.Time) bool {
	last := c.lastOK[stageIndex(stage)].Load()
	if last == 0 {
		return false
	}
	return time.Unix(0, last).After(t)
}

// lastOKTime returns the completion time of the stage's last successful run
// this session; the zero time when it never ran.
func (c *Controller) lastOKTime(stage string) time.Time {
	n := c.lastOK[stageIndex(stage)].Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// statFile returns the os.FileInfo of path, nil when unreadable.
func statFile(path string) os.FileInfo {
	fi, err := os.Stat(path)
	if err != nil {
		return nil
	}
	return fi
}

// fileChanged compares a file's state before and after a build; returns the
// recompiled entry and true when the file is new or its size/mtime changed.
func fileChanged(path string, before os.FileInfo) (Recompiled, bool) {
	after := statFile(path)
	if after == nil {
		return Recompiled{}, false
	}
	if before != nil &&
		before.Size() == after.Size() &&
		before.ModTime().Equal(after.ModTime()) {
		return Recompiled{}, false
	}
	return Recompiled{Path: path, Bytes: after.Size()}, true
}

// fileSize returns the size of path, or an error when unreadable.
func fileSize(path string) (int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// mergeUnique appends entries from add that are not already in base,
// preserving order.
func mergeUnique(base, add []string) []string {
	if len(add) == 0 {
		return base
	}
	seen := make(map[string]bool, len(base)+len(add))
	for _, s := range base {
		seen[s] = true
	}
	for _, s := range add {
		if seen[s] {
			continue
		}
		seen[s] = true
		base = append(base, s)
	}
	return base
}

// newestMtime walks dirs recursively and returns the newest mtime among files
// with one of the extensions ("" exts match every file). Missing dirs count
// as empty.
func newestMtime(dirs []string, exts ...string) time.Time {
	want := make(map[string]bool, len(exts))
	for _, e := range exts {
		want[e] = true
	}
	var newest time.Time
	for _, dir := range dirs {
		_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil //nolint:nilerr // unreadable entries are skipped, not fatal
			}
			if len(want) > 0 && !want[filepath.Ext(path)] {
				return nil
			}
			if info.ModTime().After(newest) {
				newest = info.ModTime()
			}
			return nil
		})
	}
	return newest
}

// collectLocalDirs unions the LocalPackageDirs of all scanned pages, sorted.
// Returns nil when no page imports a local package.
func collectLocalDirs(pages []helpers.WasmPage) []string {
	seen := make(map[string]struct{})
	for _, p := range pages {
		for _, d := range p.LocalPackageDirs {
			seen[d] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	dirs := make([]string, 0, len(seen))
	for d := range seen {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	return dirs
}

// CollectLocalDirs is collectLocalDirs for the host layer (which owns the
// page scan through the same helper).
func CollectLocalDirs(pages []helpers.WasmPage) []string {
	return collectLocalDirs(pages)
}
