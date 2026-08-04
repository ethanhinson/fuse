---
name: model-compat-layer-for-tool-loops
description: Local models (qwen-coder:30b via Ollama) can get stuck calling the same read-only tool forever; a two-signal compat layer — tool-result hint + no-schema forced-text turn — breaks the loop reliably
metadata:
  type: feedback
  change: 1
  created: 2026-08-04
  updated: 2026-08-04
  promotion_state: candidate
  changes: [1]
  topics: [compat, tool-use, local-models, ollama]
---

Weaker or heavily-quantized local models can enter a hard tool-call loop on read-only tools (read_file, list_directory): they generate exactly N tokens of tool-call JSON, receive the result, and call the same tool again regardless of what the result says. System prompt nudges and tool_choice:"none" are both ineffective against Ollama's local runner (tool_choice is silently ignored).

**Fix:** two-signal compat layer in loop.go:
1. On first redundant read-only call: prefix tool result with "[Already retrieved…]" hint + inject user-role nudge after all tool results
2. On the next turn: send zero tool schemas — the model cannot call any tool and must produce text

**Why:** Models trained to always use tools before answering cannot be coached by prompt alone; removing the schemas is the only reliable circuit-breaker.

**How to apply:** The readOnlyTools map in loop.go controls which tools trigger the compat layer. Add any new idempotent read tools to that set. Write-tools (bash, edit_file, write_file) must NOT be in the set (re-execution has side effects).
