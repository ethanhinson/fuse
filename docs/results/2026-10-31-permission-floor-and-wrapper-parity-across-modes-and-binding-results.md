<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0093 — Permission floor and wrapper parity across modes and bindings](../changes/active/0093-permission-floor-and-wrapper-parity-across-modes-and-binding.md)**
<!-- docket:backlink:end -->

# Permission floor and wrapper parity across modes and bindings - Results

**Human action:** No action is required to merge. This is a tightening of what permission rules approve, so read the release note in the PR body before shipping; two optional walkthroughs below confirm the behavior by hand.

## Outcome

Three gaps between the catastrophic-command floor and protective rules in auto mode, versus smart mode and the headless MCP server, are closed.

- In smart mode, a first-token `auto_approve` rule such as `bash:rm` no longer approves a catastrophic command (`rm -rf ~`, `rm -rf /`, `rm -rf ..`, `mkfs.ext4 ...`). With a human bound the call prompts with a "catastrophic command" reason; with only a policy stand-in bound (loop servers, headless MCP server) it is denied with a reason. `rm` or `dd` with an unresolved shell expansion (`rm -rf $HOME`) no longer matches a first-token allow either.
- Protective rules (`deny`, `ask`, `always_prompt`) now also match the command as written before wrappers such as `timeout`, `nice`, `env` and `bash -c` are peeled, so `bash:timeout*` style rules fire in auto mode (and argument-bearing forms in smart mode). Allow rules still match only the parsed, strict form.
- The headless MCP server gate now binds the workspace root and write roots, so workspace-ancestor deletions such as `rm -rf ..` are denied there like everywhere else.

Material departures from the spec are listed in the plan (parity rows use `env TZ=UTC make` and `bash -c "make"`; the smart end-to-end rule is `bash:timeout *`; the hosted-binding test sets a gateway URL). One side effect not in the spec: because the MCP server gate now has roots, in-workspace and scratch writes are auto-approved there in auto mode, where previously every mutation reached the approver.

## Human actions and testing

### Optional - see the floor in smart mode

Needs a built `fuse` and a config with `permissions.auto_approve: ["bash:rm"]` in smart mode (the default).

1. Run a loop server or interactive session and ask the agent to run `rm -rf ~` (do not approve it if prompted).
   Expected: a prompt (interactive) or a deny with reason starting "catastrophic command denied by rules floor" (headless); nothing is deleted.
2. Ask it to run `rm somefile` in the workspace.
   Expected: allowed without a prompt.

### Optional - wrapper rule in auto mode

Config: `permissions: {mode: auto, auto: {deny: ["bash:timeout*"]}}`. Ask the agent to run `timeout 5 touch ran`.
Expected: denied by the rules layer; `ran` is not created.

## Verification performed

Full suite (`make test`) green at the final head through the build gate. Two end-to-end loop-server tests (production wiring, scripted gateway) and a hosted-binding test cover the three parts; non-vacuity was checked by disabling each production change and confirming the tests go red. Whole-branch review (deep tier): 7 findings; 3 fixed in-branch (opaque-operand hole in the legacy allow, new floor-ordering tests, README/comment wording), 4 recorded below. No Anthropic models were used.

## Known issues and follow-ups

- Wrapper-named protective rules still do not fire in auto mode for command forms the parser refuses (`env FOO=1 make` with a non-inert assignment, `sudo ...`). Auto mode asks at the parse layer without consulting deny/ask rules; in a headless auto binding the policy stand-in then approves. Confirmed, pre-existing; the README now says the parity applies to parser-accepted forms. Suggested next action: add a raw-text protective fallback in auto mode's parse-failure branch. No existing change fits (checked 6); a new change a human captures.
- The "stand-in never approves a catastrophic command" guarantee covers parser-accepted commands only. `/bin/rm -rf /`, `sudo rm -rf /` and prompt-all mode under a stand-in are still approved headlessly. Confirmed, pre-existing. Suggested next action: under a policy stand-in, deny a parse-layer ask or run a raw-text catastrophic check. Related to #94 (approval provenance per gate), which touches the same stand-in; link it under `related:` if captured.
- The MCP server gate in auto mode now auto-approves in-workspace and scratch writes (and creates a session scratch directory). Intended parity, noted for the release note.
- `buildGate` binds the workspace root only when a gateway is configured; a gateway-less gate still cannot recognize workspace-ancestor deletions. Binding it unconditionally would also loosen degraded auto mode, so it was left alone.
- Minor: a non-catastrophic smart-mode bash call is parsed up to four times per decision. Recorded, not changed.
