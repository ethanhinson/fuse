<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0065 — bash per-tenant filesystem isolation — a Principal.Tenant-scoped bind-mount the working_dir cannot escape](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0065-bash-per-tenant-filesystem-isolation.md)**
<!-- docket:backlink:end -->

# bash per-tenant filesystem isolation — results

Change: #0065 · Branch: feat/bash-per-tenant-filesystem-isolation · PR: (opened at close-out) ·
Plan: docs/superpowers/plans/2026-09-05-bash-per-tenant-filesystem-isolation.md · ADRs: 0055, 0056, 0057

## Verify (human)

The automated suite covers the security properties directly, and the positive isolation test ran
against real Docker on the build machine. What is left for you is deployment-shaped judgment.

- [ ] **The tenant-id contract is now a breaking constraint.** A hosted deployment whose identity
      provider mints tenant ids containing uppercase letters, or beginning with a dot, now gets a
      loud `ErrNoTenantRoot` refusal (degraded-safe: nothing mounted, `working_dir` refused) where
      it previously got a working tree. Confirm your `loop_server.auth` tenants are lowercase and
      do not start with `.` before deploying. The reasoning is ADR-0057; the constraint is
      documented on `AuthTokenConfig` in `internal/config/schema.go`.
- [ ] **Host layout is a policy choice this change made for you.** Hosted bindings now provision
      `~/.fuse/workspaces/<tenant>/` at mode 0700, created on first use. Confirm that location is
      right for your deployment — a volume mount, a quota, a backup target. It is a constant, not a
      config knob, deliberately (a config-supplied mount parent would name a host directory that
      fuse bind-mounts into a container a model drives).
- [ ] **Run two authenticated principals through one `loop-serve-net` and confirm isolation by
      hand.** The suite proves it, but this is the property the change exists for and it is worth
      seeing once: tenant A writes a file via `bash`, tenant B cannot read it.
- [ ] Confirm `fuse_sandbox_unhealthy_total` appears with a real `reason` label after an induced
      failure (an unpullable image is the easiest: `pull_failed`).

## Findings

The deep whole-branch review returned 5 findings — 2 blocker, 2 important, 1 minor — all fixed
in-branch before the PR opened. Two are worth your attention beyond the diff:

1. **Tenant-id case collision (blocker, fixed in `d6047bf`).** The first implementation permitted
   both cases in a tenant id and used it verbatim as a directory name. On a case-insensitive
   filesystem — APFS, i.e. the machine this was built on — `Acme` and `acme` are ONE directory,
   while `filepath.Rel` still yields distinct segments, so both passed containment and both mounted
   the same tree. The change's headline security property, failing silently, with a green suite.
   Fixed by refusing uppercase rather than lowercasing (normalising would merge two authenticated
   identities into one tenant, which is the same defect by another route). ADR-0057.

2. **An omitted `loop_server.auth` tenant broke every bash call (blocker, fixed in `e7e1293`).**
   `tenant` is documented optional and pinned as a supported shape by `internal/config/loader_test.go`,
   but an empty tenant hit the resolver's refuse-empty invariant. The refusal was the correct
   security decision and a silent breaking change to a shipped config surface at the same time.
   Fixed at the authentication edge (`buildLoopVerifier` maps `""` → `event.DefaultTenant`, joining
   two sibling edges that already did), leaving the filesystem layer's refuse-empty floor untouched.

The other three: a tenant-root misconfiguration was being reported as substrate health once per bash
call (`025c579`); `create=true` did not re-assert 0700 on a pre-existing tenant tree (`680d6dc`);
dot-leading tenant ids were accepted (`7056930`).

## Deviations from plan

**#0074's health emitter landed only in part, by the spec's own instruction.** The spec folded
change #0074 into this one on the assumption that per-tenant mounts imply a persistent container.
They do not — the substrate is still `docker run --rm` per `Exec`. The spec's Decision 3 anticipated
exactly this and required amending the spec rather than faking the emitter, so:

- **Landed:** `pull_failed`, `acquire_failed`, and a new exit-code classifier producing `oom` /
  `runtime_exit`; the #0063 tripwire flipped to a positive assertion driving a real OOM against a
  real container on a live `/metrics` scrape.
- **Deferred back to #0074:** `unresponsive`, `recovered`, and a real `ContainerID` — all three
  require a container that outlives an `Exec`. No constants were declared for the two deferred
  reasons, deliberately: declaring them invites a synthesised transition later.

Change #0074 remains `deferred` and still carries that residue. The spec on `origin/docket` carries
the dated amendment recording why.

## Follow-ups

- **#0074** — the persistence-dependent half of the health emitter, as above.
- **Not filed, worth knowing:** the case-insensitivity hazard ADR-0057 addresses applies to any
  other host path fuse derives from an identity string. Per-principal egress socket names are the
  obvious neighbour. Not observed to collide, and out of scope here.
