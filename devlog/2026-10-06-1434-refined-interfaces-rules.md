# Adopt The Refined-Interfaces Rules With A Specification Prerequisite

The owner accepted the 6 Oct 2026 refined-interfaces handoff
(`docs/design/handoff-2026-10-06-refined-interfaces/`) as the next
presentation pass over both clients, and took the four rule-level decisions
its survey left open. Each was a one-line yes against a treatment an earlier
round had approved, so the record is here rather than in chat.

**R2, one disclosure shape.** Chose the sentence disclosure (chevron, sans 16
Title Case label, optional mono dim trailing summary) for everything that
opens in place, over keeping the 18 Sept handoff's keyword disclosure on the
task surfaces, because the survey's all-mono grammar made a card read as a
ledger and its argument ("Reason and alternatives") read as a footnote. A
keyword now heads a section only when the content below it is a section.

**R3, in place is a disclosure, away is a link.** Chose folding `Read full
report` and `Read full message` into `Full Report` and `Full Message`
disclosures over D04's approved pill, because a pill was a third shape for an
action that stays on the card. D04's six-line bound and in-place expansion are
unchanged; only the control's shape moves.

**R5, the quote is the agent's voice.** Chose the 3pt rule on the soft wash
for every agent summary and claim over the spaced treatment D07 and D08
approved, so the unverified register has one shape. The owner also accepted
the conversation consequence: no printed author labels and no message times,
with VoiceOver keeping the author and exact instants one gesture away (R17).
The `Operator · iPhone` label for a message from another device needs a
message origin the contract does not carry, so sweep 2 prints no author label
and the contract deferral #1809 adds the field. Finding cards stay bordered
items with only the proposed route quoted, because the message is
daemon-authenticated. Dusk takes its own quote and border cuts because the
token mirror's wash and rule vanish on the dusk ground.

**R7, one claim marker.** Chose the producer keyword with an `(UNVERIFIED)`
suffix and one ⓘ per card over the four markers in use, extending D03's
on-demand explanation to every card type. The filing card's screened-text
sentence stays until the owner rewords the shared one.

## Process Decisions

**A plan §9 change comes first (#1797).** The handoff is presentation only,
but §9 (revision 78) allows on-demand Unverified source details only on the
four decision-first card types, and keeps every other type's layer-1 facts
ahead of its actions; R7 and R0 need §9 to say otherwise before sweep 0. This
mirrors #1730 for the 2 Oct audit. Treating the handoff's own scope statement
as sufficient was rejected because a sweep PR would then change the active
specification silently.

**Serial sweeps under one feature tracker (#1804).** Chose a strict
`starts-after` chain over parallel sweeps because every sweep re-records the
shared screenshot digests and rewrites `app/SURFACES.md` lines, and the owner
chose a tracker over bare cross-linked issues so start order and exit live in
one place. Units start by fiat; the tracker lists them without scheduling them.

**Sweep 3 split in two (#1801, #1802).** The README's sweep 3 would have put
one 3,843-line file and 22 of the 41 per-surface decisions in a single PR.

**Sweep 0 proves itself on card 4b (#1798).** The README's sweep 0 is
components only. Composing the ready-for-final-review card as the survey's
reference build gives the owner one real card to review in the native mock
build before any surface sweep starts.

**iPhone evidence stays manual.** The screenshot regression suite is
macOS-only. An iOS digest harness was rejected as a prerequisite because it
is its own unit and would block every sweep on tooling.

Revisit when the daemon contract settles what `approve` means on an
observation-only dispute (the one ◆ no sweep settles), when native
accessibility testing contradicts a drawn frame, or when a sweep finds that
real content cannot satisfy both a rule and an existing measured layout
budget; the revision names the rule and keeps the remaining decisions.

Follow-up: #1804.
