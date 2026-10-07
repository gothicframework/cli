package browser

import (
	"strings"
	"testing"
)

// lookupKey happy-path table: every name the Act "key" action documents.
func TestLookupKeyNamed(t *testing.T) {
	cases := []string{
		"Enter", "Escape", "Tab", "Backspace", "Delete", "Space",
		"ArrowUp", "ArrowDown", "ArrowLeft", "ArrowRight",
		"Home", "End", "PageUp", "PageDown",
	}
	for _, name := range cases {
		k, err := lookupKey(name)
		if err != nil {
			t.Errorf("lookupKey(%q) = %v, want a key", name, err)
		}
		if k == 0 {
			t.Errorf("lookupKey(%q) returned the zero key", name)
		}
	}
}

func TestLookupKeySingleChar(t *testing.T) {
	k, err := lookupKey("a")
	if err != nil {
		t.Fatalf("lookupKey(\"a\"): %v", err)
	}
	if k == 0 {
		t.Fatal("lookupKey(\"a\") returned the zero key")
	}
}

func TestLookupKeyRejectsMultiCharJunk(t *testing.T) {
	for _, name := range []string{"FOO", "enter", "Ctrl+X"} {
		if _, err := lookupKey(name); err == nil {
			t.Errorf("lookupKey(%q) = nil error, want unsupported", name)
		}
	}
}

func TestSplitCSV(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"a,b", []string{"a", "b"}},
		{" a , b ", []string{"a", "b"}},
		{"a,,b", []string{"a", "b"}},
	}
	for _, c := range cases {
		got := splitCSV(c.in)
		if len(got) != len(c.want) {
			t.Errorf("splitCSV(%q) = %v, want %v", c.in, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("splitCSV(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

// scrollLocked JS-building variants are exercised through the page only with
// a browser; here verify the parse logic through the error path with no
// browser running, which fails before any scroll attempt for junk input.
func TestScrollLockedRejectsJunkWithoutLaunching(t *testing.T) {
	m := testManager(t, NewOptions())
	t.Cleanup(m.Close)
	// Junk values parse-fail before any page access; the browser stays down.
	err := m.scrollLocked(nil, "not-a-number")
	if err == nil || !strings.Contains(err.Error(), "scroll value") {
		t.Fatalf("scrollLocked junk = %v, want a scroll-value error", err)
	}
}
