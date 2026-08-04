---
name: ollama-openai-compat-gaps
description: Ollama's OpenAI-compatible endpoint silently ignores tool_choice and strips Qwen3 thinking tokens to empty string; use LM Studio or llama.cpp server for local models instead
metadata:
  type: feedback
  change: 1
  created: 2026-08-04
  updated: 2026-08-04
  promotion_state: candidate
  changes: [1]
  topics: [ollama, local-models, lm-studio, compatibility]
---

Two confirmed gaps in Ollama's OpenAI-compatible shim when used as a local model backend:

1. **tool_choice:"none" silently ignored** — the model still returns tool_calls even when the request says not to. Workaround: send an empty tools array (the compat layer does this on forced-text turns).

2. **Qwen3 thinking tokens stripped to ""** — Ollama strips `<think>…</think>` content before returning the response through the OpenAI compat endpoint. If the model produces only thinking output, content is "". The Ollama native API supports `think: false` but this is not accessible through LiteLLM's gateway. `/no_think` in the system prompt does not help.

**Fix:** Switch local model backend to LM Studio (`lms server start`, port 1234) or llama.cpp server. Both expose a cleaner OpenAI-compatible API. fuse itself needs no code changes — only the LiteLLM config's `api_base` entries for local models.

**How to apply:** When adding local models to the fuse registry, prefer LM Studio over Ollama. Update `litellm_config.yaml` entries for local/* models to point at http://localhost:1234/v1.
