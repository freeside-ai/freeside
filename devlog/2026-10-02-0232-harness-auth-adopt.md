# Run Auth Adopt From the Real-Run Harness

Work unit #1684. `scripts/run-real-work.sh` now runs `freesided auth adopt` on
the state root and checks the tree it emits against the committed agent tree.
This note records what makes one committed tree reusable across fresh state
roots, the option rejected to get there, and what the refute-first pass found.

## The Tree Does Not Depend on the State Root

The emitted tree is a function of the identity ids, the route names, the
review model, the three prompt-package digests, and three dated inputs. It
reads nothing else from the store: no generation, credential digest, cost
owner, or account binding enters it.

- **Evidence.** Two adoptions on two different fixture roots, with the dated
  inputs pinned, emitted byte-identical patches. Changing any one dated input
  changed the patch. Changing both cost owners did not.
- **Three dated inputs, not two.** `-terms-basis-date`, `-offer-not-after`,
  and `-pricing-revision` each default to the current date. The issue named
  the first two. The third moves on the first of each month.
- **One exception.** An identity `auth add` already enrolled keeps the route
  name its enrollment carries. A fresh root has no such enrollment, so the
  default route names apply.

## Pinned Through the Environment, Not New Defaults

Chose three required harness variables that pin the dated inputs, over
changing the `auth adopt` defaults, because the defaults are right for the
command's own use and wrong only for a caller that compares against a
committed tree.

- **Rejected: change the flag defaults.** A fixed default date would misstate
  the operator's terms basis on an ordinary adoption, and no fixed
  `-offer-not-after` stays valid. Making the three flags required would change
  the command for every caller to serve one. The issue lists changes to
  `auth adopt` as a non-goal unless the defaults need a different shape; they
  do not.
- **Rejected: read the dated values out of the committed tree.** The harness
  would have to parse the tree's fragments in bash and would then compare the
  tree against inputs taken from itself, which proves less.
- **Cost.** The operator keeps three more values beside the tree commit.
  Renewing an offer means a new adoption with new values and a new commit,
  which is the review the tree requires anyway.

## The Harness Never Writes to the Agent Tree

Owner decision (2026-10-02): on a mismatch the harness stops before preflight
and names the patch it wrote. It reads the checkout with `git ls-tree` and
applies the patch only in a scratch repository inside the session directory.
A commit the checkout cannot resolve counts as a mismatch, because the
operator's next step is the same.

## Refute-First Findings

- **Confirmed, fixed before commit: `required` is written in plain text.**
  The harness writes every variable in its `required` list to the retained
  session's `verification-env.sh`. The cost owners and the account are
  therefore required through a separate check and written nowhere.
- **Confirmed, fixed before commit: adoption's output can name the
  inputs.** The report and the patch carry identity ids, and the cost-owner
  mismatch error quotes both labels. The harness sends adoption's output and
  git's to session files and prints only each client's status.
- **Confirmed, fixed before commit: file modes on the two top-level files.**
  Comparing object names alone ignored the mode of `policy/lineup` and
  `policy/agents.lock`. The comparison now uses `ls-tree` entries.
- **Confirmed, owned by #1682: the reviewer cost owner is published.** The
  disposition history renders it on every published PR. The harness now
  supplies that label, so a real run publishes it until #1682 merges.
- **Disproved: a false match reaches a run.** A tag, an abbreviated name, or
  an ancestor expression that holds the same tree passes the harness check,
  and `agenttree.ReadCommit` then refuses it at preflight.
- **Disproved: the rig lease blocks adoption.** `rig hold` releases its
  database-lock probe, so `auth adopt` takes the daemon lock while the rig is
  held and no daemon runs.
- **Allowed: the inputs are process arguments.** The cost owners and the
  account reach `auth adopt` as flags, visible to local process listings, as
  they are when the operator runs the command by hand.

Revisit when `auth adopt` gains a way to renew an offer without re-emitting
the whole tree, or when the harness runs for more than one operator account
on a shared host.
