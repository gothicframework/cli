// Package pagemap encodes a rendered page into the Gothic page-view payload:
// the page's own colour palette, one letter per grid cell, the DOM regions the
// cells land in, and the alerts the raster itself implies (palette coverage,
// edge bleed, overflow). Everything after the PNG is a pure function of the
// raster plus the DOM manifest, so the encoder is testable against fixture
// PNGs with no browser and no timing.
package pagemap

import (
	"fmt"
	"image"
	"math"
	"sort"
	"strconv"
	"strings"
)

// DefaultMaxColors is the palette size the encoder quantizes to. A UI page has
// a couple dozen real colours plus an antialiasing tail; the top exact colours
// carry the layout, and every other pixel snaps to the nearest one.
const DefaultMaxColors = 10

// Color is one exact sRGB colour with alpha. It is the currency the whole
// encoder works in and doubles as a hash-map key for the exact-colour
// histogram.
type Color struct{ R, G, B, A uint8 }

// Hex renders the colour the way the payload prints it. Alpha is not printed:
// a page view is built from an opaque compositor screenshot.
func (c Color) Hex() string {
	return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B)
}

// PaletteEntry is one palette slot: an exact colour from the raster, how many
// pixels carried it verbatim, and its share of the image once the antialiasing
// tail snapped to the nearest palette entry.
type PaletteEntry struct {
	Color Color
	Hex   string
	Exact int     // pixels whose colour is exactly this one
	Share float64 // 0..1, share of all pixels after the tail snapped
}

// Palette is an extracted colour palette, sorted by pixel frequency with the
// dominant (background) colour first.
type Palette struct {
	Entries []PaletteEntry
}

// Dominant returns the most frequent colour — the page's background.
func (p Palette) Dominant() PaletteEntry { return p.Entries[0] }

// Len returns the number of palette entries.
func (p Palette) Len() int { return len(p.Entries) }

// quantized carries everything the rest of the encoder needs from the
// quantization pass: the palette, the raw exact-colour histogram (palette
// coverage is a question about EXACT colours, not snapped ones), and the map
// from every exact colour in the raster to its palette class.
type quantized struct {
	pal   Palette
	clsOf map[Color]int
	exact map[Color]int
}

// quantizeImage builds the exact-colour histogram at full resolution, keeps
// the top maxColors colours, and snaps every other pixel to the nearest entry
// by OKLab perceptual distance. No k-means: the top colours are already right,
// the tail is only rounding noise.
func quantizeImage(img image.Image, maxColors int) quantized {
	if maxColors <= 0 {
		maxColors = DefaultMaxColors
	}
	b := img.Bounds()
	hist := make(map[Color]int, 512)
	total := 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			hist[colorAt(img, x, y)]++
			total++
		}
	}
	if total == 0 {
		return quantized{clsOf: map[Color]int{}, exact: hist}
	}

	type counted struct {
		c Color
		n int
	}
	all := make([]counted, 0, len(hist))
	for c, n := range hist {
		all = append(all, counted{c, n})
	}
	// Deterministic order — count desc, then colour bytes asc — so two runs
	// over identical rasters always produce identical palettes.
	sort.Slice(all, func(i, j int) bool {
		if all[i].n != all[j].n {
			return all[i].n > all[j].n
		}
		return colorLess(all[i].c, all[j].c)
	})
	if len(all) > maxColors {
		all = all[:maxColors]
	}

	top := make(map[Color]int, len(all)) // colour → its own palette index
	for i, e := range all {
		top[e.c] = i
	}
	labs := make([][3]float64, len(all))
	for i, e := range all {
		labs[i] = oklab(e.c)
	}

	// Assign every exact colour in the raster a palette class. Entries inside
	// the palette map to themselves; the tail maps to its nearest entry. Each
	// distinct colour is visited exactly once here, so this is the memo.
	clsOf := make(map[Color]int, len(hist))
	share := make([]int, len(all))
	for c, n := range hist {
		if idx, ok := top[c]; ok {
			clsOf[c] = idx
			share[idx] += n
			continue
		}
		best, bestD := 0, math.MaxFloat64
		lab := oklab(c)
		for i := range all {
			if d := oklabDist(lab, labs[i]); d < bestD {
				best, bestD = i, d
			}
		}
		clsOf[c] = best
		share[best] += n
	}

	entries := make([]PaletteEntry, len(all))
	for i, e := range all {
		entries[i] = PaletteEntry{
			Color: e.c,
			Hex:   e.c.Hex(),
			Exact: hist[e.c],
			Share: float64(share[i]) / float64(total),
		}
	}
	return quantized{
		pal:   Palette{Entries: entries},
		clsOf: clsOf,
		exact: hist,
	}
}

// colorAt reads the pixel at (x, y) as the encoder's exact-colour currency.
func colorAt(img image.Image, x, y int) Color {
	r, g, b, a := img.At(x, y).RGBA()
	return Color{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), uint8(a >> 8)}
}

func colorLess(a, b Color) bool {
	if a.R != b.R {
		return a.R < b.R
	}
	if a.G != b.G {
		return a.G < b.G
	}
	if a.B != b.B {
		return a.B < b.B
	}
	return a.A < b.A
}

