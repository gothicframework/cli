package buildtools

import "testing"

func TestIsTemplNoise(t *testing.T) {
	cases := []struct {
		line string
		drop bool
	}{
		{"(✓) Post-generation event received, processing... [ updates=1 ]", true},
		// The real wire format: the generator colours its level icon, so the
		// line starts with an escape sequence, not with "(".
		{"\x1b[32m(✓)\x1b[0m Post-generation event received, processing... [ updates=1 ]", true},
		{"\x1b[32m(✓)\x1b[0m Complete [ updates=1 duration=26.009704ms ]", true},
		{"(✓) Complete [ updates=1 duration=18.686593ms ]", true},
		{"", true},
		{"   ", true},
		// Diagnostics must survive: dropping one would hide a broken template.
		{"(✗) Error generating code: src/pages/index.templ:12:3: unexpected token", false},
		{"src/pages/index.templ:12:3: expected expression", false},
		{"Complete without the check mark prefix", false},
		{"warning: something happened", false},
	}
	for _, c := range cases {
		if got := isTemplNoise(c.line); got != c.drop {
			t.Errorf("isTemplNoise(%q) = %v, want %v", c.line, got, c.drop)
		}
	}
}
