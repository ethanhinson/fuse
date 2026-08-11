---
name: fanout-send-snapshot-identity-not-pointer
slug: fanout-send-snapshot-identity-not-pointer
title: Non-blocking fan-out that snapshots subscriber pointers under the lock then sends outside it races a concurrent Close — snapshot identity and re-validate under the re-acquired lock
hook: "Pub/sub fan-out that copies *subscriber pointers under the lock then sends after releasing it races a concurrent unsubscribe/Close (send-on-closed-channel). Snapshot subscriber IDs, then re-validate membership under the re-acquired lock before each non-blocking send."
promotion_state: candidate
changes: [47]
created: 2026-08-11
updated: 2026-08-11
topics: [go, concurrency, race, pubsub, fanout, channels]
---

## Apply

A subscriber fan-out (`deliver`, `publish`, `broadcast`) almost always wants to send **outside**
the registry lock so a slow or blocked subscriber never wedges the publisher — the correct
non-blocking-Append / never-wedge-a-slot instinct (ADR-0025/ADR-0016). The trap is *how* you
carry the subscriber set across the lock boundary:

```go
// RACY: snapshot POINTERS under the lock, send after releasing it
s.mu.Lock()
subs := make([]*subscriber, 0, len(s.subs))
for _, sub := range s.subs { subs = append(subs, sub) }   // *subscriber escapes the lock
s.mu.Unlock()
for _, sub := range subs { select { case sub.ch <- e: default: } }  // sub.ch may be CLOSED now
```

Between the unlock and the send, a concurrent `Unsubscribe`/`Close` can close `sub.ch`. The send
is then **send-on-closed-channel → panic**, not a dropped event. It surfaced ~1-in-5 under
`-race` in `pgstore.deliver` (change 0047) and needed a 10× stress run to confirm the fix.

**Rule:** carry **identity across the lock boundary, never a live pointer to mutable/closable
state.**

```go
// SAFE: snapshot IDs, re-validate membership under the RE-ACQUIRED lock before each send
s.mu.Lock()
ids := make([]int, 0, len(s.subs))
for id := range s.subs { ids = append(ids, id) }
s.mu.Unlock()
for _, id := range ids {
    s.mu.Lock()
    sub, ok := s.subs[id]        // still subscribed? still open?
    s.mu.Unlock()
    if !ok { continue }
    select { case sub.ch <- e: default: }   // drop-newest-with-gap on a full but live channel
}
```

**Why it's easy to miss:** the pointer snapshot *looks* like it made the send lock-free and safe —
you did take the lock. But the lock protected the *slice*, not the *lifetime* of what it points
at. The object's `Close` is a separate write the snapshot doesn't see. `-race` only fires it if a
test drives publish concurrently with unsubscribe/Close in a tight loop (same missing-concurrent-test
gap as [[race-invisible-to-race-detector-without-concurrent-test]]).

**Related:** [[slot-cap-yield-while-blocked-on-children]] and the non-blocking-Append discipline —
this is the *safe* way to keep the publisher non-blocking. [[mutex-test-double-concurrent-provider]]
is the mirror on the test side (lock both getter and setter).
