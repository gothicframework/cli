package browser

// Capture tests. The pure layer — pixel deltas, keyframe flags, settle
// detection, the frame ceiling and the manifest round trip — runs against
// fixture sequences with no browser. The launch-dependent tests exercise the
// real capture API against local httptest pages and skip when the machine has
// no browser installed (rod must never download one inside a test run).

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// solidImage builds an RGBA image painted in one colour.
func solidImage(w, h int, c color.Color) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}

func TestFrameDelta(t *testing.T) {
	a := solidImage(64, 48, color.RGBA{R: 10, G: 10, B: 10, A: 255})

	if got := frameDelta(a, a); got != 0 {
		t.Fatalf("identical frames: delta = %v, want 0", got)
	}

	// A few bright pixels on a dark field: small but non-zero delta.
	b := solidImage(64, 48, color.RGBA{R: 10, G: 10, B: 10, A: 255})
	for x := 0; x < 16; x++ {
		b.Set(x, 0, color.RGBA{R: 250, G: 250, B: 250, A: 255})
	}
	if got := frameDelta(a, b); got <= 0 || got >= FrameCutDelta {
		t.Fatalf("small change: delta = %v, want 0 < d < %v", got, FrameCutDelta)
	}

	// Fully different frame: above the cut threshold.
	c := solidImage(64, 48, color.RGBA{R: 240, G: 240, B: 240, A: 255})
	if got := frameDelta(a, c); got < FrameCutDelta {
		t.Fatalf("different frames: delta = %v, want >= %v", got, FrameCutDelta)
	}

	// Size mismatch and nil baselines read as "fully changed".
	if got := frameDelta(a, solidImage(32, 48, color.Black)); got != 255 {
		t.Fatalf("size mismatch: delta = %v, want 255", got)
	}
	if got := frameDelta(nil, a); got != 255 {
		t.Fatalf("nil prev: delta = %v, want 255", got)
	}
}

func TestKeyframeFlags(t *testing.T) {
	deltas := []float64{255, 1, 2, 9, 1, 1, 8}
	flags := KeyframeFlags(deltas, FrameCutDelta)
	want := []bool{true, false, false, true, false, false, true}
	for i := range want {
		if flags[i] != want[i] {
			t.Fatalf("flags[%d] = %v, want %v (deltas %v)", i, flags[i], want[i], deltas)
		}
	}
}

func TestSettleIndex(t *testing.T) {
	// Four consecutive quiet deltas (indices 2–5): settle completes at index 5.
	deltas := []float64{255, 20, 1, 1, 1, 1, 9, 1, 1, 1, 1}
	if got := SettleIndex(deltas, FrameQuietDelta, FrameStableRun); got != 5 {
		t.Fatalf("SettleIndex = %d, want 5", got)
	}

	// A run interrupted by movement never settles.
	deltas = []float64{255, 1, 1, 1, 9, 1, 1, 1}
	if got := SettleIndex(deltas, FrameQuietDelta, FrameStableRun); got != -1 {
		t.Fatalf("interrupted run: SettleIndex = %d, want -1", got)
	}

	// Fewer stable frames than the run length: no settle.
	deltas = []float64{255, 1, 1, 1}
	if got := SettleIndex(deltas, FrameQuietDelta, FrameStableRun); got != -1 {
		t.Fatalf("short run: SettleIndex = %d, want -1", got)
	}
}

func TestTakeTimelineCeiling(t *testing.T) {
	tt := &takeTimeline{maxFrames: 5}
	for i := 1; i <= 8; i++ {
		tt.push(FrameInfo{File: fmt.Sprintf("frame_%05d.jpg", i), T: int64(i)}, float64(i%10))
	}
	if len(tt.Frames) != 5 || len(tt.Deltas) != 5 {
		t.Fatalf("timeline not capped: %d frames, %d deltas", len(tt.Frames), len(tt.Deltas))
	}
	// Oldest five dropped; newest five kept in order.
	if tt.Frames[0].File != "frame_00004.jpg" || tt.Frames[4].File != "frame_00008.jpg" {
		t.Fatalf("kept the wrong frames: first=%s last=%s", tt.Frames[0].File, tt.Frames[4].File)
	}
	// Deltas stay index-aligned with the frames.
	for i, fr := range tt.Frames {
		wantDelta := float64((int(fr.T)) % 10)
		if tt.Deltas[i] != wantDelta {
			t.Fatalf("deltas misaligned at %d: %v vs %v", i, tt.Deltas[i], wantDelta)
		}
	}
}

func TestTakeManifestRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	in := &TakeManifest{
		ID:         "rec_20260101-120000-ab12",
		URL:        "http://localhost:3000/",
		StartedAt:  1000,
		EndedAt:    4500,
		DurationMs: 3500,
		EndReason:  endReasonSettled,
		Settled:    true,
		Dropped:    2,
		Frames: []FrameInfo{
			{File: "frame_00001.jpg", T: 1100, Delta: 255, Keyframe: true, ScrollX: 0, ScrollY: 0, W: 800, H: 600},
			{File: "frame_00002.jpg", T: 1300, Delta: 0.5, W: 800, H: 600},
		},
		Keyframes: []string{"frame_00001.jpg"},
		Marks: []TakeMark{
			{T: 1250, Seq: 3, Event: "click", Dir: "act", Payload: map[string]any{"sel": "button#go", "x": 12.0, "y": 30.0}},
			{T: 1480, Seq: 5, Event: "settle", Dir: "act", Payload: map[string]any{"quiet_ms": 250.0}},
		},
		Network: []networkEvent{
			{T: 1200, Event: "request", Method: "GET", URL: "http://localhost:3000/api/x"},
			{T: 1250, Event: "response", URL: "http://localhost:3000/api/x", Status: 200},
		},
		Navigations: []TakeNavigation{{T: 1400}},
		Warnings:    []string{"re-arm screencast after navigation: boom"},
	}
	if err := writeTakeManifest(path, in); err != nil {
		t.Fatalf("writeTakeManifest: %v", err)
	}
	out, err := readTakeManifest(path)
	if err != nil {
		t.Fatalf("readTakeManifest: %v", err)
	}
	if out.ID != in.ID || out.URL != in.URL || !out.Settled || out.EndReason != in.EndReason ||
		out.DurationMs != in.DurationMs || out.Dropped != in.Dropped || len(out.Warnings) != 1 {
		t.Fatalf("scalar fields lost in round trip: %+v", out)
	}
	if len(out.Frames) != 2 || out.Frames[0].Keyframe != true || out.Frames[1].Delta != 0.5 {
		t.Fatalf("frames lost in round trip: %+v", out.Frames)
	}
	if len(out.Marks) != 2 || out.Marks[0].Payload["sel"] != "button#go" {
		t.Fatalf("marks lost in round trip: %+v", out.Marks)
	}
	if len(out.Network) != 2 || out.Network[1].Status != 200 {
		t.Fatalf("network lost in round trip: %+v", out.Network)
	}
	if len(out.Keyframes) != 1 || out.Keyframes[0] != "frame_00001.jpg" {
		t.Fatalf("keyframes lost in round trip: %+v", out.Keyframes)
	}
	if len(out.Navigations) != 1 || out.Navigations[0].T != 1400 {
		t.Fatalf("navigations lost in round trip: %+v", out.Navigations)
	}
}

func TestMarksFromRecords(t *testing.T) {
	records := []map[string]any{
		{"t": 1250, "seq": 3, "kind": "mark", "dir": "act", "event": "click", "topic": nil, "field": nil,
			"payload": map[string]any{"sel": "button#go", "trusted": true}},
		{"t": 1260, "seq": 4, "kind": "topic", "dir": "pub", "event": "gothic:topic:x", "topic": "x",
			"field": nil, "payload": map[string]any{"Count": 2}},
		{"t": 1500, "seq": 6, "kind": "mark", "dir": "act", "event": "settle", "topic": nil, "field": nil,
			"payload": map[string]any{"quiet_ms": 250}},
	}
	marks := MarksFromRecords(records)
	if len(marks) != 2 {
		t.Fatalf("got %d marks, want 2", len(marks))
	}
	if marks[0].Event != "click" || marks[0].Seq != 3 || marks[0].T != 1250 {
		t.Fatalf("first mark misdecoded: %+v", marks[0])
	}
	if marks[0].Payload["sel"] != "button#go" {
		t.Fatalf("payload misdecoded: %+v", marks[0].Payload)
	}
	if marks[1].Event != "settle" {
		t.Fatalf("second mark misdecoded: %+v", marks[1])
	}
}

