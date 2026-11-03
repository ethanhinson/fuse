<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0128 — Loop artifact store adapter for spilled tool output and compaction segments](../../changes/active/0128-loop-artifact-store-adapter-for-spilled-tool-output-and-comp.md)**
<!-- docket:backlink:end -->

# Loop artifact store adapter for spilled tool output and compaction segments — Implementation Plan

> **For the build role:** `docket-build` executes this plan one task at a time. A tier agent runs each task under the `docket-build-task` contract: focused test first, then implement, verify, self-review, and one commit. Steps use checkbox (`- [ ]`) syntax. The full suite runs once, in Task 11.

**Goal:** Add one `artifact.Store` seam, scoped by (tenant, loop). It has a local-filesystem backend, a Postgres backend (behind `pgstore`), and an in-memory backend, and all three pass one conformance suite. Spilled tool output and compaction segments become artifacts addressed by opaque `art://<tenant>/<loop>/<kind>/<id>` refs. `read_file`, `grep`, `list_directory` and `segment_read` resolve refs through the loop's own store. The runtime binds every loop's registry to the store, and the loop server passes a real segment sink. `tools.SetSpillDir` and `tools.SetSegmentsDir` are deleted.

**Architecture:**
- `internal/artifact` holds the agent-free core: `Kind`, `Ref`, `Meta`, `Store`, errors, `ParseRef`, `NewID`, `Binding`, `MemStore` and the context helpers. Its only fuse dependency is `internal/event`, for `TenantID` and `LoopID`.
- `internal/artifact/artifacttest` holds the conformance suite.
- `internal/artifact/fsstore` is the filesystem backend.
- `internal/artifact/pgstore` is the Postgres backend (`//go:build pgstore`).
- `tools.Registry` carries an `artifact.Binding`, which `Clone` and `Subset` copy. `Registry.Execute` stamps the binding onto the tool's context and spills through it. The file tools read `art://` refs through `artifact.BindingFrom(ctx)`.
- `internal/segment` stays agent-free. It gains `ParseSegment` and a store-backed `Source`.
- `internal/segment/artsink` is the agent-dependent writer (learning `break-import-cycle-with-agent-free-subpackage`). `fssink` becomes a thin constructor over `artsink` + `artifact/fsstore`.
- `runtime.Deps.Artifacts` is the process store. If it is nil, `runtime.New` installs a `MemStore` with a 64 MiB per-loop cap. `StartLoop` binds every loop's registry to `(tenant, loopID)` immediately before `BuildAgent`, on both start and resume.
- At the composition root, `selectArtifactStore` returns pgstore when a Postgres DSN is configured and fsstore under `~/.fuse/artifacts` otherwise.

**Tech stack:** Go, pgx/v5 plus testcontainers (tagged tests only), `compress/gzip`.

**Spec (authoritative):** `docs/superpowers/specs/2026-11-03-loop-artifact-store-adapter-for-spilled-tool-output-and-comp-design.md`

---

## Ground rules (every task)

- **Worktree:** `/Users/ethanhinson/dev/fuse/.worktrees/loop-artifact-store-adapter-for-spilled-tool-output-and-comp` (branch `feat/loop-artifact-store-adapter-for-spilled-tool-output-and-comp`). All paths below are relative to it.
- **Focused runs:** `go test ./<pkg> -run '<Name>' -count=1`. Tagged Postgres tests: `go test -tags pgstore ./internal/artifact/pgstore/... -count=1`. They skip cleanly without Docker, unless `FUSE_TEST_PG_DSN` is set. The full suite is `make test`, run in Task 11 only. Every task must leave `go build ./... && go vet ./...` and `go build -tags pgstore ./...` green.
- **No live model traffic.** Tests use scripted `httptest` gateways and the existing helpers (`toolCallsReply(call{...})`, `argsJSON(...)` in `cmd/fuse/workflow_activation_test.go`). Never point a test at a real provider, and never at Anthropic.
- **Naming:** no code, comment, test name, doc or commit message may name a specific host application. Say "callers".
- **Proto:** this change touches no `.proto`. If one is touched after all, regenerate with `PATH=$HOME/go/bin:$PATH make proto`.
- **Commit subjects** use the repo's `<area>: <summary>` style.
- **Tenant isolation is asserted positively**, per learning `identity-derived-path-collides-case-insensitively`. Tenant A writes a sentinel, and tenant B's read must fail with `ErrNotFound`. Negative "escape" tests alone prove nothing.
- **Enumerate every construction site, never a list from this plan** (learnings `per-instance-resource-needs-teardown-on-every-early-return`, `security-knob-inert-at-composition-root`). Before a wiring task's commit, grep for `runtime.Deps{`, `runtime.New(`, `wiring.childTools(`, `buildChildAgent(` and `buildAgentCore(`, and confirm that each site reaches the new seam.

---

## File map

| File | Change |
|---|---|
| `internal/artifact/artifact.go` (new) | `Kind`, `Ref`, `Meta`, `Store`, errors, `ParseRef`, `ParsePrefix`, `NewID`, `MaxArtifactBytes` |
| `internal/artifact/binding.go` (new) | `Binding`, `WithBinding`, `BindingFrom` |
| `internal/artifact/memstore.go` (new) | `MemStore`, `WithPerLoopCap` |
| `internal/artifact/artifacttest/conformance.go` (new) | `RunStoreConformance` |
| `internal/artifact/*_test.go` (new) | ref, binding and memstore tests |
| `internal/artifact/fsstore/store.go` (+ test, new) | filesystem backend |
| `internal/artifact/pgstore/{store.go,schema.sql,store_test.go}` (new, `pgstore` tag) | Postgres backend |
| `internal/tools/registry.go`, `spill.go`, `spill_test.go`, `registry_test.go` | binding and spill through the store; `SetSpillDir` deleted |
| `internal/tools/read.go`, `grep.go`, `list.go` (+ tests), `internal/tools/artifact_ref.go` (new) | `art://` resolution |
| `internal/runtime/inproc.go` (+ new `artifacts_test.go`) | `Deps.Artifacts`, MemStore default, StartLoop binding |
| `internal/segment/reader.go`, `source.go` (new) (+ tests) | `ParseSegment`, `Source`, `ArtifactSource` |
| `internal/segment/artsink/sink.go` (+ test, new) | store-backed `agent.SegmentSink` |
| `internal/segment/fssink/sink.go`, `sink_test.go` | thin wrapper over artsink + artifact/fsstore |
| `internal/tools/segment_read.go`, `segment_read_test.go`, `segment_read_wiring_test.go` | resolve through the binding; `SetSegmentsDir` deleted |
| `internal/tui/agents_segments.go`, `agents_model.go`, `shell_model.go` (+ tests) | `WithSegments(segment.Source)` |
| `cmd/fuse/shell.go`, `main.go`, `research_probe.go`, `runtime_binding.go` | CLI wiring |
| `cmd/fuse/durable_backend.go`, `durable_backend_pg.go`, `durable_backend_fs.go`, `loop_server.go` | `selectArtifactStore`; loop-server `Deps.Artifacts`; per-loop segment sink |
| `cmd/fuse/segment_gateway_test.go`, `segment_adversarial_test.go`, `segment_wiring_test.go` | seed and read through the store |
| `cmd/fuse/artifact_acceptance_test.go` (new) | the two cross-runtime acceptance tests |
| `docs/designs/context-management.md` | spill/segment section now describes artifact refs |

