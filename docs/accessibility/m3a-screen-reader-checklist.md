# M3a manual screen-reader checklist (plan 0004, steps 12 to 14)

**Result: PENDING (maintainer).** No one has run this checklist. Automated checks (axe, keyboard, focus and announcement-recording tests in Playwright) do not replace it. Record the outcome in the step 14 pull request using the results table at the end; fill in the date, commit and tester fields when it is run. Never mark a row done without a tester name and a date.

## As built (what is already checked, and where the build departs from the plan)

**Covered by automated tests** (Playwright with axe in all 8 projects: desktop, 390 px, 320 px, 200 % zoom, dark, reduced motion, forced colours, WebKit):
- zero axe violations on every listed screen state, and no CSP violations;
- keyboard-only paths, 44 px targets, default focus on Cancel in dialogs, Escape, focus returning;
- each deploy stage transition announced once and no announcement for progress ticks (a MutationObserver records the live regions), and a failure announced once through the alert;
- polling stops when the page is hidden or the deploy finished, and backs off after 2 minutes of pulling;
- a hostile log renders as text.

**What the tests cannot say:** how a real screen reader speaks any of this. That is this checklist. **The manual pass is PENDING (maintainer).**

**Departures from the plan, with reasons:**
- **"Status unknown" keeps the content on the page, marked stale**, instead of replacing it: removing the page lost the user's place and the last good state.
- **Focus moves to the page heading only when the focused control vanished** (for example Cancel when a deploy ends); otherwise focus is left where the user put it.
- **The log is `role="group"` with a name**, so a reader announces what it is when entering it; its lines are plain text.
- **The M2 footer status poll** (every 15 s while the page is visible) is still the only always-on poll outside a deploy page.

**Known gaps:**
- Cancel is disabled while the status is unknown (a trade-off, `reviewer`).
- The failure alert says "the previous version" without the deploy number.
- There is no progress bar when the registry gives no size (the pull shows text only).

## Set-up

- Build and start ServerDock, sign in, and deploy the test app once so that a service with history exists (`127.0.0.1:5000/sd-testapp:1`, port 8080, or any small public image, with `--allow-loopback-registry` for the local one).
- Use a separate browser profile with no extensions. Browse the panel at its own address.
- Combinations to run:

| Id | Screen reader | Browser | System |
|---|---|---|---|
| A | NVDA (current release) | Firefox (current release) | Windows |
| B | VoiceOver | Safari | macOS |
| C | VoiceOver | Safari | iOS |
| D | TalkBack | Chrome | Android |

- Run every item below on every combination. Keyboard steps do not apply to C and D; use swipe navigation, the rotor (C) or the reading controls (D) instead.
- **Expected** means the **content** is heard (state word, name, consequence). The exact strings in "Announcements" below are what the page puts into its live regions; a screen reader may add its own words ("alert", "status") or drop the symbols. Anything that is silent, announced twice, announced in the wrong place, or that moves focus unexpectedly is a failure: write down the item, the combination and what happened.
- **NVDA only** (Preferences, Settings, Document formatting and Object presentation):
  - Report dynamic content changes: **on**.
  - Report progress bar updates: **off** (the default is to beep; some versions speak it).
  - The pull's progress bar must stay silent whatever this is set to. If NVDA speaks it anyway, record the setting as a note, not a page failure: the bar is not a live region.

## Announcements (exact strings in the page's live regions)

Status region (polite, `role="status"`):

| When | Text |
|---|---|
| A stage starts | `Deploy #4: health check started.` (stage nouns: image pull, checks, maintenance page, first stop of the old version, start of the new version, health check, traffic switch, recording of the switch, draining of the old version, retirement of the old version) |
| A stage failed and will be retried | `Deploy #4: image pull did not succeed. Waiting to retry, attempt 2 of 3.` |
| Success | `Deploy #4 succeeded. The new version is live.` |
| Cancelled | `Deploy #4 was cancelled.` |
| Superseded, never started | `Deploy #4: replaced in the queue by #5 before it started.` |
| Superseded, was running | `Deploy #4: stopped and replaced by #5.` |
| Cancel clicked | `Cancel request sent, waiting for the answer…`, then `Cancel requested. The deploy stops at its next safe point.` |
| Status becomes unknown | `Status unknown: could not reach the server.` (once) |
| Status current again, by polling | `Status is current again.` (once) |
| Check again | `Checking…`, then `Status updated.` or `Still could not reach the server.` |
| Show full log | `Full log shown.` / `Last 40 lines shown.` |

Alert region (assertive, `role="alert"`, once per failure):

- `Deploy #4 failed: <Reason>. Your app is safe: the previous version kept serving.`
- `Deploy #4 failed: <Reason>. Your app is not safe: the previous version is not serving. ...`
- `Deploy #4 was interrupted: <Reason>. Your app status is unknown: ServerDock could not confirm ...`
- `Deploy #4 was cancelled. Your app is not safe: ...` (only when the cancel left the app down)
- Errors of an action you took (Cancel, Redeploy, Roll back) also use the alert, and focus moves to it.

