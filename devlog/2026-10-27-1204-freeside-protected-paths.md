# Freeside's Protected Paths

Work unit: #2375. Plan: `docs/plan.md` §11 (Freeside follows gh-imgup,
revision 92), §3.1, §3.2, §5.8, §9. Follow-up: #2376 widens the allowed
paths to `api/` and `scripts/`.

## Decisions

**Chose an operator-authored `-protected-path` flag over new importer
defaults.** The repository-specific control-plane classes (verification
recipes, prompts and policy, egress and trust, materiality rules) have no
universal defaults, because where a project keeps them is its own choice.
A default of `scripts/check.sh` would hold an edit to that path in every
onboarded repository, where it may mean nothing. The flag adds Freeside's
paths to Freeside's profile only, and the approval digest binds them, so the
owner reviews the exact list. The CLI refuses an
unsorted or duplicate list instead of sorting it, so the review shows the
patterns the operator typed.

**Chose import-stage classes over relying on the verify-stage risk flags.**
The verify stage already flags `**/.golangci.yml`, the recipe path, and the
recipe's literal entrypoints, but a risk flag does not hold publication.
The importer's verification-recipe, prompts-and-policy, and
materiality-rules findings are non-waivable control-plane holds, and they
fire only on profile patterns. `extra_verification_control_patterns` feeds
both stages, so one list covers both.

**The list.** Plan §3.1 and §3.2 keep these operator-authored:

| Class | Patterns | Why |
| --- | --- | --- |
| `automation_control` | `.githooks/**` | The hooks run on a maintainer's machine at every commit, and the automation defaults do not cover them. |
| `verification_control` | `.freeside/**`, `daemon/.golangci.yml`, `scripts/check-commit-messages.sh`, `scripts/check-migration-history.sh`, `scripts/check-vocabulary.sh`, `scripts/check.sh`, `scripts/daemon-test-shards.json`, `scripts/daemon-test-shards.py`, `scripts/link-plan-section-refs.sh` | The recipe, the daemon lint config, and every script the recipe or the `required` check runs for `daemon/` and `docs/` changes. |
| `prompts_and_policy` | `policy/**`, `prompts/**` | AGENTS.md (Document Gating) names these as the future control-plane directories. |
| `materiality_rules` | `docs/plan.md` | Plan §9 holds the materiality rules. |

The other three classes keep their defaults, which already cover Freeside.

**Protected `scripts/` paths before `scripts/**` is allowed.** Every path
outside `daemon/**` and `docs/**` is already refused by the allowed-path
check. Protecting them now means #2376 can widen the allowed paths without
a window where an agent could edit the scripts that verify its own work.

**Chose to protect `docs/plan.md` whole over moving §9 into its own file.**
A path pattern cannot select a section, so every agent edit to the plan
becomes a control-plane hold and plan edits stay owner-authored. That
matches revision 92, which keeps "the materiality rules in this plan"
operator-authored, and plan changes are already gated by materiality. The
narrower option, a separate materiality-rules file, is a material plan
change and its own unit. Agents can still edit every other `docs/` file.

**Left out on purpose.** The other `docs/` process files
(`docs/coordination.md`, `docs/agent-workflow.md`, the ADRs) are
development process, not a §5.8 class; the human merge gate covers them.
`AGENTS.md` is already a default reviewer-instruction path, advisory by
ADR 0002. The daemon's own gate code (`daemon/internal/importer`,
`daemon/internal/verify`) stays editable: an agent's change there takes
effect only after a human merge and an operator rebuild, the hot spot §3.2
accepts.

Revisit when Freeside should edit plan wording, a new script joins the
`required` check for `daemon/` or `docs/`, or #2376 widens the allowed
paths.
