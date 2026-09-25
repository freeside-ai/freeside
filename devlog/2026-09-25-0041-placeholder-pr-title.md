# Refuse the Placeholder PR Title in Recipe V1

For #1464, client recipe v1 now refuses a `publication.md` whose title is
`Outcome title`, ignoring case, and the implementer and remediator prompts
no longer show that placeholder. A refused claim holds the task through the
existing `holdPublicMetadataTask` route with `HoldTrustBlocked`.

The prompts told agents to "Start with exactly `# Outcome title`". Agents
copied it verbatim: freeasinbird/gh-imgup#111 and #118 both published with
that title, and #118's artifact carried the intended title on the next line.

Chose a prompt rewording plus a publisher check over a prompt-only fix
because prompt wording lowers the copy rate but cannot guarantee model
behavior. The title comes from an imported agent claim, a returned-object
trust boundary, so the publisher has to refuse the known-bad value itself.

Chose to refuse on every composition path, including replay, over a
first-publish-only check. `reconcileTask` recomposes the candidate through
`productionCandidate` for a task that already has a ready item or a finalized
publication intent. A first-publish-only check would need a second, weaker
rule for drift repair and would let repair rewrite a PR to the placeholder.
The cost: a live v1 task that already published the placeholder holds on its
next reconcile instead of repairing. Its PR stays unchanged, which the hold
recovery text already tells the operator.

Kept this in v1 instead of minting a new recipe version. Plan §5.15 freezes
v1's rendering so retry, restart, and drift repair replay identical bytes.
The check changes no rendered byte: every claim v1 still accepts renders
exactly as before, and a refused claim publishes nothing and rewrites no
PR. A new version would also leave every frozen v1 and v2 record able to
publish the placeholder, which is the defect #1464 closes.

Chose to apply the check to recipe v2's fallback too, over limiting it to
records whose recipe is v1. When the publication author is unavailable or
its output fails screening, `publicationTitleBody` renders the client claim
through the same v1 function. That fallback is how the placeholder reached
freeasinbird/gh-imgup#118, during the author input bound mismatch #1527
tracks, and new tasks are v2, so a v1-only check would leave the observed
path open. Plan §5.15 says the author role never blocks publication; that
still holds, because the author's absence still leads to the v1 rendering.
The v1 rendering already holds a malformed claim, and a placeholder title is
one.

Rejected options:

- **Prompt-only fix.** It leaves the placeholder reachable on GitHub.
- **Salvage the next line as the title.** A placeholder title is malformed
  metadata. Guessing from the lines after it would publish text the agent
  never marked as the title.
- **Reject a wider class of generic titles.** No evidence yet of any other
  copied title. The check matches only the exact old placeholder.

The prompt now says the first line is `# ` followed by the title the agent
writes, with no token such as `<title>` and no sample, because the evidence
shows agents copy displayed placeholder text literally.

Refute-first findings:

- **Disproved: a whitespace or format-character variant reaches GitHub.** The
  existing shape check refuses leading and trailing whitespace. The importer
  screen refuses Cf and control characters before the title check.
- **Disproved: another accepted claim changes.** The new branch runs after
  the shape check and matches only the placeholder. A title that contains the
  words, such as "Keep the Outcome title field stable", still renders.
- **Disproved: a caller matches on the error text.** Both composition sites
  route any error to the hold without inspecting it.
- **Allowed by decision: variants such as `Outcome title.` still pass.** They
  aren't a verbatim copy of the old prompt.
- **Allowed by decision: `docs/plan.md` keeps its `# Outcome title` wording.**
  It describes the line format, and the issue lists editing the plan as a
  non-goal.

Revisit when an agent publishes another generic or copied title, or when
recipe v2 gains a fallback other than the v1 rendering.
