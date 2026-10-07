package browser

// Capture: stills and recorded takes over the kept-alive page.
//
// The injected dev script is the trigger/annotation layer; Chrome's compositor
// is the pixel layer. Stills are compositor screenshots (one per requested
// viewport width) paired with a DOM manifest produced by a single page.Eval —
// the manifest shape is what cli/internal/pagemap consumes ({scroll, rects}).
// Takes are CDP screencast frames (JPEG, real timestamps) written to disk with
// a timeline manifest (frames + act marks + network events + keyframes).
//
// Takes never hold the manager mutex: the frame pump runs on its own
// goroutines so callers keep driving interactions through Act while recording.
// Safety valves — an auto-stop budget, a frame ceiling that drops the oldest
// frames, automatic close of a previous take, and cleanup at dev-session end —
// all live here so a runaway recording can never outgrow its welcome.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// Capture defaults and locked thresholds.
const (
	// DefaultCaptureDir is where stills and takes land when the caller gives
	// no directory. Relative to the CLI process working directory (the user's
	// project root in a dev session), beside the other .gothicCli state.
	DefaultCaptureDir = ".gothicCli/mcp"

	// DefaultRecordMaxDuration is the auto-stop budget for a take. A take
	// never runs longer than this without an explicit Stop.
	DefaultRecordMaxDuration = 60 * time.Second

	// DefaultRecordMaxFrames is the frame ceiling: beyond it the OLDEST
	// frames are dropped so a long busy take stays bounded on disk.
	DefaultRecordMaxFrames = 1200

	// DefaultRecordQuality is the JPEG quality for screencast frames.
	DefaultRecordQuality = 80

	// DefaultStillMaxRects caps the DOM manifest element count.
	DefaultStillMaxRects = 400

	// FrameCutDelta is the mean per-channel pixel delta (0..255) above which
	// a frame counts as a scene cut (a keyframe).
	FrameCutDelta = 8.0

	// FrameQuietDelta is the mean per-channel pixel delta below which a frame
	// counts as visually stable.
	FrameQuietDelta = 2.0

	// FrameStableRun is how many consecutive stable frames end a take
	// (settle detection).
	FrameStableRun = 4

	// FrameQuietWindow is the no-frame settle window: with no new frames
	// arriving for this long (the compositor emits nothing while the page is
	// static) the take counts as settled. Frame-based settle (FrameStableRun
	// consecutive stable frames) handles pages that repaint steadily.
	FrameQuietWindow = 1500 * time.Millisecond

	// takeMarkPoll is how often the recorder pulls act marks from the
	// injected script's bus batcher.
	takeMarkPoll = 100 * time.Millisecond

	// takeNetworkCap bounds the network event list inside one manifest.
	takeNetworkCap = 500

	// end reason strings used in the take manifest
	endReasonSettled = "settled"
	endReasonStopped = "stopped"
	endReasonTimeout = "auto-stop"
	endReasonSession = "session-end"
	endReasonBrowser = "browser-closed"
	endReasonError   = "error"
)

// StillOptions configures a stills capture.
type StillOptions struct {
	// Widths is one viewport width per still. Empty captures only the
	// current viewport. Heights are inherited from the current viewport.
	Widths []int

	// URL navigates there first (allowlist-gated). Empty captures the page
	// the managed tab already shows.
	URL string

	// Dir is the output directory. Empty takes DefaultCaptureDir.
	Dir string
}

// StillResult is one still that landed on disk.
type StillResult struct {
	Width        int        `json:"width"`
	Height       int        `json:"height"`
	PNGPath      string     `json:"png"`
	ManifestPath string     `json:"manifest"`
	URL          string     `json:"url"`
	Scroll       [2]float64 `json:"scroll"`
}

