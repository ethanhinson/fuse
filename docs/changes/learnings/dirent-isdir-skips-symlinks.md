---
name: dirent-isdir-skips-symlinks
slug: dirent-isdir-skips-symlinks
title: DirEntry.IsDir() returns false for symlinks — use os.Stat to follow them
hook: "DirEntry.IsDir() skips symlinked directories — fall back to os.Stat when it returns false"
promotion_state: candidate
changes: []
created: 2026-08-04
updated: 2026-08-04
topics: [go, filesystem, testing]
---

`os.ReadDir` returns `DirEntry` items whose `IsDir()` method reports the type of the **entry itself**, not its target. A symlink to a directory returns `false`. Any loop that does `if !e.IsDir() { continue }` silently skips every symlinked subdirectory.

**Rule:** When scanning a directory for subdirectories that may be symlinks (plugin dirs, skill dirs, config dirs), fall back to `os.Stat` — which follows symlinks — whenever `e.IsDir()` is false:

```go
if !e.IsDir() {
    info, err := os.Stat(filepath.Join(dir, e.Name()))
    if err != nil || !info.IsDir() {
        continue
    }
}
```

**How to apply:** Any plugin or extension discovery loop (`skills.Load`, tool scanners, config overlays) should use this two-step check. The common case — real directories — takes the fast path; only symlinks pay the extra `Stat` call.

## War story

Found during live shell testing after change 0010 landed. `~/.claude/skills/` contains symlinks to `/Users/ethanhinson/dev/docket/skills/*`. `skills.Load` called `e.IsDir()` and skipped every entry, loading zero skills despite 11 being installed. The TUI showed only the 4 builtins.
