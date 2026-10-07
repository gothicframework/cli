package pagemap

import (
	"fmt"
	"math"
)

// Alert is one derived warning: a headline plus indented bullets that carry
// the conclusion. Thresholds live as named constants so they read as policy.
type Alert struct {
	Summary string
	Bullets []string
}

// Perceptual distances (OKLab, where ~0.1 is "clearly a different colour").
const (
	// minCoverageDistance: a declared colour must be FURTHER than this from
	// every palette entry before "it never painted" is safe to say — closer
	// means it probably did paint and was quantized away with the AA tail.
	minCoverageDistance = 0.12
	// minMismatchDistance: how far a region's rendered colour must sit from
	// its declared background before the report says it rendered differently.
	minMismatchDistance = 0.08
	// minCoverageArea: elements below this many pixels are decorative noise;
	// "the accent never painted" would be claimed over almost nothing.
	minCoverageArea = 16
	// minBleedRows: cells of non-background colour touching the right edge
	// column before edge bleed is reported. One cell can be one button that
	// legitimately ends there; a run is content that continues.
	minBleedRows = 2
	// maxAlertsPerKind keeps one broken page from flooding the payload.
	maxAlertsPerKind = 5
)

// buildAlerts derives the raster alerts: overflow/position checks from the
// manifest, edge bleed from the grid, palette coverage from the exact
// histogram, and declared-vs-rendered style mismatches from the regions.
func buildAlerts(g Grid, q quantized, m *Manifest, regions []Region) []Alert {
	var out []Alert
	out = append(out, overflowAlerts(g, m)...)
	out = append(out, edgeBleedAlert(g, m)...)
	out = append(out, coverageAlerts(q, m, g.Height)...)
	out = append(out, mismatchAlerts(q, regions)...)
	return out
}

// overflowAlerts reports elements that stick out of the viewport or that
// declared scrollWidth > clientWidth at capture time.
func overflowAlerts(g Grid, m *Manifest) []Alert {
	if m == nil {
		return nil
	}
	var out []Alert
	for i := range m.Rects {
		if len(out) >= maxAlertsPerKind {
			break
		}
		el := &m.Rects[i]
		if el.W < 2 || el.H < 2 {
			continue
		}
		over := el.X + el.W - float64(g.Width)
		if !el.Overflows && over <= 0.5 {
			continue
		}
		a := Alert{Summary: fmt.Sprintf("horizontal overflow in %s", el.Selector)}
		if over > 0.5 {
			a.Bullets = append(a.Bullets, fmt.Sprintf(
				"%s extends %.0fpx past the right edge of the viewport", el.Selector, over))
		} else {
			a.Bullets = append(a.Bullets, fmt.Sprintf(
				"%s reports scrollWidth > clientWidth at capture time", el.Selector))
		}
		out = append(out, a)
	}
	return out
}

// edgeBleedAlert checks the rightmost grid column for a run of cells whose
// colour is not the page background — content pixels touching the boundary —
// and confirms the "continue" half against the manifest: only an element whose
// rect actually extends past the viewport turns the touch into a bleed. A
// full-width header or footer touches edges legitimately, so without a
// crossing rect the check stays silent rather than crying wolf.
func edgeBleedAlert(g Grid, m *Manifest) []Alert {
	if len(g.Cells) == 0 || g.Cols == 0 {
		return nil
	}
	bg := 0 // the palette's dominant colour, index 0 by construction
	var bleedRows []int
	for r := 0; r < g.Rows; r++ {
		if g.ClassAt(r, g.Cols-1) != bg {
			bleedRows = append(bleedRows, r)
		}
	}
	if len(bleedRows) < minBleedRows {
		return nil
	}
	culprit, ok := widestCrossingElement(m, float64(g.Width))
	if !ok {
		return nil // content touches the edge but nothing continues past it
	}
	a := Alert{}
	if len(bleedRows) == 1 {
		a.Summary = fmt.Sprintf("row %d: content pixels reach the right edge and continue", bleedRows[0])
	} else {
		a.Summary = fmt.Sprintf("rows %d-%d: content pixels reach the right edge and continue",
			bleedRows[0], bleedRows[len(bleedRows)-1])
	}
	a.Bullets = append(a.Bullets,
		fmt.Sprintf("horizontal overflow in %s", culprit.Selector))
	return []Alert{a}
}

// widestCrossingElement finds the visible element reaching furthest past the
// right edge, to attribute a bleed to by name.
func widestCrossingElement(m *Manifest, width float64) (*Element, bool) {
	if m == nil {
		return nil, false
	}
	var best *Element
	for i := range m.Rects {
		el := &m.Rects[i]
		if el.W < 2 || el.H < 2 {
			continue
		}
		if el.X+el.W <= width {
			continue
		}
		if best == nil || el.X+el.W > best.X+best.W {
			best = el
		}
	}
	return best, best != nil
}

// coverageAlerts compares the colours the computed styles DECLARE with what
// the raster actually rendered. A declared opaque colour with zero exact
// pixels and no perceptual neighbour in the palette never painted anywhere.
func coverageAlerts(q quantized, m *Manifest, imgH int) []Alert {
	if m == nil {
		return nil
	}
	paletteLabs := make([][3]float64, q.pal.Len())
	for i, e := range q.pal.Entries {
		paletteLabs[i] = oklab(e.Color)
	}
	var out []Alert
	seen := make(map[Color]bool) // one alert per unique colour, not per element
	for i := range m.Rects {
		if len(out) >= maxAlertsPerKind {
			break
		}
		el := &m.Rects[i]
		if el.W < 2 || el.H < 2 || el.Y >= float64(imgH) {
			continue
		}
		if el.W*el.H < minCoverageArea {
			continue
		}
		key, ok := parseCSSColor(el.BG)
		if !ok || seen[key] {
			continue
		}
		if q.exact[key] > 0 { // it rendered verbatim — nothing to say
			continue
		}
		lab := oklab(key)
		minDist := math.MaxFloat64
		for _, pl := range paletteLabs {
			if d := oklabDist(lab, pl); d < minDist {
				minDist = d
			}
		}
		if minDist <= minCoverageDistance {
			continue // quantized into an existing entry; still painted
		}
		seen[key] = true
		out = append(out, Alert{
			Summary: fmt.Sprintf("%s is in the loaded CSS but occupies 0 cells in the render", key.Hex()),
			Bullets: []string{fmt.Sprintf(
				"declared as the background of %s but never painted anywhere on the page", el.Selector)},
		})
	}
	return out
}

// mismatchAlerts fires when a region's cells are dominated by a colour
// perceptually far from the background the element declares — the
// "button.cta rendered #3f3f46, not the accent colour" diagnosis.
func mismatchAlerts(q quantized, regions []Region) []Alert {
	var out []Alert
	for _, r := range regions {
		if len(out) >= maxAlertsPerKind {
			break
		}
		if r.DeclaredHex == "" || r.renderedClass < 0 {
			continue
		}
		rendered := q.pal.Entries[r.renderedClass].Color
		if d := oklabDist(oklab(r.declaredKey), oklab(rendered)); d <= minMismatchDistance {
			continue
		}
		out = append(out, Alert{
			Summary: fmt.Sprintf("%s renders %s, not the declared %s",
				r.Selector, r.RenderedHex, r.DeclaredHex),
			Bullets: []string{fmt.Sprintf(
				"the element is painted, but its background class came out %s", r.RenderedHex)},
		})
	}
	return out
}