// CaptureStill navigates (optionally) and screenshots the compositor once per
// requested viewport width, writing <png> and a DOM manifest next to it.
// The manifest is produced by ONE page.Eval and matches the shape
// cli/internal/pagemap consumes; it additionally carries the URL and the
// viewport size (extra keys pagemap ignores).
func (m *Manager) CaptureStill(ctx context.Context, opts StillOptions) ([]StillResult, error) {
	dir := opts.Dir
	if dir == "" {
		dir = DefaultCaptureDir
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create capture dir %s: %w", dir, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	page, err := m.pageLocked()
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = m.sessionCtx
	}

	if opts.URL != "" {
		if err := m.checkURL(opts.URL); err != nil {
			return nil, err
		}
		if err := m.gotoLocked(page, opts.URL); err != nil {
			return nil, err
		}
	}

	// Remember the viewport so every width can be restored afterwards.
	var baseOverride proto.EmulationSetDeviceMetricsOverride
	hasBase := page.LoadState(&baseOverride)
	base, err := viewportDims(page)
	if err != nil {
		return nil, err
	}
	widths := opts.Widths
	if len(widths) == 0 {
		widths = []int{int(base[0])}
	}

	var out []StillResult
	for _, w := range widths {
		res, err := m.captureStillWidth(ctx, page, dir, int(w), int(base[1]), hasBase, baseOverride)
		if err != nil {
			_ = restoreViewport(page, hasBase, baseOverride)
			return out, fmt.Errorf("capture still at width %d: %w", w, err)
		}
		out = append(out, *res)
	}

	// Restore whatever the viewport was before the stills.
	_ = restoreViewport(page, hasBase, baseOverride)
	m.armIdleLocked()
	return out, nil
}

// restoreViewport re-pins the caller's prior CDP device-metrics override
// (nil when there was none) after a still capture. Every exit path of
// CaptureStills — including the mid-loop error returns — goes through it, so
// a failed capture cannot clear a pinned viewport: the manager only re-applies
// the pin on relaunch, which would leave the session stuck at the last width.
func restoreViewport(page *rod.Page, hasBase bool, base proto.EmulationSetDeviceMetricsOverride) error {
	if hasBase {
		return page.SetViewport(&base)
	}
	return page.SetViewport(nil)
}

// captureStillWidth takes one screenshot at one viewport width. Callers hold
// m.mu. The caller's ctx bounds the whole capture; the restore below always
// runs even on failure, so a mid-capture error cannot clear the manager's
// pinned viewport (it is only re-applied on relaunch).
func (m *Manager) captureStillWidth(ctx context.Context, page *rod.Page, dir string, width, height int, hasBase bool, baseOverride proto.EmulationSetDeviceMetricsOverride) (*StillResult, error) {
	p := page.Context(ctx)
	if width > 0 {
		set := proto.EmulationSetDeviceMetricsOverride{
			Width:             width,
			Height:            height,
			DeviceScaleFactor: 1,
			Mobile:            false,
		}
		if err := set.Call(p); err != nil {
			return nil, fmt.Errorf("set viewport %dx%d: %w", width, height, err)
		}
	}
	// Two animation frames so the relayout+repaint for the new metrics is on
	// screen before the compositor screenshot. A headful window that sits off
	// the active workspace never gets composited, so requestAnimationFrame
	// never fires there and the wait would run into the caller's whole budget;
	// race the pair against a short timer and take the screenshot either way
	// — the relayout wait is best-effort, the compositor is the pixel layer.
	if _, err := p.Eval(`() => Promise.race([
	  new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r))),
	  new Promise(r => setTimeout(r, 500))
	])`); err != nil {
		_ = restoreViewport(p, hasBase, baseOverride)
		return nil, fmt.Errorf("wait for relayout: %w", err)
	}

	shot, err := p.Screenshot(false, &proto.PageCaptureScreenshot{
		Format:                proto.PageCaptureScreenshotFormatPng,
		CaptureBeyondViewport: false,
	})
	if err != nil {
		_ = restoreViewport(p, hasBase, baseOverride)
		return nil, fmt.Errorf("compositor screenshot: %w", err)
	}

	manifest, err := pageManifest(p)
	if err != nil {
		_ = restoreViewport(p, hasBase, baseOverride)
		return nil, fmt.Errorf("page manifest: %w", err)
	}

	name := fmt.Sprintf("still_%d", width)
	pngPath := filepath.Join(dir, name+".png")
	manifestPath := filepath.Join(dir, name+".manifest.json")
	if err := os.WriteFile(pngPath, shot, 0o644); err != nil {
		_ = restoreViewport(p, hasBase, baseOverride)
		return nil, fmt.Errorf("write %s: %w", pngPath, err)
	}
	if err := os.WriteFile(manifestPath, manifest, 0o644); err != nil {
		_ = restoreViewport(p, hasBase, baseOverride)
		return nil, fmt.Errorf("write %s: %w", manifestPath, err)
	}

	var meta struct {
		Scroll   [2]float64 `json:"scroll"`
		URL      string     `json:"url"`
		Viewport [2]float64 `json:"viewport"`
	}
	_ = json.Unmarshal(manifest, &meta)
	return &StillResult{
		Width:        width,
		Height:       int(meta.Viewport[1]),
		PNGPath:      pngPath,
		ManifestPath: manifestPath,
		URL:          meta.URL,
		Scroll:       meta.Scroll,
	}, nil
}

