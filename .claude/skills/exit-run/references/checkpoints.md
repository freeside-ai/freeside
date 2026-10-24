# Checkpoint Lists

Templates for the exit run's gate table and its five operator checkpoints.
SKILL.md says when each is used; this file fixes their shape so every run
asks the same way. Replace every bracketed placeholder and delete rows the
contract doesn't need. Send each list as one message and wait for all of
its answers; between lists, ask the operator nothing. A row marked
"(after my step)" waits for something you do mid-list, such as a test
notification; the row says what to watch for, and you say when you sent
it.

Each row names the device, the action, and what to report back. The
operator never reports what the record shows (status, timeline, delivery,
spend): read those with `freesided inspect task` and `freesided follow
-snapshot`. Every list ends with the same last row: a screenshot of
anything that looks wrong, on either device.

## Gate Table

Part of the exit contract, written before the go-ahead. One row per gate
the tracker's Exit section names. The trigger names come from `freesided
raise-item -list` on the run's build.

```markdown
| Gate | Trigger | Expected on Mac | Expected on iPhone | When |
| --- | --- | --- | --- | --- |
| <gate as the Exit section words it> | Real: <the condition the run produces> | <card, notification, or state> | <same> | Before the merge |
| <gate> | Synthetic: `freesided raise-item -db <db> -alert <name> [-run <run>]` (synthetic-trigger proof) | <card and notification> | <same> | After the merge and the completion fact |
| <gate> | None: <why no condition or alert reaches it; what would give it a trigger> | Not measurable | Not measurable | Recorded before launch |
```

The tracker comment repeats this table with a Result column.

## 1. Environment (Before Launch)

Sent after the fixture pass completes and before the real run's deputy
starts.

```markdown
**Environment check before launch.** Please do each and reply with the
result per line.

| # | Device | Action | Report back |
| --- | --- | --- | --- |
| 1 | Mac | Run `"$HOME/Applications/Freeside.app/Contents/MacOS/FreesideMac" daemon status` | Its output and exit status. Expected: exit 0, `health=reachable`. |
| 2 | Mac | Install the client built from `<sha>` | Done, and the build shown in About. |
| 3 | iPhone | Install the client built from `<sha>` | Done, and the build shown. |
| 4 | Mac | Turn off any VPN, or confirm the tailnet route to the iPhone (#1930) | Off, or the route confirmed. |
| 5 | iPhone | (after my step) Watch for a liveness-probe test notice | Arrived or not. |
| 6 | Either | Anything that looks wrong | A screenshot. |
```

Drop row 5 when no gate depends on the liveness probe. On a failed row,
SKILL.md (The Environment Checkpoint) says what to do before launch.

## 2. Pairing And Health (After Launch)

Sent once the deputy's readiness line is out. Never put the pairing code in
a shared log; send it only in this message.

```markdown
**Pair and check health.** The run's daemon is at `<api_url>`.

| # | Device | Action | Report back |
| --- | --- | --- | --- |
| 1 | Mac | Pair with the code `<code>` (expires `<time>`) | Paired, or the error. |
| 2 | iPhone | Pair with the same code, or ask me for a fresh one | Paired, or the error. |
| 3 | Both | (after my step) Watch for the test notification I send once both are paired | Arrived or not, per device. |
| 4 | Mac | Retained root only: open Inbox > System health and resolve each open item with the action its card offers | Each item's title and the action taken. |
| 5 | Mac | Client-target mode only: in the composer, enter exactly `<source text>` and submit, once; if the result is lost, tell me rather than submitting again | Submitted, or what the client showed. |
| 6 | iPhone | Sync, and open the run's task `<task title>` | It shows, and its current card. |
| 7 | Either | Anything that looks wrong | A screenshot. |
```

## 3. Specification Approval

Sent once the record shows the open `spec_approval` item.

```markdown
**Approve the specification.** Task `<task title>`.

| # | Device | Action | Report back |
| --- | --- | --- | --- |
| 1 | iPhone | Open the specification card | The actions it offers; a screenshot. |
| 2 | <device the contract names> | Approve it | Done. |
| 3 | Either | Anything that looks wrong | A screenshot. |
```

## 4. Pre-Merge Walkthrough And Merge

Sent once the record shows the run's `publication.outcome`. Every action
the contract needs before the merge goes here; the merge is last. Under the
human closure gate the pull request is a draft until the closure card is
decided, so the closure card's rows (1 and 2) come first and row 6 holds the
merge until the pull request leaves draft. Drop rows 1, 2, and 6 when the
gate is off or the source issue is not in the managed repository, since no
card opens then.

```markdown
**Walk through the ready pull request, then merge.** Task `<task title>`,
PR <link>.

| # | Device | Action | Report back |
| --- | --- | --- | --- |
| 1 | <device the contract names> | Open the source-issue closure card in the inbox (its statement starts "Merging head <head> into <base>") | Its statement and the actions it offers; a screenshot. |
| 2 | <device the contract names> | <Approve it, or the decision the contract names: Approve With Changes with the resolves flag off, or Decline> | Done. |
| 3 | Mac | Open the ready card | The actions it offers; a screenshot. |
| 4 | iPhone | Open the ready card | The actions it offers; a screenshot. |
| 5 | <device> | <each action the contract lists: a card action, an external review, a Stop check> | <what the card or forge shows after it> |
| 6 | Owner | (after my step) Hold the merge until I confirm the PR left draft with <`Closes #<number>`, or the reference the decision gives> | Nothing; I say when. |
| 7 | Mac | Look for a STOPPED banner or an open daemon stop notice in Inbox > System health | Present or not. |
| 8 | Owner | Merge PR <link> | Merged. |
| 9 | Either | Anything that looks wrong | A screenshot. |
```

## 5. Post-Merge Alerts

Sent once the record shows the completion fact and the alerts are raised.
You have already read each run-bound item's `deliveries`; the operator
reports only what each device shows.

```markdown
**Check the raised alerts.** These were raised by command to test delivery
and display; decide nothing on them.

| # | Device | Action | Report back |
| --- | --- | --- | --- |
| 1 | Mac | Find the <alert> card (item `<id>`) | Notification seen or not; what the card shows. |
| 2 | iPhone | Find the <alert> card (item `<id>`) | Notification seen or not; what the card shows. |
| 3 | Either | Anything that looks wrong | A screenshot. |
```

Repeat rows 1 and 2 per raised alert.
