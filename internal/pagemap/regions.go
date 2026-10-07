package pagemap

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
)

// Manifest is the DOM-side document the browser layer writes next to the
// screenshot: the scroll position plus, for every visible element, its
// viewport rect and the computed colours worth checking against the raster.
// The browser layer produces it with one page.Eval() — this package only
// consumes it.
type Manifest struct {
	Scroll [2]float64 `json:"scroll"` // [scrollX, scrollY] at capture time
	Rects  []Element  `json:"rects"`
}

// Element is one visible element's rect and computed colours.
type Element struct {
	Selector  string  `json:"sel"`
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	W         float64 `json:"w"`
	H         float64 `json:"h"`
	BG        string  `json:"bg"`        // computed background-color, "" or "transparent" when none
	FG        string  `json:"fg"`        // computed color
	Overflows bool    `json:"overflows"` // scrollWidth > clientWidth at capture time
}

// ParseManifest decodes the DOM manifest JSON.
func ParseManifest(data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("decoding page-view manifest: %w", err)
	}
	return &m, nil
}

// Region is one element's footprint on the grid: the cell span its own pixels
// land in and the palette class dominating them.
type Region struct {
	Selector string
	// RowFrom/RowTo and ColFrom/ColTo are INCLUSIVE cell spans.
	RowFrom, RowTo int
	ColFrom, ColTo int
	X0, X1         int    // the element's pixel span, clipped to the raster
	DeclaredHex    string // parsed BG as hex, "" when none/transparent
	RenderedHex    string // hex of the palette class dominating the region, "" when none
	renderedClass  int
	declaredKey    Color
}

// mapRegions lays each manifest rect over the grid (point-in-rect against the
// cell centres), assigns every cell to the SMALLEST element covering it — the
// deepest content wins the cell, wrappers keep only what they alone paint —
// and reports each element's owned span plus its dominant rendered colour.
func mapRegions(q quantized, g Grid, m *Manifest) []Region {
	if m == nil {
		return nil
	}
	rows, cols, w, h := g.Rows, g.Cols, g.Width, g.Height
	if rows == 0 || cols == 0 {
		return nil
	}

	type paint struct {
		elIdx          int
		xf, xt         float64
		yf, yt         float64
		r0, r1, c0, c1 int
		area           float64
	}
	paints := make([]paint, 0, len(m.Rects))
	for i := range m.Rects {
		el := &m.Rects[i]
		if el.W < 2 || el.H < 2 { // degenerate rects only; the capture layer filters invisible clutter
			continue
		}
		if el.X >= float64(w) || el.Y >= float64(h) {
			continue
		}
		x1 := math.Min(el.X+el.W, float64(w))
		y1 := math.Min(el.Y+el.H, float64(h))
		p := paint{
			elIdx: i,
			xf:    math.Max(el.X, 0), xt: x1,
			yf: math.Max(el.Y, 0), yt: y1,
		}
		p.area = (p.xt - p.xf) * (p.yt - p.yf)
		p.c0 = clampCell(int(p.xf*float64(cols)/float64(w)), cols)
		p.c1 = clampCell(int(math.Ceil(p.xt*float64(cols)/float64(w)))-1, cols)
		p.r0 = clampCell(int(p.yf*float64(rows)/float64(h)), rows)
		p.r1 = clampCell(int(math.Ceil(p.yt*float64(rows)/float64(h)))-1, rows)
		paints = append(paints, p)
	}
	// Small elements paint first, so they own the cells they sit on.
	sort.SliceStable(paints, func(i, j int) bool { return paints[i].area < paints[j].area })

	owner := make([]int, rows*cols)
	for i := range owner {
		owner[i] = -1
	}
	tallies := make([][]int, len(paints))
	for pi, p := range paints {
		own := make([]int, q.pal.Len())
		for r := p.r0; r <= p.r1; r++ {
			for c := p.c0; c <= p.c1; c++ {
				cell := r*cols + c
				if owner[cell] != -1 {
					continue
				}
				owner[cell] = pi
				own[g.Cells[cell]]++
			}
		}
		tallies[pi] = own
	}

	regions := make([]Region, 0, len(paints))
	for pi, p := range paints {
		owned := 0
		best, bestN := -1, 0
		for cls, n := range tallies[pi] {
			owned += n
			if n > bestN {
				best, bestN = cls, n
			}
		}
		if owned == 0 { // fully covered by smaller elements: nothing of its own to report
			continue
		}
		el := &m.Rects[p.elIdx]
		reg := Region{
			Selector:      el.Selector,
			RowFrom:       p.r0,
			RowTo:         p.r1,
			ColFrom:       p.c0,
			ColTo:         p.c1,
			X0:            int(p.xf),
			X1:            int(p.xt),
			renderedClass: best,
		}
		if best >= 0 {
			reg.RenderedHex = q.pal.Entries[best].Hex
		}
		if key, ok := parseCSSColor(el.BG); ok {
			reg.DeclaredHex = key.Hex()
			reg.declaredKey = key
		}
		regions = append(regions, reg)
	}
	// Reading order: top to bottom, then left to right, then smallest first.
	sort.SliceStable(regions, func(i, j int) bool {
		a, b := regions[i], regions[j]
		if a.RowFrom != b.RowFrom {
			return a.RowFrom < b.RowFrom
		}
		if a.ColFrom != b.ColFrom {
			return a.ColFrom < b.ColFrom
		}
		return a.X0 < b.X0
	})
	return regions
}

func clampCell(v, max int) int {
	if v < 0 {
		return 0
	}
	if v > max-1 {
		return max - 1
	}
	return v
}

// regionLine renders one REGIONS entry.
func regionLine(r Region) string {
	s := fmt.Sprintf("  rows %2d-%-2d  x%d-%d  %s", r.RowFrom, r.RowTo, r.X0, r.X1, r.Selector)
	if r.DeclaredHex != "" && r.RenderedHex != "" && r.DeclaredHex != r.RenderedHex {
		s += fmt.Sprintf("  render %s", r.RenderedHex)
	}
	return s
}
