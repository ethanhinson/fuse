---
slug: named-volume-root-owned-when-mountpoint-absent
hook: "A Docker named volume is created root-owned 0755 when its mount point does NOT exist in the image — so a nonroot (distroless) container silently cannot write to its own data or home directory. Compose has no uid/gid option for volumes: chown the volume in a one-shot init service that the app service waits on, or bake the directory into the image with the right ownership. The failure is a degraded-mode log line, not a crash, so it survives every green suite."
topics: [docker, compose, distroless, nonroot, volumes, permissions, deployment]
changes: [76]
created: 2026-09-17
updated: 2026-09-17
promotion_state: candidate
promoted_to:
---

## Apply

Two facts combine badly. First, when a named volume is mounted at a path that is **absent
from the image**, Docker creates it fresh, owned `root:root` mode `0755` — it only copies
ownership and contents when the mount point already exists in the image. Second, a distroless
or otherwise hardened image runs as a nonroot uid (65532) and typically does **not** contain
its own home directory.

So `volumes: [fuse-home:/home/nonroot/.fuse]` over a distroless image yields a directory the
container's own user cannot write. The application does not crash — it degrades. In change
0076 the server logged `HOSTED with NO PER-TENANT WORKSPACE` and carried on, because a
workspace directory it could not create was treated as an optional feature rather than a
misconfiguration.

**Compose offers no `uid`/`gid`/`mode` option for named volumes**, so there are exactly three
fixes, in order of preference:

1. **Bake the directory into the image** with the right ownership (`RUN mkdir -p … && chown`),
   so the volume inherits it. Not available when the base is distroless and you cannot add a
   shell-run layer.
2. **A one-shot init service** — a `busybox` service that `chown`s every affected volume and
   exits 0, with the app service `depends_on: {init: {condition: service_completed_successfully}}`.
   This is what 0076 shipped; remember to wire it into *every* profile variant, which an
   anchor/extends makes automatic.
3. Run the container as root and drop privileges internally — defeats the point of distroless.

**Assert it, don't infer it.** A compose-render test (`docker compose config`) validates
syntax and cannot see ownership; only a real bring-up can. Check the volume's ownership
explicitly in the smoke (`65532:65532`, `0700`) and check that the directory the app needs
was actually created — and treat any "degraded, continuing" log line as a smoke-test failure
condition, not as informational output. See
[[distroless-container-healthcheck]] for the related readiness-gating rule, and
[[security-knob-inert-at-composition-root]] for the general shape: a component that silently
did not initialize reads green everywhere except production.

## War story

**2026-09-17 (#76, PR #90).** Found in live verification, after the full suite was green, a
deep review was clean, `docker compose config` rendered both the default and
`--profile docker-socket` variants, and 16 Helm chart tests passed against real `helm`. The
compose stack came up and `/readyz` returned 200 — but the server logged `HOSTED with NO
PER-TENANT WORKSPACE`, because `/home/nonroot/.fuse` is absent from the distroless image, so
the `fuse-home` named volume was created root-owned and the nonroot server could not
`mkdir ~/.fuse/workspaces`. The fix added a `busybox` init service chowning `fuse-home` and
`fuse-tmp` to 65532, with `fuse` (and, through the shared anchor, `fuse-docker-socket`)
waiting on its successful completion. Verified live: init exits 0, `fuse` healthy, `/readyz`
200, volume `65532:65532` `0700` with `workspaces/` present, and no workspace warning.
