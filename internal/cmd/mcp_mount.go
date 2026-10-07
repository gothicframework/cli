package cmd

// mcp_mount.go — the dev session's MCP surface: the capability factory over
// the session's real machinery and the mount into the dev proxy's handler
// map. The endpoint answers at /_gothicframework/mcp on the dev proxy (Stream
// able HTTP, JSON responses); --no-mcp keeps the path answering a clean 404
// instead.

import (
	"net/http"
	"time"

	"github.com/gothicframework/cli/v3/internal/browser"
	gothic_cli "github.com/gothicframework/cli/v3/internal/cli"
	"github.com/gothicframework/cli/v3/internal/mcp"
	"github.com/gothicframework/cli/v3/internal/output"
	"github.com/gothicframework/cli/v3/internal/skills"
)

// managedEngineLabel is the engine label the MCP page-view payloads carry;
// the managed browser's exact version is that of the OS-installed Chromium
// the launcher picks, which the CLI does not name.
const managedEngineLabel = "managed dev browser"

// mcpProxyBase is the base URL the MCP timeline reader uses to reach the dev
// proxy's own HTTP endpoints (the app trace and the dev bus) from this
// process. RunProxy binds "localhost":3000.
const mcpProxyBase = "http://127.0.0.1:3000"

// mountMCPSurface binds the MCP surface into the dev proxy's dev-handler map
// before the session's select loop takes over. When MCP is off, the endpoint
// answers a deterministic 404 without a backend round trip.
func (command *HotReloadCommand) mountMCPSurface() {
	if command.cli.Proxy.DevHandlers == nil {
		command.cli.Proxy.DevHandlers = map[string]http.Handler{}
	}
	if command.noMCP {
		command.cli.Proxy.DevHandlers[mcp.MCPEndpointPath] = http.NotFoundHandler()
		return
	}
	command.cli.Proxy.DevHandlers[mcp.MCPEndpointPath] = mcp.Handler(command.mcpCapability)
	output.Println("%s Dev MCP server at %s — point any Streamable-HTTP MCP client at that URL (off with --no-mcp)",
		output.Tag("MCP"), output.Link(mcpProxyBase+mcp.MCPEndpointPath))
}

// mcpCapability builds the session's machinery snapshot: the managed browser,
// the build controller, the observation timeline over the proxy's HTTP
// surface, the version-gated skill set, and the two dev-bus seams. It runs on
// every MCP request and at server construction, so it touches only
// concurrency-safe surfaces.
func (command *HotReloadCommand) mcpCapability() *mcp.Capability {
	return &mcp.Capability{
		Browser: mcp.NewBrowserPort(command.sessionBrowser()),
		Build:   command.buildCtl(),
		Timeline: mcp.HTTPBaseTimeline{
			Base: mcpProxyBase,
		},
		Skills: command.skillSet(),
		MarkSink: func(event string, payload map[string]any) error {
			command.cli.Proxy.Ingest(mcpMarkRecord(event, payload))
			return nil
		},
		Marks: func() []browser.TakeMark {
			return browser.MarksFromRecords(command.cli.Proxy.Records(-1))
		},
		Engine:  managedEngineLabel,
		Version: CURRENT_VERSION,
	}
}

// mcpMarkRecord shapes one mark record on the dev bus contract: kind "mark",
// dir "act", event the tool-provided name, payload the tool-provided context.
// Bus records are the one ring both the injected script's observed inputs and
// tool-injected marks land in.
func mcpMarkRecord(event string, payload map[string]any) []map[string]any {
	if payload == nil {
		payload = map[string]any{}
	}
	return []map[string]any{{
		"t":       float64(time.Now().UnixMilli()),
		"kind":    "mark",
		"dir":     "act",
		"event":   event,
		"payload": payload,
	}}
}

// sessionBrowser returns the session's managed browser, nil when it has not
// started (or was torn down). Reads are serialized with the assignments so a
// request landing during shutdown cannot race.
func (command *HotReloadCommand) sessionBrowser() *browser.Manager {
	command.managedMu.Lock()
	defer command.managedMu.Unlock()
	return command.managedBrowser
}

// skillSet resolves the bundled skills against the project's own framework
// module pins (the same versions the user's go.mod requires).
func (command *HotReloadCommand) skillSet() *skills.Set {
	pins := map[string]string{}
	if cfg, err := command.skillConfig(); err == nil {
		for _, mod := range cfg.FrameworkModules {
			pins[mod.Path] = mod.Version
		}
	}
	return skills.New(pins)
}

// skillConfig reads the config once per session for the pins (the parser
// re-reads go.mod; caching it keeps the MCP request path off the disk).
func (command *HotReloadCommand) skillConfig() (gothic_cli.Config, error) {
	command.skillConfigOnce.Do(func() {
		cfg, err := command.cli.GetConfig()
		command.skillConfigVal, command.skillConfigErr = cfg, err
	})
	return command.skillConfigVal, command.skillConfigErr
}
