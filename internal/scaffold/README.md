## Gothic Framework

**Gothic Framework** is a developer-first toolset built to help you craft fast, scalable, and modern web applications using the **GOTTH stack**:  
**Golang**, **TailwindCSS**, **Templ**, and **HTMX**.

Inspired by frameworks like **Next.js**, Gothic Framework brings powerful full-stack features to Go developers — including edge-ready architecture, SEO enhancements, and a fantastic development experience (DX).

This project is configured via `gothic.config.go` and deploys to AWS with **OpenTofu** (the OpenTofu binary is downloaded automatically on first deploy). Deploys require **Go 1.23+**, a running **Docker daemon**, and **AWS credentials**.

See the [top-level README](https://github.com/gothicframework/core/blob/main/README.md) for configuration, lifecycle hooks, and deployment details.

## Developing with an AI agent (dev MCP)

`gothic hot-reload` serves an MCP server (Streamable HTTP) at:

```
http://127.0.0.1:3000/_gothicframework/mcp
```

It is on by default (`--no-mcp` disables it) and is reachable only while the dev process runs. The managed browser opens VISIBLE at session start so you can watch the app while working; an agent can flip it to headless with `browser_mode`. It gives an agent the same loop a maintainer has: drive the managed browser (`navigate`, `act`, `view`, `record_start`/`record_stop`), read the request trace and the browser event bus (`trace`, `logs`), run the build stages with translated compiler errors (`build_templ`, `build_css`, `build_wasm`, `build_go`, `sync`, `build_status`), and load version-matched framework skills (`skillsearch`, `skillinfo`, `skill`, plus the `gothic-skill://{name}` resources).

Point your MCP client at that URL while `gothic hot-reload` is running:

**Claude Code**

```bash
claude mcp add --transport http gothic-dev http://127.0.0.1:3000/_gothicframework/mcp
```

**OpenCode** — add to `opencode.json` (project or `~/.config/opencode/opencode.json`):

```json
{
  "mcp": {
    "servers": {
      "gothic-dev": {
        "type": "remote",
        "url": "http://127.0.0.1:3000/_gothicframework/mcp",
        "oauth": false
      }
    }
  }
}
```

**Codex** — add to `~/.codex/config.toml`:

```toml
[mcp_servers.gothic-dev]
url = "http://127.0.0.1:3000/_gothicframework/mcp"
```

**Cursor** — add to `.cursor/mcp.json`:

```json
{
  "mcpServers": {
    "gothic-dev": {
      "url": "http://127.0.0.1:3000/_gothicframework/mcp"
    }
  }
}
```

**Pi** — Pi connects to MCP through its extension ecosystem:

```bash
pi install npm:pi-mcp-extension
```

```json
{
  "mcpServers": {
    "gothic-dev": {
      "transport": "streamable-http",
      "url": "http://127.0.0.1:3000/_gothicframework/mcp",
      "lifecycle": "eager"
    }
  }
}
```


[![See our docs](https://img.shields.io/badge/See_our_docs-ec4899?style=for-the-badge)](https://gothicframework.com)