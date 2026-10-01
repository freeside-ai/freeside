# Role-Name Lineup Keys and the Wardless Admission Class

Work unit #1421, plan §5.4 "Roles and Launch Shapes" and "Admission", §5.13.
Plan revision 65 decided the model (`2026-09-19-1105-judgment-roles-join-lineups.md`);
this note records the encoding and spellings this unit chose for it. The
agent chose them while executing the issue's implementation plan. They become
durable when #867 emits the first real lineup and #1600 keys task lines by
role, so the owner can change them cheaply before this merges and not after.

## Role Spellings

The closed role enum (`RoleName`) spells the thirteen roles of plan revision
65 in snake case: `specifier`, `implementer`, `remediator`, `reviewer`,
`shadow_reviewer`, `diagnostic`, `task_namer`, `publication_author`,
`finding_classifier`, `finding_adjudicator`, `drift_auditor`,
`attention_discussion`, and `briefer`.

- **Chose snake case over kebab case** because every other domain enum and
  every judgment site id already uses it. Four roles then share a spelling
  with their one site (`task_namer`, `finding_classifier`,
  `finding_adjudicator`, `attention_discussion`).
- **Chose `diagnostic` over the site's `execution_diagnostic`.** A role is
  not its site: the plan names the role "diagnostic", and the publication
  author already shows a role spanning two sites.
- **`verification` is not a member.** It is an engine job (plan §5.13).
- **`researcher` is not a member yet.** Plan revision 76 added it; #1656
  adds it to the enum. Until then a `researcher` key fails closed like any
  unknown role. This is the issue's stated non-goal, not an omission.

A role's site ids are plain strings in `domain`, because `domain` cannot
import `inference`. A test in `inference` reads that package's `*SiteID`
constants from source and fails when the two lists disagree, so a site added
without a role is caught.

## Key Encoding

- **One key per line: `lineup.role.<role>`.** Its value names both halves,
  `<agent-name>@<agent-digest>/<prompt-name>@<prompt-digest>`. Rejected:
  separate agent and prompt keys. Policy resolution overrides key by key, so
  two keys would let a project lineup override half a line and pair a
  deployment prompt with a project agent that nobody reviewed together.
- **`/` separates the halves** because neither an agent name, a prompt name,
  nor a digest can contain it, so the value splits one way only. Prompt names
  follow the agent-name rule.
- **A shadow line is `lineup.role.<role>.shadow.<shadow-name>`**, with the
  same value format. Role names carry no `.`, so the key parses one way only.
  The shadow name exists so a later plan revision can allow several shadow
  lines without changing a key that already exists (owner decision,
  2026-09-19: one shadow line is a cost limit for now). The validator accepts
  at most one per wardless role and none on a ward role.
- **Consequence of the named shadow key.** A project lineup overrides a
  deployment shadow line only by reusing its shadow name. A different name
  resolves to two shadow lines on one role and fails closed. That is the
  safe direction, and `policy/README.md` tells operators.
- **Stage names stopped being lineup keys.** No stage-named key was live:
  only tests called the lineup functions. `CanonicalStageRole` is unchanged,
  because it reads persisted stage names and has nothing to do with roles.

## Wardless Admission

`AdmitWardlessRole` is a pure function over records the caller has already
rebuilt, like the stage admission helpers. Decisions a reader may question:

- **The result carries no model or effort.** A stage's
  `AdmissionAgentBinding` records them since #1615. The issue contract keeps
  them out of `WardlessAdmission`; a consumer derives them from the admitted
  agent with `DeriveAgentLaunchSelection`. Adding them later changes the
  golden.
- **The result carries no "selected by" field.** It records the lineup
  revision. Whether the lineup or an alternate-agent card chose the agent
  arrives with the card (#869).
- **The interim proof is an input, not a constant.** `InterimCallLaunchAudit`
  names an adapter digest, a harness build, and an audit date. Admission
  accepts it only for a `claude_code` adapter whose digest and harness build
  both match, so another adapter build, another harness build, or another
  client kind fails closed with `ErrCallLaunchUnproved`. The audited build is
  deployment configuration (`-judgment-claude-bin`,
  `-judgment-claude-sha256`), so the domain cannot hard-code it. #1425
  supplies the value when it wires the sites, and #1424 replaces the input
  with the adapter's conformance record. The result's field is named
  `interim_launch_audit` so #1424 adds its own field beside it instead of
  changing this one's meaning.
- **The proof's provenance is the caller's duty.** Admission checks that a
  proof covers the adapter, not that the audit happened. A caller that built
  the proof from the adapter under admission would admit any build. The
  domain cannot close this: the trusted record is deployment configuration
  it cannot see. The input's contract says the proof comes from the
  deployment's audit record, and #1425 is where that is wired and tested.
  Review raised this; no caller exists yet, so the rule is stated, not
  enforced.
- **"Current generation" stays the store's fact.** The class checks that the
  generation is persisted, belongs to the agent's enrollment, and covers the
  deadline plus margin. Whether it is retired, revoked, or marked by the
  integrity probe is a store read the caller makes.
- **Nothing compares lineage groups** in lineup validation or admission.
  Independence is recorded, never gated (plan revision 65), and the two
  `LineageGroup` comments that said otherwise are corrected.

## Refutation Pass

An independent reviewer tried to break the diff before the first commit.

- **Confirmed and fixed: the join was not re-run.** An agent whose effort
  the offer does not allow, or the adapter cannot send, was admitted, and so
  was an agent whose route digest matched no supplied route. Admission now
  takes the route fragment and re-runs the route, effort, enrollment, and
  client-kind legs, as the stage recheck does.
- **Confirmed and reworded: "admits only the audited build".** The class
  admits an adapter under an audit record naming that adapter's digest and
  harness build. It cannot know which build was audited, and it compares the
  authored harness build, never the executable's SHA-256.
  `daemon/README.md` now says that.
- **Declined: checking the line's names at admission.** Names are tree-level
  and outside every digest; the digests are the binding, and a name that
  moved resolves to a different digest and fails the match.
- **Declined: rejecting a future audit date.** The domain has no clock, and
  the date is a record, not a gate.
- **Could not break:** the key and value parsers (empty segments, extra
  separators, case, trailing bytes), the ward-role refusal, and the proof
  pointer aliasing into the result.

## The Hand Audit's Record

The issue left one owner input open: which Claude CLI build the hand audit
covered, and when. The only record found is in
`2026-09-09-2145-subscription-judgments.md`: a localhost synthetic-provider
probe of Claude CLI 2.1.267 on 2026-09-09 that observed one model request
with an empty tool list. `daemon/README.md` cites that as the recorded
basis. It records a version and no executable SHA-256, and nothing records
whether the deployed `-judgment-claude-sha256` pin is that build. The owner
confirms or corrects this before #1425 supplies the audit value to a site.

Revisit when #1424 lands the call-launch conformance record (the interim
audit input goes away), when a plan revision allows more than one shadow
line per role, or when a consumer needs the admitted model and effort in the
snapshot.
