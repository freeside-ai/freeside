# Commit Message Reason From the Approved Specification

Decision record for #3025: the fallback commit message Freeside publishes
now opens its body with why the change was made, not only the run's trace
identifiers. The note is mandatory because the change touches a Tier A
replay gate (`validateProductionReplayOptions`) and adds
`internal/engine/commit_message.go` to the Tier A paths: it now screens
agent-written prose before it reaches GitHub.

## Reason From the Spec's Opening Paragraph

Chose the approved specification's first prose paragraph after its
`# Title`, cut to two sentences and 320 bytes, over the approved summary's
`## Result` section or the implementer's summary. The specification is the
digest-verified blob the subject already comes from, so import and replay
derive the reason from the same bytes with no new durable read. Either
alternative needs a new authenticated store read at import and at
publication replay.

The cost is quality: the specifier prompt asks the body to open with
behavior, not motive, so the reason often says what changes more than why.
It still beats an identifier, and it is the only why-shaped text inside the
approved, digest-bound bytes.

## Drop, Never Neutralize

A reason that fails the publication prose screen (`publicationtext.Screen`),
contains anything GitHub links or notifies on in a commit
(`autolinkTokenRe`, checked on each HTML-decoding stage), or makes the
assembled message fail `importer.ScreenMessage` is dropped. The message is
then byte-identical to v1. Chose dropping over rendering tokens inert, as
the PR body renderer does, because a commit message has no inline-code
span to hide a token in. The reason is added only to a message that already
passes, so it never pushes a message onto the `REWRITE ME` floor.

## Replay Accepts Both Recipes

Chose to have publication replay accept a stored message equal to either
the v2 or the frozen v1 derivation over storing a recipe version on the
import options. No stored record shape changes, and a run imported before
the upgrade and published after it still publishes. The gate still
refuses any other message. v1 is pinned by a golden captured from the
pre-v2 code and must never change.

"Frozen" covers the recipe's own code, not the screens it calls. v2 drops
or keeps the reason by `publicationtext.Screen`, `autolinkTokenRe`, and
`importer.ScreenMessage`, all shared with other publication paths. A later
change to one of them that alters a derivation makes replay refuse runs
imported before it. That fails closed (nothing wrong publishes) but
strands the run, so such a change has to freeze the current derivation as
another accepted recipe.

The reason is dropped when it contains a word longer than a commit line,
because wrapping would hard-split that word and could start a link token
(`www.`, `GH-12`, a hex SHA) the screen never saw. Without such a word,
wrapping only turns spaces into newlines and keeps every token boundary.

One recovery window does not converge: a released export whose durable
export record was written under v1, recovered after the upgrade while the
export-record lookup fails, re-imports under v2 and gets a different
commit. The write-once export record refuses it as an export authority
conflict, which preserves the evidence for operator repair. It fails loud
and publishes nothing wrong; a successful lookup takes the recorded
export and never re-imports.

Revisit when the reason proves too weak in owner review (the stronger
source is the approved summary's `## Result`, which needs the new
authenticated read above), or when a third recipe is needed, including
one forced by a change to a shared screen: at that point store the recipe
version instead of matching every frozen derivation.
