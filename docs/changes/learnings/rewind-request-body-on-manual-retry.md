---
name: rewind-request-body-on-manual-retry
slug: rewind-request-body-on-manual-retry
title: Rewind req.Body from GetBody on every manual retry attempt — http.Client.Do won't
hook: "A caller-driven retry loop that reuses/clones an *http.Request must reset req.Body from req.GetBody() before each attempt — http.Client.Do only auto-invokes GetBody for its own redirect handling, so a retried POST silently sends an empty body on attempt 2+"
promotion_state: candidate
changes: [14]
created: 2026-08-05
updated: 2026-08-05
topics: [http, retries, request-body, go, getbody]
---

When you write your *own* bounded-retry loop around `http.Client.Do` and reuse or
`Clone()` the request across attempts, the request body is a single-use
`io.ReadCloser` that is drained by attempt 1. `http.Client.Do` **only** rewinds it
from `req.GetBody()` for its *own* internal redirect handling — never for a
caller-driven retry. So attempt 2+ of a POST/PUT sends an **empty body**, which
fails silently as a malformed/empty request rather than a loud error.

The fix is one guard at the top of each attempt:

```go
if req.GetBody != nil {
    body, err := req.GetBody()
    if err != nil { return err }
    req.Body = body
}
```

Rules that generalize the incident:
- `http.NewRequest*` with a `*bytes.Buffer` / `*bytes.Reader` / `*strings.Reader`
  populates `GetBody` for free; a custom `io.Reader` body does **not** — set
  `req.GetBody` yourself if you intend to retry.
- `Request.Clone()` copies the `GetBody` func but **shares the same drained
  body reader** — cloning is not rewinding.
- A regression test must assert the body of **each** attempt, not just the first:
  a server-side handler that records every received body, then assert attempt 2
  carries the full payload. A test that only inspects attempt 1 passes while the
  bug ships.

## War story

(#14, PR #13) — fuse research mode, `internal/research/http.go` `doOnce`
(commit `b72990c`). The bounded HTTP helper cloned the request per attempt for
its retry/backoff loop but never rewound the body. The Brave provider (GET, no
body) was unaffected, so it passed; the Tavily provider (POST with a JSON body)
sent an empty body on every retry, surfacing as opaque Tavily 4xx only under the
retry path. Fixed by resetting `req.Body` from `req.GetBody()` before each
attempt, with a regression test asserting both requests carry the full body.
