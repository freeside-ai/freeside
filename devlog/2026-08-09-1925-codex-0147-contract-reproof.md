# Codex 0.147.0 Contract Re-Proof and the Code-Mode Host

Work unit: #623 (PR #626). Scope: `images/agent-codex/`, this note.
Successor in the #395/#401 lineage: those notes' "Revisit when: the
pinned CLI version is bumped (re-prove the contracts under Pinning)"
condition fired when #623 moved the pin 0.137.0 → 0.147.0, and PR
#626's review (finding 3745490848, P1) correctly flagged that the PR
carried only artifact-layout verification. The owner directed a full
re-proof rather than a deferral. This note records the results; the
probe methodology is #401's (canary workspaces, access-token-only auth
snapshot, CONNECT-logging/allowlisting proxy, `codex exec --json
--skip-git-repo-check`, stdin `/dev/null`), run against the built
0.147.0 package-artifact image under Apple `container`, not against
host binaries.

## Decision

**Ship `bin/codex-code-mode-host` in the agent base.** The #623 plan
said "do not ship initially" with an explicit contingency: "If a
reviewer or a runtime probe shows the CLI hard-requires an excluded
piece, ship it and record the change of assumption." The probe below
tripped that clause, and the owner approved shipping (2026-08-09).
Changed assumption, recorded: on 0.147.0 the code-mode host is not a
code-mode convenience; the CLI's **default tool surface** offers the
model a code-mode `exec` tool (`tools.exec_command`, JavaScript call
form) that it prefers for composite shell commands, and that tool
spawns `/usr/local/bin/codex-code-mode-host`. Without the executable
every such call fails (`failed to spawn code-mode host ... No such
file or directory`); `-c features.code_mode_host=false` does not
remove the tool from the surface (calls then fail "code-mode host is
disabled"). Aggravator, observed twice: after the failed calls the
model **reported success it never had** ("DONE" with no file
written), which is disqualifying for a review substrate. Simple
single commands route through the classic shell path and still work,
which is why the build's `--version` gate saw nothing.

## Re-Proven on 0.147.0 (Fixed Image, All Matching #395/#401)

- **JSONL vocabulary and exit codes**: `thread.started`,
  `turn.started`, `item.completed` with `agent_message`,
  `turn.completed` carrying usage; exit 0 on success. With the host
  absent, 0.147.0 additionally emitted a per-thread-start
  `item.completed` error item about code mode (0.146.0 does not; the
  production-proven 0.146.0 image was re-probed for the comparison).
  Moot with the host shipped (0 error items), and benign regardless:
  the daemon never parses the event stream (opaque evidence blob,
  `ward/codex_review_collection.go`), and the message matches no
  `classifyCodexTerminalFailure` bucket.
- **Skill discovery set** (gate 2): workspace
  `.agents/skills/*/SKILL.md` discovered in git and non-git
  workspaces and from a subdirectory via ancestor walk;
  `$HOME/.agents/skills` still read; `-c project_doc_max_bytes=0`
  severs workspace `AGENTS.md` but never skills; `skills=[]` parses
  and does not sever; no severance switch among the 106 feature flags
  (`skill_search` stable is discovery; `skills.enabled`,
  `skills.paths`, `skills_dir`, `features.skills` all rejected under
  `--strict-config`). Ward's shadow-overlay obligation is unchanged.
  New discovery-set member, observed: the CLI materializes system
  skills under `$CODEX_HOME/skills/.system/` (skill-creator,
  skill-installer). Daemon-owned home, vendor-shipped content, same
  trust class as the CLI binary; no workspace-controlled surface
  added.
- **`$CODEX_HOME/AGENTS.md` append authority** (gate 4): canary
  fires; survives `-c project_doc_max_bytes=0` and
  `--ignore-user-config`; the `instructions` key still *displaces*
  the base template (-3,751 input tokens vs. baseline, mirroring
  #401's -3,307) while the home document adds without displacement.
- **`shell_environment_policy` inertness** (gate 5): under default,
  `inherit="none"`, and `exclude=["FREESIDE_*","OPENAI_API_KEY"]`,
  the child environment is identical (modulo `CODEX_THREAD_ID` and
  one incidental `OLDPWD`), with both fake markers reaching the child
  verbatim. No JWT-shaped value in any child environment; ChatGPT
  credentials stay in `auth.json`. A fake `OPENAI_API_KEY` in the
  launcher environment did not displace `auth.json` auth. Delta:
  `CODEX_SANDBOX` is no longer set on 0.147.0
  (`CODEX_SANDBOX_NETWORK_DISABLED` remains).
- **Sandbox enforcement through the code-mode path**: with the host
  shipped, `-s read-only` denies a composite-command write loudly
  ("Read-only file system", no file created); `-s workspace-write`
  writes land in the workspace.
- **Egress set**: with the proxy allowlisting `chatgpt.com` alone,
  the turn completes exit 0 while `ab.chatgpt.com` and the three
  `sdmntpr*.oaiusercontent.com` ancillaries are denied and tolerated.
  Same host set as #401, no new hosts, no refresh-host contact. The
  writer `provider_only` profile stays `chatgpt.com` alone.
- **Access-token-only snapshot**: a store with `refresh_token`
  emptied runs normal turns (every probe above ran on one).

## Not Run, Owner-Approved

**Auth refresh trigger, rotation, and persistence on 0.147.0.**
Rationale, approved 2026-08-09: the production topology never lets
the in-container CLI refresh (access-token-only snapshot, refresh
host outside the writer egress profile, both re-proven above), the
rotation semantics are server-side OAuth policy already characterized
by #401 at the cost of one revoked family, and an induced refresh on
any copy of the production store would strand a superseded refresh
token, which is #401's revocation trap. Re-prove the refresh path at
#448 (proactive refresh under the lease), which owns it. The
production credential store was not read, copied, or mounted by this
unit; probes used an operator-placed access-token-only snapshot of
the host store.

## Rejected Alternatives

- **Re-pin 0.146.0 via the package artifact**: discards the unit's
  objective; 0.146.0 would need its own re-proof, and its bundle
  layout is already unavailable upstream.
- **Sever the code-mode tool by configuration**: no working severance
  found (`features.code_mode_host=false` disables the backend but
  leaves the tool advertised, so calls still fail).

Revisit when: the pinned CLI version is next bumped (re-prove the
contracts above, including the code-mode execution path, the
discovery set with `$CODEX_HOME/skills/.system/`, and re-check for a
severance switch and for `CODEX_SANDBOX` semantics); or when #448
lands (refresh-path proof); or if a future version routes *simple*
commands through the code-mode host too, which would make the host a
single point of failure for all execution.
