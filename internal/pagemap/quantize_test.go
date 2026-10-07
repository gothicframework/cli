package pagemap

import (
	"image"
	"testing"
)

func TestOklabDistance(t *testing.T) {
	black := Color{0, 0, 0, 255}
	white := Color{255, 255, 255, 255}

	if d := oklabDist(oklab(black), oklab(black)); d != 0 {
		t.Errorf("a colour must be distance 0 from itself, got %v", d)
	}
	if d := oklabDist(oklab(black), oklab(white)); d < 0.99 || d > 1.01 {
		t.Errorf("black↔white should sit at OKLab distance ~1, got %v", d)
	}
	purple := Color{0x7c, 0x3a, 0xed, 0xff}
	grey := Color{0x3f, 0x3f, 0x46, 0xff}
	if d := oklabDist(oklab(purple), oklab(grey)); d < 0.12 {
		t.Errorf("purple vs the grey it would have replaced must be clearly perceptual, got %v", d)
	}
}

func TestParseCSSColor(t *testing.T) {
	cases := []struct {
		in   string
		want Color
		ok   bool
	}{
		{"#7c3aed", Color{0x7c, 0x3a, 0xed, 0xff}, true},
		{"#7C3AED", Color{0x7c, 0x3a, 0xed, 0xff}, true},
		{"#7c3", Color{0x77, 0xcc, 0x33, 0xff}, true},
		{"rgb(15, 15, 18)", Color{15, 15, 18, 255}, true},
		{"rgb(15 15 18)", Color{15, 15, 18, 255}, true},
		{"rgba(124, 58, 237, 0.5)", Color{124, 58, 237, 128}, true},
		{"rgb(15 15 18 / 0.5)", Color{15, 15, 18, 128}, true},
		{"rgb(50%, 0%, 100%)", Color{128, 0, 255, 255}, true},
		{"rgba(0, 0, 0, 0)", Color{}, false},
		{"transparent", Color{}, false},
		{"", Color{}, false},
		{"none", Color{}, false},
		{"red", Color{}, false},
		{"rgb(300, 0, 0)", Color{255, 0, 0, 255}, true}, // clamped, as browsers clamp
		{"rgb(nonsense)", Color{}, false},
	}
	for _, tc := range cases {
		got, ok := parseCSSColor(tc.in)
		if ok != tc.ok {
			t.Errorf("parseCSSColor(%q) ok = %v, want %v (got %+v)", tc.in, ok, tc.ok, got)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("parseCSSColor(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

// TestPaletteTopColorsAndTailSnap checks the histogram contract: the top exact
// colours come out in frequency order, and a one-off near-background colour
// ("antialiasing tail") never becomes an entry — it snaps into the background
// and only moves that entry's share. The palette is capped at 3 here so the
// two single-pixel colours ARE the tail.
func TestPaletteTopColorsAndTailSnap(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 100, 100)) // 10000 px
	fillRect(img, image.Rect(0, 0, 100, 100), Color{0x0f, 0x0f, 0x12, 0xff})
	fillRect(img, image.Rect(0, 0, 100, 20), Color{0x1a, 0x1a, 0x20, 0xff})  // 2000 px
	fillRect(img, image.Rect(0, 30, 10, 40), Color{0x7c, 0x3a, 0xed, 0xff})  // 100 px
	fillRect(img, image.Rect(50, 50, 51, 51), Color{0x10, 0x10, 0x14, 0xff}) // 1 px, near-bg tail
	fillRect(img, image.Rect(52, 52, 53, 53), Color{0x80, 0x30, 0xf0, 0xff}) // 1 px, near-purple tail

	q := quantizeImage(img, 3)
	pal := q.pal
	if pal.Len() != 3 {
		t.Fatalf("expected 3 palette entries (tail must snap, not add), got %d: %s",
			pal.Len(), paletteDump(pal))
	}
	bg := pal.Entries[0]
	if bg.Color != (Color{0x0f, 0x0f, 0x12, 0xff}) || bg.Exact != 7898 {
		t.Errorf("dominant entry wrong: %+v", bg)
	}
	// 7899 own + the #101014 tail pixel = 7900 → share 0.79.
	if bg.Share < 0.789 || bg.Share > 0.791 {
		t.Errorf("tail pixel must snap into the dominant share, got %v", bg.Share)
	}
	if pal.Entries[1].Color != (Color{0x1a, 0x1a, 0x20, 0xff}) {
		t.Errorf("second entry must be the surface in frequency order, got %s", pal.Entries[1].Hex)
	}
	if pal.Entries[2].Color != (Color{0x7c, 0x3a, 0xed, 0xff}) {
		t.Errorf("third entry must be the purple in frequency order, got %s", pal.Entries[2].Hex)
	}
	// The near-purple tail pixel snaps to the purple entry, lifting its share
	// to 101/10000 — and stays well away from the background.
	if pal.Entries[2].Share < 0.0100 || pal.Entries[2].Share > 0.0102 {
		t.Errorf("purple tail should snap to the purple entry, got share %v", pal.Entries[2].Share)
	}
}

// TestQuantizePreservesSmallAccentOnDark is the load-bearing quantization
// property: a purple button on a dark page stays a distinct palette entry
// instead of being blended away.
func TestQuantizePreservesSmallAccentOnDark(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 320, 160))
	fillRect(img, img.Bounds(), Color{0x0f, 0x0f, 0x12, 0xff})
	fillRect(img, image.Rect(40, 100, 136, 132), Color{0x7c, 0x3a, 0xed, 0xff}) // 6% of the page

	q := quantizeImage(img, DefaultMaxColors)
	if q.pal.Len() != 2 {
		t.Fatalf("expected exactly 2 entries, got %d", q.pal.Len())
	}
	entry := q.pal.Entries[1]
	if entry.Color != (Color{0x7c, 0x3a, 0xed, 0xff}) {
		t.Errorf("purple accent lost to quantization: %+v", entry)
	}
	if d := oklabDist(oklab(entry.Color), oklab(q.pal.Entries[0].Color)); d < 0.12 {
		t.Errorf("accent must stay perceptually distinct from the background, dist %v", d)
	}
}

func paletteDump(pal Palette) string {
	s := ""
	for _, e := range pal.Entries {
		s += " " + e.Hex
	}
	return s
}
