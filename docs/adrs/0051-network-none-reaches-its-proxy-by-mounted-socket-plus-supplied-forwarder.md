---
id: 51
slug: network-none-reaches-its-proxy-by-mounted-socket-plus-supplied-forwarder
title: A `--network none` container reaches its egress proxy through a bind-mounted UNIX socket plus a fuse-supplied loopback forwarder
status: Accepted
date: 2026-09-01
supersedes: []
reverses: []
relates_to: [44]
change: 64
---

## Context

ADR-0044 settled the framing: for `bash`, **egress is the container's network configuration**, not an in-process dialer allowlist — because a container holds a real shell, and every subprocess it spawns (`curl`, `psql`, `git`) shares its network namespace. Change 0064 implements that framing with a `--network none` floor plus a shared host-side egress proxy that owns the allowlist decision.

That immediately poses a question ADR-0044 did not answer. `--network none` leaves the container with **loopback only and no route off-box** — which is exactly the property that makes it a floor. So how does the workload reach the proxy *at all*, without re-opening the general egress the floor exists to close? The change's spec left this open by design ("a mounted proxy socket or a userspace-net path the proxy owns — settle at plan time against the supported OCI CLIs"), and recorded a per-container sidecar as a fallback. The supported CLIs are `docker`, `nerdctl`, and `podman`, so any answer has to be expressible in flags all three accept.

## Decision

The workload container runs **`--network none`**. Reachability to the proxy is a **filesystem hole, not a network hole**:

1. The shared host-side proxy listens on a **per-principal UNIX domain socket**, bind-mounted **read-only** into the container at a fixed path.
2. A **statically linked forwarder binary built from this repo** (`cmd/fuse-egress-forward`) is also bind-mounted read-only. Inside the container it listens on `127.0.0.1` and relays to the mounted socket.
3. fuse injects `HTTP_PROXY`/`HTTPS_PROXY`/`ALL_PROXY` pointing at that loopback address, and sets `NO_PROXY` **explicitly empty** so no host is silently exempted.

All of this is emitted **only under `egress.mode: enforce`**, is established by the trusted side, and is **applied last in the options chain** — so no caller option and no model input can redirect it. In particular an `env_passthrough` entry naming `HTTP_PROXY` cannot override the injected value.

### Alternatives rejected — the load-bearing part of this record

- **A bare bind-mounted socket, no forwarder.** `curl` has no UNIX-socket *proxy* support (`--unix-socket` names the *target*, not a proxy), so ordinary clients simply cannot use it. Closing that gap with an in-image relay fails too: the sandbox image (`alpine:3.20`) ships no `socat`, and requiring one would make **containment depend on the operator's image** — a property that must not be delegated.
- **`--network container:<proxy-container>`.** Joining the proxy container's network namespace hands the workload that container's **real NIC**, re-opening precisely the general egress this change exists to close. Rejected on the same ground ADR-0044 rejected in-process allowlists: the boundary must not be something the workload is already inside of.
- **A per-container sidecar sharing a netns.** Structurally isolating and genuinely viable, but heavier: two containers per call, and warm-pool lifecycle coupling against ADR-0044's per-principal pooling. **Retained as the recorded escape hatch**, not chosen.

## Consequences

**The security property rests on `lo` having no route off-box.** Under `--network none` the only path out of the container is the mounted socket, so the single filesystem hole *is* the entire egress surface — which is what makes the proxy's allowlist authoritative rather than advisory. A future change that gives the container any NIC invalidates this ADR's reasoning wholesale, not partially.

**The forwarder becomes a distribution obligation.** fuse now ships an arch-matched binary artifact that must exist for enforce mode to work. It is **discovered next to the running executable and never taken from a config-supplied path** — a config-supplied path would let a contained model (via any config it can influence) name an arbitrary host file to be bind-mounted into the container, converting an egress control into a host-file-read primitive.

**Two assumptions were argued from Linux LSM semantics but NOT verified against a live daemon**, and are recorded here honestly rather than as settled fact:
1. that a `:ro` bind-mounted UNIX socket still accepts `connect(2)` (read-only mount semantics are argued not to gate socket connect, which is mediated as a socket permission);
2. that the socket's parent directory is created implicitly by the file mounts across all three supported CLIs.

Either being false is a straightforward implementation fix (drop `:ro` on the socket; mount the directory explicitly), not a reversal of this decision — but a reader should not treat them as verified.
