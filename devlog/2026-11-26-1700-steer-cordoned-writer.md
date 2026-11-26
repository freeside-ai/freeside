# An Operator's Message Reaches a Running Cordoned Writer

An operator can now send a running cordoned writer a message (#3632). The
cordon streams the writer's input, a message waits in the run's `ctl/`
inbox until the supervisor takes it or the daemon withdraws it, and the
daemon records each message before it offers it and settles it once. A
paired client sends one with the `send_writer_message` command, and the
shell with `freesided message`. Tier A by path: `internal/cordon/`,
`internal/cordonhost/`, `internal/exec/stage/cordon.go`,
`internal/daemoncompose/cordon_gate.go`, `internal/signet/http.go`, and
`internal/signet/invocation_transcript.go`. Migration 0151 only adds a
table, so it is Tier B.

## The Owner Kept the Unit Whole

The plan for #3632 estimated about three times the size budget and
proposed splitting at the gate: the path to the run in this unit, and the
record, command, shell verb, restart settle, and API in a new
`kind:contract` unit after it. The owner chose to keep #3632 whole as one
unit and not file the second ("Keep whole", 2026-11-26, in the
orchestrating session). No further reason was given.

## What the Pinned CLI Does With a Second Message

Measured by hand against the host agent CLI 2.1.332, the one the cordon's
`AgentCLISHA256` pins on this host, with `--input-format stream-json` and
the prompt sent as one `user` frame:

- **A message sent while a turn runs a tool call joins that turn.** The
  turn's reply answered both messages, and one `result` record followed,
  not one per message.
- **After a `result`, the CLI idles with its input open.** It stayed alive
  for over two minutes with no further output. A run whose input never
  closes would stall until the stall limit failed it.
- **A message written just before the input closes still runs.** Sent
  after a `result` and followed at once by end of input, it ran as its own
  turn, with its own `result`, and the CLI exited 0.
- **Closing the input mid-turn ends nothing early.** The running turn
  finished, emitted its `result`, and the CLI exited 0.
- **The exit status follows the last turn.** A turn that ended in an
  error `result` and then closed exited 1; the same turn followed by a
  second message that succeeded exited 0.
- **Each `result` reports its own turn's tokens and the session's
  cost.** Over two turns, `usage` held each turn's counts, while
  `total_cost_usd` grew to the running total.

Chose closing the input on a `result` with nothing waiting over counting
one `result` per frame written, because the CLI folds a mid-turn message
into the running turn: counting would wait for a `result` that never
comes. The supervisor writes `ctl/input-closed.json` before it closes, so
the daemon refuses later messages as `input_closed` rather than leaving
them waiting. A message the supervisor took just before the close still
runs, by the third finding.

The scan that sees a turn end reads stdout only to decide when the input
closes. A forged or early `result` closes the input early and does
nothing else: the run still ends by the CLI's exit status.

Chose letting that exit status stand over failing a run on any error
`result`. An operator's message after a failed turn asks the writer to
go on, and the writer launch sets no turn or budget limit for a second
turn to escape; what it produces still passes the export scan, import,
and verification like any run's. Usage extraction kept only the last
`result`, which would under-count a run of more than one turn, so it
now sums the token counts and keeps the latest cost.

## The Cordon Fixes the Input Format, Not the Spec

Chose adding `--input-format stream-json` to the cordon's fixed launch
arguments, and refusing a caller argument that sets it, over adding it in
`internal/exec/claude/spec.go`. The supervisor now owns the agent's input
and writes the prompt as the first frame, so the format is the cordon's
contract with its own supervisor, not the caller's choice. Fixing it
there also puts it in the cordon's configuration digest, so the
conformance suite must pass again before a launch runs unattended. A
warded writer keeps plain stdin and refuses a message as `not_streamed`.

The host launcher (#3688) landed while this unit was open. Its client
stands in for the backend behind the same port, but its protocol carries
no message. Chose refusing a message there as `not_streamed`, without
dialing, over adding the protocol frame and its error codes here: nothing
composes the client yet, the client already refuses exports the same way
until #3624, and the unit was already about three times its size budget.
#3702 carries messages over the protocol.

## The Message Row Is the Command's Only Record

Chose an `operator_messages` table keyed by `command_id` over a new arm in
the command receipt. A receipt is immutable, but a message moves from
`recorded` to `delivered` or `refused` after the command's transaction
commits, because the offer runs outside any transaction (the run's answer
can take seconds). A receipt arm would either repeat the row's facts or
freeze a state that changes, against the rule that a new table stores
each fact once (`devlog/2026-11-25-2231-store-once-new-tables.md`). A
replay of the `command_id` reads the row as it stands; the command-ID
uniqueness check covers the table, so no other kind can reuse its id.

## The Text Bound Is 8 KiB

The plan proposed 16 KiB. Chose 8 KiB because a message file is a cordon
record, which `readRecord` bounds, and JSON can write one byte of text as
six (`\u003c` for `<`), so the text must stay well under a sixth of the
record bound. Raising the text bound means raising the record bound for
every `ctl/` record. The record and the cordon share one constant
(`domain.MaxOperatorMessageBytes`), so a message the record accepts is
never refused for its size at the run, and a test round-trips the longest
worst-case message through the inbox. The record also carries the
client's `command_id`, so it takes the 256-byte bound the discuss
commands use; a fresh-context review found it unbounded before push,
where a long one would have kept a message from ever settling. Revisit
when operators hit the text bound.

## What Delivered Means

`delivered` means the run's supervisor took the message for the writer's
input, by renaming its inbox file to `.taken`. The daemon withdraws a
message by renaming the same file to `.refused`, so exactly one rename
wins and a message reaches the writer at most once. It is at most once,
not exactly once, and it never means the writer read the message. A
writer that exits before reading its input never sees a message taken in
the meantime; the feed keeps taking until the supervisor sees the exit.
A supervisor that dies between the take and the write loses the message,
and that run ends lost. Chose saying so over refusing a take once the
agent's input copy fails, because the daemon may already have read the
take as delivered, and the narrower window would still leave a crash
just after the write unread.

The daemon reads a message's state from the waiting file first. A
message only ever moves out of waiting, so a take that lands between two
reads can't read as no file, which would have the daemon write the
message again on a restart's offer. A fresh-context review found that
order bug before push.

A restart offers each message still `recorded` again under its own
ordinal, before the command surface serves. An ordinal offered before
settles from its files, so the run never writes a message twice. A run
whose files are gone by then settles `delivery_unknown`, because an
earlier offer may have been taken before the release; on the live command
path the same answer is `run_ended`, since the message was never offered
before.

## Refusals Before and After the Record

Bad text, a daemon with no driver, and an invocation that isn't a
cordoned writer of the run are refused before anything is recorded: the
daemon can decide them alone. A warded writer falls in that last group,
by its admission's backend. Everything the run decides settles the record
`refused` with a reason. `not_started` is a reason the plan didn't list:
a writer whose launch hasn't claimed its run directory yet (seeding, or
running its pre-job probe) can't take a message, and saying so lets the
operator send it again in a moment, which `run_ended` would not.

Revisit when the agent CLI changes how it treats a second message on its
input stream; the cordon's pin by digest makes any CLI update re-run the
suite, but the suite's fake agent doesn't reproduce the fold.
