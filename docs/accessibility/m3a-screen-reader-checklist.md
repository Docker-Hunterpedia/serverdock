# M3a manual screen-reader checklist (plan 0004, steps 12 to 14)

**Result: PENDING (maintainer).** No one has run this checklist. Automated checks (axe, keyboard and focus tests in Playwright) do not replace it. Record the outcome in the step 14 pull request, one line per row of the results table, and update this line only when the maintainer has run it.

## Set-up

- Build and start ServerDock, sign in, and deploy the test app once so that a service with history exists (`127.0.0.1:5000/sd-testapp:1`, port 8080, or any small public image). For the failure rows, deploy an image tag that does not exist.
- Use a separate browser profile with no extensions. Browse the panel at its own address.
- Combinations to run:

| Id | Screen reader | Browser | System |
|---|---|---|---|
| A | NVDA (current release) | Firefox (current release) | Windows |
| B | VoiceOver | Safari | macOS |
| C | VoiceOver | Safari | iOS |
| D | TalkBack | Chrome | Android |

- Run every page below on every combination. Keyboard steps do not apply to C and D; use swipe navigation and the rotor or the reading controls instead.
- Expected means "heard in this order or similar wording"; the exact words may differ by a screen reader, but the **content** (state word, name, consequence) must be there. Anything that is silent, announced twice, announced in the wrong place, or that moves focus unexpectedly is a failure: write down the page, the combination and what happened.

## Step 12: Services, New service, Overview

1. **Overview** (`/`)
   - On load the page title and the heading "Overview" are announced.
   - Service counts and the "Needs attention" list are read as lists, each state as a word plus a symbol word ("Running", not only a dot).
   - If the exposure banner shows, it is read as text, with a link named "Read about this".
   - Expected silence: nothing is announced by itself while the page sits idle (it does not poll).
2. **Services** (`/services`)
   - Desktop width: the table is announced with its caption; each row reads name, state word, image, live deploy, address, last deploy.
   - Phone width (or zoom 200%): each service is a card with the same words, in the same order.
   - The "New service" button is reachable and named.
3. **New service** (`/services/new`)
   - Each field announces its label and its hint (the public-port field reads the firewall and shared-address hints).
   - Submit with a bad name: the error summary is announced at once (alert), focus moves to it, and its links jump to the fields. Typed text is kept.
   - A good submit: the Deploy page opens, focus is on its heading.

## Step 13: Service detail and its dialogs

4. **Service detail** (`/services/<project>/<service>`)
   - Heading "<service>" with the state word. Cards "Running now", "Traffic", "Strategy" are headings with text under them.
   - The sub-navigation is announced as a navigation named "Service sections"; the current link is announced as the current page.
   - **Refresh**: "Refreshing" then "Updated" are announced; focus stays on the button.
5. **Dialogs: Deploy, Redeploy, Restart, Roll back, Delete, public-port confirmations**
   - On open: the dialog's title and body (target and consequence) are announced; focus is on **Cancel**.
   - Tab stays inside the dialog; Escape closes it; focus returns to the button that opened it.
   - A refused request (for example an unknown tag): the error is announced once and the typed text stays.
   - Delete: the confirm button is announced as unavailable until the target is typed exactly; the hint is read.
6. **Settings**
   - Saving announces "Saved. Deploy to apply." once. Field errors follow the same pattern as New service.
   - Public port: the confirmation dialog says "Applied now: ..." with the address.
7. **Deploys list**
   - Each result is a word plus symbol ("Failed . app safe", "Interrupted", "Superseded by #N").
   - "Roll back to #N" is offered only on deploys that were once live.

## Step 14: Deploy detail with live progress

8. **A deploy that runs** (queue a deploy of a large image, or one that waits in the health gate)
   - On open: "Deploy #N" and its status word; "What happened" and "Stages" are headings. Stages are an ordered list; each item reads "<stage>: Done / Running / Waiting / Failed / Skipped".
   - While it pulls: a labelled progress bar "Image pull progress" with its text can be reached by browsing. **It is not announced as it changes**, and nothing is announced for progress ticks.
   - Each stage change is announced **once**, politely, for example "Now: health check." The announcement does not interrupt what you are reading, and focus does not move.
   - On success: "Deploy #N succeeded. The new version is live." is announced once.
   - Leave the page open and read on for a minute: no repeated announcements.
9. **A deploy that fails** (nonexistent tag, or an image that never turns healthy)
   - The failure is announced **once**, at once (alert), with the reason and "Your app: safe, #M kept serving" (or "not safe" / "unknown"). Focus does not move on its own.
   - Under "What happened": result, "Your app", "New container", "Reason" are read as term and description pairs.
   - A disk-full or interrupted deploy shows its extra paragraph.
10. **Cancel deploy**
    - While pulling or queued: the button is named "Cancel deploy"; activating it announces "Cancel request sent" and then either the cancelled state or "Cancel requested. The deploy stops at its next safe point."
    - During the switch stage (use a very short health check on a fast image, or watch a deploy with a long drain): the button is announced as **unavailable (dimmed)** and its reason ("Traffic is being switched ...") is read with it. Activating it does nothing.
11. **Hidden and visible again**
    - Switch to another tab or app while a deploy runs, wait for it to finish, and come back. The page refreshes first; then at most one announcement of the current state is made (not one per missed stage).
    - While away, no sound or announcement comes from the page.
12. **Status unknown**
    - Stop the server (or disconnect the network) while a deploy page is open. Within about 10 s the page shows "Status unknown: could not reach the server" in place of the stage list. It is **not** announced; browsing finds it. A "Check again" button is reachable.
    - Restart the server: the page recovers by itself and the state is shown again.
13. **Container log**
    - "Container log (last 40 lines)" is a heading. The skip link "Skip the container log" is the first stop after it; activating it moves focus to "Show full log".
    - The log is a focusable region with a name ("Container output, last 40 lines"). It can be scrolled by keyboard (arrow keys, Page Down) and sideways for long lines. Its text is read as plain text, never as markup.
    - "Show full log" (expanded state announced) adds a separate block named "Deploy header, written by ServerDock" and a block "Container output, full log"; the headings "Written by ServerDock" and "Printed by the container" tell them apart.
    - An old deploy says "Log no longer kept (only the last 10 deploys keep logs)". A busy server says to try again in a moment, and "Try again" works.
14. **Roll back / Redeploy on the Deploy page**
    - Same dialog behaviour as item 5. After confirming, the new deploy's page opens and focus is on its heading.

## Results

| Combination | Pages 1-7 (steps 12, 13) | Pages 8-14 (step 14) | Notes |
|---|---|---|---|
| A NVDA + Firefox | PENDING (maintainer) | PENDING (maintainer) | |
| B VoiceOver + Safari (macOS) | PENDING (maintainer) | PENDING (maintainer) | |
| C VoiceOver + Safari (iOS) | PENDING (maintainer) | PENDING (maintainer) | |
| D TalkBack + Chrome | PENDING (maintainer) | PENDING (maintainer) | |

Findings go in the pull request and, when they need work, in `docs/plans/` as a follow-up. A step is not closed on this checklist until the maintainer replaces PENDING with a dated result.
