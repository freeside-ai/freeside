# Run Every Pre-Spend Check In One Command (#2222)

Work unit: #2222. Scope: `daemon/cmd/freesided`, `daemon/README.md`,
`docs/production-walkthrough.md`, `.claude/skills/exit-run`, and
`devlog/`. The command takes the rig lease, mints an App token through
composition preflight, and writes a lease token beside the database, so
this is credential and destructive-path work and the note is mandatory.

## Decisions

**Ship without the live target-issue read, at the plan's split seam, over
a janitor pass or a local janitor stand-in.** The plan read the issue
through `publish.NewIssueReadTokenSource` over a minter built as
composition preflight builds its own, with no janitor. The refute-first
pass found that this minter fails closed for every resolving source: the
janitor is the only thing that tells it a durable installation is still
trusted. A janitor pass would write janitor state and can remove
installations; a stand-in that answers from the durable store would skip
the live observation the janitor exists to make. Composition preflight's
`AuthorityInspectionTokenSource` mints from the durable binding without a
janitor, but only `contents: read`, which can't read issues. The fix is an
issue-read sibling of that source in `daemon/internal/publish`, outside
this unit's scope, and the unit was already past its size estimate.
Follow-up: #2269.

**Reuse `inspectCompositionIdentity` for the input checks over extracting
`validateSubmitInputs` from `submit.go`.** Preflight's per-submission
identity check already applies submit's refusals before the store opens
(path boundary, egress, allowlist, work unit, publication). Extracting a
second copy would have left two validators to drift. Prelaunch adds the
two refusals it doesn't make: the launch sizes and specification policy
(`checkSubmittedPolicy`), and, against the store and the composed
manifest, `refuseUnfinishedDuplicate` and `validateLaunchComposition`,
called as submit calls them. `refuseUnfinishedDuplicate` now takes a
`ReadTx`, which a `WriteTx` embeds, so prelaunch runs it read-only.

**Walk the prerequisites from the store, over keying on manifest evidence
strings.** The plan derived `first_missing` from `trust_profile` and
`implementer_image` evidence text, which a rewording in `preflight.go`
would break silently. Prelaunch reads the trust profile and the project
image's provenance directly, in the same read-only transaction as the
identity check, and records whether each read was reached, so a failed
read is never reported as a missing prerequisite. The agent-tree step
still uses the manifest's `agent_selection` result, since that is where
the tree binds the settings' lineup.

**Hold the rig lease for every check that needs it, release it with the
deputy's proof, and abandon it when the proof fails.** Prelaunch never
recovers a stale lease, its own root's or another's; the fix names the
deputy start or `freesided rig recover`. A rig it can't prove clear is
abandoned, as the deputy's release does, so the next start recovers it.
The lease token lives in `prelaunch-rig-acquisition.json`, never the
deputy's `rig-acquisition.json`, and is removed on every path.

**Compose in memory and never write `composition-manifest.json`.** A
manifest beside the database binds submissions, so only a deputy start
writes one. `manifest_digest` is the digest of the bytes the deputy would
write, and a test pins it equal with and without `-no-live`.

## Refute-First Pass

A fresh-context reviewer tried to prove the change wrong.

- **Confirmed, split out:** the issue reader's minter fails closed without
  a janitor (the first decision above).
- **Confirmed, fixed:** prelaunch did not compare the publication's
  `commit_author` with the App bot the launch composition resolves, and
  did not apply submit's duplicate-source refusal. Both are now checks
  (`launch_binding`, `duplicate_source`), each with a test.
- **Confirmed, fixed:** a failed trust-profile read was reported as a
  missing trust profile.
- **Confirmed, fixed:** a release failure overwrote a token-write
  failure's evidence; both are now kept.
- **Confirmed, fixed:** no test covered the abandon path or pinned that a
  stale lease is left in place. Both are tested.
- **Moot with the split:** the issue GET followed redirects, and the
  issue's `state` string was not validated. #2269 carries both.
- **Confirmed, fixed (second pass, on the fixes):** the walk read every
  failed `agent_selection` check as a missing tree, including a store read
  failure and a reviewer-provider mismatch. It now keys on that check's
  unresolved-lineup fix, a constant `preflight.go` shares; a local file
  error in prelaunch's own tree check no longer names the tree missing
  either.
- **Declined (second pass):** re-reading the policy without comparing its
  digest, and an out-of-allowlist path failing `submission` rather than
  `launch_binding`. The first needs the operator to edit the file mid-run,
  and `submit` re-checks everything; the second is the right refusal under
  a broader label.
- **Disproved:** a token leak through the record or stderr; a lease left
  held on any path; a lease token left behind; stale-lease recovery; a
  deadlock between the daemon lock and the rig lease; a migration through
  the read-only open; a behavior change in the deputy's extracted helpers;
  a check missing from the record on any early-exit path.

Revisit when the deputy's start gains a check that spends nothing and
prelaunch doesn't run, or when `inspectCompositionIdentity` stops applying
one of submit's pre-store refusals.
