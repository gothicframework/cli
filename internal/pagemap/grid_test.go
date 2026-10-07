package pagemap

import (
	"image"
	"testing"
)

func TestRuleLine(t *testing.T) {
	if got, want := ruleLine(13), "   0    5   10"; got != want {
		t.Errorf("ruleLine(13) = %q, want %q", got, want)
	}
	if got, want := ruleLine(48), "   0    5   10   15   20   25   30   35   40   45"; got != want {
		t.Errorf("ruleLine(48) =\n got %q\nwant %q", got, want)
	}
}

// TestClassGridModePerCell is the mode-vs-mean contract: the cell reports its
// most frequent class — a purple-majority cell stays purple — while the MEAN
// of the very same pixels is a blended colour perceptually far from the
// accent, the blend that used to swallow purple buttons into dark
// backgrounds.
func TestClassGridModePerCell(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 24, 12)) // 4 cols × 2 rows of 6x6 cells
	fillRect(img, img.Bounds(), Color{0x0f, 0x0f, 0x12, 0xff})
	fillRect(img, image.Rect(12, 0, 16, 6), Color{0x7c, 0x3a, 0xed, 0xff}) // 4x6 px inside cell (row 0, col 2)

	q := quantizeImage(img, DefaultMaxColors)
	g := buildGrid(img, q, 4, 2)
	if g.Cells == nil {
		t.Fatal("empty grid")
	}
	purpleClass := -1
	for i, e := range q.pal.Entries {
		if e.Color == (Color{0x7c, 0x3a, 0xed, 0xff}) {
			purpleClass = i
		}
	}
	if purpleClass < 0 {
		t.Fatalf("purple not in palette: %s", paletteDump(q.pal))
	}
	if got := g.ClassAt(0, 2); got != purpleClass {
		t.Errorf("cell (0,2) must classify purple (mode), got class %d", got)
	}
	if got := g.ClassAt(0, 1); got != 0 {
		t.Errorf("untouched cell (0,1) must stay the background class, got %d", got)
	}
	if got := g.ClassAt(1, 2); got != 0 {
		t.Errorf("cell below the strip (1,2) must stay background, got %d", got)
	}

	// The very same pixels averaged would be a dull blend, perceptually away
	// from the accent — the failure mode mode-per-cell exists to prevent.
	var r, gr, b, a uint32
	for y := 0; y < 6; y++ {
		for x := 12; x < 18; x++ {
			rr, gg, bb, aa := img.At(x, y).RGBA()
			r += rr >> 8
			gr += gg >> 8
			b += bb >> 8
			a += aa >> 8
		}
	}
	mean := Color{uint8(r / 36), uint8(gr / 36), uint8(b / 36), uint8(a / 36)}
	if d := oklabDist(oklab(mean), oklab(Color{0x7c, 0x3a, 0xed, 0xff})); d < 0.08 {
		t.Errorf("expected the mean blend to sit perceptually far from the accent, got %v", d)
	}
}

func TestPaletteSymbols(t *testing.T) {
	// Dark-theme-ish palette: background, a surface a hair above it, then two
	// loud colours. '.' / ':' for the quiet pair, letters for the rest.
	img := image.NewRGBA(image.Rect(0, 0, 40, 40))
	fillRect(img, img.Bounds(), Color{0x0f, 0x0f, 0x12, 0xff})
	fillRect(img, image.Rect(0, 0, 40, 10), Color{0x1a, 0x1a, 0x20, 0xff})
	fillRect(img, image.Rect(0, 10, 40, 12), Color{0xe4, 0xe4, 0xe7, 0xff})
	fillRect(img, image.Rect(0, 12, 40, 14), Color{0x7c, 0x3a, 0xed, 0xff})

	q := quantizeImage(img, DefaultMaxColors)
	syms := paletteSymbols(q.pal)
	want := []rune{'.', ':', 'A', 'B'}
	if len(syms) != len(want) {
		t.Fatalf("expected %d symbols, got %d", len(want), len(syms))
	}
	for i := range want {
		if syms[i] != want[i] {
			t.Errorf("symbol %d = %q, want %q", i, syms[i], want[i])
		}
	}
}

func TestGridRender(t *testing.T) {
	g := Grid{Cols: 4, Rows: 2, Width: 40, Height: 20, Cells: []int{0, 1, 0, 2, 0, 0, 0, 0}}
	got := g.render([]rune{'.', ':', 'A'})
	want := "   0\n" +
		" 0 .:.A\n" +
		" 1 ....\n"
	if got != want {
		t.Errorf("render mismatch:\n got %q\nwant %q", got, want)
	}
}
