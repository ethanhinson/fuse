<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0004 — Skill Runtime](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0004-skill-runtime.md)**
<!-- docket:backlink:end -->

# Skill Runtime

**Change:** [0004-skill-runtime](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0004-skill-runtime.md)

## Context

Phase 1 shipped skill *discovery*: `internal/skills/loader.go` scans three directories, parses `SKILL.md` frontmatter, and makes skills available as slash commands in the shell. But skills cannot yet *do anything beyond injecting their body as a prompt*. Two things are broken:

1. **Skills can't invoke other skills.** Docket skills (and any future multi-step skill) call `skill("some-other-skill")` as a blocking step — the Claude Code `Skill` tool equivalent. Fuse has no such tool, so the model has nowhere to put the call.
2. **Third-party skills aren't slash-accessible.** Skills from `~/.claude/skills/` (docket, codeindex) don't set `slash_command` frontmatter — that field is fuse-only. So `SlashCommands()` returns an empty map for all of them; they show up in the system prompt block but can't be triggered.

Additionally, skill slash commands with trailing arguments (e.g. `/docket-new-change design the auth layer`) drop the args on the floor.

This change adds the three missing runtime pieces: a `skill` tool, auto-derived slash commands, and args forwarding. Docket is the acceptance test — after this change, `/docket-status` and `/docket-new-change <description>` work end-to-end.

## Goals

1. **`skill` tool** — the model can call `skill({"name": "docket-convention"})` and get the skill body back as a tool result. Any installed skill is reachable by name. This is the fuse equivalent of Claude Code's `Skill` tool.
2. **Auto-derived slash commands** — when `slash_command` frontmatter is absent, `SlashCommands()` falls back to `/<name>`. No changes to any upstream skill file.
3. **Args forwarding** — `handleSlash` appends `\n\nARGUMENTS: <rest>` to the skill body when trailing text follows the command. Matches the Claude Code convention that docket skills already read.

## Out of scope

- `context: fork` / subagent dispatch — docket skills specify this; fuse ignores it for now and runs them inline. The field is parsed and stored for future use.
- Trigger-based auto-invocation (`triggers:` frontmatter).
- Hooks system.
- `fuse skills` list subcommand (trivial follow-up, not needed for docket).

## Architecture

### 1. `skill` tool — `internal/tools/skill.go`

```go
type skillTool struct {
    lookup func(name string) (skills.Skill, bool)
}

func NewSkillTool(lookup func(string) (skills.Skill, bool)) Tool { ... }

func (t *skillTool) Name() string        { return "skill" }
func (t *skillTool) Description() string { return "Load an installed skill by name and return its full body." }
func (t *skillTool) Parameters() map[string]any {
    return map[string]any{
        "type": "object",
        "properties": map[string]any{
            "name": map[string]any{"type": "string", "description": "Skill name (e.g. \"docket-convention\")"},
        },
        "required": []string{"name"},
    }
}
func (t *skillTool) Execute(ctx context.Context, args string) Result {
    // unmarshal {"name": "..."}, look up, return body or error
}
```

