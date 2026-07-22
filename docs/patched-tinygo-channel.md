# The patched-TinyGo channel

Gothic compiles each page's client-side WASM with a **pinned, managed TinyGo
toolchain** (default `0.41.1`, downloaded once into the CLI cache). This document
is for maintainers who need to ship a **critical TinyGo fix that is already
merged upstream but not yet in an official TinyGo release** — temporarily routing
Gothic to a patched TinyGo build hosted on a fork, then reverting to the official
release once it ships. The mechanism is generic: it works for any such patch, not
one specific fix.

Two independent primitives make this work, plus a runtime capability signal.
Both are **off by default** — a stock checkout builds against official TinyGo
`0.41.1` with Gothic's manual-GC wasm_exec runtime.

---

## 1. Version convention and download routing

A patched TinyGo build is pinned by a version string in the
`‹base›-gothic.‹n›` form, where `‹base›` is the upstream TinyGo semver the patch
is built on and `‹n›` is the Gothic patch iteration:

```
0.41.1-gothic.1      # first patched build on top of TinyGo 0.41.1
0.41.1-gothic.2      # a re-cut of the same base
0.42.0-gothic.1      # a patched build on top of a later base
```

The CLI resolves the TinyGo download host **from the version string alone**
(`cli/internal/build/wasm_binary.go`):

- A version matching `^\d+\.\d+\.\d+-gothic\.\d+$` is downloaded from the
  maintainer fork **`github.com/felipegenef/tinygo/releases`**.
- Every other version is downloaded from upstream
  **`github.com/tinygo-org/tinygo/releases`**.

Nothing is hardcoded to a specific patch tag, so a future `0.42.0-gothic.3`
routes to the fork automatically while a bare official `0.42.0` stays on
upstream. The release-asset filename scheme
(`tinygo‹version›.‹platform›.tar.gz` / `.zip`) and the `checksums.txt`
verification are unchanged — only the host differs. Binaryen (`wasm-opt`) always
comes from its own upstream and is never rerouted.

Pin the version with `wasmTinyGoVersion` in `gothic.config.go`:

```go
var Config = gothic.Config{
	// ...
	WasmTinyGoVersion: "0.41.1-gothic.1",
}
```

`WasmBinary` (json `wasmBinary`) still overrides the tinygo **binary path**
directly and bypasses the download entirely; it carries its own `TINYGOROOT`.
Use it to point at a local build during development.

---

## 2. The capability profile (safe by default)

A patched toolchain can change **which runtime the browser needs**. The load-
bearing case is the wasm_exec shim served at `/_gothic/wasm_exec.js`:

- **Manual-GC shim** (`core/wasmexec/wasm_exec.js`, the default) carries a
  `_releaseValue` reclamation, called from `_makeFuncWrapper`, that force-frees
  dead `js.Value` ref-table slots. It is **required** on a TinyGo whose
  `syscall/js` has **no finalizers** (0.41.1's `runtime.SetFinalizer` is a
  no-op), because without it the `_values` table grows unbounded under repeated
  callbacks.
- **Stock shim** (`core/wasmexec/wasm_exec_stock.js`) is the same file with the
  `_releaseValue` method and its `_makeFuncWrapper` call removed. It is
  **required** on a TinyGo whose `syscall/js` **does** provide finalizers,
  because there the manual reclamation and the real `finalizeRef` both manage the
  `_values` table and free the same slot twice — a use-after-free that surfaces
  as `call to released function` under concurrent fetches.

Which shim a toolchain needs is a property of that specific **build**, and it
**must never be inferred from the version number** — a future bare `0.42.0`
might ship with or without the fix. The CLI therefore consults an explicit,
verified table (`cli/internal/build/wasm_profile.go`):

