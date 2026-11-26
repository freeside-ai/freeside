# Build and Admit the Claude Agent Base From the Merged Tip

Issue #2338, one unit by owner fiat ("Don't split"). Tier A: it decides
which base every project image builds FROM, and it adds a daemon-origin
task that no device submits.

## Decision

The daemon keeps the Claude agent base current itself (plan §5.7,
Agent-base upkeep). A weekly schedule opens a fixed bump task against the
Freeside repository. After the bump merges and a daemon carrying the new
pin is installed, image upkeep builds `images/agent-claude/` at the tip,
proves the image, admits its digest, and rebuilds the project image on it.

## Why the Merged Tip Is Trusted

Review at merge is the gate on what the Containerfile runs, as it is for
every other change to the repository. The daemon adds three checks around
that gate, so the merge is the only thing that decides what is built:

- **One named source.** The instance's `agent_base` setting names the one
  repository a base may come from, by ID and slug. It is re-checked against
  the settings gate on every read, and a daemon builds only when it names
  the repository that daemon serves. A daemon without the setting builds
  nothing.
- **The daemon's own pin.** The daemon builds only a tip whose
  `CLAUDE_CODE_VERSION` equals its compiled-in `ClaudeCodeHarnessBuild`,
  and every image the Containerfile reads must be digest pinned, with no
  `# syntax` or `# escape` directive. A tip it can't run waits for a daemon update
  instead.
- **Probes on the image.** The base is admitted only after the ward's
  allowlist (local and published), `claude --version` offline, and the
  managed-settings check pass on that exact image.

## The Install Stays in the Loop

The daemon compiles in the CLI build it accepts. Adapter digests and
conformance records key on that constant, and nothing updates the daemon by
itself. So a merged bump takes effect only once the owner installs a daemon
built at or after the merge. Moving the pin out of the binary was rejected
in the issue: it would redesign adapter digests and conformance, which is
Tier A work well beyond this unit.

## Claude Only

Only the Claude base is upkept. The Codex base and the exporter stay
operator-built: neither has a bump task or a probe path here, and the
Claude base is the one every writer's project image builds FROM.

## The Daemon Reads npm Before Opening a Bump (Owner, 2026-11-26)

The issue made "vendor release polling from the daemon host" a non-goal,
and asked that a bump with nothing to change end without a PR. Freeside has
no clean ending for a task with nothing to change: the specification stage
can't decline, and a writer that changes nothing can only report blocked,
which leaves an open task and a card a person must close. Since only one
bump task may be open, a blocked one would also stop every later bump until
someone abandoned it.

The owner chose to keep a daemon-side check: each fire reads the release
npm's `latest` tag names and opens a task only when that is a plain release
newer than both the daemon's pin and the tip's. The answer is untrusted and
decides only whether a task opens, never what it asks; the writer still
looks the release up itself. Rejected:

- **Open every week and let the writer report blocked.** It honors the
  non-goal but costs a person's step most weeks.
- **Add an "already current" ending at the specification stage.** It
  honors the non-goal with no noise, but it is an engine change outside
  this unit's scope.

## Other Choices

- **Base upkeep runs inside the image-upkeep pass,** not as its own job. It
  shares the pass's checkout, a build slot, the builder VM's turn, the
  memory reservation, and the drain check. The base it admits is the one the
  same pass rebuilds the image on, and its faults join the pass's health
  item. A repository's two passes take one pointer turn for the base as for
  the image, so the second reads the base the first admitted.
