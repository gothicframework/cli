package mcp

// tools_skills.go — the skill tools: skillsearch, skillinfo, skill.
// Progressive disclosure: search and info are metadata-sized; skill is the
// one request that carries full document bodies.

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ── skillsearch ────────────────────────────────────────────────────────────

type skillSearchIn struct {
	Query string `json:"query" jsonschema:"case-insensitive substring matched against skill names and descriptions; empty = the whole applicable set"`
}

type skillRefOut struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type skillSearchOut struct {
	OK   bool          `json:"ok"`
	Hits []skillRefOut `json:"hits"`
}

func (s *surface) skillSearchTool(_ context.Context, _ *mcp.CallToolRequest, in skillSearchIn) (*mcp.CallToolResult, skillSearchOut, error) {
	c, err := s.currentCap()
	if err != nil {
		return nil, skillSearchOut{}, err
	}
	sk, err := c.needSkills()
	if err != nil {
		return nil, skillSearchOut{}, err
	}
	refs := sk.Search(in.Query)
	out := skillSearchOut{OK: true}
	for _, r := range refs {
		out.Hits = append(out.Hits, skillRefOut{Name: r.Name, Description: r.Description})
	}
	return nil, out, nil
}

// ── skillinfo ──────────────────────────────────────────────────────────────

type skillInfoIn struct {
	Names []string `json:"names" jsonschema:"the skill names to describe"`
}

type skillInfoOut struct {
	OK     bool            `json:"ok"`
	Skills []skillInfoItem `json:"skills"`
}

type skillInfoItem struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	AppliesTo   string `json:"applies_to,omitempty"`
}

func (s *surface) skillInfoTool(_ context.Context, _ *mcp.CallToolRequest, in skillInfoIn) (*mcp.CallToolResult, skillInfoOut, error) {
	c, err := s.currentCap()
	if err != nil {
		return nil, skillInfoOut{}, err
	}
	sk, err := c.needSkills()
	if err != nil {
		return nil, skillInfoOut{}, err
	}
	if len(in.Names) == 0 {
		return nil, skillInfoOut{}, fmt.Errorf("names is required (at least one skill name)")
	}
	infos := sk.Info(in.Names)
	if len(infos) == 0 {
		return nil, skillInfoOut{}, fmt.Errorf("none of the requested skills exist (known: %s)", strings.Join(sk.Names(), ", "))
	}
	out := skillInfoOut{OK: true}
	for _, i := range infos {
		out.Skills = append(out.Skills, skillInfoItem{
			Name: i.Name, Description: i.Description, AppliesTo: i.AppliesTo,
		})
	}
	return nil, out, nil
}

// ── skill ──────────────────────────────────────────────────────────────────

type skillIn struct {
	Names []string `json:"names,omitempty" jsonschema:"the skill names to load (empty = only the name list; full documents only on this explicit request)"`
}

type skillDocOut struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	AppliesTo   string `json:"applies_to,omitempty"`
	Body        string `json:"body,omitempty"`
}

type skillOut struct {
	OK    bool          `json:"ok"`
	Names []string      `json:"names,omitempty" jsonschema:"every embedded skill name (when no names were requested)"`
	Docs  []skillDocOut `json:"docs,omitempty"`
}

func (s *surface) skillTool(_ context.Context, _ *mcp.CallToolRequest, in skillIn) (*mcp.CallToolResult, skillOut, error) {
	c, err := s.currentCap()
	if err != nil {
		return nil, skillOut{}, err
	}
	sk, err := c.needSkills()
	if err != nil {
		return nil, skillOut{}, err
	}
	// No names: the light form — the name list only.
	if len(in.Names) == 0 {
		return nil, skillOut{OK: true, Names: sk.Names()}, nil
	}
	for _, name := range in.Names {
		if !namedDoc(sk, name) {
			return nil, skillOut{}, fmt.Errorf("unknown skill %q (known: %s)", name, strings.Join(sk.Names(), ", "))
		}
	}
	docs := sk.Load(in.Names)
	out := skillOut{OK: true}
	for _, d := range docs {
		out.Docs = append(out.Docs, skillDocOut{
			Name: d.Name, Description: d.Description, AppliesTo: d.AppliesTo, Body: d.Body,
		})
	}
	return nil, out, nil
}

// namedDoc reports whether the skill set knows the name.
func namedDoc(sk SkillsPort, name string) bool {
	for _, i := range sk.Info([]string{name}) {
		if i.Name == name {
			return true
		}
	}
	return false
}
