# The Default Policy Closes the Source Issue Without Asking the Model (#3023)

The owner decided on 2026-11-10 that, by default, a published pull request
whose source issue can be closed says `Closes #N`, with no model judgment and
no approval card. The `Refs` reference that waits for a person stays, but only
for a project that sets `gates.source_issue_closure` to `true`. This is plan
revision 114.

> By default I want it to do the closes thing. I don't want it to go the refs,
> needing effect approval unless somebody chooses that as a setting (add a
> setting). I don't want this friction.

## What It Reverses

Plan revision 68 and
[the close-reference note](2026-09-22-1530-publication-close-reference.md)
made the `publication_author_propose` site's `resolves` flag decide, under the
default policy, between `Closes` and `Refs`, with the policy actor recording
the approval. Two paths left an issue open after a merge that resolved it:

- **The model said no.** The Wave 9 exit run (#2386) found
  freeasinbird/gh-imgup#147 publishing `Refs #134` though it did everything
  #134 asked; the site answers with a bare bool and stores no reason, so
  nobody could audit why.
- **The site failed.** An unavailable site, an input it couldn't build, or a
  failed call recorded a `daemon_fallback`, which also publishes `Refs`.

Under the default policy neither path can now arise: the engine never asks the
site.

## Chose a Change of Default Over Explaining the Model's Answer

The issue's first approach kept the model's answer, stored a reason beside it,
and showed the reason on the ready card. The owner rejected it: it explains the
friction instead of removing it, and the person still has to close the issue by
hand after the merge.

Accepted cost: a pull request that only partly addresses its issue closes the
issue on merge. The `Closes #N` line sits in the PR body, so the person who
merges sees it first. A project that wants a person to decide sets the gate.

## Chose a New Origin, `policy_default`, Over Reusing `propose_site`

The proposal still needs an origin. Reusing `propose_site` with `resolves`
true would make the stored proposal and the task record claim an answer the
site never gave. `policy_default` says the default policy set the flag. It
always resolves: the domain refuses a non-resolving one on validate and the
constructor forces `resolves` true for it, the mirror of `daemon_fallback`,
which always carries false.

Rejected: skipping the proposal and approval entirely and having the publisher
write `Closes` from the policy alone. The publisher's `Closes` comes only from
a recorded approval bound to the proposal digest and the exact merge
(plan §5.13), and keeping that path means the moved-head supersession, the
foreign-instance re-bind, and the closable-source re-gate all still apply.

## The Gate Key Is the Setting; No Second Key

`gates.source_issue_closure` already existed with `true` meaning the human
gate. Only its absent-or-`false` meaning changed, from "the site's flag
decides, approved by policy" to "always close, approved by policy". A
malformed value still turns the gate on: that was chosen so a typo never
became an unreviewed close, and it now means a typo adds friction instead,
which is the safe direction. Rejected: a second key, or a key whose `false`
asks the model under the policy actor; the owner asked for one setting, and
the model-decides-under-policy mode is the one that caused the finding.

## Rows Stored Before the Change Still Work

A run in flight across the upgrade can carry a default-policy `propose_site`
checkpoint. The engine still binds its policy approval and the store still
reads one back, so it publishes `Closes` or `Refs` by its stored flag. A
stored `daemon_fallback` still opens its notice. Dropping either arm would
strand those runs or lose their close.

Revisit when a project reports partial fixes closing issues it wanted left
open often enough that the gate's friction looks cheaper than the cleanup.

## Refute Pass

A fresh-context reviewer tried to prove the change wrong before push.

- **Confirmed and fixed:** the store's policy-approval check named only the
  origin it refused, so a future origin would have been honored silently. It
  is now an exhaustive switch the linter makes each new origin answer.
- **Confirmed and fixed:** the oversized-input restart test lost its closure
  reason check when the default stopped asking the site. It now also runs
  under the human gate, where that reason still exists.
- **Disproved by reading:** a policy approval coexisting with the gate on.
  The engine and the publisher read the run's one insert-once resolved policy,
  the closure checkpoint is keyed to its run, and the merge binding checks the
  gate before it looks at the origin.
- **Disproved by reading:** a non-closable source gaining `Closes`. The
  closable-source decision runs before the proposal, and the store re-gates
  closability on admission, approval, and publish.
- **Allowed:** a `policy_default` card would offer revision, which could
  never change the flag. No card opens without the gate, and the gate never
  stores this origin, so it is unreachable.
