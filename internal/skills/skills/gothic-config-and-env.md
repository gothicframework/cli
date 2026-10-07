---
name: gothic-config-and-env
description: "Gothic v3 config: gothic.config.go schema (Runtime cache/static modes, CacheConfig), ENV builders (gothic.Env/SSMParam/SecretsManager), the static-literal parse contract, main.go wiring, GOTHIC_MODE=dev."
applies_to: "core@>=1.0.0,<2.0.0 middlewares@>=1.3.0"
---

# Gothic config and env

The project is configured by a type-safe Go file, `gothic.config.go`, at the project root — a `gothic.Config` value from `github.com/gothicframework/core/config` (aliased `gothic`). Runtime/cache settings live under `Config.Runtime`; deploy settings under `Config.Deploy` (see the deploy skill). `main.go` mounts the runtime and registers routes.

## How it works

### Schema

```go
// gothic.config.go
package main

import gothic "github.com/gothicframework/core/config"

var Config = gothic.Config{
    ProjectName: "my-app",
    Runtime: gothic.RuntimeConfig{
        CacheStrategy:         gothic.REDIS,     // production cache backend
        LocalDevelopmentCache: gothic.IN_MEMORY, // dev cache (active when GOTHIC_MODE=dev)
        ServeStaticFiles:      gothic.CDN,       // CDN | DISK | EMBEDDED
        CacheConfig: &gothic.CacheConfig{
            RedisURL:          os.Getenv("REDIS_URL"),
            RedisPassword:     os.Getenv("REDIS_PASSWORD"),
            RedisTLS:          true, // ElastiCache / Upstash / any TLS Redis
            Compression:       true,
            CompressionMethod: gothic.BROTLI,
        },
    },
    // Deploy: &gothic.DeployConfig{...} — see the deploy skill
}
```

`RuntimeConfig` reference:

| Field | Type | Default | Description |
|---|---|---|---|
| `CacheStrategy` | `CacheType` | `CACHE_CONTROL_HEADERS` | Production cache backend |
| `LocalDevelopmentCache` | `CacheType` | `IN_MEMORY` | Dev cache (GOTHIC_MODE=dev) |
| `ServeStaticFiles` | `StaticFilesMode` | `CDN` | Where `/public/*` is served from |
| `CacheConfig` | `*CacheConfig` | `nil` | Redis/files/compression settings |

The zero value is the sensible default, so `Runtime` may be omitted entirely.

`CacheType`: `CACHE_CONTROL_HEADERS` (no server store — CDN/browser via headers, production default), `IN_MEMORY` (`sync.Map`, cleared on restart), `REDIS` (multi-instance production; auto-falls back to IN_MEMORY with a warning if the connection fails at startup), `LOCAL_FILES` (`.gothic-cache/` on disk).

`ServeStaticFiles`: `CDN` (do not serve `/public/*` in prod — CDN/object store does; still disk-served in dev), `DISK` (serve `./public` from disk everywhere), `EMBEDDED` (bake `./public` into the binary via `go:embed` — one self-contained binary for self-hosted containers/VMs).

`GOTHIC_MODE=dev` activates: the `LocalDevelopmentCache` store, a cache flush at every startup, unconditional disk serving of `/public/*`, `no-store` headers on static responses, and stripping of `If-None-Match`/`If-Modified-Since`. `gothic hot-reload` sets it automatically.

### main.go wiring

```go
router := chi.NewMux()
router.Use(middlewares.Middleware(Config.Runtime)) // the ENTIRE runtime
routes.RegisterFileBasedRoutes(router)
```

In the layout `<head>`: `@gothicComponents.Styles()` and `@gothicComponents.RuntimeScripts()`.

### Source-aware ENV values

`ENV` entries (and the per-stage domain/cert fields) are pulled at deploy time from a source — secrets never sit in the config in plain text:

| Builder | Resolves to |
|---|---|
| `gothic.Env("literal")` | a literal string |
| `gothic.SSMParam("/path")` | an SSM Parameter Store value |
| `gothic.SecretsManager("/path")` | a Secrets Manager secret (whole string) |
| `gothic.SecretsManager("/path").Get("field")` | one field of a JSON secret |

### The parse contract

CLI tooling parses `gothic.config.go` with `go/parser` — **no type checker**:

- Only static composite literals are understood: string/int literals, nested struct/map literals, and the three ENV builder calls.
- A non-literal expression (bare identifier, `os.Getenv(...)` in a field) is **silently ignored** by that tooling — the field reads as its zero value with no warning.
- An **unknown ENV builder is a hard parse error** (e.g. `gothic.Secret(...)` aborts the load): valid builders are `gothic.Env`, `gothic.SSMParam`, `gothic.SecretsManager` (+ chained `.Get`).
- The config literal must be `var Config = ...{}`; `GoModName` is never declared here — it is read from `go.mod`.

### Lifecycle hooks

Two optional top-level functions in `gothic.config.go` run around a deploy: `BeforeDeploy(ctx, *gothic.GothicContext) error` (after prepare, before image build + apply — a returned error aborts) and `AfterDeploy(ctx, *gothic.GothicContext) error` (with stack outputs populated). Hooks run out-of-process via `GOTHIC_CONTEXT`; `--action delete` runs none. Undeclared hooks are no-ops.

### The dev command and the Developer MCP