// viewportDims reads the current viewport size with one Eval.
func viewportDims(page *rod.Page) ([2]float64, error) {
	res, err := page.Eval(`() => JSON.stringify({w: innerWidth, h: innerHeight})`)
	if err != nil {
		return [2]float64{}, fmt.Errorf("read viewport: %w", err)
	}
	var raw struct {
		W, H float64
	}
	if err := json.Unmarshal([]byte(res.Value.Str()), &raw); err != nil {
		return [2]float64{}, fmt.Errorf("parse viewport: %w", err)
	}
	return [2]float64{raw.W, raw.H}, nil
}

// pageManifest produces the DOM manifest with one Eval: scroll, plus for every
// visible element its viewport rect and computed colours, keyed exactly as
// cli/internal/pagemap consumes ({sel,x,y,w,h,bg,fg,overflows}). Invisible and
// degenerate clutter is filtered here (the capture layer's job); extra keys
// (url, viewport) ride along for the capture metadata contract.
const pageManifestJS = `() => {
  const MAX = 400;
  function cssPath(el) {
    const parts = [];
    let depth = 0;
    while (el && el.nodeType === 1 && depth < 6 && parts.join(" > ").length < 120) {
      let sel = el.nodeName.toLowerCase();
      if (el.id) { parts.unshift(sel + "#" + el.id); break; }
      const cl = (el.classList && el.classList.length)
        ? "." + Array.from(el.classList).slice(0, 2).join(".")
        : "";
      parts.unshift(sel + cl);
      el = el.parentElement;
      depth++;
    }
    let out = parts.join(" > ");
    if (out.length > 120) out = out.slice(-120);
    return out || "*";
  }
  const rects = [];
  const all = document.body ? document.body.querySelectorAll("*") : [];
  for (let i = 0; i < all.length && rects.length < MAX; i++) {
    const el = all[i];
    const cs = getComputedStyle(el);
    if (cs.display === "none" || cs.visibility === "hidden" || cs.opacity === "0") continue;
    const r = el.getBoundingClientRect();
    if (r.width < 2 || r.height < 2) continue;
    if (r.bottom < 0 || r.right < 0 || r.top > innerHeight || r.left > innerWidth) continue;
    rects.push({
      sel: cssPath(el),
      x: r.left, y: r.top, w: r.width, h: r.height,
      bg: cs.backgroundColor,
      fg: cs.color,
      overflows: el.scrollWidth > el.clientWidth
    });
  }
  return JSON.stringify({
    scroll: [window.scrollX, window.scrollY],
    url: location.href,
    viewport: [innerWidth, innerHeight],
    rects: rects
  });
}`

// pageManifest runs the manifest Eval and returns its JSON bytes.
func pageManifest(page *rod.Page) ([]byte, error) {
	res, err := page.Eval(pageManifestJS)
	if err != nil {
		return nil, err
	}
	return []byte(res.Value.Str()), nil
}

// ── Recorded takes ──────────────────────────────────────────────────────────

// TakeMark is one act marker (or settle signal) the injected dev script
// observed, pulled from the dev bus ring. Shape mirrors the script's
// kind:"mark" records; t is the epoch-ms timestamp that makes the marker
// contract ("something happened 220ms after my click") computable.
type TakeMark struct {
	T       int64          `json:"t"`
	Seq     int64          `json:"seq"`
	Event   string         `json:"event"`
	Dir     string         `json:"dir"`
	Payload map[string]any `json:"payload"`
}

// MarksFromRecords filters dev-bus records down to their kind:"mark" entries
// and decodes them into TakeMarks. This is the adapter the dev session wires
// between the proxy's bus ring and RecordOptions.Marks.
func MarksFromRecords(records []map[string]any) []TakeMark {
	out := make([]TakeMark, 0, len(records))
	for _, rec := range records {
		if kind, _ := rec["kind"].(string); kind != "mark" {
			continue
		}
		raw, err := json.Marshal(rec)
		if err != nil {
			continue
		}
		var mk TakeMark
		if err := json.Unmarshal(raw, &mk); err != nil {
			continue
		}
		out = append(out, mk)
	}
	return out
}