## How to produce each state

Real deploys are preferred. For states that are hard to reach, the Playwright fixtures in `web/e2e/seed.ts` (`deployStates.*`) answer `deploys.get` with that state; run the page under `npx playwright test e2e/deploy-detail.spec.ts --headed` with a pause, or ask a maintainer to host them. Record which method was used.

| State | Real | Fixture |
|---|---|---|
| Pulling with a bar | Deploy a large public image | `pulling`, `pullingUnknownSize` |
| Waiting to retry | Queue a deploy, then stop the local registry (`docker stop sd-registry`) while it pulls | `waitingToRetry` |
| Health gate | Deploy an image with a health path that answers late | `healthGate` |
| Switch (Cancel disabled) | Very brief in real life: use the fixture | `switching` |
| Failed, app safe | Deploy a tag whose health check never passes on a service that is live | `failedAppSafe` |
| Failed, app not safe | The same on a stop-first service | `failedNotSafe` |
| Cancelled | Click Cancel deploy while a large image pulls | `cancelled`, `cancelledNotSafe` |
| Superseded | Queue deploy A (large image) and, while it pulls, queue deploy B: A is stopped and replaced. Queue B and C while A runs: B was replaced in the queue | `supersededStarted`, `superseded` |
| Interrupted | `kill -9` ServerDock while a deploy runs, start it again | `interrupted` |
| Disk full | A VM with a small Docker root, filled up | `diskFull` |
| Log no longer kept | Open deploy #1 of a service with more than 10 deploys | seeded service with 12 deploys |

## Items

### Step 12: Services, New service, Overview

1. **Overview** (`/`): the page title and heading "Overview" are announced on load. Counts and the "Needs attention" list are read as lists, each state as a word plus a symbol word ("Running"). The exposure banner is read as text with a link named "Read about this". Nothing is announced by itself while the page sits idle (it does not poll).
2. **Services** (`/services`): the table is announced with its caption and each row reads name, state word, image, live deploy, address, last deploy. At phone width (or zoom 200%) each service is a card with the same words. "New service" is reachable and named.
3. **New service** (`/services/new`): each field announces its label and hint (the public-port field reads the firewall and shared-address hints). A bad name: the error summary is announced at once (alert), focus moves to it and its links jump to the fields; typed text is kept. A good submit opens the Deploy page with focus on its heading.

### Step 13: Service detail and dialogs

4. **Service detail**: heading with the state word; the cards are headings with text; the sub-navigation is a navigation named "Service sections" with the current link announced as current. **Refresh** announces "Refreshing" then "Updated"; focus stays on the button.
5. **Dialogs** (Deploy, Redeploy, Restart, Roll back, Delete, public-port confirmations): on open the title and body (target and consequence) are announced and focus is on **Cancel**. Tab stays inside, Escape closes, and focus returns to the opening button. A refused request is announced once and the typed text stays. Delete: the confirm button is unavailable until the target is typed exactly and its hint is read.
6. **Settings**: saving announces "Saved. Deploy to apply." once; field errors follow item 3. The public-port confirmation says "Applied now: ..." with the address.
7. **Deploys list**: each result is a word plus symbol; "Roll back to #N" appears only on deploys that were once live.

### Step 14: Deploy detail

8. **A deploy that runs** (pulling, then health, then done). On open: "Deploy #N" and its status word. "What happened", "This deploy" and "Stages" are headings; stages are an ordered list and each item reads "Pull the image: Done" / "Running" / "Waiting" / "Failed" / "Skipped". The current stage is announced as current step when browsing.
   - The pull's progress bar "Image pull progress" and its text can be reached by browsing and are **not announced as they change**. When the size is not known there is no bar, only the text.
   - Each stage change is announced **once** and politely (see the table); it does not interrupt your reading; focus does not move.
   - Success is announced once. Stay on the page for a minute: nothing repeats.
