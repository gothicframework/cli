package mcp

// tools_record.go — the recording tools: record_start, record_stop,
// record_mark, record_frames. The take's act marks ride the dev bus: the
// session wires the take's Marks closure to the bus ring (marks recorded by
// the injected script AND record_mark injections land on one timeline).

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/gothicframework/cli/v3/internal/browser"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ── record_start ───────────────────────────────────────────────────────────

type recordStartIn struct {
	URL        string `json:"url,omitempty" jsonschema:"navigate here first (optional)"`
	MaxSeconds int    `json:"max_seconds,omitempty" jsonschema:"auto-stop budget in seconds (default 60)"`
	MaxFrames  int    `json:"max_frames,omitempty" jsonschema:"frame ceiling (default 1200; the oldest frames drop past it)"`
}

type recordStartOut struct {
	OK     bool   `json:"ok"`
	TakeID string `json:"take_id"`
	Dir    string `json:"dir"`
}

func (s *surface) recordStartTool(_ context.Context, _ *mcp.CallToolRequest, in recordStartIn) (*mcp.CallToolResult, recordStartOut, error) {
	c, err := s.currentCap()
	if err != nil {
		return nil, recordStartOut{}, err
	}
	b, err := c.needBrowser()
	if err != nil {
		return nil, recordStartOut{}, err
	}
	if strings.TrimSpace(in.URL) != "" {
		if err := b.Open(in.URL); err != nil {
			return nil, recordStartOut{}, fmt.Errorf("navigate to %s: %w", in.URL, err)
		}
	}
	// The take's act marks come from the dev bus (the injected script's
	// kind:"mark" records) — the same ring record_mark writes into. The
	// session wires the puller to the proxy's ring directly; the fallback
	// rides the Timeline port's bus read.
	opts := browser.RecordOptions{
		MaxDuration: time.Duration(in.MaxSeconds) * time.Second,
		MaxFrames:   in.MaxFrames,
	}
	switch {
	case c.Marks != nil:
		opts.Marks = c.Marks
	case c.Timeline != nil:
		tl := c.Timeline
		opts.Marks = func() []browser.TakeMark {
			recs, err := tl.Bus(-1)
			if err != nil {
				return nil
			}
			return browser.MarksFromRecords(recs)
		}
	}
	take, err := b.StartRecording(opts)
	if err != nil {
		return nil, recordStartOut{}, err
	}
	s.mu.Lock()
	s.lastTake = take
	s.lastManifest = nil
	s.mu.Unlock()
	return nil, recordStartOut{OK: true, TakeID: takeID(take), Dir: takeDir(take)}, nil
}

// ── record_stop ────────────────────────────────────────────────────────────

type recordStopIn struct {
	Discard         bool `json:"discard,omitempty" jsonschema:"true deletes the take directory and returns only the tombstone"`
	IncludeManifest bool `json:"include_manifest,omitempty" jsonschema:"include the full timeline (frames + marks) — the heavy form"`
}

type recordStopOut struct {
	OK       bool             `json:"ok"`
	Summary  string           `json:"summary,omitempty"`
	TakeID   string           `json:"take_id,omitempty"`
	Stopped  bool             `json:"stopped"`
	Settled  bool             `json:"settled"`
	DurMs    int64            `json:"duration_ms,omitempty"`
	Frames   int              `json:"frames,omitempty"`
	Marks    int              `json:"marks,omitempty"`
	Dir      string           `json:"dir,omitempty"`
	URL      string           `json:"url,omitempty"`
	Manifest *takeManifestOut `json:"manifest,omitempty" jsonschema:"the full timeline when include_manifest is set (heavy)"`
}

// takeManifestOut is the heavy form of a take's timeline.
type takeManifestOut struct {
	ID         string             `json:"id"`
	URL        string             `json:"url"`
	DurationMs int64              `json:"duration_ms"`
	EndReason  string             `json:"end_reason"`
	Frames     []takeFrameOut     `json:"frames"`
	Marks      []browser.TakeMark `json:"marks"`
	Warnings   []string           `json:"warnings,omitempty"`
}

// takeFrameOut is one frame line of the heavy form.
type takeFrameOut struct {
	File     string  `json:"file"`
	T        int64   `json:"t"`
	Delta    float64 `json:"delta"`
	Keyframe bool    `json:"keyframe"`
}

func (s *surface) recordStopTool(_ context.Context, _ *mcp.CallToolRequest, in recordStopIn) (*mcp.CallToolResult, recordStopOut, error) {
	c, err := s.currentCap()
	if err != nil {
		return nil, recordStopOut{}, err
	}
	b, err := c.needBrowser()
	if err != nil {
		return nil, recordStopOut{}, err
	}
	s.mu.Lock()
	take := s.activeTakeLocked(b)
	s.mu.Unlock()
	if take == nil {
		return nil, recordStopOut{}, fmt.Errorf("no recording is active (call record_start first)")
	}
	manifest, err := take.Stop(in.Discard)
	s.mu.Lock()
	if !in.Discard {
		s.lastManifest = &manifest
	}
	s.lastTake = nil
	s.mu.Unlock()
	if err != nil {
		return nil, recordStopOut{}, fmt.Errorf("stop take: %w", err)
	}
	return nil, takeStopOut(manifest, in.Discard, in.IncludeManifest), nil
}

