// Package skills serves the bundled Gothic Framework skill documents to AI
// agents working on a Gothic project.
//
// The markdown files under skills/ are embedded in the CLI binary
// (//go:embed) — the baseline set is always available, offline, with no
// filesystem or network access. Each document carries front matter:
//
//	name: <skill name>
//	description: <one-line summary>
//	applies_to: <module constraints, e.g. "core@>=1.0.0,<2.0.0 middlewares@>=1.3.0">
//
// Version resolution keys off the framework module versions the consuming
// project pins in its go.mod require block (see cli.Config.FrameworkModules):
// a skill is part of the resolved set when every applies_to constraint is
// satisfied. Empty applies_to means the skill applies to any version. When NO
// pins are known at all (empty pin matrix), the embedded baseline is served
// in full — nothing can disqualify it.
//
// Discovery (Search / Applicable) is version-gated. Explicit named requests
// (Info / Load) serve the embedded document whenever the name exists — the
// caller asked for it by exact name, so intent wins over the pin filter.
package skills

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"golang.org/x/mod/semver"
)

//go:embed skills
var skillFiles embed.FS

// modulePaths maps the short module names used in applies_to constraints to
// the published module paths. A constraint may use either form; a pin map may
// also use either form.
var modulePaths = map[string]string{
	"core":        "github.com/gothicframework/core",
	"components":  "github.com/gothicframework/components",
	"middlewares": "github.com/gothicframework/middlewares",
	"htmx-go":     "github.com/gothicframework/htmx-go/v4",
}

// shortNames is modulePaths inverted (full module path → short name).
var shortNames = func() map[string]string {
	m := make(map[string]string, len(modulePaths))
	for short, full := range modulePaths {
		m[full] = short
	}
	return m
}()

// Ref is the lightweight descriptor used by discovery (Search/Applicable).
type Ref struct {
	Name        string
	Description string
}

// Info is a Ref plus the raw version contract of the document.
type SkillInfo struct {
	Ref
	AppliesTo string
}

// Document is a fully loaded skill: front matter fields plus the markdown
// body (front matter block stripped).
type Document struct {
	Ref
	AppliesTo string
	// Body is the markdown content without the front matter block.
	Body string
}

// skill is one embedded document, parsed.
type skill struct {
	name        string
	description string
	appliesTo   string

	// constraints are the parsed applies_to entries; parseErr is non-nil when
	// the applies_to string is malformed (the skill is then excluded from
	// version-gated discovery — never a panic at serve time).
	constraints []constraint
	parseErr    error

	body string
}

// embedded holds every parsed document, keyed by name.
var embedded map[string]skill

// allNames is the sorted list of embedded skill names.
var allNames []string

func init() {
	matches, err := fs.Glob(skillFiles, "skills/*.md")
	if err != nil {
		panic(fmt.Sprintf("skills: glob embedded docs: %v", err))
	}
	embedded = make(map[string]skill, len(matches))
	for _, path := range matches {
		data, err := skillFiles.ReadFile(path)
		if err != nil {
			panic(fmt.Sprintf("skills: read embedded %s: %v", path, err))
		}
		s := parseSkill(string(data))
		stem := strings.TrimSuffix(fsPathBase(path), ".md")
		if s.name == "" {
			s.name = stem
		}
		if s.name != stem {
			// Lookup is by front matter name; a mismatch would make the
			// document unreachable by its own filename.
			panic(fmt.Sprintf("skills: %s: front matter name %q must match the file stem %q", path, s.name, stem))
		}
		embedded[s.name] = s
		allNames = append(allNames, s.name)
	}
	sort.Strings(allNames)
}

func fsPathBase(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// parseSkill splits the front matter block and extracts name, description and
// applies_to. Unknown front matter keys are ignored (forward compatible).
func parseSkill(text string) skill {
	var s skill
	text = strings.TrimPrefix(text, "\ufeff")
	if !strings.HasPrefix(text, "---\n") {
		s.parseErr = fmt.Errorf("missing front matter block (must start with `---`)")
		return s
	}
	rest := text[4:]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		s.parseErr = fmt.Errorf("front matter block not closed")
		return s
	}
	s.body = strings.TrimLeft(rest[end+len("\n---\n"):], "\n")

	for _, line := range strings.Split(rest[:end], "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`) {
			value = value[1 : len(value)-1]
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "name":
			s.name = value
		case "description":
			s.description = value
		case "applies_to":
			s.appliesTo = value
		}
	}

	s.constraints, s.parseErr = parseApplies(s.appliesTo)
	return s
}

// constraint is one module version requirement from applies_to.
type constraint struct {
	module  string // as written: short name ("core") or full module path
	op      string // ">=", "<=", ">", "<", "="
	version string // canonical form with a leading "v"
}

// parseApplies parses a constraint string like
// "core@>=1.0.0,<2.0.0 middlewares@>=1.3.0" — whitespace-separated module
// tokens, each "module@spec" where spec is comma-separated "op version" pairs.
func parseApplies(spec string) ([]constraint, error) {
	var out []constraint
	for _, tok := range strings.Fields(spec) {
		module, specPart, ok := strings.Cut(tok, "@")
		if !ok || module == "" {
			return nil, fmt.Errorf("applies_to token %q: want <module>@<constraint>", tok)
		}
		for _, c := range strings.Split(specPart, ",") {
			op, version := "", strings.TrimSpace(c)
			for _, candidate := range []string{">=", "<=", ">", "<", "="} {
				if strings.HasPrefix(version, candidate) {
					op, version = candidate, strings.TrimPrefix(version, candidate)
					break
				}
			}
			if op == "" {
				return nil, fmt.Errorf("applies_to token %q: constraint %q must start with an operator (>=, <=, >, <, =)", tok, c)
			}
			normalized := version
			if !strings.HasPrefix(normalized, "v") {
				normalized = "v" + normalized
			}
			if !semver.IsValid(normalized) {
				return nil, fmt.Errorf("applies_to token %q: %q is not a valid semver", tok, version)
			}
			out = append(out, constraint{module: module, op: op, version: normalized})
		}
	}
	return out, nil
}