// networkEvent is one entry in a take's network timeline.
type networkEvent struct {
	T      int64  `json:"t"`
	Event  string `json:"event"` // "request" | "response"
	Method string `json:"method,omitempty"`
	URL    string `json:"url"`
	Status int    `json:"status,omitempty"`
}

// FrameInfo is one screencast frame in a take manifest.
type FrameInfo struct {
	File     string  `json:"file"`     // frame_NNNNN.jpg inside the take dir
	T        int64   `json:"t"`        // epoch ms (CDP frame swap timestamp when present)
	Delta    float64 `json:"delta"`    // mean pixel delta vs the previous frame
	Keyframe bool    `json:"keyframe"` // scene cut (delta >= FrameCutDelta)
	ScrollX  float64 `json:"scroll_x,omitempty"`
	ScrollY  float64 `json:"scroll_y,omitempty"`
	W        int     `json:"w"`
	H        int     `json:"h"`
}

// takeTimeline is a take's frame list with its deltas kept index-aligned and
// the frame ceiling applied on every push: past the cap the OLDEST entries
// leave the list first, and push returns them so the caller can delete their
// files. No I/O — pure structure, exercised by the fixture tests.
type takeTimeline struct {
	Frames    []FrameInfo
	Deltas    []float64
	maxFrames int
}

// push appends one frame (+ its delta) and applies the ceiling.
func (tt *takeTimeline) push(info FrameInfo, delta float64) (dropped []FrameInfo) {
	tt.Frames = append(tt.Frames, info)
	tt.Deltas = append(tt.Deltas, delta)
	for len(tt.Frames) > tt.maxFrames {
		dropped = append(dropped, tt.Frames[0])
		tt.Frames = tt.Frames[1:]
		tt.Deltas = tt.Deltas[1:]
	}
	return dropped
}

// TakeNavigation is one navigation observed during a take (timestamp only —
// transitions are where bugs live, so they are part of the timeline).
type TakeNavigation struct {
	T int64 `json:"t"`
}

// TakeManifest is the timeline manifest written next to a take's frames.
type TakeManifest struct {
	ID          string           `json:"id"`
	URL         string           `json:"url"`
	StartedAt   int64            `json:"started_at"`
	EndedAt     int64            `json:"ended_at"`
	DurationMs  int64            `json:"duration_ms"`
	EndReason   string           `json:"end_reason"`
	Settled     bool             `json:"settled"`
	Dropped     int              `json:"dropped"`
	Frames      []FrameInfo      `json:"frames"`
	Keyframes   []string         `json:"keyframes"`
	Marks       []TakeMark       `json:"marks"`
	Network     []networkEvent   `json:"network"`
	Navigations []TakeNavigation `json:"navigations"`
	Warnings    []string         `json:"warnings,omitempty"`
	Discarded   bool             `json:"discarded,omitempty"`
}

// RecordOptions configures a recorded take.
type RecordOptions struct {
	// BaseDir under which rec_<id>/ is created. Empty takes DefaultCaptureDir.
	BaseDir string

	// MaxDuration is the auto-stop budget. <=0 takes the 60s default.
	MaxDuration time.Duration

	// MaxFrames is the ceiling; beyond it the oldest frames are dropped.
	// <=0 takes the 1200 default.
	MaxFrames int

	// Quality is the JPEG quality for frames. <=0 takes 80.
	Quality int

	// MaxWidth/MaxHeight optionally cap the screencast frame size.
	MaxWidth, MaxHeight int

	// Marks pulls the newest act marks from the dev bus ring (newest first).
	// The recorder keeps everything not yet consumed. Nil means no marks.
	Marks func() []TakeMark
}

// Take is one active (or finished) recording. The manifest is readable after
// the take's Done channel closes.
type Take struct {
	ID        string
	Dir       string
	mgr       *Manager
	startURL  string
	ctx       context.Context
	cancel    context.CancelFunc
	startedAt time.Time
	opts      RecordOptions
	stopCh    chan stopRequest
	done      chan struct{}
	result    TakeManifest
	resultErr error
}

// stopRequest is the reason a take should end, plus any warning to record.
type stopRequest struct {
	reason  string
	warning string
}

// takeRegistry tracks each manager's active take (capture.go cannot add
// fields to Manager's struct in browser.go, so the association lives here).
var takeRegistry = struct {
	mu  sync.Mutex
	byM map[*Manager]*Take
}{byM: map[*Manager]*Take{}}

