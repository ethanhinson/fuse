---
slug: pod-level-security-floor-overridden-by-container-scope
hook: "A read-back assertion that a remote orchestrator admitted your security floor must inspect EVERY scope and EVERY container list that can override it — a pod-level `securityContext` is silently overridden by a container-level one, and `initContainers`/`ephemeralContainers`/`shareProcessNamespace` are injection and escape channels a spec-level check that only reads `.spec.securityContext` and `.spec.containers` never sees."
topics: [security, kubernetes, sandbox, containment, admission, read-back, mutating-webhook]
changes: [75]
created: 2026-09-17
updated: 2026-09-17
promotion_state: candidate
promoted_to:
---

## Apply

When "contained" cannot be verified by construction — because the isolation primitive belongs to an
orchestrator you do not own — the substitute is **confirm-or-refuse**: create the object, read back
what the API server actually admitted, and assert the floor on the admitted spec. That is the right
shape. The trap is that the assertion is written against the fields you *set*, while the floor can
be defeated through fields you did not.

Change 0075's Kubernetes substrate hit four instances of exactly this, three of them after a deep
review had already fixed the first:

1. **Container scope beats pod scope.** `spec.securityContext` is a default; each container's own
   `securityContext` overrides it field by field. A mutating webhook setting container
   `runAsUser: 0`, `runAsNonRoot: false`, or `seccompProfile: Unconfined` leaves every pod-level
   assertion passing while the workload runs as root.
2. **Extra container lists.** `initContainers` and `ephemeralContainers` are the canonical
   injection shape, and a check iterating only `spec.containers` never looks at them.
3. **Intra-pod namespace sharing.** `shareProcessNamespace: true` is `hostPID`'s twin inside the
   pod: it lets the workload read a sidecar's `/proc/<pid>/root` and so reach a credential mounted
   *only* into that sidecar — defeating a credential split enforced purely by checking mounts.
4. **Fields the renderer treats as load-bearing but the assertion forgot** (`FSGroup`,
   `RunAsGroup`).

**Why it stays invisible:** the object was created with the right spec, so every unit test over the
renderer is green, and the read-back assertion is green too — it is asserting a true statement about
the wrong subset of the object.

**How to apply.** Write the posture assertion from the *attacker's* inventory, not the renderer's:
enumerate every scope that can override the field (pod vs container), every collection that can
introduce a container (`containers`, `initContainers`, `ephemeralContainers`), and every spec-level
switch that dissolves the boundary between them (`shareProcessNamespace`, `hostPID`, `hostIPC`,
`hostNetwork`). Assert the floor on each container independently — a pod-level default that is
correct proves nothing about a container that overrode it. Then write one test per override channel
that mutates the admitted object the way a webhook would and asserts the check **refuses**; a test
that only feeds it your own renderer's output can never fail.

Related: [[trusted-root-never-model-selectable]], [[security-knob-inert-at-composition-root]].