`gothic hot-reload` is the dev session: it builds and serves the app on the
internal port (`:60714`) behind the dev proxy at **`127.0.0.1:3000`**, sets
`GOTHIC_MODE=dev` for you, watches sources (templ/Tailwind/WASM) and reloads.

- **The Developer MCP is part of this session**: it serves
  `/_gothicframework/mcp` on the proxy — **default-on**, with an opt-out
  `--no-mcp`. The session owns a managed browser (headless or headful) that
  the MCP drives; the developer's own browser is NOT opened by the dev
  command.
- **If you are reading this through the MCP, the session is already running —
  never start a second `gothic hot-reload` as part of normal work.** A second
  instance builds first, then dies at bind with `failed to start proxy
  server: ... address already in use`; if it started while the first was
  rebuild-halted, two watchers race the same `.gothicCli/` caches and the
  served artifacts become undefined. The ports are fixed (`:3000` proxy /
  `:60714` app) and machine-shared — there is no `--port` override in this
  version, so **one dev session per machine** regardless of directory.

**When CAN a (re)start happen?**

- **Nothing answers on `127.0.0.1:3000`** (a quick GET fails to connect):
  no session holds the ports — starting one is the normal path.
- **The developer explicitly asks for a restart**: stop the old session
  first (its own shutdown or the pid file the starter saved), confirm `:3000`
  is free, then start the new one. Never start-before-stop.

**When a restart shows up as legitimate but is still mine to refuse:**

- Procedural restarts, cursor-style re-invocations, "let me run it to be
  sure" — an agent inside an MCP session got here THROUGH the live session;
  starting another one is always a mistake with a painful cleanup.
- `GOTHIC_MODE=dev` effects (also active under any other dev run): local-dev
  cache backend, cache flush at startup, unconditional disk serving of
  `/public/*`, `no-store` headers, conditional-request headers stripped.

> Note: the public docs page for the dev command predates this — it does not
> mention the MCP/browser behavior yet.

### VS Code extension

`gothicframework-vscode` — the Gothic VS Code extension — formats `templ`
files and hides generated files (`*_templ.go`, `*_gen.go`, `routes_gen.go`,
the EMBEDDED-mode `gothic_embed_gen.go`), so the editor surface is only what
the developer owns. Suggest it when the developer is on VS Code.

### What `middlewares.Middleware` actually does

`router.Use(middlewares.Middleware(Config.Runtime))` is the entire framework
runtime in one middleware. Setup (once):

1. Builds a private chi mux and mounts:
   - **`/public/*`** — served per `ServeStaticFiles`: `CDN` keeps prod from
     serving them (the CDN/object store does; dev still disk-serves),
     `DISK` serves from disk everywhere, `EMBEDDED` serves from a `go:embed`
     of `public/` generated as `gothic_embed.go` (one self-contained binary).
   - **`/optimizedImage/{name}/{extension}`** — the OptimizedImage Phase-2
     endpoint (needs its own chi mux so `chi.URLParam` works).
   - **`/_gothic/*`** — the 5 embedded runtime assets (core js/wasm/boot/exec
     + the wasm_exec shim), content-negotiated br/gzip, `?v=` content-hash
     immutable caching.
2. Initializes the process-wide cache backend per `Runtime` (deploy behavior
   of each choice: see the deploy skill — CDN cache rules exist on the AWS
   side as well).

Per request, only `public`/`optimizedImage`/`_gothic` prefixes go through
this internal mux; everything else falls through to your file-based routes.
Future built-in middlewares (auth, CORS, …) join the same `middlewares`
module and mount automatically in `main.go`.

### Environment variables

`GOTHIC_MODE=dev` (set automatically by hot-reload) and `HTTP_LISTEN_ADDR` (default `:8080`).

## What the agent CAN do

- Set/omit `Runtime` fields — the zero value is the documented default.
- Ride the running dev session (its MCP and managed browser are already up) and propose restarts via the developer — not with a second process.
- Suggest the Gothic VS Code extension (`gothicframework-vscode`) to a VS Code user — templ formatting + generated files hidden.
- Choose cache backends per environment (production `CacheStrategy` vs dev `LocalDevelopmentCache`).
- Configure Redis (URL/password/TLS), compression (GZIP/Brotli), or disk cache paths.
- Pick `CDN`/`DISK`/`EMBEDDED` static serving — `EMBEDDED` for a single self-hosted binary.
- Source ENV values from env/SSM/Secrets Manager, including JSON-keyed secret fields via `.Get`.
- Declare `BeforeDeploy`/`AfterDeploy` hooks with the exact signatures above.

## What the agent CANNOT do

- Start another `gothic hot-reload` while any dev session is live — fixed ports are machine-shared (`:3000`/`:60714`, no override), one session per machine; two instances die at bind or flip the first session to a stale server. Restart only zero-before-one: stop → confirm `:3000` free → start.
- Rely on dynamic/non-literal config values being seen by CLI tooling — they are silently ignored by the parser.
- Call ENV builders other than the three (plus `.Get`) — hard parse error.
- Put `GoModName`/module name in `gothic.config.go` — it comes from `go.mod`.
- Rename or reorder the `ServeStaticFiles` modes — the enum is append-only and its ordinals are baked into compiled projects.
- Expect `GOTHIC_MODE=dev` behaviors in production — they are dev-only.
- Use a v2 `gothic-config.json` on v3 — run `gothic migrate-v3` (a present `.json` alongside a `.go` config is ignored; a `.json` alone is an error).
