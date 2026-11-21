# The Live Transcript Reads Through the Runtime's Exec, Scanned in the Stage Driver

#1926 builds plan §5.7's live view of a running writer. Chose a root
`container exec` that copies a byte range of the fixed transcript file over
a host-visible read of the container's filesystem, because the exec reaches
only the one file the launcher made root-only and opens no channel the
writer can use. The stage driver scans every byte with the retained
transcript's credential rules before any leaves the daemon, and the API
serves it only for implementation writers, remediations included. This
note records why, how each of the five conditions holds, and what the
owner should know about the choices made without them.

Prior context: `devlog/2026-11-19-1526-live-writer-view.md` (revision 132
and its "What #1926 Chooses and Must Prove").

## The Route

The ward runs `container exec --user 0:0 <id> sh -c <fixed script>` with
the path and the byte range as arguments. The script refuses a symlink at
the path (exit 3), answers empty when the file doesn't exist yet, refuses
anything but a regular file (exit 4), and otherwise runs
`tail -c +<offset> | head -c <limit>`. stderr is discarded and the host
caps what it reads. So:

- **It follows no path the agent chooses.** The path is a constant pinned
  to the launcher's `launchTranscriptPath` by a test in
  `internal/exec/claude`. The launcher creates its directory root-owned
  and 0700 before it drops privilege, so the agent can't create or
  replace an entry there. The symlink and regular-file checks are a
  second line, not the guarantee.
- **It runs no parser as root.** `tail` and `head` copy bytes; nothing in
  the container interprets them.
- **The writer gains no channel.** The exec is the runtime's own command
  channel, driven from the host. The writer can't open it, write to it, or
  see it.
- **It reaches no daemon state or host credential.** The bytes go to the
  stage driver's scanner and nowhere else.

Rejected: a host-visible read of the container's filesystem. The
transcript sits on the container's own root filesystem, outside every
mount, so reading it from the host means exposing that credential-bearing
filesystem where the daemon reads, the risk the prior note named.

Rejected: serving through `StageDriver.Stream`. Its contract says the
stream is durable and replayable, and the specification engine reads
outcome from it, so live bytes there would become an outcome source. The
live read is its own method on the driver.

## Which Writer a Read Reaches

The ward keeps an in-memory registry of running writers, keyed by run. A
handoff registers its writer after `StartContainer` succeeds and removes it
as soon as the writer wait returns. Every read checks the registration,
then runs a fresh `Inspect` and requires this handoff's ownership evidence
(`classifyEvidence`) and a running container, then runs the exec under a
5-second bound merged with the registration's context. Removal only
deletes the entry and cancels that context; it never waits for a read.

The registry needs no persistence. A restart's recovery tears down a
writer that hadn't completed, so no live writer outlives the process that
registered it.

## Grants Nothing, Proven Live

A read never touches the journal, the writer wait, teardown, or the
absence proof. The live tests stopped a writer while an exec was wedged on
it: on alpine the stop took 147 ms against 192 ms with no exec in flight,
and on the agent image 136 ms against 200 ms; the wedged exec died with
exit 137 before the stop returned. The read doesn't delay the stop.

The stage driver's tailer exists only around `gate.Handoff`: opened before
it and closed when it returns. The signet answer prefers a kept claim
whenever one exists, so once the writer's transcript is kept, the kept
copy is all a client can read.

## Scanned Before Display

The tailer starts a pull at most once every 2 seconds, one at a time,
from the offset it holds, and only when a client asks. A pull reads at
most 1 MiB; while pulls fill that cap it pulls again, up to 8 MiB, so a
view opened late on a long run reaches the writer's present in a few
reads. It scans every pulled byte with the retained
transcript's rules (`credentialpattern.Match`), carrying the last 4 KiB
across pulls, as the export scan does, so a token split across two pulls
or two records is still caught. It scans before it splits lines, and it
serves only complete records. A match latches the invocation withheld for
the rest of the handoff, and every client drops what it holds on the next
answer that isn't live.