// matches reports whether a module version pin satisfies the constraint.
// pin must already be a valid semver string (with a "v" prefix).
func (c constraint) matches(pin string) bool {
	cmp := semver.Compare(pin, c.version)
	switch c.op {
	case ">=":
		return cmp >= 0
	case "<=":
		return cmp <= 0
	case ">":
		return cmp > 0
	case "<":
		return cmp < 0
	case "=":
		return cmp == 0
	}
	return false
}

// Set is a view of the embedded skill set resolved against a pin matrix.
// The zero-value pin matrix (empty map / nil) selects the full embedded
// baseline — with no versions observed, nothing can be disqualified.
type Set struct {
	pins map[string]string
}

// New builds a Set resolved against a pin matrix of framework module
// versions. Keys may be the short module name ("core", "components",
// "middlewares", "htmx-go") or the full module path
// ("github.com/gothicframework/core");
// values are versions like "v1.6.0" (a leading "v" is optional). The pins
// should come from the consuming project's go.mod require block
// (cli.Config.FrameworkModules), optionally merged with the CLI's own
// FrameworkModules baseline where a project omits a pin.
func New(pins map[string]string) *Set {
	return &Set{pins: pins}
}

// pinFor resolves a module reference (short name or full path) to a valid
// semver version from the pin matrix.
func (s *Set) pinFor(module string) (string, bool) {
	lookup := []string{module}
	if full, ok := modulePaths[module]; ok {
		lookup = append(lookup, full)
	}
	if short, ok := shortNames[module]; ok {
		lookup = append(lookup, short)
	}
	for _, key := range lookup {
		if v, ok := s.pins[key]; ok {
			v = strings.TrimSpace(v)
			if !strings.HasPrefix(v, "v") {
				v = "v" + v
			}
			if !semver.IsValid(v) {
				return "", false // invalid pin version cannot satisfy any constraint
			}
			return v, true
		}
	}
	return "", false
}

// applicable reports whether a skill belongs to the resolved set for these
// pins: a parse error excludes it; no constraints means always applicable;
// an empty pin matrix is the baseline — nothing observed, nothing disqualifiable,
// so every embedded skill applies; otherwise every constraint must be
// satisfiable by a pinned version.
func (s *Set) applicable(sk skill) bool {
	if sk.parseErr != nil {
		return false
	}
	if len(sk.constraints) == 0 || len(s.pins) == 0 {
		return true
	}
	for _, c := range sk.constraints {
		pin, ok := s.pinFor(c.module)
		if !ok || !c.matches(pin) {
			return false
		}
	}
	return true
}

// Applicable returns the Refs of the resolved skill set for these pins,
// sorted by name.
func (s *Set) Applicable() []Ref {
	var out []Ref
	for _, name := range allNames {
		if sk, ok := embedded[name]; ok && s.applicable(sk) {
			out = append(out, Ref{Name: sk.name, Description: sk.description})
		}
	}
	return out
}

// Search returns the applicable skills whose name or description contains the
// query (case-insensitive substring). An empty query returns the whole
// applicable set.
func (s *Set) Search(query string) []Ref {
	applicable := s.Applicable()
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return applicable
	}
	var out []Ref
	for _, r := range applicable {
		if strings.Contains(strings.ToLower(r.Name), q) ||
			strings.Contains(strings.ToLower(r.Description), q) {
			out = append(out, r)
		}
	}
	return out
}

// Info returns the metadata (front matter) of the named skills, preserving the
// requested order. Unknown names are omitted. Named requests are NOT
// version-gated: an agent that names a skill gets its Info regardless of pins.
func (s *Set) Info(names []string) []SkillInfo {
	var out []SkillInfo
	for _, name := range names {
		if sk, ok := embedded[name]; ok {
			out = append(out, SkillInfo{
				Ref:       Ref{Name: sk.name, Description: sk.description},
				AppliesTo: sk.appliesTo,
			})
		}
	}
	return out
}

// Load returns the full documents for the named skills, preserving the
// requested order. Unknown names are omitted. Named requests are NOT
// version-gated: an agent that names a skill gets the document regardless of
// pins. An empty names slice loads every embedded document.
func (s *Set) Load(names []string) []Document {
	load := names
	if len(load) == 0 {
		load = allNames
	}
	var out []Document
	for _, name := range load {
		if sk, ok := embedded[name]; ok {
			out = append(out, Document{
				Ref:       Ref{Name: sk.name, Description: sk.description},
				AppliesTo: sk.appliesTo,
				Body:      sk.body,
			})
		}
	}
	return out
}

// Names lists every embedded skill name (the baseline set), sorted.
func (s *Set) Names() []string {
	return append([]string(nil), allNames...)
}

// defaultSet resolves with no pins: the full embedded baseline.
var defaultSet = New(nil)

// Search runs Search on the baseline skill set (all embedded skills).
func Search(query string) []Ref { return defaultSet.Search(query) }

// Info runs Info on the baseline skill set.
func Info(names []string) []SkillInfo { return defaultSet.Info(names) }

// Load runs Load on the baseline skill set.
func Load(names []string) []Document { return defaultSet.Load(names) }