func srgbToLinear(c uint8) float64 {
	f := float64(c) / 255
	if f <= 0.04045 {
		return f / 12.92
	}
	return math.Pow((f+0.055)/1.055, 2.4)
}

// oklab converts an sRGB colour to OKLab (Ottosson's published constants), a
// perceptual space in which plain Euclidean distance tracks the distance a
// human sees — which is what snapping the antialiasing tail, and alert
// thresholds, should measure.
func oklab(c Color) [3]float64 {
	r := srgbToLinear(c.R)
	g := srgbToLinear(c.G)
	b := srgbToLinear(c.B)
	l := math.Cbrt(0.4122214708*r + 0.5363325363*g + 0.0514459282*b)
	m := math.Cbrt(0.2119034982*r + 0.6806995451*g + 0.1073969566*b)
	s := math.Cbrt(0.0883024619*r + 0.2817188376*g + 0.6299787005*b)
	return [3]float64{
		0.2104542553*l + 0.7936177850*m - 0.0040720468*s,
		1.9779984951*l - 2.4285922050*m + 0.4505937099*s,
		0.0259040371*l + 0.7827717662*m - 0.8086757660*s,
	}
}

func oklabDist(a, b [3]float64) float64 {
	dl, da, db := a[0]-b[0], a[1]-b[1], a[2]-b[2]
	return math.Sqrt(dl*dl + da*da + db*db)
}

// parseCSSColor reads the computed-style colour strings a browser manifest
// carries ("#7c3aed", "rgb(15, 15, 18)", "rgba(1, 2, 3, 0.5)", and the modern
// space-separated form). Anything unparseable or transparent reports false —
// the caller then simply has no declared colour to compare against.
func parseCSSColor(s string) (Color, bool) {
	s = strings.TrimSpace(strings.ToLower(s))
	switch s {
	case "", "none", "transparent", "initial", "inherit", "currentcolor":
		return Color{}, false
	}
	if strings.HasPrefix(s, "#") {
		return parseCSSColorHex(s[1:])
	}
	open := strings.IndexByte(s, '(')
	if open < 0 || !strings.HasSuffix(s, ")") {
		return Color{}, false
	}
	switch strings.TrimSpace(s[:open]) {
	case "rgb", "rgba":
	default:
		return Color{}, false
	}
	body := strings.NewReplacer(",", " ", "/", " ").Replace(s[open+1 : len(s)-1])
	parts := strings.Fields(body)
	if len(parts) != 3 && len(parts) != 4 {
		return Color{}, false
	}
	ch := make([]uint8, 3)
	for i := 0; i < 3; i++ {
		v, ok := cssComponent(parts[i])
		if !ok {
			return Color{}, false
		}
		ch[i] = uint8(clamp255(v))
	}
	a := float64(255)
	if len(parts) == 4 {
		v, ok := cssComponent(parts[3]) // alpha is a fraction, not a byte
		if !ok {
			return Color{}, false
		}
		a = v * 255
	}
	if a <= 13 { // ~5%: hides content less than it renders nothing
		return Color{}, false
	}
	return Color{ch[0], ch[1], ch[2], uint8(clamp255(a))}, true
}

func parseHexPair(s string) (uint8, bool) {
	v, err := strconv.ParseUint(s, 16, 8)
	if err != nil {
		return 0, false
	}
	return uint8(v), true
}

func parseCSSColorHex(hex string) (Color, bool) {
	switch len(hex) {
	case 3, 4: // #rgb / #rgba — digits expand
		var cs [3]uint8
		for i := 0; i < 3; i++ {
			v, ok := parseHexPair(hex[i : i+1])
			if !ok {
				return Color{}, false
			}
			cs[i] = v*16 + v
		}
		a := uint8(255)
		if len(hex) == 4 {
			v, ok := parseHexPair(hex[3:4])
			if !ok {
				return Color{}, false
			}
			if a = v * 16; a == 0 {
				return Color{}, false
			}
		}
		return Color{cs[0], cs[1], cs[2], a}, true
	case 6, 8:
		var cs [3]uint8
		for i := 0; i < 3; i++ {
			v, ok := parseHexPair(hex[i*2 : i*2+2])
			if !ok {
				return Color{}, false
			}
			cs[i] = v
		}
		a := uint8(255)
		if len(hex) == 8 {
			v, ok := parseHexPair(hex[6:8])
			if !ok {
				return Color{}, false
			}
			if a = v; a == 0 {
				return Color{}, false
			}
		}
		return Color{cs[0], cs[1], cs[2], a}, true
	}
	return Color{}, false
}

// cssComponent parses one rgb() component: a byte value, a percentage, or a
// clamped float as the browser serializes it.
func cssComponent(s string) (float64, bool) {
	pct := strings.HasSuffix(s, "%")
	if pct {
		s = strings.TrimSuffix(s, "%")
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) {
		return 0, false
	}
	if pct {
		v = v / 100 * 255
	}
	return v, true
}

func clamp255(v float64) int {
	return int(math.Round(math.Max(0, math.Min(255, v))))
}