A record over 1 MiB, or one that isn't valid UTF-8, is withheld whole and
counted; it is still scanned. The tailer keeps a window of 8 MiB or 65,536
records and trims from the front; a client that falls behind gets the
window with `omitted_before` and says earlier activity isn't shown. An
`after` that isn't 0 or a record boundary in or before the window is a
400, so a client can't read from inside a record.

## Which Invocations Are Live

Chose implementation writers only, remediations included, over every
stage's writer. The API serves live only for an attempt of a stage whose
canonical role is implementation. A remediation qualifies because the
engine records it at admission as an attempt of an `implement` stage.
The specifier is left out: its transcript carries fetched web text, the
case the prior note flagged, and its output is a document the operator
reads once it's kept. Reviewers and discussion replies stay unread while
they run, as the plan says. Revisit when the specifier's runs grow long
enough that watching one matters.

## Client Choices

- **No link before the first observation.** The design's open question
  recommended placing the link under `Invocation Started` until the
  invocation's observations exist. Not done: the engine writes the first
  observation within one reconcile pass of the driver reporting the
  writer, and the daemon has nothing to serve live before the writer
  runs, so the link would open onto an empty sheet for at most that gap.
- **Polling every 3 seconds** while the sheet is open, from the held
  offset, matching the daemon's 2-second pull without asking more often
  than it can answer.
- **Nothing is stored.** The client holds at most 8 MiB in memory and
  drops it on any answer that isn't live, before it shows anything else.
  The running sheet's open folds live in memory too: their keys are
  positions in a stream that grows and trims.
- **It says none was kept only after the writer ends.** An answer with no
  transcript while the writer runs, or shortly after it ends, says the
  sheet is waiting; the run page can see the writer end before the daemon
  records the kept copy. After ten such answers from an ended writer, the
  sheet says none was kept and stops reading.

## Refute-First Findings

From the build's own checks and one fresh-context reviewer asked to
refute the change:

- **Confirmed, fixed: a token across a record boundary.** Scanning per
  record would miss a key split across two lines (a GCP `private_key_id`
  record followed by its key). The scan runs over the byte stream before
  the split, with a test.
- **Confirmed, fixed: oversize and invalid records.** Skipping them
  unscanned would let a token through the carry. They are scanned, then
  withheld.
- **Confirmed, fixed: a late view crept forward.** One 1 MiB pull per
  client read left a view opened 40 minutes in showing the run's start for
  a minute and more. The catch-up pulls above fix it.
- **Confirmed, fixed: the client.** It polled forever, said "No transcript
  was kept" in the normal gap before the kept copy was recorded, and saved
  positional fold keys to the device.
- **Allowed: a preparation command can write the transcript path first.**
  The writer is registered when its container starts, but the launcher
  creates the root-only directory only after the repository's preparation
  command has run as root. A preparation command that writes that exact
  path gets its bytes scanned and shown, and the real transcript then reads
  from the wrong offset. Allowed because the view is display-only, every
  byte shown is scanned, and repository config can already shape what the
  agent writes into its transcript. The guarantee that the agent can't
  place an entry there belongs to the launcher, not the read.
- **Disproved: a restarted handoff stitches two transcripts.** A second
  handoff for one invocation in one process would restart offsets at 0
  under a client's held offset. The pipeline opens the tailer only on the
  path that advances to the running phase, and a resumed running phase
  goes through ward recovery, never a new handoff
  (`daemon/internal/exec/stage/driver.go`).
- **Confirmed, fixed: the records description.** A failed writer's launcher appends
  plain-text stderr to the same file, so a record isn't always JSON. The
  API description says so; the client already counts unreadable lines.

## One Pull Request

The issue proposed splitting the daemon read from the app. The owner
didn't apply the split, so this lands as one contract unit across
`daemon/`, `api/`, and `app/`, under the one-work-unit rule for a spec
change and its consumers.
