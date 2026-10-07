package mcp

// resources.go — the bundled skill documents as MCP resources.
//
// Each known skill is registered as a fixed resource (so resources/list
// shows it with its description), plus one URI template
// (gothic-skill://{name}) through which any embedded skill name can be read —
// unknown names answer with the SDK's resource-not-found error.

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// skillScheme is the resource URI scheme of the bundled skill documents.
const skillScheme = "gothic-skill"

// skillResourceURI renders one skill's resource URI.
func skillResourceURI(name string) string { return skillScheme + "://" + name }

// registerResources registers the skill documents as MCP resources.
func (s *surface) registerResources(srv *mcp.Server) {
	read := func(uri string) (*mcp.ReadResourceResult, error) {
		name := strings.TrimPrefix(uri, skillScheme+"://")
		if name == "" || strings.ContainsAny(name, "?#") {
			return nil, mcp.ResourceNotFoundError(uri)
		}
		c, err := s.currentCap()
		if err != nil {
			return nil, err
		}
		sk, err := c.needSkills()
		if err != nil {
			return nil, err
		}
		docs := sk.Load([]string{name})
		if len(docs) == 0 {
			return nil, mcp.ResourceNotFoundError(uri)
		}
		d := docs[0]
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{{
				URI:      uri,
				MIMEType: "text/markdown",
				Text:     d.Body,
			}},
		}, nil
	}

	// The fixed resources: every embedded skill name at registration time.
	srv.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: skillScheme + "://{name}",
		Name:        "skill",
		Description: "A bundled Gothic Framework skill document (markdown). The name is the skill's front-matter name.",
		MIMEType:    "text/markdown",
	}, func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return read(req.Params.URI)
	})

	for _, name := range s.embeddedSkillNames() {
		n := name // capture
		srv.AddResource(&mcp.Resource{
			URI:         skillResourceURI(n),
			Name:        n,
			Description: skillDescription(s, n),
			MIMEType:    "text/markdown",
		}, func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return read(req.Params.URI)
		})
	}
}

// embeddedSkillNames lists the skill names through the capability (nil-safe).
func (s *surface) embeddedSkillNames() []string {
	c, err := s.currentCap()
	if err != nil || c.Skills == nil {
		return nil
	}
	return c.Skills.Names()
}

// skillDescription returns one skill's description (nil-safe).
func skillDescription(s *surface, name string) string {
	c, err := s.currentCap()
	if err != nil || c.Skills == nil {
		return ""
	}
	infos := c.Skills.Info([]string{name})
	if len(infos) == 0 {
		return fmt.Sprintf("Gothic Framework skill document: %s", name)
	}
	return infos[0].Description
}
