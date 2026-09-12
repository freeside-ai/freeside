# Judgment Answer Selection And Adjudicator Time Bound

Work unit: PR #1308, judgment driver (`daemon/internal/claudeinference`) and
the adjudicator site (`daemon/internal/inference`). Refs #1275, #1001.

## Decision

Chose to select the last site-valid JSON object inside a Claude judgment
completion over the previous rule that the entire completion text must
validate. Chose a 120-second adjudicator site bound over the previous 30
seconds.

## Why

The first live production judgment calls (Wave 7 controlled challenge, run
adc995da) both fell back to "inference unavailable" seconds after Codex
returned a genuine finding. The classifier answered with the requested object
inside a ```json Markdown fence. The adjudicator answered with about 1,500
tokens of prose reasoning followed by the object, and its call also could not
finish inside 30 seconds. The driver refused all of this for shape, not for
content, and the round parked as a `review_dispute` item with no route to
remediation. The site instructions already demand a bare object; the model
does not reliably comply, and the CLI offers no structured-output mode the
driver could pin.

Rejected options:

- Prompt-only hardening. It changes nothing the daemon can verify and the
  fenced and prose-prefixed answers arrived under the existing "return only"
  instruction.
- Keeping the whole-text rule. It makes the production judgment path fail
  closed on ordinary model formatting, which is the observed outcome.
- Accepting the first object found. A model that writes an example object
  in its reasoning and then its answer would have the example selected.
  Trying candidates from the last opening brace backwards makes the final
  answer win and lets a nested object yield to the object enclosing it.
- Keeping the 10-minute per-root starvation allowance. The ledger reserves
  each site's full bound per call, and the original allowance was the
  20-call root ceiling at the 30-second bound every site then had, so the
  allowance never parked a root the ceiling still admitted. Keeping that
  invariant at the 120-second adjudicator bound makes it 40 minutes. A
  20-minute value sized for one adjudication per root was tried first and
  rejected: each review round with residue adjudicates again, so eight
  rounds of one classification and one adjudication would have parked the
  ninth at 16 calls. 120 seconds still leaves three to five times the
  measured 24 to 36 seconds; 180 seconds would have been three times the
  measurement and was trimmed first, before the reviewer showed the
  allowance itself was the binding limit.
- Validating every balanced span. Nested spans overlap, so a completion of
  deeply nested braces would cost a validation per level, quadratic in its
  size, after the bounded CLI call has already returned. Selection now
  stops after eight passes' worth of candidate bytes and fails closed; a
  well-formed answer nests two levels and costs three.

## Refute-First Findings

- Claim: selecting a substring widens the returned-object trust boundary.
  Disproved by inspection: the selected span reaches the same
  `Site.ValidateOutput` strict decode as before, and the whole-text path is
  unchanged. Nothing is trusted that the validator did not accept.
- Claim: prose or fences could smuggle a second, different answer past the
  engine. Disproved by the selection order: at most one span is returned, the
  last one the site validates, and the engine's own lattice, route and
  identity gates run on it as before.
- Claim: an earlier site-valid object could be selected when the final answer
  is invalid. Allowed by decision: both spans would be complete, schema and
  lattice-valid proposals, the engine still gates every proposal, and the
  alternative (fail closed) is the parked round this change exists to end.
- Claim (Codex, P1): a 180-second adjudicator bound exhausts the 10-minute
  per-root starvation allowance by the third round. Confirmed by the ledger's
  reservation arithmetic; resolved by the 120-second bound above.
- Claim (Codex, P1): a 20-minute allowance covers one adjudication per
  root, not one per review round, so a run of single-residue rounds parks
  at 16 of its 20 calls. Confirmed by the reservation arithmetic; resolved
  by restoring the ceiling-times-largest-bound sizing, 40 minutes.
- Claim (Codex, P1): collecting spans in one linear pass leaves validation
  quadratic, because every nested span is validated in full. Confirmed;
  resolved by the eight-pass bound on validated candidate bytes, with
  decode cases for 3,000 nested braces before, instead of, and after the
  answer.
- Claim (repository ratchet): a typed JSON decode outside `strictjson` is
  forbidden. Confirmed by CI; resolved by balancing braces without decoding
  and leaving the one strict decode to the site validator.

## Revisit When

- The pinned Claude CLI gains a structured-output or JSON-schema mode the
  driver can require; the brace scan should then become unnecessary.
- Adjudication batches routinely carry more than a handful of findings, or
  measured adjudicator calls approach 60 seconds; the per-root allowance and
  the site bound would then need to move together.
- A daemon adjudicates more than 30 batches a day: the adjudicator site's
  one-hour starvation limit admits 30 reservations at 120 seconds, below its
  100-call limit, because the ledger reserves the bound rather than the
  measured duration.
