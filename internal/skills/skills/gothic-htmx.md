---
name: gothic-htmx
description: "htmx in Gothic: the v4 surface (hx-action/hx-method, hx-status HCON overrides, htmx-config meta, namespaced events, finally events, extension allowlist), the Go HtmxEvent constants, preload/sigv4 extensions, and the Topic⇄htmx bridge."
applies_to:
---

# htmx in Gothic

**Import**: nothing on the page — htmx ships compiled into the framework's static
core asset (`gothic-core.wasm`) and `window.htmx` is installed at page boot. There
is no htmx JS file to load, serve, or pin; `window.htmx.version` reports the
upstream string. This document covers the **htmx-go v4 surface (htmx 4.0.0)**.

The Go side of a page (stateful WASM components) binds htmx events through
`core/wasm/wasm-runtime/runtime`: `HTMX.On(Evt…)` / `HTMX.OnGlobal(Evt…)` with the
`HtmxEvent` constants below, plus `HTMX.Ajax`, `HTMX.Swap`, `HTMX.Trigger`,
`HTMX.AddClass`, `HTMX.Find`, `HTMX.Values`.

## The v4 verb surface

- `hx-action="<url>"` + `hx-method="<verb>"` is the unified verb form. Verbs:
  `get`, `post`, `put`, `delete`, `patch`, `query`. Without `hx-method` the verb
  defaults to `get`.
- `hx-method="query"` is its own method token: the request goes out as HTTP
  `QUERY`, not GET — the receiving route must accept it.
- The per-verb attributes `hx-get`, `hx-post`, `hx-put`, `hx-patch`, `hx-delete`,
  `hx-query` still work and behave the same.
- Target/swap/select semantics are unchanged from classic htmx
  (`hx-target`, `hx-swap`, `hx-select`, OOB swaps).

## hx-status:<pattern> — per-response HCON overrides

An element (or closest ancestor) may carry `hx-status:200`, `hx-status:20x`, or
`hx-status:2xx` attributes. On a response whose status matches (exact string,
two-digit+`x`, or one-digit+`xx`), the attribute value is parsed as HCON and its
`swap` key overrides the swap for that response — so a 200 can force
`{"swap":"none"}` (fetch without swapping) or `{"swap":"textContent"}`. Patterns
are tried exact → `NNx` → `Nxx`.

## htmx-config meta (HCON)

`<meta name="htmx-config" content='{"extensions":"preload","defaultSwapStyle":"innerHTML"}'>`
is read at boot and merged into `window.htmx.config`. HCON accepts JSON objects
or `key: value` pairs. The `extensions` key is a comma-separated allowlist:
`htmx.defineExtension("name", ext)` (and the v4 alias `htmx.registerExtension`)
refuse any name not in the list, logging a console error. Register extensions
from Go after the core boots, e.g. `preload.Register()`.

## Events

v4 names events with namespaces. The Go constants keep their historic names but
point at the 4.0 strings:

| Go constant | Event that fires |
|---|---|
| `EvtBeforeRequest` / `EvtAfterRequest` | `htmx:before:request` / `htmx:after:request` |
| `EvtFinallyRequest` | `htmx:finally:request` — fires on every request round: success, HTTP error, abort, timeout, and a vetoed `before:request` |
| `EvtBeforeSwap` / `EvtAfterSwap` | `htmx:before:swap` / `htmx:after:swap` |
| `EvtFinallySwap` | `htmx:finally:swap` — after every swap round, success or error |
| `EvtBeforeInit` / `EvtAfterInit` | `htmx:before:init` / `htmx:after:init` |
| `EvtBeforeProcess` / `EvtAfterProcess` | `htmx:before:process` / `htmx:after:process` |
| `EvtBeforeCleanup` / `EvtAfterCleanup` | `htmx:before:cleanup` / `htmx:after:cleanup` |
| `EvtBeforeHistoryRestore` | `htmx:before:history:restore` |
| `EvtError` | `htmx:error` |

**The 2.x event names (`htmx:afterSwap`, `htmx:beforeSend`, …) never fire.** A
`document.addEventListener("htmx:afterSwap", …)` handler is silently dead —
migrate to the namespaced names. Several 2.x events no longer exist at all;
their Go constants remain for compile compatibility but match nothing
(disabled `EvtLoad`, `EvtSendError`, `EvtSwapError`, history-cache family, …).
Custom or extension events stay reachable via a string cast:
`HtmxEvent("htmx:sse:message")`.

## Extensions

- **preload** (`htmx-go/ext/preload`): warm the browser HTTP cache for `[preload]`
  anchors inside an `hx-ext="preload"` subtree. Call `preload.Register()` once,
  after the core boots; hover issues the warm GET, the click is a cache hit.
- **sigv4** (`htmx-go/ext/sigv4`): AWS SigV4 signing for htmx requests, registered
  through the RequestTransformer API.
- Standalone streaming extensions from upstream (`hx-sse`, `hx-ws`,
  `hx-multipart`, `hx-live`) are not part of the framework; load them from their
  own sources if a project needs them.

## Topic⇄htmx bridge

- `hx-trigger="topic:<name>"` — the element refetches when the topic publishes.
- `hx-publish="topic:<name>"` — the element publishes (notify-style) after its
  swap. See the `gothic-wasm-state-topics` skill for the topic side.

## What the agent CAN do

- Write htmx markup on templ pages using `hx-action`/`hx-method` or the legacy
  per-verb attributes; fragments are ordinary Gothic routes returning HTML.
- Use `hx-status:<pattern>` attributes to tune the swap per response status.
- Set framework-wide htmx config through a `htmx-config` meta (layout head or
  page body, before boot) and allowlist extensions there.
- Bind Go handlers to the namespaced events via the `Evt*` constants.
- Register the preload/sigv4 extensions from Go after boot.

## What the agent CANNOT do

- Expect 2.x event names to fire — listeners on `htmx:afterSwap` and friends are
  dead; use the namespaced names or the `Evt*` constants.
- Use `hx-method="query"` against a route that only accepts GET — the request
  arrives as `QUERY`.
- Gate an extension on the allowlist by registering before the `htmx-config`
  meta is parsed — the meta is read at boot, before any `defineExtension`.
- Serve htmx from a separate script tag or CDN — the runtime is the one inside
  `gothic-core.wasm`; a second htmx copy would shadow or conflict with it.

## Pitfalls

| Symptom | Cause | Fix |
|---|---|---|
| `hx-action` button does nothing, no console error | Pre-v4.0.1 runtime (the element was never initialised) | Update the core to the release shipping htmx-go v4 |
| JS listener never fires | Bound to a 2.x event name | Migrate to the 4.0 namespaced name |
| `hx-method="query"` request fails | Route accepts only GET | Accept the `QUERY` method, or use `hx-method="get"` |
| `hx-status:2xx` ignored on a 200 | Pattern misspelled, or the element's own `hx-swap` set on a pre-v4.0.1 core | Check the attribute name; update the core |
| Extension "not registered" console error | Name missing from the `htmx-config` `extensions` allowlist | Add it to the comma-separated list |
| htmx-config changes have no effect mid-page | Meta is read once, at boot | Full-page reload; keep config in the layout head |
