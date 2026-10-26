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

## An Exit Run In Order

An exit run is this runbook from start to finish. The exit-run skill
(`.claude/skills/exit-run/SKILL.md`) says where to stop for the operator,
whether a step counts, and what to record; each step's procedure is in the
section it links.

1. Write the real run's settings and inputs
   ([The Inputs And What Refuses Them](#the-inputs-and-what-refuses-them)),
   then run the pre-spend checks on its root ([Start And Verify](#start-and-verify)).
2. Run the fixture pass on the build the real run will use
   ([Rehearse Before The Real Run](#rehearse-before-the-real-run)).
3. Prepare the operator's devices
   ([Prepare The Operator's Devices](#prepare-the-operators-devices)).
4. Start the deputy on the real root, pair both clients, test their
   notifications, and resolve a retained root's health items
   ([Start And Verify](#start-and-verify)).
5. Submit the target from the CLI ([Start And Verify](#start-and-verify)),
   or, in client-target mode, the visibility seed and then the composer's
   task ([Submit A New Task From A Client](#submit-a-new-task-from-a-client)).
6. Follow the task record through the specification approval to
   publication ([Start And Verify](#start-and-verify)); a client-composed
   task follows [The Exercise](#the-exercise).
7. Decide the closure card, walk through the ready card, and merge
   ([Walk Through, Merge, And Raise The Alerts](#walk-through-merge-and-raise-the-alerts)).
8. Wait for the completion fact and raise the alerts (same section).
9. Apply the record checks to the final task record
   ([Start And Verify](#start-and-verify)).
10. End the deputy ([Complete Deliberately](#complete-deliberately)).

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

Build `freesided` from a clean checkout of the reviewed commit, stamping that
commit as its version. In `daemon/`:

```sh
go build -ldflags "-X main.version=<sha>" -o <dir>/freesided ./cmd/freesided
```

`<sha>` is the checkout's commit (`git rev-parse HEAD`). The composition
preflight's `daemon_build` check refuses a build whose version is empty,
`devel`, `(devel)`, or ends in `-dirty`, and a plain `go build` reports
`(devel)`.

Before the start, run every pre-spend check in one command, with the files
the run will submit:

```sh
freesided prelaunch -root <root> -task /absolute/spec.md -policy /absolute/policy.json \
  -publication /absolute/publication.json -work-unit /absolute/work-unit.json
```

It exits 0 only when no check failed, and writes the JSON it prints to
`<root>/daemon/prelaunch.json`. A failed check carries its `fix`, and
`first_missing` names the first onboarding step the root lacks; on a fresh
root that is the agent tree ([The First Start](#the-first-start)). It takes
and releases the rig lease, so run it before the deputy starts. Its lineup
probe spends one minimal call per lineup launch; `-no-live` skips it
([Run The Pre-Spend Checks Before A Launch](../daemon/README.md#run-the-pre-spend-checks-before-a-launch)).
A failed `images` check names the pinned reference the local container store
no longer holds; [Reseed A Pinned Image That Stopped Resolving](#reseed-a-pinned-image-that-stopped-resolving)
restores it.

Then start the deputy and keep its shell open:

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

Submit prints its result as JSON. Its `task_id` is the `<task-id>` that
`freesided inspect task` takes below; its `run_id` names the implementation
run, which exists only once the specification is approved.

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

Once both clients are paired, send each a test notification, as the deputy's
OS user:

```sh
freesided notify-test -state-dir <root>/daemon
```

It prints a line per paired device, ending `accepted by ntfy` or the failure
([Control Socket and Pairing Codes](../daemon/README.md#control-socket-and-pairing-codes)).
Acceptance is the provider's, so the operator confirms that each notice
arrived. A phone can't send itself one yet (#1959).

A standing root keeps the system-health items its earlier runs left open, and
a restart does not close them. The blocking ones hold the next run's first
admission with reason `blocking_system_health` until an operator resolves
them in a paired client: Inbox > System health in the Mac client, with the
action the item's card offers (on run 117, More Actions > Resume Unattended
closed the run-116 `daemon_durable_stop` notice; Acknowledge only records,
and the item stays open). Advisory items never hold admission. Resolve them
once the clients are paired and before the first approval, then confirm
that the read's `admission_error` and `operator_stop` are `null` and no
item's `admission` is `holds`:

```sh
freesided inspect health -db <root>/daemon/freeside.db
```

A blocking item that no offered action closes keeps holding admission; stop
the run and report it rather than going on. A missed blocking item shows as
the run's hold reason `blocking_system_health`. A CLI-submitted task's
specification run already waits for admission, so the hold can show at once;
in client-target mode nothing waits until the operator composes a task.

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

A record that fails any check is a finding, not proof of the run.

### Walk Through, Merge, And Raise The Alerts

Once the run's `publication.outcome` is set, the run goes on in this order,
with the deputy still serving:

1. **The closure card, first.** Under the human closure gate, for a source
   issue in the managed repository, the pull request stays a draft until
   the card is decided
   ([The Publication File And Work-Unit Declaration](#the-publication-file-and-work-unit-declaration)),
   so nothing can merge ahead of it. Read
   `gh pr view <number> --repo <owner/repo> --json isDraft,body` before the
   decision and after it: afterwards the pull request has left draft and
   carries the final reference.
2. **The walkthrough.** Fetch the run on both paired clients and exercise
   the ready-card actions the run needs; in an exit run, the ones its
   contract lists. Record which actions ran, the task and
   run IDs, the endpoint, and the published head the record shows. Later
   actions can change the run, so an earlier read does not prove a later
   head. Health from the default service on port 7331 does not prove this
   run is still available at its paired endpoint.
3. **The stopped-daemon check.** Right before the merge, look on the Mac for
   the STOPPED banner or an open `daemon_durable_stop` notice in Inbox >
   System health. A daemon that stopped itself records no completion, and
   the review-growth alert depends on that fact. The notice names the
   daemon, not a task, so no `freesided` read shows it.
4. **The merge.** The pull request merges; on real work in an exit run, the
   owner merges it.
5. **The completion fact.** Wait for the run's `publication.completion` in
   the task record and the resolved ready item. The daemon records the fact
   against the task's work-unit declaration: the one submitted with a CLI
   task, or the one it wrote for a client-composed task.
6. **The alerts.** Raise each alert the exit contract triggers
   synthetically, with the deputy's build and the implementation run's id,
   in the order of the `raise-item` command block in
   [Operational Commands](../daemon/README.md#operational-commands). This
   is the only point where all three work: `review-growth` is refused until
   the completion fact is recorded, and ending the deputy stops its daemon.
   Each command prints the raised item's ID and type. A run-bound item
   (`held-work`, `review-growth`) shows in `inspect task`'s `items[]` with a
   `deliveries` entry per paired device. The `stall` notice names the
   daemon, not a run, so no `freesided` read shows it; the operator's report
   is its delivery evidence. A refusal (a daemon that is not `ephemeral`, no
   daemon, a run with no completion) is a finding, not something to work
   around.
7. **The record checks** above, on the final task record.
8. **The end.** [Complete Deliberately](#complete-deliberately). If the
   deputy died first, start it again on the same root rather than a fresh
   one: it recovers the stale lease
   ([Recover An Interrupted Deputy](#recover-an-interrupted-deputy)), and a
   fresh root would not hold this run's evidence.

## The Inputs And What Refuses Them

One table per mode. Each row names the input, where it lives, and the check
that refuses it, so a missing input fails at a known point instead of during
the run. The daemon guide's settings table
([Run Real Work With The Deputy](../daemon/README.md#run-real-work-with-the-deputy))
is the authority for the settings, and it changes. The pre-spend record's
`inputs` checks apply `submit`'s refusals to the files
([Run The Pre-Spend Checks Before A Launch](../daemon/README.md#run-the-pre-spend-checks-before-a-launch)).

**CLI mode.**

| Input | Where it lives | What refuses it |
| --- | --- | --- |
| Settings, every key | `<root>/daemon/settings.json`, mode `0600`: prompt packages from the approved default-branch commit, the approved recipe digest | The deputy, before it takes the lease: a missing, unknown, or repeated key (`requireEveryKey`, `daemon/cmd/freesided/deputy_settings.go`), a relative path, an invalid prompt package. A stale package passes every check; only the person preparing the run catches it (run-70 lost its publication to one). |
| Project | `submit -project`: the settings' `repository.project` | `freesided submit`, which requires it; composition preflight refuses a project the launch does not admit |
| Specification | `submit -task`: a file holding exactly the target's bare canonical issue URL, or a specification file | `freesided submit`, which requires it |
| Policy | `submit -policy`: the resolved policy keys, including `gates.spec_approval` and `gates.source_issue_closure` | `freesided submit`, which validates the keys and the specification policy (`specify.ParsePolicy`, `daemon/internal/specify/policy.go`) |
| Publication | `submit -publication`: for a bare-URL target, the client record with `source_issue` set to that same URL ([The Publication File And Work-Unit Declaration](#the-publication-file-and-work-unit-declaration)) | Composition preflight and `freesided submit` (`submittedSourceReference`, `daemon/cmd/freesided/submit.go`) |
| Work-unit declaration | `submit -work-unit`: at least `{"completion_criterion": "bound_pr_merged"}` | Composition preflight and `freesided submit` (`errWorkUnitRequired`, same file). A task accepted without one never records completion (#1931). |

A target in the managed repository becomes the pull request's closing
reference; one in another repository is linked and stays open.

**Client-target mode.** The settings and the seed's `submit` inputs are the
CLI-mode rows, for the visibility seed on a fresh root
([The Visibility Seed](#the-visibility-seed)). Two rows are added:

| Input | Where it lives | What refuses it |
| --- | --- | --- |
| Manual-submission file | `manual_submission_config` in the settings, the JSON in the daemon guide ([Configure Client Task Submission](../daemon/README.md#configure-client-task-submission)) | The deputy at start, before the lease. A file with an empty `projects` array starts but accepts no client submission. |
| Composer source text | The pairing checkpoint list, which the exit run writes and the operator types | Nothing: `Please handle <URL>.` creates valid work with no saved source reference (#1928). When the contract requires source-issue closure, write the bare canonical issue URL, with no prose or trailing punctuation. |

## Rehearse Before The Real Run

An exit run starts with a fixture pass: the real run's daemon runs the whole
pipeline once, unattended, on Freeside's fixture repository and its
canonical issue, so a launch, input, publication, completion, or alert
problem shows up before the operator is involved. The pass proves the
build's workflow, not the real project's settings, models, images, or trust
profile; preflight checks those against the real project (#2222, #2208).
The exit-run skill (`.claude/skills/exit-run/SKILL.md`, Rehearse Before The
Operator) says when a pass counts; this section is the procedure. Decision
record: `devlog/2026-10-23-1321-fixture-pass.md`.

**Root and build.** Use a fresh root and a `freesided` built from a clean
checkout of the SHA the real run will use, with the version flag in
[Start And Verify](#start-and-verify): a plain `go build` fails preflight's
`daemon_build` check. A real run may start from a newer tip than its pass
only when `bash scripts/runtime-diff.sh <pass-sha> <run-sha>` exits 0 (both
SHAs build the same `freesided`, and the diff touches no image, `prompts/`,
or `policy/` source) and the run record names both SHAs and the verdict;
otherwise the pass runs again on the new tip.

**The fixture.** The repository is `freeasinbird/freeside-review-challenge`
(private) and its canonical issue is
[freeasinbird/freeside-review-challenge#2](https://github.com/freeasinbird/freeside-review-challenge/issues/2),
a small change sized for a clean review: a review finding or a closure gate
opens a card only a person can answer, and that stops the pass. The `prod`
GitHub App is installed there. The daemon reads the private issue with a
read-only installation token for that repository, so the policy file's
`research.allowlist` must include `https://api.github.com` ([Configure
Client Task Submission](../daemon/README.md#configure-client-task-submission)).

Reset the fixture before every pass, before its onboarding, so every pass
starts from the same commit and issue and a pass on one build compares with
the pass on the last. From a clone of the fixture:

```sh
git push --force origin fixture-baseline:main
gh issue reopen 2 --repo freeasinbird/freeside-review-challenge
git ls-remote --heads origin
```

`git ls-remote` lists `main` alone, unless the last pass's branch survived
its merge; delete that one with `git push origin --delete <branch>`. The
force-push works because the repository has no branch protection (a private
repository on a free plan allows none); if it ever gains protection, the
reset needs a bypass. Because the reset discards each pass's merge, the
fixture's `AGENTS.md` lets the pass merge its pull request.

**Settings.** The fixture has settings of its own, fixed here and never
derived from a real run's:

| Key | Fixture value |
| --- | --- |
| `repository.repo` | `freeasinbird/freeside-review-challenge` |
| `repository.repository_id` | `1363494422` |
| `repository.base_ref` | `main` |
| `repository.base_sha` | The `fixture-baseline` commit (`git ls-remote origin fixture-baseline`) |
| `repository.allowed_paths` | `src/**`, `test/**`, `README.md` |
| `repository.checkout` | A clone of the fixture of its own |
| `repository.project` | `fixture` |
| `listen`, `seed_root` | Of its own |
| `manual_submission_config` | `""` |
| `writers.specifier`, `writers.implementer`, `writers.remediator` | `claude-haiku-5-5` at `low` |
| `review.model`, `review.effort` | `gpt-6-luna` at `low` |
| `review.auth_mode` | `subscription` |
| `agent_tree.review_harness` | `codex_cli`, or absent |
| `judgment.model`, `judgment.effort` | `claude-haiku-5-5` at `low`, with the pinned `claude_bin` and `claude_sha256` |
| `images.agent`, `approved_recipe` | From the fixture's onboarding on this root (below) |
| `agent_tree.commit` | The fixture tree's commit (below) |

These are the cheapest models expected to complete the workflow; onboarding
step 6 below checks that the pinned CLIs serve them. `gpt-6-luna` is the
affordable listed model in the pinned Codex CLI's bundled catalog
(`codex debug models --bundled` in the review image). A role that is
refused, or can't complete the fixture's issue at these values, is a pass
finding: move it up one tier and change this table with the pass. Every
other key is machine-bound (paths, identities, cost owners, dated inputs,
`prod.*`, the build's `images.exporter` and `images.review`) and comes from
[Start And Verify](#start-and-verify), as for any root. Keep
`review.instructions`, `judgment.publication_author_prompt`, and
`timeouts.*` the same from pass to pass: a pass that changes one isn't
comparable with the last.

The fixture's models and efforts make a tree of its own. Adopt it once
through [The First Start](#the-first-start) and keep it on its own branch of
the agent-tree checkout; later passes reuse its commit until an adoption
input changes, such as a build whose prompt packages changed, which the
deputy's self-check refuses. Then adopt it again.

**Onboard the fixture.** The fresh root starts with an empty database, so
every pass onboards the fixture, and the onboarding takes the fixture's own
review configuration digest. On the fresh root, in this order:

1. Write the settings with every value above. Until onboarding replaces
   them, set `images.agent` and `approved_recipe` to any digest-pinned image
   and recipe digest, such as an earlier pass's.
2. Adopt the fixture tree through [The First Start](#the-first-start), or
   set `agent_tree.commit` to the one an earlier pass adopted.
3. On the start whose tree matches, the self-check fails `trust_profile` and
   the checks that need the project image, since this root holds neither
   yet. Those failures are expected here. It still writes
   `<root>/daemon/composition-manifest.json`. Read its
   `review_configuration_digest`; `freesided preflight` with the same inputs
   reports the same digest. End the deputy with Ctrl-C: onboarding refuses a
   database the daemon holds.
4. Onboard the fixture on this root, as daemon/README.md describes under
   Renew A Codex Subscription Credential (the `freesided onboard
   <owner/name>` paragraphs):

   ```sh
   freesided onboard freeasinbird/freeside-review-challenge \
     -db <root>/daemon/freeside.db -repository-id 1363494422 \
     -commit <fixture-baseline-sha> -base-ref main -source <fixture-clone> \
     -review-config-digest <digest> ...
   ```

   The App and base-image flags (`-state-dir`, `-registration-id`,
   `-base-image`, `-base-build-ref`) are the machine's. Run it once without
   `-approve` and read the review, then again with `-approve
   <approval_digest>`. Until the fixture is trusted in `prod`'s App
   authority, the first call opens an installation intent and every later
   call, the `-approve` one included, passes `-resume`; the daemon guide's
   `freesided onboard` steps give the intent's expiry and `-replace-intent`.
5. Set `images.agent` to the approving run's `project_image.image_ref` and
   `approved_recipe` to its `project_image.recipe_digest`.
6. Run `freesided probe-lineup -root <root>`
   ([Probe The Lineup Before A Run](../daemon/README.md#probe-the-lineup-before-a-run))
   before the deputy starts again. A model the pinned CLIs refuse fails here
   for the cost of one short call per role, not partway through the pass.

**Inputs.** The task file holds the canonical issue's bare URL, and the
publication file names that URL in `source_issue`
([The Publication File And Work-Unit Declaration](#the-publication-file-and-work-unit-declaration)).
The work-unit declaration is `{"completion_criterion": "bound_pr_merged"}`.
The policy file names the settings' `allowed_paths` in `paths`, includes
`https://api.github.com` in `research.allowlist`, sets `gates.spec_approval`
to `false`, and leaves `gates.source_issue_closure` absent or `false`, so
neither a specification nor a closure card waits for a person. These are the
CLI-mode rows of [The Inputs And What Refuses Them](#the-inputs-and-what-refuses-them),
so the pass exercises each of them. It leaves `manual_submission_config`
empty, so in client-target mode the real run's deputy start is the first
check of that file. The pass submits no `--task-line`: a real run's lines name agents in the real
project's tree, not the fixture's. `submit` checks only a line's shape, so
until the readiness walk (#2222) checks the real lines before spend, a line
the real tree can't honor is first refused at the real run's admission.

**Run it.** Start the deputy and submit as in [Start And Verify](#start-and-verify),
on the fixture root with `-project fixture`. Follow the task with `freesided
inspect task` at each change of its `as_of_revision` until the run's
`publication.outcome` is set. Merge the pull request in the fixture, wait for
the run's `publication.completion`, and raise the alerts the exit contract
triggers synthetically with `freesided raise-item`
([Walk Through, Merge, And Raise The Alerts](#walk-through-merge-and-raise-the-alerts)).
With no paired client on the fixture root, the record shows each run-bound
item but no `deliveries`: the pass proves the daemon side only, and the
`stall` item's proof is the ID `raise-item` prints.

**End it before the real run.** Apply the four record checks above, then end
the deputy with Ctrl-C ([Complete Deliberately](#complete-deliberately)). It
must exit 0. A nonzero exit, or a line saying it left the rig lease for the
next start to recover, means the lease was abandoned; `rig-acquisition.json`
is removed either way, so its absence proves nothing. Recover an abandoned
lease with `freesided rig recover -state-root <fixture-root>/daemon -confirm`
before the real run, which needs the rig lease and starts on its own root.

## Prepare The Operator's Devices

After the fixture pass and before the real run's deputy starts, the
operator's devices need what this list names. Each item says how to check it
and how to fix it. The app's commands are the operator's to run
([plan §10](plan.md#environments-prod-dev-and-ephemeral)).

- **`prod` runs driverless** from an installed app that includes #2080, and
  the App authority lives in its directories. Confirm it runs with the app's
  `daemon status` command in [Start And Verify](#start-and-verify): a
  stopped `prod` passes preflight's `prod_execution` check, so only that
  read catches it. Leave it running; the deputy never stops or restarts it.
  On failure, run the app's `daemon start` (app/README.md, Control The
  Supervised Daemon From A Command), or install the current app when the
  installed one predates #2080.
- **Both clients come from the run's tip.** Rebuild and install the client on
  the Mac and on the physical iPhone from the commit the run uses
  (app/README.md, Installing the Operator Client and Installing on an iOS
  Device). A client built from an older commit proves the older commit.
- **The iPhone reaches the daemon.** Both devices are on the tailnet the
  daemon will listen on. A VPN on the Mac can cut the iPhone off (#1930):
  turn it off, or confirm the tailnet route. Pairing comes after the start,
  against the deputy's `api_url`, so a fresh root has no paired endpoint to
  check yet.
- **Credentials need nothing from the operator** once preflight passes
  `claude_credentials` and `codex_credentials`. A Codex chain that renewal
  reports as revoked needs `codex login` and a paired client's **Resolve
  re-enrollment**
  ([Replace A Revoked Chain And Resolve Its Hold](#replace-a-revoked-chain-and-resolve-its-hold))
  before any pass can complete.
- **The liveness probe reaches the phone,** when a gate depends on it. The
  probe runs outside the daemon, so nothing in the run installs or tests it:

  ```sh
  bash scripts/daemon-liveness-probe.sh check
  bash scripts/daemon-liveness-probe.sh notify-test
  ```

  `check` must exit 0, and the operator confirms that the test notice
  arrived on the iPhone. On failure, run
  `bash scripts/daemon-liveness-probe.sh install` or fix the phone's
  subscription, then repeat both.

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
ids, the route names, each writer role's model and effort (`writers.<role>`;
an effort is a Freeside level: `low`, `medium`, `high`, or `max`), the
review model and effort (`review.model`, `review.effort`), the three prompt
packages, the three dated inputs, and, when the run has subscription
judgments on, `judgment.model`, `judgment.effort`, and
`judgment.publication_author_prompt`. It doesn't depend on the state root,
the cost owners, or the account.

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
recovers it as `freesided rig recover -migrate` would, then files an advisory
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
keep the rig gate closed. Recovery never creates a database, and by default
it reads the review journal only at this build's schema.

A lease left by an older build, whose database is at an older schema, makes
recovery refuse when the manifest records review volumes or networks. The
refusal names the database's schema, this build's schema, and the newest
migration file at the database's schema. It may come after the recorded
containers are removed; the gate stays closed. There are two ways out:

- Rerun with `-migrate` to migrate the database to this build's schema
  before the journal teardown, as the daemon's next start on that root
  would. Migrating is one-way: a build at the older schema can no longer
  open that database.
- Recover with a build at the database's schema, identified by that
  migration file.

```sh
freesided rig recover -state-root <that-root>/daemon -confirm -migrate
```

The deputy migrates its own root's database when it recovers its own stale
lease, because its start migrates that database anyway. A stale lease for
another root still refuses the start and names `rig recover`.

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

## Reseed A Pinned Image That Stopped Resolving

A pinned image reference (`images.exporter`, `images.agent`, or
`images.review`) can stop resolving between runs: the local container store
loses its entry under the digest. The cause is not known (#2309). The digest
is unchanged, so a reseed from a registry that still holds it restores the
entry without a rebuild.

**The symptom.** `freesided prelaunch` fails an `images` check, the deputy's
self-check fails `exporter_image`, or the composition preflight reports that
an image is not in the local container store; each names the reference. A
container create that fails on it names the image too, in `auth-adopt.log`
or the deputy log. To confirm, `container image inspect <ref>` reports
`image not found`.

**When the port's registry is running.** Pull the digest reference from it
over HTTP:

```sh
container image pull --scheme http <ref>
```

A plain `container image pull <ref>` tries HTTPS and fails with
`bad protocol version` against a local registry.

**When it is a project image and its registry is stopped.** `freesided
onboard -local-registry-port <port>` keeps its registry container but no
local tag. Find the container by its label, start it, and pull as above:

```sh
container list --all --format json   # the entry labelled ai.freeside.project-image.registry-port=<port>
container start <id>
container image pull --scheme http <ref>
```

**When it is a script-built image and no registry runs on that port.** The
`scripts/build-*-image.sh` scripts remove their temporary registry but keep
the image's tag, which `container image list` still shows under
`127.0.0.1:<port>/<name>`. Start the same pinned registry helper on the
reference's port, push that tag to it, pull the digest reference, and remove
the registry. `<port>`, `<name>`, and `<tag>` are the reference's port, image
name, and that tag:

```sh
container run --detach --name freeside-reseed-registry \
  --publish 127.0.0.1:<port>:5000 \
  docker.io/library/registry@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373
until curl --silent --fail http://127.0.0.1:<port>/v2/ >/dev/null; do sleep 1; done
container image push --scheme http 127.0.0.1:<port>/<name>:<tag>
container image pull --scheme http <ref>
container delete --force freeside-reseed-registry
```

The push sends the cached image, so the pull by digest gets the same bytes
and the pinned reference does not change. Settings stay as they are.

**When neither works.** If no registry holds the digest and no cached tag
does either, so the pull by digest fails, rebuild the image and re-pin its
new digest in the settings.

**To confirm.** `container image inspect <ref>` now succeeds, and
`freesided prelaunch` passes its `images` checks.

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
  `Refs #<number>` when the closure is declined. Under the human closure
  gate (`gates.source_issue_closure` set to `true` in the policy file), the
  publisher opens the pull request as a draft carrying `Refs #<number>` and
  holds it there until the closure card is decided. An approval that keeps
  the resolves flag marks it ready with `Closes #<number>`; a decline, or an
  approval that turns the flag off, marks it ready with `Refs #<number>`.
  The card is an effect-proposal card in a paired client's inbox;
  `freesided inspect task` doesn't list it, because its subject is the
  proposal batch, not the task (#2226). Only the approval path has run live.
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
clients. Under the human closure gate, the operator decides the closure card
in a client inbox before the merge, and the pull request stays a draft until
then. `gh pr view <number> --repo <owner/repo> --json isDraft,body` confirms
it left draft with the final reference; the merge waits for that. Record the
target task's evidence separately from the seed's, and
leave live acceptance outstanding if the approved inputs or either physical
client are unavailable.

### Next Exit-Run Commands

For the #1211 exit run against `freeasinbird/gh-imgup#82`, after a fixture
pass on the same build or one `runtime-diff` carries ([Rehearse Before The Real Run](#rehearse-before-the-real-run)),
with the settings file naming the manual-submission file and `prod` left
running:

```sh
freesided prelaunch -root <root> -task /absolute/seed-spec.md -policy /absolute/policy.json \
  -publication /absolute/publication.json -work-unit /absolute/work-unit.json
freesided deputy -root <root>
# in a second terminal, the visibility seed for a fresh root:
freesided submit -db <root>/daemon/freeside.db --require-composition \
  -project <project> -task /absolute/seed-spec.md -policy /absolute/policy.json \
  -publication /absolute/publication.json -work-unit /absolute/work-unit.json
# once the client composer has created the target task, with the task id from
# the composer's result (submit's task_id names the seed's task):
freesided inspect task -db <root>/daemon/freeside.db -task <task-id>
```

Verify the published head on both paired clients, then end the deputy
deliberately with Ctrl-C. These are the commands only; every step between
them is in [An Exit Run In Order](#an-exit-run-in-order).
