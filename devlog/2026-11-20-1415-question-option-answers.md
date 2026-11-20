# Question Answers Name Options By Label And Render At Acceptance

Issue #3444 lets the owner answer an agent_question by picking one option per
decision on the card, plus an "Other answer" written in the note. The
decision command gains `question_answers`, the daemon advertises
`question_option_answers` in `/health`, and the card draws the recommended
option first. This note records why the contract has its shape. It is
mandatory: the change is a contract change on the decision-binding path
(Tier A).

## Decisions

- **Name an option by its exact label, not a minted id or its position.**
  `Decision.Validate` keeps labels unique within a decision, and a re-asked
  question that reorders its options keeps each label, so the label is
  already a stable id. Minting an id would change the decision facts the
  agent writes and every stored question. Position was rejected because the
  card reorders options (recommended first) and a re-ask may too.
- **Render the answers into `Command.Message` at acceptance.** Chose this
  over teaching each engine consumer (retry prompts, specification revision
  comments, the hand-back) to read typed answers. The rendered message is
  one `Decision N: <label>` line per decision in decision order, then a
  blank line and the note. Every consumer keeps reading the message it
  already bounds at 8 KiB (`MaxSpecRevisionCommentBytes`), and the typed
  answers stay on the record for later readers (#3443 shows past answers).
- **Lower the note's limit to 6 KiB when answers ride along.** Eight lines
  with labels cut at 200 bytes take at most about 1.7 KiB, so a 6 KiB note
  keeps the whole message within 8 KiB without a consumer changing its bound.
  A label over 200 bytes is cut on a UTF-8 boundary with an ellipsis in the
  rendered line only; the typed answer keeps the exact label.
- **Store answers sorted by decision index.** `NewCommand` sorts them, and
  `Validate` refuses an unsorted stored set, so a retry that lists the same
  answers in another order replays the stored record instead of conflicting.
- **Read a null or absent `option_label` as an answer in the note.** The
  note then must not be blank. Making the member required and nullable, to
  refuse a label dropped by accident, was rejected: the app's generated
  client omits a nil optional instead of writing null, so the daemon would
  refuse every "Other answer" the app sends.
- **Validate in two places.** The note and shape rules run where the message
  renders (`decisionMessage`), which needs no item, so a replay is
  deterministic. The completeness and label rules run against the item
  (`validateQuestionAnswers`), next to the answer-route rule.
- **Gate the picker on the `/health` feature.** An older daemon refuses the
  unknown member under strict decoding, so the app sends answers only when
  `question_option_answers` is served and keeps the free-text composer
  otherwise. The card also keeps the composer for a question without typed
  decisions, which has nothing to pick, and for a scope conflict, whose
  actions already decide: Record Answer publishes "kept the approved
  scope", so a picked option could contradict the published decision.
- **Draw the recommended option first but keep the agent's numbers.** An
  option drawn first still reads "Option 2" when the agent listed it second,
  so an owner who types "option 2" on the composer means the agent's second
  option.

## Refute-First Findings

- **A question without typed decisions is unanswerable.** Confirmed and
  fixed before commit: with the feature served, the card's answer actions
  waited for picks the card could not draw. The picker now requires typed
  decisions (`DecisionCardCompositionTests`).
- **A label can forge extra `Decision N:` lines.** Allowed by decision: the
  label is the agent's own text, already on the item, and the daemon accepts
  only a label one of the decision's options carries. A label with a newline
  misleads only the agent that wrote it.
- **The app and daemon render differently.** Disproved by checks: the client
  renders the same bytes to trust the record, and the shared cases (decision
  order, the note-less form, a whitespace-only note, a multi-byte label cut
  at the cap) are pinned on both sides (`question_answer_test.go`,
  `QuestionAnswersTests`). Whitespace sets agree: Go's `unicode.IsSpace` and
  Foundation's `whitespacesAndNewlines` agree on ordinary whitespace. They
  differ at the edges (for example U+180E), which only a crafted request to
  the mock reaches: the app trims the note before it renders, and it copies
  labels from the item, so Swift's canonical string equality never meets a
  label the daemon's byte comparison would refuse.
- **A stale pick reaches the daemon after a re-ask.** Disproved by a check:
  the card builds answers only from labels the current decisions carry, so
  a stale pick reads as no pick (`AgentQuestionPresentationTests`), and the
  daemon refuses an unknown label regardless.
- **"Other answer" is refused by the daemon.** Confirmed by the fresh-context
  review and fixed before push: the generated client left `option_label` out
  instead of sending null, and the daemon required the member. The mock's
  synthesized decoding read the missing key as nil, so no test saw it. The
  daemon now reads absent as null, and its decode test pins that form.
- **Two long labels render as the same line.** Allowed by decision: options
  whose labels share their first 197 bytes render identically once cut, so
  the agent can't tell which was picked. Telling them apart needs the
  question at render time, which the render avoids so a replay renders the
  same bytes without the item. Labels that long and that alike are an
  authoring defect the card still shows whole. Revisit when an engine
  consumer reads the typed answers, which keep the exact label.
- **Answers are checked before the stale-version check.** Allowed: a stale
  card would get a 400 instead of a 409 only if a question's decisions
  changed under the same item id, and a question item's id is per
  invocation, so its decisions never change.
- **The card renumbers the options it reorders.** Confirmed by the second
  review round and fixed: a typed "option 1" would have named the
  recommended option, not the agent's first. Options keep the agent's number.
- **A picker on a scope conflict contradicts the published decision.**
  Confirmed by the second review round and fixed in the app: the card keeps
  the typed answer for a scope conflict. Free text could already contradict
  the action, so the daemon still accepts answers there; the picker would
  have presented the options as choices the system acts on.
