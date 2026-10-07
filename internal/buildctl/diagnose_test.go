package buildctl

import (
	"strings"
	"testing"
)

// TestDiagnoseTranslatesTheTaxonomy is table-driven over the six taxonomy
// entries: each raw error signature must translate to a diagnosis + fix +
// skill, and must not be mistaken for another entry.
func TestDiagnoseTranslatesTheTaxonomy(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantSub []string // substrings the diagnosis must carry
		wantFix string   // substring the fix must carry
		wantSk  string
	}{
		{
			name:    "templ text node starting with for",
			raw:     "src/pages/counter.templ:17:3: syntax error: unexpected for, expected element or expression",
			wantSub: []string{"text node", "for"},
			wantFix: "{ \"for …\" }",
			wantSk:  "gothic-routing",
		},
		{
			name:    "go:embed directive inside templ text",
			raw:     "src/pages/index_templ.go:42:1: misplaced go:embed directive",
			wantSub: []string{"//go:embed", "templ"},
			wantFix: "plain .go file",
			wantSk:  "gothic-invariants",
		},
		{
			name:    "net/http dragged into TinyGo by Decode DTO",
			raw:     "wasm: build counter (embedded tinygo): error: package net/http is not supported by TinyGo",
			wantSub: []string{"net/http", "TinyGo"},
			wantFix: "src/shared",
			wantSk:  "gothic-typed-fetch-json",
		},
		{
			name:    "undefined topic accessor (stub ordering)",
			raw:     "./.gen-123/main.go:23:5: undefined: AppTopic",
			wantSub: []string{"topic_gen.go"},
			wantFix: "PregenerateTopicStubs",
			wantSk:  "gothic-wasm-state-topics",
		},
		{
			name:    "local import resolution fails",
			raw:     "wasm: load packages: no required module provides package demo/src/shared; to add it: go get demo/src/shared",
			wantSub: []string{"replace", "go mod tidy"},
			wantSk:  "gothic-invariants",
		},
		{
			name:    "package-level var in ClientSideState",
			raw:     "src/pages/counter_templ.go:12:2: ClientSideState references package-level var \"step\" — only func, const, and type declarations can be tree-shaken into WASM; move \"step\" inside the ClientSideState body",
			wantSub: []string{"package-level", "tree-shaken"},
			wantFix: "const",
			wantSk:  "gothic-wasm-state-topics",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diags := DiagnoseAll(tt.raw)
			if len(diags) == 0 {
				t.Fatal("DiagnoseAll returned no diagnostics for a non-empty raw text")
			}
			got := diags[0]
			if got.Raw != tt.raw {
				t.Errorf("Raw = %q, want the verbatim raw text %q", got.Raw, tt.raw)
			}
			for _, sub := range tt.wantSub {
				if !strings.Contains(got.Diagnosis, sub) {
					t.Errorf("Diagnosis %q missing %q", got.Diagnosis, sub)
				}
			}
			if !strings.Contains(got.Fix, tt.wantFix) {
				t.Errorf("Fix %q missing %q", got.Fix, tt.wantFix)
			}
			if tt.wantSk != "" && got.Skill != tt.wantSk {
				t.Errorf("Skill = %q, want %q", got.Skill, tt.wantSk)
			}
		})
	}
}

// TestDiagnoseDoesNotCrossTranslate pins that a raw text from one taxonomy
// area is not diagnosed as another (order and anchors of the table).
func TestDiagnoseDoesNotCrossTranslate(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		notSkill string // a diagnosis area this raw must NOT map to
	}{
		{
			name:     "templ parse error is not a module problem",
			raw:      "src/pages/home.templ:5:1: syntax error: unexpected EOF",
			notSkill: "gothic-invariants",
		},
		{
			name:     "missing module is not a templ problem",
			raw:      "no required module provides package github.com/you/app/v3/src/shared",
			notSkill: "gothic-routing",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, d := range DiagnoseAll(tt.raw) {
				if d.Skill == tt.notSkill {
					t.Errorf("raw translated into the wrong area: skill %q, diagnosis %q", d.Skill, d.Diagnosis)
				}
			}
		})
	}
}

// TestDiagnosePreservesUnrecognizedRaw pins the fail-open behaviour: raw text
// no entry matches is returned untranslated, verbatim.
func TestDiagnosePreservesUnrecognizedRaw(t *testing.T) {
	raw := "some totally unknown tool failure"
	diags := DiagnoseAll(raw)
	if len(diags) != 1 || diags[0].Raw != raw {
		t.Errorf("expected the verbatim raw text back, got %+v", diags)
	}
	if diags[0].Diagnosis != "" {
		t.Errorf("an unrecognized raw text must not invent a diagnosis, got %q", diags[0].Diagnosis)
	}
	if got := DiagnoseAll("   \n  "); got != nil {
		t.Errorf("DiagnoseAll(whitespace) = %+v, want nil", got)
	}
}
