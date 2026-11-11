# Checkpoint Lists

Templates for the exit run's gate table and its operator checkpoints: five
on every run, and a sixth when a seeded pass has a row that needs a device.
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
the tracker's Exit section names, and per Acceptance line of the
exit-proof unit that no Exit bullet covers. The trigger names come from
`freesided raise-item -list` on the run's build.

```markdown
| Gate | Trigger | Expected on Mac | Expected on iPhone | When |
| --- | --- | --- | --- | --- |
| <gate as the Exit section words it> | Real: <the condition the run produces> | <the card and where it shows, or the state> | <same> | Before the merge |
| <gate> | Synthetic: `freesided raise-item -db <db> -alert <name> [-run <run>]` (synthetic-trigger proof) | <the card and where it shows> | <same> | After the merge and the completion fact |
| <gate> | Seeded: <the mechanism the gate names and its command> (seeded-condition proof) | <the card and where it shows, the state, or "record only"> | <same> | On its seeded pass, or the rehearsal when the seed rides it |
| <gate> | None: <why no condition or alert reaches it; what would give it a trigger> | Not measurable | Not measurable | Recorded before launch |
```

Where a card shows: a decision card is in the inbox, marked unviewed until
someone opens it on either device; a system-health finding (`stall`,
`held-work`, a daemon stop) is in Settings > System Health. Write "and a
notification" into an Expected cell only when the gate names one, and only
for the iPhone: device notifications are off until the device's switch is
turned on, and nothing on the Mac listens (SKILL.md, Derive the Exit
Contract).

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
the walkthrough says how to fix it (docs/production-walkthrough.md,
Prepare The Operator's Devices), and SKILL.md (The Environment Checkpoint)
says which failures change the run.

## 2. Pairing And Health (After Launch)

Sent once the deputy's readiness line is out. Never put the pairing code in
a shared log; send it only in this message.

```markdown
**Pair and check health.** The run's daemon is at `<api_url>`.

| # | Device | Action | Report back |
| --- | --- | --- | --- |
| 1 | Mac | Pair with the code `<code>` (expires `<time>`) | Paired, or the error. |
| 2 | iPhone | Pair with the same code, or ask me for a fresh one | Paired, or the error. |
| 3 | iPhone | Turn this device's notifications on (Settings > Notifications), then follow the steps shown there: in the ntfy app, subscribe to the topic **Copy Topic** copies | On and subscribed. |
| 4 | iPhone | (after my step) Watch for the test notification I send once row 3 is done | Arrived or not. |
| 5 | Mac | Retained root only: open Settings > System Health and resolve each open finding with the action its card offers | Each finding's title and the action taken. |
| 6 | Mac | Client-target mode only: in the composer, enter exactly `<source text>` and submit, once; if the result is lost, tell me rather than submitting again | Submitted, or what the client showed. |
| 7 | iPhone | Sync, and open the run's task `<task title>` | It shows, and its current card. |
| 8 | Either | Anything that looks wrong | A screenshot. |
```

Drop rows 3 and 4 unless the gate table has a row that expects a
notification. A phone paired to a fresh root has a topic it has never
subscribed to, and the client shows **Copy Topic** only once the switch is
on, so without row 3 a test notification has nowhere to arrive. A card's
notification reaches the phone only for items opened after the switch went
on, so row 3 comes before the first card the run needs notified.

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
| 7 | Mac | Look for the Stopped notice above the list, or an open daemon stop finding in Settings > System Health | Present or not. |
| 8 | Owner | Merge PR <link> | Merged. |
| 9 | Either | Anything that looks wrong | A screenshot. |
```

## 5. Post-Merge Alerts

Sent once the record shows the completion fact and the alerts are raised.
You have already read each item in the record; the operator reports only
what each device shows.

```markdown
**Check the raised alerts.** These were raised by command to test that
each reaches the clients and how it displays; decide nothing on them.

| # | Device | Action | Report back |
| --- | --- | --- | --- |
| 1 | Mac | Find the <alert> card (item `<id>`) in <Settings > System Health, or the inbox> | Where it shows and what the card says. |
| 2 | iPhone | Find the <alert> card (item `<id>`) in <Settings > System Health, or the inbox> | Where it shows and what the card says. |
| 3 | Either | Anything that looks wrong | A screenshot. |
```

Repeat rows 1 and 2 per raised alert. `stall` and `held-work` raise
system-health findings, which show in Settings > System Health;
`review-growth` raises a review card in the inbox. Add "notification seen
or not" to the iPhone row's report only when the gate table expects one.

## 6. A Seeded Pass (When A Row Needs A Device)

Sent only when a seeded-condition row's Expected cells name something a
device shows, and only after the rehearsal has completed and list 1 has
passed. The clients pair to the seeded pass's daemon, so send its pairing
rows first (list 2, rows 1 and 2) when it isn't the daemon they are
already paired to. A row whose Expected cells say "record only" needs no
list.

```markdown
**Check the seeded <condition>.** This condition is seeded to test what
the daemon does once it holds. Task `<task title>`.

| # | Device | Action | Report back |
| --- | --- | --- | --- |
| 1 | <device the contract names> | Scenario pass only: approve the specification card, then keep the task open on both devices | Approved. |
| 2 | Mac | Open the task | The state it shows; a screenshot. |
| 3 | iPhone | Open the task | The state it shows; a screenshot. |
| 4 | <device the contract names> | <the action the gate names, such as Retry With Another Agent on a held run> | <what the card or task shows after it> |
| 5 | Either | Anything that looks wrong | A screenshot. |
```

Drop row 1 on a deputy pass and row 4 when the gate names no action. On
the `quota-park` scenario the park lasts two minutes from the refusal, so
send this list before the approval: the operator's approval starts the
attempt the provider refuses, and rows 2 and 3 are read while the task is
parked. The park allocates the next attempt as a new run; `freesided
follow -run <that run> -until park-ended -timeout <duration>
-approved-recipe <digest>` waits for its
admission, which ends the park. On the refused run it holds at once.
