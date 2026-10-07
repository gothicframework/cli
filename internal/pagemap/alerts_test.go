package pagemap

import (
	"image"
	"strings"
	"testing"
)

func encodePayload(t *testing.T, img *image.RGBA, m *Manifest) string {
	t.Helper()
	return Encode(img, m, Options{URL: "http://localhost:3000/docs/routing", Engine: "chromium 131"})
}

// TestCoverageAlertFiresOnNeverPaintedAccent: the CSS declares a purple
// background on a visible button, the raster renders grey — the accent never
// painted anywhere, and the payload must say so in the button's words.
func TestCoverageAlertFiresOnNeverPaintedAccent(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 320, 160))
	fillRect(img, img.Bounds(), Color{0x0f, 0x0f, 0x12, 0xff})
	fillRect(img, image.Rect(40, 100, 136, 132), Color{0x3f, 0x3f, 0x46, 0xff})
	m := &Manifest{Rects: []Element{
		{Selector: "body", X: 0, Y: 0, W: 320, H: 160, BG: "#0f0f12"},
		{Selector: "button.cta", X: 40, Y: 100, W: 96, H: 32, BG: "#7c3aed", FG: "#e4e4e7"},
	}}

	payload := encodePayload(t, img, m)
	if !strings.Contains(payload, "#7c3aed is in the loaded CSS but occupies 0 cells in the render") {
		t.Errorf("palette-coverage alert missing:\n%s", payload)
	}
	if !strings.Contains(payload, "declared as the background of button.cta") {
		t.Errorf("coverage alert must attribute the declared colour:\n%s", payload)
	}
	if !strings.Contains(payload, "button.cta renders #3f3f46, not the declared #7c3aed") {
		t.Errorf("style-mismatch alert missing:\n%s", payload)
	}
}

// TestCoverageAlertSilentWhenPainted: the same accent rendered verbatim must
// produce no palette-coverage and no style-mismatch alert.
func TestCoverageAlertSilentWhenPainted(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 320, 160))
	fillRect(img, img.Bounds(), Color{0x0f, 0x0f, 0x12, 0xff})
	fillRect(img, image.Rect(40, 100, 136, 132), Color{0x7c, 0x3a, 0xed, 0xff})
	m := &Manifest{Rects: []Element{
		{Selector: "body", X: 0, Y: 0, W: 320, H: 160, BG: "#0f0f12"},
		{Selector: "button.cta", X: 40, Y: 100, W: 96, H: 32, BG: "#7c3aed", FG: "#e4e4e7"},
	}}

	payload := encodePayload(t, img, m)
	if strings.Contains(payload, "ALERTS") && !strings.Contains(payload, "(none)") {
		t.Errorf("a clean page must report no alerts:\n%s", payload)
	}
}

// TestEdgeBleedAlertFires: a child painted past the right edge must fire both
// the pixel-level bleed alert (with the row span) and the overflow check that
// names the culprit.
func TestEdgeBleedAlertFires(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 320, 160))
	fillRect(img, img.Bounds(), Color{0x0f, 0x0f, 0x12, 0xff})
	fillRect(img, image.Rect(16, 40, 240, 120), Color{0x1a, 0x1a, 0x20, 0xff})
	fillRect(img, image.Rect(200, 48, 320, 64), Color{0xe4, 0xe4, 0xe7, 0xff})
	m := &Manifest{Rects: []Element{
		{Selector: "body", X: 0, Y: 0, W: 320, H: 160, BG: "#0f0f12"},
		{Selector: "div.card", X: 16, Y: 40, W: 224, H: 80, BG: "#1a1a20"},
		{Selector: "span.piece", X: 200, Y: 48, W: 200, H: 16, BG: "#e4e4e7", Overflows: true},
	}}

	payload := encodePayload(t, img, m)
	if !strings.Contains(payload, "content pixels reach the right edge and continue") {
		t.Errorf("edge-bleed alert missing:\n%s", payload)
	}
	if !strings.Contains(payload, "horizontal overflow in span.piece") {
		t.Errorf("overflow alert missing:\n%s", payload)
	}
	if !strings.Contains(payload, "span.piece extends 80px past the right edge") {
		t.Errorf("overflow alert must quantify the overhang:\n%s", payload)
	}
}

// TestEdgeBleedSilentForFullWidthElements: a full-width header legitimately
// touches both edges; with no element crossing the viewport the raster-level
// bleed check must stay silent instead of crying wolf.
func TestEdgeBleedSilentForFullWidthElements(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 320, 160))
	fillRect(img, img.Bounds(), Color{0x0f, 0x0f, 0x12, 0xff})
	fillRect(img, image.Rect(0, 0, 320, 24), Color{0x1a, 0x1a, 0x20, 0xff}) // full-width header
	m := &Manifest{Rects: []Element{
		{Selector: "body", X: 0, Y: 0, W: 320, H: 160, BG: "#0f0f12"},
		{Selector: "header.site-header", X: 0, Y: 0, W: 320, H: 24, BG: "#1a1a20"},
	}}

	payload := encodePayload(t, img, m)
	if strings.Contains(payload, "reach the right edge") {
		t.Errorf("full-width header must not read as edge bleed:\n%s", payload)
	}
	if !strings.Contains(payload, "(none)") {
		t.Errorf("this page has no alerts to report:\n%s", payload)
	}
}
