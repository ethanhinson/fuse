---
name: yaml-plain-scalar-colon-space
slug: yaml-plain-scalar-colon-space
title: YAML plain scalars cannot contain ': ' — extract free-text fields with a line reader
hook: "YAML rejects unquoted ': ' in plain scalars — extract free-text fields (name, description) via a line reader, not yaml.Unmarshal"
promotion_state: retained
changes: []
created: 2026-08-04
updated: 2026-08-04
topics: [yaml, parsing, go]
---

YAML's plain (unquoted) scalar syntax forbids `: ` (colon + space) because the parser treats it as a mapping key delimiter. A `description:` value containing inline code like `` `skills: brainstorm:` `` causes `yaml: mapping values are not allowed in this context`, even though the value is clearly a string to a human reader.

**Rule:** For frontmatter fields whose values are free-form text (name, description, title), extract them line-by-line from the raw block rather than via `yaml.Unmarshal`. Reserve `yaml.Unmarshal` for simple identifier fields (`slug`, `context`, `agent`, flags) that will never contain `: `.

```go
// Extract free-text fields directly, then strip them before YAML-parsing the rest.
name, description := extractLineFields(head)
yaml.Unmarshal([]byte(stripLineFields(head)), &fm)
```

**How to apply:** Any parser that reads YAML frontmatter from user-authored markdown files (SKILL.md, change files, ADRs) should treat `name:` and `description:` as line-reader fields. If you control the file format, document that these fields must be single-quoted or double-quoted when they contain colons.

## War story

Found during live testing after change 0010. `docket-brainstorm/SKILL.md` has a description containing `` `skills: brainstorm:` ``. `skills.Load` returned an error and the shell exited before starting.
