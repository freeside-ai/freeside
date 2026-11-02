# Finding-Adjudication Size Bounds

The adjudicator's provider output and the persisted finding-adjudication
artifact shared one 512 KiB number, but conversion and the artifact envelope
grow the output, so the largest accepted answer built an artifact the bounded
decoder refused. Nothing decoded through that bound in production, which is why
the gap never failed, but the Discuss path already turned an oversized result
into a lane error. This note records the contract #961 settles.

## Two Bounds and a Fixed Headroom

`MaxFindingAdjudicationBytes` stays the persisted bound at 512 KiB.
`MaxFindingAdjudicationProposalBytes` bounds the provider output at the
persisted bound less `FindingAdjudicationEnvelopeHeadroomBytes` (128 KiB). The
envelope is under 1 KiB, and conversion adds a bounded constant per entry: the
producer label, the offered alternatives, and the proposed route on demotion.
At the full proposal bound, one model entry plus one engine fast-path entry
grew by 909 bytes, so 128 KiB covers that structure for a batch of a few
hundred entries. It does not cover escaping: `json.Marshal` writes each `<`,
`>`, and `&` as a six-byte escape, so prose dense in them grows with its
content.

Chose a fixed headroom over raising the persisted bound alone, because any
fixed persisted bound still sits below some accepted output; the two numbers
have to differ.

## The Guarantee Is an Exact Check, Not the Headroom

`FindingAdjudication.Encode` refuses a body above the persisted bound with
`ErrFindingAdjudicationOversized`, so Encode and `DecodeFindingAdjudication`
accept the same set. `PutFindingAdjudication` admits only through `Encode`, so
no caller can store a body the bounded decoder rejects.

The engine runs the same check before it writes, so overflow fails safe:

- **Initial batch:** parks the round with cause `batch_oversized`.
- **Discuss:** builds and checks the successor before accepting the
  completion, because a completion is accepted once. An oversized successor
  ends the discussion as unavailable with the prior revision still the round
  head. A completion accepted before this check existed retires the
  discussion instead.

Chose no per-round cap on findings or entries. Many tiny entries, or prose
dense in escaped characters, can outgrow any fixed headroom, and the exact
check already parks that batch; a cap would add a second limit to tune without
adding a guarantee.

## What Stays an Ordinary Error

- **Human-decision successor:** the successor built from a human decision goes
  through the same `PutFindingAdjudication` check and fails as an ordinary
  error, with no park path. A human entry is bounded by
  `MaxHumanAdjudicationReasonBytes` (2 KiB), so this needs a prior already
  within a few KiB of the bound.
- **Stored rows:** rows written before this change are not rechecked; store
  reconstruction keeps its unlimited decoder. The daemon is pre-release.

Revisit when a per-round finding cap appears, when a production path decodes
stored artifacts through `DecodeFindingAdjudication`, or when parking with
`batch_oversized` shows up in practice, which would mean the headroom is too
small for real batches.
