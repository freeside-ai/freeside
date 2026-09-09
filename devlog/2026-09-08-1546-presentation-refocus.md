# Presentation Refocus: Lead With the Guarantees, Not the Category

Two landscape reports the owner commissioned on 2026-09-08 (kept outside
the repository) compared Freeside with Warren, Agent Orchestrator,
Symphony, Maestro, T3 Code, the vendor desktop apps, GitHub Agentic
Workflows, and others. This note records what the owner decided to change
in how Freeside presents itself, and what was deliberately left alone.

## Decisions

- **Chose to lead outward-facing text with the guarantees over the
  "agent control plane" label** because T3 Code, HumanLayer, and others
  now use the same phrase, so it no longer says what is different. The
  text explains what those safeguards do: the agent cannot declare its own
  work ready or publish it; the daemon keeps GitHub credentials outside the
  agent's workspace and publishes the reviewed work; verification and review
  cover the code being accepted; and decisions survive retries and restarts
  without extending an approval to changed inputs. The label stays in plan §1
  and AGENTS.md as the technical category. The README first names the local
  task-to-PR workflow,
  so readers know what the safeguards protect. The category comparison
  belongs in this note, not in the introduction.
- **Chose to keep the introduction intent-only and put reality
  elsewhere.** The README gives a brief warning that Freeside is under
  development and has rough edges. Its setup sections explain how to build
  from source and prepare real agent runs. The owner chose to keep detailed
  operating evidence and implementation progress in the pinned wave tracker,
  instead of repeating them in the README. That record can change without
  rewriting the introduction's account of the intended product.
- **Chose to describe the failure path beside the happy path** in the
  introduction, because the ten-step workflow did not show how Freeside
  handles failed checks, revised work, or a restart. These examples explain
  why the safeguards matter in use.
- **Chose to say what Freeside is for, and rejected the reports' line that
  small fixes belong in the harness.** Both reports placed small fixes and
  exploratory work outside Freeside's fit. The owner disagreed: size is not
  the test. Small fixes can have consequences that warrant specification,
  checks, and review too. The introduction describes the operator's role
  alongside the agent's work.
  It makes no claim that small and large changes benefit equally. Unattended
  execution still includes human oversight, specification approval, and final
  review.
- **Owner chose not to run a net-leverage measurement now.** The reports
  proposed matched-task comparisons against vendor apps or a lighter
  supervisor. With one operator who is also the person repairing the
  controller, and the loop not yet past its Wave 7 exit, that would
  measure noise. The §8 attention telemetry keeps recording; the study is
  deferred, not rejected.

## Rejected

- **Any uniqueness claim.** Report 1 refuted every broad version
  (publication authority outside the agent, independent review, durable
  approvals) with at least one neighbor. The text claims a combination,
  never a first.
- **Renaming or re-branding.** The register and tagline are unchanged.
- **Pulling onboarding, the pi adapter, or the drift audit forward on the
  strength of the reports.** Each already sits in Wave 8 or 9 with the
  scope the reports endorse; a sequencing change is a wave-planning
  decision, not a presentation one.

## Revisit When

- A second repository with a different task shape, or a second operator,
  exists. Reconsider whether that experience supports a useful comparison.
- The 1B exit evaluation (Wave 10) is planned; it should decide what
  "useful work per unit of attention" is measured against.
- Installation or operation becomes simple enough that the README's warning
  about rough edges no longer helps a new user.
