# Branch coverage in rules_go — implementation plan

Status: **planned, not implemented**. This document captures the agreed design so
the work can be picked up later.

## Goal

Add **branch (decision) coverage** to rules_go as an opt-in, experimental feature,
running **alongside** the existing statement/line coverage so both land in one
LCOV `coverage.dat`. Demonstrate it in `examples/coverage`.

## Background (why a new instrumenter is needed)

`go tool cover` only does statement coverage by design (golang/go#28888 closed
won't-do; golang/go#70306 still open). So branch coverage requires a *different*
instrumenter, not a flag on the existing one.

## Locked decisions

1. **Tool source: vendor a minimal subset of `rillig/gobco`** (BSD-2-Clause —
   compatible; retain notice + attribution). gobco is `package main` and must be
   reshaped into a library and behaviorally forked, so vendoring beats an
   external-repo dependency + patch stack. We use gobco's **`branch` mode**
   (instrument the whole controlling condition of `if`/`for`/`switch`), giving
   branch/decision coverage.
2. **Runtime/registry: mirror the `//go/tools/coverdata` pattern** — a shared
   `//go/tools/branchcoverdata` package; each instrumented package registers its
   condition table + path mapping via an injected `init()` (like `registerCoverage`).
3. **Opt-in: new boolean build setting** `--@rules_go//go/config:experimental_branch_coverage`.

## Architecture (mirrors the statement-coverage flow)

```
GoCompilePkg action:  branch-instrument cover srcs (vendored gobco, branch mode)  <- new
                      + inject init() registering table with branchcoverdata      <- new
runtime:              //go/tools/branchcoverdata (counters + path map)            <- new
test exit:            converter -> LCOV BRDA/BRF/BRH (merged with DA records)      <- new
opt-in:               experimental_branch_coverage build setting -> builder flag  <- new
```

## What we vendor vs. drop (from gobco)

- **Keep/adapt:** the instrumenter core (`markConds` -> `findRefs` -> `prepareStmts`
  -> `replace` -> `callCover`, plus `codeGenerator` and switch/type-switch lowering),
  and the per-condition true/false counting from `templates/gobco_fixed.go`.
- **Drop:** `ParseDir` whole-directory driver, `TestMain` rewrite,
  `gobco_no_testmain_test.go` and `gobco_bridge_test.go` injection. rules_go owns
  test-main generation and the test-exit hook, so finalization goes there.

## Key risks / required adaptations

1. **Type resolution in the sandbox.** gobco's `resolveTypes` uses a *source*
   importer needing dependency source, which is absent in a Bazel action (only
   compiled archives exist), and it panics on error. Make type resolution
   **best-effort**: types are only needed for the rare `type MyBool bool` case;
   comparisons already yield `bool`. Degrade gracefully when unavailable.
2. **Multi-package linking.** Many instrumented packages link into one test binary,
   so use a shared `branchcoverdata` registry with per-package tables (gobco assumes
   a single in-package table).

## Implementation note: vendoring location (decided during Phase 0)

The plan originally proposed an importable `go/tools/builders/internal/gobco/`
package. That is **not viable**: the builder is compiled by `go_tool_binary`
(`go/private/sdk_build_defs.bzl`), which runs `go build {srcs}` with
`GO111MODULE=off` and **only the Go SDK** available — its srcs "must be in
'package main'". An in-tree importable package cannot be linked in.

Resolution: the vendored instrumenter lives as **`package main` files directly
in `go/tools/builders/`** (`gobco_instrumenter.go`, `gobco_codegen.go`),
alongside `cover.go`, added to the `builder_srcs` filegroup. The full
BSD-2-Clause notice + attribution is kept in `gobco_codegen.go`'s header. The
instrumenter uses only the standard library, so it compiles under the bootstrap
build.

## Phased steps

- **Phase 0 — Vendor. [DONE]** Added `go/tools/builders/gobco_instrumenter.go`
  and `gobco_codegen.go` (adapted gobco subset, branch mode, best-effort type
  resolution) as `package main`, with the BSD-2-Clause notice + attribution.
  Dropped the `ParseDir` driver, `TestMain` rewrite, and black-box bridge. No
  `repositories.bzl` / `MODULE.bazel` dependency entry. Unit test added in
  `cover_test.go` (`TestInstrumentForBranchCoverage`).
- **Phase 1 — Builder action. [DONE]** Added `instrumentForBranchCoverage(...)`
  and `generateBranchRuntime(...)` in `go/tools/builders/cover.go`. Added a
  `-experimental_branch_coverage` builder flag in `compilepkg.go`, threaded into
  `compileArchive`, where (when set) it branch-instruments the cover srcs and
  appends a per-package runtime (`gobco_runtime.go`, defining `GobcoCover` + a
  live counter slice) instead of running `go tool cover`. Starlark wiring of the
  flag remains Phase 4.
- **Phase 2 — Registry. [DONE]** Added `//go/tools/branchcoverdata` (analogue of
  `coverdata`): a `go_tool_library` exposing `RegisterCond(importPath, pos, code,
  counts)` and a `Conditions` map keyed by import path. `generateBranchRuntime`
  now imports it and injects an `init()` that registers the package's parallel
  `pos`/`code` tables plus the `gobcoCounts []uint32` slice **by reference**
  (`counts[2*i]`/`counts[2*i+1]` are condition i's true/false counts), so counts
  from every linked package are collectible at exit. `instrumentForBranchCoverage`
  now takes the `importPath`; `checkImportsAndBuildCfg` auto-injects the
  `branchcoverdata` archive into the importcfg when branch mode is on (erroring if
  the rule did not supply it — that provisioning is Phase 4). `cover_test.go`
  type-checks the generated package against a `branchcoverdata` stub.
- **Phase 3 — Collection + conversion. [DONE]** `go/tools/bzltestutil/lcov.go`
  now reads `branchcoverdata.Conditions` (via `collectBranchData`, grouped by the
  exec-root-relative source path encoded in each condition's position) and emits
  LCOV `BRDA:`/`BRF:`/`BRH:` records. Branch records are merged into the per-file
  `SF:` blocks alongside the existing `DA:` records (`emitLcovLines`), and files
  with branch data but no statement coverage get branch-only `SF:` blocks
  (`emitRemainingBranches`). Each condition emits two `BRDA` branches (0=false,
  1=true) keyed by condition index as the block, reporting `-` when the condition
  was never evaluated. To keep positions aligned with line coverage,
  `instrumentForBranchCoverage` now takes per-file `srcNames` and parses each
  source under that name. No `generate_test_main.go` change was needed: both exit
  paths already funnel branch emission through `bzltestutil` — the legacy
  `testdeps.CoverProcessTestDirFunc` calls `ConvertCoverToLcov` (now also emits
  branch-only output when no statement profile exists), and the Go 1.24+ `bincov`
  exit hook calls `ConvertCoverFromReaderToLcov`; both reach the shared
  `convertCoverToLcov`. `branchcoverdata` is now a dep of `bzltestutil`.
- **Phase 4 — Starlark wiring.** Add `experimental_branch_coverage` build setting;
  thread it `context.bzl` -> `archive.bzl` -> `compilepkg.bzl` as a builder flag;
  set up converter/env in `test.bzl`.
- **Phase 5 — Example + docs + tests.** Wire into `examples/coverage`, fill in the
  README "Running branch coverage" section with the real command, and add a
  `go_bazel_test` (mirroring `tests/core/coverage`).

## Acceptance criteria

- `bazel coverage --@rules_go//go/config:experimental_branch_coverage //:greeting_test`
  produces a `coverage.dat` containing both `DA:` (line) and `BRDA:`/`BRF:`/`BRH:`
  (branch) records.
- For `examples/coverage`: `Greet`'s `if name == ""` shows the **true** branch
  taken and the **false** branch not taken; `Farewell` shows both branches
  uncovered.
- Statement coverage output is unchanged when the flag is off.

## Files to touch (reference)

- `go/tools/builders/cover.go` — `instrumentForBranchCoverage` + `generateBranchRuntime`. [DONE]
- `go/tools/builders/gobco_instrumenter.go`, `gobco_codegen.go` — vendored
  `package main` instrumenter (BSD-2-Clause notice in `gobco_codegen.go`). [DONE]
- `go/tools/builders/compilepkg.go` — `-experimental_branch_coverage` flag threaded
  through `compileArchive`. [DONE]
- `go/tools/builders/BUILD.bazel` — vendored files added to `builder_srcs` and
  `cover_test`. [DONE]
- `go/tools/builders/cover_test.go` — `TestInstrumentForBranchCoverage`. [DONE]
- `go/tools/branchcoverdata/` — new registry package (`RegisterCond` + `Conditions`). [DONE]
- `go/tools/bzltestutil/lcov.go` — BRDA/BRF/BRH emission merged into the LCOV
  records; `lcov_test.go` covers it; `BUILD.bazel` gains the `branchcoverdata`
  dep. [DONE]
- `go/private/context.bzl`, `go/private/actions/archive.bzl`,
  `go/private/actions/compilepkg.bzl`, `go/private/rules/test.bzl` — wiring.
- `go/config/BUILD.bazel` — `experimental_branch_coverage` build setting.
- `examples/coverage/{README.md,BUILD.bazel}` — demo + docs.
