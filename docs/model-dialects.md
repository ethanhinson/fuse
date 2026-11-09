# Model dialects

A dialect is one model family: what it does on the wire on top of fuse's
OpenAI-compatible request. The gateway speaks one wire format, but families differ in what
that format should carry. Anthropic models cache a prompt prefix only where the request
marks it; OpenAI-style providers cache automatically.

A dialect is a name, ID markers used for detection, a set of capability defaults, and two
optional hooks (`prepare` edits the request, `finish` edits a parsed reply). The code is in
`internal/model/dialect*.go`. A model picks its dialect with `dialect:`; unset, it is
detected from the model ID. `dialect_options:` then tunes that family for one model.

## Families

| capability | openai | anthropic | what it does |
|---|---|---|---|
| cache markers | off | on | marks prompt-cache breakpoints (`cache_control`) on the system prompt and the last two user or tool messages |
| reasoning field | arrived field | arrived field | the field earlier reasoning is replayed in; empty means the field it arrived in |
| recover tool calls | on | on | lifts a tool call written as XML text into a structured call |
| retry empty unstreamed | on | on | after an empty streamed reply, asks once more without streaming |
| attachments | off | on | image input when `attachments:` is unset and the model has no `vision` tag |

Detection looks at the whole model ID. `anthropic`, `claude`, `opus`, `sonnet` and `haiku`
select anthropic; anything else is openai. `retry empty unstreamed` is a family capability
only: it is not a config option.

## Config

```yaml
models:
  ds:
    id: cloud/deepseek-v4-flash
    dialect_options:
      cache_markers: true          # bool; default: family default
      reasoning_field: reasoning   # reasoning_content | reasoning | reasoning_text; default "" = arrived field
      recover_tool_calls: false    # bool
  haiku:
    id: cloud/claude-haiku-5.5
    dialect: anthropic             # plain string, still valid
    dialect_options: {cache_markers: false}
```

`dialect_options` accepts only `cache_markers`, `reasoning_field` and `recover_tool_calls`.
An unknown key or an invalid `reasoning_field` is a startup problem; `dialect_options`
that is not a mapping, or a non-boolean value, fails config loading. Image input has its
own `attachments:` key and is not repeated here.

On an openai-family provider, `cache_markers: true` sends the system prompt and recent
messages as content parts with `cache_control`; whether that helps depends on the provider
passing the parts through.

## Precedence

1. Family: `dialect:` when it names a known dialect, else detected from `id`, else `openai`.
   (An unknown `dialect:` is a config problem at startup; the adapter still tolerates it by
   detecting, as before.)
2. Each of `cache_markers`, `reasoning_field`, `recover_tool_calls`:
   `dialect_options.<key>` when set, else the family default. An empty `reasoning_field`
   counts as unset.
3. Image input: `cli/` id is false; else `attachments:` when set; else tag `vision` is true;
   else the family default.
4. Replay reasoning: `replay_reasoning: false` is off; else on. `reasoning_field` only
   matters when replay is on.
5. Helper calls (summarizer, relevance classifier, permission classifier, human router):
   family detected from the helper's model ID, family defaults, no options.

`cli/` models never reach the dialect layer; `dialect:` and `dialect_options:` on them are
accepted and ignored.

## Known limit: helper calls

Helper calls know only a model ID, not its config entry, so they ignore both `dialect:`
and `dialect_options:`. Applying a model's options to helpers would change the wire for
existing configs that set `dialect: anthropic` on an opaque alias with summarization on.
Threading the helper's model config through is a possible follow-up.

## Adding a family

1. Create `internal/model/dialect_<name>.go`:
   ```go
   const DialectGemini = "gemini"
   var geminiDialect = dialect{
   	name:     DialectGemini,
   	markers:  []string{"gemini"},
   	defaults: capabilities{recoverToolCalls: true, retryEmptyUnstreamed: true, attachments: true},
   	// prepare/finish only for wire edits a capability flag cannot express.
   }
   ```
2. Add `&geminiDialect` to `dialects` before `&openAIDialect` (position is detection priority).
3. Add `dialect_<name>_test.go`: detection cases and a fake-gateway wire test (copy the
   pattern of `TestDialectWire_*` in `dialect_options_test.go`).
4. Update `TestModelDialectIsValidated` in `internal/config` (it asserts the substring
   `anthropic, openai`) and the families table above.
5. A behaviour several families could want, or a user should be able to toggle, is a new
   capability, not a hook: add the field to `capabilities`, `model.DialectOptions` and
   `config.DialectOptions` (with its known-key list, validation and conversion), and the
   docs. If it is the replay-reasoning default, also replace `ModelConfig.DropReasoning`
   with a tri-state, which cannot express "explicitly on" today.

Runtime-loaded plugins are out of scope: Go's `plugin` package needs the identical
toolchain and is unsupported on Windows and in static release binaries, and a WASM or
subprocess boundary inside every request and stream would need a stable ABI over unexported
wire types. Families are small, reviewed in-tree code; per-user tuning is `dialect_options:`.
