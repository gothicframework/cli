package pagemap

import (
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The three fixture scenes every page-view test builds on. They are drawn
// deterministically in code; `-update` also writes the PNGs, the DOM manifests
// and the golden payloads under testdata/pageview/ so a fresh clone has real
// files to eyeball. The normal run regenerates everything in memory and only
// checks that the committed fixtures agree (drift guard) and that the payload
// matches the golden.

var updateGolden = flag.Bool("update", false, "rewrite page-view fixture and golden files")

type pageviewFixture struct {
	name        string
	url         string
	engine      string
	description string
	draw        func() (*image.RGBA, *Manifest)
}

// fillRect paints a solid rectangle in the encoder's exact-colour currency.
func fillRect(img *image.RGBA, r image.Rectangle, c Color) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			img.SetRGBA(x, y, color.RGBA{c.R, c.G, c.B, c.A})
		}
	}
}

var pageviewFixtures = []pageviewFixture{
	{
		name:        "dark",
		url:         "http://localhost:3000/docs/routing",
		engine:      "chromium 131",
		description: "dark theme page: background, surface header, bright text, painted accent button",
		draw: func() (*image.RGBA, *Manifest) {
			img := image.NewRGBA(image.Rect(0, 0, 320, 160))
			fillRect(img, img.Bounds(), Color{R: 0x0f, G: 0x0f, B: 0x12, A: 0xff})
			fillRect(img, image.Rect(0, 0, 320, 24), Color{R: 0x1a, G: 0x1a, B: 0x20, A: 0xff})
			fillRect(img, image.Rect(176, 60, 280, 92), Color{R: 0xe4, G: 0xe4, B: 0xe7, A: 0xff})
			fillRect(img, image.Rect(40, 100, 136, 132), Color{R: 0x7c, G: 0x3a, B: 0xed, A: 0xff})
			m := &Manifest{Scroll: [2]float64{0, 0}, Rects: []Element{
				{Selector: "body", X: 0, Y: 0, W: 320, H: 160, BG: "#0f0f12"},
				{Selector: "header.site-header", X: 0, Y: 0, W: 320, H: 24, BG: "#1a1a20", FG: "#e4e4e7"},
				{Selector: "p.hero", X: 176, Y: 60, W: 104, H: 32, BG: "rgba(0, 0, 0, 0)", FG: "#e4e4e7"},
				{Selector: "button.cta", X: 40, Y: 100, W: 96, H: 32, BG: "#7c3aed", FG: "#e4e4e7"},
			}}
			return img, m
		},
	},
	{
		name:        "overflow",
		url:         "http://localhost:3000/docs/routing",
		engine:      "chromium 131",
		description: "a child span painted past the right edge of the viewport",
		draw: func() (*image.RGBA, *Manifest) {
			img := image.NewRGBA(image.Rect(0, 0, 320, 160))
			fillRect(img, img.Bounds(), Color{R: 0x0f, G: 0x0f, B: 0x12, A: 0xff})
			fillRect(img, image.Rect(16, 40, 240, 120), Color{R: 0x1a, G: 0x1a, B: 0x20, A: 0xff})
			fillRect(img, image.Rect(200, 48, 320, 64), Color{R: 0xe4, G: 0xe4, B: 0xe7, A: 0xff})
			m := &Manifest{Scroll: [2]float64{0, 0}, Rects: []Element{
				{Selector: "body", X: 0, Y: 0, W: 320, H: 160, BG: "#0f0f12"},
				{Selector: "div.card", X: 16, Y: 40, W: 224, H: 80, BG: "#1a1a20", FG: "#e4e4e7"},
				{Selector: "span.piece", X: 200, Y: 48, W: 200, H: 16, BG: "#e4e4e7", Overflows: true},
			}}
			return img, m
		},
	},
	{
		name:        "unpainted",
		url:         "http://localhost:3000/docs/routing",
		engine:      "chromium 131",
		description: "the CSS declares an accent button, but the render shows plain grey",
		draw: func() (*image.RGBA, *Manifest) {
			img := image.NewRGBA(image.Rect(0, 0, 320, 160))
			fillRect(img, img.Bounds(), Color{R: 0x0f, G: 0x0f, B: 0x12, A: 0xff})
			fillRect(img, image.Rect(0, 0, 320, 24), Color{R: 0x1a, G: 0x1a, B: 0x20, A: 0xff})
			fillRect(img, image.Rect(40, 100, 136, 132), Color{R: 0x3f, G: 0x3f, B: 0x46, A: 0xff})
			m := &Manifest{Scroll: [2]float64{0, 0}, Rects: []Element{
				{Selector: "body", X: 0, Y: 0, W: 320, H: 160, BG: "#0f0f12"},
				{Selector: "header.site-header", X: 0, Y: 0, W: 320, H: 24, BG: "#1a1a20", FG: "#e4e4e7"},
				{Selector: "button.cta", X: 40, Y: 100, W: 96, H: 32, BG: "#7c3aed", FG: "#e4e4e7"},
			}}
			return img, m
		},
	},
}

