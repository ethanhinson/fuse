---
name: fail-closed-guard-calibrate-benign-set
title: A fail-closed security guard must carve out the common benign case, or prompt-fatigue trains users to blanket-approve
promotion_state: candidate
changes: [37]
created: 2026-08-06
updated: 2026-08-06
topics: [security, permissions, shell, ux]
---

When a fail-closed guard blocks a whole syntactic category, enumerate the benign members of that category and let them through — otherwise the guard fires so often on safe input that users learn to reflexively approve, which defeats it. Change 0017's C1 fix rejected *any* shell redirect (`len(st.Redirs) > 0`), which correctly closed `echo x > /etc/passwd` but also prompted on `wc -l ... 2>/dev/null | tail -5` — the single most common benign redirect idiom. First real auto-mode run stalled on exactly that. 0037 narrowed it: `/dev/null` targets and pure fd-duplications (`2>&1`, `>&2`) pass; every redirect naming a real path/var/substitution still fails closed.

**Why:** A security control that cries wolf on routine input is worse than a narrower one — it manufactures the exact "approve without reading" habit it exists to prevent. The correctness boundary (no arbitrary-path write) is preserved by classifying the target, not by banning the operator.

**How to apply:** When adding a fail-closed guard over a syntax class, immediately ask "what's the benign 80% of this class?" and carve it out with a *positive* allow-test (literal `/dev/null`, numeric fd-dup) that is itself hard to spoof — never a substring/prefix check. Verify at the real surface that a routine command in that class no longer prompts while the attack still does. Related: [[shell-parse-inspect-every-ast-channel]].