func activeTakeFor(m *Manager) *Take {
	takeRegistry.mu.Lock()
	defer takeRegistry.mu.Unlock()
	return takeRegistry.byM[m]
}

func setActiveTake(m *Manager, t *Take) {
	takeRegistry.mu.Lock()
	defer takeRegistry.mu.Unlock()
	if t == nil {
		delete(takeRegistry.byM, m)
	} else {
		takeRegistry.byM[m] = t
	}
}

// clearActiveTake removes the registry entry only when it still points at t,
// so a late finalizer can never evict a newer take's registration.
func clearActiveTake(m *Manager, t *Take) {
	takeRegistry.mu.Lock()
	defer takeRegistry.mu.Unlock()
	if takeRegistry.byM[m] == t {
		delete(takeRegistry.byM, m)
	}
}

// ActiveTake returns the manager's currently recording take, or nil.
func (m *Manager) ActiveTake() *Take {
	return activeTakeFor(m)
}

// newTakeID is a sortable, collision-safe id: timestamp + 4 random hex chars.
func newTakeID() string {
	var rnd [2]byte
	_, _ = rand.Read(rnd[:])
	return time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(rnd[:])
}

// StartRecording begins a screencast take. A take already running is closed
// automatically (its manifest keeps a warning), which is the double-start
// valve. The take cleans up after itself when the dev session context ends or
// the browser closes — a take can never outlive its manager.
func (m *Manager) StartRecording(opts RecordOptions) (*Take, error) {
	if opts.MaxDuration <= 0 {
		opts.MaxDuration = DefaultRecordMaxDuration
	}
	if opts.MaxFrames <= 0 {
		opts.MaxFrames = DefaultRecordMaxFrames
	}
	if opts.Quality <= 0 {
		opts.Quality = DefaultRecordQuality
	}
	base := opts.BaseDir
	if base == "" {
		base = DefaultCaptureDir
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Double-start valve: close the previous take with a warning first, so
	// its screencast teardown cannot race the new one.
	if prev := activeTakeFor(m); prev != nil {
		prev.shutdown(stopRequest{
			reason:  endReasonStopped,
			warning: "closed automatically: a new recording started",
		}, 5*time.Second)
	}

	page, err := m.pageLocked()
	if err != nil {
		return nil, err
	}

	id := newTakeID()
	dir := filepath.Join(base, "rec_"+id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create take dir %s: %w", dir, err)
	}

	pageURL := ""
	if res, err := page.Eval(`() => location.href`); err == nil {
		pageURL = res.Value.Str()
	}

	ctx, cancel := context.WithCancel(m.sessionCtx)
	maxW, maxH := opts.MaxWidth, opts.MaxHeight
	start := proto.PageStartScreencast{
		Format:    proto.PageStartScreencastFormatJpeg,
		Quality:   &opts.Quality,
		MaxWidth:  &maxW,
		MaxHeight: &maxH,
	}
	if maxW <= 0 {
		start.MaxWidth = nil
	}
	if maxH <= 0 {
		start.MaxHeight = nil
	}
	if err := start.Call(page); err != nil {
		cancel()
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("start screencast: %w", err)
	}

	t := &Take{
		ID:        "rec_" + id,
		Dir:       dir,
		mgr:       m,
		startURL:  pageURL,
		startedAt: time.Now(),
		opts:      opts,
		stopCh:    make(chan stopRequest, 1),
		done:      make(chan struct{}),
	}
	t.ctx, t.cancel = ctx, cancel
	setActiveTake(m, t)

	go t.run(page)
	return t, nil
}

// Stop finalizes the take and returns its timeline manifest. With discard,
// the take directory is deleted and no frames are returned.
func (t *Take) Stop(discard bool) (TakeManifest, error) {
	select {
	case t.stopCh <- stopRequest{reason: endReasonStopped}:
	default:
	}
	<-t.done
	res := t.result
	if discard {
		_ = os.RemoveAll(t.Dir)
		res = TakeManifest{
			ID:        t.ID,
			StartedAt: res.StartedAt,
			EndedAt:   res.EndedAt,
			Discarded: true,
		}
	}
	return res, t.resultErr
}

// Wait blocks until the take finalizes on its own (settle, auto-stop, session
// end or browser close) and returns the manifest.
func (t *Take) Wait() (TakeManifest, error) {
	<-t.done
	return t.result, t.resultErr
}

// Done exposes the take's completion channel.
func (t *Take) Done() <-chan struct{} { return t.done }

// shutdown asks the take to end and waits for its finalizer. Used by the
// double-start valve; Stop is the caller-facing version.
func (t *Take) shutdown(req stopRequest, within time.Duration) {
	select {
	case t.stopCh <- req:
	default:
	}
	select {
	case <-t.done:
	case <-time.After(within):
	}
}

// run is the take's main loop. It consumes compositor frames, watches for
// navigations (re-arming the screencast), polls act marks, and applies the
// safety valves. page is the live page at take start; if the browser goes
// away mid-take the pump channel closes and the take finalizes.
func (t *Take) run(page *rod.Page) {
	defer close(t.done)
	defer clearActiveTake(t.mgr, t)
	defer t.cancel() // release the take's context either way

	ctx, cancel := context.WithTimeout(t.ctx, t.opts.MaxDuration)
	defer cancel()

	frameCh := make(chan *proto.PageScreencastFrame, 256)
	navCh := make(chan int64, 8)
	netCh := make(chan networkEvent, 256)
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		wait := page.EachEvent(
			func(f *proto.PageScreencastFrame) bool {
				select {
				case frameCh <- f:
				default: // the ceiling valve exists precisely for flood; never block the event pump
				}
				return false
			},
			func(e *proto.PageFrameStartedLoading) bool {
				select {
				case navCh <- time.Now().UnixMilli():
				default:
				}
				return false
			},
			func(e *proto.NetworkRequestWillBeSent) bool {
				if e.Request.URL == "" || isFrameworkPath(e.Request.URL) {
					return false
				}
				select {
				case netCh <- networkEvent{T: time.Now().UnixMilli(), Event: "request", Method: e.Request.Method, URL: e.Request.URL}:
				default:
				}
				return false
			},
			func(e *proto.NetworkResponseReceived) bool {
				u := e.Response.URL
				if u == "" || isFrameworkPath(u) {
					return false
				}
				select {
				case netCh <- networkEvent{T: time.Now().UnixMilli(), Event: "response", URL: u, Status: int(e.Response.Status)}:
				default:
				}
				return false
			},
		)
		wait() // pumps events until a callback returns true (never) or the page ends
	}()

	// Keep the idle-close timer pushed back for the take's lifetime: an idle
	// browser mid-take would tear down the page under the recorder. The tick
	// must be shorter than the shortest idle window (tests use 500ms).
	keepDone := make(chan struct{})
	defer close(keepDone)
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-keepDone:
				return
			case <-ticker.C:
				if m := t.mgr; m != nil {
					m.mu.Lock()
					m.armIdleLocked()
					m.mu.Unlock()
				}
			}
		}
	}()

	var (
		keyframes []string
		marks     []TakeMark
		net       []networkEvent
		navs      []TakeNavigation
		warnings  []string
		prev      image.Image
		stableRun int
		dropped   int
		reason    string
		lastMarkT int64
	)
	startMs := t.startedAt.UnixMilli()
	lastActivity := t.startedAt
	seq := 0

	// The take timeline: frames, their deltas (index-aligned), and the frame
	// ceiling applied on every push (dropped entries are removed from disk by
	// the caller). Pure structure, no I/O — testable against fixtures.
	timeline := &takeTimeline{maxFrames: t.opts.MaxFrames}
	emit := func(info FrameInfo, delta float64) {
		for _, victim := range timeline.push(info, delta) {
			_ = os.Remove(filepath.Join(t.Dir, victim.File))
			dropped++
		}
		if info.Keyframe {
			keyframes = append(keyframes, info.File)
		}
	}

	for reason == "" {
		select {
		case req := <-t.stopCh:
			reason = req.reason
			if req.warning != "" {
				warnings = append(warnings, req.warning)
			}

		case <-t.ctx.Done():
			reason = endReasonSession

		case <-ctx.Done():
			reason = endReasonTimeout

		case <-pumpDone:
			reason = endReasonBrowser

		case navMs := <-navCh:
			// Re-arm the screencast on EVERY navigation: transitions are
			// where bugs live, and the compositor stream can stall across
			// them. The previous frame is no longer comparable, so the delta
			// baseline resets rather than recording a phantom cut.
			if err := startScreencast(page, t.opts); err != nil {
				warnings = append(warnings, fmt.Sprintf("re-arm screencast after navigation: %v", err))
			}
			prev = nil
			timeline.Deltas = nil
			stableRun = 0
			navs = append(navs, TakeNavigation{T: navMs})

		case f := <-frameCh:
			// The compositor stops streaming until each frame is acked.
			ackCtx, ackCancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = proto.PageScreencastFrameAck{SessionID: f.SessionID}.Call(page.Context(ackCtx))
			ackCancel()
			seq++
			info, baseline, err := decodeFrame(f, seq)
			if err != nil {
				warnings = append(warnings, err.Error())
				continue
			}
			delta := frameDelta(prev, baseline)
			prev = baseline
			lastActivity = time.Now()
			stableRun = nextStableRun(stableRun, delta)
			info.Delta = delta
			info.Keyframe = delta >= FrameCutDelta
			if wErr := os.WriteFile(filepath.Join(t.Dir, info.File), f.Data, 0o644); wErr != nil {
				warnings = append(warnings, wErr.Error())
				continue
			}
			emit(info, delta)
			if stableRun >= FrameStableRun {
				reason = endReasonSettled
			}

		case ev := <-netCh:
			net = append(net, ev)
			if len(net) > takeNetworkCap {
				net = net[len(net)-takeNetworkCap:]
			}

		case <-time.After(takeMarkPoll / 2):
			if t.opts.Marks != nil {
				var fresh []TakeMark
				fresh, lastMarkT = t.pullMarks(lastMarkT, startMs)
				marks = append(marks, fresh...)
			}
			// No-frame settle: a static page emits no compositor frames, so
			// silence itself is a settled signal once the take has been
			// alive long enough for the driver to start acting.
			if time.Since(lastActivity) >= FrameQuietWindow &&
				time.Since(t.startedAt) >= 2*FrameQuietWindow {
				reason = endReasonSettled
			}
		}
	}

	// Final marks pull so the tail of the interaction is in the manifest.
	if t.opts.Marks != nil {
		var fresh []TakeMark
		fresh, lastMarkT = t.pullMarks(lastMarkT, startMs)
		marks = append(marks, fresh...)
	}

	// Stop the compositor stream; the page may already be gone.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = proto.PageStopScreencast{}.Call(page.Context(stopCtx))
	stopCancel()

	ended := time.Now()
	res := TakeManifest{
		ID:          t.ID,
		URL:         t.url(page),
		StartedAt:   startMs,
		EndedAt:     ended.UnixMilli(),
		DurationMs:  ended.Sub(t.startedAt).Milliseconds(),
		EndReason:   reason,
		Settled:     reason == endReasonSettled,
		Dropped:     dropped,
		Frames:      timeline.Frames,
		Keyframes:   keyframes,
		Marks:       marks,
		Network:     net,
		Navigations: navs,
		Warnings:    warnings,
	}
	if err := writeTakeManifest(filepath.Join(t.Dir, "manifest.json"), &res); err != nil {
		t.resultErr = err
	}
	t.result = res
}

