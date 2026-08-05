<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0013 — ASCII art startup banner — shell init & fuse help](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0013-startup-banner.md)**
<!-- docket:backlink:end -->

# ASCII art startup banner — shell init & fuse help — results
Change: #13 · Branch: feat/startup-banner · PR: (opened at close-out) · Plan: docs/superpowers/plans/2026-08-05-startup-banner-plan.md · ADRs: none

## Verify (human)

<!-- The banner is a visual/terminal-facing artifact; automated tests assert the marker string and
     exit codes, but the rendered appearance is best confirmed by eye against a real built binary. -->
- [ ] `fuse help` renders the banner + command list and exits 0.
- [ ] `fuse` with no args prints the banner on stderr and exits 2 (usage path).
- [ ] Launching the interactive shell shows the banner in scrollback (survives the alt-screen switch).
- [ ] The Banner3-D wordmark reads cleanly at a standard terminal width (design-approved face).

## Findings

- **Wordmark face swapped mid-build (human-directed design change).** The spec/plan specified a
  classic slant wordmark; during review the human directed swapping it to the figlet Banner3-D face.
  Test assertions were updated to key on the stable `'########:'##` marker rather than the full
  rendered art, so the face can evolve without brittle golden-string tests. The human approved the
  final design in conversation.
- **Plain-ASCII enforced.** No ANSI color; a test guards that the banner contains only plain ASCII,
  matching the spec's maximum-compatibility goal.
- **Review-fix: quickstart wording corrected to real commands.** The initial quickstart lines named
  commands that did not match the actual CLI surface; they were corrected to `fuse "<task>"`,
  `fuse shell`, and `fuse help`.

## Follow-ups

- A `--no-banner` flag remains explicitly out of scope (see the change's Out of scope) — a candidate
  for a future change only if users request it.
