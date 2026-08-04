---
name: nil-safe-optional-tool-registration
description: Pass capability-injection functions as nil to omit context-sensitive tools from one-shot/test tool registries without branching at call sites
metadata:
  type: feedback
  change: 4
  created: 2026-08-04
  updated: 2026-08-04
  promotion_state: retained
  changes: [4]
---

# Nil-safe optional tool registration

When a tool should only be available in some runtime contexts (e.g. `skill` tool in shell mode but not one-shot mode), pass the capability as a nullable function pointer into the registry builder rather than forking the builder or using a boolean flag.

```go
func defaultToolRegistry(skillLookup func(string) (skills.Skill, bool)) *tools.Registry {
    // ...
    if skillLookup != nil {
        r.Register(tools.NewSkillTool(skillLookup))
    }
    return r
}
```

Call sites in one-shot / test contexts pass `nil`; the shell passes `set.Lookup`. No `if isShellMode` branching in the registry builder. The tool constructor itself gets the exact lookup function it needs — no global state, no registry introspection.

This pattern extends naturally: any future context-sensitive tool (e.g. a `session` tool only in long-running sessions) follows the same shape.