// pullMarks consumes one marks batch from the source and returns the marks
// not yet taken, plus the advanced high-water timestamp. The bus ring hands
// the batch over NEWEST first, so the batch is re-sorted ascending by
// timestamp (seq as tiebreak) before the monotonic filter: feeding a
// newest-first batch into a `t > last` filter would keep only the newest mark
// and drop the rest of a fill→click→key burst. Marks older than the take's
// start are always excluded.
func (t *Take) pullMarks(lastMarkT, startMs int64) ([]TakeMark, int64) {
	pulled := t.opts.Marks()
	sort.SliceStable(pulled, func(i, j int) bool {
		if pulled[i].T != pulled[j].T {
			return pulled[i].T < pulled[j].T
		}
		return pulled[i].Seq < pulled[j].Seq
	})
	var out []TakeMark
	for _, mk := range pulled {
		if mk.T <= lastMarkT || mk.T < startMs {
			continue
		}
		out = append(out, mk)
		lastMarkT = mk.T
	}
	return out, lastMarkT
}

// url returns the page URL at finalize time, falling back to the take-start
// snapshot when the page is already gone.
func (t *Take) url(page *rod.Page) string {
	if res, err := page.Eval(`() => location.href`); err == nil {
		if u := res.Value.Str(); u != "" {
			return u
		}
	}
	return t.startURL
}