9. **A deploy that fails.** The failure is announced **once**, at once (alert), with the capitalised reason and the app line: "Your app is safe: the previous version kept serving." Focus does not move. Browse to "What happened": Result, Your app, New container and Reason are read as term and description pairs; "Your app" there names the previous live deploy (#M) once the page has looked it up. Check the same for the interrupted, disk-full and not-safe states, and for a cancelled and a superseded deploy: **a cancelled deploy must not claim "kept serving" unless the page says "safe"**.
10. **Cancel deploy.** While queued or pulling the button is named "Cancel deploy"; activating it announces "Cancel request sent..." then the cancelled state or "Cancel requested...". At the switch stage the button is announced as **unavailable** and its reason is read with it ("Traffic is being switched ..."); activating it does nothing. **If Cancel has focus when the deploy finishes**, focus moves to the "Deploy #N" heading (it is not lost to the page start).
11. **Hidden and visible again.** Switch to another tab or app while a deploy runs, wait for it to finish, return. The page refreshes first; then at most one announcement of the current state (not one per missed stage). While away the page makes no sound.
12. **Status unknown.** With a deploy running, stop the server or disconnect the network.
    - Within about 10 s "Status unknown: could not reach the server." is announced **once**. The page **keeps the last known content** and shows the note "Status unknown: could not reach the server. What is shown below may be out of date." above it; Cancel, Redeploy and Roll back are announced as unavailable with the reason. A "Check again" button is reachable.
    - Activate **Check again** while still offline: "Checking…" then "Still could not reach the server." Nothing else is announced.
    - Restore the server: without touching the page, "Status is current again." is announced once and the state updates. Or activate Check again: "Checking…", "Status updated.", and focus is on the "Deploy #N" heading (the button has gone).
    - A server that answers 403 is shown as a refusal with its own message, not as "could not reach the server".
13. **Container log.** "Container log (last 40 lines)" is a heading. The skip link "Skip the container log" appears over the log's corner when focused (nothing on the page moves) and moves focus to "Show full log".
    - The log is a group named by that heading and is focusable. **NVDA:** in browse mode the text is read line by line with the arrow keys; Tab to the log puts NVDA in focus mode on the group: arrow keys then scroll it (Page Down, sideways with Right arrow for long lines). Note which mode feels right. **iOS:** use the rotor to move by heading to the log, then swipe to read; a long line is scrolled with a three-finger swipe. **TalkBack:** scroll sideways with a two-finger swipe.
    - The log text is read as plain text, never as markup.
    - "Show full log" announces "Full log shown." and adds a separate block named "Written by ServerDock" and a block named "Printed by the container"; the button then reads "Show last 40 lines only" and announces "Last 40 lines shown." when used.
    - An old deploy says "Log no longer kept (only the last 10 deploys keep logs)". A busy server says to try again in a moment; "Try again" works and focus is on the heading afterwards.
14. **Redeploy / Roll back on the Deploy page.** Same dialog behaviour as item 5. **Redeploy asks the server again first**; the dialog says "Redeploys the live version of <service>: deploy #N (<image>)". After confirming, the new deploy's page opens with focus on its heading.
15. **Moving to another deploy** (after Redeploy, or Back): the notice and warnings of the first deploy do not follow, nothing of the first is announced, and the log shown is the new deploy's.

## Results

Fill one row per item per combination. Allowed values: PENDING (maintainer), PASS, FAIL (note), N/A. Everything below is **PENDING (maintainer)**: nobody has run it.

| Item | A NVDA + Firefox | B VoiceOver + Safari (macOS) | C VoiceOver + Safari (iOS) | D TalkBack + Chrome | Notes |
|---|---|---|---|---|---|
| 1 Overview | PENDING (maintainer) | PENDING (maintainer) | PENDING (maintainer) | PENDING (maintainer) | |
| 2 Services | PENDING (maintainer) | PENDING (maintainer) | PENDING (maintainer) | PENDING (maintainer) | |
| 3 New service | PENDING (maintainer) | PENDING (maintainer) | PENDING (maintainer) | PENDING (maintainer) | |
| 4 Service detail | PENDING (maintainer) | PENDING (maintainer) | PENDING (maintainer) | PENDING (maintainer) | |
| 5 Dialogs | PENDING (maintainer) | PENDING (maintainer) | PENDING (maintainer) | PENDING (maintainer) | |
| 6 Settings | PENDING (maintainer) | PENDING (maintainer) | PENDING (maintainer) | PENDING (maintainer) | |
| 7 Deploys list | PENDING (maintainer) | PENDING (maintainer) | PENDING (maintainer) | PENDING (maintainer) | |
| 8 A deploy that runs | PENDING (maintainer) | PENDING (maintainer) | PENDING (maintainer) | PENDING (maintainer) | |
| 9 A deploy that fails | PENDING (maintainer) | PENDING (maintainer) | PENDING (maintainer) | PENDING (maintainer) | |
| 10 Cancel deploy | PENDING (maintainer) | PENDING (maintainer) | PENDING (maintainer) | PENDING (maintainer) | |
| 11 Hidden and visible | PENDING (maintainer) | PENDING (maintainer) | PENDING (maintainer) | PENDING (maintainer) | |
| 12 Status unknown, Check again | PENDING (maintainer) | PENDING (maintainer) | PENDING (maintainer) | PENDING (maintainer) | |
| 13 Container log | PENDING (maintainer) | PENDING (maintainer) | PENDING (maintainer) | PENDING (maintainer) | |
| 14 Redeploy / Roll back | PENDING (maintainer) | PENDING (maintainer) | PENDING (maintainer) | PENDING (maintainer) | |
| 15 Another deploy in place | PENDING (maintainer) | PENDING (maintainer) | PENDING (maintainer) | PENDING (maintainer) | |

| Field | Value |
|---|---|
| Date run | PENDING (maintainer) |
| Commit tested | PENDING (maintainer) |
| Tester | PENDING (maintainer) |
| Screen reader and browser versions | PENDING (maintainer) |

Findings go in the pull request and, when they need work, in `docs/plans/` as a follow-up. A step is not closed on this checklist until the maintainer replaces PENDING with dated results.
