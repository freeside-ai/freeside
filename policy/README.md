# policy

Per-project policy configuration: initiators, review policy, gates, budgets, security mode, telemetry (see `docs/plan.md` §5.12). The Phase 1 workflow is a Go state machine in `daemon/`; YAML here is policy only, never a pipeline DSL (a DSL waits for three genuinely different workflow shapes).

This directory is **control-plane** content: the daemon loads it only from an approved default-branch commit, running stages snapshot its digests, and workspace copies are data (see `docs/plan.md` §5.8). It holds the policy for work *on Freeside itself*, which becomes a managed repo only as the bootstrap test after the deliberately boring first repository proves the path (plan §11); a consumed repo's policy lives in that repo.

- **Toolchain:** YAML (policy values interpreted by the daemon's code-defined state machines).
- **Scope boundary:** policy configuration only. Changes here are control-plane changes: gated, reviewed like code, never batched silently into feature PRs.
- **Status:** layout and file formats fixed (below); no files until an operator commits the baseline patch `freesided auth adopt` prints.

## Agents, Fragments, and Lineups

The admitted-agent tree (`docs/plan.md` §5.4, revision 39; the contract types live in `daemon/internal/domain`). Every document here is operator-authored configuration, reviewed as a diff; the daemon never writes this tree. Enrollments, generations, and admissions are facts and live in the daemon's store, never here.

Reserved layout:

```text
policy/
  agents/
    <agent-name>            # one four-line agent document (who/through/running/asking)
    <agent-name>.attended   # unattended-eligible mark: exact agent + launch digests
  fragments/
    routes/<route-name>     # service operator, protocol, authorities, billing, terms basis
    adapters/<adapter-name> # Freeside adapter build, pinned harness build, capabilities
    offers/<route-name>/<offer-name>  # one route's offer of one model
  lineup                    # the deployment lineup: one line per role naming an agent and a prompt
  agents.lock               # the name-to-digest map for the tree revision
```

Binding rules, fixed by the contract:

- **Names are tree-level.** The name-to-digest map (`agents.lock`) binds each name to the digest of its canonical body; no name enters any digest, so a rename changes nothing and a content edit changes exactly the digests that consumed it.
- **An offer lives under its route.** Which route an offer is authored under is a tree fact (the directory), supplied to resolution as context, deliberately outside the offer's digest.
- **The lineup names digests.** A line maps one role to an agent and a prompt, and resolves to one key in the resolved policy: `lineup.role.<role>`, with the value `<agent-name>@<agent-digest>/<prompt-name>@<prompt-digest>`. One key per line means a project lineup overrides a whole line, never half of one. A line that named only a name could follow a tree edit nobody approved for the role.
- **Lineup keys are role names.** `<role>` is a name from the closed role list (`docs/plan.md` §5.13; `RoleName` in `daemon/internal/domain`): `specifier`, `implementer`, `remediator`, `reviewer`, `shadow_reviewer`, `diagnostic`, `task_namer`, `publication_author`, `finding_classifier`, `finding_adjudicator`, `drift_auditor`, `attention_discussion`, and `briefer`. A stage name is not a role name, and `verification` is an engine job, never a role; both are rejected, like any unknown role. A role needs a line only while policy asks for its work, and one role never borrows another role's line.
- **Judgment roles have baseline lines.** `freesided auth adopt` writes a line for every judgment role with a built site (`diagnostic`, `task_namer`, `finding_classifier`, `finding_adjudicator`, `drift_auditor`, `attention_discussion`, and, when adopt is given `-judgment-publication-author-prompt`, `publication_author`), each on the baseline call agent `claude-code-call-default`. That agent runs through its own call adapter, pinned to the Claude CLI build the call launch was audited on, because a judgment call is admitted only under an audit that names its adapter's build. Each line names the role's prompt: a code-owned versioned name for every role but the publication author, whose prompt is the operator's file by content digest. A deployment adopted before these lines existed adds them by patch: run `freesided auth adopt` again for the baseline, take the call agent, its adapter fragment, the lineup lines, and the lock entries from the patch it prints, and commit them. Until a role's line exists its sites return their fail-safe and the daemon raises a `system_health` item naming the role (`daemon/README.md`).
- **A wardless role may carry one shadow line.** Its key is `lineup.role.<role>.shadow.<shadow-name>`, with the same value format. A second shadow line on one role and a shadow line on a ward role are rejected. A shadow line is never a fallback for a missing primary line. A project lineup that overrides a deployment shadow line reuses its shadow name, because a different name is a second shadow line.
- **The attended mark rides beside the agent, outside the hashed body**, naming the exact agent and launch digests it was given for; a line edit is a different agent, so the mark does not carry.

## File Formats

Every file is text that ends with one newline, with no comments and no blank lines. The daemon reads the tree from one exact commit, never the working tree, and refuses the whole revision when any file is outside the layout, does not parse, is larger than 64 KiB, or is not a plain file. `daemon/internal/agenttree` is the reader and the renderer.

- **Agent document** (`agents/<agent-name>`): exactly four lines, in this order. Fields are separated by spaces or tabs, so column alignment is free.

  ```text
  who      enrollment  <enrollment-id>
  through  route       <route-name>
  running  adapter     <adapter-name>
  asking   offer       <offer-name>, effort <effort>
  ```

- **Attended mark** (`agents/<agent-name>.attended`): one `<agent-digest> <launch-digest>` line for each launch the agent may run unattended. A line that names an earlier digest of the agent is not an error; it no longer applies. A mark file for an agent the tree lacks is refused.
- **Fragments** (`fragments/...`): the fragment's JSON encoding from `daemon/internal/domain` on one line, including its `digest`. Unknown fields and a digest of other content are refused.
- **Lineup** (`lineup`): one `<key> <value>` line for each line, where `<key>` is the part of the policy key after `lineup.role.` (`reviewer`, or `briefer.shadow.<shadow-name>`) and `<value>` is the selection (`<agent-name>@<agent-digest>/<prompt-name>@<prompt-digest>`).
- **Name-to-digest map** (`agents.lock`): one `<kind> <name> <digest>` line for each agent and fragment, sorted by kind and then name. The kinds are `adapter`, `agent`, `offer`, and `route`; an offer's name is `<route-name>/<offer-name>`. The map must equal the digests the content resolves to: a missing entry, an extra entry, or another digest refuses the revision, so an edit to a fragment always shows up in review as a changed digest for every agent that consumes it.

Names: an agent name is lowercase ASCII letters and digits with `-` and `_` inside, at most 246 bytes. A fragment name also allows `.` inside (`gpt-5.6-sol`), at most 255 bytes.
