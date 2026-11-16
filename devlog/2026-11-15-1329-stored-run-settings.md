# Run Settings Are Stored As Section Revisions Re-Checked On Load

Issue #2082, unit 1 of tracker #3201. A `kind:contract` unit (a migration
that adds two tables, and shared domain types) and Tier A: the
records hold the approved recipe, the path allowlist, the publication
repository, and the container tool path a run uses. The direction is
`devlog/2026-11-14-1114-one-daemon-every-project.md`: the installed daemon
runs every project, so it needs settings that live in its store, not in
one deputy root's `settings.json`.

## Owner Decisions

- **Chose one PR for the whole unit over the planned three-way split**
  (2026-11-15). The plan comment proposed records, then commands, then
  the loader and import, because the unit ran about 2,600 lines against a
  1,500-line budget. The owner kept it whole; the commits follow the
  split's order so each part reviews on its own.
- **Kept the three planning choices the issue offered for rejection**
  (2026-11-15):
  - **One revision row per section change over one whole-record document
    per revision.** It copies the Workflow settings model (#3145): an
    operator changes one section at a time, each row names who changed
    what, and a fold is a scan. Rejected: a whole document per revision,
    which makes every set rewrite every section and hides which one
    changed.
  - **Only the container tool gets a stored absolute path; git and
    `tailscale` stay on `PATH`.** The container tool is the one the
    installed daemon's launchd `PATH` is known not to hold. Revisit when
    #2083 finds git or `tailscale` missing under launchd.
  - **A settings set doesn't create the project's `projects` row.** Label
    intake and admission keep writing that binding. A `repository` set and
    the loader check against the row when one exists.

## Decisions

- **These tables sit apart from the Workflow settings tables (#3145).**
  Those are Tier B and sync to clients. These carry authority, so they
  get the Tier A bar (a re-gate on read, tamper-style stale tests) without
  pulling a synchronized Tier B table under it, and nothing here syncs.
- **The loader re-checks every section on every load and fails closed.**
  A stored value was written under the policy of its day. The loader
  re-runs the domain rules and the settings gate (`ExplicitAllowedPaths`,
  the transport's repository, branch, and label forms, the recipe digest,
  the review auth mode, the build inputs) and the repository's binding,
  and returns no settings on any failure. A set runs the same gate, so a
  value that would fail at load is refused when it is written.
- **The deputy and the store compose the driver through one function.**
  `NewClaudeDriverConfig` takes `RunSettings` plus a launch's inputs; the
  deputy maps its file to `RunSettings`. Chose passing the container tool
  in over the plan's `exec.LookPath` with a bare-name fallback, because
  the deputy's configuration must not change: it keeps the bare name
  `ward.NewCLIRuntime` resolves, and the import stores an absolute path.
- **One `/settings` control route carries every verb**, as
  `/routing-policy` does, over a route per verb. The forwarded args name
  the verb, so the running daemon runs the same dispatch.
- **Every set says when it takes effect**, not only a set of the
  allowlist, recipe, or repository. Nothing reads the records live until
  #3209, so the note is true of every section.
- **The store scans cross-check each row's columns against its body.**
  Section, set_by, and set_at are stored both ways; a row whose two
  copies disagree is refused (`errRowInconsistent`).

## Refute-First Pass

Recorded per `docs/agent-workflow.md` §refute-first.

- **The deputy's configuration is unchanged.** Disproved a change by
  check: a pinned test (`TestDeputyClaudeDriverConfigForTheFixture`),
  written before the refactor, covers the fixture and every optional key;
  a scratch comparison ran the old and new compositions over 50,000
  generated inputs, including nil versus empty slices, a judgment with an
  empty `claude_bin`, an empty label, and a zero macOS port base, and
  found no difference.
- **A fresh-context reviewer tried to prove the branch wrong.** Its
  outcomes:
  - Confirmed and fixed: the new tables broke the stored-row walk's
    coverage check and an integration test's migration rewind. Both now
    cover the tables.
  - Confirmed and fixed: a section object could leave out a key and store
    its zero value, so `{}` for `build` turned rebuilds off. A section
    value now must hold every key its shape names, as the deputy file
    must; only `macos_verification.port_base` stays optional.
  - Confirmed and fixed: an import forwarded to a running daemon looked
    the container tool up on the daemon's launchd `PATH`, the one known
    not to hold it. The command now resolves it where the operator runs
    it and passes the path on.
  - Allowed by decision: the gate refuses a `refs/`-prefixed base ref the
    deputy file accepts. It fails closed; the operator imports a file
    that names the branch.
  - Disproved by check: a gate that skips a set section, a decode that
    accepts trailing data or duplicate or case-folded duplicate keys, a
    clear (`-clear`, `null`, or padded `null`) that removes a required
    section, a fold that tolerates gaps or another project's rows, and an
    import that writes outside one transaction.
