# The patched-TinyGo channel

Gothic compiles client-side WASM with a pinned, managed TinyGo toolchain. The
default is official **TinyGo `0.42.0`**, downloaded from
`github.com/tinygo-org/tinygo/releases`. It contains upstream PR #5545: real
`syscall/js` finalizers, finalizer-pressure collection at scheduler idle points,
finished asyncify stack and argument cleanup, and the per-block finalizer bitmap.

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
