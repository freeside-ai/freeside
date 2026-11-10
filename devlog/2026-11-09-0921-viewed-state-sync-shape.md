# Viewed State Rides the Revision as a Separate View Row

Contract decisions for #2930, which carries out the owner's call in
`devlog/2026-11-09-0820-notifications-default-off.md` that the inbox marks
unviewed items from daemon-held state shared across devices. The problem
that note left open: viewed state comes from the `card_opened` comprehension
event, and comprehension recording never advanced the server revision, so
no other device could learn an item was viewed.

## Decisions

- **A separate `attention_item_views` row, projected at read time.** Chose a
  new table keyed by (item id, decision-surface digest), written in a
  revision-advancing `store.Write`, and copied onto served items as
  `surface_first_opened_at` by signet. Rejected writing the time into the
  attention item's stored body: that bumps `item_version` and
  `entity_version`, so an open would refuse a decision another device was
  about to submit with a stale-version 409. The view row moves only the
  snapshot's `as_of_revision` (the larger of the item row's and the view
  row's), which the inbox store already accepts. The stored body always
  carries `null`.
- **Insert-only, so only the first view advances the revision.** A repeat
  open of the same surface writes nothing and stays on the internal,
  non-advancing path. Two devices racing on a first open cost at worst one
  extra revision; the insert's `DO NOTHING` keeps the first time.
- **A new surface starts unviewed by keying on the digest.** A surface
  revision changes the digest, finds no view row, and serves `null`. No
  reset write is needed, and old rows stay as history.
- **No backfill.** Owner decision: items open at migration time show as
  unviewed. Inferring views from past `card_opened` events is more mechanism
  than one round of marks is worth.
- **Named `surface_first_opened_at`.** It says what the value is (the first
  open of this surface) rather than a boolean `viewed`, so a later reader
  can't mistake it for a per-device or per-person state.
- **The 409 replacement carries it.** A stale-version, closed-item, or
  agent-pending refusal returns a replacement item; `Submit` projects the
  view onto it in one place, so the item a refused client adopts matches
  what sync serves. Five construction sites build those errors; one wrap
  covers them all.
- **Tier C on read.** A malformed view row is logged and served as unviewed,
  and the write path treats it as present so it never loops on a bump. It is
  a display hint derived from telemetry.
- **A dot at the row's top right, and nothing else.** The owner first chose
  a leading dot plus bold, then dropped it on review of the renders: the
  14pt gutter every item row reserved set inbox rows in from task rows and
  wrapped titles that fit before. Owner choice among four renders: a dot
  after the type line's chips, which moves nothing. Rejected a color-only
  mark (bright when unviewed, dim when viewed): too subtle in light mode,
  and task rows, which carry no viewed state, stayed bright and read as
  unviewed. Also rejected an accent keyword, the tone state chips already
  use for attention, and an accent border, which reads as selection.

## Revisit When

- Anything reads `surface_first_opened_at` as authority (a gate, a filter
  that hides items, an escalation): it must stay a Tier C hint.
- The owner wants a backfill, or per-person viewed state once more than one
  operator shares a daemon.
- The dot proves too easy to miss: reconsider pairing it with a second cue.
