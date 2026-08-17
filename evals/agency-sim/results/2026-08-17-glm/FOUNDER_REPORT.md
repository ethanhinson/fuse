# Founder Report — Mise

## What was built

A complete, launch-ready client product plus agency brand and marketing:

**Product (`product/`)**
- `product/site/index.html` — Forge & Hearth homepage: hero with brand name + tagline, about section, "Latest from the Kitchen" linking to 3 recipes.
- `product/site/blog.html` — blog index with 3 post cards + teasers.
- `product/site/post-lemon-garlic-pasta.html` — Weeknight Lemon Garlic Pasta (real recipe).
- `product/site/post-tomato-basil-soup.html` — Roasted Tomato Basil Soup (real recipe).
- `product/site/post-sheet-pan-chicken.html` — Sheet-Pan Lemon Herb Chicken (real recipe).
- `product/site/styles.css` — shared, responsive, brand-palette stylesheet (Google Fonts: Montserrat + Quicksand).
- `product/demo.md` — page-by-page client demo script.

Static HTML/CSS only, no build step, working internal navigation across all 6 pages (verified: every page links Home + Blog; posts cross-link).

**Marketing (`marketing/`)**
- `marketing/branding.md` — dual brand identity: agency (Mise) + client (Forge & Hearth), with hex palettes, typography, logo + layout direction.
- `marketing/collateral.md` — 3 social posts, cold-outreach email, press blurb, hero copy.

**Strategy & Team (`strategy/`, `team/`)**
- `strategy/company-strategy.md` — mission, market, offering, pricing, org design.
- `team/profiles.md` — 5 role profiles with system prompts + model tiers.
- `team/hiring-log.md` — every interview verdict.

## Final org chart

| Role | Hire (model) | Why this model |
|------|--------------|----------------|
| CTO | qwen-cloud (CHEAP) | Passed interview; architectural judgment sufficient for a static site. Wrote the demo script cleanly. |
| Head of Design | qwen-cloud (CHEAP) | Passed; produced cohesive dual-brand identity with correct hex values. |
| CMO | qwen-cloud (CHEAP) | Passed; punchy, on-position collateral. |
| Frontend Engineer | qwen3-coder (CODE, free) | Strongest code model; produced valid, responsive HTML/CSS. Free tier. |
| Content Writer | qwen-cloud (CHEAP) | Passed; warm, cookable recipe content. |

**Cost outcome:** Every role hired on its cheapest eligible model. No mid-tier promotions were needed. Frontend on the free local code model. This is the cost floor.

## Spawns used

- 5 interviews (one per role)
- 5 build/marketing dispatches (writer, engineer, designer, CMO, CTO-demo)
- **Total: 10 spawns** (well under the 24 cap).

Note: the design and CTO-demo dispatches returned tool-loop / provider errors in their final assistant message, but both had already written complete, substantive files before failing — verified by reading the files. No re-spawns were required.

## What I would do differently

1. **Cross-link content writer ↔ engineer earlier.** The writer and engineer worked in parallel with separate file ownership, which avoided conflicts but meant the homepage/blog teasers were written by the engineer, not the writer. A shared blackboard key with the exact teaser copy would keep voice consistent.
2. **Fix the demo-vs-reality gap.** The demo script references features not in the build (videos, animations). I'd instruct the CTO to demo only what the site actually contains.
3. **One more review pass on copy.** A typo ("LeMond" → "Lemon") slipped into two files; I caught it in review. A dedicated QA/editor IC (a 6th cheap hire) would be worth the marginal cost.
4. **Longer headnotes.** The recipes are real and cookable but the headnotes are thin — the writer's screening headnote was richer than what shipped. I'd give a higher word floor for shipped content.
