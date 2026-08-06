---
name: shell-parse-inspect-every-ast-channel
title: Shell-safety classifiers must inspect every AST channel that names a target — argv extraction alone drops redirects
promotion_state: candidate
changes: [17]
created: 2026-08-06
updated: 2026-08-06
topics: [security, shell, parsing, go, permissions]
---

When classifying parsed shell for safety (allowlists, read-only detection), enumerate every AST channel that can name a file or target — not just `CallExpr.Args`. In `mvdan.cc/sh/v3/syntax`, redirections attach to `*syntax.Stmt.Redirs`, so a walker that extracts argv sees `echo x > /etc/passwd` as a bare `echo x` and classifies it read-only: a complete out-of-workspace write primitive with zero prompts. The fix is a fail-closed guard at the statement chokepoint (`if len(st.Redirs) > 0 { return ErrUnparseable }`) — placed on `*Stmt`, it covers top-level statements, each compound-command operand, and re-parsed `bash -c` bodies for free.

**Why:** Whole-branch review of change 0017 (PR #15) found the auto-approve pipeline's parser collected only call arguments; the spec explicitly listed "redirection to a file" as fail-closed, every layer downstream trusted the segment list, and four read-only-listed commands (`echo`, `grep`, `cat`, `ls`) each yielded arbitrary-path writes. Same class as the Codex basename-collapse bug: the parse floor silently narrowing what upper layers see.

**How to apply:** When touching any shell-classification parser, audit the AST node docs for every field that can carry a word naming a target (`Redirs`, here-docs, process substitutions, assignments) and either classify or fail closed on each. Add adversarial corpus rows per channel asserting the *layer* that catches them.