// startScreencast (re)arms the compositor stream with the take's options.
func startScreencast(page *rod.Page, opts RecordOptions) error {
	start := proto.PageStartScreencast{
		Format:    proto.PageStartScreencastFormatJpeg,
		Quality:   &opts.Quality,
		MaxWidth:  &opts.MaxWidth,
		MaxHeight: &opts.MaxHeight,
	}
	if opts.MaxWidth <= 0 {
		start.MaxWidth = nil
	}
	if opts.MaxHeight <= 0 {
		start.MaxHeight = nil
	}
	return start.Call(page)
}

// decodeFrame decodes one screencast frame, fills its metadata, and returns
// the decoded image as the new delta baseline.
func decodeFrame(f *proto.PageScreencastFrame, seq int) (info FrameInfo, baseline image.Image, err error) {
	img, dErr := jpeg.Decode(bytes.NewReader(f.Data))
	if dErr != nil {
		return FrameInfo{}, nil, fmt.Errorf("decoding screencast frame %d: %v", seq, dErr)
	}
	b := img.Bounds()
	info = FrameInfo{
		W: b.Dx(),
		H: b.Dy(),
	}
	if f.Metadata != nil {
		info.ScrollX = f.Metadata.ScrollOffsetX
		info.ScrollY = f.Metadata.ScrollOffsetY
		if f.Metadata.Timestamp > 0 {
			info.T = int64(f.Metadata.Timestamp * 1000) // CDP seconds-since-epoch → ms
		}
	}
	if info.T == 0 {
		info.T = time.Now().UnixMilli()
	}
	info.File = fmt.Sprintf("frame_%05d.jpg", seq)
	return info, img, nil
}

