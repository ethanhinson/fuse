<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0101 — Operator spec policy: MCP host allowlist, private-network deny, registry-only models](../../changes/active/0101-operator-spec-policy-mcp-host-allowlist-private-network-deny.md)**
<!-- docket:backlink:end -->

# Operator spec policy implementation plan

Note: authored inline by the implementer (plan-writer dispatch not used).

- [ ] Task 1: config.SpecPolicyConfig under loop_server (trusted-only loader, validate, defaults).
- [ ] Task 2: harnessresolve spec policy checks (mcp url, research.custom.url, registry-only models) wired into Resolve, with tests.
- [ ] Task 3: proto SpecPolicy message on GetCapabilitiesResponse, regenerate stubs, map through capabilities/loopconnect/sdk, host wiring.
- [ ] Task 4: docs section and ADR; make test green.
