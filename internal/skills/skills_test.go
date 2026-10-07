package skills

import (
	"strings"
	"testing"
)

// fullPins is the pin matrix of a current project: the framework libraries at
// the versions the CLI scaffolds (cli/internal/cmd/version.go FrameworkModules).
var fullPins = map[string]string{
	"github.com/gothicframework/core":        "v1.6.0",
	"github.com/gothicframework/components":  "v1.3.0",
	"github.com/gothicframework/middlewares": "v1.3.0",
}

func TestEmbeddedDocsParseCleanly(t *testing.T) {
	if len(allNames) == 0 {
		t.Fatal("no embedded skills found")
	}
	for _, name := range allNames {
		sk, ok := embedded[name]
		if !ok {
			t.Fatalf("name %q in allNames but not in embedded map", name)
		}
		if sk.parseErr != nil {
			t.Errorf("skill %q: front matter/contract parse error: %v", name, sk.parseErr)
		}
		if sk.description == "" {
			t.Errorf("skill %q: empty description", name)
		}
		if sk.body == "" {
			t.Errorf("skill %q: empty body", name)
		}
		if strings.Contains(sk.body, "---") && strings.HasPrefix(sk.body, "---") {
			t.Errorf("skill %q: body still contains the front matter block", name)
		}
	}
}

// TestLoadAllBaseline loads every embedded document by name and verifies the
// shape of each returned Document.
func TestLoadAllBaseline(t *testing.T) {
	docs := Load(nil)
	if len(docs) != len(allNames) {
		t.Fatalf("Load(nil) returned %d docs, want %d", len(docs), len(allNames))
	}
	for _, d := range docs {
		if d.Name == "" || d.Description == "" || d.Body == "" {
			t.Errorf("document %q missing fields: %+v", d.Name, d)
		}
		if strings.HasPrefix(d.Body, "---") {
			t.Errorf("document %q body starts with front matter", d.Name)
		}
		// CAN/CANNOT sections are part of the skill document contract.
		if !strings.Contains(d.Body, "What the agent CAN do") ||
			!strings.Contains(d.Body, "What the agent CANNOT do") {
			t.Errorf("document %q missing the CAN/CANNOT sections", d.Name)
		}
	}
}

// TestLoadAndInfoUnknownNames skipped — unknown names are omitted, in order.
func TestLoadAndInfoUnknownNames(t *testing.T) {
	docs := Load([]string{"gothic-routing", "nope", "gothic-invariants"})
	if len(docs) != 2 || docs[0].Name != "gothic-routing" || docs[1].Name != "gothic-invariants" {
		t.Fatalf("Load with an unknown name = %+v, want the two known docs in order", docs)
	}
	infos := Info([]string{"nope"})
	if len(infos) != 0 {
		t.Errorf("Info with only unknown names = %+v, want empty", infos)
	}
}

func TestInfoReturnsContract(t *testing.T) {
	infos := Info([]string{"gothic-routing"})
	if len(infos) != 1 {
		t.Fatalf("Info returned %d entries, want 1", len(infos))
	}
	info := infos[0]
	if info.Name != "gothic-routing" || info.Description == "" {
		t.Errorf("Info fields wrong: %+v", info)
	}
	if !strings.Contains(info.AppliesTo, "core@") {
		t.Errorf("AppliesTo = %q, want a core@ constraint", info.AppliesTo)
	}
}

