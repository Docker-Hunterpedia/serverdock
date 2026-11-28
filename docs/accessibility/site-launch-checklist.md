# Public website: manual launch checklist (plan 0014, step 6)

**Result: PENDING (maintainer).** No one has run this checklist. The automated checks (axe, keyboard, focus, 44 px targets, zoom, text spacing and forced colours in Playwright, under the real CSP) do not replace it: axe cannot hear a screen reader and cannot see forced-colour or print rendering. Fill in a row only with a tester name and a date. Never mark a row done without both.

This is AI-written and has not been confirmed by the maintainer.

## What the automated tests already cover

Playwright with `@axe-core/playwright` runs against `site/scripts/serve.mjs`, which sends the production CSP. It runs every page in the sitemap, the 404 page and (in the test build only) `/styleguide/`, in 11 projects: desktop, phone, 320 px, 200% zoom, 400% zoom, dark, light, reduced motion, forced colours, `prefers-contrast: more` and WebKit.

- zero axe violations (tags `wcag2a`, `wcag2aa`, `wcag21a`, `wcag21aa`, `wcag22aa`, `best-practice`), and no colour-contrast "incomplete" result;
- no CSP violation in the console, and no request to any other host;
- skip link first in tab order and moving focus to `main`, and visible focus on every link and summary: Chromium projects only, plus WebKit on Linux CI. Playwright's WebKit on macOS does not Tab to links, so Safari on macOS is manual (row 17). The narrow menu opening with Enter and Space runs in every project;
- one `h1` and no skipped heading levels; no horizontal scroll at 320 px or 400% zoom; no sticky header;
- 44 by 44 px targets and 24 px in-text links at phone, 320 px and 400% zoom widths;
- text-spacing override (WCAG 1.4.12) with nothing clipped.

## Set-up

- Production build from the commit under test, served from the real host (`https://serverdock.oday-bakkour.com`) and from `https://serverdock.pages.dev`.
- A separate browser profile with no extensions.
- Page types to walk: Home, Features, Benchmarks (tables), Roadmap, a doc page with a table of contents, Security, 404.

## Checks

| # | Check | Tool and browser | Date | Result | Sign-off |
|---|---|---|---|---|---|
| 1 | Read every page type top to bottom: headings, landmarks, the two navs (only one is announced), the status pill name, the Evidence links, the deploy card as a list with "done" words, the meter text, table captions and cells | VoiceOver, Safari on macOS | | | |
| 2 | The same on a phone: the Menu summary, the Docs pages summary, table regions reachable and named | VoiceOver, Safari on iOS | | | |
| 3 | The same walk | NVDA with Firefox, or Narrator with Edge | | | |
| 4 | The same walk on a phone | TalkBack, Chrome on Android | | | |
| 5 | Keyboard only through every page type: skip link, order, no trap, focus ring never hidden, table regions scrollable with the arrow keys, Menu opens with Enter and Space | Keyboard, any desktop browser | | | |
| 6 | 200% browser text-only zoom: nothing lost or overlapping | Firefox (text zoom) | | | |
| 7 | 400% zoom: one column, no horizontal scroll except in table regions and code | Chrome or Firefox | | | |
| 8 | Text-spacing bookmarklet (WCAG 1.4.12): nothing clipped | Any browser | | | |
| 9 | Windows high contrast: borders, pill, chips, callouts, cards, table regions, meter track and fill, stage ticks all visible; screenshots attached | Windows, Edge | | | |
| 10 | Firefox forced colours with the same screenshots | Firefox | | | |
| 11 | `prefers-contrast: more` turned on in the OS: muted text and lines become stronger | OS setting | | | |
| 12 | Reduced motion turned on in the OS: nothing moves, no smooth scrolling | OS setting | | | |
| 13 | Print preview: light colours, no nav, link addresses after links | Any browser | | | |
| 14 | "Fast 3G": Home is readable in under 2 s | Chrome DevTools throttling | | | |
| 15 | Zero CSP violations in the console on every page type, on the custom domain | Chrome DevTools | | | |
| 16 | One Lighthouse run, recorded for information only | Lighthouse | | | |
| 17 | Safari on macOS with Option+Tab (or Full Keyboard Access): the skip link comes first and moves focus to `main`; the focus ring is visible on every link and summary, on every page type | Keyboard, Safari on macOS | | | |
| 18 | Screenshot figures: alt text of every figure read (Home and Features) so it names the on-screen labels and says nothing the page text does not | VoiceOver, Safari; NVDA, Firefox | | | |
| 19 | The screenshots switch between the dark and light image with the OS theme, with no reload | OS setting, any browser | | | |
| 20 | A forced-colours screenshot of a figure: the frame and the "Demo data" badge keep their borders | Windows high contrast, Edge; screenshot attached | | | |
| 21 | Home at 400% zoom: the hero image scales to the column, no horizontal scroll, the h1 and sub line come first | Chrome or Firefox | | | |
| 22 | Print preview of Features: the light images are used, the full-size links are hidden, the stage ticks and meter keep their border | Any browser | | | |

A 1 GB VPS check is not relevant: nothing runs on the user's server.

## Results

Record the commit, the date and the tester for each row above, and attach screenshots for rows 9 and 10.
