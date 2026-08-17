# Founder Charter — the agency-sim eval prompt

This file is the canonical charter given to the founder agent. `run.sh` copies it
into a fresh workspace as `FOUNDER_CHARTER.md` and launches fuse one-shot with the
task: *"You are the founder. Read FOUNDER_CHARTER.md in the current directory with
your file tools, then execute it fully, end to end. Follow its process, rules, and
cost-optimization requirements exactly."*

The model-alias roster below maps to `~/.fuse/config.yml` aliases. Update the tiers
when the roster changes, but keep the tier *structure* (cheap / mid / code / one
expensive exception) stable — the cost-optimization pressure is part of the eval.

---

You are the FOUNDER of a brand-new web-design agency. You are running a compressed,
end-to-end company simulation. Your job: build the company, hire a team of AI agents,
and ship your first client product — **a website with a blog, for a cooking brand** —
plus the platform demo, branding, marketing collateral, and company strategy.

Work entirely inside the current working directory. Every artifact is a file here.

## The hiring mechanic (IMPORTANT)

You hire agents by spawning them with `spawn_agent`. Each candidate/employee is defined
by a `system_prompt` (their agent profile) and a `model` (their brain). Available model
aliases, by cost tier:

- CHEAP (prefer these): `qwen-cloud`, `deepseek-flash`
- MID (use when a role needs more depth): `glm`, `kimi`, `minimax`
- CODE SPECIALIST (local, free): `qwen3-coder` — strongest at writing code
- `deepseek-pro` — most expensive allowed; use for AT MOST one role, only if justified

You must OPTIMIZE FOR TOKEN COST: default every role to the cheapest model that can do
the job; promote to a mid model only when a cheap candidate's interview answer is
clearly inadequate.

### Process

1. **Strategy first.** Write `strategy/company-strategy.md`: mission, target market,
   service offering, pricing sketch, and the org design (which C-suite roles you need
   and why). Keep it tight — one page.
2. **Author agent profiles.** Write `team/profiles.md` containing, for each role
   (C-suite: e.g. CTO, CMO, Head of Design; plus individual contributors you need),
   a written agent profile: role name, responsibilities, the exact system prompt you
   will hire them with, and the model tier you think the role needs.
3. **Interview / hire.** For each role, INTERVIEW candidates: spawn a candidate with
   the role's system prompt on a candidate model, `label` like `interview-cto-qwen-cloud`,
   and a task holding ONE short role-relevant screening question (answer ≤ 150 words).
   Judge the answer. If the cheap candidate passes, hire that model; if not, interview
   ONE better model for the role. Record every interview verdict in
   `team/hiring-log.md` (role, model, question, pass/fail, one-line rationale, and the
   final hire). Interview at most 2 candidates per role. Hire 4–6 employees total.
4. **Build the product.** Dispatch your hired employees (spawn each with their profile
   system prompt and hired model, label like `cto-build-site`) to produce, under
   `product/`:
   - `product/site/` — the actual cooking-brand website: `index.html`, a blog index and
     at least 3 real blog posts as HTML pages (with real recipe content), shared
     `styles.css`, working internal navigation. Static files only; no build step.
     Use `qwen3-coder` for the code-writing spawns.
   - `product/demo.md` — a platform demo script: how the agency would demo this site
     to the client, page by page. The demo may describe ONLY what the shipped site
     actually contains — verify against the real files before writing.
5. **Brand + market it.** Employees produce, under `marketing/`:
   - `marketing/branding.md` — agency brand AND the cooking-brand identity: names,
     taglines, color palettes (hex), typography choices, logo direction.
   - `marketing/collateral.md` — launch collateral: 3 social posts, a cold-outreach
     email, a one-paragraph press blurb, and homepage hero copy.
6. **Close the loop.** Write `FOUNDER_REPORT.md`: what was built, the final org chart
   with each hire's model and why, total spawns used, what you would do differently.

## Rules

- You (the founder) coordinate; employees do the work. Delegate real tasks with real
  deliverables written to files.
- Keep each spawned task focused and bounded; tell employees to keep prose outputs
  under ~400 words (code files exempt).
- Use the blackboard tools to share decisions (brand name, palette, file layout)
  between employees so their outputs are consistent.
- If web_search is available, you may use it for at most 2 quick searches (e.g.
  competitor scan for the strategy doc); if it errors, proceed without it.
- Do not exceed ~24 total spawns. Do not use any `claude`/`sonnet` model alias.
- When everything is written, end your turn with a short summary of what exists.
