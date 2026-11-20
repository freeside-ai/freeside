# A Read-Only Live View of a Running Writer Comes Forward

Plan revision 132 lets the operator watch a running stage's writer before
Phase 3, under five conditions: the view is read-only, the daemon pulls,
content is scanned before display, the view grants nothing, and clients
label it unverified and render it inertly. The owner accepts one residual:
content the credential rules miss reaches the operator's clients while the
writer runs, and content displayed before a later match can't be taken
back. Mid-turn steering, stop-and-redirect, and live streaming of
discussion replies stay in Phase 3. #1926 builds the view on tracker #3461
and chooses the mechanism against these conditions.

## What "Nothing Reads the Writer While It Runs" Protects

The daemon's `Stream` returns an empty transcript today because "the
gate's containment guarantee is exactly that nothing reads the writer
while it runs" (`daemon/internal/exec/stage/driver.go`, `Stream`, at
`69fef1a2e`). That guarantee does four things:

1. **Output leaves only from a credential-free context.** The First Ward
   Gate (plan §5.7) ends the credential-bearing context, remounts the
   workspace read-only in a fresh one, and only then exports. The process
   that copies output out can't reach the provider credential, daemon
   state, or host credentials, and the agent can't change files while
   they are copied.
2. **The transcript is out of the agent's reach by path.** While the
   writer runs, the CLI streams its transcript to a root-only directory on
   the container's own filesystem, outside every mount (`launchDir` in
   `daemon/internal/exec/claude/spec.go`). The agent reaches it only
   through the output descriptors it was started with. No root process in
   the VM parses agent output (the usage driver runs after the privilege
   drop).
3. **What crosses is bounded, scanned, and evidence only.** A failure
   transcript is copied after quiescence from a fixed, invocation-bound
   path, refusing symlinks, capped at `MaxFailureTranscriptBytes`, then
   secret-scanned with the scanner's details withheld because they may
   carry the matched credential (`daemon/internal/ward/failure_evidence.go`).
   A completed run's transcript crosses through the evidence channel under
   the whole-output export scan.
4. **The transcript decides nothing.** It is evidence, never an outcome
   signal (§5.7): the CLI's terminal event can report success alongside an
   authentication error, and only the exit status tells them apart.