// TestPullMarksNewestFirstBatch covers the bus-ring handoff: Records(-1)
// serves the batch newest first, so the pull must re-sort ascending before
// the monotonic filter — otherwise a fill→click→key burst collapses to the
// single newest mark.
func TestPullMarksNewestFirstBatch(t *testing.T) {
	const startMs = 1000
	batch := []TakeMark{
		{T: 1500, Seq: 9, Event: "settle", Dir: "act"},
		{T: 1400, Seq: 7, Event: "keyup", Dir: "act"},
		{T: 1200, Seq: 4, Event: "click", Dir: "act"},
		{T: 1100, Seq: 2, Event: "fill", Dir: "act"},
		{T: 900, Seq: 1, Event: "before-take", Dir: "act"}, // outside the window
	}
	tk := &Take{opts: RecordOptions{Marks: func() []TakeMark { return batch }}}

	fresh, last := tk.pullMarks(0, startMs)
	if len(fresh) != 4 {
		t.Fatalf("got %d marks from a 4-in-window batch, want all 4: %+v", len(fresh), fresh)
	}
	for i, want := range []int64{1100, 1200, 1400, 1500} {
		if fresh[i].T != want {
			t.Fatalf("mark[%d].T = %d, want ascending %d: %+v", i, fresh[i].T, want, fresh)
		}
	}
	if last != 1500 {
		t.Fatalf("high-water T = %d, want 1500", last)
	}

	// A second pull of the same ring yields nothing new: the monotonic
	// dedup stays correct across polls.
	fresh2, last2 := tk.pullMarks(last, startMs)
	if len(fresh2) != 0 || last2 != last {
		t.Fatalf("re-pull leaked %d marks or moved the high-water: %+v last=%d", len(fresh2), fresh2, last2)
	}

	// Ties on T break by seq; the monotonic dedup keys on T alone, so of a
	// same-T pair only the lower-seq mark survives — deterministically.
	tied := []TakeMark{{T: 1200, Seq: 8, Event: "b"}, {T: 1200, Seq: 5, Event: "a"}}
	tk2 := &Take{opts: RecordOptions{Marks: func() []TakeMark { return tied }}}
	fresh3, _ := tk2.pullMarks(0, startMs)
	if len(fresh3) != 1 || fresh3[0].Event != "a" || fresh3[0].Seq != 5 {
		t.Fatalf("same-T pair not deduped deterministically by seq: %+v", fresh3)
	}
}

// ── Launch-dependent tests (real capture API) ───────────────────────────────