```go
type toolchainProfile struct {
	StockWasmExec bool // room for future per-patch toggles
}

var knownToolchainProfiles = map[string]toolchainProfile{
	"0.41.1":          {StockWasmExec: false}, // no finalizers → manual GC
	"0.41.1-gothic.1": {StockWasmExec: true},  // PR #5521 finalizers → stock
}

func ProfileFor(version string) toolchainProfile {
	if p, ok := knownToolchainProfiles[version]; ok {
		return p
	}
	return toolchainProfile{StockWasmExec: false} // SAFE DEFAULT
}
```

Any unknown/unverified version returns the **safe default** — the manual-GC
runtime. On a not-yet-verified toolchain that can at worst leave a slow leak,
never the use-after-free crash a wrongly-assumed stock shim would cause. **A
missing or wrong row fails safe.**

### Adding a verified version row

1. Build or obtain the toolchain and confirm — on real hardware, by running the
   pre-flight gate below — whether its `syscall/js` provides finalizers.
2. Add one row to `knownToolchainProfiles` with the observed capability:
   `StockWasmExec: true` if it has finalizers, `false` if it does not.
3. Never guess from the version — verify, then add the row.

---

## 3. How the shim selection reaches the server

The wasm_exec shim is served by the **running server** (the `core` runtime), not
written by the CLI build — so the build-time capability decision reaches the
server process through an environment variable, exactly as `GOTHIC_PROVIDER=AWS`
does for request signing:

- `core/wasmexec` embeds **both** shims and picks one **once at process start**:
  `GOTHIC_WASM_EXEC=stock` selects the stock shim; unset (the default) selects
  the manual-GC shim.
- The CLI sets that variable from `ProfileFor(version).StockWasmExec`:
  - **`gothic hot-reload`** adds it to the app server it launches.
  - **`gothic deploy`** injects it into the Lambda environment.
- For a self-hosted binary you run yourself (DISK/EMBEDDED static modes), set
  `GOTHIC_WASM_EXEC=stock` in the server environment when you have pinned a
  finalizer-carrying toolchain.

`WasmExecEnvKey` (`cli/internal/build/wasm_profile.go`) is the single source of
truth for the variable name, shared by the CLI (which sets it) and
`core/wasmexec` (which reads it).

---

## 4. Cutting a patched TinyGo release on the fork

The fork `felipegenef/tinygo` reuses TinyGo's own release build workflows
(`.github/workflows/linux.yml`, `build-macos.yml`, `windows.yml`), which already
emit release assets named exactly as the CLI expects. To cut a build:

1. **Base and patch.** Create a branch off the upstream tag you are basing on
   and apply the merged-but-unreleased fix.

2. **Build.** Locally, `make tinygo` produces `build/tinygo` with `TINYGOROOT`
   at the repo root; verify it compiles a small `GOOS=js GOARCH=wasm` program.
   For distributable cross-platform tarballs, run the fork's build workflows.

3. **Asset names.** The CLI downloads, per platform:

   | Platform        | Asset filename                                  |
   | --------------- | ----------------------------------------------- |
   | linux/amd64     | `tinygo‹version›.linux-amd64.tar.gz`            |
   | linux/arm64     | `tinygo‹version›.linux-arm64.tar.gz`            |
   | darwin/amd64    | `tinygo‹version›.darwin-amd64.tar.gz`           |
   | darwin/arm64    | `tinygo‹version›.darwin-arm64.tar.gz`           |
   | windows/amd64   | `tinygo‹version›.windows-amd64.zip`             |

   where `‹version›` is the bare version, e.g. `0.41.1-gothic.1`
   (`tinygo0.41.1-gothic.1.linux-amd64.tar.gz`). Also publish a
   `checksums.txt` with a `sha256  ‹filename›` line per asset — the CLI verifies
   each download against it.

