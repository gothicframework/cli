---
name: gothic-typed-fetch-json
description: "Gothic v3 typed data from WASM: Fetch → Response (Text/Bytes/OK/MapAny), FetchAsync/FetchChan with per-scope auto-cancel, reflection-free Decode[T]/Encode[T], the net/http-free DTO rule."
applies_to: "core@>=1.0.0,<2.0.0"
---

# Gothic typed fetch and JSON

**Import**: `import . "github.com/gothicframework/core/wasm"` (dot import). All helpers are no-ops server-side; real browser-API implementations live in the WASM binary.

## How it works

### Fetch → Response

`Fetch(url string, cfg ...FetchConfig) (Response, error)` runs the browser's fetch and blocks the component until complete (asyncify). It must be called inside a goroutine or a `CreateWasmFunc` handler — never at the top level of `ClientSideState`.

```go
CreateWasmFunc("load", func() {
    go func() {
        resp, err := Fetch("/api/todos/1")
        if err != nil {
            SetText("error", "failed: "+err.Error())
            return
        }
        if !resp.OK() { // 200..299
            SetText("error", "http "+strconv.Itoa(resp.Status))
            return
        }
        SetHTML("result", resp.Text())
    }()
})
```

```go
type Response struct {
    Status  int               // HTTP status code
    Headers map[string]string // lower-cased keys, e.g. "content-type"
    Body    []byte            // raw body bytes
}

func (r Response) Text() string                   // string(Body)
func (r Response) Bytes() []byte                  // binary payloads, untouched
func (r Response) OK() bool                       // Status in 200..299
func (r Response) MapAny() (map[string]any, error) // untyped JSON, object roots only
```

`FetchConfig` fields: `Method` (default GET), `Headers`, `Body` (text), `BodyBytes` (binary — used when `Body` is empty), `Query` (URL-encoded automatically). `Fetch` is subject to browser CORS.

Async forms: `FetchAsync(url, cfg, done func(Response, error))` — `done` runs in the calling component's scope, no goroutine needed. `FetchChan(url, cfg...) <-chan FetchResult` — one result per call; fire N, collect N (`Promise.allSettled`), composes with `select`. Every `Fetch`/`FetchAsync`/`FetchChan` registers an `AbortController` on the calling scope — **in-flight requests abort automatically on teardown**; nothing to wire.

`GetFileBytes(id string) []byte` reads the first file of a `<input type="file">` (blocks until FileReader completes; `nil` on any failure). Chunked uploads: split the bytes, send each chunk with a `Content-Range` header.

### Decode[T] / Encode[T] — typed JSON without reflection

WASM components run under TinyGo, whose `reflect`/`encoding/json` are partial and discouraged. The CLI detects each `Decode[T]`/`Encode[T]` call at build time and replaces it with generated straight-line code — neither `reflect` nor `encoding/json` enters the binary.

```go
v, err := Decode[shared.EchoStruct](resp)   // explicit struct type arg, always
body := Encode[shared.EchoStruct](sent)     // typed marshal → request-body bytes
```

**Two non-negotiable rules:**

1. **The type argument is mandatory, explicit, and must be a struct.** The rewrite is a surgical byte-offset edit of the call site: `Decode(resp)` / `Encode(v)` / `Decode[int](resp)` all fail the build.
2. **The DTO package must be `net/http`-free.** A cross-package `Decode[T]`/`Encode[T]` compiles T's package into the WASM `main`; TinyGo cannot build `net/http`. Put shared DTOs in a leaf package (reference convention: `src/shared`) — struct declarations only, no `net/http`/`reflect`/`encoding/json` — imported by BOTH the server handler and the WASM page (the own-BFF pattern: one struct, two importers, zero drift).

```go
// src/shared/echo.go — TinyGo-safe DTO
type EchoStruct struct {
    Message  string     `json:"message"`
    UserName string     `json:"user_name"` // json key ≠ Go field name is honored both ways
    Nested   EchoNested `json:"nested"`
}
```

Decode coercion: missing key / `null` → zero value; `json:"name"` tag matched (falling back to field name); `json:"-"` skipped; nested structs/slices/pointers/string-keyed maps supported; embedded fields flattened; unknown keys ignored; **numbers decode through `float64` — int64 above 2^53 loses precision (send as strings)**. Encode: fields in struct order keyed by `json:` tag; `json:"-"` skipped; nil slices/pointers/maps → `null`; **`,omitempty` is a no-op — the field is always emitted**.

`resp.MapAny()` is the untyped read of the same body: object → `map[string]any`, number → `float64`; never panics; requires a JSON **object** root.

### HTMX vs Fetch — the discriminator

- Put server/BFF-rendered **HTML** in the DOM (so nested components boot) → `HTMX.Swap` / `HTMX.Ajax`.
- **Compute** on structured data (branch, sum, build a body) → `Fetch` + `Decode[T]` / `MapAny`.
- Third-party API needing a secret or lacking CORS → a server-side BFF route, then fetch your own route.

## What the agent CAN do

- Fetch from WASM callbacks and inspect status/headers/body via the structured `Response`.
- Send JSON (via `Encode[T]` + `FetchConfig.BodyBytes`) and binary bodies; upload files (`GetFileBytes`, chunked with `Content-Range`).
- Decode responses into a shared struct the server also marshals — the own-BFF, zero-drift pattern.
- Use `FetchAsync`/`FetchChan` for non-blocking/fan-out patterns with automatic per-scope cancellation.
- Read untyped JSON via `resp.MapAny()` for quick, schema-free access.

## What the agent CANNOT do

- Treat `Fetch`'s result as a string — it returns `(Response, error)`; read via `resp.Text()`/`resp.Bytes()`.
- Call `Fetch`/`GetFileBytes` at the top level of `ClientSideState` — they block.
- Decode with an inferred or non-struct type argument — build error.
- Put a DTO in a package that (transitively) imports `net/http` — TinyGo build failure. Use a leaf `net/http`-free package.
- Rely on `,omitempty` in `Encode` — fields are always emitted.
- Send exact int64 values > 2^53 as JSON numbers — precision loss through `float64`.
- Call `MapAny` on a non-object JSON root — it errors.
- Bypass CORS — cross-origin requests need server `Access-Control-Allow-*` headers.
- Read `HttpOnly` cookies (e.g. auth sessions) — the browser sends them, WASM cannot read them.
