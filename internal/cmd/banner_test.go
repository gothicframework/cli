package cmd

import (
	"strings"
	"testing"

	"github.com/gothicframework/cli/v3/internal/termcolor"
)

func TestBannerHasNoEscapesWithoutColour(t *testing.T) {
	orig := termcolor.Enabled
	termcolor.Enabled = false
	defer func() { termcolor.Enabled = orig }()

	got := banner()
	if strings.Contains(got, "\x1b[") {
		t.Errorf("a redirected banner must be plain text: %q", got)
	}
	if !strings.Contains(got, "Gothic App is up and running!") {
		t.Error("the status text must survive")
	}
}

func TestBannerCarriesTheBrandRamp(t *testing.T) {
	orig := termcolor.Enabled
	termcolor.Enabled = true
	defer func() { termcolor.Enabled = orig }()

	got := banner()
	for i, stop := range brandRamp {
		if !strings.Contains(got, stop) {
			t.Errorf("ramp stop %d (%q) missing from the banner", i, stop)
		}
	}
}

func TestBannerBoltIsNotAnEmoji(t *testing.T) {
	orig := termcolor.Enabled
	termcolor.Enabled = true
	defer func() { termcolor.Enabled = orig }()

	// An emoji bolt would ignore the escape and stay yellow, which is the whole
	// reason a text glyph is used here.
	if strings.Contains(banner(), "⚡") {
		t.Error("the bolt must not be the emoji codepoint")
	}
	if !strings.Contains(banner(), "↯") {
		t.Error("the bolt should be the colourable text glyph")
	}
}
