---
name: gothic-routing
description: "Gothic v3 routing: RouteConfig for templ pages/components, file-based URL routing with var_ params, STATIC/ISR/DYNAMIC route types, ApiRouteConfig JSON endpoints in src/api/, layouts."
applies_to: "core@>=1.0.0,<2.0.0"
---

# Gothic routing

**Import**: `routes "github.com/gothicframework/core/router"` — always aliased `routes` (the package itself is `package helpers`; generated code and user code both alias it `routes`).

## How it works

The CLI scans `src/pages/` and `src/components/` (`.templ`) plus `src/api/` (`.go`) and regenerates `src/routes/routes_gen.go` on every `gothic build`. `main.go` mounts the whole runtime as one chi middleware and registers the generated routes:

```go
router := chi.NewMux()
router.Use(middlewares.Middleware(Config.Runtime)) // caching, /public/*, /optimizedImage/*, /_gothic/*
routes.RegisterFileBasedRoutes(router)             // pages, components, and src/api/ routes
```

### RouteConfig (pages and components)

Per `.templ` file, discovery is independent for the two halves:

- **Config**: the FIRST `var X = routes.RouteConfig[T]{...}` in the file — any variable name.
- **Handler**: the first exported func returning `templ.Component`.
- No `RouteConfig` var → the route falls back to `routes.DefaultConfig` (STATIC, GET), silently.

```go
type LoginPageProps = interface{} // single Props struct; use interface{} when there are none

var LoginConfig = routes.RouteConfig[LoginPageProps]{
    Type:       routes.DYNAMIC,        // auth-required pages are DYNAMIC
    HttpMethod: routes.GET,
    Middleware: func(w http.ResponseWriter, r *http.Request) LoginPageProps {
        return LoginPageProps{} // load data; chi.URLParam(r, "name") for URL params
    },
}

templ Login(props LoginPageProps) { ... }
```

Route types:

| Type | Middleware timing | Caching | Use when |
|---|---|---|---|
| `routes.STATIC` | Once at server start | The rendered page is cached (route-level store; CDN/headers/deploy choice); every later request is served straight from that cache | Public pages, no per-request data |
| `routes.DYNAMIC` | Every request | Never cached (`no-store`) | Auth / personalized pages |
| `routes.ISR` | On cache miss | Cached, revalidates at `RevalidateInSec` | Rarely-changing public data |

### The STATIC cache trap (before any auth or per-user page)

A STATIC route runs its `Middleware` **once** at server start; from then on
every request — every user, every session — is served from the cached render.
An auth or role check on a STATIC page executes once and its result is frozen
for the next request regardless of who asks: it does not protect anything.
Personalized, session-, role-, auth-, or locale-dependent pages are DYNAMIC.
This is the single most frequent route-type mistake.

Other fields: `RevalidateInSec` (ISR TTL), `WasmCompiler` / `WasmCompression` (with `ClientSideState`), `Multiplexed bool` (one shared WASM instance across N placements of the same component type; topic managers are never multiplexed, and it never mixes with `CreateWasmFuncWithReturn`). `Path` is auto-populated by `RegisterRoute` — leave it unset and read it instead.

### URL params and directory mapping

`var_paramName/` directories become `{paramName}` chi params:

```
src/pages/blog/var_slug/index.templ → GET /blog/{slug}
src/pages/index.templ               → GET /
src/components/counter/counter.templ → GET /components/counter
```

### API routes (`src/api/`)

Plain `.go` files, auto-registered, full-HTTP-response caching (status + headers + body):

```go
var ProductsConfig = routes.ApiRouteConfig{HttpMethod: routes.GET, Type: routes.ISR, RevalidateInSec: 60}

func Products(w http.ResponseWriter, r *http.Request) { /* plain handler */ }
```

- Discovery: first `var X = routes.ApiRouteConfig{...}` → config; first top-level func with no return values → handler. Fallback: `DefaultApiConfig` (GET, DYNAMIC).
- URL params use the same `var_paramName/` convention; read with `chi.URLParam`.
- `src/api/` routes are registered WITHOUT auth middleware. Auth-protected or HTML-fragment (HTMX) endpoints are registered manually in `main.go` middleware groups.

### templ file discovery — order matters

Per `.templ` file, the FIRST exported func returning `templ.Component` is the
one rendered on this route. If the file needs more templ funcs (fragment
helpers, sub-components), declare them BELOW that first function — anything
declared above it displaces the route handler. Prefer one page/component per
file; multiple are legal only in the first-below order. Every templ func
MUST take a props argument, even the 404 page (`interface{}` + `nil`).

### Where code belongs

- **`main.go`** — global concerns: the chi middleware chain
  (`router.Use(...)`), auth/session middleware GROUPS (including every
  endpoint from `src/api/` that needs a login), the custom 404 catch-all
  (`router.Get("/*", ...)`), and any cross-cutting wiring that must wrap ALL
  routes.
- **The per-route `Middleware` func** — per-route data: load this route's
  props, read URL params, per-route validation. It is NOT the place for a
  global concern (and on STATIC/ISR it runs at most once — the cache trap).