---

## Task 1: `internal/artifact` core, MemStore and conformance suite

**Tier:** standard

**Files:** create `internal/artifact/artifact.go`, `binding.go`, `memstore.go`, `artifact_test.go`, `memstore_test.go`, `internal/artifact/artifacttest/conformance.go`.

- [ ] **Step 1: Write the failing tests first.** Cover the following:
  - `artifact_test.go`:
    - `TestRefStringRoundTrip`: `Ref{Tenant:"acme", Loop:"0190abc", Kind:KindSpill, ID:"x1"}.String() == "art://acme/0190abc/spill/x1"`, and `ParseRef` returns it unchanged.
    - `TestParseRefRejects`: refuses all of: missing scheme, wrong segment count, empty segment, unknown kind, a segment with `/` or `..`, a leading `.`, a segment over 128 bytes, and any character outside `[A-Za-z0-9._-]`.
    - `TestParsePrefix`: `art://t/l` and `art://t/l/spill/` parse with an empty Kind/ID as appropriate.
    - `TestNewIDUniqueAndSortable`: 10k ids are unique, and ids from successive calls sort ascending.
  - `memstore_test.go`:
    - `TestMemStoreConformance` calls `artifacttest.RunStoreConformance(t, func(t *testing.T) artifact.Store { return artifact.NewMemStore() })`.
    - `TestMemStorePerLoopCap`: `NewMemStore(WithPerLoopCap(100))`. A 60-byte Put succeeds. A second 60-byte Put on the same (tenant, loop) returns `ErrQuotaExceeded`. The same Put on another loop succeeds. An overwrite of the first id with 30 bytes succeeds, because the old size is released.
  - `TestBindingScopesOpen` (in `artifact_test.go`): a `Binding{Store, Tenant:"a", Loop:"l1"}`. `Open` of a ref whose tenant or loop differs returns `ErrNotFound`, and the store's `Open` is never reached. Use a spy store to check this. This re-asserts the full key on every resolve (learning `cache-over-tenant-scoped-source-reassert-key-on-hit`).
- [ ] **Step 2: Implement `artifact.go`:**

```go
package artifact

type Kind string

const (
	KindSpill        Kind = "spill"
	KindSegment      Kind = "segment"
	KindSegmentIndex Kind = "segment-index"
)

// Ref is an opaque, tenant-scoped artifact address. Its String form is what a
// transcript carries; it never names a filesystem path.
type Ref struct {
	Tenant event.TenantID
	Loop   event.LoopID
	Kind   Kind
	ID     string
}

func (r Ref) String() string // "art://<tenant>/<loop>/<kind>/<id>"
func (r Ref) Validate() error // ErrInvalidRef: every segment matches ^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$, kind known; ID may be "" only where Put allocates
func ParseRef(s string) (Ref, error)    // exactly four segments
func ParsePrefix(s string) (Ref, error) // tenant+loop required; kind and id optional (for list_directory)
const Scheme = "art://"
func IsRef(s string) bool { return strings.HasPrefix(s, Scheme) }

type Meta struct {
	Ref       Ref               `json:"ref"`
	Size      int64             `json:"size"`       // uncompressed bytes
	CreatedAt time.Time         `json:"created_at"` // set by the store on every Put
	Tool      string            `json:"tool,omitempty"`
	Turn      int               `json:"turn,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

// Store is blob-shaped on purpose so an object-storage backend can follow.
type Store interface {
	// Put writes r under ref. ref.ID == "" makes the store allocate a unique id (NewID).
	// A non-empty ID REPLACES any existing artifact under that ref. Bodies larger than
	// MaxArtifactBytes are refused with ErrTooLarge and nothing is written.
	Put(ctx context.Context, ref Ref, r io.Reader, meta Meta) (Meta, error)
	// Open returns the uncompressed body. A missing ref, or one under another tenant, is ErrNotFound.
	Open(ctx context.Context, ref Ref) (io.ReadCloser, Meta, error)
	// List returns the (tenant, loop, kind) artifacts ordered by CreatedAt, then ID.
	List(ctx context.Context, tenant event.TenantID, loop event.LoopID, kind Kind) ([]Meta, error)
}

