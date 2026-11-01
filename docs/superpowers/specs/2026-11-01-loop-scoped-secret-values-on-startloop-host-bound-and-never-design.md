# Loop-scoped secret values on StartLoop, host-bound and never recorded

Change 0102 — Loop-scoped secret values on StartLoop, host-bound and never recorded

- Type: feat
- Priority: high

## Why

a networked server resolves `secret_ref` names only from the operator's `loop_server.secrets` (env or file on the server host, granted per tenant). A client that brings its own MCP servers has no way to bring its own credentials for them: the operator would have to put every client's token into the server process environment and restart, which is both operationally wrong and discloses each client's secret to the operator's config. The spec deliberately carries only secret names (it is recorded durably by `loop.harness`), so the values need a transport-level, memory-only channel.

## What changes

- `StartLoopRequest.secrets` (`repeated LoopSecret {name, value, hosts}`), documented as loop-scoped, in-memory only, never recorded, redacted from logs and errors; at most 32 entries, value at most 16 KiB.
- `harnessresolve.Resolver` consults loop-scoped secrets after operator secrets: an operator entry for the tenant wins; otherwise a loop-scoped entry resolves the name only when the destination URL's hostname is in that entry's `hosts` (a loop-scoped secret with empty `hosts` is a violation at `secrets[i].hosts`). Names must match `SecretRefPattern`.
- The Connect handler requires scope `secrets:inline` (unrestricted principals included) when `secrets` is non-empty, else `CodePermissionDenied`; the runtime seam receives the values through `LoopConfig` and they are dropped when the loop ends.
- `loop.harness` keeps recording the spec with names only; a redaction test asserts no loop-scoped value ever appears in the event store, logs or a `ValidationError` text.
- `sdk/fuse` `StartLoop` options gain `WithSecrets(...)`.
- ADR recording: loop-scoped secrets are transport-level, host-bound, memory-only; operator entries take precedence.

## Out of scope

a pluggable remote `SecretResolver` (file as later); secrets for `Send`; rotation during a loop; encrypting values at rest (there is no rest).

## Acceptance criteria

- A spec referencing `client_secret_ref: my-token` on `https://mcp.acme.test/` starts when the request carries `secrets: [{name: my-token, value: v, hosts: [mcp.acme.test]}]` and the MCP server (fake) receives `v`; the same request with `hosts: [other.test]` is refused with a violation at `mcp_servers[0].auth.client_secret_ref` naming the host rule.
- With an operator entry `my-token` for the tenant, the operator value is used and the loop-scoped value ignored (test asserts the fake server saw the operator value).
- A principal with scopes `[loop:start]` sending `secrets` gets `CodePermissionDenied` naming `secrets:inline`; with `[loop:start, secrets:inline]` it succeeds; an unrestricted principal succeeds.
- After the loop, grepping the event store directory, the server log and the error text of a forced failure finds no occurrence of the value.
- 33 entries or a 16 KiB+1 value is `CodeInvalidArgument`. `make test` green.