// TestPinMatrixSelection is the resolver's table-driven core: a pin matrix
// selects exactly the skills whose applies_to is satisfied.
func TestPinMatrixSelection(t *testing.T) {
	want := []string{
		"gothic-config-and-env",
		"gothic-deploy-aws",
		"gothic-htmx",
		"gothic-invariants",
		"gothic-page-essentials",
		"gothic-routing",
		"gothic-typed-fetch-json",
		"gothic-wasm-state-topics",
	}
	names := func(refs []Ref) []string {
		var out []string
		for _, r := range refs {
			out = append(out, r.Name)
		}
		return out
	}

	tests := []struct {
		name string
		pins map[string]string
		want []string
	}{
		{
			name: "baseline: no pins observed → full embedded set",
			pins: nil,
			want: want,
		},
		{
			name: "current pins → full embedded set",
			pins: fullPins,
			want: want,
		},
		{
			name: "short-name pin keys resolve like full paths",
			pins: map[string]string{
				"core":        "v1.6.0",
				"components":  "v1.3.0",
				"middlewares": "1.3.0", // leading "v" optional
			},
			want: want,
		},
		{
			name: "core major beyond the constraint excludes every gated skill",
			pins: map[string]string{
				"github.com/gothicframework/core":        "v2.0.0",
				"github.com/gothicframework/components":  "v1.3.0",
				"github.com/gothicframework/middlewares": "v1.3.0",
			},
			// gothic-invariants declares no constraints, so it always applies.
			want: []string{"gothic-htmx", "gothic-invariants"},
		},
		{
			name: "core below the minimum excludes every gated skill",
			pins: map[string]string{
				"github.com/gothicframework/core":        "v0.9.0",
				"github.com/gothicframework/components":  "v1.3.0",
				"github.com/gothicframework/middlewares": "v1.3.0",
			},
			want: []string{"gothic-htmx", "gothic-invariants"},
		},
		{
			name: "missing core pin excludes every skill constraining core",
			pins: map[string]string{
				"github.com/gothicframework/components": "v1.3.0",
			},
			want: []string{"gothic-htmx", "gothic-invariants"},
		},
		{
			name: "missing middlewares pin excludes only the skills that constrain it",
			pins: map[string]string{
				"github.com/gothicframework/core":       "v1.6.0",
				"github.com/gothicframework/components": "v1.3.0",
			},
			want: []string{
				"gothic-htmx",
				"gothic-invariants",
				"gothic-page-essentials",
				"gothic-routing",
				"gothic-typed-fetch-json",
				"gothic-wasm-state-topics",
			},
		},
		{
			name: "invalid pin version satisfies no constraint",
			pins: map[string]string{
				"github.com/gothicframework/core":        "not-a-version",
				"github.com/gothicframework/components":  "v1.3.0",
				"github.com/gothicframework/middlewares": "v1.3.0",
			},
			want: []string{"gothic-htmx", "gothic-invariants"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := names(New(tt.pins).Applicable())
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("Applicable() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSearchFiltersApplicableSet(t *testing.T) {
	set := New(fullPins)

	// Empty query → the whole applicable set.
	if got := len(set.Search("   ")); got != len(allNames) {
		t.Errorf("Search(\"\") = %d results, want %d", got, len(allNames))
	}

	// Case-insensitive substring over name + description.
	got := Search("TOPICS")
	if len(got) == 0 || got[0].Name != "gothic-wasm-state-topics" {
		t.Errorf("Search(\"TOPICS\") = %+v, want gothic-wasm-state-topics first", got)
	}

	// Discovery is version-gated: a v2 core leaves only the unconstrained skills.
	gated := New(map[string]string{"core": "v2.0.0"})
	if got := gated.Search(""); len(got) != 2 || got[0].Name != "gothic-htmx" || got[1].Name != "gothic-invariants" {
		t.Errorf("v2-core Search(\"\") = %+v, want only the unconstrained skills", got)
	}
	if got := gated.Search("routing"); len(got) != 0 {
		t.Errorf("v2-core Search(\"routing\") = %+v, want empty", got)
	}

	if got := Search("no-such-thing"); len(got) != 0 {
		t.Errorf("Search(\"no-such-thing\") = %+v, want empty", got)
	}
}

// TestNamesIsBaselineUnfiltered — Names always lists the full embedded set,
// even for a pin matrix that excludes some skills from discovery.
func TestNamesIsBaselineUnfiltered(t *testing.T) {
	gated := New(map[string]string{"core": "v2.0.0"})
	if got := gated.Names(); len(got) != len(allNames) {
		t.Errorf("Names() = %d, want the full embedded set (%d)", len(got), len(allNames))
	}
}

func TestParseAppliesErrors(t *testing.T) {
	tests := []struct {
		spec string
		ok   bool
	}{
		{"", true}, // no constraints = always applicable
		{"core@>=1.0.0,<2.0.0", true},
		{"github.com/gothicframework/core@>=1.0.0", true},
		{"core", false},          // missing @
		{"core@1.0.0", false},    // missing operator
		{"core@>=banana", false}, // invalid semver
		{"core@>=", false},       // missing version
	}
	for _, tt := range tests {
		_, err := parseApplies(tt.spec)
		if (err == nil) != tt.ok {
			t.Errorf("parseApplies(%q) err = %v, want ok=%v", tt.spec, err, tt.ok)
		}
	}
}

func TestConstraintOps(t *testing.T) {
	tests := []struct {
		spec string // "op version" pair inside a core@ token
		pin  string
		want bool
	}{
		{">=1.0.0", "v1.6.0", true},
		{">=1.0.0", "v0.9.0", false},
		{"<2.0.0", "v1.6.0", true},
		{"<2.0.0", "v2.0.0", false},
		{"<=1.6.0", "v1.6.0", true},
		{">1.6.0", "v1.6.0", false},
		{"=1.6.0", "v1.6.0", true},
		{">=1.0.0", "v2.0.0", true},
	}
	for _, tt := range tests {
		constraints, err := parseApplies("core@" + tt.spec)
		if err != nil {
			t.Errorf("parseApplies(core@%q): %v", tt.spec, err)
			continue
		}
		if got := constraints[0].matches(tt.pin); got != tt.want {
			t.Errorf("constraint %q vs pin %q = %v, want %v", tt.spec, tt.pin, got, tt.want)
		}
	}
}
