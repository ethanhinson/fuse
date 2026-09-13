<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0076 — fuse server deployment — a docker-compose stack and a Helm chart over the released image (not an operator)](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0076-fuse-server-helm-chart-compose-stack.md)**
<!-- docket:backlink:end -->

# fuse server deployment — compose stack + Helm chart — results

Change #76 · plan: `docs/superpowers/plans/2026-09-13-fuse-server-helm-chart-compose-stack-plan.md`
Branch: `feat/fuse-server-helm-chart-compose-stack` · base: `origin/main` @ b966b5e · tip: `a325ad8`

## What shipped

All 14 plan tasks, plus 9 in-branch fixes for every finding a deep review returned.

- **Server prerequisites:** `event.Pinger` optional seam on both stores; `fuse version` reports the
  compiled backends on a third line; unauthenticated `/healthz` + `/readyz` with a cached readiness
  gate; SIGTERM + a bounded graceful drain; `FUSE_INSTANCE_ID` env fallback; a `fuse healthcheck`
  subcommand; `-tags pgstore` on release builds and `make build`/`install`.
- **`deploy/compose/`** — a dev stack that `include`s the observability stack, adds `fuse` +
  Postgres, and carries an opt-in `docker-socket` sibling service.
- **`deploy/charts/fuse/`** — the chart, its guards, and a `helm lint` + `helm template` matrix test.
- **CI** — chart publish on tag; `helm lint`/`template`/`package` + `docker compose config` on every PR.
- **`docs/deploying.md`** — the operator guide.

## Test evidence — what actually ran

**`make test` (the full suite, the gate): GREEN** on `a325ad8` — exit 0, zero FAIL lines, 42
packages ok. Run twice independently: once by the build, once by the implementer after the fix loop.

| Suite | Status | Notes |
|---|---|---|
| `go test ./...` (42 pkgs) | **green** | via `make test` |
| `observability-validate` | **green** | now `go run ./deploy/observability` (package form — see deviations) |
| `deploy/charts` (16 tests) | **green, 0 skips** | ran against real `helm`; a skip here is NOT a pass |
| `deploy/observability` (compose validator) | **green** | new `validateCompose` entry point |
| `deploy/smoke` | **green** | the smoke client's own unit tests |
| `go test -race ./cmd/fuse/` | **green** | the drain + readiness work is concurrent; race was mandatory |
| `docker compose config` (default + `--profile docker-socket`) | **green** | parses; invariants asserted below |
| `helm lint` | **green** | but see the warning below — lint does NOT fire template guards |
| `make build && ./fuse version` | **`backends: fsstore,pgstore`** | the empirical proof the build tag took |

### Deliberately NOT run — operator-only, and nobody should read these as tested

`make compose-smoke` and `make helm-smoke` are **operator-only by design** (the spec and plan both
say so), and **this build did not run them**. No `docker compose up`, no `kind` cluster, no
`helm install`, no live LLM calls of any kind.

`helm`, `kind`, `docker`, and `kubectl` were all present and the docker daemon was up, so the
skip-clean paths were **not** exercised as skips either — they are written to print a loud `SKIP:`
and exit 0 when tooling is absent (modelled on `observability-compose-smoke`), but that behaviour is
unverified on a machine that lacks the tools.

**So these acceptance criteria from the spec remain UNVERIFIED and need an operator:**

1. `docker compose ... up` yields a server answering `/readyz` 200, Prometheus scraping it, and a
   `loop.start` over the SDK succeeding with the dev token.
2. `helm install` on kind with `postgres.dev.enabled` reaches Ready; a rolling restart keeps at
   least one Ready replica throughout and a client reattaches to its loop afterwards.
3. Two replicas sharing loops through a real Postgres (a loop started on one observable from the
   other).
4. SIGTERM drain end-to-end against a real container: readiness flips 503 before the listener
   closes, in-flight unary calls complete, the process exits inside the grace period with the egress
   proxy directory removed. *(The ordering and the awaited-drain property are unit-proven; the
   container-level timing is not.)*
5. The actual `helm push` to `oci://ghcr.io/ethanhinson/charts` — CI packages it on every PR but
   only a real tag pushes.

### A trap worth remembering

