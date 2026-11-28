# Plan 0014: Public website (serverdock.oday-bakkour.com)

**Status:** revision 2 APPROVED (2026-11-28) by `reviewer`, `security`, `accessibility` and `footprint`. **Revision 3** (real dashboard screenshots, a new maintainer requirement of 2026-11-28) is waiting for review by all four; steps 1 and 2 are not affected by it. The maintainer accepted the recommended answers (Q1–Q4, Q6–Q8); Q5 (Bot Fight Mode) is decided at step 6. · **Author:** planner (AI). The maintainer has not confirmed this plan, except where the [maintainer answers](#10-maintainer-answers-2026-11-28) are recorded. · **Date:** 2026-11-28

**Roadmap:** this is not a product milestone. It serves the README's "Open" and "Honest" promises and gives the project reach before the early usable version. It changes nothing in the binary, the agent or the web UI.

**Binding inputs:**
- the maintainer decisions and answers of 2026-11-28 (the URL, Cloudflare Pages, Astro in `site/`, the page list, the visual language of `docs/assets/og.html`, and Q1 to Q8 below);
- the README merged in PR #11 (`8854e76`), which is the source of truth for content;
- `CLAUDE.md`: report state honestly, make no unmeasured claims, add no telemetry, and never put secrets in logs or in the repo.

**Preconditions:**
- **P1 (met).** PR #11 (the README, `docs/assets/og.html` and `og.png`) is merged to `main` at `8854e76`, and the GitHub social preview has been uploaded. Rebase this branch on `main` before step 1.
- **P2: PR #7 must be merged before steps 3, 4 and 5.** PR #7 (commit `79296ac`, CI run 46147556661) does two things this plan needs:
  - it records the M3a CI bench figures in `docs/research/05-phase0-measurements.md` and `bench/results/`;
  - it reorders the milestones in `docs/roadmap.md`, which feeds the Roadmap page, the "what's next" section on Home and `status.json`.
  Steps 1 and 2 do not depend on it.

## 1. Goal

A fast, accessible, static public website at **https://serverdock.oday-bakkour.com** that:
- states the hook: *anyone can build a server manager; few take care of the details, the usage and the usability. AI can build what you ask for, but someone has to lead it and check the work*;
- backs every claim with a number or a link to evidence in the repo, giving the source and the date;
- publishes the public docs from `docs/*.md` at build time, so the site never drifts from the repo;
- is honest about status: **pre-release, Phase 1, no install script yet**;
- meets the product's own accessibility bar, and ships no JavaScript, no tracking and no third-party requests.

## 2. Out of scope

- Install instructions, downloads or a "Get started" flow. There is no install script until the early usable version, and the site says so.
- Blog, newsletter, search, comments, translations (English only), and a theme toggle (Q7).
- Any analytics or RUM, including Cloudflare Web Analytics. Third-party fonts, badges, embeds and CDNs.
- Rendering `docs/plans/`, `docs/research/`, `docs/wireframes/`, `docs/accessibility/` or the `docs/diagrams/` sources as pages (Q1). These are linked on GitHub instead.
- An interactive API reference. The site links to `docs/api/openapi.json`.
- Pages Functions, `_worker.js`, `_routes.json` or any server-side code. A gate enforces this (section 3.5).
- Preview deploys on PRs (Q4). Each PR gets the built site as a CI artifact instead.
- The `/styleguide/` page in production. It is built only in test builds (section 3.4).

## 3. Design

### 3.1 Information architecture

```
/                 Home
/features/        Features
/benchmarks/      Benchmarks
/roadmap/         Roadmap
/docs/            Docs index (generated)
/docs/<slug>/     One page per published doc (generated)
/security/        SECURITY.md (generated) + link to the threat model
/contributing/    CONTRIBUTING.md (generated)
/404.html         h1 "Page not found", links to Home and Docs
/.well-known/security.txt
```

**Header.** The header is never sticky, at any size. It holds:
- the logo and "ServerDock";
- the main nav: Features, Benchmarks, Roadmap, Docs, and "GitHub" (with visually hidden text " (on GitHub)");
- a **status pill** reading "Pre-release · Phase 1" that links to `/roadmap/#status`. Its accessible name starts with its visible text, followed by visually hidden text ". See project status" inside the link. It has no `aria-label`.

**Footer.** Inside `<nav aria-label="Footer">`:
- AGPL-3.0;
- "No tracking, no cookies, no third-party scripts. Hosted on Cloudflare Pages.";
- Security and Contributing;
- the site's source on GitHub;
- "Built with AI agents, led by a human".

**Navigation without JS** (accessibility 5):
- The main nav links are rendered twice:
  - `<nav aria-label="Main">`, shown at widths of `48rem` and up;
  - `<details><summary>Menu</summary><nav aria-label="Main menu">…</nav></details>`, shown below `48rem`.
- The hidden copy uses `display:none`, so only one copy is ever in the accessibility tree.
- The docs sidebar uses the same pattern: `<nav aria-label="Docs">` on wide screens, `<details><summary>Docs pages</summary>` on narrow ones.
- Every `<summary>` has visible text, a target of at least 44×44 px, the focus ring, and a visible state marker that is more than a rotating arrow: a CSS-only "+" / "−" set from `details[open]`. The summary's name stays "Menu". The open or closed state comes from `<details>` itself, so it is never added to the name.
- The navs are named Main, Main menu, Docs, Docs pages, On this page and Footer.
- Breakpoints are in `rem`, so 200% zoom gets the narrow layout.

### 3.2 Page outlines and copy direction

The copy is plain, short and concrete, in the README's voice. The README is the source of truth. A PR that changes a claim in the README also changes the site in the same PR; the reviewer checks this.

**Home**
1. **Hero.** Two columns that stack on phones.
   - **Heading.** The h1 is **og.html's line**: "Anyone can build a server manager. **We sweat the details.**" The README uses its own variant, "Few sweat the details." The h1 must fit in two lines at 320 px.
   - **Sub line and chips.** The chips look like links and make sense on their own ("54 MB idle RAM: see benchmarks", and so on).
   - **Deploy card** (accessibility 4). *[Revision 3: the hero shows the real "live deploy" screenshot instead, if the maintainer confirms R3-Q1. This HTML card is used only if the answer is no.]* A `<figure>` holding:
     - an `<ol>` of stages. Each stage has a visible ✓, a visually hidden state word ("done") and the stage note;
     - a result line;
     - the figcaption "Example of a blue-green deploy in ServerDock, not a live deploy".

     The card has no `role=img` and no `aria-live`. The optional entrance animation never changes the reading order, and its final frame is the static state. Every stage label must be true for M3a; the reviewer checks it against plan 0004.
   - **Meter** (reviewer 3, accessibility 3). The visible text "54 MB of 1 GB" carries the meaning.
     - The bar is a decorative inline `<svg aria-hidden="true">`. Its fill is a `<rect>` whose `width` **attribute** is computed at build time from `benchmarks.json` as value ÷ 1024: 54/1024 = **5.3%**. (og.html says 5.4%; the site corrects this in its own copy only.)
     - It is a fixed figure, not `<meter>`, `<progress>` or `role=meter`.
     - Track and fill have at least 3:1 contrast in both themes. In forced colours the fill uses `CanvasText`/`Highlight`.
2. **Status callout.** "Pre-release. Deploying from an image is built and tested; there is no install script yet." It names the early-usable milestone as `roadmap.md` lists it after P2, and links to the Roadmap.
3. **"The details we care about".** Eight cards from the README table.
   - Each card has an Evidence link with a unique name, such as "Evidence for Light" (using visually hidden text), plus "on GitHub" when the link leaves the site.
   - The Light card's comparison ("popular panels idle at 0.7–1.2 GB") is presented as **the conclusion of our research summary**, not a measurement, and links to Benchmarks Table B.
4. **"Built with AI, led by a human".**
   - The pipeline as an `<ol>` styled like the deploy stages.
   - The README's four bullets.
   - Links to `CLAUDE.md` and `.claude/agents/` on GitHub.
5. **"What works today (M1–M3a)" and "What's next".** The next milestones come from `status.json`, in the order `roadmap.md` gives after P2.
6. **Closing line.** A link to Build from source, labelled as for contributors only.

**Features**
- **"Works today"**: M1–M3a, from the README list.
- **"Planned"**: grouped by phase from `docs/features.md`. Planned items get the text tag "Planned" and never a tick.
- A link to `/docs/features/`.
- No feature is shown as available before its milestone closes.

**Benchmarks** (reviewer 4)
- **Method.** What VmRSS means; the S2 and S4 scenarios; the difference between the CI runner, a local container and a 1 GB VPS; medians of 3 runs. Dates use `<time datetime>`. SHAs wrap (`overflow-wrap:anywhere`).
- **Table A: ServerDock.** Each row shows the value, environment, commit, date and source. The rows:
  - S2 live watcher (CI), S4 (CI) and idle CPU (CI), using the PR #7 figures;
  - binary size (gzip);
  - SPA first-load JS;
  - the zero-downtime recording;
  - the restart outage.
- **Table B: other panels.** Idle RAM and official minimums only, cited and dated 2026-11-20 (Q2).

  | Row | Displayed source (`primary_url`) | Checked source (`source_path`, line at the time of writing) |
  |---|---|---|
  | Coolify official minimum: 2 CPU / 2 GB | coolify.io installation docs | `docs/research/01-coolify-dokploy.md` l.11 |
  | Dokploy official minimum: 2 GB | docs.dokploy.com installation docs | research 01 l.42 |
  | Dokploy app container 800–850 MiB, about 1 GB on a fresh install | Dokploy issue #3909 | research 01 l.46 |
  | *Research conclusion, not a measurement:* "real idle cost of both is about 0.7–1.2 GB on the control plane" | link to research 01 on GitHub | research 01 l.66 |

  The table states that no primary source gives a total idle figure for Coolify. The wording is neutral ("reported in issue #3909"), with no CVE or security claims.
- **"Not measured yet" box.** The real 1 GB VPS check, arm64 (reported, not gated) and pull-time buffers. Any table cell without a number reads "not measured yet", never blank or a dash.
- **Data and the source check.** Every row comes from `site/src/data/benchmarks.json`.
  - Fields: `label`, `value`, `match_label`, `match_value`, `source_path` (checked), `source_anchor`, `primary_url` (displayed only), `environment`, `commit`, `date`.
  - The build fails unless:
    - (a) `source_path` exists;
    - (b) `match_label` and `match_value` both appear **on the same line** of that file. `match_value` must match on token boundaries: no digit, `.` or `,` directly before or after it, so `54.0` does not match `154.0` or `54.05`;
    - (c) `source_anchor` matches a heading in that file, using GitHub's slug rules.
- **Tables** (accessibility 7).
  - Each table has a `<caption>`, `scope=col`/`scope=row`, and a wrapping `<div role="region" tabindex="0" aria-labelledby="<caption id>">`.
  - Below `37.5rem` the first column is sticky.
  - The sticky first column takes at most 40% of the region's width at 320 px.
  - Below `37.5rem`, a visible text cue, "Scroll sideways for more columns", always shows.
  - The table elements keep their native `display`, so their table semantics are not lost.

**Roadmap**
- **Status block** (`#status`), built from `site/src/data/status.json`: the phase, milestones done, next milestones (from `roadmap.md` after P2), "Install script: not yet", and "as of <time>".
- **The full `docs/roadmap.md`**, rendered below the status block.
- **`roadmap-phases.svg`**, shown as `<img role="img" alt="…">` with a long description that links to the matching headings. It sits on a solid light panel in both themes and is checked for legibility in forced colours.
- **Updates.** The PR that closes a milestone updates `status.json`; the reviewer checks it.

**Docs**
- **Index.** Grouped as:
  - Start here: Vision, Features, Architecture, Roadmap;
  - Security: Threat model;
  - Design: UI, Decisions/ADRs;
  - Reference: the OpenAPI document on GitHub;
  - Engineering records: Plans, Research, Wireframes, as GitHub links with one line each.
- **Each doc page** has:
  - the doc's own `#` heading as the single h1;
  - "Source: docs/<file>.md (on GitHub)";
  - "Last changed <time>", from `git log -1 --format=%cs -- <path>` (the date only; no names or emails are read);
  - "Edit <title> on GitHub";
  - a table of contents in `<nav aria-label="On this page">`.
- **Repo URL.** One constant in `site/src/config.ts`.

### 3.3 Docs pipeline

- **The glob is the allowlist.** `site/src/docs.config.ts` sets the loader patterns:
  - `vision.md`, `features.md`, `architecture.md`, `roadmap.md`, `security.md`, `ui.md`;
  - `decisions/*.md`. This wildcard is accepted on purpose, because every ADR is public;
  - the root `SECURITY.md` and `CONTRIBUTING.md`.

  Everything else is unpublished by default. A build gate checks that the set of `dist/docs/*` routes **equals** the expanded allowlist. The file is in CODEOWNERS.
- **Loading.** Astro's built-in content collection `glob()` loader, with a base outside the project root (`../docs` and `..`). There is no copy step.
  - Step 3 first confirms that the loader accepts that base, and whether dev needs `..` in Vite `server.fs.allow`. If the loader cannot do this, the fallback is a 30-line custom loader using `node:fs`.
  - The docs have no front matter. The title is the first `#` heading. The description is the first paragraph, cut to 160 characters.
- **Link rewriting.** A local remark plugin, `site/src/lib/rewrite-links.mjs` (about 80 lines, no new dependency), rewrites each relative link and image in a published doc:
  1. Resolve the target with `realpath`, relative to the doc's repo path. **Fail the build on any path outside the repo root.**
  2. If the target is a published doc, link to its site route plus the anchor.
  3. If it is any other repo file, link to `<repo>/blob/main/<path>` (folders use `/tree/main/`) and add visually hidden " (on GitHub)" to the link text.
  4. If it is an image under `docs/`, copy it to `/docs-assets/` and give it `width`, `height` and `loading="lazy"`.
  5. If the target is missing, **fail the build** with the file and line.
  6. Leave absolute `http(s)` links as they are, with no `target="_blank"`.

  Heading ids follow GitHub's slug rules; step 3 tests this on the real docs.
- **Tables.** A rehype step wraps every table in the region described in section 3.2.
- **Code.** Prism (class-based, no inline styles). Its token colours are in the contrast table.
- **Raw HTML in docs.** This is trusted repo content, but it still passes through the CSP and the gates (no `<script>`, `on*=`, `style=` or `<style>`). So a doc cannot add inline styles, including `style=""` inside inline SVG.

### 3.4 Design system

- **Tokens.** `site/src/styles/tokens.css`. The dark values use the same names and values as og.html.

  | Token | Dark (og.html) | Light (starting values; the unit test is the gate) | Used for |
  |---|---|---|---|
  | `--bg` | `#0b0f14` | `#ffffff` | page |
  | `--bg2` | `#111821` | `#f4f7fa` | sections |
  | `--card` | `#111821` | `#ffffff` | cards, table regions |
  | `--code-bg` | `#0e141b` | `#eef2f6` | code |
  | `--text` | `#e8eef5` | `#0b0f14` | body text |
  | `--muted` | `#8b9bb0` | `#52627a` | secondary text |
  | `--accent` / `--accent-text` | `#3ddc97` / `#3ddc97` | `#3ddc97` (fills only) / `#0a7a50` | brand green |
  | `--accent2` / `--accent2-text` | `#5ab0ff` / `#5ab0ff` | `#5ab0ff` (fills only) / `#1f66b8` | links, focus ring |
  | `--warn-text` | `#f5b74a` | `#8a5a00` | warnings |
  | `--line` | `#1f2a36` | `#e3e8ee` | **decorative only** |
  | `--border` | `#5b6b80` | `#7a8899` | boundaries that matter (≥ 3:1) |
  | `--meter-track` | `#1a2430` | `#d5dde6` | meter track, with a 1 px `--border` outline that stays in forced colours |
  | `--meter-fill` | `--accent` | `--accent-text` (`#0a7a50`) | meter fill |
  | `--prism-*` | listed explicitly | listed explicitly | one token per Prism class used (comment, keyword, string, number, function, operator, punctuation, …). All are in the contrast test, and the list must be complete in `tokens.css` before step 2 closes |

  - **Contrast test** (`site/test/contrast.test.mjs`). It parses `tokens.css`, computes WCAG ratios and checks both themes:
    - every text token, including the Prism token colours, is at least 4.5:1 on each of `--bg`, `--bg2`, `--card` and `--code-bg`;
    - `--border`, the focus ring, and the meter fill against its track are at least 3:1.
  - **Token-match test.** A second test checks that the dark tokens still equal og.html's `:root`.
- **Surfaces.**
  - Text sits only on solid surfaces.
  - The og.html radial glows and 40 px grid appear only in areas without text: the hero margins and behind the card's outer edge.
  - An axe color-contrast result in "incomplete" fails the build, unless it is listed with a reason for the manual pass.
- **Theme.**
  - `:root { color-scheme: dark light; }`.
  - Dark is the default design; light applies under `prefers-color-scheme: light` (Q7).
  - A `prefers-contrast: more` block uses `--text` for `--muted` and `--border` for `--line`.
- **Type and layout.**
  - The system font stack, with no webfonts.
  - Font sizes and breakpoints in `rem`.
  - `overflow-wrap:anywhere` on SHAs, URLs and paths.
  - Text spacing (WCAG 1.4.12) must not clip text, so text containers have no fixed heights.
- **Focus and skip link.**
  - `:focus-visible` shows a 2 px ring with a 2 px offset in `--accent2-text`. It has at least 3:1 contrast in both themes and is never clipped by `overflow:hidden`.
  - The skip link is the first tab stop. It is visible on focus with a solid background of at least 3:1 and targets `<main id="main" tabindex="-1">`.
  - On docs pages the sidebar comes before `main` in the DOM, so the same skip link jumps past both the header and the sidebar.
  - With no sticky header, focus is never hidden (WCAG 2.4.11). `scroll-margin-top` is still set for anchor targets.
- **Targets.** These are at least 44×44 px: nav links, summaries, the pill, chips, sidebar items, footer links, the Evidence, Edit and Source links, and the "On this page" links. Links inside running text follow the 24 px rule.
- **Motion.** The optional card entrance animation, smooth scrolling and the `details` transition all sit inside `prefers-reduced-motion: no-preference`. Nothing else animates.
- **Forced colours.**
  - No `forced-color-adjust:none` on text.
  - The stage dots and the meter fill use system colours (`CanvasText`, `Highlight`).
  - The pill, chips, callouts, cards and table regions have real borders.
  - Meaning never depends on colour alone.
- **Language.** `lang="en"`, and a `lang` attribute on any non-English fragment.
- **Print.** A print stylesheet: light colours, no nav, link URLs shown after the links.
- **Favicon.** An SVG of the og.html logo on a solid tile, readable on both light and dark tab bars.
- **Components.** Chip, card, stage list, meter, evidence link, status pill, callout (info/warn), table region, doc layout, `<details>` nav, skip link.
- **`/styleguide/`.** Shows every component, including the focus ring, summary, skip link and a scrolling table. It is built only when `SITE_STYLEGUIDE=1`, which test builds set. A gate checks that the production build has no `/styleguide/`.
- **Meta per page.**
  - `<title>` "<Page> · ServerDock", `description`, and a `canonical` that always uses the custom domain.
  - Open Graph: `og:title`, `og:description`, `og:url`, `og:type`, and `og:image` = `/og.png` (1280×640) with `og:image:alt`.
  - `twitter:card=summary_large_image`, and `theme-color` for both schemes.
  - `og.png` is **copied from `docs/assets/og.png` at build time** and never committed twice.
  - `robots.txt`, and a `sitemap.xml` from a small endpoint.
- **`/.well-known/security.txt`** (security NIT, Q8):
  - `Contact:` the GitHub private advisory URL from SECURITY.md;
  - `Expires:` at most 11 months ahead, staying under RFC 9116's "less than a year". It is renewed in the same PR as every second 90-day token rotation;
  - `Canonical:` `https://serverdock.oday-bakkour.com/.well-known/security.txt`;
  - `Policy:` the SECURITY.md URL;
  - `Preferred-Languages: en`.

  The build fails when `Expires` is less than 30 days away. Step 6 confirms that private vulnerability reporting is turned on in the repo settings.

### 3.5 Build, gates and deploy

**`site/`.** Holds `package.json`, `package-lock.json`, `astro.config.mjs` and `.nvmrc` (the same Node version as `web/.nvmrc`). Configuration:
- `output: 'static'`, `build.inlineStylesheets: 'never'`, `trailingSlash: 'always'`;
- `image.service: passthroughImageService()`, so `astro:assets` and `sharp` never run at build time. Step 1 confirms that the build works with `--ignore-scripts`, which skips sharp's install script;
- no client islands;
- `ASTRO_TELEMETRY_DISABLED=1` in the npm scripts and in CI.

**`site/deploy/`.** Its own `package.json` and `package-lock.json` containing **only `wrangler`**, at an exact version (security B1, footprint 3).

**`site/public/_headers`**:
```
/*
  Content-Security-Policy: default-src 'none'; script-src 'none'; style-src 'self'; img-src 'self'; font-src 'none'; connect-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; upgrade-insecure-requests
  Referrer-Policy: no-referrer
  X-Content-Type-Options: nosniff
  Permissions-Policy: camera=(), microphone=(), geolocation=(), usb=(), payment=()
  Cross-Origin-Opener-Policy: same-origin
  Strict-Transport-Security: max-age=31536000
/_astro/*
  Cache-Control: public, max-age=31536000, immutable
https://:project.pages.dev/*
  X-Robots-Tag: noindex
https://:version.:project.pages.dev/*
  X-Robots-Tag: noindex
```
`style-src 'self'` also blocks `style=""` in inline SVG and in raw HTML from docs. The gates make sure nothing depends on inline styles.

**Build gates.** Gates 1–7 live in `site/scripts/gates.mjs`, which imports **only `node:` built-ins** and needs no `node_modules`. Gates 8–9 live in `page-weight.mjs`. Both run in the build job. The deploy job runs `gates.mjs --dist-only` (gates 1–5) again. The build fails on:
1. any `<script>` element, `on*=` attribute, `style=` attribute or `<style>` element in `dist`;
2. a missing `dist/_headers`, or one without `script-src 'none'`;
3. a missing `dist/.well-known/security.txt`, or one whose `Expires` is less than 30 days away;
4. any `dist/_worker.js`, `dist/functions/` or `dist/_routes.json`;
5. `/styleguide/` in a production build;
6. `dist/docs` routes that differ from the allowlist;
7. broken internal links or anchors. Every same-site `href`/`src` in `dist/**/*.html` must resolve to a file, and every `#id` must exist;
8. page weight over budget (section 4, `site/scripts/page-weight.mjs`);
9. the `dist` total over budget (section 4);
10. *(Revision 3, in `gates.mjs`)* the screenshot checks, described in Revision 3 under "Checks";
11. *(Revision 3, in `page-weight.mjs`)* the per-image and per-page image ceilings (section 4).

The benchmark source check and the link rewriter fail inside `astro build` itself.

**Local server for tests.** `site/scripts/serve.mjs` (`node:http`, about 60 lines, no dependency) serves `dist` with the `/*` rules from `_headers`, so Playwright runs **under the real CSP**. Any CSP violation in the console, or any request to another host, fails the test.

**CI workflow `.github/workflows/site.yml`.** Separate from `ci.yml`.
- **Not a required check.** The workflow is path-filtered, and a skipped run would block a required check. If the site build breaks on `main`, only the deploy fails and the last good version stays live.
- **Triggers.** `pull_request`, `push` to `main` and `workflow_dispatch`, for paths `site/**`, `docs/**`, `SECURITY.md`, `CONTRIBUTING.md` and `.github/workflows/site.yml`.
- **Top level.**
  - `permissions: contents: read`.
  - Actions pinned by SHA.
  - Workflow-level `concurrency: { group: site-${{ github.event.pull_request.number || github.ref }}, cancel-in-progress: ${{ github.event_name == 'pull_request' }} }`.
    - Every push and dispatch run on `main` shares the group `site-refs/heads/main`, and none is cancelled.
    - GitHub keeps one run going and at most one pending, and a newer pending run replaces an older pending one.
    - So runs on `main` go in order, and the newest commit is always the last to deploy.
- **Job `build`** (every trigger, no secrets):
  1. Check out with `fetch-depth: 0` and `persist-credentials: false`.
  2. `npm ci --ignore-scripts`, `npm audit --audit-level=high`, `astro check`.
  3. Production build, then the gates.
  4. Test build with the styleguide, served by `serve.mjs`.
  5. Playwright + axe, with `~/.cache/ms-playwright` cached.
  6. `upload-artifact` of the production `site/dist`, with **`include-hidden-files: true`** (for `.well-known/`), kept for 7 days.

  **Job time ceiling: 10 minutes.** Step 2 measures the job time and records it in this plan. If it is over the ceiling, the footprint reviewer decides at step 2 what moves (section 7). `timeout-minutes: 15`.
- **Job `deploy`** (security B1–B3, reviewer 5/6).
  - **Conditions.**
    - `needs: build`.
    - `if: github.ref == 'refs/heads/main' && (github.event_name == 'push' || github.event_name == 'workflow_dispatch')`.
    - `environment: production`.
    - `permissions: {}`. Step 6 confirms that the sparse checkout of the public repo still works with no permissions. If it does not, the job gets `contents: read` only.
    - Job-level `concurrency: { group: site-deploy-main, cancel-in-progress: false }`. One group covers every deploy, so deploys queue and never overlap.
    - A `workflow_dispatch` on `main` can redeploy, for example after a token rotation. A dispatch on any other ref only builds.
    - **No head-of-main check.** Because of the path filter, a later push that does not touch the site would make that check skip a site change forever. Ordering comes from the two concurrency groups.
  - **Steps.**
    1. Sparse checkout of **`site/deploy/` and `site/scripts/gates.mjs` only**, with `persist-credentials: false`.
    2. Run `npm ci --ignore-scripts` in `site/deploy`.
    3. Download the artifact from the current run (no `run-id`, no `github-token`) to `$RUNNER_TEMP/dist`.
    4. Run `node site/scripts/gates.mjs --dist-only "$RUNNER_TEMP/dist"` for gates 1–5. It uses only `node:` built-ins and runs **before** the only step that gets the token.
    5. With `working-directory: site/deploy`, run `npx --no-install wrangler pages deploy "$RUNNER_TEMP/dist" --project-name=serverdock --branch=main --commit-hash=$GITHUB_SHA`.
  - **Secrets.** Only the wrangler step has step-level `env`: `CLOUDFLARE_API_TOKEN: ${{ secrets.CLOUDFLARE_API_TOKEN }}`, `CLOUDFLARE_ACCOUNT_ID: ${{ vars.CLOUDFLARE_ACCOUNT_ID }}`, `WRANGLER_SEND_METRICS: "false"`.
  - **Fallback.** Step 6 confirms whether `wrangler pages deploy` needs workerd's install script, which `--ignore-scripts` skips. If it does:
    - the fallback is a plain `npm ci` (scripts allowed) of the **same `site/deploy` lockfile**, with the token not in the environment during the install;
    - `cloudflare/wrangler-action` (Cloudflare's own) may only run the wrangler that is already installed, never install its own, and using it needs security sign-off at step 6.
  - **Honest state.** The summary reports "deployed <url>" only after wrangler exits 0. A failure reads "failed", and the last good version stays live.
- **Bench and integration gate** (security B4, reviewer 2).
  - `docs_only` in `.github/scripts/bench-needed.sh` gains exactly `':(exclude)site/'`. The check for `.go` files anywhere still applies.
  - The `changes` job's output decides **both `bench` and `integration`** in `ci.yml`, so a site-only PR skips both.
  - Update the `docs_only` comment, the `ci.yml` comments at l.278–280, and the `architecture.md` tree at l.249 (add `site/` and `site.yml` lines).
  - `.github/workflows/site.yml` is **not** excluded, so changing it still runs the bench. Excluding it would need its own security sign-off and is not proposed.
- **Repo housekeeping.**
  - `.github/dependabot.yml` gets npm entries for `/site` and `/site/deploy`.
  - `CODEOWNERS` gets `/site/public/_headers`, `/site/scripts/gates.mjs`, `/site/src/docs.config.ts` and `/site/deploy/` (`/.github/` is already covered).
  - `.gitignore` gets `/site/dist/`, `/site/.astro/` and `/site/node_modules/`.
  - `.dockerignore` gets `site/node_modules`, `site/dist` and `site/.astro`.
- **Domain** (Q3/Q5).
  - Same Cloudflare account as the zone: Pages → Custom domains creates the proxied CNAME to `serverdock.pages.dev`.
  - Dedicated account: add a **DNS-only** CNAME in the zone, and Pages validates it. Test this before committing to the dedicated-account option.

## 4. Footprint budget

**Product.** No change: `site/` is not embedded, and no Go code changes. The footprint reviewer confirms that the `go:embed` patterns and the bench inputs do not include `site/`. In CI, site-only PRs **save bench and integration minutes**, but overall CI time still grows by the site job on PRs that touch `site/` or `docs/`.

**Site.** These budgets are **ceilings**. There is one metric everywhere: **each file compressed with gzip -9, summed**.
- **The script: `site/scripts/page-weight.mjs`.**
  - For each HTML page in `dist`, it sums the HTML, every same-site `<link rel=stylesheet>`, every `<img src>`, every `url()` inside the linked CSS, and the favicon.
  - Requests are counted as 1 (the HTML) plus the unique same-site URLs found in those attributes and in CSS `url()`. It uses attribute regexes, which is acceptable here because the input is our own build output; the Playwright check for requests to other hosts backs it up.
  - It prints a table to the job summary.
  - A unit-test fixture exceeds each budget once.
- **Ceilings.**

  | What | Ceiling |
  |---|---|
  | JavaScript | **0 bytes** on every page |
  | Webfonts | 0 bytes |
  | CSS | one file, at most 15 KB |
  | Home, non-image (HTML + CSS + favicon) | at most 80 KB *(unchanged)* |
  | Home, images | *(Revision 3)* at most **190 KB** worst case; exactly 1 eager image (≤ 120 KB), the rest `loading="lazy"` |
  | Home, requests on first view | *(Revision 3)* at most **5** (lazy images not counted) |
  | Features, non-image | at most 80 KB |
  | Features, images | *(Revision 3)* at most 700 KB worst case (6 screenshots); the same rule of one eager image (≤ 120 KB), the rest lazy |
  | One screenshot file | *(Revision 3)* desktop: 1x ≤ 50 KB, 2x ≤ 120 KB; phone: 1x ≤ 25 KB, 2x ≤ 60 KB |
  | Screenshot files, total in `dist` | *(Revision 3)* at most 2 MB raw (24 files) |
  | Any other page | at most 150 KB total; no screenshots |
  | HTML per page | at most 60 KB, **except the named exceptions below** |
  | `dist` total | at most 5 MB and 500 files, in raw bytes on disk (not gzip) |

  Step 6 confirms the current Cloudflare Pages Direct Upload limits (file count and per-file size); our ceiling is far below them.

  **Image metric (Revision 3).** Images are counted in **raw bytes**, because WebP is already compressed and gzip saves nothing. For each `<picture>`, "worst case" means the largest single candidate the browser could fetch: the 2x file of whichever theme is heavier. Browsers fetch one source per `<picture>`, never both themes. Home shows two screenshots: the `deploy-live` hero and the `phone-deploy` view. Its worst-case first view is about 200 KB (80 KB non-image plus one 120 KB eager image). Its full scroll is about 180 KB of images (120 + 60), under a ceiling of 190 KB for images and 270 KB in total. That is honestly more than the 80 KB of revision 2, and the footprint reviewer must sign it off.

  **Gate 11 (`page-weight.mjs`, Revision 3 review fix):**
  - **Worst case per `<picture>`.** For each `<picture>`, the worst case is the 2x file of the heavier theme. The script does **not** add up every `srcset` and `<source>` candidate.
  - **First view.** Lazy images are left out of first-view bytes and requests. A second eager image on a page fails the gate.
  - **Caps.** The per-file caps and the total screenshot cap above apply.
  - **Fixtures.** One failing fixture each: an over-heavy candidate, a second eager image, too many first-view requests, a file over its cap, and the total over its cap.
  - **Phones.** Step 3b decides whether phones get the `phone-deploy` hero through art direction (`<source media="(max-width: 37.5rem)">`, worst case 60 KB) instead of `sizes` picking the 1600 w desktop file on a 3x phone (worst case 120 KB). It records the chosen worst case here.
  - **Check at 320 px.** Playwright checks that only the eager image loads on the first view at 320 px, because lazy images inside the first viewport still load.
- **Named HTML exceptions.** Listed in `site/budgets.json` with a page, a ceiling and a reason. A new exception needs footprint sign-off.
  - **`/docs/security/`: HTML ceiling 100 KB**, still inside the 150 KB page total. The source is already about 55.5 KB gzip. The threat model is one document, and splitting it would break anchors that other docs link to. Step 3 records the measured value and sets the ceiling to the measured value plus 15%, capped at 100 KB. If the measurement is above about 87 KB (where 15% headroom would pass 100 KB), step 3 has two options: split the doc, keeping the old anchors as links to the new pages, or raise the cap with a written reason and footprint sign-off.
  - `/docs/architecture/` and `/roadmap/` are measured in step 3. They get an exception only if they exceed 60 KB, under the same rule.
- **Recorded in step 1.** `npm ls --all | wc -l` and the lockfile size for `site/` and `site/deploy/`, and whether `sharp` was installed.

## 5. Security notes

- **What the token can do** (security B3). An account API token with *Account → Cloudflare Pages → Edit* can, for **every** Pages project in that account:
  - deploy any content, **including Functions and `_worker.js`**;
  - edit project settings, including environment variables and secrets, and probably KV, D1 and R2 bindings (step 6 verifies this);
  - create and delete projects.

  The token cannot be limited to one project. That is why:
  - the choice of account depends on an inventory (Q3);
  - the token reaches only the wrangler step of deploy runs on `main`;
  - the deploy job runs no repo code except two things: `site/scripts/gates.mjs` (only `node:` built-ins, run before the token step, in CODEOWNERS) and the lockfile-pinned wrangler from `site/deploy/`;
  - the deploy job has `permissions: {}`;
  - the deploy job checks the artifact again for Functions before deploying.

  Step 6 confirms that Pages:Edit alone is enough for `wrangler pages deploy`.
- **Required whichever account is used** (Q3):
  - B1 is done;
  - **the token expires after 90 days**, with the renewal date in the maintainer's calendar;
  - the environment `production` uses "Selected branches" = `main` only, with no tag rules;
  - **written assumption:** the `main` ruleset requires a PR with code-owner review for `/.github/` and forbids force-push. The `site.yml` deploy job is only as safe as this rule; step 6 verifies and records it;
  - *optional:* a required reviewer on `production`.
- **Exposure to untrusted code.**
  - The secret is never available to `pull_request` runs.
  - There is no `pull_request_target` and there are no previews (Q4).
  - The artifact comes only from the same run.
- **Supply chain.** Exact versions plus lockfiles, `npm ci --ignore-scripts`, `npm audit` in CI, actions pinned by SHA, and Dependabot for `/site` and `/site/deploy`.
- **Site surface.**
  - Static files only: no forms, no cookies, no JS; `script-src 'none'`; the build gates in section 3.5.
  - The link rewriter rejects paths outside the repo (using `realpath`), so a symlink cannot pull in other files.
  - The last-changed date reads only `%cs`, never names or emails.
- **Cloudflare features that inject code.** These must be off for this hostname, using a Configuration Rule scoped to the hostname: Rocket Loader, Email Address Obfuscation, Zaraz, RUM/Web Analytics, and Cloudflare Apps where present.
  - **Bot Fight Mode** is zone-wide on the free plan and injects challenge and detection scripts. Recommendation: keep it **off** for the zone. This affects the maintainer's other sites, so it is the maintainer's choice (Q5). If it stays on, the step 6 "zero CSP violations" check would falsely fail on `/cdn-cgi/` scripts; that is then recorded as a known exception, not hidden.
- **Privacy.** The site collects nothing. Cloudflare's edge logs are outside our control, and the footer names the host.

## 6. Steps

Each step is one PR into `main` and can be tested on its own.

1. **Scaffold, CI build job, all build gates, and the bench/integration exclusion.**
   - **Work:**
     - `site/` with Astro: the base layout, header and footer with both navs, the skip link, `404`, `_headers`, `robots.txt`, the favicon, `security.txt`, telemetry off, and the passthrough image service;
     - `site/scripts/gates.mjs`, `page-weight.mjs` and `serve.mjs`;
     - `site.yml` with the `build` job only;
     - the `bench-needed.sh` change, its comments, the `ci.yml` comments and the `architecture.md` line;
     - the dependabot, CODEOWNERS, `.gitignore` and `.dockerignore` entries;
     - recording the dependency counts.
   - **Tests:**
     - a unit test for every gate, each with a failing fixture, including the page-weight budgets and the `security.txt` expiry;
     - the build passes;
     - new `bench-needed_test.sh` cases:

       | Change | Result |
       |---|---|
       | site only | skip |
       | `site/x.go` | bench |
       | site + `web/` | bench |
       | site + `.github/workflows/site.yml` | bench |
       | site + `.github/workflows/ci.yml` | bench |
       | `sitex/a` | bench |
       | `web/site/a` | bench |
       | rename `internal/x` → `site/x` | bench |
       | site-only `synchronize` increment after a passing gate | reuse |
       | symlink under `site/` | site only |

     - live checks:
       - a site-only PR skips `bench` and `integration`;
       - a docs-only PR runs the site build;
       - a Go change runs `ci.yml`'s bench and integration, and also the site build if the paths match.
2. **Tokens, components and the axe harness.**
   - **Work:**
     - `tokens.css` for both themes, and the components;
     - `/styleguide/` (test builds only);
     - the contrast and og.html token tests;
     - Playwright + `@axe-core/playwright` against `serve.mjs`, in the section 7 projects;
     - the bounding-box, no-other-host, CSP-console, heading-order, 400% zoom and text-spacing checks;
     - recording the measured job time.
   - **Tests:** zero axe violations on the styleguide and the 404 in every project; the gates still pass.
3. **Docs pipeline and Roadmap.** *Needs P2.*
   - **Work:**
     - the allowlist and the loader, after confirming the out-of-root base and `fs.allow`;
     - title and description extraction;
     - `rewrite-links.mjs`, table wrapping and Prism;
     - the "On this page" nav, "Edit on GitHub" and the last-changed date;
     - the docs index and sidebar, `/security/` and `/contributing/`;
     - `status.json`, and `/roadmap/` with the SVG;
     - the measured HTML budget exceptions.
   - **Tests:**
     - unit tests for each rewrite rule: a published doc, an anchor, an unpublished doc going to GitHub, a folder going to `/tree/`, a missing file, a path outside the repo, a symlink;
     - the allowlist equals the routes;
     - GitHub-slug anchors work on the real docs;
     - axe on every doc page.
3b. **Real screenshots** *(Revision 3)*. Needs step 2 only, and can run alongside step 3. It must land before step 4. See Revision 3.
4. **Home and Features.** *Needs P2 and step 3b.* Use the screenshots from step 3b through the `Screen` component (Revision 3).
   - **Work:**
     - the hero and h1;
     - the deploy card;
     - the meter as an SVG rect with a width attribute;
     - the status callout;
     - the details cards with evidence links;
     - the AI-led section;
     - works today and what's next;
     - Features.
   - **Tests:**
     - axe in all projects;
     - the h1 fits in two lines at 320 px;
     - the Home budget;
     - no `style=` anywhere;
     - reviewer checklist: every "Works today" item is a closed milestone in `roadmap.md`, and the card's stage labels are true for M3a.
5. **Benchmarks.** *Needs P2.*
   - **Work:**
     - `benchmarks.json` with Table A and Table B (section 3.2);
     - the source check (label and value on the same line, plus the anchor);
     - the table regions, sticky first column and scroll cue;
     - "Not measured yet".
   - **Tests:**
     - unit tests of the source check: a missing file, the value on a different line from the label, a missing anchor, and a value that matches only inside a longer number (`54.0` against `154.0`);
     - the stale sentence in research 05 l.182 ("They do not replace the PENDING cells above") is fixed in this step, unless PR #7 already fixed it;
     - axe;
     - a phone-width check that cell labels stay visible.
6. **Deploy, domain, launch check and ADR.** Starts after the maintainer's manual setup (Q3, Q5).
   - **Work:**
     - `site/deploy/` and the `deploy` job;
     - confirm the Cloudflare facts this plan marks "step 6":
       - Pages:Edit is enough for the deploy;
       - `--ignore-scripts` works for `pages deploy`;
       - which bindings the token can edit;
       - the Direct Upload limits;
       - the `_headers` host patterns;
       - a cross-account domain test, if a dedicated account is used;
     - merge, then check production on `serverdock.pages.dev` and on the custom domain;
     - the manual launch checklist file (section 7);
     - the site link in the README and in the repo's "Website" field;
     - an ADR at the next free number, "Public website: Astro static site on Cloudflare Pages, deployed from Actions";
     - update the AI-written Obsidian note (outside the repo).
   - **Tests:**
     - `curl -sI` shows every header on both hostnames, and `noindex` only on `*.pages.dev`;
     - canonical URLs use the custom domain;
     - zero CSP violations in the console on every page type on the custom domain;
     - **ordering:** two quick pushes to `main` that touch `site/` both build. Their deploys run one after the other in `site-deploy-main`, and at the end the newer commit is live (its deployment commit hash matches);
     - a site push followed by a non-site push to `main` still deploys the site change;
     - a unit test shows that `gates.mjs --dist-only` fails on a `dist` with `_worker.js`, `functions/` or `_routes.json` added, or with `_headers`/`security.txt` missing. In CI the gates step comes before the wrangler step, so such an artifact never reaches the token.

## 7. Test plan

- **Unit tests (`node:test`):**
  - every gate in `gates.mjs` and `page-weight.mjs`, with failing fixtures;
  - link rewriting: each rule, outside-repo paths, symlinks, the error path;
  - title and description extraction;
  - the benchmark source check;
  - contrast ratios from `tokens.css`, including Prism;
  - the og.html token match;
  - `bench-needed_test.sh`, with the step 1 cases.
- **Build gates:** section 3.5, items 1–9, plus `astro check`.
- **Accessibility in CI:** Playwright + axe under the real CSP, served by `serve.mjs`.
  - **Pages:** every page in the sitemap, plus the 404 page and (in the test build) `/styleguide/`.
  - **axe settings:**
    - tags `wcag2a`, `wcag2aa`, `wcag21a`, `wcag21aa`, `wcag22aa` and `best-practice`;
    - **zero violations**;
    - a color-contrast result in "incomplete" fails unless it is listed with a reason for the manual pass;
    - a rule may be disabled only with accessibility sign-off.
  - **Projects:**
    - desktop Chromium;
    - phone (Pixel size);
    - 320 px;
    - 200% zoom;
    - 400% zoom (a 1280 px window scaled ×4, which equals 320 CSS px);
    - dark and light;
    - reduced motion;
    - forced colours;
    - `prefers-contrast: more`;
    - WebKit.
  - **Checks on every page:**
    - the skip link is the first tab stop and moves focus to `main`;
    - focus is visible on every link;
    - the `<details>` navs open with Enter and Space;
    - no horizontal scroll at 320 px or at 400%, except inside table regions and code blocks;
    - **heading order**: one h1, no skipped levels;
    - **bounding boxes** on the phone and 320 px projects: every target listed in 3.4 Targets is at least 44×44 px, and in-text links are at least 24 px;
    - **WCAG 1.4.12 text spacing**: the override is injected with `bypassCSP: true` for that test only, and nothing may clip or overlap;
    - **no request to any other host**;
    - **no CSP violation** in the console;
    - unique accessible names for Evidence, Edit and Source links.
  - **Job time.** Ceiling 10 minutes. Step 2 records the measured value here, and the footprint reviewer acts on it at step 2. If it is over, **WebKit only** moves to push-on-`main`, where it **blocks the deploy** (`deploy` needs it) rather than only warning. Forced colours, 320 px, zoom and reduced motion always run on PRs. (This disagrees with a footprint NIT; see Revision 1.)
- **Not used:** Lighthouse CI. One manual Lighthouse run at launch is recorded for information only.
- **Manual launch checklist** (step 6). A file, `docs/accessibility/site-launch-checklist.md`, with columns for tool, browser, date, result and sign-off. It covers:
  - screen readers:
    - VoiceOver with Safari on macOS and iOS;
    - NVDA with Firefox, or Narrator with Edge;
    - TalkBack on an Android phone;
  - a keyboard-only walk through every page type;
  - zoom and spacing:
    - 200% browser text-only zoom;
    - 400% zoom;
    - a text-spacing bookmarklet;
  - forced colours: Windows high contrast and Firefox forced colours, with screenshots (axe cannot see the visuals);
  - real OS settings:
    - `prefers-contrast: more`;
    - reduced motion turned on in the OS;
  - print preview;
  - "Fast 3G": Home is readable in under 2 s;
  - zero CSP violations on the custom domain.

  A 1 GB VPS check is **not relevant**: nothing runs on the user's server.

## 8. Reviews required

| Reviewer | Required for |
|---|---|
| reviewer | The plan and every step. Checks that the copy matches the README and that no claim goes beyond what the repo shows. |
| accessibility | **Required.** Steps 2–5, step 3b (alt text, captions, `<picture>`), and the launch checklist in step 6. |
| security | Step 1: CSP, `_headers`, gates, `bench-needed.sh`, CODEOWNERS. Step 3b: demo data, excluded screens, the `screens` CI job. Step 6: deployer, token, environment, account choice, Cloudflare settings. |
| footprint | Step 1: the page-weight script, gates and dependency counts. Step 3b: image budgets, encoder, repo growth (**sign-off required**). Steps 3 and 4: budget exceptions and Home. Also confirms the product footprint is unchanged. |
| systems | Not required. The reviewer may ask for it on the `bench-needed.sh` change. |

## 9. Dependencies

Exact versions are pinned in the lockfiles in step 1.

| Package | Where | Why |
|---|---|---|
| `astro` | `site/` (build) | The generator the maintainer chose. Its built-in collections, `glob()` loader, remark/rehype and Prism mean no Markdown plugins are needed. |
| `@astrojs/check` + `typescript` | `site/` (dev) | `astro check` in CI. TypeScript at the same version as `web/`. |
| `@playwright/test` | `site/` (dev) | Chromium and WebKit tests. Same version as `web/`. |
| `@axe-core/playwright` | `site/` (dev) | The axe gate. Same version as `web/`. |
| `wrangler` | **`site/deploy/` only** | Cloudflare's CLI for `pages deploy`. Its own lockfile means the build never installs it and the deploy installs nothing else. |

**Not added:**
- Tailwind or any CSS framework;
- `@astrojs/sitemap`;
- MDX;
- image tooling (passthrough service; `sharp`, if Astro pulls it in as optional, never runs);
- link-check packages;
- a static-server package (`serve.mjs` is local);
- Lighthouse CI;
- font packages.

## 10. Maintainer answers (2026-11-28)

The maintainer accepted every recommendation.

- **Q1:** publish the guides (vision, features, UI), architecture, security, roadmap and the ADRs. Plans and research appear only as GitHub links.
- **Q2:** other panels appear only with idle RAM and official minimums, cited and dated, and no CVE claims.
- **Q3:** the maintainer does the Cloudflare setup by hand. Steps, including the security revisions:
  1. **Take an inventory first.** List the Pages projects in the account that holds the zone. If **any** of them serves a live site or has Functions with bindings, **use a dedicated free Cloudflare account** for ServerDock. In that case, first test the cross-account custom domain (a DNS-only CNAME in the zone, then Pages domain validation). Otherwise the same account is fine.
  2. Create the Pages project `serverdock` as **Direct Upload**. It cannot be switched to Git integration later. Set the production branch to `main` and leave Web Analytics off.
  3. Create an account API token with only *Account → Cloudflare Pages → Edit*, for that one account, with a **90-day expiry**.
  4. In GitHub, create the environment **`production`**: "Selected branches" = `main`, no tag rules, optionally a required reviewer. Add the secret `CLOUDFLARE_API_TOKEN` and the variable `CLOUDFLARE_ACCOUNT_ID`.
  5. Confirm that the `main` ruleset requires a PR with code-owner review for `/.github/` and forbids force-push. Confirm that private vulnerability reporting is on.
- **Q4:** no PR previews in v1.
- **Q5 (still open; recommendation):**
  - add the custom domain through Pages, so Cloudflare creates the CNAME (or a DNS-only CNAME if a dedicated account is used);
  - add a Configuration Rule for the hostname that turns off Rocket Loader, Email Obfuscation, Zaraz, RUM and Apps;
  - **keep Bot Fight Mode off** for the zone, or accept a recorded exception in the CSP check.
- **Q6:** resolved by PR #7 (P2).
- **Q7:** the light theme follows the system setting, with no toggle.
- **Q8:** publish `security.txt`.

## Revision 1 (review fixes)

All four reviews returned REVISE on revision 0. Each item and where it landed:

**Reviewer**

| # | Finding | Where |
|---|---|---|
| 1 | P2 is PR #7: CI figures and the roadmap reorder | Preconditions; steps 3–5 "Needs P2"; Home and Roadmap read the order from `roadmap.md`/`status.json` |
| 2 | Merge to about 6 steps; the exclusion also skips `integration`; step 1 tests | Section 6 (6 steps); 3.5 bench/integration gate; step 1 tests |
| 3 | CSP vs meter; style gate; tests under the CSP; drop `manifest-src` | 3.2 meter (SVG `rect` width attribute); gate 1; `serve.mjs`; CSP updated |
| 4 | Source check by line and anchor; `source_path` vs `primary_url`; Table B rows | 3.2 Benchmarks (table and checks); "0.7–1.2 GB" shown as research 01's conclusion |
| 5/6 | Wrangler isolation; deploy ordering | 3.5 deploy job (`site/deploy/`; ordering reworked in Revision 2) |
| NIT | "We sweat" vs "Few sweat"; 5.3%; dependabot; not a required check; `site.yml` stays bench-relevant; `og.png` copied at build; styleguide not in production; loader and `fs.allow`; Pages:Edit | 3.2 hero and meter; 3.5 housekeeping and workflow; 3.4 meta and styleguide; 3.3 loading; section 5 and step 6 |

**Security**

| # | Finding | Where |
|---|---|---|
| B1 | Separate deployer lockfile, sparse checkout, step-level env, `permissions: {}`, metrics off, dependabot | 3.5 deploy job; section 9; housekeeping |
| B2 | Hidden files in the artifact; asserts before deploy; same gates in build; same-run download | 3.5 gates 2–4 and deploy steps |
| B3 | Full reach of the token; account choice by inventory; required controls | Section 5; Q3 |
| B4 | Exact `':(exclude)site/'`, comments, test cases | 3.5 bench gate; step 1 test table |
| NITs | `:version` noindex; RUM/Apps; Bot Fight Mode; glob = allowlist + route gate; `realpath`; `%cs`; `security.txt` fields and expiry gate; private reporting; `--commit-hash`; `workflow_dispatch` (redeploys from `main` only); CODEOWNERS; `style-src` vs inline SVG | `_headers`; section 5; 3.3; 3.4; 3.5; Q3 |

**Accessibility**

| # | Finding | Where |
|---|---|---|
| 1 | Light values for every token; ratio unit test | 3.4 token table and contrast test |
| 2 | Solid surfaces; axe "incomplete" results | 3.4 surfaces; section 7 |
| 3 | `--border` at least 3:1, `--line` decorative; meter | 3.4 tokens; 3.2 meter |
| 4 | Deploy card as `<ol>` in `<figure>` with state words | 3.2 Home |
| 5 | No-JS nav pattern; named navs; summary | 3.1 |
| 6 | `main` `tabindex=-1`; skip link; sidebar order; no sticky header; focus ring | 3.4 focus and skip link |
| 7 | Table regions; narrow layout; scroll cue | 3.2 tables; 3.3 rehype |
| 8 | Scope of reduced motion; forced colours; `prefers-contrast` | 3.4 motion, forced colours, theme |
| 9 | 400% zoom; text spacing; `rem`; wrapping | Section 7; 3.4 |
| 10 | 44 px targets with a bounding-box check | 3.4 targets; section 7 |
| 11 | axe tags; heading order; unique link names; one h1; "On this page"; GitHub cue | Section 7; 3.2; 3.3 |
| 12 | Pill name; `<time>`; "not measured yet" | 3.1; 3.2 |
| 13 | Manual checklist file with more assistive technologies | Section 7 |
| NITs | SVG alt and forced colours; Prism in the contrast table; 404; `color-scheme`; favicon; unclipped ring; text-only zoom; hook in two lines; styleguide contents; `lang` | 3.2 Roadmap; 3.4; 3.1; section 7; step 4 |

**Footprint**

| # | Finding | Where |
|---|---|---|
| 1 | One metric; named script; request counting; fixtures | Section 4 |
| 2 | `/docs/security/` over 60 KB | Section 4: named exception (100 KB, within the 150 KB total); budgets stated as ceilings |
| 3 | Wrangler out of `site/package.json` | `site/deploy/` |
| NITs | PR cancel; Playwright cache; job ceiling; "saves bench and integration minutes"; `ci.yml`/`architecture.md` comments; `.dockerignore`/`.gitignore`; dependency counts; `sharp` with `--ignore-scripts`; no-other-host check; `dist` total and the Cloudflare limit | 3.5; section 4; section 7; step 1 |

**Where this revision disagrees** (left for the reviewers to decide):
- **Forced colours and WebKit only on `main` (footprint NIT).** I disagree for forced colours. It is an accessibility gate that the product runs on every PR, and moving it to `main` would let regressions merge. Instead, keep every project on PRs under the 10-minute ceiling. If the measured time goes over it, move **WebKit only** to push-on-`main`.
- **Excluding `site.yml` from the bench (reviewer NIT).** Not proposed. Benching a workflow change costs little, and excluding a workflow file from a gate needs its own security review. That can be a separate PR later.

## Revision 2 (deploy job; reviewer and security)

Accessibility and footprint approved revision 1. The reviewer and security asked for changes to the deploy job only.

**Blocking**
1. **Concurrency.** The head-of-main check and "superseded, not deployed" are gone: with the path filter, that check could leave a site change undeployed forever.
   - The workflow group is now `site-${{ github.event.pull_request.number || github.ref }}`, with `cancel-in-progress: ${{ github.event_name == 'pull_request' }}`.
   - The deploy job has its own group, `site-deploy-main`, with `cancel-in-progress: false`.
   - Push and dispatch runs queue in order.
   - The step 6 tests are now: ordering of two site pushes, and a site push followed by a non-site push.
2. **Gates in the deploy job.**
   - The sparse checkout adds `site/scripts/gates.mjs`, which uses only `node:` built-ins and runs before the token step. It is added to CODEOWNERS.
   - The artifact goes to `$RUNNER_TEMP/dist`, and wrangler runs with `working-directory: site/deploy` against that path.
   - Section 5 now names `gates.mjs` and wrangler as the only repo code that runs in the deploy job.
   - Step 6 confirms that checkout works with `permissions: {}`; otherwise the job gets `contents: read`.
3. **Fallback.** A plain `npm ci` of the same lockfile, with no token during the install. `wrangler-action` may only run the wrangler already installed, and needs security sign-off at step 6.

**NITs**
- **Benchmarks:** `match_value` matches on token boundaries, with a unit test. The stale research 05 l.182 sentence is fixed in step 5, unless PR #7 already fixed it.
- **`security.txt`:** `Expires` is at most 11 months ahead (RFC 9116), renewed with every second token rotation.
- **Footprint:**
  - the footprint reviewer acts on the 10-minute ceiling at step 2, and the measured value is recorded;
  - the `dist` total is in raw bytes on disk;
  - if `/docs/security/` measures above about 87 KB, step 3 splits the doc or raises the cap with a written reason.
- **Accessibility:**
  - the meter fill is `--accent-text` in light, and the track has a `--border` outline that also shows in forced colours;
  - the 44 px check covers footer, Evidence/Edit/Source and "On this page" links (in-text links follow the 24 px rule), and runs at 320 px too;
  - the scroll cue always shows below `37.5rem`, and the sticky column takes at most 40% at 320 px;
  - the summary name stays "Menu", with a CSS-only "+"/"−" marker;
  - the GitHub link's hidden text is " (on GitHub)";
  - the pill's name starts with its visible text, using appended hidden text instead of `aria-label`;
  - the Prism tokens are listed explicitly in `tokens.css` before step 2 closes.
- **WebKit:** if it moves to `main`, the deploy needs it, so a WebKit failure blocks the deploy.

## Step 1 record (implementer, 2026-11-28)

Measured and decided while building step 1. AI-written; the maintainer has not confirmed it.

- **Dependency counts.** `npm ls --all | wc -l` is 558 lines for `site/` and 160 for `site/deploy/`. Lockfiles: 185,581 bytes (`site/`) and 69,060 bytes (`site/deploy/`).
- **`sharp`.** It is installed, because Astro lists it as a dependency. `npm ci --ignore-scripts` skips its install script, and it never runs (passthrough image service). The build works under `--ignore-scripts`.
- **Units.** 1 KB is 1000 bytes in `page-weight.mjs`, like the `web/` size check.
- **`site/deploy/`** is kept in step 1 (the plan said step 6 for the job itself) so its Dependabot entry resolves.
- **Footer.** The footer text ("AGPL-3.0. No tracking…", "Built with AI agents…") sits outside `<nav aria-label="Footer">`, which holds only the links. This is better than the plan's wording.
- **Deviations the reviewers accepted:**
  - placeholder pages for Features, Benchmarks, Roadmap, Docs, Security and Contributing, so gate 7 can pass; steps 3 to 5 replace them;
  - gate 6 uses an empty allowlist until step 3, and fails closed if `src/docs.config.ts` exists;
  - gate 5 fails on any `styleguide/` directory;
  - `src/node-shim.d.ts` declares the few Node APIs `og.png.ts` uses, so `@types/node` is not a dependency.
- **`telemetry: false`** was removed from `astro.config.mjs`: it is not a valid Astro config key. `ASTRO_TELEMETRY_DISABLED=1` in the npm scripts and in `site.yml` turns telemetry off.
- **`_headers`** also sets `Cross-Origin-Resource-Policy: same-origin` and `X-Frame-Options: DENY`, added at security's request in round 1, so the plan's section 3.5 and the file agree.
- **Review round 1 changes.** Gate 1 also catches attributes after `/` or a quote, a stray quote hiding a `<script>`, `javascript:` URLs and meta refresh. Gate 2 parses `_headers` and requires one `/*` CSP with `script-src` and `default-src` exactly `'none'`. Gate 3 also checks `Contact`, a single RFC 3339 `Expires`, and an 11-month maximum. Gate 6 covers nested and flat HTML. Budget exceptions are validated and a bad `budgets.json` exits 2.

## Step 2 record (implementer, 2026-11-28)

AI-written; the maintainer has not confirmed it.

- **Built:** `tokens.css` (both themes, explicit `--prism-*`), `global.css`, `components.css`, the components (chip, card, callout, evidence link, deploy stages, meter, table region, docs nav, "On this page"), the docs layout slot (sidebar before `main`), `/styleguide/` (built only when `SITE_STYLEGUIDE=1`), the placeholder `base.css` removed, and `docs/accessibility/site-launch-checklist.md`.
- **Dependencies:** `@playwright/test` 1.65.1 and `@axe-core/playwright` 4.14.0, the exact versions `web/` pins on `main`.
- **Production CSS:** one file, 11 KB raw and 3.2 KB gzip (ceiling 15 KB). Every page is 4.5 KB gzip over 3 requests.
- **Tests:** `npm test` has 45 unit tests, including the contrast ratios from `tokens.css` (text at least 4.5:1, borders, focus ring and meter at least 3:1, both themes, all Prism tokens), the og.html token match, and a build-twice test of the styleguide switch. `npm run e2e` runs 736 tests (254 skipped by design: each check runs only in the projects it applies to) in 11 projects, against the test build served by `scripts/serve.mjs`.
- **Measured time, local (Apple silicon, browsers already installed):** `astro check`, unit tests, both builds, gates, page weight and Playwright in about 40 s in total (Playwright about 35 s). **CI, measured:** the whole `build` job took **4 min 13 s** (run 46160694236): a cold browser install of 35 s and Playwright 2 min 29 s. That is under the 10-minute ceiling, so nothing moves to `main`. The cost: a docs-only PR now pays about 4 minutes of `site.yml` time.
- **Deviations and findings:**
  - WebKit upgrades `http://127.0.0.1` to https because of `upgrade-insecure-requests` (Chromium exempts loopback). `serve.mjs` drops that one directive when `SERVE_NO_UPGRADE=1`, which only the Playwright config sets. Every other directive is as shipped.
  - Playwright's WebKit does not Tab to links, so the skip-link and focus-ring tests skip WebKit; the other checks and axe run there. Real Safari keyboard behaviour is on the manual checklist.
  - axe reports the "✓" glyph in the stage dots as a colour-contrast "incomplete" (`nonBmp`). The tick is now generated content on an empty `aria-hidden` span, and its colours are covered by the contrast unit test.
  - `<aside>` for callouts fails axe `landmark-unique` when there are two; callouts are plain `div`s with a text label.
  - The e2e files are not type-checked: `@types/node` is not a dependency, and Playwright transpiles TypeScript without checking it.

## Revision 3 (real dashboard screenshots, maintainer requirement 2026-11-28)

**Requirement.** The site shows the real dashboard, using real screenshots that are made automatically. Playwright opens the real ServerDock web UI with demo data and captures the views in dark and light. The site never shows an HTML copy of the dashboard. Each image is labelled "demo data" and compressed. AI-written; the maintainer has not confirmed this beyond the requirement itself.

### Honesty rules
- **Only screens that exist today.** Every capture comes from a route under `web/src/routes/` on `main` (M1–M3a). No mock-ups, no planned screens, and no feature that is not built.
- **Mocks change data, never the UI, and this is enforced** (review fix).
  - Today the `seed.ts` fixtures take `Record<string, unknown>`, and `web/tsconfig.json` covers `src` only.
  - Step 3b types every fixture the capture uses with `satisfies components['schemas'][…]` from `web/src/lib/api-types.ts`, which is generated from `docs/api/openapi.json`.
  - Step 3b also adds `web/tsconfig.e2e.json` (covering `e2e/screens.capture.ts` and `e2e/seed.ts`), with `tsc --noEmit -p tsconfig.e2e.json` as a step in the `web` job.
  - Node types come from a small local shim, like `site/src/node-shim.d.ts`. If the shim gets too large, `@types/node` is added as a pinned dev dependency with footprint sign-off.
  - The result: a mock cannot invent a field or a state the API does not have. Every state shown is one the product really produces.
- **Caption format** (one order, gated): "<What the screen shows>. Captured <time>. Demo data: the states are staged with seeded rows and mocked API responses."
  - The last sentence is exact, and gate 10 checks it.
  - There is **no commit** in the caption or the manifest (review fix): PR runs check out `refs/pull/N/merge`, so a commit SHA would be misleading. The capture date and `inputs_hash` identify the set.
- **Badge.** A "Demo data" badge sits in the figure's frame header, outside the image crop:
  - a solid `--card`/`--bg2` background, a real `--border`, and text at least 4.5:1;
  - it keeps its border in forced colours;
  - it is `aria-hidden="true"`, because the caption carries the words.
- **Route check.** Each manifest entry names its SvelteKit route id (for example `(shell)/services/[project]/[name]/deploys/[number]`). Gate 10 fails if `web/src/routes/<route>/+page.svelte` does not exist. This reads a repo file only and needs no web build.

### Views (v1)

Stage names are the on-screen labels from `STAGE_LABELS` in `web/src/lib/services.ts`. Alt text must use the labels as they appear on screen.

| id | Route | Demo state | Viewports | Used on | Page text that carries the claim |
|---|---|---|---|---|---|
| `deploy-live` | `(shell)/services/[project]/[name]/deploys/[number]` | blue-green deploy #7 of `api`: "Pull the image" and "Start the new version" done, "Health check" running, live log below | desktop | Home hero, Features | Home: a visible ordered list next to the hero (after the h1 and sub line) of the blue-green stages, in the order the product runs them. The list is taken from `STAGE_LABELS`/`stageRows('blue_green', …)` at step 4, not typed by hand. Plus one sentence: traffic switches only after the health check passes. Features: the "Blue-green deploys" sentence |
| `services` | `(shell)/services` | three services: running, deploying, needs attention | desktop | Features | Features: "Every service shows its true state, including unknown" |
| `service-detail` | `(shell)/services/[project]/[name]/(service)` | live deploy and history, with a rolled-back deploy | desktop | Features | Features: "Deploy history with one-click rollback" |
| `rollback-dialog` | same route, with the existing `ConfirmDialog` open | dialog with the confirm button "Roll back to #5" | desktop | Features | Features: "Rollback asks for confirmation and names the target deploy" |
| `new-service` | `(shell)/services/new` | form filled with a demo image | desktop | Features | Features: "Deploy from a container image" |
| `phone-deploy` | the deploy route | same as `deploy-live` | phone 390×844 | Home "Usable" card, Features | Home: the "Usable" card text ("works on a phone, at 200% zoom…") |

Reviewer checklist item: every figure has the sentence in this column on the same page, and no claim is carried by the image alone.

**Never captured:** `(auth)/setup` (setup token, TOTP secret and QR, recovery codes), `(shell)/settings/security` (API tokens are shown once, TOTP), `(auth)/login/2fa`, and any dialog that shows a token, a secret or recovery codes. That gives 6 views × 2 themes × 2 densities = **24 files**.

### Pipeline
- **The capture spec:** `web/e2e/screens.capture.ts`, run only through its own config, `web/playwright.screens.config.ts`. The main `playwright.config.ts` ignores it in every set, including `E2E_SET` unset/`all`, so `npm run e2e` never runs it. Only the six route ids in an allowlist constant at the top of the spec may be captured. It reuses the existing harness:
  - `server.ts` and `global-setup.ts`: the real binary with `-tags spa`, a temp data directory, signed in;
  - `seed.ts` rows written through `node:sqlite` (`seedService`, `seedHistory`, `seedThreeServices`, `stageRows`, `seedDeployLog`);
  - the API mocks (`mockServiceDetail`, `mockDeployGet` with `deployGetFixture`, `mockAction`) for states that would otherwise need containers.
  - **No Docker and no local registry are needed.** The "running" and "deploying" states come from typed mocks, so the spec runs wherever the UI e2e set runs.
- **Making captures deterministic:**
  - a fixed clock (`page.clock`) and fixed seeded `createdAt` values;
  - `animations: 'disabled'`, `caret: 'hide'`, and `reducedMotion: 'reduce'`;
  - Chromium only;
  - desktop: a viewport of about 1024 px wide at `deviceScaleFactor: 2`, clipped to the main content (about 800 CSS px), so text is shown near 1:1 and stays legible;
  - phone: 390×844 at `deviceScaleFactor: 3`;
  - `colorScheme` set to `dark` and `light` in turn;
  - **clipping is required.** Each capture is clipped to the main content, or to the dialog with its page behind it. The shell's status footer (which shows the capture server's real RAM, CPU and version) stays **outside** the clip. No browser chrome or URL bar appears.
- **Encoding: no new dependency.** The spec loads each PNG into a blank page and draws it on a `<canvas>` at the target widths. `canvas.toBlob('image/webp', q)` exports it, using the libwebp encoder inside Chromium, which is already in use. The starting quality is 0.8, tuned in step 3b to meet the per-file ceilings.
  - Re-encoding through a canvas also drops all metadata.
  - `sharp` stays unused. It is installed in `site/` only, it is not in `web/`, and with `--ignore-scripts` it is not a dependency we want to activate.
  - Fallback, only if canvas output turns out non-deterministic or too large: the runner's apt `webp` package (`cwebp`, a system tool, not an npm dependency). That needs footprint sign-off.
  - Format: WebP only, with no PNG fallback, because every supported browser decodes WebP.
- **Sizes:**
  - desktop display width at most 800 CSS px: files at **800 w (1x) and 1600 w (2x)**;
  - phone display width 300 CSS px: files at **300 w and 600 w**.
- **Output: committed** to `site/public/screens/` as `<id>-<theme>-<width>.webp`, plus `manifest.json`. They go in `public/`, not `src/assets/`, because the site uses no `astro:assets`, so the files are served as they are.
- **The manifest** has a strict schema, and gate 10 fails on unknown keys.
  - Top level:
    - `inputs_hash` (40 hex);
    - `captured_at` (RFC 3339);
    - `run_id` (digits);
    - `runner_image` and `chromium` (version strings);
    - `growth`: a list of `{captured_at, bytes_added}` entries, one per regeneration.
  - Each entry:
    - `id`, matching `^[a-z0-9-]+$`;
    - `route` (no `..`, and it must be in the allowlist);
    - `viewport` and `theme`;
    - `files`: names matching `^<id>-(dark|light)-\d+\.webp$`, with `width`/`height`/`bytes`.
  - There is no commit field.
  - The site build reads the committed files and the manifest only. **It never builds Go or the web UI.**
- **Only the CI artifact may be committed.** Local captures differ in fonts and rendering (for example on macOS). `make screens` is for previewing only.
  - The rule is in CONTRIBUTING.
  - The slow-path byte comparison (below) shows any locally made file as "differs".
- **Make target:** `make screens`. It builds the web UI and the binary, then runs the screens config into a scratch folder, not into `site/public/screens/`.
- **Freshness: the `screens` job in `ci.yml`.**
  - **The hash.** `inputs_hash` is the git tree hash of these inputs (decision per input, review fix):

    | Input | In the hash? | Why |
    |---|---|---|
    | `web/src/**` | yes | the UI itself |
    | `web/e2e/screens.capture.ts`, `seed.ts`, `server.ts`, `global-setup.ts` | yes | they make the demo state and the session |
    | `web/e2e/fixtures.ts` | only if the capture imports it | the hash covers what the capture imports, listed in one constant |
    | `web/playwright.screens.config.ts` | yes | viewports and options. `playwright.config.ts` is **not** an input, because the capture has its own config |
    | `web/package-lock.json` | yes | Svelte, Vite and Playwright/Chromium versions change the rendering. **Dependabot `web` PRs will therefore flag `screens`.** This is accepted: the refresh procedure below adds the artifact commit to the Dependabot PR |
    | `web/vite.config.ts` | yes | build output |
    | `web/svelte.config.js`, `web/static/` | yes if present (neither exists today) | build output |
    | `docs/api/openapi.json` | yes | API shapes, as a proxy for the Go API |
    | Go code behind the seeded views | **no** | too broad. A behaviour change that keeps the schema is caught by the slow-path byte comparison on `main` (below) |

  - **When the job runs.** On every `ci.yml` run, as a first step that takes seconds. A pathspec diff is not used, so site-only refresh PRs are checked too.
  - **Fast path.** If the hash matches the manifest, the job passes.
  - **Slow path** (hash differs, and on every push to `main`, which also checks that the self-reported hash is honest). The job:
    1. downloads the same-run `web-build` artifact;
    2. builds the binary like `e2e-ui` does;
    3. runs the capture;
    4. **compares the new bytes with the committed files.**

    Then:
    - **identical files** are left out of the result;
    - **changed files and the new manifest** (with a `growth` entry of their `bytes_added`) go into the `site-screens` artifact;
    - the job **reports** "screens are stale: run the refresh procedure";
    - on a push to `main` where the hash matches but the bytes differ, it reports "screens differ with an unchanged hash: an input is missing from the hash, or rendering is not deterministic".
    - Same bytes for unchanged inputs holds only on the same runner image and Chromium version. Both are recorded in the manifest, and a change in either is reported as the cause.
  - **Growth check.** The job fails when one regeneration's `bytes_added` exceeds 2 MB, or when the sum of `growth` over the last 365 days exceeds 50 MB. In that case the maintainer switches the policy to "regenerate at milestone close". This replaces the 3-month manual review. Because `site.yml` and `changes` use `fetch-depth: 0`, history growth also costs checkout time; step 3b records the checkout time.
  - **Security of the job:**
    - `contents: read`, `persist-credentials: false`, actions pinned by SHA;
    - artifacts only from the same run, no secrets, no `pull_request_target`;
    - it never pushes commits.
  - **Warning first, then required (maintainer decision 2026-11-28).**
    - For about a week, the job runs with `continue-on-error: true` and writes the stale report to the job summary; it is not a required check.
    - Once the slow path on `main` has shown the same bytes for an unchanged hash every time that week, the maintainer makes `screens` required.
    - Why: two UI PRs that merge close together can leave `main` stale, and a required check would then block every PR until it is fixed.
    - When the check becomes required, the plan recommends also turning on "Require branches to be up to date before merging" for `main`. Then the second UI PR must rebase and refresh before it merges.
- **Refresh procedure** (for any UI PR, Dependabot `web` PRs, and a stale `main`). It is written in `CONTRIBUTING.md` ("Refresh screenshots") and in `.claude/agents/implementer.md`, which step 3b edits.
  1. Open the failed or warning `screens` job of the PR's latest run. On a stale `main`, use the latest push run on `main`.
  2. Run `gh run download <run-id> -n site-screens -D site/public/screens`.
  3. Commit with the message "Refresh screenshots (run <run-id>)" and push. On a stale `main`, open a PR named "Refresh screenshots" containing only that commit.
  4. The re-run of `screens` takes the fast path and passes.

  **Cost to a UI PR:** the slow path (measured in step 3b, expected 4–6 min), plus the download and commit, plus one more CI run. Step 3b confirms that a cloud agent can run `gh run download`, which needs `actions: read` on its token, and can commit binary files. If it cannot, the maintainer does steps 1–3.
- **`site.yml` paths:** add `web/src/routes/**`, so that a PR that renames or removes a captured route runs gate 10 (the route file check) in that PR.
- **Order against M3b.** Step 3b should land **before** the M3b UI steps. From then on, an M3b PR that changes a captured view includes its refresh commit, and the screenshot diff is part of its review. New M3b screens are not captured until a later plan revision adds them to the allowlist. While the check is still a warning, M3b work is never blocked by it.

### Markup (the `site/src/components/Screen.astro` component)
```html
<figure class="screen">
  <div class="screen-head"><span class="badge" aria-hidden="true">Demo data</span></div>
  <picture>
    <source media="print, (prefers-color-scheme: light)" type="image/webp"
            srcset="/screens/services-light-800.webp 800w, /screens/services-light-1600.webp 1600w"
            sizes="(min-width: 60rem) 50rem, 100vw">
    <img src="/screens/services-dark-800.webp"
         srcset="/screens/services-dark-800.webp 800w, /screens/services-dark-1600.webp 1600w"
         sizes="(min-width: 60rem) 50rem, 100vw"
         width="800" height="500" alt="…" loading="lazy" decoding="async">
  </picture>
  <figcaption>The services list with three services in project demo. Captured <time datetime="2026-11-28">28 Nov 2026</time>. Demo data: the states are staged with seeded rows and mocked API responses.</figcaption>
  <p class="screen-full">
    <a class="only-dark" href="/screens/services-dark-1600.webp">Open full-size image<span class="vh">: services list, dark theme</span></a>
    <a class="only-light" href="/screens/services-light-1600.webp">Open full-size image<span class="vh">: services list, light theme</span></a>
  </p>
</figure>
```
- **Full-size link** (desktop figures only). It opens in the same tab, is at least 44 px tall, and has a unique name. One link per theme is shown with CSS only (`display:none` on the other), so only one is ever in the accessibility tree.
- **Print.** The `<source>` uses `media="print, (prefers-color-scheme: light)"`, so printing uses the light image.
- **Order at 320 px.** The h1 and sub line come before the hero image, both in the DOM and visually.
- **Theme.** Dark is the `<img>` default and light is the `<source>`, the same logic as the site CSS (dark default, light under `prefers-color-scheme: light`). It needs no JS.
- **Layout shift.** `width` and `height` come from the manifest. CSS uses `max-width:100%; height:auto`, so there is no layout shift. Gate 10 checks that both themes have the same dimensions.
- **Loading.** Only the hero image (R3-Q1 is yes) is eager, with `fetchpriority="high"` and no `loading`. Every other image is `loading="lazy"`.
- **CSS only, no `style=`.** The frame uses `--border` (at least 3:1, and `CanvasText` in forced colours). It has a solid `--card` background behind the caption. `prefers-contrast` cannot change the images; the text around them carries the meaning.

### Accessibility
- **Alt text** describes what the screen shows and why it matters, using the on-screen labels.
  - Example for `deploy-live`: "ServerDock's deploy page for deploy #7 of service api in project demo: Pull the image and Start the new version are done, Health check is running, and the live log is below."
  - `rollback-dialog` names the dialog's title and its exact target: "…confirm button Roll back to #5".
  - It never says "screenshot" or "image of".
  - The alt text and captions live in `site/src/data/screens.json`, keyed by id, and the accessibility reviewer edits them there.
- **Gate 10 fails** when alt text:
  - is empty, only whitespace, "…" or a file name;
  - matches `/\b(screenshot|image of|picture of)\b/i`;
  - is over 250 characters;
  - is the same as another alt text on the page;
  - is the same as its figcaption.

  It also fails when a caption does not end with the exact "Demo data: …" sentence.
- **Axe cannot judge alt quality.** The manual pass (VoiceOver and NVDA) is the gate for that.
- **Never the only carrier of information.** Every claim the image supports (stages, rollback, phone use) is also stated in the page text. The image adds evidence; it carries no claim alone. There are no text-in-image claims that are not also in HTML text.
- **Captions.** Captions use `--text` or `--muted` on a solid surface, which the contrast unit test already covers. The caption is the `<figcaption>`; it does not repeat the alt text.
- **Forced colours, zoom and print.** Images stay as they are in forced colours, while the frame and badge get real borders. At 320 px and 400% zoom, images scale to the column width with no horizontal scroll. In print, images print at their column width.
- **Tests.**
  - Axe on Home and Features in every project.
  - The existing heading-order, focus, 44 px and no-other-host checks.
  - Step 3b adds these rows to `docs/accessibility/site-launch-checklist.md`:
    - alt text read with VoiceOver and NVDA;
    - images switching with the OS theme;
    - a forced-colours screenshot of a figure;
    - Home at 400% zoom;
    - print preview of Features.

### Security and privacy
- **Demo data only:**
  - project `demo`; services `web`, `api` and `worker`;
  - images `registry.example.test/demo/<name>:<semver>`;
  - hostnames under `example.test`;
  - the admin user `admin`, which is the username the harness creates (`server.ts`);
  - any IP shown taken from RFC 5737 ranges (192.0.2.0/24);
  - image digests and SHAs taken from a fixed list of demo values in the spec;
  - no real tokens, emails or hosts.
- **Leak check before each capture** (the capture fails on any hit). It scans the clip region's `innerText` plus the value and placeholder of every form control on the page.
  - **Layer 1, exact matches against runtime values:**
    - the setup token, TOTP secret and each recovery code (from `server.ts`'s `Admin`);
    - `PASSWORD`;
    - the session cookie value and the CSRF token;
    - the temp data directory path;
    - `os.hostname()` and `os.userInfo().username`;
    - every non-loopback address from `os.networkInterfaces()`.
  - **Layer 2, patterns:**
    - the API token prefix;
    - TOTP, QR or recovery-code elements;
    - `127.0.0.1` and `localhost`;
    - a hostname not under `example.test`;
    - an email address;
    - any string of 32+ characters that looks like a secret and is **not** one of the allowlisted demo digests.
  - **Route check:** the route must be one of the six allowlisted ids.
- **Review of committed images.** GitHub shows image diffs in PRs, so the reviewer looks at every changed screenshot.
- **CSP.** `img-src 'self'` already covers `/screens/`. No CSP change and no third-party request.
- **The `screens` CI job.** No secrets, `contents: read`, `persist-credentials: false`, actions pinned by SHA, artifacts only from the same run, no `pull_request_target`, and the same temp-directory test server as `e2e-ui`.
- **CODEOWNERS** adds `/web/e2e/screens.capture.ts`, `/site/public/screens/` and `/site/src/data/screens.json`.

### Checks (gate 10 in `gates.mjs`, `node:` built-ins only)
- **Manifest.** It passes the strict schema above. Every route is in the **code-owned allowlist of six route ids in `gates.mjs`**, and the gate fails on any route under `(auth)/` or containing `settings/`.
- **Files.** Every file under `site/public/screens/` except `manifest.json` is listed in the manifest, and every manifest file exists.
- **WebP structure.** Each file starts with the `RIFF`/`WEBP` magic and contains only `VP8 `, `VP8L`, `VP8X` and `ALPH` chunks. `EXIF`, `XMP ` and `ICCP` fail the gate.
- **Route files.** The route file exists for each entry.
- **Themes.** Both themes have the same `width`/`height`.
- **Image attributes.** Every `<img>` has `width`, `height`, `alt` and `decoding="async"`.
- **Text rules.** The alt and caption rules above.
- **Ids.** `screens.json` and `manifest.json` have the same ids.
- **Gate 11 (`page-weight.mjs`):** see section 4.
- **Unit tests:** a failing fixture for each check, including a disallowed route, an unknown manifest key, a stray file, a WebP with an EXIF chunk, a duplicate alt text, and alt text equal to its caption.

### Step 3b (new, one PR)
1. **Capture:** `web/e2e/screens.capture.ts` and `web/playwright.screens.config.ts`. This covers the route allowlist, the typed fixtures, `tsconfig.e2e.json` with the `tsc --noEmit` step in the `web` job, the canvas WebP encoder, and the two-layer leak check. The main config must ignore the spec in every set.
2. **`make screens`** (preview only).
3. **The `screens` job in `ci.yml`:** fast path, slow path with byte comparison, growth check, starting as a warning with `continue-on-error`. Also its comment, and `web/src/routes/**` added to `site.yml` paths.
4. **The first committed set** in `site/public/screens/` with `manifest.json`, **taken from the CI artifact**.
5. **Site side:** `site/src/data/screens.json` (alt text and captions), the `Screen` component (badge, full-size links, print source), gates 10–11 with fixtures, and the styleguide entry.
6. **Docs:** the refresh procedure in `CONTRIBUTING.md` and `.claude/agents/implementer.md`, the CODEOWNERS entries, and the checklist rows.
7. **Record:**
   - the real file sizes after WebP quality tuning (raise a cap with a written reason rather than make text unreadable);
   - the total;
   - the phone-hero decision and its worst case;
   - the fast-path and slow-path `screens` job times;
   - the checkout time;
   - whether a cloud agent could download the artifact and commit it.

**Tests:**
- The capture spec's own assertions, including the leak check against a page that contains a planted runtime value.
- `tsc --noEmit` on the e2e config.
- The gate unit tests.
- In CI:
  - changing a file under `web/src/` without refreshing makes `screens` report stale;
  - committing the artifact makes it pass;
  - a push to `main` with an unchanged hash gives identical bytes;
  - `E2E_SET` unset does not run the capture.
- Playwright at 320 px: only the eager image loads on the first view.
- Axe on the styleguide page with a `Screen`.
- No layout shift: every `<img>` has dimensions, checked by the gate.
- Manual: both themes switch with the OS setting, and alt text is read correctly.

**Reviews:**
- accessibility (**required**): alt text, captions, figure.
- footprint (**required sign-off**): budgets, encoder, repo growth.
- security: demo data, excluded routes, the CI job.
- reviewer: honesty, meaning only built screens and typed mocks.

### Maintainer questions (Revision 3)
**Maintainer answers (2026-11-28):**
- **R3-Q1:** yes. The hero shows the real `deploy-live` screenshot.
- **R3-Q2:** strict. A PR whose screenshot inputs changed must commit refreshed screenshots.
- **R3-Q3, replaced by a later decision the same day:** `screens` **starts as a warning** (not required) for about a week. Once the hash and bytes prove stable, it becomes required. A documented "refresh screenshots" PR procedure fixes `main` if it ever goes stale. Reason: two UI PRs merging close together can leave `main` stale, and a required check would then block every PR.

- **R3-Q1. Replace the HTML deploy card in the hero with the real `deploy-live` screenshot?** *Recommendation: yes.* "No HTML replica" suggests it, and the real screen is stronger evidence. The 54 MB meter stays, because it shows site data and is not a copy of the UI. The deploy-stages component from step 2 is reused for the "Built with AI, led by a human" pipeline list.
- **R3-Q2. How strict is freshness?** *Recommendation: fail on every PR whose inputs changed* (true "never out of date"), with the measured repo growth reviewed after 3 months. The alternative is regenerating at milestone close and showing the capture date, which means less repo growth but screenshots that may lag the UI by one milestone.
- **R3-Q3. Make `screens` a required check?** *Recommendation: yes.* It runs on every `ci.yml` run and takes seconds when the screenshots are fresh, so it never blocks as a skipped path-filtered check would. *(Replaced: it starts as a warning; see the maintainer answers above.)*

### Revision 3, review fixes

All four reviewers returned REVISE on Revision 3. Each fix is made in the Revision 3 text above or in section 4.

**Reviewer**
1. **Typed mocks.** Fixtures use `satisfies` against `api-types.ts`, and `tsconfig.e2e.json` with `tsc --noEmit` runs in the `web` job. A local Node shim is preferred over adding `@types/node`.
2. **Stage labels.** The real `STAGE_LABELS` text is used in the views table and the alt examples. Alt text must use on-screen labels. The Home stage list is taken from the code, not typed by hand.
3. **Commit.** **Dropped** from captions and the manifest. The caption shows the capture date; the manifest has `inputs_hash` and `run_id`.
4. **Stale `main`.** The check starts as a warning, then becomes required. There is a refresh procedure and a "Refresh screenshots" PR for `main`, and the plan recommends "require branches up to date" once the check is required.
5. **Freshness inputs.** There is a decision table for each input. The capture has its own Playwright config, so `playwright.config.ts` is not an input. Dependabot `web` PRs will flag `screens`, which is accepted with the refresh procedure. Go code is not hashed but is caught by the byte comparison on `main`. The per-PR cost is stated. The procedure goes into CONTRIBUTING and the implementer agent file. A cloud agent's ability to download and commit is confirmed in step 3b. `E2E_SET` unset or `all` never runs the capture.
6. **Order.** Step 3b goes before the M3b UI steps.
- **NITs:** `web/src/routes/**` is added to the `site.yml` paths. Only the CI artifact may be committed. "Same bytes" is limited to the same runner image and Chromium version, and both are recorded.

**Security**
1. **Leak check.** Two layers: exact runtime values (setup token, TOTP secret, recovery codes, `PASSWORD`, session cookie, CSRF token, data directory, hostname, username, non-loopback addresses) and patterns. It scans the clip's `innerText` plus form values and placeholders. Clipping is required, and the status footer is kept outside the clip.
2. **Allowlist.** The capture spec has a route allowlist, and gate 10 has a code-owned one that rejects `(auth)/` and `settings/`. The manifest schema is strict and fails on unknown keys.
3. **Files.** Gate 10 checks the WebP magic and allowed chunks (rejecting EXIF, XMP and ICCP) and fails on any file not in the manifest.
- **NITs:**
  - the admin is `admin`;
  - exact demo digests are allowlisted;
  - the job settings are hardened;
  - CODEOWNERS entries are added;
  - the self-reported hash is verified by the byte comparison on `main`;
  - only the CI artifact may be committed.

**Accessibility**
- **R1.** A "Page text that carries the claim" column and a reviewer checklist item. The hero gets a visible stage list and a sentence.
- **R2.** Gate 10 alt checks: unique per page, not equal to the caption, not empty, "…" or a file name. The caption has one gated final sentence. The rollback alt names the title and target.
- **R3.** The badge sits in the frame header outside the crop, on a solid background with a border and at least 4.5:1 contrast, and is `aria-hidden`. The caption says the states are staged with seeded rows and mocked responses.
- **NITs:**
  - full-size links: one per theme, CSS-only, at least 44 px, unique names, same tab;
  - desktop capture at about 1024 px clipped to about 800 px;
  - h1 and sub line before the hero at 320 px;
  - a print source;
  - forced-colours frame and badge;
  - `prefers-contrast` noted;
  - checklist rows added;
  - the manual pass is the gate for alt quality.

**Footprint**
1. **Gate 11** is specified: the worst case is the heavier theme's 2x file per `<picture>`, lazy images are left out of the first view, a second eager image fails, per-file and total caps apply, there are failing fixtures, and images count in raw bytes.
2. **Home** images are at most 190 KB with at most 5 first-view requests. Features follows the single-eager rule (one eager image, 120 KB or less). Step 3b makes the phone-hero decision (art direction) and records its worst case.
3. **Growth.** Bytes are compared, and identical files are not committed. The manifest `growth` list is checked by CI (2 MB per regeneration, 50 MB per 365 days). The `fetch-depth: 0` checkout cost is recorded.
- **NITs:** slow-path and checkout times are recorded; a Playwright check at 320 px; WebP quality is tuned, and a cap is raised with a written reason rather than making text unreadable.

**Where I differ, for the reviewers to decide**
- **Slow path on every push to `main`.** This costs one capture run (expected 4–6 min) per `main` push. It is the only way to verify the self-reported hash (security NIT) and to catch Go-side rendering changes (reviewer 5). I kept it. If footprint objects, the alternative is a weekly scheduled run, which already exists as the nightly `ci.yml` schedule.
- **Go code is not hashed.** Hashing Go packages would make most backend PRs flag `screens`. Catching this through the byte comparison on `main` instead means `main` can briefly go stale after a Go-only change that alters a view. The warning report and the refresh procedure cover that case.
