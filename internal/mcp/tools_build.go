package mcp

// tools_build.go — the build-pipeline tools. Each one runs a single stage
// through the BuildPort (the buildctl Controller) and returns the stage's
// structured BuildResult verbatim — diagnostics, what recompiled, what went
// stale — which is exactly the shape the terminal narration already renders.

import (
	"context"
	"fmt"
	"strings"

	"github.com/gothicframework/cli/v4/internal/buildctl"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// buildOut is the shared result envelope of the build tools: the stage's own
// BuildResult, with OK kept at top level for the model's fast path.
type buildOut struct {
	OK          bool                  `json:"ok"`
	Summary     string                `json:"summary,omitempty"`
	Stage       string                `json:"stage"`
	Target      string                `json:"target,omitempty"`
	MS          int64                 `json:"ms"`
	Recompiled  []buildctl.Recompiled `json:"recompiled,omitempty"`
	NowStale    []string              `json:"nowStale,omitempty"`
	Diagnostics []buildctl.Diagnostic `json:"diagnostics,omitempty"`
	Raw         string                `json:"raw,omitempty"`
	Suppressed  bool                  `json:"suppressed,omitempty"`
}

// renderBuildOut maps one BuildResult onto the envelope.
func renderBuildOut(res buildctl.BuildResult) buildOut {
	out := buildOut{
		OK:          res.OK,
		Stage:       res.Stage,
		Target:      res.Target,
		MS:          res.MS,
		Recompiled:  res.Recompiled,
		NowStale:    res.NowStale,
		Diagnostics: res.Diagnostics,
		Raw:         res.Raw,
		Suppressed:  res.Suppressed,
	}
	switch {
	case res.Suppressed:
		out.Summary = "suppressed: an edit session holds the build lock"
	case res.OK:
		if len(res.Recompiled) > 0 {
			names := make([]string, 0, len(res.Recompiled))
			for _, r := range res.Recompiled {
				names = append(names, r.Path)
			}
			out.Summary = fmt.Sprintf("stage %s ok in %dms: %s", res.Stage, res.MS, strings.Join(names, ", "))
		} else {
			out.Summary = fmt.Sprintf("stage %s ok in %dms (nothing to recompile)", res.Stage, res.MS)
		}
	default:
		out.Summary = fmt.Sprintf("stage %s failed: %s", res.Stage, res.Err)
	}
	return out
}

func (s *surface) buildTemplTool(_ context.Context, _ *mcp.CallToolRequest, in struct {
	File string `json:"file,omitempty" jsonschema:"one .templ file to regenerate (empty = every dirty file)"`
},
) (*mcp.CallToolResult, buildOut, error) {
	c, err := s.currentCap()
	if err != nil {
		return nil, buildOut{}, err
	}
	b, err := c.needBuild()
	if err != nil {
		return nil, buildOut{}, err
	}
	res := b.BuildTempl(strings.TrimSpace(in.File))
	return nil, renderBuildOut(res), nil
}

func (s *surface) buildCSSTool(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, buildOut, error) {
	c, err := s.currentCap()
	if err != nil {
		return nil, buildOut{}, err
	}
	b, err := c.needBuild()
	if err != nil {
		return nil, buildOut{}, err
	}
	return nil, renderBuildOut(b.BuildCSS("")), nil
}

func (s *surface) buildWasmTool(_ context.Context, _ *mcp.CallToolRequest, in struct {
	Target string `json:"target,omitempty" jsonschema:"scope the report to one route/component/topic (empty = full inventory)"`
},
) (*mcp.CallToolResult, buildOut, error) {
	c, err := s.currentCap()
	if err != nil {
		return nil, buildOut{}, err
	}
	b, err := c.needBuild()
	if err != nil {
		return nil, buildOut{}, err
	}
	return nil, renderBuildOut(b.BuildWasm(strings.TrimSpace(in.Target))), nil
}

func (s *surface) buildGoTool(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, buildOut, error) {
	c, err := s.currentCap()
	if err != nil {
		return nil, buildOut{}, err
	}
	b, err := c.needBuild()
	if err != nil {
		return nil, buildOut{}, err
	}
	return nil, renderBuildOut(b.BuildGo()), nil
}

func (s *surface) syncTool(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, buildOut, error) {
	c, err := s.currentCap()
	if err != nil {
		return nil, buildOut{}, err
	}
	b, err := c.needBuild()
	if err != nil {
		return nil, buildOut{}, err
	}
	return nil, renderBuildOut(b.Sync()), nil
}

func (s *surface) buildStatusTool(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, buildctl.StatusResult, error) {
	c, err := s.currentCap()
	if err != nil {
		return nil, buildctl.StatusResult{}, err
	}
	b, err := c.needBuild()
	if err != nil {
		return nil, buildctl.StatusResult{}, err
	}
	return nil, b.BuildStatus(), nil
}