func fixtureDir(t *testing.T) string {
	t.Helper()
	return filepath.Join("testdata", "pageview")
}

func encodeFixture(fx pageviewFixture) (string, *image.RGBA, *Manifest) {
	img, m := fx.draw()
	payload := Encode(img, m, Options{URL: fx.url, Engine: fx.engine})
	return payload, img, m
}

// TestPageViewGolden is the whole-pipeline golden test. Run with -update to
// rewrite the fixture PNGs, the manifest JSONs and the golden payloads.
func TestPageViewGolden(t *testing.T) {
	dir := fixtureDir(t)
	if *updateGolden {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	for _, fx := range pageviewFixtures {
		t.Run(fx.name, func(t *testing.T) {
			payload, img, m := encodeFixture(fx)
			pngPath := filepath.Join(dir, fx.name+".png")
			manifestPath := filepath.Join(dir, fx.name+".manifest.json")
			goldenPath := filepath.Join(dir, fx.name+".golden")

			if *updateGolden {
				if err := writePNGFixture(pngPath, img); err != nil {
					t.Fatalf("writing fixture png: %v", err)
				}
				if err := writeManifestFixture(manifestPath, m); err != nil {
					t.Fatalf("writing fixture manifest: %v", err)
				}
				if err := os.WriteFile(goldenPath, []byte(payload), 0o644); err != nil {
					t.Fatalf("writing golden: %v", err)
				}
				return
			}

			guardFixture(t, pngPath, manifestPath, img, m)

			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("reading golden (run with -update to create): %v", err)
			}
			if got := string(want); got != payload {
				t.Errorf("payload differs from golden %s\nfirst diff: %s",
					goldenPath, firstDiff(got, payload))
			}
		})
	}
}

// guardFixture decodes the committed fixture files and requires them to be
// exactly what the draw code produces — the fixtures never rot silently.
func guardFixture(t *testing.T, pngPath, manifestPath string, img *image.RGBA, m *Manifest) {
	t.Helper()
	f, err := os.Open(pngPath)
	if err != nil {
		t.Fatalf("opening committed fixture png (run with -update to create): %v", err)
	}
	defer f.Close()
	committed, err := png.Decode(f)
	if err != nil {
		t.Fatalf("decoding committed fixture png: %v", err)
	}
	committedRGBA, ok := committed.(*image.RGBA)
	if !ok {
		t.Fatalf("committed fixture png decodes as %T, not *image.RGBA", committed)
	}
	if !samePixels(committedRGBA, img) {
		t.Errorf("committed fixture %s disagrees with the draw code — run with -update", pngPath)
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("reading committed fixture manifest (run with -update to create): %v", err)
	}
	parsed, err := ParseManifest(data)
	if err != nil {
		t.Fatalf("decoding committed fixture manifest: %v", err)
	}
	if !sameManifests(parsed, m) {
		t.Errorf("committed manifest %s disagrees with the draw code — run with -update", manifestPath)
	}
}

func writePNGFixture(path string, img *image.RGBA) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

func writeManifestFixture(path string, m *Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func samePixels(a, b *image.RGBA) bool {
	if !a.Bounds().Eq(b.Bounds()) {
		return false
	}
	for i := range a.Pix {
		if a.Pix[i] != b.Pix[i] {
			return false
		}
	}
	return true
}

func sameManifests(a, b *Manifest) bool {
	if a.Scroll != b.Scroll || len(a.Rects) != len(b.Rects) {
		return false
	}
	for i := range a.Rects {
		if fmt.Sprint(a.Rects[i]) != fmt.Sprint(b.Rects[i]) {
			return false
		}
	}
	return true
}

// firstDiff locates the first differing line for a readable golden failure.
func firstDiff(want, got string) string {
	wantLines := strings.Split(want, "\n")
	gotLines := strings.Split(got, "\n")
	for i := 0; i < len(wantLines) || i < len(gotLines); i++ {
		var w, g string
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if w != g {
			return fmt.Sprintf("line %d:\n  want %q\n  got  %q", i+1, w, g)
		}
	}
	return "(identical)"
}
