package tofu

import "testing"

func TestResourceSuffixDeterministic(t *testing.T) {
	a := ResourceSuffix("example.com/demo", "demo")
	b := ResourceSuffix("example.com/demo", "demo")
	if a != b {
		t.Errorf("ResourceSuffix not deterministic: %q vs %q", a, b)
	}
	if len(a) != 8 {
		t.Errorf("suffix length = %d, want 8", len(a))
	}
}

func TestResourceSuffixVariesByInput(t *testing.T) {
	if ResourceSuffix("example.com/a", "x") == ResourceSuffix("example.com/b", "x") {
		t.Error("different module names should produce different suffixes")
	}
	if ResourceSuffix("m", "a") == ResourceSuffix("m", "b") {
		t.Error("different project names should produce different suffixes")
	}
	// The separator ":" must prevent collisions between concatenation boundaries.
	if ResourceSuffix("ab", "c") == ResourceSuffix("a", "bc") {
		t.Error("separator should disambiguate boundary collisions")
	}
}

func TestResourceSuffixIsHex(t *testing.T) {
	s := ResourceSuffix("example.com/demo", "demo")
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("suffix %q contains non-hex char %q", s, c)
		}
	}
}