// takeStopOut renders the stop result (summary or full manifest).
func takeStopOut(m browser.TakeManifest, discarded, includeManifest bool) recordStopOut {
	out := recordStopOut{
		OK:      true,
		Stopped: true,
		Settled: m.Settled,
		DurMs:   m.DurationMs,
		URL:     m.URL,
	}
	if m.ID != "" {
		out.TakeID = m.ID
	}
	out.Frames = len(m.Frames)
	out.Marks = len(m.Marks)
	if discarded {
		out.Dir = ""
		out.Summary = "take discarded (frames deleted)"
		return out
	}
	if includeManifest {
		full := &takeManifestOut{
			ID:         m.ID,
			URL:        m.URL,
			DurationMs: m.DurationMs,
			EndReason:  m.EndReason,
			Frames:     make([]takeFrameOut, 0, len(m.Frames)),
			Marks:      m.Marks,
			Warnings:   m.Warnings,
		}
		for _, f := range m.Frames {
			full.Frames = append(full.Frames, takeFrameOut{
				File: filepath.Base(f.File), T: f.T, Delta: f.Delta, Keyframe: f.Keyframe,
			})
		}
		out.Manifest = full
	}
	return out
}

// ── record_mark ────────────────────────────────────────────────────────────

type recordMarkIn struct {
	Name    string         `json:"name" jsonschema:"the mark's name (e.g. \"before-submit\")"`
	Payload map[string]any `json:"payload,omitempty" jsonschema:"optional structured context riding the mark"`
}

type recordMarkOut struct {
	OK    bool   `json:"ok"`
	Event string `json:"event"`
}

func (s *surface) recordMarkTool(_ context.Context, _ *mcp.CallToolRequest, in recordMarkIn) (*mcp.CallToolResult, recordMarkOut, error) {
	c, err := s.currentCap()
	if err != nil {
		return nil, recordMarkOut{}, err
	}
	if strings.TrimSpace(in.Name) == "" {
		return nil, recordMarkOut{}, fmt.Errorf("name is required")
	}
	if c.MarkSink == nil {
		return nil, recordMarkOut{}, fmt.Errorf("the dev bus is not available in this session; marks cannot be injected")
	}
	event := "mcp:" + strings.TrimSpace(in.Name)
	payload := in.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	if err := c.MarkSink(event, payload); err != nil {
		return nil, recordMarkOut{}, fmt.Errorf("inject mark %q: %w", in.Name, err)
	}
	return nil, recordMarkOut{OK: true, Event: event}, nil
}

// ── record_frames ──────────────────────────────────────────────────────────

type recordFramesIn struct {
	IncludeFrames bool `json:"include_frames,omitempty" jsonschema:"include the per-frame lines (file names, deltas, keyframes) — the heavy form"`
}

type recordFramesOut struct {
	OK         bool           `json:"ok"`
	Active     bool           `json:"active" jsonschema:"true when a take is recording right now"`
	TakeID     string         `json:"take_id,omitempty"`
	Frames     int            `json:"frames,omitempty"`
	Keyframes  int            `json:"keyframes,omitempty"`
	Marks      int            `json:"marks,omitempty"`
	Settled    bool           `json:"settled,omitempty"`
	EndReason  string         `json:"end_reason,omitempty"`
	Dir        string         `json:"dir,omitempty"`
	FrameLines []takeFrameOut `json:"frame_lines,omitempty"`
	Note       string         `json:"note,omitempty"`
}

func (s *surface) recordFramesTool(_ context.Context, _ *mcp.CallToolRequest, in recordFramesIn) (*mcp.CallToolResult, recordFramesOut, error) {
	c, err := s.currentCap()
	if err != nil {
		return nil, recordFramesOut{}, err
	}
	b, err := c.needBrowser()
	if err != nil {
		return nil, recordFramesOut{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if active := s.activeTakeLocked(b); active != nil {
		out := recordFramesOut{OK: true, Active: true, TakeID: takeID(active), Note: "take is still recording; stop it for the final timeline"}
		// The active manifest is not final; the on-disk one would race.
		return nil, out, nil
	}
	if s.lastManifest == nil {
		return nil, recordFramesOut{}, fmt.Errorf("no take in this session yet (record_start, then record_frames)")
	}
	m := s.lastManifest
	out := recordFramesOut{
		OK:        true,
		TakeID:    m.ID,
		Frames:    len(m.Frames),
		Keyframes: len(m.Keyframes),
		Marks:     len(m.Marks),
		Settled:   m.Settled,
		EndReason: m.EndReason,
		Note:      "the take's manifest.json and frames live under .gothicCli/mcp/rec_" + m.ID,
	}
	if in.IncludeFrames {
		const cap = 200
		for i, f := range m.Frames {
			if i >= cap {
				out.Note = fmt.Sprintf("first %d of %d frames listed (use the take dir for the full manifest.json)", cap, len(m.Frames))
				break
			}
			out.FrameLines = append(out.FrameLines, takeFrameOut{
				File: filepath.Base(f.File), T: f.T, Delta: f.Delta, Keyframe: f.Keyframe,
			})
		}
	}
	return nil, out, nil
}

// activeTakeLocked returns the take to stop/read: the browser's active take
// when one is recording, else the last handle this session started. Callers
// hold s.mu.
func (s *surface) activeTakeLocked(b BrowserPort) TakePort {
	if t := b.ActiveTake(); t != nil {
		s.lastTake = t
		return t
	}
	return s.lastTake
}

// takeID/takeDir unwrap the take handle defensively: the handle may be a
// *browser.Take (production) or a test double; the caller names itself via
// TakeID when it wants a real id.
func takeID(t TakePort) string {
	if bt, ok := t.(*browser.Take); ok {
		return bt.ID
	}
	if ider, ok := t.(interface{ TakeID() string }); ok {
		return ider.TakeID()
	}
	return "take"
}

func takeDir(t TakePort) string {
	if bt, ok := t.(*browser.Take); ok {
		return bt.Dir
	}
	if direr, ok := t.(interface{ TakeDir() string }); ok {
		return direr.TakeDir()
	}
	return ""
}
