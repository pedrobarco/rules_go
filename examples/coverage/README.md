# rules_go coverage example

A minimal, standalone Bazel module that demonstrates `bazel coverage` support
in rules_go and lets you inspect the instrumentation output.

This module uses `local_path_override` in `MODULE.bazel` to point at the
rules_go checkout that contains it (`../..`), so the commands below exercise the
**local** coverage code (instrumentation, test-main generation, and LCOV
conversion) rather than a released version.

## The code under test

`greeting.go` has two functions:

- `Greet` — called by `greeting_test.go`, so it shows up as **covered**.
- `Farewell` — never called by the test, so it shows up as **uncovered**.

## Coverage types

This example targets two structural coverage types:

1. **Line (statement) coverage** — was each source line executed at least once.
   This is what the Go SDK's `go tool cover` produces natively and what rules_go
   supports today. The `coverage.dat` shown below reports it via the
   `DA:`/`LH:`/`LF:` records.

2. **Branch (decision) coverage** — was every branch of each decision taken in
   *both* directions (e.g. the `if name == ""` in `Greet`/`Farewell` evaluated
   both true *and* false). This is strictly stronger than line coverage: you can
   execute every line while only ever exercising one side of a branch. LCOV
   represents it with `BRDA:`/`BRF:`/`BRH:` records.

### Running line coverage

Produced by the default `bazel coverage` run — see the run sections below. No
extra command is needed.

### Running branch coverage

> **Note:** `go tool cover` does not emit branch coverage, so this requires a
> dedicated branch-instrumentation action rather than a flag on the existing
> one. The intended invocation will be documented here once that action is
> wired into this example.

## Run coverage (LCOV, the default format)

```bash
bazel coverage //:greeting_test
```

The per-test report is written to
`bazel-testlogs/greeting_test/coverage.dat`. For this example it looks like:

```
SF:greeting.go
FNF:0
FNH:0
DA:7,2      # Greet: hit
DA:8,2
DA:9,1
DA:10,1
DA:11,1
DA:16,0     # Farewell: not hit
DA:17,0
DA:18,0
DA:19,0
DA:20,0
LH:5        # lines hit
LF:10       # lines found
end_of_record
```

`DA:<line>,<count>` is the execution count per line: `Greet` (lines 7-11) has
non-zero counts, while `Farewell` (lines 16-20) is `0`.

## Run coverage in Go's native profile format

```bash
bazel coverage --@rules_go//go/config:cover_format=go_cover //:greeting_test
cat bazel-testlogs/greeting_test/coverage.dat
```

This emits the familiar `go test -coverprofile` format (per-block counts):

```
mode: atomic
.../greeting.go:7.32,8.16 1 2
.../greeting.go:8.16,10.3 1 1
.../greeting.go:11.2,11.40 1 1
.../greeting.go:16.35,17.16 1 0
.../greeting.go:17.16,19.3 1 0
.../greeting.go:20.2,20.42 1 0
```

## A combined cross-language report

```bash
bazel coverage --combined_report=lcov //:greeting_test
```

Bazel's `lcov_merger` combines every `*.dat` file into a single report,
printed at the end as `.../_coverage/_coverage_report.dat`.

## How this maps to the rules_go coverage flow

1. **Detection** — `bazel coverage` sets `coverage_enabled`; `ctx.coverage_instrumented()`
   marks `:greeting` for instrumentation. The `go_test` rule advertises an
   `InstrumentedFilesInfo` so Bazel sets `COVERAGE_OUTPUT_FILE`/`COVERAGE_DIR`.
2. **Instrumentation** — inside the `GoCompilePkg` action, the rules_go builder
   runs the Go SDK's `go tool cover` on `greeting.go`, producing an instrumented
   `cover_*.go` that is compiled in its place. The `coverdata` dependency is
   injected automatically (visible via `bazel aquery 'deps(//:greeting_test)'`).
3. **Test main** — a generated `testmain.go` wires up the coverage hooks and
   points `-test.coverprofile` at `$COVERAGE_OUTPUT_FILE`.
4. **Runtime + conversion** — the test writes a Go coverprofile, then (for the
   default `lcov` format) `bzltestutil.ConvertCoverToLcov` rewrites it into the
   `coverage.dat` shown above.

See the source under `go/tools/builders/cover.go`,
`go/private/rules/test.bzl`, and `go/tools/bzltestutil/lcov.go`.