const MaxArtifactBytes = 16 << 20
var (
	ErrNotFound      = errors.New("artifact: not found")
	ErrInvalidRef    = errors.New("artifact: invalid ref")
	ErrTooLarge      = errors.New("artifact: exceeds size cap")
	ErrQuotaExceeded = errors.New("artifact: loop quota exceeded")
)
func NewID() string // fmt.Sprintf("%016x-%s", time.Now().UnixNano(), 8 random bytes hex): lowercase, sortable
```

  Every store normalizes `ref.Tenant` with `event.NormalizeTenant` before validating it.

- [ ] **Step 3: Implement `binding.go`.** `type Binding struct{ Store Store; Tenant event.TenantID; Loop event.LoopID }` with these methods:
  - `Valid() bool`: true when Store != nil and Loop != "".
  - `Put(ctx, kind, r, meta) (Meta, error)`: builds `Ref{Tenant, Loop, kind, ""}`.
  - `Open(ctx, ref)`: returns `ErrNotFound` unless `NormalizeTenant(ref.Tenant)==NormalizeTenant(b.Tenant) && ref.Loop==b.Loop`.
  - `PutID(ctx, kind, id, r, meta)` and `List(ctx, kind)`.
  - Context helpers `WithBinding(ctx, b) context.Context` and `BindingFrom(ctx) (Binding, bool)`.
- [ ] **Step 4: Implement `memstore.go`.** Use a mutex-guarded map keyed by the full `Ref`. Store bodies uncompressed. Read through `io.LimitReader(r, MaxArtifactBytes+1)` to enforce the cap. Implement `Option`/`WithPerLoopCap(n int64)`, which tracks total bytes per (tenant, loop) and releases the old size on overwrite. Export `const DefaultMemLoopCap = 64 << 20`.
- [ ] **Step 5: Implement `artifacttest.RunStoreConformance(t *testing.T, newStore func(t *testing.T) artifact.Store)`.** Use subtests, each on a fresh store:
  1. **RoundTrip:** Put an explicit id, then Open. The bytes match, and Meta.Size, Tool, Turn, Labels and Ref match.
  2. **AllocatedIDsUniqueUnderConcurrency:** 50 goroutines Put with `ID:""`. All 50 returned ids are distinct, all Open, and `List` returns 50.
  3. **ListOrderAndScoping:** three Puts with explicit increasing ids, separated by small sleeps, in (t1, l1, spill). One in (t1, l1, segment), one in (t1, l2, spill), one in (t2, l1, spill). `List(t1,l1,spill)` returns exactly the three, in order.
  4. **WrongTenantOpenIsNotFound:** tenant `ta` writes a sentinel. Open of the same loop/kind/id under `tb` is `errors.Is(err, ErrNotFound)`.
  5. **OverwriteReplaces:** Put id `index` twice. Open returns the second body, and List shows it once.
  6. **CompressionTransparent:** a 1 MiB highly compressible body round-trips byte-identically. A body that is itself gzip bytes also round-trips byte-identically: the store must not gunzip caller data.
  7. **SizeCap:** a `MaxArtifactBytes+1` body returns `ErrTooLarge`, after which Open returns `ErrNotFound`.
  8. **OpenMissing:** returns `ErrNotFound`.
  9. **InvalidRefRefused:** Put of a ref with an `..` segment returns `ErrInvalidRef`.
- [ ] **Step 6: Verify:** `go test ./internal/artifact/... -count=1 -race`. Commit `artifact: tenant-scoped store seam, MemStore and conformance suite`.

---

## Task 2: `artifact/fsstore` filesystem backend

**Tier:** premium. This is the tenant-isolation boundary on a host filesystem. A path-derivation mistake is a cross-tenant read that every containment test still passes.

**Files:** create `internal/artifact/fsstore/store.go`, `store_test.go`.

- [ ] **Step 1: Write the failing tests:**
  - `TestFSStoreConformance` runs `RunStoreConformance` with `fsstore.New(t.TempDir())`.
  - `TestFSStoreLayoutAndGzipAtRest`: the body file is at `<base>/<tenant>/<loop>/<kind>/<id>`, and its first two bytes are `0x1f 0x8b`.
  - `TestFSStoreRefusesUnsafeSegments`: an uppercase tenant (`Acme`), a leading-dot id, and `..` are all refused with `ErrInvalidRef` on Put and Open. Refuse; never lowercase. Tenants `acme` and `Acme` must never share a tree.
  - `TestFSStoreTenantSiblingsDisjoint`: tenants `a` and `b` resolve to distinct direct children of `<base>`.
  - `TestFSStoreAtomicWrite`: after a successful Put, no `*.tmp*` files remain. A Put whose reader errors midway leaves no body and no meta, so Open returns `ErrNotFound`.
  - `TestFSStoreCrossInstance`: two `fsstore.New(sameDir)` values. A writes and B reads. This is the "shared store, different runtime" property.
- [ ] **Step 2: Implement:**
  - `New(baseDir string) *Store`.
  - On top of `Ref.Validate`, fsstore refuses any uppercase character in tenant, loop or id (learning `identity-derived-path-collides-case-insensitively`).
  - The body lives at `<base>/<tenant>/<loop>/<kind>/<id>` and is gzip-compressed at rest. The meta JSON lives at `<base>/<tenant>/<loop>/<kind>/.meta/<id>.json`. Ids cannot start with `.`, so `.meta` never collides with an id.
  - Write path: `MkdirAll(0o700)`. Stream the body through gzip into a `CreateTemp` file in the same directory, counting the uncompressed bytes against `MaxArtifactBytes`. `Sync`, `Close`, then `Rename`. Write the meta the same way after the body. On any error, remove the temp files.
  - `Open` gunzips. It returns `ErrNotFound` when the body or meta file is missing.
  - `List` reads the `.meta/*.json` files and sorts them by (CreatedAt, ID). A missing directory returns an empty list, not an error.
- [ ] **Step 3: Verify:** `go test ./internal/artifact/fsstore -count=1 -race`. Commit `artifact/fsstore: filesystem backend, gzip at rest, refuse unsafe ids`.

---

## Task 3: `artifact/pgstore` Postgres backend

**Tier:** standard

**Files:** create `internal/artifact/pgstore/store.go`, `schema.sql`, `store_test.go` (all `//go:build pgstore`).

- [ ] **Step 1: Write the test.** Copy the testcontainers harness pattern from `internal/event/pgstore/store_test.go`: `getHarness`, `dsnOrSkip`, a `FUSE_TEST_PG_DSN` override, `postgres:16-alpine`, and skip when Docker is absent. `TestPGStoreConformance` runs `RunStoreConformance`. Each `newStore` call does `Open` and then `TRUNCATE artifacts`, and registers `Close` in `t.Cleanup`. Add `TestPGStoreBodyCompressed`: query `body` directly and assert that it starts with the gzip magic.
- [ ] **Step 2: Implement:**
  - `schema.sql`: `CREATE TABLE IF NOT EXISTS artifacts (tenant_id text NOT NULL, loop_id text NOT NULL, kind text NOT NULL, id text NOT NULL, meta jsonb NOT NULL, body bytea NOT NULL, created_at timestamptz NOT NULL, PRIMARY KEY (tenant_id, loop_id, kind, id));`
  - `Open(ctx, dsn) (*Store, error)`: creates its own `pgxpool`, then applies the schema under `pg_advisory_xact_lock(<a constant distinct from the event store's schemaLockKey>)`, as `internal/event/pgstore.applySchema` does. Also provide `Close()`.
  - `Put`: read through `LimitReader(MaxArtifactBytes+1)`, gzip in memory, then `INSERT ... ON CONFLICT (tenant_id, loop_id, kind, id) DO UPDATE SET meta=EXCLUDED.meta, body=EXCLUDED.body, created_at=EXCLUDED.created_at`. Allocate the id with `artifact.NewID()` when it is empty.
  - `Open`: select by the full key, then gunzip. `pgx.ErrNoRows` becomes `ErrNotFound`.
  - `List`: `ORDER BY created_at, id`.
- [ ] **Step 3: Verify:** `go test -tags pgstore ./internal/artifact/pgstore -count=1` (green or skip-clean). Then `go build ./... && go vet ./...` untagged, and confirm that `go list -deps ./cmd/... ./internal/... | grep pgx` stays empty untagged. Commit `artifact/pgstore: Postgres backend behind the pgstore tag`.

---

## Task 4: Registry binding and spill through the store; delete `SetSpillDir`

**Tier:** standard

**Files:** modify `internal/tools/registry.go`, `internal/tools/spill.go`, `internal/tools/spill_test.go`, `internal/tools/registry_test.go`, `cmd/fuse/main.go`, `cmd/fuse/shell.go`, `cmd/fuse/research_probe.go`.

- [ ] **Step 1: Write the failing tests in `spill_test.go`.** Replace the dir-based tests.
  - Build the registry with `NewRegistry().WithArtifacts(artifact.NewMemStore(), "acme", "loop1")` and register a fake tool that returns 50 KiB with a unique middle needle.
  - `TestSpillThroughStore`:
    - The output keeps the head and tail.
    - The marker contains `Full output saved as artifact art://acme/loop1/spill/`.
    - The output contains no `/` path outside that ref. Concretely, assert no `os.TempDir()` prefix.
    - `List(acme, loop1, spill)` returns one artifact with `Tool == "<fake tool name>"`, and its body contains the needle.
  - `TestSpillWithoutBindingTruncatesOnly`: the marker says `Re-run the tool with a narrower scope`, and nothing is written.
  - `TestSpillStoreFailureFallsBack`: a store whose Put errors falls back to the narrower-scope marker.
  - `TestSmallOutputPassesThrough`.
  - In `registry_test.go`:
    - `TestCloneAndSubsetCarryBinding`: both copies return the same `Artifacts()`.
    - `TestExecuteStampsBinding`: a fake tool sees `artifact.BindingFrom(ctx)` equal to the registry's binding.
- [ ] **Step 2: Implement:**
  - Add a field `art artifact.Binding` to `Registry`, guarded by `mu`.
  - `func (r *Registry) WithArtifacts(store artifact.Store, tenant event.TenantID, loop event.LoopID) *Registry` sets the binding (tenant normalized) and returns r.
  - `func (r *Registry) Artifacts() artifact.Binding`.
  - `Clone` and `Subset` copy the binding.
  - In `Execute`, take the binding under the RLock, set `ctx = artifact.WithBinding(ctx, b)` when `b.Valid()`, and then call `res.Output = spillOutput(ctx, b, name, res.Output)`.
- [ ] **Step 3: Rewrite `spill.go`.** Keep `spillThreshold`, `spillKeep`, the head/tail logic and `sanitizeToolName` (it is still used for Meta.Tool).
  - Delete `spillDir`, `spillMu`, `spillSeq`, `SetSpillDir`, `sweepSpillDir`, `spillMeta`, `parseSpillName`, `writeSpill` and `spillMaxAge`. Retention is out of scope.
  - `spillOutput(ctx, b, toolName, output)` puts the full output through `b.Put(putCtx, artifact.KindSpill, strings.NewReader(output), artifact.Meta{Tool: toolName})`, where `putCtx` is `context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)`. This way a timed-out tool call still records its output.
  - Marker: `" Full output saved as artifact %s — read_file it with start_line/end_line or grep it with that ref, or spawn_agent a child to analyze it; do NOT read the whole artifact back."`
  - Without a binding, or on a Put error: `" Re-run the tool with a narrower scope for the elided middle."`
  - Remove the now-unused `internal/archive` import only if nothing else in the file uses it. `read_file` still uses `archive.Open` for ordinary paths.
- [ ] **Step 4: Fix the three CLI callers.** Delete the `tools.SetSpillDir(...)` lines in `cmd/fuse/main.go`, `shell.go` and `research_probe.go`. Recovery for those bindings is restored in Tasks 8 (shell) and 9 (one-shot and probe). Their outputs still truncate in the meantime. Run `grep -rn "SetSpillDir" --include='*.go' .` and confirm it is empty.
- [ ] **Step 5: Verify:** `go test ./internal/tools -count=1 -race && go build ./... && go build -tags pgstore ./...`. Commit `tools: spill oversized results to the loop's artifact store; drop SetSpillDir`.

---

## Task 5: Runtime binds every loop's registry; `Deps.Artifacts` with a MemStore default

**Tier:** standard

**Files:** modify `internal/runtime/inproc.go`; create `internal/runtime/artifacts_test.go`.

- [ ] **Step 1: Write the failing tests:**
  - `TestStartLoopBindsRegistryToLoopKey`: `Deps.Artifacts = artifact.NewMemStore()`. `BuildAgent` captures `toolReg.Artifacts()` and returns a scripted agent, following the existing test fakes in this package (e.g. `runtime_test.go`). Assert `Tenant == NormalizeTenant(cfg.Tenant)`, `Loop == LoopID(handle loop id)`, and `Store == deps.Artifacts`. Repeat with `cfg.Tenant = "acme"`.
  - `TestStartLoopNilArtifactsDefaultsToCappedMemStore`: with `Deps.Artifacts` nil, the captured binding is `Valid()`, and a Put of `artifact.DefaultMemLoopCap+1` total bytes on that loop fails with `ErrQuotaExceeded`.
  - `TestResumeRebindsSameLoop`: follow the existing resume test setup in `resume_test.go`. The resumed loop's registry binding carries the original loop id and tenant.
  - `TestRuntimeBindingOverridesBindingSuppliedOne`: the runtime is the authoritative, last writer (learning `runtime-deps-field-overwrites-builder-injection`). A `NewToolRegistry` that pre-binds `(other, other)` still ends up bound to the loop's key.
- [ ] **Step 2: Implement:**
  - Add a `Deps` field with a doc comment: `Artifacts artifact.Store // per-(tenant, loop) artifact store for spill + segments; nil ⇒ an in-memory store with a per-loop cap`.
  - In `New(deps)`: `if deps.Artifacts == nil { deps.Artifacts = artifact.NewMemStore(artifact.WithPerLoopCap(artifact.DefaultMemLoopCap)) }`. Grep for the constructor, `func New(`, in `internal/runtime`.
  - In `StartLoop`, after `toolReg` is chosen and immediately before `buildAgent(store, tree, cfg.ModelID, toolReg)`: `toolReg.WithArtifacts(r.deps.Artifacts, tenant, event.LoopID(rootID))`. This covers the orchestrator registry, harness-resolved factories and resume, since they all pass through this one site.
  - `internal/artifact` imports only `internal/event`, so `import_direction_test.go` stays green. Run it.
- [ ] **Step 3: Verify:** `go test ./internal/runtime -count=1 -race`. Commit `runtime: bind each loop's tool registry to its artifact store`.

---

## Task 6: `read_file`, `grep` and `list_directory` resolve `art://` refs

**Tier:** standard

**Files:** create `internal/tools/artifact_ref.go`; modify `internal/tools/read.go`, `grep.go`, `list.go`, and their tests.

- [ ] **Step 1: Write the failing tests** (`read_test.go`, `grep_test.go`, `list_test.go`). Bind with `artifact.WithBinding(ctx, Binding{MemStore, "acme", "l1"})` and seed a 3000-line spill artifact.
  - `read_file {path: ref, start_line: 1500, end_line: 1502}` returns exactly those lines.
  - An unranged read is windowed with the existing footer.
  - A ref under tenant `other` returns `IsError` with text containing `artifact not found`, and the same text for a ref that does not exist. The two must be indistinguishable (no oracle).
  - With no binding in ctx: `IsError`, `artifact refs are not available in this session`.
  - A gzip-bytes body behaves as on disk today: `archive.Open` semantics sniff the magic, so mirror that with `segment`-style sniffing. A NUL-containing body is refused as binary.
  - `grep {pattern:"NEEDLE", path: ref}` returns `art://acme/l1/spill/<id>:<line>: ...`, and the result cap applies.
  - `list_directory {path:"art://acme/l1/spill"}` lists each artifact as `<ref>  <size> bytes  <tool>  <RFC3339 created>`. `art://acme/l1` lists every kind.
  - A path that is not a ref behaves exactly as before. The existing tests stay green.
- [ ] **Step 2: Implement `artifact_ref.go`:**
  - `func readArtifact(ctx context.Context, path string) (data []byte, handled bool, err error)`: returns `handled == false` unless `artifact.IsRef(path)`. It parses with `ParseRef`, resolves via `BindingFrom(ctx)` and `Binding.Open`, and reads through a `MaxArtifactBytes` limit. Any `ErrNotFound` or `ErrInvalidRef` becomes `fmt.Errorf("artifact not found: %s", path)`. Then gunzip if the gzip magic is present, reusing `archive`'s sniffing helper if one is exported, else a local one.
  - `func listArtifacts(ctx, path) (string, bool, error)`: built on `ParsePrefix` and `Binding.List`. Re-assert that tenant and loop equal the binding's, otherwise `artifact not found`.
- [ ] **Step 3: Wire the three tools:**
  - `read.go`: before `archive.Open`, `if data, ok, err := readArtifact(ctx, a.Path); ok { ... }`. Refactor the existing range/binary logic into `renderRead(path string, data []byte, a readArgs) Result` so both sources share it.
  - `grep.go`: when `artifact.IsRef(a.Path)`, scan the artifact's lines with the same regex, limit and "path:line: text" format. The ref string is the path. A prefix-only ref gets an error asking for a full ref.
  - `list.go`: when `artifact.IsRef(a.Path)`, return `listArtifacts`.
  - Update the three tool `Description()` strings to say that `path` may also be an `art://` artifact ref.
- [ ] **Step 4: Verify:** `go test ./internal/tools -count=1 -race`. Commit `tools: read_file, grep and list_directory resolve art:// refs through the loop's store`.

---

## Task 7: Segments over the store: `ParseSegment`, `segment.Source`, `artsink`

**Tier:** standard

**Files:** modify `internal/segment/reader.go`; create `internal/segment/source.go`, `source_test.go`, `internal/segment/artsink/sink.go`, `sink_test.go`.

- [ ] **Step 1: Write the failing tests:**
  - `TestParseSegmentMatchesLoadSegment`, for both gzip and plain bytes.
  - `artsink`:
    - `var _ agent.SegmentSink = (*artsink.Sink)(nil)`.
    - `TestArchiveWritesSegmentAndIndex`, against a MemStore binding: Archive returns a pointer that `ParseRef`s to `KindSegment` under the binding's tenant and loop. `ArtifactSource.ReadIndex` has one entry whose `Path` equals that ref's ID, with the turn range, tools and tokens. `Load(entry)` round-trips the messages.
    - `TestArchiveAppendsAcrossSinks`: two `artsink.New(binding)` values over the same store, representing two runtimes. The second Archive appends to the first's index, giving two entries, and neither overwrites the other's segment, because ids are store-allocated.
    - `TestArchiveConcurrent`: 20 goroutines on one sink produce 20 entries.
    - `TestNewInvalidBindingIsNil`: `artsink.New(artifact.Binding{})` returns nil. Callers pass a nil `agent.SegmentSink` and get the agent's no-op.
- [ ] **Step 2: Implement:**
  - `reader.go`: `func ParseSegment(b []byte) (Segment, error)` (gunzip if needed, then the existing parse). `LoadSegment` reads the file and delegates to it. Keep `ReadIndex` and `LoadSegment` for legacy on-disk readers.
  - `source.go`, which stays agent-free:

```go
const IndexArtifactID = "index"
type Source interface {
	ReadIndex(ctx context.Context) (Index, error) // missing index ⇒ empty Index, nil
	Load(ctx context.Context, e IndexEntry) (Segment, error)
}
func NewArtifactSource(b artifact.Binding) Source // nil-safe: an invalid binding yields a Source whose ReadIndex is empty
```

    Index entries' `Path` holds the segment artifact ID, under the same JSON key `path`. Document this on `IndexEntry`.
  - `artsink/sink.go`: `type Sink struct{ b artifact.Binding; mu sync.Mutex }`, and `func New(b artifact.Binding) agent.SegmentSink`, which returns a nil interface when `!b.Valid()`. `Archive(r)` runs under `mu`:
    1. Render `segment.RenderSegment(seg)` (plaintext; the store compresses at rest).
    2. `meta, err := b.Put(ctx, KindSegment, body, Meta{Turn: r.TurnStart, Labels: {"turns": "<s>-<e>"}})`.
    3. Read the current index through `NewArtifactSource(b).ReadIndex`, which is store state, never memory, so a moved loop appends correctly.
    4. Append the entry and `PutID(ctx, KindSegmentIndex, IndexArtifactID, json)`.
    5. Return `meta.Ref.String()`.

    Use `context.Background()` with a 10s timeout. `SegmentSink.Archive` takes no ctx, and archive is best-effort.
- [ ] **Step 3: Verify:** `go test ./internal/segment/... -count=1 -race`. Commit `segment: store-backed index/source and artsink SegmentSink`.

---

## Task 8: `segment_read` through the binding; delete `SetSegmentsDir`; shell and TUI on the store

**Tier:** premium. This task carries shell parity: acceptance 5 and the 0030 adversarial gz/plain parity gate. It touches the TUI, and the shell drives turns itself, so `StartLoop` never binds its registry.

**Files:** modify `internal/tools/segment_read.go`, `segment_read_test.go`, `segment_read_wiring_test.go`, `internal/tools/registry.go` (`DefaultTools`), `internal/segment/fssink/sink.go`, `sink_test.go`, `internal/tui/agents_segments.go`, `agents_model.go`, `shell_model.go`, `agents_segments_test.go`, `cmd/fuse/shell.go`, `cmd/fuse/runtime_binding.go` (if needed), `cmd/fuse/segment_gateway_test.go`, `segment_adversarial_test.go`, `segment_wiring_test.go`.

- [ ] **Step 1: Write and adapt the failing tests:**
  - `segment_read_test.go`: seed through `artsink` on a MemStore binding, then call the tool with `artifact.WithBinding(ctx, b)`. Turn-range overlap, `tool_filter`, ordering and the 32 KiB cap all behave as today. With no binding: `no segments — segment archiving is not active for this session`.
  - `segment_read_wiring_test.go`: `NewSegmentRead()` takes no args. The Clone/Subset assertions stay.
  - `segment_adversarial_test.go`:
    - Make `seedSegmentsDir` → `seedSegments(t, gzipIt, systemGzip bool) artifact.Binding`. It puts the SAME rendered bytes (plain, Go-gzipped, or system-`gzip`-compressed) as a `KindSegment` artifact on an `artifact/fsstore` under `t.TempDir()`, and writes the index artifact with the entry's `Path` set to that id.
    - `driveNeedleRecovery` binds `reg.WithArtifacts(...)` in place of `SetSegmentsDir`.
    - The two parity gates stay byte-for-byte as written.
  - `segment_gateway_test.go`:
    - The sink is `artsink.New(binding)` over fsstore, and the registry is bound to the same binding.
    - (a) The index artifact has one entry, and the segment body's first bytes on disk under `<base>/<tenant>/<loop>/segment/` are the gzip magic.
    - (b) The post-compaction request carries `Recovery: grep your past at art://`.
    - (c) `segment_read` round-trips. Add (d): `grep` on the pointer ref finds `RAWDATA`.
  - `fssink/sink_test.go`: `fssink.New(dir, "sess-1")` archives into `<dir>/_default/sess-1/segment/` and returns an `art://_default/sess-1/segment/` pointer.
  - TUI `agents_segments_test.go`: use a fake `segment.Source` (or artsink over MemStore). The indicator line and the "s" drill-in render as before.
- [ ] **Step 2: Implement `segment_read.go`.** Delete `segmentsDir`, `segmentsMu`, `SetSegmentsDir` and `currentSegmentsDir`.
  - `NewSegmentRead() Tool`.
  - `Execute` resolves `artifact.BindingFrom(ctx)`. When there is none, it gives the "not active" message. Otherwise `src := segment.NewArtifactSource(b)`, then `src.ReadIndex(ctx)` and `src.Load(ctx, e)` in place of the dir reads. Everything else is unchanged.
  - Update `DefaultTools` to `NewSegmentRead()`.
- [ ] **Step 3: Rewrite `fssink`** as the one-code-path wrapper: `func New(artifactsDir, loopID string) agent.SegmentSink { return artsink.New(artifact.Binding{Store: fsstore.New(artifactsDir), Tenant: event.DefaultTenant, Loop: event.LoopID(loopID)}) }`. Delete `FSSegmentSink`, `gzipBytes`, `appendIndex` and `fileExists`. Keep the package doc explaining the import-cycle split.
- [ ] **Step 4: Update the TUI.** Replace `segmentsDir string` with `segments segment.Source` on `AgentsModel` and `ShellModel`, and replace `WithSegmentsDir(dir)` with `WithSegments(src segment.Source)`. The `segmentSummary` cache moves from index mtime to a 1-second TTL (`segCacheAt time.Time`), so a per-frame render does not re-read the store every frame. `loadSegmentRegions` iterates `src.ReadIndex` and `src.Load`. A nil source renders nothing, as an empty dir did.
- [ ] **Step 5: Shell wiring in `cmd/fuse/shell.go`:**
  - `artsDir := filepath.Join(filepath.Dir(session.DefaultLogDir()), "artifacts")`, i.e. `~/.fuse/artifacts`.
  - `arts := artfs.New(artsDir)` (alias the import `artfs "…/internal/artifact/fsstore"`; `event/fsstore` is already imported).
  - Right after `toolReg` is built, and BEFORE anything clones it: `toolReg.WithArtifacts(arts, event.DefaultTenant, event.LoopID(tree.RootID()))`.
  - `segmentSink := artsink.New(toolReg.Artifacts())`, replacing `fssink.NewFSSegmentSink`. Delete the `tools.SetSegmentsDir` line.
  - `m = m.WithSegments(segment.NewArtifactSource(toolReg.Artifacts()))`, replacing `WithSegmentsDir`.
  - Grep `runtime_binding.go` (`buildShellRuntimeDeps`) for every registry the shell's root and children use (`reg2`, `childToolReg`). Confirm each derives from the bound `toolReg` via Clone/Subset. If one is built fresh, bind it too.
- [ ] **Step 6: Verify:**
  - `grep -rn "SetSegmentsDir\|currentSegmentsDir\|NewFSSegmentSink" --include='*.go' .` is empty.
  - `go test ./internal/tools ./internal/segment/... ./internal/tui ./cmd/fuse -run 'Segment|Adversarial|Spill' -count=1 -race`.
  - `go build ./... && go build -tags pgstore ./...`.
  - Commit `segments: archive and recover through the artifact store; shell keeps ~/.fuse parity`.

---

## Task 9: Composition root: artifact store selection, loop-server sink, CLI bindings

**Tier:** standard

**Files:** modify `cmd/fuse/durable_backend.go`, `durable_backend_pg.go`, `durable_backend_fs.go`, `loop_server.go`, `runtime_binding.go` (one-shot and research-probe deps), `durable_backend_test.go` / `durable_backend_strict_test.go`.

- [ ] **Step 1: Write the failing tests:**
  - `TestSelectArtifactStoreUntaggedIsFS` (untagged file): `selectArtifactStore(cfg)` returns an `*artfs.Store` rooted at `~/.fuse/artifacts` (with `HOME=t.TempDir()`), and creates the directory.
  - `TestLoopServerDepsWireArtifacts` (security-knob-inert learning): `buildLoopServerRuntimeDepsStrict(...)` yields `Deps.Artifacts != nil`.
  - `TestLoopServerStrictRefusesArtifactStoreError`: inject `selectArtifactStoreFn` returning an error. The strict builder returns it, and the lenient builder leaves `Artifacts` nil.
  - `TestLoopServerBuildAgentInstallsSegmentSink`:
    - Call the loop-server factory's `NewToolRegistry()` and `WithArtifacts(MemStore, "acme", "loopx")`, then `BuildAgent(...)` with a tree rooted at `loopx` and `cfg.Context.Summarization.Enabled = true`.
    - Assert that the built agent's sink archives under (acme, loopx). Drive one `Archive` via the agent if a test accessor exists. Otherwise assert through Task 10's segment acceptance test and keep this test to "BuildAgent succeeds with a bound registry".
  - Tagged (`durable_backend_pg` test, `pgstore` tag, skip without DSN): with `FUSE_PG_DSN` set, `selectArtifactStore` returns the pg store.
- [ ] **Step 2: Implement:**
  - Untagged `durable_backend.go`: `func selectArtifactStore(cfg config.Config) (artifact.Store, error)` returns `newFSArtifactStore()`. Put the shared helper in `durable_backend_fs.go`: `MkdirAll(~/.fuse/artifacts, 0o700)` and `artfs.New(dir)`. Wrap a failure in `fsBackendError`.
  - Tagged `durable_backend_pg.go`: if `pgDSN() != ""`, return `artpg.Open(context.Background(), dsn)`, else the fs helper. This is the only file that imports `internal/artifact/pgstore`.
  - `loop_server.go`:
    - Add `var selectArtifactStoreFn = selectArtifactStore`, beside `selectDurableBackendFn`.
    - In `buildLoopServerRuntimeDepsStrict`: `arts, aerr := selectArtifactStoreFn(cfg)`. Return an error in the same two shapes as the durable error (fs vs DSN). Set `d.Artifacts = arts` after `assembleLoopServerRuntimeDeps`.
    - The lenient `buildLoopServerRuntimeDepsWithObserver` sets it when there is no error and leaves it nil (MemStore default) otherwise.
  - Loop-server per-loop segment sink in `loopServerHost.factory` → `BuildAgent`:
    - First line: `segSink := artsink.New(loopToolReg.Artifacts())`. `loopToolReg` may be nil; guard it, since `Artifacts()` must be nil-receiver-safe or checked.
    - Pass `segSink` in place of the trailing `nil` sink argument to the root `buildAgentCore`, the child `buildChildAgent` and the child `buildAgentCore` (`loop_server.go` around lines 439, 445 and 477 today; grep `buildChildAgent(\|buildAgentCore(` in the file).
    - Update the "No segment sink installed (nil)" comment and `installSummarizer`'s doc ("Only the interactive shell installs a real sink" becomes "the shell and the loop server install a real sink").
  - One-shot (`buildOneShotRuntimeDeps`) and research-probe (`buildResearchProbeRuntimeDeps`): set `Artifacts` from `newFSArtifactStore()`. On error, log to stderr and leave it nil (MemStore default). This restores CLI spill recovery under `~/.fuse/artifacts`, replacing the deleted `SetSpillDir` calls. Their segment sinks stay nil, as today.
  - If the process closes the durable pg store on shutdown, close the artifact pg store next to it. Grep `Close()` near `selectDurableBackendFn` callers. If nothing closes the event store, add nothing.
- [ ] **Step 3: Verify:** `go test ./cmd/fuse -run 'Artifact|DurableBackend|LoopServer' -count=1 -race`, `go build -tags pgstore ./...`, and `go list -deps ./cmd/fuse | grep -c pgx` = 0 untagged. Commit `fuse: select the artifact store at the composition root; loop server archives segments per loop`.

---

## Task 10: Cross-runtime acceptance tests

**Tier:** standard

**Files:** create `cmd/fuse/artifact_acceptance_test.go`.

- [ ] **Test A: `TestSpillRefRecoveredOnAnotherRuntime`.** Model it on `TestColdResumeAcceptance` in `cmd/fuse/loop_serve_net_resume_test.go`, and reuse `waitForKindInStore`, `waitForNotLive` and `waitForParkCount`.
  - Setup:
    - `t.Setenv("HOME", t.TempDir())`.
    - Write a fixture file of 600 lines × ~70 bytes (about 42 KiB) with `NEEDLE-7f3a` on line 300, outside the 8 KiB head and tail.
    - One shared durable `event/fsstore` and ONE shared `artfs.New(t.TempDir())`.
    - `buildDeps(idleTTL)` sets `d.DurableStore`, `d.Registry`, `d.Artifacts = sharedArts`, `d.BaseDir = ""` and `d.IdleTTL`.
    - `rtA` uses a 150 ms idleTTL, and `rtB` uses 1 hour. Each runtime gets its own connect server.
  - Scripted gateway, built with `toolCallsReply` / `argsJSON`:
    1. The first main request (no tool results) returns a `read_file {path: fixture}` call.
    2. The request carrying that tool result returns a plain answer, so the loop parks.
    3. On B's turn (the request carrying the user text `recover it`), extract the first `art://[^"\s\\]+` match from the request body with a regexp, and return `read_file {path: <ref>, start_line: 295, end_line: 305}`.
    4. The request carrying that result returns a final answer. Record whether its body contains `NEEDLE-7f3a`.
  - Flow:
    1. On A: `StartLoop` (Interactive), then wait for the park.
    2. Assert A's stream has a `tool.result` event whose payload contains `art://` and contains neither the fixture's directory nor any path under `HOME`.
    3. `waitForNotLive`, then `clientB.Send("recover it")`, then `waitForParkCount(rtB, 2)`.
    4. Assert the gateway saw the needle on B's recovery request (anti-vacuity: also assert the B-side `read_file` call was actually issued).
- [ ] **Test B: `TestSegmentRecoveredOnAnotherRuntime`.**
  - Setup:
    - Shared `artfs` store.
    - `cfg` from `config.Load()` with `cfg.Context.Summarization.Enabled = true`.
    - Scripted gateway, as in `segment_gateway_test.go`: the summarizer request returns a canned summary, and the main request returns `done`.
  - Runtime A's side:
    - `depsA := buildLoopServerRuntimeDeps(...)`, `regA := depsA.NewToolRegistry().WithArtifacts(shared, "acme", "loopseg")`.
    - `a, _, _, err := depsA.BuildAgent(event.NoopStore{}, agent.NewAgentTreeWithRootID(alias, alias, 2, "loopseg"), alias, regA)`, so the production factory builds the sink.
    - Set `a.ContextWindow = 4000` and run it on the big-history fixture from `segment_gateway_test.go`.
    - Assert that the post-compaction main request carries `Recovery: grep your past at art://acme/loopseg/segment/`.
  - Runtime B's side:
    - An independent `depsB` graph, with `regB := depsB.NewToolRegistry().WithArtifacts(shared, "acme", "loopseg")`.
    - `regB.Execute(ctx, "segment_read", {"turn_range": "<range from the index>"})` contains `RAWDATA`.
    - `regB.Execute(ctx, "grep", {"pattern":"RAWDATA","path":<pointer ref>})` returns matches.
    - Tenant isolation: `regC` bound to `("other","loopseg")` gets `no segments overlap` (or the not-active/empty result), and `read_file` on the pointer returns `artifact not found`.
- [ ] **Verify and commit:** `go test ./cmd/fuse -run 'RecoveredOnAnotherRuntime' -count=1 -race`. Commit `fuse: cross-runtime spill and segment recovery acceptance`.

---

## Task 11: Docs, dead-code sweep, full verification

**Tier:** economy

**Files:** modify `docs/designs/context-management.md`. Leave historical results, plans and specs untouched.

- [ ] Rewrite the spill/segment recovery paragraphs. Cover:
  - Oversized results and compaction segments are artifacts in the loop's store, addressed as `art://<tenant>/<loop>/<kind>/<id>`.
  - Local runs keep them under `~/.fuse/artifacts`, and a Postgres-backed server keeps them in the `artifacts` table.
  - `read_file`, `grep`, `list_directory` and `segment_read` accept refs.
  - Do not name any host application.
- [ ] `grep -rn "SetSpillDir\|SetSegmentsDir" --include='*.go' .` is empty (acceptance 4). Also grep for orphans: `sweepSpillDir`, `spillMeta`, `FSSegmentSink`, `WithSegmentsDir`.
- [ ] `go vet ./... && go vet -tags pgstore ./...`.
- [ ] `make test` is green.
- [ ] `go test -tags pgstore ./internal/artifact/... ./internal/event/pgstore/... ./cmd/fuse -run 'Artifact|PG|DurableBackend' -count=1` is green or skip-clean. Report which it was.
- [ ] Commit `docs: artifact refs replace spill and segment paths`.

---

## Task 12: ADR (parent task, not a build worker)

**Tier:** parent. `docket-implement-next` runs this through the `docket-adr` agent; a build worker skips it.

Record the decision as follows:
- Artifacts (spilled tool output and compaction segments) are addressed by tenant-scoped opaque refs (`art://<tenant>/<loop>/<kind>/<id>`), never by filesystem paths.
- The store is an adapter seam (`artifact.Store`) with local-filesystem and Postgres backends plus an in-memory default, selected at the composition root alongside the durable event store.
- The runtime binds each loop's registry to (tenant, loop).
- Tenant isolation is enforced at `Binding.Open` (full-key re-assertion) and by every backend (wrong tenant ⇒ not found).
- Relationship to prior ADRs: it extends ADR-0017's agent-free reader / leaf writer split to `segment` / `segment/artsink`, and follows ADR-0030's composition-root selection.

---

## Acceptance coverage map

| Spec acceptance | Covered by |
|---|---|
| 1. Conformance passes for fsstore, MemStore and pgstore | Tasks 1–3 (`RunStoreConformance`), Task 11 (tagged run) |
| 2. No absolute spill path; hosted >20 KiB result carries `art://`; recoverable on another runtime | Task 4 (marker), Task 5 (binding), Task 9 (loop-server store), Task 10 Test A |
| 3. `segment_read` on a non-compacting runtime; `grep` on `art://` | Task 6 (grep), Tasks 7–9, Task 10 Test B |
| 4. `SetSpillDir` / `SetSegmentsDir` gone | Tasks 4 and 8, Task 11 grep |
| 5. Shell parity under `~/.fuse`; 0030 adversarial parity tests pass | Task 8 (shell wiring, adapted adversarial and gateway tests) |
| 6. ADR | Task 12 |
