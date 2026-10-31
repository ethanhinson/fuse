<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0089 — Static-credential MCP servers authenticate, and every loop reports its MCP servers](../changes/active/0089-static-credential-mcp-servers-authenticate-and-every-loop-re.md)**
<!-- docket:backlink:end -->

# Static-credential MCP auth and mcp.server events - results

**Human action:** No human action is required beyond reviewing and merging the PR.

## Outcome

MCP servers configured with the `static` auth type now authenticate: the resolved secret is sent as a bearer credential, the same as `bearer`. Previously they failed at connect with `unknown auth type "static"` and were silently skipped.

Every hosted loop now records one durable `mcp.server` event per configured MCP server (operator and spec), right after `loop.harness`, carrying name, transport, auth type, connected, error and the tool names exposed to the model. No credential is ever included. Unreachable servers are still skipped and the loop continues.

## Verification performed

- Golden test connects a fake streamable-HTTP MCP server for every auth kind in `SpecMCPAuthKinds()` (none, bearer, static), checks the credential is sent, tools are listed, and an `mcp.server` event is recorded without the secret value.
- Unreachable-server test confirms a `connected=false` event with an error and that the loop still runs.
- `make test` and `make lint` pass on the final head.
- Review: lean tier; findings are listed in the PR body.

## Known issues and follow-ups

- When a loop resumes, the MCP manager is rebuilt and servers are re-dialed, so the stream gets a fresh set of `mcp.server` events reflecting current state (suspected duplicates are by design). Practical impact: consumers counting events per loop see one set per run segment. No workaround needed. No existing change fits (checked 0).
