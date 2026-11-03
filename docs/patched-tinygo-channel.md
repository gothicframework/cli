# The patched-TinyGo channel

Gothic compiles client-side WASM with a pinned, managed TinyGo toolchain. The
default is official **TinyGo `0.43.0`**, downloaded from
`github.com/tinygo-org/tinygo/releases`. It contains the upstream runtime work
Gothic depends on: real `syscall/js` finalizers, finalizer-pressure collection
at scheduler idle points, finished asyncify stack and argument cleanup, and the
per-block finalizer bitmap (all from 0.42.0), plus the precise GC layouts and
precise global scanning added in 0.43.0.

The patched channel remains available for a future fix that Gothic needs before
an official TinyGo release contains it. This document describes how a
`<base>-gothic.<n>` version routes to the maintainer fork and how to return to an
official release afterward.

## Version convention and download routing

A patched TinyGo build uses `<base>-gothic.<n>`, where `<base>` is the upstream
TinyGo semver and `<n>` is the Gothic patch iteration:

```text
0.43.0-gothic.1
0.43.0-gothic.2
```

`cli/internal/build/wasm_binary.go` chooses the download host from the version
string:

- `^\d+\.\d+\.\d+-gothic\.\d+$` downloads from
  `github.com/felipegenef/tinygo/releases`.
- Every other version downloads from `github.com/tinygo-org/tinygo/releases`.

Archive naming is identical on both hosts. Official releases are verified
against the `sha256:` digest in GitHub's release metadata; patched fork releases
are verified against their `checksums.txt`. Binaryen is always downloaded from
its own upstream release.

Projects can override the managed version with `WasmTinyGoVersion`:

```go
var Config = gothic.Config{
	WasmTinyGoVersion: "0.43.0-gothic.1",
}
```

`WasmBinary` bypasses the download and points directly to a local TinyGo binary.
The CLI obtains that binary's matching `TINYGOROOT`, so it can test a patch
before release assets exist.

## Runtime compatibility

Gothic serves one stock TinyGo `wasm_exec.js` shim unconditionally. There is no
runtime capability profile, shim selection, or `GOTHIC_WASM_EXEC` environment
variable. These were removed because choosing the compiler at build time and the
shim in a separate server process allowed incompatible pairs and double-free
failures.

A TinyGo version used by Gothic must therefore provide working `syscall/js`
finalizers. Verify that property empirically before changing the default or
publishing a patched build.

## Cutting a patched release

1. Base the fork branch on the intended upstream tag and apply the required fix.
2. Build locally with `make tinygo` and verify a `GOOS=js GOARCH=wasm` program.
3. Run the fork's Linux, macOS, and Windows release workflows.
4. Publish all supported archives plus `checksums.txt` under a leading-`v` tag.

The CLI expects these asset names:

| Platform | Asset |
|---|---|
| linux/amd64 | `tinygo<version>.linux-amd64.tar.gz` |
| linux/arm64 | `tinygo<version>.linux-arm64.tar.gz` |
| darwin/amd64 | `tinygo<version>.darwin-amd64.tar.gz` |
| darwin/arm64 | `tinygo<version>.darwin-arm64.tar.gz` |
| windows/amd64 | `tinygo<version>.windows-amd64.zip` |

Each fork `checksums.txt` entry uses the standard `sha256  filename` format. The CLI
rejects a download whose checksum is absent or mismatched.

## Shipping and retiring a patch

To ship a patched toolchain:

1. Set `tinyGoVersion` in `cli/internal/build/wasm.go` to the published
   `<base>-gothic.<n>` version.
2. Regenerate `core/corewasm/core.wasm` with that exact compiler, then regenerate
   `core/runtimeassets`.
3. Run the CLI tests and `make gate-soak` in `e2e-tests`.
4. Publish a beta before promoting the framework release.

When an official release contains the complete fix, verify the tagged source and
release assets, then replace the default with its bare semver. Bare versions
route back to upstream automatically; no runtime profile or shim change exists.

## TinyGo 0.42.0 migration

TinyGo PR #5545 merged on 2026-08-26 and shipped in official TinyGo `0.42.0` on
2026-09-01. The tag includes all behavior Gothic previously consumed from
`0.42.0-gothic.4`, including the finalizer bitmap and asyncify cleanup.

Gothic therefore uses `0.42.0` as its managed default. The `-gothic.*` routing
code remains dormant for future upstream gaps.

## TinyGo 0.43.0 migration

0.43.0 moved the runtime to precise GC layouts and precise global scanning on
every platform. Gothic already compiles the core and every component with
`-gc precise`, so the bump needs no flag change: it is the bare version pin, a
fresh toolchain download, and a full gate run on artifacts rebuilt with it. The
pre-flight gate below is the acceptance check.

Measured against 0.42.0 on the release gate, 0.43.0 keeps every leak and timing
test green while cutting component heap sharply (for example the large-payload
Fetch spec went from a 48 MB high-water to 4 MB) and roughly doubling combined
topic/mux throughput with no change in application code.

## Why 0.44.0 is not pinned

0.44.0 passes the functional suite but fails `stress-fetch-htmx-remount-leak`
deterministically: one `Uint8Array` survives per Fetch+HTMX re-mount even after
an explicit `HeapProfiler.collectGarbage` (20 over 20 re-mounts; the budget is
8, and 0.42.0/0.43.0 both measure 0). A 50-re-mount ramp measured 50 retained
buffers, so the growth is linear, not a plateau.

The cause is upstream PR #5930 ("scan only the live part of asyncify stacks"):
it keeps every task with a heap stack on a list until it exits, so a goroutine
blocked after its component's teardown stays on that list and its stack keeps
being scanned. Dead locals in that stack — including the Fetch response buffer —
become roots. Reverting the change in the 0.44.0 runtime removes the leak (Δ0
over 50 re-mounts). Upstream tracks the regression in PR #6041, still open, and
`dev` matches 0.44.0 in these files, so no release carries the fix yet.

0.44.0's throughput and heap numbers match 0.43.0 within noise, so the pin stays
on 0.43.0 until #6041 lands.

## Pre-flight gate

For any future toolchain change:

1. Reinstall the workspace CLI with
   `go install github.com/gothicframework/cli/v4/cmd/gothic`.
2. Run `gothic wasm install` and confirm `gothic wasm version` reports the
   intended managed version.
3. Remove the E2E WASM cache so every component recompiles with that toolchain.
4. Run `make gate-soak`; it includes functional, memory/timing, and long-running
   JS-WASM bridge tests on fresh server processes.
5. Confirm `finalizer-jsvalue-leak`, `stress-fetch-largepayload`,
   `codec-ctsetdeep-repro`, and `codec-bridge-leak-extreme` pass.
