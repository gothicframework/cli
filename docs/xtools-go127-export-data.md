# Go 1.27 export data breaks the CLI's package scanner (x/tools bump required)

> Status: **FIXED in-tree — `cli/go.mod` bumped to `golang.org/x/tools v0.53.0`,
> CLI version bumped to `v4.0.0-beta.11` (2026-11-21).** All verification re-run
> and green: `internal/build` + `internal/build/astx` + `astconfig` tests, a
> fresh `gothic init` + `gothic wasm` scaffold on Go 1.27.3, hot-reload boot,
> and the full chromium gate (191/191). Release = tag + install; the rest of
> this document is the runbook.
> Written 2026-11-21 after a full diagnosis during a family-finances session
> (reproduced on clean projects, root cause named, workaround verified).

## TL;DR

The CLI pins `golang.org/x/tools v0.44.0`, whose `gcimporter` reads Go
**export data** only up to **format version 4**. The Go **1.27.x** compiler emits
**format version 5**. On any machine whose active `go` is ≥ 1.27, every Gothic
command that type-checks user packages through `x/tools/go/packages` **without
`NeedDeps`** (the astx WASM scanner; `gothic wasm`, hot-reload's WASM stage, and
any other stage riding `astx.NewLoader`) dies with:

```
internal error: package "strings" without types was imported from "<user pkg>"
```

The real error (`cannot decode … export data version 5 is greater than maximum
supported version 4`) is swallowed by go/packages as a "secondary error", so the
surface message is misleading and sounds like a TinyGo problem. It is not —
**TinyGo never runs**; the fatal happens during the CLI's own scan, before the
managed TinyGo 0.43.0 toolchain is invoked.

**Fix:** bump `golang.org/x/tools` in `cli/go.mod` to a reader that understands
version-5 export data — **v0.53.0 verified working end-to-end** (see below).

## Who is affected

- **Every Gothic project** on a machine with `go` ≥ 1.27 active — not
  project-specific. Verified with a 2-file module (`fmt` import suffices) and
  with a fresh `gothic init` scaffold (fails with `core/wasm without types`).
- Machines with `go` ≤ 1.26.x are unaffected (version-4 data still emitted).
- Unaffected: `go build`/`go test` of user projects (they don't ride go/packages
  export-data loading in this mode), and the WASM builds themselves (TinyGo's
  loader is independent).

## Root cause, precisely

1. `cli/internal/build/astx.NewLoader(dir)` loads `"./..."` with
   `Mode: NeedName|NeedFiles|NeedCompiledGoFiles|NeedSyntax|NeedTypes|
   NeedTypesInfo|NeedImports|NeedModule` and `Env: os.Environ()+GOWORK=off`.
2. That mode contains **`NeedTypes` without `NeedDeps`**, so go/packages resolves
   dependency packages from **export data** (`go list -export=true -deps=true`)
   and parses it with x/tools' vendored `gcimporter`.
3. x/tools **v0.44.0**'s importer tops out at export-data **version 4**; Go
   1.27.3's `gc` writes **version 5**. Reading fails per dependency package.
4. go/packages records the failure as a per-package "secondary error" and leaves
   the package without types; the type-checker then hits the first import edge
   and `log.Fatalf`s from `x/tools/go/packages/packages.go` with the
   `without types was imported from` message. The genuine importer error is:

```
internal error in importing "fmt"
(cannot decode "fmt", export data version 5 is greater than maximum supported
version 4); please report an issue
```

   (Captured by calling `gcexportdata.Read` (v0.44.0) directly on a Go-1.27
   produced export file.)

5. Adding `NeedDeps` (source type-checking of deps) makes the same load pass —
   confirming the export-data path is the only broken leg.

## Reproductions (all verified 2026-11-21)

- Minimal: any module with a single package importing `fmt`/`strings`, loaded by
  the same `packages.Config` as `astx.NewLoader` → fails with
  `package "fmt" without types was imported from "<module>"`.
- Clean project: `gothic init <name> && gothic wasm` →
  `package "github.com/gothicframework/core/wasm" without types was imported
  from "<name>/src/topics"`.
- TinyGo standalone: NOT reproducible (toolchain is healthy; it never runs —
  a wrapper on `h.TinyGoBinary()` confirmed the fatal fires before exec).

## Fix (what ships)

Bump the dependency in `cli/go.mod`:

```bash
cd cli
GOFLAGS=-mod=mod go get golang.org/x/tools@v0.53.0
go build ./internal/... && go test ./internal/build/...   # spot gates
```

- **v0.53.0 verified**: with the bump, the astx-equivalent load (exact mode copy)
  is clean on Go 1.27.3, `gothic wasm` compiles real projects (2 components in
  the reporting app, 3 modules in a clean scaffold), and the full use-case test
  suite passes.
- If you want the minimal bump, bisect v0.45–v0.52 for the first version whose
  `gcimporter` accepts format version 5 (not done — v0.53.0 was the verified
  candidate at hand).
- `wasm_binary.go` / the TinyGo pin are **not** part of this fix; keep the
  TinyGo `0.43.0` pin as is.

## Release runbook (next CLI beta)

Follow the standard checklist (KB `01_architecture.md` §8.1). The steps that
matter for this change:

1. `cli/go.mod`: `golang.org/x/tools v0.44.0` → the chosen fixed version.
2. `GOWORK=off` filtered build (§8.1 step 2 — the `internal/scaffold` wildcard
   trap) + the pins-agreement `diff` between `cli/go.mod` and
   `internal/cmd/version.go` `FrameworkModules`.
3. Re-run the WASM-path tests that exercise `astx` (`internal/build/astx/...`,
   `internal/build/wasm_*`), then the Playwright release gate if tagging a beta
   that ships to users.
4. Tag the CLI beta. Until users get the new binary, the machine-level
   workaround below keeps affected machines moving.

## Interim machine-level workaround (no release needed)

Rebuild the installed `gothic` binary from a checkout whose `cli/go.mod` carries
the bumped `x/tools` (keep the released pin in git — the workaround is build-time
only):

```bash
cp ~/go/bin/gothic ~/go/bin/gothic.bak-<suffix>          # backup
cd <cli checkout> && GOFLAGS=-mod=mod go get golang.org/x/tools@v0.53.0
go build -o ~/go/bin/gothic ./cmd/gothic                 # restore go.mod after
```

Applied 2026-11-21 on the reporting machine (backup suffix
`gothic.bak-xtools044`); `gothic wasm` went green immediately, hot-reload boots
and stays up.

## Related notes

- The failure can stay hidden behind an earlier pipeline error: on the reporting
  project the routes stage aborted first (`src/api` missing — core v1.6.1-beta.3
  requires the folder), so the scanner was never reached until the folder
  existed. If you see `failed to walk through api: lstat ./src/api`, fix the
  folder first — the scanner fatal may surface next.
- Symptom fingerprint for future searches: `without types was imported`,
  `export data version 5`, `gothic wasm internal error`, `TinyGo strings
  without types`.
