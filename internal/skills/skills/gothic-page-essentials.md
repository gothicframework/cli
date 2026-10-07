---
name: gothic-page-essentials
description: "Default-good page assembly: route-type choice (the STATIC cache trap), initial state via StatefulComponentData, OptimizedImage keyed literals + LCP Priority, built-in preload, SEO via server-rendered templ, the minimal-JS decision order."
applies_to: "core@>=1.0.0,<2.0.0 components@>=1.0.0"
---

# Gothic page essentials

The layer that separates a working page from a good one. Every new page should
pass this checklist in order — each bullet names the exact tool; deeper
specifics live in the dedicated skills (`gothic-routing`,
`gothic-wasm-state-topics`, `gothic-typed-fetch-json`, `gothic-htmx`).

## The checklist

1. **Choose the route type first** (`gothic-routing`): STATIC runs its middleware
   once and serves the cached render to every later request — an auth check on
   STATIC protects nothing after the first hit. Auth/session/per-user →
   DYNAMIC; public non-personal pages → STATIC (best for caching/SEO);
   rarely-changing public data → ISR with `RevalidateInSec`.
2. **Interactive region → StatefulComponentOf**, not inline WASM. A stateful
   component must be reached through `@gothicComponents.StatefulComponentOf(&Config)`
   — each placement owns a scoped WASM instance (its mount fetches the route,
   which serves the component markup + its bootstrap). Pass
   `gothicComponents.StatefulComponentData{"user": id}` as the optional second
   arg to carry initial parameters on that first request (`hx-vals`); the
   route's `Middleware` turns them into per-request props.
3. **Shared client state → Topics**, not element-scoped rewrites. Cross-component
   reactivity is `CreateTopic`/`GetTopic` on `gothic:*` events; a topic
   auto-registers when an accessor is used in `ClientSideState` (no mount
   ceremony). htmx interop: `hx-trigger="topic:name"` and
   `hx-publish="topic:name"`.
4. **JSON APIs → typed Fetch**, not hand parsing: `Fetch`/`FetchAsync` +
   `Decode[T]` under `ClientSideState`, DTOs free of `net/http` imports. A
   response that must NOT be HTML (mobile/external consumer) or must match a
   library's exact JSON shape is a **BFF route in `src/api/`** — see
   `gothic-routing` → "When `src/api/` routes are the answer"; the BFF
   reuses the same URL for the page, vendored JS, and external clients.
5. **Every `<img>` above the fold → `OptimizedImage` with `Priority: true`**
   (LCP: `fetchpriority=high`, no lazy). KEYED field literals — `Priority`
   sits after `Alt`, so positional literals broke when it was added and stay
   broken in practice; write `OptimizedImageProps{IsFirstLoad: true, ImgName:
   "hero", ImgExtension: "webp", Alt: "...", Priority: true}`. Run
   `gothic optimize-images` to generate the `blurred`/`original` variants the
   two-phase loader swaps between.
6. **Likely-to-be-followed targets → preload.** `hx-ext="preload"` +
   `preload="…"` attributes on anchors/datalist-built hover targets: the
   response is warmed before the click. preload is built into the runtime —
   no script tag, no npm.
7. **SEO = server-rendered truth.** templ renders full HTML at the server;
   crawlers need no JS execution. Keep key content in the templ body, pick
   CRAWLER-visible route types (STATIC/ISR for public pages), and keep the
   layout `<head>` schema (`@gothicComponents.Styles()` +
   `@gothicComponents.RuntimeScripts()`).
8. **The JS decision**: nothing. If a feature seems to need a `.js` file, check
   the Go/WASM tools first (see `gothic-invariants` — Philosophy); a vendored
   third-party `<script>` with no WASM equivalent is the only accepted use.

## What the agent CAN do

- Assemble a page that uses route types, WASM components, topics, typed fetch,
  image optimization and preload together by following the checklist order.
- Carry initial state into a WASM placement with `StatefulComponentData`
  (it marshals through `hx-vals`).
- Use `OptimizedImage` with keyed props and `Priority: true` on the LCP image
  only.
- Add `hx-ext="preload"` and `preload` attributes knowing preload is built in.

## What the agent CANNOT do

- Put an auth/session check on a STATIC route and expect per-request
  enforcement — STATIC's middleware runs once; use DYNAMIC.
- Use positional `OptimizedImageProps{...}` literals — the field order is
  source-breaking; keyed literals only.
- Re-create preload with a script tag — built into the runtime.
- Scaffold a `.js` file to glue the page when the tool list above covers it.
- Set `RouteConfig.Path` manually or hand-edit `routes_gen.go` (see
  `gothic-routing`).

## Pitfalls

| Symptom | Cause | Fix |
|---|---|---|
| Everyone shares one user's data on a STATIC page | The middleware result was frozen into the cache at server start | Route type DYNAMIC |
| Image loads twice / placeholder stuck | Non-keyed literal dropped `IsFirstLoad`, or `blurred/original` variants not generated | Keyed literals + `gothic optimize-images` |
| LCP late despite lazy images | LCP candidate not marked | `Priority: true` on the above-fold image (keyed literal) |
| WASM component never mounts | Stateful component returned as plain templ instead of `StatefulComponentOf` | Wrap with `StatefulComponentOf(&Config)` |
| Topic publish never received | Topic name mismatched prefix, or accessor unused | Register aliases before `Start()`; load `gothic-wasm-state-topics` |
| JSON field empty after fetch | DTO imports `net/http` or `Encode` not codegen'd | net/http-free DTO; `Decode[T]` per `gothic-typed-fetch-json` |
| JS chart library demands its own JSON shape, WASM twists itself to produce it | The shape is being built client-side | Build it in a `src/api/` BFF route and render/serve there; the page consumes the same URL (see `gothic-routing` BFF section) |
