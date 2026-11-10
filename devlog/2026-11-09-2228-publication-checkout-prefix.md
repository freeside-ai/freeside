# The Publication Paths Share a Checkout Step, Not a Verification Prefix

#2858 gives the forward, reentry, and operator-feedback paths one
admitted-base checkout step (`fetchAdmittedBase` in
`daemon/internal/engine/publication_core.go`) and gives the three
after-verification crash seams one helper. It renames
`reentryTransportFailure` to `publicationTransportFailure`, since forward
and handoff use it too. Behavior, error text, and hold outcomes are
unchanged. This note records why the shared step stops at the checkout,
how a fetch failure is told apart from a binding refusal, and what the
refute-first pass found.

## Decision

- **Chose to share only the checkout and the crash seam over one
  checkout-through-verification prefix.** The issue first asked for one
  prefix helper that took the recipe-gate rule as an input. Planning
  (`Plan #2858`, at `5b6215edb`) found the two paths share nothing past
  the checkout. Forward imports the replay, recovers remediation and
  continuation, and runs the recovered-publication branch before its
  recipe gate. Reentry proves a rebase, checks the merge, fetches the
  head, and resolves the image first. The recipe gates differ in outcome
  (forward completes a blocked task or holds; reentry only holds), and
  the verifiers differ in checkpoint type, verify function, and failure
  handling. A helper that took all of that as inputs would be a bundle of
  callbacks, not a shared step.
- **Chose a marker type for a fetch failure over telling the two apart
  by `domain.ErrParentKeyMismatch`.** Every binding refusal wraps that
  sentinel, but a transport error may wrap it too, so its absence can't
  identify a fetch failure. Only the `FetchBase` error is wrapped in
  `publicationBaseFetchError`; a stat error and a binding refusal come
  back bare. A caller that missed the marker would treat a substituted
  checkout as a refusal, which fails closed; the reverse mistake, a
  refusal held or retried as a transport failure, is the one the type
  rules out.
- **Kept scratch with the callers.** Later steps write
  `scratch/base-instructions` and `scratch/handoff` beside the checkout,
  so the helper neither creates nor removes the directory.
- **Added no file to the Tier A list.** The helper sits in
  `publication_core.go`, already listed for the publication transport.
  `operator_feedback.go` only calls it and decides nothing new.

## Refute-First Pass

The checkout binding decides whether the engine trusts a checkout the
transport returned, so the unit took the refute-first pass
(`docs/agent-workflow.md` §refute-first). The claim to refute: some
caller now treats a binding refusal as a transport failure, or returns
different text or a different outcome for a fetch failure.

- **Disproved by a check: the shared checkout decides differently from
  the old sequence.** A scratch test ran the old inline sequence (stat,
  `FetchBase`, binding check, as at `5b6215edb`) and `fetchAdmittedBase`
  over 1,360 enumerated transport behaviors: a missing scratch
  directory; no fetch error, a plain one, one wrapping
  `ErrParentKeyMismatch`, and one wrapping `fs.ErrNotExist`; a nil, typed
  nil, or real checkout with each mismatched repo, ref, and SHA
  combination; a checkout directory created, missing, a file, a symlink,
  or under a replaced parent; and a returned directory that is the
  requested one, its parent, another directory, or a symlink to it. Every
  case matched on which error a caller classifies, the other error's text
  and `errors.Is` results, and the validated directory. Changing the
  helper to mark binding refusals too failed 168 of those cases and the
  new unit test.
- **Disproved by reading: forward classifies a failed fetch
  differently.** Forward's removed inline block and
  `publicationTransportFailure` call the same three classifiers in the
  same order, hold with the same `importer.Result{CommitSHA:
  task.HeadSHA}` and hold reasons, and wrap with the same `"%s: %w"`
  text.
- **Disproved by a fresh-context reviewer: any other drift.** Asked to
  refute, the reviewer confirmed no defect. Operator feedback's one
  caller returns the marked error as is, and no engine, signet, or
  command code compares, switches on, or type-asserts it. Every engine
  `FetchBase` call outside tests goes through the helper. The crash seam
  keeps its text, its `errProductionCrashSeam` join, and its nil skip at
  all three sites. Breaking the helper's marking or binding check in any
  of four ways fails the new test.
- **Confirmed and fixed: the stat-failure path had no test case.** The
  reviewer found it; the test now pins that a missing scratch directory
  comes back unmarked.

Revisit when a fourth path fetches the admitted base, or when forward
and reentry converge on one recipe gate and verifier: then a longer
shared prefix may be a real step rather than callbacks.