**`helm lint` exits 0 without firing the chart's `fail` guards.** It validated a chart that would
refuse to render with no auth and no DSN. The load-bearing verification is `helm template` plus the
Go matrix test — do not later "simplify" CI to lint-only. This is the
`declarative-config-validator-accepts-typos` learning (recorded from #0082, same `.goreleaser.yaml`)
recurring in a second tool: a linter checks structure, not whether your value gates the behavior you
think it gates. The `-tags pgstore` change was verified the same way — empirically, via
`./fuse version`, not by `goreleaser check`.

## Review findings — 11, all addressed

A deep-rung review (selected by rule: highest build profile was `premium`; the 5609-changed-line
modifier was already at the cap) returned 3 blockers, 5 important, 3 minor. **Every one was fixed
in-branch**; none was deferred, reverted, or merely recorded. The suite gate after the fix loop was
green on the first run, so no revert was triggered.

### Blockers — all three were invisible to a green suite

1. **The graceful drain never drained** (`2195739`). `srv.Shutdown` closes listeners first, so the
   blocked `Serve` returned immediately, `serveNetWithOptions` returned nil, and `main`'s `os.Exit`
   killed the still-draining goroutine. The drain tests passed only because they run in a test
   process that stays alive — nothing exercised the return-to-`main` path. Fixed by making the drain
   awaited (a `drained` channel the return blocks on). Deliberately **no** second timeout: any bound
   would either never fire or truncate the drain being fixed.
2. **`/readyz` returned 200 with Postgres down, in every deployment this change produces**
   (`6604953`). `deps.DurableStore` is wrapped in `projectingDurableStore` before reaching the
   readiness gate, and that wrapper embeds only `CommittedDurableStore`, so it does not satisfy
   `event.Pinger` and the probe took its "nothing to probe ⇒ ready" path. Both shipped configs
   enable metrics, so the wrapper is always applied. This defeated the readiness gate **and** the
   chart's `maxUnavailable: 0` guarantee. Fixed by forwarding `Ping` through the wrapper, with a
   `var _ event.Pinger` compile-time guard so it cannot be deleted as apparently-dead code.
   An audit found no second swallowing wrapper in the tree.
3. **The chart's Service routed Connect traffic to the Postgres pod** (`3910a5d`). Label selectors
   match on subset: the server's `{name, instance}` selector was satisfied by the dev-Postgres pod,
   which carried those two labels plus a component label. The Service, Deployment, PDB and
   NetworkPolicy all captured it. Fixed by narrowing the server's selectors with
   `component: server`. **`Deployment.spec.selector` is immutable**, so this would be a breaking
   upgrade on an installed release — the chart is unreleased (`0.0.0-dev`, never published), which is
   why no migration path was written.

### Important

4. **Concurrent readiness probes serialized behind a 2s Ping** (`c9eb613`) — and the chart's kubelet
   `timeoutSeconds` is also 2, so a second waiter had already burned its whole budget before
   acquiring the lock. A slow-but-alive store would evict the pod on latency alone. Fixed with a
   singleflight shape rather than by decoupling the two timeouts, which would only narrow the window
   and would couple a Go constant to a chart value across two independently-editable files. Cold
   start deliberately makes the first caller wait for a real answer rather than seeding "ready"
   (which would advertise an instance whose store may be dead).
5. **The metrics listener was outside the drain** (`176e6e0`). Fixed by shutting it down inside the
   same `drainCtx`, after the Connect server so a final scrape can land. Bounded best-effort: if the
   budget is already spent, the metrics port degrades to pre-fix behaviour — never a grace overrun.
6. **`config.existingSecret` + `auth.tokens` silently discarded the tokens** (`58ab4e8`), where the
   analogous postgres combination already refused. Now guarded. Two adjacent combinations were
   examined and deliberately left unguarded, each for a stated reason: `existingSecret` +
   `allowDevToken` is not discardable input (it makes the chart *omit* auth so the server synthesizes
   its own token, ADR-0034) and is pinned by a test; `existingSecret` + a populated `config:` map is
   real but **unguardable at this layer**, because the chart ships `config:` already populated and
   Helm cannot distinguish an operator override from the shipped defaults after merge — documented in
   `values.yaml` instead.
7. **The chart's `alerts.yml` copy was hand-maintained and its only drift gate needed `helm`**
   (`7a12538`) — so on a machine without helm the gate skipped and a stale copy would ship silently.
   Now generated by `make charts-sync-alerts` (idempotent) and guarded by a **helm-independent** Go
   test that parses both files directly. No latent drift existed: the rule bodies already matched
   byte-for-byte.
8. **`docker compose --profile docker-socket up` bare collided on published ports** (`b2baf13`).
   Compose cannot express "exclude this service while that profile is active" — a service with no
   `profiles:` key is always in the default set — so the collision cannot be designed away. The
   `ports:` block was lifted out of the `&fuse` anchor and repeated verbatim on both services so the
   conflict is visible in the file, and the old comment (which wrongly claimed the profiled service
   "does not contend for the published ports") was corrected. `docs/deploying.md` gained the warning.

### Minor (batched, `a325ad8`)

9. Dead `stdout` parameter on `runHealthcheck`. 10. `deploy/charts/validate_test.go` split
multi-document YAML on `"\n---"`, which also splits inside a block scalar — replaced with a real
`yaml.Decoder`. This mattered because that test's stated purpose is that "a Pod added later cannot
slip through," and a mis-split could silently shrink the set it walks.
11. A `0.0.0.0-dev` typo in a `Chart.yaml` comment.

## Deviations from the plan

- **`fuse-docker-socket` is a sibling service via a YAML anchor**, not the plan's profile-gated
  mount: Compose profiles gate services, not individual mounts, so the plan's shape is
  inexpressible. Consequence (the port collision) is documented above and in the file.
- **Compose healthcheck is `["CMD","/fuse","healthcheck"]`**, not the plan's bare
  `["/fuse","healthcheck"]` — Compose v5.1.2 rejects the bare form. Still exec form, no shell.
- **`observability-validate` became `go run ./deploy/observability`** (package form). The plan
  predicted this: the old single-file `go run …/validate.go` would not have compiled the new
  validator, so the check would never have run in the gate it was added to.
- **`azure/setup-helm@v4` added to CI's `test`, `unit-race`, and `release-dry-run` jobs** — without
  it the chart tests silently skip in CI, which is the same green-skip failure the repo's
  `release-dry-run` job exists to prevent.
- **`deploy/smoke/` is a new package** not in the plan's file list — the shared readiness +
  `loop.start` client both operator smokes call. It has its own unit tests.
- **`sandbox.mode: kubernetes` is refusal-only.** `sandbox.kubernetes.minVersion` stays `""`, so the
  guard refuses before any manifest is consulted. `deploy/k8s/sandbox-rbac.yaml` is #75's
  deliverable and was neither created nor referenced. #75 supplies the manifest and pins
  `minVersion` when it lands.

## Follow-ups (reported, not minted — `auto_capture` is disabled)

1. **The untagged-build "must stay pgx-free" invariant has no test.** `cmd/fuse/durable_backend.go`'s
   header asserts `go list -deps` must be pgx-free without the tag, but unlike the sibling
   import-direction gates in `internal/runtime` and `internal/event`, nothing enforces it. A `chore`.
2. **An optional-interface promotion convention.** Blocker 2's class of bug — a wrapper silently
   failing a `Pinger`-style assertion — is prevented by a `var _ Iface = Wrapper{}` assertion next to
   every wrapper. Worth a convention, not just the one fix.
3. **`observability-compose-smoke` (pre-existing, `Makefile:150`) exits 2, not 0, when docker is
   absent** — its SKIP guards print and then the recipe fails. A one-line fix for that target's owner;
   the new smoke targets fold their guards into a single `set -e` recipe shell to avoid it.
4. **The ServiceMonitor's selector is also a subset of the postgres-dev Service's labels.**
   Harmless today (that Service exposes only `postgres`/5432 and the endpoint scrapes `metrics`, so
   nothing resolves), but it would start scraping if postgres-dev ever gains an exporter sidecar.
5. **Minor 10's fix is correct by construction but not proven end-to-end through `helm template`** —
   helm's own marshaling re-indents nested block scalars, so no `--set` path could stage a column-0
   `---`. Demonstrated synthetically against the same decoder call instead. Stated plainly rather
   than claimed as a live proof.

## For the human at the merge gate

- Run the two operator smokes (`make compose-smoke`, `make helm-smoke`) before trusting the
  deployment paths; the unverified acceptance criteria above are exactly what they cover.
- Read **blocker 3's immutability note** if you have any pre-existing install of this chart from a
  branch build — `Deployment.spec.selector` cannot be changed by `helm upgrade`.
- One commit subject (`12689e9`) says **"docket 0083 task 11"** — a typo for 0076. Left unamended
  rather than rewriting published history.
- Commit trailers are inconsistent across the branch (`Claude Opus 5 (1M context)` vs
  `Claude Fable 5.1`): the harness's attribution guidance changed mid-run and explicitly replaced the
  earlier instruction. Cosmetic.
