---
name: gothic-invariants
description: "Gothic v3 laws: naming/validation rules, deterministic resource names, append-only enums, versioning model, generated-file rules, WASM wire contracts. Consult before changing anything with a rule."
applies_to:
---

# Gothic invariants

The laws every tool, template, and generated artifact must hold. Each entry names the rule, where it is enforced, and what breaks when violated.

## Philosophy — minimal JavaScript

Gothic's design bet: the interactive parts of a web app are built with the Go/WASM runtime, not with JavaScript. The framework ships the tools — ClientSideState observables, Topics, typed `Fetch`/`Decode`, the htmx engine compiled into `core.wasm`. When a capability can be expressed in Go/WASM, that is the expected implementation:

- Do not scaffold hand-written `.js` glue files and serve them. The tooling (routes, `ClientSideState`, topics, htmx attributes) covers wiring without any page JS.
- The accepted escape hatch is a **vendored third-party library** with no Go/WASM equivalent: load its upstream `<script>` (it attaches to `window.htmx` if it is an htmx extension), and prefer documenting the reason it must be JS over porting logic into a bespoke script file.
- Reaching for `.js` before checking the Go-side tool list (`Fetch`, `Decode`, typed JSON, topics, htmx triggers/publish, preload) is the recurring anti-pattern this law exists to stop.

## Naming and validation rules

| Rule | Regex / law | Enforced at | Breaks if violated |
|---|---|---|---|
| Project name | `^[a-z0-9]+(-[a-z0-9]+)*$` — kebab, no underscore/uppercase | `gothic init` prompts/derives | Invalid project name propagates into every AWS resource name and fails bucket validation at deploy |
| Stage name | `^[a-zA-Z0-9]+$` — alphanumeric only | `gothic deploy` entry | A dash breaks the deterministic `project-stage-suffix` names and the state key `gothic/<project>/<stage>/…` |
| S3 bucket name | `^[a-z0-9][a-z0-9.\-]{1,61}[a-z0-9]$` | checked on `ProjectName-stage-suffix` before any AWS call | AWS rejects the bucket / assets target an invalid bucket |
| URL param dirs | `var_paramName/` → `{paramName}` | router + WASM path scanner | Params read empty in handlers |
| Go major suffix | a module's last segment `vN` is skipped for project-name derivation (`github.com/you/my-app/v3` → `my-app`) | `gothic init` | — |

## Deterministic naming

- **ResourceSuffix** = `hex(crc32.IEEE(goModuleName + ":" + projectName))` — 8 lowercase-hex chars, derived from the globally unique module path. Every machine and every fresh clone derives the SAME suffix; there is no stored app-id.
- **ECR repo, asset bucket, Lambda** are each named `project-stage-suffix` (identical strings) — per stage, never shared. A shared ECR repo would let a `dev` teardown delete images a `prod` Lambda still pulls.
- **State bucket + lock table** are `project-state-suffix` / `project-lock-suffix` — shared across all stages of a project; state keys are `gothic/<project>/<stage>/terraform.tfstate`.
- **`deploy` refuses an undeclared stage** (a typo would stand up a parallel stack); **`delete` is permissive** with a warning. `--action` is restricted to `deploy|delete`; `-s` defaults `dev`.

## Versioning model

- The CLI module is `github.com/gothicframework/cli/v4` (the `/v3` path element carries the generation). The libraries `core`, `components`, `middlewares` are suffixless modules versioning independently at v1.x.
- A suffixless module cannot be tagged `/vN` (N≥2) — a v2 of `core` would need a `/v2` module path.
- Release tag order: `core` → `components` → `middlewares` → `cli` → e2e (downstream modules require upstream tags).
- `gothic init` is the only command that writes framework versions into a user's `go.mod` (pinned per module); nothing else rewrites those pins. The written pins are the version truth — a local `replace` directive may redirect where a module resolves from, but the `require` line still states the intended version.

## Append-only enums

- `StaticFilesMode`: `CDN(0)`, `DISK(1)`, `EMBEDDED(2)` — **never reorder or renumber**; ordinals are baked into projects' compiled configs and the CLI keys off them. Never rename existing identifiers (a legacy-name alias may be kept in the parser for not-yet-updated sources).
- `ConfigType`: `{ISR, STATIC, DYNAMIC}`; `HttpMethod`: `{GET, POST, PUT, PATCH, DELETE}`. `DefaultConfig` = STATIC+GET; `DefaultApiConfig` = DYNAMIC+GET.

## Generated files — never hand-edited

`src/routes/routes_gen.go`, `src/topics/topic_gen.go`, the root `gothic_embed.go` (EMBEDDED static mode), and everything under `.gothicCli/` are outputs of `gothic build` / `gothic wasm` / deploy. Edit the source `.templ`/`.go`/`infra/` files and regenerate. WASM cache: `.gothicCli/wasm-cache.json` (per-symbol hashes); templ cache: `.gothicCli/templ-cache.json`.

## Import-path hygiene

- v3 user code never imports `github.com/felipegenef/...` — that org's module was the v2 path; `gothic migrate-v3` rewrites every such import to the `github.com/gothicframework` org modules, and the rewrite is idempotent (unknown subpaths fall back to `core`, so no legacy path is ever emitted).
- `core/router` is internally `package helpers` but is always imported aliased `routes`.

## WASM wire contracts (load-bearing)

- Every binary codec frame opens with a single `WireVersion` byte (`1`); a decoder built for one version rejects any other — bumping it is an intentional, irreversible wire break.
- Topic events are named `gothic:topic-req:<key>:<field>` / `gothic:topic:<key>:<field>` / `gothic:topic-online:<key>`; the htmx alias binds `topic:` → `gothic:topic:`. Renaming these strings breaks every compiled page.
- Topic payloads stay under ~100 kB: WASM32 linear memory grows but never shrinks.
- Callbacks are scope-isolated through `data-gothic-scope` + a per-scope registry; a placement removed from the DOM must be torn down (cleanups → listeners → refs → halt), or the whole WASM instance leaks.

## What the agent CAN do

- Rely on deterministic naming: the same module path + project name always derives the same suffix and resource names.
- Validate inputs against the exact regexes above before any name reaches AWS.
- Add NEW enum values by appending (documenting the ordinal) — and version-gate any code that depends on the new value.
- Treat the `require`-block versions as the authoritative library versions when checking capability or compatibility.

## What the agent CANNOT do

- Reorder or renumber an append-only enum (`StaticFilesMode`, `ConfigType`, `HttpMethod`) — compiled projects carry the ordinals.
- Hand-edit generated files (`routes_gen.go`, `topic_gen.go`, `gothic_embed.go`, `.gothicCli/*`).
- Emit or keep `github.com/felipegenef/...` import paths in v3 code.
- Share an ECR repo across stages, or make `ResourceSuffix` non-deterministic.
- Rename a topic event string or change the codec frame layout without a wire-version break.
- Ship hand-written glue `.js` files when the capability exists in the Go/WASM layer — check typed `Fetch`, `Decode`, topics and htmx first; a vendored third-party `<script>` is the last-resort escape hatch, not the default.
- Bypass the declared-stage check with a typo'd stage name — it is the guard against parallel stacks.