- **Only the configured repository's upkeep keeps the base.** Image upkeep
  became one loop per served repository (#3210) while this unit was open,
  but the admitted base is one pointer for the daemon and the setting names
  one repository. Giving every loop a keeper would have each other
  repository's pass raise a fault for a setting that names another
  repository, so those loops get none and keep their images on the base
  they have. The bump task goes to the configured repository's
  image-repair project and compares that loop's tip read. Production serves
  one repository, so this changes nothing there today.
- **A failed build the source caused is remembered for its tree and pin
  until restart,** so a broken source doesn't rebuild on every scheduled
  pass. A new source or a new daemon tries again. A host fault or the time
  bound isn't remembered and retries at the next pass, as a project
  rebuild's host fault does, since the same source would likely build.
- **The base publishes beside the pointed-to project image,** in that
  image's registry. With no admitted project image there is nowhere to
  publish, so nothing builds.
- **The record has no probe-evidence digest.** The probes are pass or fail,
  and the record's ID addresses every fact but the admission time. That
  includes the source tree, so an edited row fails on decode.
- **A daemon-origin submission's source kind comes from its command ID's
  namespace,** not a stored kind column. The store gate dispatches on the
  prefix: an image-upkeep ID needs a recorded repair refusal, and a bump ID
  needs a recorded `agent_base_bump` occurrence and, when written, the
  fixed text. Existing rows already carry the image-upkeep prefix, so no
  stored data changes.
- **The bump schedule is armed whenever a project takes manual
  submissions,** and the setting is read at each fire, so setting or
  clearing it takes effect without a restart. The kind is always
  registered, because a schedule armed by an earlier start persists.
- **A project whose writers run warded gets a warning log, not a health
  item.** A warded writer can't reach npm, and that is a standing
  configuration, not a fault to clear.

## Refute-First Review

Two rounds of fresh-context review tried to prove the change wrong before
push; the second reviewed the first round's fixes.

- **Confirmed, fixed: the bump text tied stored rows to the build.** The
  store gate re-checked a stored bump's text digest against this build's
  text on every decode, so rewording the text would have made every
  recorded bump undecodable and failed the pre-update database check. The
  text is now checked when a row is written; decode re-checks only the
  occurrence, and a replay compares the request, not the text.
- **Confirmed, fixed: one image admitted from two trees.** A change the
  build doesn't read, such as the directory's README, moves the tree but
  rebuilds the same image, and `image_ref` was unique, so every pass
  rebuilt and failed. `image_ref` is no longer unique: the image is
  admitted again from the new tree, and a lookup by image reads the latest
  record.
- **Confirmed, fixed: a refused setting stopped image upkeep.** It now
  faults only the base, and the project image is kept on the base it has.
- **Confirmed, fixed: images outside `FROM`.** A `# syntax` frontend,
  `COPY`/`ADD --from`, and `RUN --mount` from also pull images, so the pin
  check now covers them. Review at merge stays the gate on what the
  Containerfile runs.
- **Confirmed, fixed: a stored pointer outlived its setting.** Upkeep
  moved every image onto the admitted base even after the operator cleared
  or retargeted `agent_base`. The pointer is now re-gated on each pass: it
  applies only while the setting names the repository the base was built
  from.
- **Confirmed, fixed: continued lines skipped the pin check.** A `--from`
  or `--mount` flag on a `\`-continued line wasn't read with its
  instruction. Continued lines are now joined first, and a `# escape`
  directive, which changes the continuation character, is refused.
- **Confirmed, fixed: a second bump before the tip is read.** The bump
  compared npm only with the daemon's pin when upkeep hadn't read the tip,
  so a merged bump could open another before the install. A fire now opens
  nothing until upkeep has read a plain release at the tip.
- **Allowed by decision: `internal/signet/task_submission.go` stays off
  the Tier A list.** Its bump submission passes the store gate, which is
  listed and re-checks the occurrence and the text. The engine submitter,
  which decides which daemon task skips the writer-image check, joins the
  list.
- **Allowed by decision: the slug comparison is case-sensitive.** A slug
  cased differently from the daemon's fails closed with a fault naming
  both.
- **Allowed by decision: a settled refusal skips the base.** While a
  refusal stands at the tip, a scheduled pass reuses it without a
  checkout, so the base isn't evaluated. No base could reach the image
  until the refusal clears, and only a tip move clears it, which runs the
  full pass.
- **Allowed by decision: the bump reads the last tip upkeep read.** A fire
  between a bump's merge and the pass its tip move starts can open one
  more task, which finds the pin current and ends without a PR.

## Revisit When

- A task can end at specification with nothing to change: then drop the
  daemon's npm read and let the writer decide.
- The Codex base needs the same upkeep.
- The CLI pin moves out of the daemon binary.
