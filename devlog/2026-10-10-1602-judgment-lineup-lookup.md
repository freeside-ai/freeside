# Look Up Each Judgment Role in the Lineup at Every Call

Work unit #1425, plan §5.4 (Admission) and §8. Follows the wardless
admission class of #1421 and the hand audit recorded in
`devlog/2026-09-09-2145-subscription-judgments.md`. The issue body carries
the contract and the planning decisions; this note records the two owner
decisions taken at handoff to implementation, the places the implementation
left its plan, and what the refute-first pass found.

## Owner Decisions at Handoff (2026-10-10)

**A call adapter pinned to the audited build, over an audit record naming
the ward image's build.** The hand audit covered Claude CLI 2.1.267. The
baseline ward adapter pins `claude-code 2.1.220`, and wardless admission
compares the audit's harness build with the adapter's, so a judgment line on
the ward agent could be admitted only under an audit record that names
2.1.220. No recorded audit supports that. The baseline gains a call adapter
pinned to 2.1.267 and a call agent on it, and the seven judgment lines name
that agent. The owner confirmed the audit record as 2.1.267 on 2026-09-09.

The audit record (`judgmentCallAudit`) is written out in the daemon with the
adapter's digest as a literal. Built from the adapter under admission it
would prove whatever the lineup named. A test pins the literal to the
baseline call adapter, so a baseline edit fails the test instead of silently
moving the audit.

**Record a site-contract digest, over moving each site's instruction into the
role prompt.** A site's fixed instruction sits outside the prompt digest, and
the call launch's digest is the same at every site, so without a key of its
own an instruction change leaves calls from before and after grouped
together. Moving the instruction into the digest-bound prompt would bring back
a per-site selector on the prompt. The record carries the digest of the site
id, the composed instruction, and the output-contract identifier, and plan §8
names it as a comparison key.

## The Client Admits; the Source Only Resolves

Chose to run `domain.AdmitWardlessRole` inside the inference client on
whatever the role source returns, over the planning comment's resolver that
admits and hands back an admitted call. A source that returned its own
admission would make every fake and every later source a place where an
admitted call can be asserted, and the client would be trusting a returned
trust bit. The client now checks the line's prompt name against the prompt it
will send, rehashes the prompt bytes, validates the credential source, and
runs the admission before it reserves budget or reaches the driver. The test
fake builds a real agent closure for the same reason: admission passes on its
merits, and a test that edits one fact sees the refusal that fact causes.

## Unbound, Off, and the Startup Check

**One fixed reason reaches the result; the detail reaches only the health
item.** An unbound role returns the fail-safe with `judgment role has no
admissible lineup line` and the producer `unavailable/unbound`. The refusal's
detail names digests and enrollment state, and a call result is stored and
shown in more places than a `system_health` item is.

**A publication author with no prompt file is off, not unbound.** Its sites
return their fail-safe as before and no item is raised. Policy asks for a
role's line only while it asks for the role's work, and an item for a role
the operator never configured would be a standing false alarm. The plan's
§5.4 text now says so.

**The daemon admits every judgment role once at startup.** The planning
comment converged items only on calls. The drift auditor's item is blocking
and holds unattended admission, and a held deployment makes no drift-auditor
call, so no later call could ever resolve the item that holds it. The startup
check (`Client.CheckRoles`) admits each role without a call or a reservation,
raises the items before any site is reached, and resolves an item whose line
was fixed. Preflight runs the same check and fails on an unbound role.

**A start that cannot write the items stops.** The reporter logs a failed
write and retries on the next report, because a health item never fails a
judgment call. At startup that left a gap review found: with the drift auditor
unbound and the store failing, the daemon started with nothing holding
unattended admission. The startup check now returns the write error, as
agent selection's own startup item already did. A start cancelled during the
check reports the cancellation and writes nothing, since it proves nothing
about the lineup.

Consequence for a deployment adopted before this unit with judgment flags
set: it has no judgment lines, so preflight fails naming the roles, and a
daemon started anyway holds unattended admission on the drift auditor's item
until the lines are patched in. That is the intended signal, not a
regression to soften: without the lines every judgment site would run on its
fail-safe.

**The health reporter reads before it writes and remembers what it wrote.**
Every store write advances the revision clients watch. The client reports on
every call, so an unconditional converge would advance it per call.

**An item is keyed by its role and its reason, and replaced only at a
process's first report.** Keyed by role alone, an item kept the reason it
opened with: after a partial fix and a restart, the blocking item still told
the operator to fix what was already fixed. Rejected: rewriting the item
whenever the reason changes. Expiry refusals name the call's own deadline, so
the reason differs on every call and the item would be rewritten per call.
The startup check is the first report, so a restart always states the current
reason; within one process a changed reason waits for the next start.

## Limits the Record States Instead of Hiding

- `credential_source` is `interim_flag`: the line names the implementation
  identity's enrollment while the bytes sent are the flag's snapshot. #1426
  ends this.
