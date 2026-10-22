# Production Walkthrough And Recovery

A real-work run is `freesided deputy -root <root>`: one process that takes the
production rig lease, adopts the agent tree, runs the composition preflight,
and serves the run's daemon until you stop it. Its settings, start-up order,
and what it writes are in the daemon guide,
[Run Real Work With The Deputy](../daemon/README.md#run-real-work-with-the-deputy).
This runbook covers operating one: starting it, submitting work, reading the
proof, ending it, and recovering when something goes wrong.

The deputy has no automatic deadline and reads no stdin. It keeps serving the
same database, state root, and listener to the paired Mac and iPhone after
publication, until you end it ([Complete Deliberately](#complete-deliberately)).
Closing its shell is an interruption, not a completion.

For a task-specific Stop exercise, follow the
[controlled cancellation check](task-cancellation-check.md). It records runtime
evidence separately from the walkthrough's publication and shutdown checks.

## Start And Verify

Write `<root>/daemon/settings.json`, mode `0600`, with every key the daemon
guide's settings table lists. Every path in it is absolute and clean, and JSON
expands neither `~` nor `$HOME`, so write each one out in full; the deputy
refuses a relative path before it takes the lease. The three trusted prompt
packages ship in the repository, so no operator authors one: in a checkout of
the approved default-branch commit, set `prompts.prompt_package` to
`<checkout>/prompts/phase-1a/implementer.md`,
`prompts.specification_prompt_package` to
`<checkout>/prompts/phase-1a/specifier.md`, and
`prompts.remediation_prompt_package` to
`<checkout>/prompts/phase-1a/remediator.md`.
The daemon admits the content digest of these exact bytes; a workspace copy is
never prompt authority.

The production daemon keeps running during the run (#2080). It must run
without a driver, which the installed LaunchAgent does: it passes no
`-driver`. A driverless `prod` never opens the App authority's files, so the
run stays their only writer
([plan §10](plan.md#environments-prod-dev-and-ephemeral)). Both sides check
this. Preflight's `prod_execution` check reads `prod`'s execution mode over
its control socket and fails when `prod` reports a driver or advertises a
socket that does not report its mode, so the installed app must include
#2080; an older installed build fails the check. A stopped `prod` passes it,
so confirm `prod` is running before the start:
`"$HOME/Applications/Freeside.app/Contents/MacOS/FreesideMac" daemon status`
must exit 0 and print `health=reachable` (app/README.md, Control The
Supervised Daemon From A Command). And while a production rig lease exists,
live or stale, `freesided -environment prod` refuses `-driver fake`,
`-driver claude`, and `-fake-publication`. Choosing **Stop** or **Start**
mid-run changes nothing for the run, and the deputy never stops or restarts
`prod`.

The run uses `prod`'s own App authority, where the App's single authority
lives ([plan §10](plan.md#environments-prod-dev-and-ephemeral)). Set
`prod.app_state_dir` to `<home>/Library/Application Support/Freeside/daemon`
and `prod.app_credentials_dir` to
`<home>/Library/Application Support/Freeside/credentials`, where `<home>` is
your home directory written out; the deputy refuses any other directory,
including a copy. Under the lease it asks the installed `freesided`
(`prod.installed_daemon`, normally
`<home>/Applications/Freeside.app/Contents/Resources/freesided`) which App
authority formats it reads, and refuses to start if that build predates #1583
or can't read what this build writes. Install the current app before the
first run.

Once, before the first run after #1583, move the App authority that earlier
runs kept in their own directories into `prod`'s, with **Stop** chosen and no
run active. Pass the old directories:

```sh
bash scripts/move-app-authority-to-prod.sh <old-app-state-dir> <old-app-credentials-dir>
```

The script moves both or neither, and stops without
changing anything when `prod` already holds App state or credentials, when a
rig lease exists, when `prod` is running, or when a source holds a file it
doesn't recognize or no App registration. It never
merges two authorities: resolve such a stop by hand. It keeps each source,
renamed to `<dir>.moved-to-prod-<timestamp>`, so a stale setting fails loudly.
It never deletes them; remove them yourself once a run has published through
`prod`'s directories.

Build `freesided` from a clean checkout of the reviewed commit; the
composition preflight refuses a `-dirty` build. Then start the deputy and keep
its shell open:

```sh
freesided deputy -root <root>
```

It prints its build, the settings digest, and the repository it publishes to
on stderr, then the self-check's result: `composition passed; manifest <path>`,
or the failed checks and their remediation with execution left disabled. Once
the daemon serves, its readiness line on stdout gives the `api_url` and a
pairing code. Everything under `<root>/daemon` is private: the database, the
adoption report and log, and the agent-tree patch. Do not upload it.

Submit the task from a second terminal. `--require-composition` binds it to
this launch's passing manifest, `<root>/daemon/composition-manifest.json`:

```sh
freesided submit -db <root>/daemon/freeside.db --require-composition \
  -project <project> -task /absolute/spec.md -policy /absolute/policy.json \
  -publication /absolute/publication.json -work-unit /absolute/work-unit.json
```

Each submission saves its identity and exact input bytes before it is sent.
Identical input in another submission creates separate work. If submit's
output is lost, its private journal is under
`<database>.submissions/<submission-id>.json`; resolve it with
`freesided submit --db <database> --retry-submission-id <submission-id>`.
This loads the saved inputs and never creates a replacement identity. Neither
the CLI nor the clients automatically resend unresolved submissions. A second
unfinished task for the same specification in the same project is refused;
the refusal names the first task and the commands that settle it.

If pairing has not finished before the startup code expires, request a fresh
code as the deputy's OS user:

```sh
freesided pairing-code -state-dir <root>/daemon
```

Use the returned `api_url`, not the normal installed service's endpoint. The
command prints a private code and its ten-minute expiry. Keep that output out
of shared logs and evidence; record a redacted mint/preview receipt and
successful client pairing instead. Renewal does not consume or create a
workflow attempt, restart the deputy, or authorize another submission. Any
campaign-specific minting budget still applies.

A standing root keeps the system-health items its earlier runs left open, and
a restart does not close them. The blocking ones hold the next run's first
admission with reason `blocking_system_health` until an operator resolves
them in a paired client (Inbox > System health; Resume Unattended closed a
durable-stop notice on run 117). Advisory items never hold admission. Resolve
them once the clients are paired and before the first approval. A blocking
item that no offered action closes keeps holding admission; stop the run and
report it rather than going on.

**The task record is the proof of a run.** Read it while the deputy serves:

```sh
freesided inspect -db <root>/daemon/freeside.db
freesided inspect task -db <root>/daemon/freeside.db -task <task-id>
```

The first lists the tasks the daemon holds; the second prints one task's
timeline, runs, attention items, and publication outcome as versioned JSON
([Read A Task's Record](../daemon/README.md#read-a-tasks-record)). A published
run's outcome names the pull request of exactly the exported head. Each
attention item carries its per-device `deliveries`, so the record also shows
whether a card's notification reached each paired client. For one run's
derived supervision state, read
`freesided follow -db <root>/daemon/freeside.db -run <run-id> -snapshot`.

The record proves a run only when it holds together. `inspect task` exits 0
when a section or item fails its store check, so read the fields, not the
exit status. For the published run, check that:

- **Every read succeeded:** `task_error`, `timeline_error`, and `items_error`
  are `null`, no item carries an `error`, and the run's `errors` is empty.
- **The evidence holds:** `publication.evidence_snapshot_present` and
  `publication.outcome.evidence_eligible` are both `true`.
- **The heads agree:** `publication.export_head_sha` equals
  `publication.outcome.head_sha`.
- **The forge agrees:** `gh pr view <outcome.pr_number> --repo <outcome.repo>
  --json headRefOid,baseRefName` reports that head and `outcome.base_ref`.

A record that fails any check is a finding, not proof of the run. During the
walkthrough, fetch the run on both paired clients and exercise its offered
ready-card actions. Record which actions ran, the task and run IDs, the
endpoint, and the published head the record shows. Later actions can change
the run, so an earlier read does not prove a later head. Health from the
default service on port 7331 does not prove this run is still available at
its paired endpoint.

## Rehearse Before The Real Run

An exit run starts with a rehearsal pass: the same build runs the whole
pipeline once, unattended, on a disposable target, so a launch, input,
credential, or publication problem shows up before the operator is involved.
The exit-run skill (`.claude/skills/exit-run/SKILL.md`, Rehearse Before The
Operator) says when a pass counts; this section is the procedure.

**Root and build.** Use a fresh root and a `freesided` built from a clean
checkout of the SHA the real run will use. A real run on another build needs
a new pass.

**Settings.** Copy the real run's settings file and change only these keys:

| Key | Rehearsal value |
| --- | --- |
| `listen` | A listener of its own |
| `seed_root` | A directory of its own for the daemon-owned base checkouts |
| `repository.*` | The disposable repository: its `repo`, `repository_id`, `base_ref`, `base_sha`, `allowed_paths`, `checkout`, and `project` |
| `manual_submission_config` | `""` |
| `review.model`, `review.effort` | The lowest-cost model the Codex subscription admits, at `low` |
| `judgment.model` | When the real run sets `judgment`: the lowest-cost Claude model the pinned `judgment.claude_bin` serves |
| `agent_tree.commit` | The rehearsal tree's commit (below) |

The review and judgment models are part of the emitted agent tree, so the
rehearsal needs its own agent-tree commit. Adopt it once through
[The First Start](#the-first-start) and keep it, for example on its own branch
of the same agent-tree checkout; later rehearsals reuse it. The writer,
specifier, and remediator run the pinned default route model, which no setting
changes.

**Disposable target.** An issue in a dedicated throwaway repository where the
`prod` GitHub App is installed, as the `freeside-review-challenge` repository
was for #1275. The images and approved recipe stay as the real run's, so the
repository must be one they serve. Keep the issue small enough for a clean
review: a review finding or a closure gate opens a card only a person can
answer, and that stops the pass.

**Inputs.** Build the `submit` inputs as the real run will, naming the
disposable issue and project instead of the target. The policy file sets
`gates.spec_approval` to `false` and leaves `gates.source_issue_closure`
absent or `false`, so neither a specification nor a closure card waits for a
person.

**Run it.** Start the deputy and submit as in [Start And Verify](#start-and-verify),
on the rehearsal root. Follow the task with `freesided inspect task` until the
run's `publication.outcome` is set. Merge the rehearsal pull request in the
disposable repository, wait for the run's `publication.completion`, and raise
the alerts the exit contract triggers synthetically with
`freesided raise-item` (exit-run skill, Walk Through on Both Clients).
With no paired client on the rehearsal root, the record shows each run-bound
item but no `deliveries`: the pass proves the daemon side only.

**End it before the real run.** Apply the four record checks above, then end
the deputy with Ctrl-C ([Complete Deliberately](#complete-deliberately)). It
must exit 0. A nonzero exit, or a line saying it left the rig lease for the
next start to recover, means the lease was abandoned; `rig-acquisition.json`
is removed either way, so its absence proves nothing. Recover an abandoned
lease with `freesided rig recover -state-root <rehearsal-root>/daemon -confirm`
before the real run, which needs the rig lease and starts on its own root.

## Enroll The Identities And Check The Agent Tree

The daemon selects every identity through the admitted-agent tree
([Select Agents Through The Lineup](../daemon/README.md#select-agents-through-the-lineup)),
and a fresh state root holds no enrollment for the tree to name. The deputy
closes that gap on every start, after it takes the lease and before the
composition preflight:

1. **Record.** It records the two identities `agent_tree.auth_identity` and
   `agent_tree.review_auth_identity` name, once. A later settings file that
   names other ones is refused.
2. **Adopt.** It runs `freesided auth adopt` on that root, writing
   `<root>/daemon/auth-adopt.json` and `auth-adopt.log`. A root that was
   already adopted changes nothing.
3. **Check.** It compares the tree adoption emitted,
   `<root>/daemon/agent-tree.patch`, with the tree at `agent_tree.commit` in
   `agent_tree.checkout`: the mode and content of every file under
   `policy/agents` and `policy/fragments`, and of `policy/lineup` and
   `policy/agents.lock`. On a match it goes on to the composition preflight.

The deputy never writes to the agent-tree checkout. It applies the patch only
in a scratch repository under its root.

### The Stop On A Tree Mismatch

When the trees differ, or the checkout doesn't hold the configured commit, the
self-check fails `agent_tree`. The deputy still serves, with execution
disabled, and prints:

```text
freesided deputy: agent_tree failed; execution stays disabled (the agent tree at <commit> in <agent-tree-checkout> is not the tree auth adopt emitted: review and commit <root>/daemon/agent-tree.patch in that checkout, set agent_tree.commit to the new commit, and restart; ...)
```

A blocking `system_health` card (`deputy_preflight_failed`) says the same in
the paired clients, and the deputy accepts no new work. Nothing was
submitted. The identities stay adopted on that root. The adoption report and
log name the identities, and the log can quote a cost owner, so keep them out
of shared logs.

When adoption itself fails or writes no patch, the check fails the same way
and names those two files instead.

### The First Start

The first start always stops this way, because no commit holds the tree yet:

1. Create the agent-tree checkout (below) and set `agent_tree.checkout` to
   it and `agent_tree.commit` to its current commit.
2. Choose the cost owners and the account
   ([Name The Cost Owner And The Account](../daemon/README.md#name-the-cost-owner-and-the-account))
   and the three dated inputs, and start the deputy. It reports the mismatch
   above.
3. Stop the deputy, read the patch, then commit it in the checkout:

   ```sh
   git -C <agent-tree-checkout> apply --index <root>/daemon/agent-tree.patch
   git -C <agent-tree-checkout> commit -m "Adopt the baseline agent tree"
   git -C <agent-tree-checkout> rev-parse HEAD
   ```

4. Set `agent_tree.commit` to the printed commit and start the deputy again.

Later fresh roots reuse that commit. The emitted tree depends on the identity
ids, the route names, the review model, the review effort (`review.effort`, a
Freeside effort level: `low`, `medium`, `high`, or `max`), the three prompt
packages, the three dated inputs, and, when the run has subscription
judgments on, `judgment.model` and `judgment.publication_author_prompt`. It
doesn't depend on the state root, the cost owners, or the account.

Keep `agent_tree.terms_basis_date`, `agent_tree.pricing_revision`, and
`agent_tree.offer_not_after` at the values the commit was adopted with. A
changed value is a different tree and fails the check.

The patch creates every file, so it doesn't apply over an existing tree. To
replace a committed tree (a renewed offer, a new review model or effort, a
changed prompt package), remove the old one in the same commit:

```sh
git -C <agent-tree-checkout> rm -r -q policy/agents policy/fragments policy/lineup policy/agents.lock
git -C <agent-tree-checkout> apply --index <root>/daemon/agent-tree.patch
```

### Where The Agent Tree Lives

Keep the tree in its own local repository, separate from any Freeside working
checkout, for example:

```sh
git init <agent-tree-checkout>
git -C <agent-tree-checkout> commit --allow-empty -m "Start the agent tree"
```

- **The commit must stay.** The daemon reads the tree from an exact commit
  through git. A working branch is rebased, force-pushed, and deleted after
  merge, and a commit that only such a branch held can be pruned.
- **The tree names your identities.** A commit on a working branch travels
  with the next push. A separate repository is pushed only when you choose to.
- **A tree change is its own review.** It changes which agent each role runs,
  independent of any code change.

## Replace The Runtime

The deputy composes once per start, so a new build or a settings change takes
effect at the next start on the same root. End the running deputy
([Complete Deliberately](#complete-deliberately)), build the reviewed
replacement from a clean checkout, and start `freesided deputy -root <root>`
again. The start prints the new settings digest, adopts the agent tree, and
reruns the composition preflight; the root's tasks resume or fail on their own
records. Pairing, signing material, approvals, and attempts stay where they
are. A new build opens the existing database and never rolls it back, so
keep the root's encrypted checkpoints until the new build has served a run.

The deputy has no retained-session resume and keeps no copy of earlier
inputs: the settings file and the files it names are the inputs, read at each
start.

## Complete Deliberately

End the run with SIGINT (Ctrl-C in the deputy's shell) or SIGTERM. The deputy
stops its daemon, deletes the containers its manifest records, and proves
every recorded container, volume, and network absent before it releases the
lease and removes `rig-acquisition.json` and `composition-manifest.json`. A
failed proof, or a daemon that ended in error, abandons the lease instead, so
the next start recovers it. The database, the adoption files, and the kept
manifests under `<root>/production-evidence/composition/` remain.

## Recover An Interrupted Deputy

A deputy that dies without releasing its lease (a crash, SIGKILL, a reboot)
leaves a stale lease. The next `freesided deputy -root <root>` on the same root
recovers it as `freesided rig recover` would, then files an advisory
`system_health` card (`deputy_run_interrupted`) naming the run it
interrupted; the root's tasks resume or fail on their own records. A stale
lease for another root refuses the start and names the command that recovers
it:

```sh
freesided rig recover -state-root <that-root>/daemon -confirm
```

A stale lease also keeps `prod` from starting with a driver; the driverless
installed daemon starts and keeps running, and recovery does not need it
stopped. `rig recover` rejects a live holder, a locked database, a live
listener, or resources that cannot be cleaned, and it never kills a stored
PID. Never delete a rig manifest by hand or clear the gate while the daemon
or recorded resources remain live.

Recovery first removes exactly the manifest-recorded containers, then
recovers review-owned volumes and networks through their existing journal.
The full review namespace must belong to the stale manifest; journal
ownership, fingerprints, and lease checks still govern persistent resource
removal. A lost review receives its normal interrupted outcome, without
running a provider or advancing the workflow. Unknown persistent resources
keep the rig gate closed. Recovery opens only an existing database at the
binary's exact schema; it never migrates or recreates one.

## Recover Expired Reviewer Credentials

If composition preflight fails `codex_credentials`, use the matching
remediation. Preflight still enforces its lifetime floor, identity binding,
and re-enrollment hold. Recovery creates no task and starts no agent
execution. Both procedures below need the database free, so end the deputy
first, and use the build it ran so the database stays at its schema.

### Renew An Existing Store

```sh
freesided renew-codex \
  -db <root>/daemon/freeside.db \
  -auth-identity <agent_tree.review_auth_identity> \
  -auth-store-root <review.input_root> \
  -auth-store <review.auth_snapshot> \
  -approved-recipe <approved_recipe>
```

Pass every approved recipe when the store uses more than one. Success reports
only readiness coordinates and creates no hold. Start the deputy again; its
composition preflight must now pass `codex_credentials`. If renewal was
interrupted, rerun it first to recover any pending rotation.

### Replace A Revoked Chain And Resolve Its Hold

When renewal reports a rejected or unavailable refresh chain, follow
[Enroll A Codex Subscription Identity](../daemon/README.md#enroll-a-codex-subscription-identity):
run `codex login`, stage its output in a separate private input root, and run
`enroll-codex` against the deputy's database and live store. Enrollment
leaves a hold that requires a paired-device command.

Serve the deputy's root with a driverless daemon:

```sh
freesided -environment ephemeral -driver disabled \
  -listen <listen> \
  -db <root>/daemon/freeside.db -state-dir <root>/daemon \
  -approved-recipe <approved_recipe>
```

`<listen>` and `<approved_recipe>` are the settings file's values. If the
database also holds evidence for other approved recipes, repeat
`-approved-recipe sha256:<digest>` for each so paired-client sync can
reconstruct it. The daemon runs no agent, opens no App authority, and takes
no rig lease, so it needs none of the deputy's other settings.

Its readiness line gives the endpoint and a pairing code; a client already
paired with the deputy at the same listener reconnects, and
`freesided pairing-code -state-dir <root>/daemon` mints a fresh code. In the
paired client, inspect the hold's exact digest, fence, and expiry, then choose
**Resolve re-enrollment**. The daemon cannot accept the hold on your behalf.
Stop it with Ctrl-C when done, then start the deputy again.

Recovery leaves the task count unchanged; for an exercise requiring its first
task from the client composer, keep the database at zero tasks and use the
client-submission path below.

## Submit A New Task From A Client

This is the live creation exercise for #1360 and the Mac/physical-iPhone
acceptance retained by #1211. It needs current installed clients and the
normal authenticated App, policy, admission, and specification-approval
gates. Hermetic HTTP tests do not replace this evidence.

`manual_submission_config` in the settings names the operator-owned JSON
described in the
[daemon guide](../daemon/README.md#configure-client-task-submission); an
empty value disables client tasks. The daemon reads and validates it once at
start, so an edit takes effect at the next start, and a file with an empty
`projects` array disables new submissions. A paired client's submission binds
this launch implicitly: the deputy creates the task only for a configured
project whose policy and commit author the launch admits, and answers any
other project as unconfigured.

### The Publication File And Work-Unit Declaration

A CLI submission whose task file is exactly one GitHub issue URL names the
issue in its publication file, with the record a client-composed task saves:

```json
{"recipe": "freeside.client-publication/v2",
 "source_issue": "https://github.com/<owner>/<repo>/issues/<number>",
 "commit_author": {"app_slug": "canonical-app-slug", "bot_user_id": 123}}
```

Composition preflight refuses a publication file that omits `source_issue` for
such a task or names anything but the task's issue. What the pull request then
carries depends on where the issue lives:

- **An issue in the managed repository:** `Closes #<number>`, or
  `Refs #<number>` when the closure is declined.
- **An issue in another repository:** `Source issue: <url>`, which closes
  nothing.

A publication file with a literal `title` and `body` is still accepted for any
other task file. Its text is published as written, with no publisher-written
issue reference. `freesided submit -h` describes both forms.

The work-unit declaration is required for every new submission. It says when
the task is finished, and the smallest one finishes the task when its pull
request merges:

```json
{"completion_criterion": "bound_pr_merged"}
```

Composition preflight and `freesided submit` each refuse a new submission
without it, because the daemon records completion against the declaration: a
task accepted without one never finishes when its pull request merges, and it
keeps its work-in-progress slot. A task the daemon accepted before this rule
is left as it is and stays "Ready" after its merge. Release it with
`freesided abandon -db <database> -task <task-id>`. The command releases the
slot, closes that exact workflow episode, and expires its open decisions and
notices. These steps commit separately. If one fails, inspect `episode`,
`workflow_closed`, and `cleanup_complete`, then repair the reported episode
with `freesided abandon -db <database> -task <task-id> -episode <episode>`.
A delayed repair cannot release newer work. Closure refuses new engine dispatch
claims and publication. A winning claim may finish downstream preparation and
launch; closure neither revokes it nor undoes a dispatched effect.
Their late results remain history and consume capacity until the
normal release evidence exists. An explicit reasoned reattempt still needs
an approved specification, valid lineage, no cancellation fence, and cap-checked
admission. Upgrading does not infer closure from historical abandonment.

### The Visibility Seed

Until #1332 lists configured projects in the sync bootstrap, the client
composer offers a project only once it holds a task. For a fresh root, submit
a seed task for the project from the CLI, as in
[Start And Verify](#start-and-verify), before the client exercise. The seed
spends one real specification execution and produces a spec-approval
attention item; leave it unapproved, since approving it would run the
throwaway seed to implementation. The writer identity has a single execution
slot, so the client task's specification run queues behind the seed's until
the seed reaches that gate, a few minutes. This is an accepted trade-off,
tracked in #1405 and removed by #1332.

### The Exercise

1. Confirm the chosen project has no existing task for your source. For the
   requested gh-imgup exercise, use
   `https://github.com/freeasinbird/gh-imgup/issues/82` as one line. When the
   exit condition requires source-issue closure, the agent must give the
   operator the exact bare canonical URL before submission, with no prose or
   trailing punctuation. Prose-wrapped URLs remain valid task input but do not
   save a publication source reference. Never pre-create this exercise's
   target through the CLI: the target must be the task the client composer
   creates.
2. Enter that source in the client composer and submit. Record the command
   result, the new task ID, its specification-run ID, and the returned sync
   state. Verify the task name and run timeline on the Mac and physical
   iPhone.
3. Read the task's record with `freesided inspect task` and confirm it is the
   client's task, not the seed's, in the configured project.
4. Follow the normal clarification or specification flow for that task. Leave
   the human specification approval gate in place. Each deliberate client
   submission of the same project and source creates a distinct task, so a
   second submission is a second task, not proof of reuse; record exactly one
   as the target.

The deputy runs the client's task like any other: after the specification is
approved, it runs implementation, review, and publication, and the task record
shows each step. A ready publication continues to the walkthrough on both
clients. Record the target task's evidence separately from the seed's, and
leave live acceptance outstanding if the approved inputs or either physical
client are unavailable.

### Next Exit-Run Commands

For the #1211 exit run against `freeasinbird/gh-imgup#82`, after a rehearsal
pass on the same build ([Rehearse Before The Real Run](#rehearse-before-the-real-run)),
with the settings file naming the manual-submission file and `prod` left
running:

```sh
freesided deputy -root <root>
# in a second terminal, the visibility seed for a fresh root:
freesided submit -db <root>/daemon/freeside.db --require-composition \
  -project <project> -task /absolute/seed-spec.md -policy /absolute/policy.json \
  -publication /absolute/publication.json -work-unit /absolute/work-unit.json
# once the client composer has created the target task:
freesided inspect task -db <root>/daemon/freeside.db -task <task-id>
```

Verify the published head on both paired clients, then end the deputy
deliberately with Ctrl-C.
