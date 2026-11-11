# Read Claude's Fable Bucket as One Model-Scoped Weekly Window

#3040 makes Freeside read the Fable weekly bucket a Claude subscription
carries. Before it, a writer turn on Fable made the pinned CLI's `get_usage`
answer carry `rate_limits.model_scoped` as an array. The decoder expects every
`rate_limits` entry to be an object, so it treated the answer as malformed and
dropped the turn's whole capture, `five_hour` and `seven_day` included. The
Usage page never showed the bucket, and the pool's readings went stale after
every Fable turn. Tier A by path (`daemon/internal/ward/`); the readings grant
nothing.

## Evidence

- **The provider sends it.** On 2026-11-10 a live Fable turn
  (`claude-fable-5-2`, CLI 2.1.322) carried
  `unifiedWindows.seven_day_overage_included` in its rate-limit event, 11%
  used, with the same reset as `seven_day`. The CLI's own label for that
  window is "Fable limit".
- **The pinned CLI reports it as an array.** 2.1.319 under the production
  driver, a fake setup token, and a loopback mock sending `5h`, `7d`, and
  `7d_oi` headers answered `get_usage` with `five_hour`, `seven_day`, and
  `model_scoped: [{"display_name":"Fable",…}]`. That transcript is the
  fixture `claude_usage_loopback_fable_2_1_319.jsonl`. On the header-seed path
  the CLI builds `model_scoped` from the `7d_oi` headers alone, so it has at
  most one entry, named from its overage-included-models allowlist (default
  `["Fable","Fable 5","Fable 5.1"]`).
- **The 2026-11-03 spike missed it** because its turns carried only `7d`
  headers ([spike note](2026-11-03-1457-claude-usage-share-spike.md)).

## Decision

- **Chose the window name `seven_day_overage_included` over one derived from
  the display name.** It is the CLI's name for the bucket in its header seed
  and in a `rate_limit_event`'s `rateLimitType`, so the answer's share, an
  event's `rejected` status, and any refusal mark all land on one window. A
  name like `seven_day_fable` would split the answer from the event.
- **Chose to read only one entry that names Fable, and leave anything else
  out rather than sink the answer.** The one-entry shape is what the seed
  path produces. Several entries come only from the account usage read,
  which production never makes, and naming them all after one window would
  be wrong. An entry for another model would get the wrong label and the
  wrong route scope. Leaving those out keeps the answer's other windows. A
  value that isn't an array of entries, or a Fable entry with a bad share or
  reset, is still malformed, as for every other window.
- **Chose a fixed list of Fable route model ids over a family rule.**
  `domain.ModelWindow` maps exact route ids, and a window named in no entry
  applies to every model. Left unmapped, an exhausted Fable bucket would hold
  Opus work on the same pool. A prefix rule would change the shared domain
  contract; the list (`fable`, `claude-fable-5`, `claude-fable-5-1`,
  `claude-fable-5-2`) lives in the Claude collector. An id missing from it is
  not held by the window; the provider's refusal still parks that attempt.
- **Chose to report the mapping on every collection over only when the turn
  reported the window.** The scope is a fact about the provider, not about
  one turn. A later Opus turn's collection drops the Fable reading, and
  without the mapping a refusal mark left on that window would read as
  general and hold every model.

## Refute-First Findings

A fresh-context reviewer tried to prove the change wrong.

- **Confirmed and fixed: the collector broke its memory-only containment
  test.** `TestClaudeUsageCollectorMakesNoRequest` allows the collector to
  name only `exec.ClaudeUsageCapture`. Chose to spell the window in the
  collector and pin it equal to `exec.ClaudeFableWindow` in an internal test
  over widening that allowlist, so the containment check stays as it was.
- **Confirmed and fixed: a versioned display name was dropped.** The CLI's
  allowlist includes "Fable 5" and "Fable 5.1", so the decoder now takes
  "Fable" or "Fable " plus a version, in any case.
- **Allowed by decision: a left-out `model_scoped` entry leaves no trace in
  the collection detail.** The capture carries windows, not notes, and
  adding one would change its stored shape for a case the seed path doesn't
  produce.
- **Disproved: an answer carrying the bucket both as a named window and in
  `model_scoped` loses the capture.** It would, through the duplicate-window
  check, but the 2.1.319 seed answer holds only `five_hour`, `seven_day`, and
  `model_scoped`; the named key appears only in the account read, which
  production never makes.
- **Disproved: the mapping changes routing for other models.** The Fable
  window was never produced before (the collector left it out as unknown), a
  mapped window doesn't apply to other route ids, and the router ranks only
  windows with a reading, so an always-present mapping with no reading ranks
  nothing.
- **Disproved: the decoder accepts input it used to refuse.** A null named
  window, a list for `rate_limits`, an out-of-range or unparseable share,
  duplicate keys, and invalid UTF-8 are still refused, each by the same check
  as before.

Revisit when the pinned CLI changes the `model_scoped` shape, the seed path
starts to carry more than one bucket, `domain` gains a model-family rule, or
a new Fable model id ships.
