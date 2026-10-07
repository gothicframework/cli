package mcp

// tools_view.go — the raster tools: view, zoom, diff, compare. All of them go
// through the browser's still capture (screenshot + DOM manifest) and encode
// through pagemap; diff/compare keep a small per-session state (the baseline
// still) under the surface mutex.

import (
	"context"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"os"
	"strings"

	"github.com/gothicframework/cli/v4/internal/browser"
	"github.com/gothicframework/cli/v4/internal/pagemap"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ── view ───────────────────────────────────────────────────────────────────

type viewIn struct {
	URL         string `json:"url,omitempty" jsonschema:"navigate here first (optional)"`
	Widths      []int  `json:"widths,omitempty" jsonschema:"one viewport width per view (optional; default = the current viewport)"`
	IncludeText bool   `json:"include_text,omitempty" jsonschema:"include the page-view text payload in the result (the heavy form; default = paths only)"`
}

type viewOut struct {
	OK    bool            `json:"ok"`
	Views []pageViewEntry `json:"views"`
}

// pageViewEntry is one width's capture: the artifact paths plus, when
// requested, the encoded text map.
type pageViewEntry struct {
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	URL          string `json:"url,omitempty"`
	PNG          string `json:"png"`
	ManifestPath string `json:"manifest"`
	Text         string `json:"text,omitempty"`
}

func (s *surface) viewTool(ctx context.Context, _ *mcp.CallToolRequest, in viewIn) (*mcp.CallToolResult, viewOut, error) {
	c, err := s.currentCap()
	if err != nil {
		return nil, viewOut{}, err
	}
	b, err := c.needBrowser()
	if err != nil {
		return nil, viewOut{}, err
	}
	stills, err := b.CaptureStill(ctx, browser.StillOptions{Widths: in.Widths, URL: in.URL})
	if err != nil {
		return nil, viewOut{}, err
	}
	out := viewOut{OK: true}
	for _, st := range stills {
		entry := pageViewEntry{
			Width:        st.Width,
			Height:       st.Height,
			URL:          st.URL,
			PNG:          st.PNGPath,
			ManifestPath: st.ManifestPath,
		}
		if in.IncludeText {
			txt, err := encodePageViewFile(st, "", c.Engine)
			if err != nil {
				return nil, viewOut{}, err
			}
			entry.Text = txt
		}
		out.Views = append(out.Views, entry)
	}
	return nil, out, nil
}

// encodePageViewFile encodes one still into the page-view payload. It reads
// the PNG and the manifest back from disk (the browser layer wrote both).
func encodePageViewFile(st browser.StillResult, grid, engine string) (string, error) {
	img, err := decodePNGFile(st.PNGPath)
	if err != nil {
		return "", err
	}
	var manifest *pagemap.Manifest
	if data, err := os.ReadFile(st.ManifestPath); err == nil {
		if m, perr := pagemap.ParseManifest(data); perr == nil {
			manifest = m
		}
	}
	gw, gh, err := parseGridSpec(grid)
	if err != nil {
		return "", err
	}
	return pagemap.Encode(img, manifest, pagemap.Options{
		URL:       st.URL,
		Engine:    engine,
		ViewportW: st.Width,
		ViewportH: st.Height,
		GridW:     gw,
		GridH:     gh,
	}), nil
}

// parseGridSpec parses "WxH" into the two grid dimensions; "" takes the
// pagemap defaults (0,0, which Encode resolves to 48x24).
func parseGridSpec(s string) (int, int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, nil
	}
	var w, h int
	if _, err := fmt.Sscanf(s, "%dx%d", &w, &h); err != nil {
		return 0, 0, fmt.Errorf("invalid grid %q: want WxH, e.g. 48x24", s)
	}
	if w < 4 || w > 160 || h < 4 || h > 160 {
		return 0, 0, fmt.Errorf("grid %q: each axis must be between 4 and 160", s)
	}
	return w, h, nil
}

