# Give Recipe Commands the Candidate's Git History

Work unit: #3154. Consumer: #3121.

A recipe command may now ask for the candidate's git history with
`"history": true`. The verifier writes its own shallow `.git` into that
command's workspace after the exact-tree check, and exports
`FREESIDE_VERIFY_BASE_SHA` and `FREESIDE_VERIFY_HEAD_SHA`. A command that
doesn't ask keeps today's workspace and environment exactly. Mechanism lives
in `daemon/README.md` and `daemon/internal/verify/history.go`.

The owner decided on 2026-11-12, on #3121, that the verifier supplies the
history so Freeside's recipe runs the same `scripts/check-commit-messages.sh`
and `scripts/check-migration-history.sh` that `required` runs. The daemon
doesn't copy their rules into Go.

## Decisions

- **Chose a verifier-written shallow repository over copying full
  history.** The importer writes each candidate commit with one parent, so
  the commits in base..head, their boundary commits listed in `shallow`, and
  every object in those commits' trees are enough for both scripts: their
  own `git merge-base` returns the base, which is a boundary commit. A full
  copy would cost the whole repository per command and carry objects from
  the daemon checkout no check needs. The object list comes from each
  commit's own tree (`rev-list --objects --no-walk`), not a walk that
  excludes the base, because such a walk drops a blob a candidate commit
  restores from history older than the base. The `.git` holds only `HEAD`,
  a fixed `config`, `shallow`, an empty `refs/`, and one pack; nothing comes
  from the candidate, so no hook, attribute, or config can reach git in the
  room from there.
- **Chose the merge base over the moved tip as the exported base.** On a
  base-advance re-entry the enforced base is the moved tip. Exporting it
  would put every mainline commit since the head's own base into the range,
  and a shallow repository without them would make the scripts'
  `git merge-base` fail. The merge base keeps the range to the candidate's
  commits. Verification fails closed when the head and base have no merge
  base or more than one. The head is always the candidate head, never the
  prospective merge, so the commit-message check sees the candidate's own
  commits while the workspace's files are still the merge's tree.
- **Set `safe.directory=*` through `GIT_CONFIG_COUNT`.** The image room may
  run the command as a uid other than the workspace's owner, and git refuses
  a repository another user owns. git reads `safe.directory` only from
  protected config; command-line config, which `GIT_CONFIG_COUNT` entries
  count as, is protected. The value is `*` because the verifier doesn't know
  the in-room path. The only repository the verifier writes is its own, and
  candidate code already runs in the room (the named §5.6 residual).
- **Rejected history in the macOS room for now.** The root-owned slot helper
  passes exactly `HOME`, `TMPDIR`, `PATH`, and `LC_ALL` and refuses anything
  else. Passing the SHAs would change the helper and require reinstalling it
  on every executor. Both scripts are bash and git, so they run in the image
  room; the parser refuses `"history": true` with `macos_host`.
- **Rejected checking messages in Go.** That is the option the owner
  decision rules out: a second copy of the rules would drift from the script
  `required` runs.
- **Kept `verify.Room` unchanged.** A new optional `EnvRoom` interface
  carries the extra environment, so `macverify.Room` and the engine's
  `hostSlotsRoom` don't change. A history command whose room isn't an
  `EnvRoom` fails closed before any workspace is built.
- **The image proof skips history commands.** The proof runs each command
  in a copy of one commit with no candidate, so it has no base and head to
  give one. Verification is where a history command first runs.

## Known Limits

- **A merge inside the candidate's range can hide part of the base-merge
  rule.** A merge whose other parent is an ancestor of the merge base leaves
  that parent as a shallow boundary, so the commit-message check's
  `merge-base --is-ancestor` can't see it's in the base. Daemon-built heads
  are linear (only the prospective merge has two parents, and it is never
  the exported head), but a `head_changed` re-entry verifies a head someone
  pushed, which can hold merges. So it is reachable, though rare. Merging a
  commit at or after the merge base is still caught, since that commit is
  the exported base.
- **Ownership in the image room is shown only by a live run.** Unit tests
  run as one uid. A wrong assumption about `safe.directory` would show up as
  `detected dubious ownership` in #3121's first verification.

## Refute-First Findings

A fresh-context reviewer tried to prove the change wrong before commit. It
found no blocker.

- **Confirmed, fixed: `pack-objects` wrote its temporary pack into the
  daemon checkout.** Given a destination, it builds `tmp_pack_*` in the
  checkout's object directory and moves it into scratch, which leaves debris
  on a crash and fails across volumes. The verifier now streams
  `pack-objects --stdout` into scratch and runs `index-pack` there.
- **Confirmed, deferred to #3166: the engine retries
  `ErrHistoryUnavailable`.** Neither the clean-verification nor the re-entry
  classifier names it, so a candidate with more than one merge base is
  retried. No recipe asks for history until #3121, and the engine is outside
  this unit's paths. In scope, the `.git`-path refusal now also wraps
  `ErrMalformedTree`, which the engine already stops on; no merge base
  already wraps `ErrGitPlumbing`.
- **Confirmed, fixed: Known Limits called a merge in the range
  unreachable.** A pushed head on re-entry can carry one; the limit above
  now says so.
- **Disproved by a check:** the written repository works for `log`,
  `merge-base`, `diff`, and `fsck` with a restored old blob, an empty range,
  and a moved tip; the candidate reaches no host git argument (rev-list
  lines are validated as SHAs) and no git config; every `.git` alias in the
  tree is refused, gitlinks included, before install; extra environment
  can't become a `container run` flag (keys match `[A-Z_][A-Z0-9_]*`, go
  before `--`, and refuse NUL, CR, LF, duplicates, `HOME`, and `LC_ALL`);
  commands without history see no change; old reports stay canonical
  (`omitempty`); and the image proof is the only `ProofCommands` caller.
- **Allowed by decision:** the preparation command sees the installed
  `.git` without the extra environment. It is image-baked, runs no git, and
  comes from the trusted recipe. Git older than about 2.38 may ignore
  `safe.directory` from `GIT_CONFIG_COUNT`; the ownership limit above covers
  it.

Revisit when the importer writes a candidate commit with more than one
parent, or when a macOS check needs history.
