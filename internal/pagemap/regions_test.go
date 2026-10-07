package pagemap

import (
	"image"
	"testing"
)

// TestMapRegionsOwnership checks the point-in-rect contract: each cell is
// owned by the smallest covering element, spans are reported inclusive, and a
// fully-covered wrapper drops out of the report.
func TestMapRegionsOwnership(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 24, 12))
	fillRect(img, img.Bounds(), Color{0x0f, 0x0f, 0x12, 0xff})
	fillRect(img, image.Rect(0, 0, 24, 4), Color{0x1a, 0x1a, 0x20, 0xff})
	fillRect(img, image.Rect(0, 4, 12, 8), Color{0xe4, 0xe4, 0xe7, 0xff})
	fillRect(img, image.Rect(0, 4, 4, 6), Color{0x0f, 0x0f, 0x12, 0xff}) // the inner span's own pixels
	q := quantizeImage(img, DefaultMaxColors)
	grid := buildGrid(img, q, 6, 3)

	m := &Manifest{Scroll: [2]float64{0, 0}, Rects: []Element{
		{Selector: "body", X: 0, Y: 0, W: 24, H: 12, BG: "#0f0f12"},
		{Selector: "header.site-header", X: 0, Y: 0, W: 24, H: 4, BG: "#1a1a20"},
		{Selector: "pre.code-block", X: 0, Y: 4, W: 12, H: 4, BG: "#e4e4e7"},
		{Selector: "span.inner", X: 0, Y: 4, W: 4, H: 2, BG: "#0f0f12"},
	}}
	regions := mapRegions(q, grid, m)

	bySel := make(map[string]Region, len(regions))
	for _, r := range regions {
		bySel[r.Selector] = r
	}
	header, ok := bySel["header.site-header"]
	if !ok {
		t.Fatalf("header missing from regions: %+v", regions)
	}
	if header.RowFrom != 0 || header.RowTo != 0 {
		t.Errorf("header should own row 0 only, got rows %d-%d", header.RowFrom, header.RowTo)
	}
	if header.RenderedHex != "#1a1a20" {
		t.Errorf("header rendered hex = %s, want #1a1a20", header.RenderedHex)
	}

	code, ok := bySel["pre.code-block"]
	if !ok {
		t.Fatalf("code block missing from regions: %+v", regions)
	}
	if code.ColFrom != 0 || code.ColTo != 2 {
		t.Errorf("code block should span cols 0-2 of a 6-col grid, got %d-%d", code.ColFrom, code.ColTo)
	}
	if code.RenderedHex != "#e4e4e7" {
		t.Errorf("code block rendered hex = %s, want #e4e4e7", code.RenderedHex)
	}

	// span.inner (4x2 px) covers part of the code block; its own cells must be
	// dark — the smallest element wins the cell.
	inner, ok := bySel["span.inner"]
	if !ok {
		t.Fatalf("inner span missing from regions: %+v", regions)
	}
	if inner.RenderedHex != "#0f0f12" {
		t.Errorf("inner span must own its cells and render #0f0f12, got %s", inner.RenderedHex)
	}

	// body is the largest element: it keeps only the cells nothing else claims.
	body, ok := bySel["body"]
	if !ok {
		t.Fatalf("body missing from regions: %+v", regions)
	}
	if body.RenderedHex != "#0f0f12" {
		t.Errorf("body rendered hex = %s, want #0f0f12", body.RenderedHex)
	}

	// Regions come out in reading order (top to bottom).
	if regions[0].Selector != "header.site-header" {
		t.Errorf("first region should be the topmost element, got %s", regions[0].Selector)
	}
}

func TestRegionLine(t *testing.T) {
	r := Region{Selector: "button.cta", RowFrom: 15, RowTo: 19, ColFrom: 6, ColTo: 20, X0: 40, X1: 136}
	if got, want := regionLine(r), "  rows 15-19  x40-136  button.cta"; got != want {
		t.Errorf("regionLine = %q, want %q", got, want)
	}
	r.DeclaredHex = "#7c3aed"
	r.RenderedHex = "#3f3f46"
	if got, want := regionLine(r), "  rows 15-19  x40-136  button.cta  render #3f3f46"; got != want {
		t.Errorf("regionLine with render = %q, want %q", got, want)
	}
}