// decodePNGFile opens and decodes a PNG, closing the file eagerly.
func decodePNGFile(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return img, nil
}

// ── zoom ───────────────────────────────────────────────────────────────────

type zoomIn struct {
	Selector string `json:"selector" jsonschema:"CSS selector of the region to zoom into (matched against the DOM manifest's selectors)"`
	URL      string `json:"url,omitempty" jsonschema:"navigate here first (optional)"`
}

type zoomOut struct {
	OK     bool   `json:"ok"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
	Rect   string `json:"rect,omitempty" jsonschema:"the cropped rect in page pixels"`
	PNG    string `json:"png,omitempty" jsonschema:"the cropped raster on disk"`
	Text   string `json:"text,omitempty" jsonschema:"the page-view payload of just the region"`
}

func (s *surface) zoomTool(ctx context.Context, _ *mcp.CallToolRequest, in zoomIn) (*mcp.CallToolResult, zoomOut, error) {
	c, err := s.currentCap()
	if err != nil {
		return nil, zoomOut{}, err
	}
	b, err := c.needBrowser()
	if err != nil {
		return nil, zoomOut{}, err
	}
	if strings.TrimSpace(in.Selector) == "" {
		return nil, zoomOut{}, fmt.Errorf("selector is required")
	}
	stills, err := b.CaptureStill(ctx, browser.StillOptions{URL: in.URL})
	if err != nil {
		return nil, zoomOut{}, err
	}
	if len(stills) == 0 {
		return nil, zoomOut{}, fmt.Errorf("capture returned no stills")
	}
	still := stills[0]
	img, err := decodePNGFile(still.PNGPath)
	if err != nil {
		return nil, zoomOut{}, err
	}
	var manifest *pagemap.Manifest
	if data, err := os.ReadFile(still.ManifestPath); err == nil {
		manifest, _ = pagemap.ParseManifest(data)
	}
	rect, err := manifestRect(manifest, in.Selector)
	if err != nil {
		return nil, zoomOut{}, err
	}
	crop := cropImage(img, rect)
	pngPath := strings.TrimSuffix(still.PNGPath, ".png") + "_zoom_" + sanitizeName(in.Selector) + ".png"
	if err := writePNGFile(pngPath, crop); err != nil {
		return nil, zoomOut{}, err
	}
	// The zoomed payload: a dense grid over just the region — several times
	// the per-cell resolution of the full-page map.
	txt := pagemap.Encode(crop, manifest, pagemap.Options{
		URL:       fmt.Sprintf("%s [zoom: %s]", still.URL, in.Selector),
		Engine:    c.Engine,
		ViewportW: rect.Dx(),
		ViewportH: rect.Dy(),
		GridW:     32,
		GridH:     24,
	})
	return nil, zoomOut{
		OK:     true,
		Width:  rect.Dx(),
		Height: rect.Dy(),
		Rect:   fmt.Sprintf("x=%d y=%d w=%d h=%d", rect.Min.X, rect.Min.Y, rect.Dx(), rect.Dy()),
		PNG:    pngPath,
		Text:   txt,
	}, nil
}

// manifestRect resolves a CSS selector to a rect image.Rectangle, using the
// DOM manifest's rects (the capture already wrote them). Selectors match
// exactly first, then by suffix (the manifest's cssPath is a chain, so the
// leaf selector suffix is the usual handle).
func manifestRect(m *pagemap.Manifest, selector string) (image.Rectangle, error) {
	if m == nil {
		return image.Rectangle{}, fmt.Errorf("no DOM manifest was captured; cannot resolve %q", selector)
	}
	for _, want := range []func(string) bool{
		func(sel string) bool { return sel == selector },
		func(sel string) bool { return strings.HasSuffix(sel, selector) },
	} {
		for _, r := range m.Rects {
			if want(r.Selector) {
				return image.Rect(int(r.X), int(r.Y), int(r.X+r.W), int(r.Y+r.H)), nil
			}
		}
	}
	return image.Rectangle{}, fmt.Errorf("selector %q not found among the captured elements", selector)
}

// cropImage crops img to rect, clamped to the image bounds, as an RGBA.
func cropImage(img image.Image, rect image.Rectangle) image.Image {
	r := rect.Intersect(img.Bounds())
	if r.Empty() {
		return image.NewRGBA(image.Rectangle{})
	}
	out := image.NewRGBA(image.Rectangle{Max: image.Pt(r.Dx(), r.Dy())})
	draw.Draw(out, out.Bounds(), img, r.Min, draw.Src)
	return out
}

// sanitizeName makes a selector safe as a filename fragment.
func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if len(out) > 40 {
		out = out[len(out)-40:]
	}
	if out == "" {
		out = "region"
	}
	return out
}

// writePNGFile writes img to path as PNG.
func writePNGFile(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	return nil
}

// ── diff ───────────────────────────────────────────────────────────────────

type diffIn struct {
	URL   string `json:"url,omitempty" jsonschema:"navigate here first (optional)"`
	Width int    `json:"width,omitempty" jsonschema:"viewport width of the compared still (must be stable across a diff pair)"`
}

type diffOut struct {
	OK          bool   `json:"ok"`
	BaselineSet bool   `json:"baseline_set" jsonschema:"true when this call only stored the baseline"`
	Changed     int    `json:"changed,omitempty" jsonschema:"changed cells between the baseline and now"`
	Total       int    `json:"total,omitempty" jsonschema:"grid cells compared"`
	Summary     string `json:"summary,omitempty"`
	PNG         string `json:"png,omitempty" jsonschema:"the current still on disk"`
}

// baselineState is the diff tool's stored still.
type baselineState struct {
	url   string
	width int
	img   image.Image
}

func (s *surface) diffTool(ctx context.Context, _ *mcp.CallToolRequest, in diffIn) (*mcp.CallToolResult, diffOut, error) {
	c, err := s.currentCap()
	if err != nil {
		return nil, diffOut{}, err
	}
	b, err := c.needBrowser()
	if err != nil {
		return nil, diffOut{}, err
	}
	var widths []int
	if in.Width > 0 {
		widths = []int{in.Width}
	}
	stills, err := b.CaptureStill(ctx, browser.StillOptions{Widths: widths, URL: in.URL})
	if err != nil {
		return nil, diffOut{}, err
	}
	if len(stills) == 0 {
		return nil, diffOut{}, fmt.Errorf("capture returned no stills")
	}
	still := stills[0]
	img, err := decodePNGFile(still.PNGPath)
	if err != nil {
		return nil, diffOut{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.baseline == nil {
		s.baseline = &baselineState{url: still.URL, width: still.Width, img: img}
		return nil, diffOut{
			OK:          true,
			BaselineSet: true,
			Summary:     fmt.Sprintf("baseline stored (%s at width %d); the next diff reports changed cells", still.URL, still.Width),
			PNG:         still.PNGPath,
		}, nil
	}
	changed, total := gridDelta(s.baseline.img, img)
	s.baseline = &baselineState{url: still.URL, width: still.Width, img: img}
	return nil, diffOut{
		OK:      true,
		Changed: changed,
		Total:   total,
		Summary: fmt.Sprintf("%d of %d cells changed since the previous still", changed, total),
		PNG:     still.PNGPath,
	}, nil
}

// gridDelta quantizes two rasters to a small grid and counts the cells whose
// dominant symbols differ. The grid is coarse on purpose: the diff answer is
// "where did the page change", not a pixel metric.
func gridDelta(prev, cur image.Image) (changed, total int) {
	const (
		gw = 32
		gh = 16
	)
	pb, cb := prev.Bounds(), cur.Bounds()
	for gy := 0; gy < gh; gy++ {
		for gx := 0; gx < gw; gx++ {
			if cellBrightnessDiff(prev, pb, gw, gh, gx, gy) != cellBrightnessDiff(cur, cb, gw, gh, gx, gy) {
				changed++
			}
			total++
		}
	}
	return changed, total
}

// cellBrightnessDiff returns 1 when a grid cell is brighter than mid-grey on
// average. A cheap two-level quantization that survives anti-aliasing noise.
func cellBrightnessDiff(img image.Image, b image.Rectangle, gw, gh, gx, gy int) int {
	x0 := b.Min.X + gx*b.Dx()/gw
	x1 := b.Min.X + (gx+1)*b.Dx()/gw
	y0 := b.Min.Y + gy*b.Dy()/gh
	y1 := b.Min.Y + (gy+1)*b.Dy()/gh
	if x0 >= x1 || y0 >= y1 {
		return 0
	}
	var sum, n int64
	for y := y0; y < y1; y += 2 {
		for x := x0; x < x1; x += 2 {
			r, g, bl, _ := img.At(x, y).RGBA()
			sum += int64(r>>8) + int64(g>>8) + int64(bl>>8)
			n += 3
		}
	}
	if n == 0 {
		return 0
	}
	if int(sum/n) > 128 { // above mid-grey (sum/n is the per-channel mean)
		return 1
	}
	return 0
}

// ── compare ────────────────────────────────────────────────────────────────

type compareIn struct {
	URL         string `json:"url,omitempty" jsonschema:"navigate here first (optional)"`
	Widths      []int  `json:"widths" jsonschema:"exactly two viewport widths to compare"`
	IncludeText bool   `json:"include_text,omitempty" jsonschema:"include both full page-view payloads (heavy)"`
}

type compareOut struct {
	OK      bool            `json:"ok"`
	Widths  []int           `json:"widths"`
	Changed int             `json:"changed" jsonschema:"grid cells that differ between the two widths"`
	Total   int             `json:"total" jsonschema:"grid cells compared"`
	Views   []pageViewEntry `json:"views,omitempty" jsonschema:"per-width artifacts (payloads only when include_text)"`
	Summary string          `json:"summary,omitempty"`
}

func (s *surface) compareTool(ctx context.Context, _ *mcp.CallToolRequest, in compareIn) (*mcp.CallToolResult, compareOut, error) {
	c, err := s.currentCap()
	if err != nil {
		return nil, compareOut{}, err
	}
	b, err := c.needBrowser()
	if err != nil {
		return nil, compareOut{}, err
	}
	if len(in.Widths) != 2 {
		return nil, compareOut{}, fmt.Errorf("compare needs exactly two widths")
	}
	stills, err := b.CaptureStill(ctx, browser.StillOptions{Widths: in.Widths, URL: in.URL})
	if err != nil {
		return nil, compareOut{}, err
	}
	if len(stills) != 2 {
		return nil, compareOut{}, fmt.Errorf("capture returned %d stills, want 2", len(stills))
	}
	one, err := decodePNGFile(stills[0].PNGPath)
	if err != nil {
		return nil, compareOut{}, err
	}
	two, err := decodePNGFile(stills[1].PNGPath)
	if err != nil {
		return nil, compareOut{}, err
	}
	changed, total := gridDelta(one, two)
	out := compareOut{
		OK:      true,
		Widths:  []int{stills[0].Width, stills[1].Width},
		Changed: changed,
		Total:   total,
		Summary: fmt.Sprintf("%d of %d cells differ between width %d and %d",
			changed, total, stills[0].Width, stills[1].Width),
	}
	for _, st := range stills {
		entry := pageViewEntry{
			Width:        st.Width,
			Height:       st.Height,
			URL:          st.URL,
			PNG:          st.PNGPath,
			ManifestPath: st.ManifestPath,
		}
		if in.IncludeText {
			txt, err := encodePageViewFile(st, "", c.Engine)
			if err != nil {
				return nil, compareOut{}, err
			}
			entry.Text = txt
		}
		out.Views = append(out.Views, entry)
	}
	return nil, out, nil
}