4. **Tag and publish.** The git tag carries a leading `v` (matching upstream, and
   the CLI's `…/releases/download/v‹version›/…` URL):

   ```bash
   gh release create v0.41.1-gothic.1 \
     --repo felipegenef/tinygo \
     --title "0.41.1-gothic.1" \
     --notes "TinyGo 0.41.1 + syscall/js finalizers (upstream PR #5521)" \
     tinygo0.41.1-gothic.1.linux-amd64.tar.gz \
     tinygo0.41.1-gothic.1.linux-arm64.tar.gz \
     tinygo0.41.1-gothic.1.darwin-amd64.tar.gz \
     tinygo0.41.1-gothic.1.darwin-arm64.tar.gz \
     tinygo0.41.1-gothic.1.windows-amd64.zip \
     checksums.txt
   ```

---

## 5. Shipping the patch through Gothic

1. Pin `wasmTinyGoVersion` to the patched tag in the framework's own
   `gothic.config.go` fixtures / defaults.
2. If the patch changes runtime behavior, add the verified capability row
   (Section 2) — e.g. `StockWasmExec: true` for a finalizer-carrying build.
3. Cut a **beta** of the affected modules, run the pre-flight gate, then promote
   to stable.

---

## 6. Retiring the fork pin

When an official TinyGo release is verified to contain the fix:

1. Pin `wasmTinyGoVersion` to the **official** version (or clear it to fall back
   to the bundled default once the bundled default is that version).
2. Add the official version's capability row (e.g. `StockWasmExec: true` once the
   official release carries finalizers).
3. Drop the `‹base›-gothic.‹n›` row.

Download routing returns to upstream automatically — the official version does
not match the `-gothic.‹n›` pattern.

---

## Worked example: syscall/js finalizers (upstream PR #5521)

TinyGo `0.41.1`'s `runtime.SetFinalizer` is a no-op, so `syscall/js` never
finalizes `js.Value` bridge slots and they leak. Gothic's default
`wasm_exec.js` compensates with a manual `_releaseValue` reclamation.

Upstream PR #5521 implements real finalizers but is not yet in an official
release. A TinyGo built with it (`felipegenef/tinygo`, tagged
`0.41.1-gothic.1`) provides `finalizeRef` — so pairing it with the manual
`_releaseValue` shim would double-free `_values` slots and crash with
`call to released function` under concurrent fetches. That build is therefore
paired with the **stock** shim via its capability row (`StockWasmExec: true`).

Flow: pin `wasmTinyGoVersion: "0.41.1-gothic.1"` → the CLI downloads the fork
build and, via the capability row, sets `GOTHIC_WASM_EXEC=stock` for the server
→ the server serves the stock shim → finalizers reclaim slots and the leak and
the crash are both gone. When PR #5521 ships in an official TinyGo, pin that
official version, give it a `StockWasmExec: true` row, drop the `-gothic.1`
row, and routing returns to upstream.

**Safety property.** Because unverified toolchains always fall back to the
manual-GC runtime, a wrong or missing capability row can never silently
reintroduce the leak (it is fixed by the toolchain finalizers) nor the
use-after-free crash (which only a wrongly-assumed stock shim causes).

---

## Pre-flight gate

Before cutting a beta, validate a pinned toolchain end-to-end against the
Playwright suite in `e2e-tests`:

1. In `e2e-tests/gothic.config.go`, set `WasmTinyGoVersion` to the patched tag
   (drives the capability profile) and, until the fork release is published,
   `WasmBinary` to the local patched `tinygo` binary (bypasses the download).
2. Reinstall the CLI: `go install github.com/gothicframework/cli/v3/cmd/gothic`.
3. Clear caches: remove `e2e-tests/.gothicCli/wasm-cache.json` and
   `e2e-tests/public/wasm/*`.
4. `cd e2e-tests && make dev`, wait for `localhost:3000`, run
   `npx playwright test`.
5. Confirm the served `/_gothic/wasm_exec.js` matches the expected variant
   (stock has no `_releaseValue`), the memory-leak stress test grows ~0 slots,
   and the concurrency/large-payload fetch tests pass.
6. Revert the two `gothic.config.go` overrides — the committed tree stays on the
   stable line (bundled `0.41.1`, manual-GC shim).
