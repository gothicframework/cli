package buildtools

import "testing"

func TestIsTailwindNoise(t *testing.T) {
	cases := []struct {
		line string
		drop bool
	}{
		{"Rebuilding...", true},
		{"Done in 96ms.", true},
		{"Done in 2187ms.", true},
		{"Done in 1.4s", true},
		{"", true},
		{"   ", true},
		// Anything that carries information must survive.
		{"Browserslist: caniuse-lite is outdated. Please run:", false},
		{"error: cannot read src/css/app.css", false},
		{"Done in the middle of a sentence", false},
		{"warn - No utility classes were detected", false},
	}
	for _, c := range cases {
		if got := isTailwindNoise(c.line); got != c.drop {
			t.Errorf("isTailwindNoise(%q) = %v, want %v", c.line, got, c.drop)
		}
	}
}
