package pagemap

import (
	"fmt"
	"image"
	"strings"
)

// Locked defaults: the grid shape and the palette size.
const (
	DefaultGridW = 48
	DefaultGridH = 24
)

// Options carries what the raster alone cannot tell the encoder. URL and
// Engine are capture metadata; ViewportW/H override the reported viewport
// (0 = use the raster size); GridW/H are the cell grid (0 = 48x24).
type Options struct {
	URL       string
	Engine    string
	ViewportW int
	ViewportH int
	GridW     int
	GridH     int
}

// Encode turns a rendered page into the page-view payload a text-only model
// reads: grid = geometry and colour, legend = identity and content, alerts =
// the conclusions. img is the full-resolution screenshot; m the DOM manifest
// (nil is allowed and simply produces no regions and no manifest-derived
// alerts).
func Encode(img image.Image, m *Manifest, opts Options) string {
	if opts.GridW <= 0 {
		opts.GridW = DefaultGridW
	}
	if opts.GridH <= 0 {
		opts.GridH = DefaultGridH
	}
	b := img.Bounds()
	imgW, imgH := b.Dx(), b.Dy()
	vW, vH := opts.ViewportW, opts.ViewportH
	if vW <= 0 {
		vW = imgW
	}
	if vH <= 0 {
		vH = imgH
	}

	q := quantizeImage(img, DefaultMaxColors)
	syms := paletteSymbols(q.pal)
	g := buildGrid(img, q, opts.GridW, opts.GridH)
	regions := mapRegions(q, g, m)
	alerts := buildAlerts(g, q, m, regions)

	var sb strings.Builder
	sb.WriteString("=== GOTHIC PAGE VIEW ===\n")
	fmt.Fprintf(&sb, "URL       %s\n", opts.URL)
	fmt.Fprintf(&sb, "ENGINE    %s\n", engineLabel(opts.Engine))
	fmt.Fprintf(&sb, "VIEWPORT  %dx%d   scroll %s\n", vW, vH, scrollText(m))
	fmt.Fprintf(&sb, "GRID      %dx%d   cell %.0fx%.0fpx\n",
		g.Cols, g.Rows, float64(imgW)/float64(g.Cols), float64(imgH)/float64(g.Rows))

	sb.WriteString("\nPALETTE (extracted from the render)\n")
	labels := paletteLabels(m, q.pal)
	for i, e := range q.pal.Entries {
		fmt.Fprintf(&sb, "  %s  %s  %-*s %3.0f%%\n", string(syms[i]), e.Hex, legendLabelWidth, labels[i], e.Share*100)
	}

	sb.WriteString("\n")
	sb.WriteString(g.render(syms))

	sb.WriteString("\nREGIONS  (point-in-rect against the DOM manifest)\n")
	if len(regions) == 0 {
		sb.WriteString("  (none)\n")
	}
	for _, r := range regions {
		sb.WriteString(regionLine(r))
		sb.WriteByte('\n')
	}

	sb.WriteString("\nALERTS (derived from the raster)\n")
	if len(alerts) == 0 {
		sb.WriteString("  (none)\n")
	}
	for _, a := range alerts {
		fmt.Fprintf(&sb, "  ⚠ %s\n", a.Summary)
		for _, bullet := range a.Bullets {
			fmt.Fprintf(&sb, "    → %s\n", bullet)
		}
	}
	return sb.String()
}

func engineLabel(engine string) string {
	if engine == "" {
		return "unknown (png input)"
	}
	return engine
}

// scrollText renders the scroll offset the manifest recorded at capture time.
func scrollText(m *Manifest) string {
	if m == nil {
		return "0,0"
	}
	return fmt.Sprintf("%.0f,%.0f", m.Scroll[0], m.Scroll[1])
}

// legendLabelWidth is the column width reserved for the palette legend's
// provenance label.
const legendLabelWidth = 24

// paletteLabels derives each palette entry's provenance from the manifest: a
// colour that is some element's computed background is reported as
// "bg <sel>", one that is only a text colour as "fg <sel>", and the rest as
// plain "render". The dominant entry is the page background by definition.
func paletteLabels(m *Manifest, pal Palette) []string {
	labels := make([]string, pal.Len())
	for i := range labels {
		labels[i] = "render"
	}
	if len(labels) > 0 {
		labels[0] = "background"
	}
	if m == nil {
		return labels
	}
	for i := 1; i < pal.Len(); i++ {
		entry := pal.Entries[i].Color
		for j := range m.Rects {
			el := &m.Rects[j]
			if el.W*el.H < minCoverageArea {
				continue
			}
			if key, ok := parseCSSColor(el.BG); ok && key == entry {
				labels[i] = trimLabel("bg " + el.Selector)
				break
			}
			if key, ok := parseCSSColor(el.FG); ok && key == entry {
				labels[i] = trimLabel("fg " + el.Selector)
				break
			}
		}
	}
	return labels
}

// trimLabel keeps the legend column fixed-width, cutting long selectors at
// the front (the distinguishing tail is what matters).
func trimLabel(s string) string {
	if len(s) <= legendLabelWidth {
		return s
	}
	return "…" + s[len(s)-legendLabelWidth+1:]
}
