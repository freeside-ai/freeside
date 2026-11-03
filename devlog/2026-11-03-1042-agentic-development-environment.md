# Call Freeside an Agentic Development Environment (Plan Revision 108)

Chose "an agentic development environment" (ADE) as Freeside's category line
over keeping "an agent control plane" and over coining a new term, because a
person using Freeside no longer spends the day editing code: they direct
coding agents, approve specifications, and judge results, and Freeside's app
is where that happens. The owner decided this on 2026-11-03 in chat; #2507's
work contract records it. "Control plane" stays, as the architectural term
for the daemon and its policy, never as the category.

## Why

- **"Agent control plane" names the architecture, not the product.** It
  describes what decides versus what does the work, not the place a person
  spends the day.
- **The old label no longer differentiated.** The 2026-09-08 refocus found
  other tools using the same phrase.
- **ADE is a category people already search for.** Warp coined it in June
  2025 for tools built around coding agents as first-class participants.
  Freeside adopts the category and differs inside it: the environment keeps
  working while you're away, and work counts as ready only on evidence, not on
  your watching it. The text claims that combination, never a first, as the
  2026-09-08 note requires.

## Superseded Decisions

- **Brand register, plan revision 10**
  (`devlog/2026-07-17-0050-brand-register.md`) fixed the category line as "an
  agent control plane" and kept it undecorated. The phrase now neither names
  the product people use nor distinguishes it, so the category line changes. The tagline, casing rule, visual register, and subsystem names
  stay.
- **Presentation refocus** (`devlog/2026-09-08-1546-presentation-refocus.md`)
  stopped leading outward-facing text with the label but kept it in plan §1
  and AGENTS.md as the technical category. Its assumption was that the
  category label and the architecture term were the same thing. They are now
  split: the human-facing category is ADE on every surface, and "control
  plane" is the architecture. The refocus's ordering holds: the README still
  opens with the workflow and the safeguards, and names the category after
  them.

## Rejected

- **Keep "agent control plane".** For the two reasons above.
- **Coin a term, such as "agent supervisor" or "Agent Management
  Interface".** Nobody searches for a coined term, and AMI already means
  Amazon Machine Image.

## Risks

- **Warp's definition centers on an interactive terminal**, a tool you sit
  at. Freeside's differentiator is the opposite, so a reader who knows only
  Warp's sense may misread the category. The README states the
  differentiator beside the category for that reason.
- **An early AGPL project is named ADE.** A search for the acronym alone can
  land there. Surfaces spell out the term at first use.

## Revisit When

- The ADE category comes to mean an interactive tool only, so that claiming
  it misdescribes an environment that runs without you.
- A second operator finds the term misleading in practice, for example
  expecting a tool they must sit at.
