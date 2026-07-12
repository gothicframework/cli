package cmd

import (
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	webp "github.com/gen2brain/webp"
	gothic_cli "github.com/gothicframework/cli/v3/internal/cli"
	"github.com/spf13/cobra"
)

func writeWEBP(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 200, 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create webp: %v", err)
	}
	defer f.Close()
	if err := webp.Encode(f, img, webp.Options{Quality: 90}); err != nil {
		t.Fatalf("encode webp: %v", err)
	}
}

// isWebP reports whether b is a real WebP file (RIFF....WEBP), guarding against
// the old bug where the .webp original was actually lossless PNG bytes.
func isWebP(b []byte) bool {
	return len(b) >= 12 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP"
}

func writePNG(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 0, 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create png: %v", err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
}

func writeJPEG(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), 100, uint8(y), 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create jpeg: %v", err)
	}
	defer f.Close()
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
}

func TestNewImgOptimizationCommandCli(t *testing.T) {
	cli := gothic_cli.NewCli()
	cmd := NewImgOptimizationCommandCli(&cli)
	if cmd.inputDir != "./optimize" || cmd.outputDir != "./public" {
		t.Errorf("unexpected default dirs: in=%q out=%q", cmd.inputDir, cmd.outputDir)
	}
}

func TestOptimizeImagesPNGAndJPEG(t *testing.T) {
	chdirTemp(t)
	writeConfig(t, `{"projectName":"demo","goModuleName":"demo","optimizeImages":{"lowResolutionRate":25}}`)
	if err := os.MkdirAll("optimize", 0o755); err != nil {
		t.Fatalf("mkdir optimize: %v", err)
	}
	writePNG(t, "optimize/logo.png", 100, 80)
	writeJPEG(t, "optimize/photo.jpg", 120, 60)

	cli := gothic_cli.NewCli()
	cmd := NewImgOptimizationCommandCli(&cli)
	if err := cmd.OptimizeImages(); err != nil {
		t.Fatalf("OptimizeImages() error: %v", err)
	}

	for _, p := range []string{
		"public/logo/original.png",
		"public/logo/blurred.png",
		"public/photo/original.jpg",
		"public/photo/blurred.jpg",
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected output %s: %v", p, err)
		}
	}

	// Blurred variant must be smaller than the original on disk.
	orig, _ := os.Stat("public/logo/original.png")
	blur, _ := os.Stat("public/logo/blurred.png")
	if orig != nil && blur != nil && blur.Size() >= orig.Size() {
		t.Errorf("expected blurred png (%d) smaller than original (%d)", blur.Size(), orig.Size())
	}
}

func TestOptimizeImagesDefaultResolutionRate(t *testing.T) {
	chdirTemp(t)
	// No lowResolutionRate -> default of 20 is used.
	writeConfig(t, `{"projectName":"demo","goModuleName":"demo"}`)
	if err := os.MkdirAll("optimize", 0o755); err != nil {
		t.Fatalf("mkdir optimize: %v", err)
	}
	writePNG(t, "optimize/pic.png", 50, 50)

	cli := gothic_cli.NewCli()
	cmd := NewImgOptimizationCommandCli(&cli)
	if err := cmd.OptimizeImages(); err != nil {
		t.Fatalf("OptimizeImages() error: %v", err)
	}
	if _, err := os.Stat(filepath.Join("public", "pic", "blurred.png")); err != nil {
		t.Errorf("expected blurred output with default rate: %v", err)
	}
}

func TestOptimizeImagesUnsupportedFormat(t *testing.T) {
	chdirTemp(t)
	writeConfig(t, `{"projectName":"demo","goModuleName":"demo"}`)
	if err := os.MkdirAll("optimize", 0o755); err != nil {
		t.Fatalf("mkdir optimize: %v", err)
	}
	if err := os.WriteFile("optimize/notes.txt", []byte("hello"), 0o644); err != nil {
		t.Fatalf("write txt: %v", err)
	}

	cli := gothic_cli.NewCli()
	cmd := NewImgOptimizationCommandCli(&cli)
	if err := cmd.OptimizeImages(); err == nil {
		t.Fatal("expected error for unsupported file format")
	}
}

