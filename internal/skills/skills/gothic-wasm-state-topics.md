---
name: gothic-wasm-state-topics
description: "Gothic v3 client state: ClientSideState + observables (CreateObservable/Observe/ObserveWithCleanup), Topics for cross-component shared state, scope isolation, teardown, tree-shaking, compiler choice."
applies_to: "core@>=1.0.0,<2.0.0"
---

# Gothic WASM state, observables and topics

**Import**: `import . "github.com/gothicframework/core/wasm"` (dot import, inside `ClientSideState`). All helpers compile as no-ops server-side and as the real implementations in the WASM binary — no build tags.

## How it works

`RouteConfig.ClientSideState func()` marks a page/component as stateful. The CLI extracts the body, tree-shakes same-package helpers into it, generates an entry point, and compiles a per-page TinyGo WASM binary (`gothic wasm`). The component is lazy-loaded via `@gothicComponents.StatefulComponentOf(&config)` — never inline it in SSR HTML.

### Observables (per-component state)

```go
count := CreateObservable(0)
Observe(func() {
    SetText("count", strconv.Itoa(count.Get()))
}, count)                       // deps list decides what re-runs the effect

CreateWasmFunc("increment", func() {
    count.Set(count.Get() + 1)  // Get() inside callbacks just reads — no subscription
})
```

- Effects re-run only for observables listed in the **deps list** — list every observable the effect reads; `Get()` alone does not subscribe.
- `ObserveWithCleanup(fn, deps...)` returns cleanup that runs before each re-run — use it for goroutines/tickers/connections. With no deps the cleanup is discarded.
- Compute derived values inside an effect; do not store them in extra observables.
- Scope-isolated DOM helpers (`SetText`, `SetHTML`, `GetValue`, `AddClass`, `SetAttr`, `SetStyle`) search the calling component's `[data-gothic-scope]` subtree — multi-instance safe. CSS property names are camelCase (`backgroundColor`, not `background-color`). `SetHTML` is for trusted content only.
- `*Observable[T]` has `Get`/`Set` only. `Peek()` exists only on topic fields (`*ObservableField[T]`).
- `BeginBatch`/`EndBatch` are runtime-internal, not exported — effects re-run once per `Set()`.

### Tree-shaking rules

Same-package `func`, `const`, `type` referenced by `ClientSideState` are inlined (transitively). Package-level `var` and `init()` are a **build error** — use `const`/`func`. Local project imports are allowed if they stay TinyGo-clean; changes to imported packages invalidate the WASM cache for pages using them.

### Topics (cross-component shared state)

Use Topics when ≥2 independent WASM components share state; `CreateObservable` for state local to one component.

```go
// src/topics/counter_topic.go — src/topics/ is a required folder; run `gothic wasm`
package gothicwasm

import . "github.com/gothicframework/core/wasm"

type Counter struct {
    Count int
    Theme string `gothic:"skip"` // skip: not broadcast — keeps payloads small
}

var CounterTopic = CreateTopic(Counter{}, TopicConfig{Name: "counter", Compression: BROTLI})
```

`gothic wasm` generates `src/topics/topic_gen.go` with the typed accessor (`func CounterTopic() *counterTopic`). **There is no mount step and no manager component** — calling the accessor inside any component's `ClientSideState` auto-registers the topic with the always-loaded static core hub.

- Field methods: `Get()` (read + register dep — use in `Observe` closures), `Peek()` (read without dep — use in `CreateWasmFunc` callbacks), `Set()` (write + broadcast).
- `topic.Field.Set(v)` broadcasts only that field — prefer it over whole-struct `topic.Set(struct)` on hot paths (whole-struct makes the hub diff fields).
- Wire: little-endian binary codec, auto-generated; `gothic:"i32|u32|skip"` tags adjust it. Keep payloads under ~100 kB — WASM32 linear memory never shrinks.
- Every topic needs a unique `TopicConfig.Name`. Supported field types: primitives, aliases, slices, maps (≤2 nesting levels), pointers, nested structs, `time.Time`.
- htmx bridge: `hx-trigger="topic:<name>"` refetches on publish; `hx-publish="topic:<name>"` publishes (notify-style) after a swap.

### Client-side storage

Three same-shaped helper families in `core/wasm` pick lifetime per need (all no-ops server-side, real browser implementations in WASM):

- `LocalStorageSet(key, value)` / `LocalStorageGet(key)` / `LocalStorageRemove(key)` — per-origin, survives restarts (`Get` returns `""` when missing). Default-first choice for UI persistence (theme, drafts, prefs).
- `SessionStorageSet/Get/Remove(key…)` — per-tab session: survives reloads, dies with the tab.
- `CookieSet(key, value, opts ...CookieOptions)` / `CookieGet(key)` / `CookieDelete(key)` — the only one the server can read on the next request; pass `CookieOptions{…}` for path/expiry semantics. HttpOnly cookies set by the server are NOT readable here (browser policy — see the typed-fetch skill).

### Scopes and teardown

Each placement gets a unique scope ID stamped as `data-gothic-scope`; callbacks dispatch per-scope via a registry, so two instances never collide. A page-level MutationObserver tears down instances removed from the DOM: user `OnUnmount(fn)` cleanups run, listeners detach, JS refs drop, then the instance halts — plus per-scope auto-cancel of in-flight `Fetch`/htmx requests. Full-page hx-boost navigation tears down the previous page scope automatically.

`Multiplexed: true` shares ONE WASM instance across all placements of the component type; each placement keeps its own isolated scope, and the instance halts when the last placement unmounts.

### Compiler choice

`WasmCompiler: routes.GothicTinyGo` (default) vs `routes.Golang` — the latter is only for code that transitively needs full stdlib (see the typed-fetch skill's DTO rule before switching).

## What the agent CAN do

- Declare observables, effects and DOM updates entirely in Go inside `ClientSideState`.
- Expose element callbacks via `CreateWasmFunc` / `CreateWasmStringFunc` / `CreateWasmBoolFunc` (wired by `onclick`/`oninput`/`onchange`).
- Share state across components with topics: declare in `src/topics/`, run `gothic wasm`, call the accessor everywhere.
- Use `OnUnmount` for cleanup; `ObserveWithCleanup` for resources recreated on dep change.
- Import local project packages into `ClientSideState` (TinyGo-clean ones) — tree-shaken in.
- Persist per-placement reactive state across re-mounts with `DurableObserve` + a stable `data-gothic-durable-key`.
- Persist UI-level values client-side with the storage helpers — localStorage (survives restarts), sessionStorage (per tab), cookies (server-readable; the only one the server can see).

## What the agent CANNOT do

- Use `net/http`, `crypto/tls`, `os/exec` (or packages importing them) under the default compiler — build error. Use `Fetch` from `core/wasm`.
- Reference a package-level `var` or `init()` from `ClientSideState` — build error. Use `const`/`func`.
- Call `BeginBatch`/`EndBatch` — not exported in `core/wasm`; the SSR-side compile fails.
- Call `Peek()` on a plain `*Observable[T]` — topic fields only.
- Rely on `Get()` inside an effect body to subscribe — deps come from the deps list.
- Call `Fetch`/`GetFileBytes` at the top level of `ClientSideState` — they block; call inside a goroutine or callback.
- Share state between two components via `DurableObserve` — it is store-only, private to one placement; use a topic.
- Combine `Multiplexed` with `CreateWasmFuncWithReturn` — the bare-global callback collides across shared scopes.
- Inline a stateful component in SSR HTML — always `StatefulComponentOf`.