// nextStableRun extends the consecutive-stable counter with one new delta.
func nextStableRun(run int, delta float64) int {
	if delta < FrameQuietDelta {
		return run + 1
	}
	return 0
}

// frameDelta returns the mean per-channel absolute difference (0..255) between
// two frames, sampled on a coarse grid. A nil or resized previous frame is
// "fully changed" — the recorder must not read a false calm into a transition.
func frameDelta(prev, cur image.Image) float64 {
	if prev == nil || cur == nil {
		return 255
	}
	sa, sb := prev.Bounds().Size(), cur.Bounds().Size()
	if sa != sb {
		return 255
	}
	const stride = 4
	var sum int64
	var n int64
	for y := prev.Bounds().Min.Y; y < prev.Bounds().Max.Y; y += stride {
		for x := prev.Bounds().Min.X; x < prev.Bounds().Max.X; x += stride {
			pr, pg, pb, _ := prev.At(x, y).RGBA()
			cr, cg, cb, _ := cur.At(x, y).RGBA()
			sum += abs64(int(pr>>8)-int(cr>>8)) + abs64(int(pg>>8)-int(cg>>8)) + abs64(int(pb>>8)-int(cb>>8))
			n += 3
		}
	}
	if n == 0 {
		return 255
	}
	return float64(sum) / float64(n)
}

func abs64(v int) int64 {
	if v < 0 {
		return int64(-v)
	}
	return int64(v)
}

// KeyframeFlags marks the frames that start a new visual scene: the first
// frame, and any frame whose delta from the previous one reaches cut.
// Pure; exercised against fixture delta sequences.
func KeyframeFlags(deltas []float64, cut float64) []bool {
	flags := make([]bool, len(deltas))
	for i, d := range deltas {
		flags[i] = i == 0 || d >= cut
	}
	return flags
}

// SettleIndex returns the index of the first frame whose trailing run of
// stableN consecutive deltas below quiet completes — the point where a take
// ends. -1 when the sequence never settles.
func SettleIndex(deltas []float64, quiet float64, stableN int) int {
	run := 0
	for i, d := range deltas {
		if d < quiet {
			run++
			if run >= stableN {
				return i
			}
		} else {
			run = 0
		}
	}
	return -1
}

// writeTakeManifest serializes the timeline manifest (json.MarshalIndent so
// the on-disk artifact is developer-readable).
func writeTakeManifest(path string, m *TakeManifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding take manifest: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// readTakeManifest decodes a manifest.json from disk.
func readTakeManifest(path string) (*TakeManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var m TakeManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", path, err)
	}
	return &m, nil
}

// isFrameworkPath filters the dev proxy's own traffic out of a take's
// network timeline (the observer's bus POSTs, the reload script itself).
func isFrameworkPath(url string) bool {
	return strings.Contains(url, "/_gothicframework/")
}