- The requested model is the agent's; the observed model is what the CLI
  reports under `-judgment-model`. The record claims no agreement and nothing
  checks one (#1619).
- Nothing compares the deployed `-judgment-claude-sha256` pin with the audited
  build. Admission compares the adapter's authored harness build only.
- The producer label on a call result is now the admitted agent's
  `<service_operator>/<route_model_id>`, as planned. On the binding it was the
  driver protocol with the judgment configuration digest and the flag's
  model, so a call record no longer commits to the configured CLI pin. The
  run's preflight manifest and the startup digest check still do, per run.
  Restoring a per-call field is the owner's call, with #1619.
- The independence entries compare against the writing roles as the lineup
  names them at the call, not against the agent that wrote the run under
  judgment. A lineup change between the writing and the judging, or a task
  line that picked another writer for the task (#1640), misstates the
  pairing. The lineage is read from the tree alone, so a writer whose
  enrollment cannot run today is still compared.
- Identity is optional per record, because records carried over from a
  version 2 file have none, and nothing marks a record as written at version
  3. A version 3 record with its identity stripped therefore loads. The ledger
  is local state the daemon alone writes, and the identity is history, never
  an input to a later admission.
- A stored identity is checked for shape, never against today's registries
  (which roles write, which role a site belongs to). A registry edit would
  otherwise fail every retained record, and a ledger that fails to load is
  disabled and cannot prune the records that disabled it. The enums a record
  carries (credential source, effort, lineage relation) are still checked,
  so a value stays registered while a retained record can carry it.
- The publication author's prompt name is the role name, so one deployment
  has one author prompt at a time. #1428 moves prompts to files and renames
  the lines with it.

## Refute-First Findings

One fresh-context reviewer was asked to prove the change wrong, with the
ledger decode, the role source's return, and the driver's observed fields as
the boundaries.

Confirmed and fixed:

- An open item kept the reason it opened with across a restart (the keyed
  item above).
- Load validation compared stored independence entries with today's writing
  roles and a stored role with today's site registry, which a later registry
  edit would turn into a permanently disabled ledger (shape-only validation
  above).
- Writer lineage came from the full role resolution, so a writer whose
  identity was disabled or whose generation was marked recorded `unknown`
  where the lineage was known (tree-only read above).
- Observed identifiers refused control characters but passed format
  characters such as a bidirectional override. They are now limited to
  graphic characters.
- A compiled daemon binary had been committed with an earlier commit of the
  branch; it was removed before the branch was pushed.

Missed before review, and moved into the unit:

- The real-run harness gave the publication author's prompt file to preflight
  and the daemon but not to `auth adopt`, so adoption wrote no line for a role
  the run had switched on and the new preflight check stopped every run with
  judgments enabled. The adoption helper
  (`scripts/real-work-agent-tree.sh`) now passes the file under the harness's
  own condition. The harness scripts were outside the unit's declared paths;
  the flag and its one consumer move together, so the scope grew.

Disproved by a check:

- Admission bypass: no path in `Client.Call` reaches the reservation, the
  driver, or the credential without the admission succeeding. Twelve edits to
  what the source returns each produce no driver request and no ledger record
  (`TestClientAdmitsWhatTheSourceReturns`).
- A refusal leaking the credential or its detail: the result's reason is
  fixed, the ledger is untouched, and the health reason carries no credential
  bytes.
- Identity forged across the migration: a version 1 or 2 file with any
  identity is refused, as is an unknown version.
- Item-id confusion between roles: no role name holds a `-`, and converging
  one role keeps every other role's item.
- Duplicate items, a write per call, and a race on the reporter's cache: the
  mutex covers the converge, the write transaction re-reads, and a test
  asserts the store revision is unchanged by a repeated report.
- A cancelled call opening a blocking item: the health write shares the
  cancelled context and fails, so nothing is cached.
- A regression in the driver's prompts: the six instruction literals are
  byte-identical to the ones the driver held before.

Allowed by decision:

- The producer label no longer carries the judgment configuration digest
  (planned; see the limits above).
- On load, the requested model id, enrollment id, and harness build are
  checked only as non-empty. The daemon alone writes the file, the admission
  that produced them validated them, and nothing renders or acts on them.
- Preflight fails on any unbound judgment role, including the advisory ones
  (planned: the fix is one patch, and a run that starts without the line
  cannot recover it mid-run).
- A disabled ledger sends every site to its fallback with the role still
  reported resolved. That is the ledger's behavior before this unit and has
  no health signal of its own; it is outside this unit. Follow-up: #1943.

## Revisit When

- #1424 lands the call launch as a conformance-suite capability: the hand
  audit record and its literal digest go away.
- #1426 reads the credential from the line's enrollment: `credential_source`
  gains its second value and the interim limit above ends.
- #1428 moves judgment prompts to files: the code-owned prompt identities and
  the author's role-named prompt are replaced.
- A second ledger reader appears that acts on a record's identity: the
  optional-identity limit needs a written-at marker first.
