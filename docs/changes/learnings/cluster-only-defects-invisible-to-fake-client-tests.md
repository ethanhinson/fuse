---
slug: cluster-only-defects-invisible-to-fake-client-tests
hook: "Golden-object tests against a fake API client prove you RENDER the right object; they cannot prove the object runs or that you read the response correctly. Typed-error matching against the wrong package's type, a security field combination the runtime rejects at startup, and a missing namespaced prerequisite are all 100%-fatal production defects that a fully green fake-client suite reports as passing."
topics: [testing, kubernetes, integration, fake-client, go, error-handling, sandbox]
changes: [75]
created: 2026-09-17
updated: 2026-09-17
promotion_state: candidate
promoted_to:
---

## Apply

A fake/in-memory client for a remote control plane (`k8s.io/client-go/kubernetes/fake`, a mock SDK,
a recorded-response double) makes a whole class of test cheap and deterministic: given this input,
do we construct the right request object? Keep those — they are the right tool for rendering,
policy shape, and label/annotation correctness.

But a fake client has three blind spots that are each individually fatal in production, and change
0075 shipped all three past a green suite until a real `kind` cluster ran:

1. **Typed errors you match on the wrong type.** The code matched `k8s.io/utils/exec`'s
   `CodeExitError` where the exec stream actually returns `k8s.io/client-go/util/exec`'s. Two
   packages, same type name, no compile error. Effect: `Verify` disqualified **every** cluster and
   every non-zero command exit read as a broken substrate. A fake client never produces the real
   error value, so the branch was never taken under test.
2. **Field combinations the runtime rejects.** `runAsNonRoot: true` with no `runAsUser` is a valid
   object the API server admits happily, and the kubelet then refuses to start the container
   against any image whose `USER` is unset. Admission validity is not startability.
3. **Prerequisites outside the object.** The canary namespace lacked a ServiceAccount; a missing
   `services: get` RBAC verb made the ClusterIP lookup fail, so the egress floor could never be
   proven in any real deployment. A fake client serves whatever you seeded and enforces no RBAC at
   all.

**How to apply.** Decide, per code path, whether it is *rendering* (fake is sufficient) or
*interaction* — reading a response, reacting to an error, depending on runtime admission or on a
permission grant. Every interaction path needs at least one acceptance against the real control
plane, and that lane must fail loudly when it does not run (see
[[smoke-over-fake-backend-proves-wire-not-system]]). Specifically: assert on typed errors via the
exact package the client library returns, prove the RBAC manifest against the API calls the code
makes rather than against intent, and run one real end-to-end acceptance per security floor you
claim to enforce.

Related: [[smoke-over-fake-backend-proves-wire-not-system]], [[spec-asserted-error-code-verify-emitter-first]].