func TestOptimizeImagesFailsOnSubdirectory(t *testing.T) {
	chdirTemp(t)
	writeConfig(t, `{"projectName":"demo","goModuleName":"demo"}`)
	// A subdirectory inside ./optimize triggers the "optimizeImages key not
	// found" error branch (the code only handles regular files).
	if err := os.MkdirAll("optimize/nested", 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cli := gothic_cli.NewCli()
	cmd := NewImgOptimizationCommandCli(&cli)
	if err := cmd.OptimizeImages(); err == nil {
		t.Fatal("expected error when optimize contains a subdirectory")
	}
}

func TestOptimizeImagesJpegExtension(t *testing.T) {
	chdirTemp(t)
	writeConfig(t, `{"projectName":"demo","goModuleName":"demo"}`)
	if err := os.MkdirAll("optimize", 0o755); err != nil {
		t.Fatalf("mkdir optimize: %v", err)
	}
	// .jpeg (not .jpg) must be decoded via the jpeg branch too.
	writeJPEG(t, "optimize/banner.jpeg", 64, 64)
	cli := gothic_cli.NewCli()
	cmd := NewImgOptimizationCommandCli(&cli)
	if err := cmd.OptimizeImages(); err != nil {
		t.Fatalf("OptimizeImages() error: %v", err)
	}
	if _, err := os.Stat("public/banner/blurred.jpeg"); err != nil {
		t.Errorf("expected blurred.jpeg output: %v", err)
	}
}

func TestOptimizeImagesFailsWithoutConfig(t *testing.T) {
	chdirTemp(t)
	cli := gothic_cli.NewCli()
	cmd := NewImgOptimizationCommandCli(&cli)
	if err := cmd.OptimizeImages(); err == nil {
		t.Fatal("expected error without gothic-config.json")
	}
}

func TestOptimizeImagesFailsWithoutOptimizeDir(t *testing.T) {
	chdirTemp(t)
	writeConfig(t, `{"projectName":"demo","goModuleName":"demo"}`)
	// No ./optimize directory: os.ReadDir must fail.
	cli := gothic_cli.NewCli()
	cmd := NewImgOptimizationCommandCli(&cli)
	if err := cmd.OptimizeImages(); err == nil {
		t.Fatal("expected error when optimize dir is missing")
	}
}

// TestOptimizeImagesWebP verifies a .webp source is re-encoded as a REAL WebP
// (not lossless PNG bytes in a .webp file), for both the original and blurred
// variants — the root-cause fix for the image bloat.
func TestOptimizeImagesWebP(t *testing.T) {
	chdirTemp(t)
	writeConfig(t, `{"projectName":"demo","goModuleName":"demo"}`)
	if err := os.MkdirAll("optimize", 0o755); err != nil {
		t.Fatalf("mkdir optimize: %v", err)
	}
	writeWEBP(t, "optimize/hero.webp", 96, 64)

	cli := gothic_cli.NewCli()
	cmd := NewImgOptimizationCommandCli(&cli)
	if err := cmd.OptimizeImages(); err != nil {
		t.Fatalf("OptimizeImages() error: %v", err)
	}
	for _, p := range []string{"public/hero/original.webp", "public/hero/blurred.webp"} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if !isWebP(b) {
			t.Errorf("%s is not a real WebP (must not be PNG-in-.webp): header=%q", p, b[:min(12, len(b))])
		}
	}
}

func TestClampQuality(t *testing.T) {
	cases := map[int]int{0: 1, -10: 1, 1: 1, 50: 50, 100: 100, 101: 100, 500: 100}
	for in, want := range cases {
		if got := clampQuality(in); got != want {
			t.Errorf("clampQuality(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestResolveQuality(t *testing.T) {
	orig := optimizeImagesQuality
	t.Cleanup(func() { optimizeImagesQuality = orig })

	optimizeImagesQuality = 0
	if got := resolveQuality(0); got != defaultOriginalQuality {
		t.Errorf("no flag, no config: got %d, want default %d", got, defaultOriginalQuality)
	}
	if got := resolveQuality(55); got != 55 {
		t.Errorf("config quality: got %d, want 55", got)
	}
	if got := resolveQuality(250); got != 100 {
		t.Errorf("config quality clamped: got %d, want 100", got)
	}
	optimizeImagesQuality = 42
	if got := resolveQuality(55); got != 42 {
		t.Errorf("flag overrides config: got %d, want 42", got)
	}
	optimizeImagesQuality = 300
	if got := resolveQuality(0); got != 100 {
		t.Errorf("flag clamped: got %d, want 100", got)
	}
}

func TestNewOptimizeImagesCommandRunE(t *testing.T) {
	chdirTemp(t)
	writeConfig(t, `{"projectName":"demo","goModuleName":"demo"}`)
	if err := os.MkdirAll("optimize", 0o755); err != nil {
		t.Fatalf("mkdir optimize: %v", err)
	}
	writePNG(t, "optimize/x.png", 30, 30)

	runE := newOptimizeImagesCommand(gothic_cli.NewCli())
	if err := runE(&cobra.Command{}, nil); err != nil {
		t.Fatalf("optimize-images RunE error: %v", err)
	}
}