- **Layouts (`src/layouts/`)** — the document: `<html>/<head>` +
  `@gothicComponents.Styles()` / `@gothicComponents.RuntimeScripts()`.
- Rule of thumb: if two routes need it, it is a middleware or a helper
  package — not two copies of the same code in two `Middleware` funcs.

### When `src/api/` routes are the answer (the BFF idea)

API routes exist for **responses that are not a templ component**. Pick this
layer when one of these is true:

- **The consumer is not a browser page**: a mobile app, a native client, a
  third-party integration, a webhook target. Return the exact JSON (or any
  byte format) that consumer expects — no HTML anywhere in the response.
- **A JS library demands a precise JSON shape** (chart/date-map/component-tree
  packages). Build that shape on the server instead of fighting it from
  WASM: a BFF route collects from the DB/external APIs and returns the exact
  document the library wants (the `Fetch`→`Decode` typed flow in
  `gothic-typed-fetch-json` consumes it client-side without extra JS).
- **Aggregation / BFF** — the Next.js-style Backend-for-Frontend page
  composition: one route merges several upstream calls into a single
  response the page renders. The page (or another component) calls this route
  instead of making N fragile cross-origin calls from the browser.

Shape of a BFF route:

```go
// src/api/chart/var_series/chart.go — GET /chart/{series}
var ChartConfig = routes.ApiRouteConfig{HttpMethod: routes.GET, Type: routes.DYNAMIC}

func Chart(w http.ResponseWriter, r *http.Request) {
    series := chi.URLParam(r, "series")
    data := collectAndShape(series) // collect from the DB/external APIs; output = the exact wire shape
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(data)
}
```

- The BFF route serves every consumer: the page's WASM code calls it via
  `Fetch`/`Decode`, vendored JS reads it with `fetch()`/the library's loader,
  and external clients get the same URL. One source of truth per JSON shape.
- Caching type choice applies here: ISR caches the aggregated response (TTL
  for shapes that change rarely); DYNAMIC for downstream/live shapes.
- Auth: same rule as any `src/api/` handler — manual registration in `main.go`.

### Custom 404 (not-found)

Register a catch-all in `main.go` AFTER `RegisterFileBasedRoutes`:

```go
router.Get("/*", func(w http.ResponseWriter, r *http.Request) {
    pages.NotFound(nil).Render(r.Context(), w)
})
```

A RouteConfig is optional for pages/components, but every templ page/component function MUST take a props argument even when it is `interface{}` and receives `nil` — so the 404 page receives `nil` here.

### Layouts

Pages use layouts from `src/layouts/` (project code); components return fragments. Put `@gothicComponents.Styles()` and `@gothicComponents.RuntimeScripts()` in the layout `<head>`.

## What the agent CAN do

- Add pages/components by creating `.templ` files with a `RouteConfig` var, then running `gothic build` and confirming the route in `routes_gen.go`.
- Add fragment/sub component templ funcs to the same file BELOW the route's first exported templ func (never above it).
- Choose route types per page; set `RevalidateInSec` on ISR routes.
- Read URL params via `chi.URLParam` inside the `Middleware` func; return per-request props.
- Embed stateful WASM components with `@gothicComponents.StatefulComponentOf(&config)` — it reads the auto-populated `config.Path`.
- Register `Multiplexed: true` for a component placed many times on one page.
- Register a custom 404 by catching `/*` in `main.go` after the auto-routes.
- Register HTMX data endpoints and auth-protected endpoints manually in `main.go` groups.

## What the agent CANNOT do

- Hand-edit `src/routes/routes_gen.go` — regenerated on every build.
- Set `RouteConfig.Path` manually — the router populates it.
- Declare a secondary templ func above the file's first exported route func — that func is the route renderer; helpers go below.
- Auth-protect anything under `src/api/` — those routes register without middleware; move them to `main.go` groups.
- Write API endpoints as `.templ` files — `src/api/` scans `.go` files only.
- Write `<html>/<head>/<body>` inside `src/pages/` templates — only layouts in `src/layouts/` own document tags; `src/components/` templates must stay fragment-only.
- Rely on `STATIC` for personalized or per-request content — the middleware runs once and the cached render is what everyone sees.
- Add an auth check to a STATIC route and expect it to run per request — it will not; re-check the route type before any auth wiring.
- Expect inferred Props types — the `RouteConfig[T]` type argument must match the templ function's first argument.

## Pitfalls

| Symptom | Cause | Fix |
|---|---|---|
| Route registers as `DefaultConfig` | No `RouteConfig` var in file | Add `var X = routes.RouteConfig[T]{...}` |
| Wrong page renders on a multi-templ-file route | A templ func was declared ABOVE the intended route handler | Move added funcs BELOW the first exported route func |
| URL param always empty | Directory not `var_paramName` | Rename the directory to the `var_` convention |
| ISR page serves stale content | `RevalidateInSec` too high | Lower it, or switch to DYNAMIC |
| Auth not enforced on a page | Route type is STATIC — its middleware ran once at start and the cached render is served to every later request | Use DYNAMIC for anything auth/session/role-dependent |
| API handler not found | No top-level no-return func in the file | `func Name(w http.ResponseWriter, r *http.Request)` |