// captureServer serves a small page with a button that flips the background,
// so interactions produce real compositor changes and document-level marks.
func captureServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("flipped") == "1" {
			_, _ = w.Write([]byte(`<html><body style="background:#202020">
				<button id="flip" style="width:120px;height:40px;background:#e07a5f">Flip</button>
				</body></html>`))
			return
		}
		_, _ = w.Write([]byte(`<html><body style="background:#f2f2f2">
			<button id="flip" style="width:120px;height:40px;background:#3d405b">Flip</button>
			</body></html>`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestCaptureStillAndTake(t *testing.T) {
	skipUnlessLaunchable(t)
	srv := captureServer(t)
	mgr := testManager(t, Options{})
	dir := t.TempDir()

	// ── Stills: one per requested width, manifest parses ──
	results, err := mgr.CaptureStill(context.Background(), StillOptions{
		URL:    srv.URL + "/",
		Dir:    dir,
		Widths: []int{400, 800},
	})
	if err != nil {
		t.Fatalf("CaptureStill: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d stills, want 2", len(results))
	}
	for _, res := range results {
		if _, err := os.Stat(res.PNGPath); err != nil {
			t.Fatalf("missing %s: %v", res.PNGPath, err)
		}
		manifestData, err := os.ReadFile(res.ManifestPath)
		if err != nil {
			t.Fatalf("missing manifest: %v", err)
		}
		var pm struct {
			Scroll   [2]float64 `json:"scroll"`
			URL      string     `json:"url"`
			Viewport [2]float64 `json:"viewport"`
			Rects    []struct {
				Sel string  `json:"sel"`
				X   float64 `json:"x"`
				Y   float64 `json:"y"`
				W   float64 `json:"w"`
				H   float64 `json:"h"`
				BG  string  `json:"bg"`
				FG  string  `json:"fg"`
			} `json:"rects"`
		}
		if err := json.Unmarshal(manifestData, &pm); err != nil {
			t.Fatalf("manifest not pagemap-shaped: %v\n%s", err, manifestData)
		}
		if pm.URL != srv.URL+"/" {
			t.Fatalf("manifest URL = %q", pm.URL)
		}
		if int(pm.Viewport[0]) != res.Width {
			t.Fatalf("manifest viewport %v, want width %d", pm.Viewport, res.Width)
		}
		found := false
		for _, r := range pm.Rects {
			if r.Sel == "button#flip" && r.W > 100 && r.H > 30 {
				found = true
			}
		}
		if !found {
			t.Fatalf("button rect missing from manifest: %+v", pm.Rects)
		}
	}

	// ── Take: flip the page, expect frames + a settled end ──
	markCount := 0
	take, err := mgr.StartRecording(RecordOptions{
		BaseDir:     dir,
		MaxDuration: 8 * time.Second,
		Marks: func() []TakeMark {
			markCount++
			return []TakeMark{{T: time.Now().UnixMilli(), Seq: int64(markCount), Event: "click", Dir: "act"}}
		},
	})
	if err != nil {
		t.Fatalf("StartRecording: %v", err)
	}
	if mgr.ActiveTake() != take {
		t.Fatalf("ActiveTake does not report the running take")
	}
	// Drive a real interaction: click navigates to the flipped page — the
	// re-arm-on-navigation path runs for real.
	if err := mgr.Act(context.Background(), []Step{
		{Action: "click", Selector: "button#flip"},
	}); err != nil {
		t.Fatalf("Act during take: %v", err)
	}
	manifest, err := take.Wait()
	if err != nil {
		t.Fatalf("take.Wait: %v", err)
	}
	if mgr.ActiveTake() != nil {
		t.Fatalf("ActiveTake still set after the take finished")
	}
	if !manifest.Settled || manifest.EndReason != endReasonSettled {
		t.Fatalf("take did not end settled: reason=%q settled=%v", manifest.EndReason, manifest.Settled)
	}
	if len(manifest.Frames) == 0 {
		t.Fatalf("no frames captured")
	}
	if manifest.Frames[0].T <= 0 {
		t.Fatalf("frames carry no timestamps")
	}
	if len(manifest.Keyframes) == 0 {
		t.Fatalf("no keyframes: first frame should always be one")
	}
	if len(manifest.Marks) == 0 {
		t.Fatalf("Marks source was wired but the manifest holds no marks")
	}
	if manifest.URL == "" {
		t.Fatalf("manifest URL empty")
	}
	// The files exist on disk and the manifest round-trips.
	onDisk, err := readTakeManifest(filepath.Join(dir, manifest.ID, "manifest.json"))
	if err != nil {
		t.Fatalf("manifest.json missing: %v", err)
	}
	if len(onDisk.Frames) != len(manifest.Frames) {
		t.Fatalf("manifest on disk disagrees: %d vs %d frames", len(onDisk.Frames), len(manifest.Frames))
	}
	for _, fr := range onDisk.Frames {
		if _, err := os.Stat(filepath.Join(dir, manifest.ID, fr.File)); err != nil {
			t.Fatalf("missing frame file %s: %v", fr.File, err)
		}
	}

	// ── Discard: the take dir is deleted, no frames returned ──
	take2, err := mgr.StartRecording(RecordOptions{BaseDir: dir, MaxDuration: 8 * time.Second})
	if err != nil {
		t.Fatalf("StartRecording (second): %v", err)
	}
	if _, err := take2.Stop(true); err != nil {
		t.Fatalf("Stop(discard): %v", err)
	}
	if _, err := os.Stat(take2.Dir); !os.IsNotExist(err) {
		t.Fatalf("discard did not delete %s", take2.Dir)
	}

	// ── Double-start valve: the first take closes with a warning ──
	t1, err := mgr.StartRecording(RecordOptions{BaseDir: dir, MaxDuration: 30 * time.Second})
	if err != nil {
		t.Fatalf("StartRecording (t1): %v", err)
	}
	t2, err := mgr.StartRecording(RecordOptions{BaseDir: dir, MaxDuration: 8 * time.Second})
	if err != nil {
		t.Fatalf("StartRecording (t2): %v", err)
	}
	m1, err := t1.Wait()
	if err != nil {
		t.Fatalf("t1.Wait: %v", err)
	}
	if len(m1.Warnings) == 0 {
		t.Fatalf("double-start did not warn the closed take: %+v", m1)
	}
	if _, err := t2.Stop(false); err != nil {
		t.Fatalf("t2.Stop: %v", err)
	}
	_ = os.RemoveAll(dir)
}

// pageDims exposes the page's current inner size (test helper).
func (m *Manager) pageDims() ([2]float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	page, err := m.pageLocked()
	if err != nil {
		return [2]float64{}, err
	}
	return viewportDims(page)
}