The tool takes a `lookup` function so it has no import cycle (`tools` → `skills` is fine; the inverse isn't needed).

### 2. `skills.Set.Lookup` — `internal/skills/loader.go`

```go
// Lookup finds a skill by name. Returns false if not found.
func (s *Set) Lookup(name string) (Skill, bool) { ... }
```

Used by `NewSkillTool(set.Lookup)`.

### 3. Auto-derived slash commands — `internal/skills/loader.go`

`SlashCommands()` currently skips skills where `slash_command` is empty. Change: fall back to `"/" + sk.Name` when the field is unset.

```go
func (s *Set) SlashCommands() map[string]Skill {
    out := map[string]Skill{}
    for _, sk := range s.skills {
        key := sk.SlashCommand
        if key == "" {
            key = "/" + sk.Name
        }
        out[key] = sk
    }
    return out
}
```

`docket-status` → `/docket-status`. `codeindex:impact` → `/codeindex:impact`. No transformation — whatever the name field says is the slash command suffix.

### 4. Args forwarding — `internal/tui/shell_model.go`

In `handleSlash`, when the input matches a skill slash command, join any trailing fields and append as `ARGUMENTS:`:

```go
if sk, ok := m.slash[cmd]; ok {
    body := sk.Body
    if len(fields) > 1 {
        body += "\n\nARGUMENTS: " + strings.Join(fields[1:], " ")
    }
    return m.startPrompt(body)
}
```

### 5. Additional frontmatter fields — `internal/skills/parser.go`

Parse `context` and `agent` (docket skills and others use these) and store them on `Skill`. Not acted on yet — stored for future subagent dispatch:

```go
type frontmatter struct {
    Name         string `yaml:"name"`
    Description  string `yaml:"description"`
    SlashCommand string `yaml:"slash_command"`
    Context      string `yaml:"context"`  // e.g. "fork"
    Agent        string `yaml:"agent"`    // e.g. "docket-status"
}

type Skill struct {
    Name         string
    Description  string
    SlashCommand string
    Context      string
    Agent        string
    Body         string
    Path         string
}
```

### 6. Wiring — `cmd/fuse/run.go` + `cmd/fuse/shell.go`

The `skill` tool needs the skill `*Set` at agent build time. Minimal change: thread a skill lookup through to `defaultToolRegistry`.

`run.go`:
```go
func defaultToolRegistry(skillLookup func(string) (skills.Skill, bool)) *tools.Registry {
    r := tools.NewRegistry()
    for _, t := range tools.DefaultTools() { r.Register(t) }
    for _, t := range tools.CodeindexTools() { r.Register(t) }
    if skillLookup != nil {
        r.Register(tools.NewSkillTool(skillLookup))
    }
    return r
}
```

`buildAgentCore` gains a `skillLookup func(string) (skills.Skill, bool)` parameter and passes it to `defaultToolRegistry`.

`buildAgentWithRenderer` gains the same parameter. `runShell`'s `build` closure passes `set.Lookup`:

```go
build := func(a string, r agent.Renderer) (*agent.Agent, error) {
    return buildAgentWithRenderer(cfg, reg, a, r, verbose, skillBlock, set.Lookup)
}
```

One-shot mode (`buildAgent`) passes `nil` — no skill tool in one-shot, which is fine.

## Acceptance test: docket-status end-to-end

After this change, from `fuse shell`:

1. `/docket-status` — dispatched as slash command (auto-derived from name), body injected as prompt.
2. Model calls `skill("docket-convention")` → gets the convention markdown back as a tool result.
3. Model calls `bash` to run `docket.sh preflight` → gets the config block.
4. Session proceeds as a normal docket-status run.

`/docket-new-change design the auth layer`:
- `fields[0]` = `/docket-new-change`, `fields[1:]` = `design the auth layer`
- Skill body + `\n\nARGUMENTS: design the auth layer` is sent as the prompt.
- Model reads the arguments from the prompt and uses them as the change description.

## Key design decisions

**`skill` tool returns body only, no execution.** Skills are markdown instruction documents. The tool delivers the document into context; the model follows the instructions. There is no skill "execution engine" — the model is the engine. This keeps the runtime simple and matches how Claude Code works.

**Auto-derivation uses `/<name>` verbatim.** No normalization (no colon→dash transforms, no lowercasing). Whatever `name:` says in the SKILL.md frontmatter becomes the slash command. `docket-status` → `/docket-status`. `codeindex:impact` → `/codeindex:impact`. Normalization would silently break lookups when the model calls `skill("codeindex:impact")` but the key in the map was derived as `/codeindex-impact`.

**`skillLookup` is nilable; one-shot mode passes nil.** One-shot (`fuse "<task>"`) doesn't load skills today. The skill tool being absent there is intentional — not a phase 2 task, just the current scope.

**`context: fork` parsed but not dispatched.** Storing it now makes the future subagent dispatch change a one-field reader rather than a parser + dispatcher.