What the guarantee does **not** do is keep the credential out of the
transcript's content. The launcher reads the setup token and passes it in
the CLI's environment (`CLAUDE_CODE_OAUTH_TOKEN` in `spec.go`), so the
agent and every tool it runs can print it. Against that, the retained
transcript has only the credential scan, a best-effort pattern match
(`internal/credentialpattern/`, plan §5.4). The clients already show the
retained transcript after a run (#1131).

## Whether a Display-Only, Daemon-Pulled, Scanned Read Weakens It

Each protection, tested against the conditions:

1. **Credential-free export: kept.** The view exports nothing. The
   retained transcript still crosses only through the evidence channel
   after the writer is gone. The new reader does run while the
   credential-bearing context is alive, so the condition "the daemon
   pulls" has to carry the weight the fresh context carries for export:
   the pull reads the transcript and nothing else, follows no path or
   link the agent chooses, adds nothing to the VM that reaches daemon
   state or host credentials, and returns bytes the daemon treats as
   untrusted. A route that can't show this doesn't meet the condition.
2. **Out of the agent's reach: kept.** The pull adds no mount, no writable
   path, and no outbound channel to the writer, and its egress profile is
   unchanged. The writer may be able to tell it is watched (a reader
   process inside the container is visible in its process table) and
   behave differently while watched. That reaches neither the transcript
   nor the credential, and the view was never a check on the writer's
   work; verification and the export gates are.
3. **Scanned and bounded: weakened in one case.** The view shows content
   to the same audience that already sees the retained transcript, under
   the same credential rules, before it leaves the daemon. Two things
   change. The operator sees agent text minutes or hours sooner. And the
   retained path is all or nothing: one match withholds the whole failure
   transcript (`failure_evidence.go`, where a scan refusal marks the copy
   unavailable) or refuses the whole handoff (the export scan), but a view
   can't take back what it showed before a later match.
4. **Decides nothing: kept.** The view supplies no source, verification,
   export, publication, or outcome signal. It writes nothing to the ward
   journal or the execution record, and a pull that fails or hangs never
   cancels, fails, or classifies the run, or delays the writer's stop,
   quiescence, or teardown. The plan states this last part as a condition,
   since a reader process left in the container could hold up the
   epilogue or the absence proof that `WriterComplete` needs.

So, provided the conditions hold, the view keeps protections 1, 2, and 4
and weakens 3 in one case, which the next section names.

### Cases a Reviewer Will Raise

- **The agent prints its own credential.** It can, today, into the
  retained transcript. The scan is the only barrier before display in
  both cases. A match ends the view for that invocation: the matching
  record and everything after it are never shown, and the scanner's
  details never reach a log or response. The view doesn't stop the run;
  the export scan still refuses the handoff on its own when the writer is
  gone.
- **Content shown before a later match (the accepted residual).** The
  agent prints its token base64-encoded, which no rule matches, and the
  view shows it; later it prints the token plainly, which matches. Today
  that transcript is withheld whole and nothing is shown. With the view,
  the operator has already seen the encoded copy. Ending the view on a
  match, and having clients drop what they hold of it, bounds this to
  content already displayed, but no streaming design can withdraw what a
  client displayed short of waiting for the run to end, which is what
  #1926 exists to avoid. This is a new exposure, not only an earlier one.
  It needs a form the rules miss, which is wider than encoding: a token
  with its prefix cut off, or a PEM key without its header line, matches
  no rule. The scan already misses those forms whenever no plain copy sits
  beside them.
- **Rendering could carry the residual off the device.** The agent prints
  a Markdown image whose URL carries an encoded token. A client that
  fetched it would send the token to a third party, past the writer's
  egress boundary, and a link the operator taps would do the same. So the
  plan's last condition requires inert rendering: the view loads no image
  or preview, and a link shows as plain text that can't be opened. With
  that, the residual reaches only the operator's own
  clients, the audience the retained transcript already reaches.
- **A secret split across read boundaries.** A scan of whatever bytes one
  read returned could miss a token cut in half. The scan therefore runs
  on whole records, complete JSONL lines, and holds back a trailing
  partial line until it completes. A record over a size bound, or one that
  isn't valid UTF-8, is withheld whole, never truncated and shown, since a
  shown prefix can carry most of a token without matching any rule. A
  withheld record is still scanned, and a match in it ends the view, as
  the retained transcript's whole-file scan would withhold it.
- **The match must outlive a restart.** If the daemon restarts while the
  writer runs and the pull resumes past the match, records after it would
  be shown. #1926 keeps the ended state or rescans from the start.
- **A credential the agent re-encodes, with no plain copy beside it.**
  Base64, reversal, or a token spread across two records passes pattern
  rules. That residual already
  applies to the retained transcript (§5.4 promises a best-effort scan,
  not universal detection); the view shows it sooner, to the same
  audience. Allowed by the owner's decision below.
- **The agent controls its filesystem.** The pull must not open a path the
  agent can create or replace. The transcript's directory is root-only;
  a route that reads it must not follow a link, and must not read a
  workspace path the agent can write.
- **The daemon parses untrusted bytes while the writer runs.** Reads are
  bounded per pull and per record, and the bytes are untrusted text, as
  the retained transcript's are. No root process inside the VM parses
  them.
- **The guest may be hostile.** An agent that gained root in its VM could
  feed the pull arbitrary bytes. Those are no worse than what it can
  already print to stdout, so the size bounds and the scan still apply.
  A route must also give the guest no channel back into the host beyond
  the bytes it returns; #1926 shows this for whichever route it picks.
- **The operator reads agent text sooner.** Text written to persuade the
  operator reaches them while the run is still going. The view labels
  it unverified agent output, as the retained transcript and agent
  replies already are.
- **The view stores nothing it shows.** The daemon may hold what it
  serves in memory only, dropped on a match and when the writer ends; it
  never enters SQLite, the artifact store, or the synchronized snapshots.
  Nothing reads it as evidence or as the retained transcript. Recovery,
  export, verification, and what a retained transcript holds are
  unchanged.
- **The reader shares the writer's VM.** A reader process inside the
  container counts against the VM's memory limit, so it could add to an
  out-of-memory kill, and a reader left running could hold up the
  epilogue or the absence proof. The condition that a pull never delays or
  changes the writer's stop, quiescence, teardown, or outcome rules both
  out; #1926 shows how its route meets it.

### What #1926 Chooses and Must Prove

The plan names conditions, not a mechanism. Candidate routes:

- **Read the root-only transcript inside the running container,** through
  the runtime's own command channel, copying a bounded byte range of the
  fixed file. It must show the read follows no link, runs no parser as
  root in the VM, and opens no channel back to the host beyond the
  returned bytes.
- **Another route,** such as a host-visible read of the container's
  filesystem. It must show the same properties and that it doesn't mount
  the credential-bearing filesystem anywhere the daemon would otherwise
  read untrusted paths.

Either way, the view reads only the transcript file. After a nonzero exit
the launcher's epilogue appends the CLI's stderr to it while the container
is still up (`spec.go`). That text is as agent-controlled as the rest and
goes through the same scan, so the view may show it.

Whatever the route, the view must not be served through
`StageDriver.Stream` as it stands. That method's contract says the stream
is durably recorded and replayable from the start, and the specification
engine reads the specifier's output from `Stream` first
(`readSpecificationOutput` in `daemon/internal/engine/specification.go`),
falling back to the persisted artifact only when the stream has no
result. Live bytes there would make the view a source of outcome, which
"grants nothing" forbids. #1926 uses its own read surface or removes that
read first. Which stages' writers the view covers, such as the specifier,
whose transcript carries fetched web text, is #1926's choice under the
same conditions.

The view can still show content no retained copy carries: the
retained copy is withheld whole on a match, past
`MaxFailureTranscriptBytes`, or when it isn't valid UTF-8. The match case
is the residual above; the size and encoding cases withhold a transcript
for diagnosability, not secrecy, so showing the part the view scanned
exposes nothing the scan didn't already pass.

## Owner Decision (2026-11-19)

- **Chose to bring a read-only live view of a running writer forward, under
  the five conditions,** over keeping it in Phase 3 with ACP interactive
  attachment. The owner asked for the view on 2026-10-10 during the Wave 8
  exit run (#1926): real implementations run an hour or more, and the
  clients show only that the stage is active. On 2026-11-19 the owner
  created tracker #3461 and this unit, with these conditions, and kept
  steering in Phase 3. The owner's approval of the pull request is the
  sign-off on the safety argument above, including the residual that
  content shown before a later match can't be taken back.

## Alternatives Rejected

- **Waiting for Phase 3's ACP attachment.** It bundles the view with
  steering, a write path into the running writer, which needs its own
  safety argument. The view needs none of it.
- **A writer-pushed stream.** It gives the writer a new outbound channel,
  which the egress boundary (§5.7) exists to prevent.
- **Showing only the retained transcript after the run.** That's what
  clients do today, and it doesn't meet #1926: the operator still waits an
  hour with nothing to read.

## Scope

The exception covers a running stage's writer only. Reviewers and
discussion replies are unchanged; discussion replies still arrive whole.

## Refute-First Findings

Recorded per `docs/agent-workflow.md` §refute-first, from three rounds of
fresh-context review, each asked to refute the argument against the plan
text and the daemon code at `69fef1a2e`. The first reviewer found:

- **Confirmed: per-record withholding is weaker than the retained path.**
  The retained path withholds the whole transcript on a match; the first
  draft withheld only the matching record. Fixed in part: a match now ends
  the view (plan §5.7). The rest, content shown before the match, can't be
  fixed by a live view and is the residual the owner accepts above.
- **Confirmed: a pull could hold up the writer's lifecycle.** "Grants
  nothing" covered signals, not timing; a reader left in the container
  could delay the epilogue or the absence proof. Fixed: the plan's
  condition now says a pull never delays or changes the writer's stop,
  quiescence, teardown, or outcome.
- **Confirmed: "Exactly two channels leave the agent workspace" read as
  contradicted.** It holds, because the transcript sits outside the
  workspace, but the text didn't say so. Fixed: §5.6 calls the view a
  display-only route out of the running writer that reads outside the
  workspace.
- **Confirmed: the first draft said the writer can't tell it is
  watched.** False for a route that runs a reader inside the container.
  Corrected above.
- **Allowed by decision: the view can show the scanned part of a
  transcript that retention withholds for size or invalid UTF-8.** Those
  limits serve diagnosability, not secrecy; records over the per-record
  bound or with invalid UTF-8 are still never shown.
- **Checked and holding (round 1):** the token is in the CLI's
  environment and the Anthropic token rule matches its plain form; the
  transcript directory is root-only and opened before the privilege drop;
  no other plan sentence contradicts the exception. Codex tokens match no
  rule, a gap the retained transcript shares.

A second fresh-context reviewer, given the same brief on the revised
change, found no way around the conditions and these gaps:

- **Confirmed: no condition required inert rendering.** A Markdown image
  whose URL carries an encoded token would send it to a third party if a
  client fetched it. Fixed: the last condition now requires inert
  rendering, and the residual's audience bound rests on it.
- **Confirmed: "keeps nothing" and "never kept as evidence" disagreed.**
  A buffer or synchronized snapshot could keep pre-match content and
  serve it after the match. Fixed: the plan says the view stores nothing
  it shows and clients drop what they hold on a match; the note says the
  buffer is memory-only.
- **Confirmed: "the only route by which a writer's transcript is kept"
  was too broad.** The `projects/` continuity volume keeps the provider's
  session transcript inside the ward. Fixed: the evidence channel is the
  only route by which it leaves the ward to be kept.
- **Confirmed: the residual was described too narrowly.** Any form the
  rules miss suffices, not only encoding. Fixed above, and the residual
  now has its own row in plan §14 Risks.
- **Confirmed: the epilogue appends stderr to the transcript file before
  the container stops.** First recorded as a cutoff for #1926; the third
  round revised it.
- **Confirmed: a watched writer may behave differently, and a reader
  shares the VM's memory.** The first changes no safety property;
  the second the lifecycle condition already covers. Both recorded above.

A third fresh-context reviewer, given the same brief, again found no way
around the conditions beyond the accepted residual, and these gaps:

- **Confirmed: `Stream` already feeds an outcome.** The specification
  engine reads the specifier's output from `Driver.Stream` before the
  persisted artifact. Recorded above as a constraint on #1926.
- **Confirmed: the plan's "daemon pulls" condition left out what the
  first protection depends on.** Fixed: the plan now says the pull reads
  only the transcript, follows no agent-chosen path, reaches no daemon
  state or host credential, and treats what it reads as untrusted. These
  are trust-boundary properties, so they belong in the plan.
- **Confirmed: "inert" still allowed a link the operator taps.** A link
  carrying an encoded token would leave the device on a tap. Fixed: links
  show as plain text that can't be opened, as the rendered-Markdown screen
  already neutralizes autolinks for published prose.
- **Confirmed: a withheld record never ended the view, and the ended
  state could be lost on restart.** Recorded above as constraints on
  #1926.
- **Confirmed: the stderr cutoff can't be observed reliably.** The first
  draft asked the view to stop at the CLI's exit; Apple container 1.1.0
  exposes no exit status. Dropped: stderr is scanned like the rest.

## Revisit When

#1926 picks a mechanism the conditions don't cover, such as one that needs
a channel from the writer or that keeps what it shows; Phase 3 adds
steering, which needs its own argument; or a writer runs under a credential
the rules don't match, such as a Codex token, which leaves the residual
unbounded for that writer.
