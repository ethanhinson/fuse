<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0084 — HarnessSpec proto and public validator](../../changes/archive/2026-10-30-0084-harnessspec-proto-and-public-validator.md)**
<!-- docket:backlink:end -->

# HarnessSpec proto and public validator - plan (recorded after the fact)

This plan is recorded after implementation. The work was built and merged as PR #101
(merge commit c4327b0) before docket could adopt its workspace, so this file documents the
task sequence as it was actually executed, one commit per task.

## Tasks

1. **Path-carrying config rules** (0981576). Expose the existing `internal/config` rules as
   `Config.Problems()` with YAML paths, keep `Validate()` byte-identical, and extract
   `config.FromYAML` from the loader's trusted merge with no behavior change.
2. **Shared model-alias checks** (821c594). Move model-alias reference checks out of
   `cmd/fuse` into `internal/configcheck` so the CLI and the spec validator share them.
3. **The `fuse.harness.v1` proto** (f7a1ab5). Define `HarnessSpec` over the client-settable
   surface only, UI-hint field options in `options.proto`, protovalidate shape rules, and the
   `HarnessService.ValidateHarness` RPC. Generate committed Go and TS stubs with a drift guard.
4. **Mapping and public validator** (0457a36). `internal/harnessconfig` maps spec to and from
   `config.Config`; `github.com/ethanhinson/fuse/harness` exposes `Validate`, `ValidateShape`,
   and the RPC handler, translating config paths to spec field paths.
5. **TS SDK export** (943d0cc). Export the harness types and `validateHarnessShape` from
   `sdk/ts`, driven by the shared `shape_cases.json` parity fixture.
6. **Review fixes** (84bd275). Close the independent review's findings, including the
   silently dropped `default` model alias and the secret-ref/credential-header rules.

## Verification

`make test`, `make lint`, `make proto` with zero drift, sdk/ts tests, `-race` on the new
packages, and a byte-for-byte comparison of CLI error output before and after. All green, and
all GitHub CI checks passed on PR #101.
