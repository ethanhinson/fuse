---
slug: abortable-client-stream-teardown
hook: "A browser SDK's long-lived observe stream needs an explicit, idempotent AbortSignal teardown — breaking an async iterator cannot be reached from page lifecycle hooks and leaks one stream per page load."
topics: [typescript, streaming, lifecycle, browser, sdk, resource-leak]
changes: [56]
created: 2026-08-12
updated: 2026-08-12
promotion_state: candidate
promoted_to:
---

## Apply

Expose cancellation for every long-lived client observation rather than relying on the consumer
to break a `for await` loop. Thread an `AbortSignal` through the reconnect loop and the underlying
transport, close the stream exactly once, and make both pre-aborted signals and repeated aborts
safe no-ops after the closed transition. This gives pagehide/component teardown a handle it can
reach from outside the iterator and prevents one leaked stream per page lifetime.

Test the normal abort path, a pre-aborted signal, and abort-after-close/double-abort. The lifecycle
callback should observe one `closed` transition in every case.

## War story

- 2026-08-12 (#56, PR #57) — Wander needed to release its `Observe` stream on `pagehide`, but the
  old TS SDK could only stop when code inside the async iterator broke the loop. The SDK added an
  `AbortSignal` option, threaded it through reconnect and transport teardown, and covered consume-
  then-abort, pre-abort, and double-abort cases with one `closed` notification.
