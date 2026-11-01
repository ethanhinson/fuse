<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0103 — Per-tenant loop limits: live-loop cap and start rate with per-tenant overrides](../changes/active/0103-per-tenant-loop-limits-live-loop-cap-and-start-rate-with-per.md)**
<!-- docket:backlink:end -->

# Results: Per-tenant loop limits

**Human action:** No action is required before merge; the optional walkthrough below adds confidence beyond the scripted tests.

## Outcome

`loop_server.limits` (trusted home config only) bounds each tenant on `fuse loop-serve-net`: `default` and per-tenant `tenants` overrides each carry `max_live_loops` and `starts_per_minute`; zero or no block means unlimited. A `StartLoop` beyond the start rate or at the live-loop cap returns `ResourceExhausted` naming the limit, with a whole-second `Retry-After` for the rate case. A `Send` that resumes a reaped loop goes through the same limiter. `GetCapabilities` reports the caller's effective limits (proto field, Go SDK `Capabilities.Limits`). README and deployment docs describe the keys; the decision is recorded in an ADR.

## Human actions and testing

- **Optional:** run `fuse loop-serve-net` with `loop_server.limits.default.max_live_loops: 1` in the home config, start one interactive loop as tenant A, then start a second. Expect `ResourceExhausted`; tenant B is unaffected. Nothing persistent changes beyond the loops you started. Scripted tests already cover this over h2c.

## Verification performed

- Full suite (`make test`) green at the final head via the native gate driver.
- Review ran at the deep tier. Fixed in-branch: a resuming `Send` bypassed the cap (now admitted through the limiter). Other findings are under Known issues.

## Known issues and follow-ups

- **Live-loop cap counts the tenant's whole registry history.** When it occurs: every `StartLoop` for a tenant with a cap lists all of its registry records under that tenant's admission lock (fsstore also holds its store-wide lock). Practical impact: admission latency grows with history. Confirmed. Workaround: none. Next action: add a count query to the registry stores. Backlog verdict: tracked by change 0106.
- **A start in flight can be counted twice.** When it occurs: while a loop is registered but its start has not returned, it counts once in the registry and once as a reservation, so concurrent starts can be refused slightly below the cap (fail-closed, no `Retry-After`). Practical impact: rare over-refusal. Confirmed. Next action: key reservations by loop id. Backlog verdict: No existing change fits (checked 3).
- **Per-tenant buckets and locks are never evicted.** Practical impact: one small entry per distinct authenticated tenant for the process lifetime. Confirmed, minor. Next action: evict idle entries. Backlog verdict: tracked by change 0107.
- ADR-0074's "concurrent starts cannot overshoot the cap" holds within a process only; across replicas the live-loop cap is a soft bound (overshoot bounded by the replica count).
- A resuming `Send` also spends a start-rate token, and a tenant at its cap sending to a finished, unresumable loop gets `ResourceExhausted` rather than `FailedPrecondition`.
- The plan was authored inline because `superpowers:writing-plans` is unavailable here.
