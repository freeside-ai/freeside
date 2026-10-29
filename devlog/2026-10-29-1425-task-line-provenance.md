# A Router Task Line Records Why It Was Chosen

Mandatory note: a contract change (a new task line source, its provenance,
a timeline event kind, and `task_lines` on the synced task) and a
reconstruction trust boundary (the store decodes provenance from a row and
re-derives the line id over it) (#2391).

## Decisions

Chose to bind provenance into the line id but omit it from the canonical
encoding when absent, over always encoding it. A router line's id covers
its provenance, so an edited stored provenance changes the derived id and
the read fails closed. An operator line has no provenance, so its canonical
bytes and every existing id stay unchanged; no stored row needs a rewrite.

Chose an explicit `"provenance": null` in a line's JSON over omitting the
field. The golden convention renders an optional pointer as null, so the
operator-line goldens gained one line each while their ids held. The issue
asked for the operator goldens to stay byte-identical; ids are what stored
rows depend on, and the rendered null is the convention every other
optional field follows.

Chose to cite the quota reading as opaque text (pool, window, observed-at)
over a typed reference into the quota model. The router (#2393) does not
exist yet, so the line records what the router read without fixing which
store it read from. Pool and window must be non-empty valid UTF-8, and the
time must be UTC.

Kept the triggers to `first_admission` and `exhausted_mark`, per the owner's
revision of the issue. There is no `quota_refusal` trigger: under policy
revision 103 the router writes lines only at a task's first admission, and
a quota refusal parks the task (#2421) instead of rerouting it. Only
`exhausted_mark` carries a reading and a passed-over agent; the validator
requires both or neither by trigger, and refuses a passed-over agent equal
to the chosen one.

Chose to refuse a router line at writer admission ("nothing routes a task
line yet") over accepting it unanchored, as `proposal_start` is refused
today. The engine anchors every other source to the stored operator action
that set it; a router line has nothing to anchor to until #2393, and an
unanchored source would be a trust bit taken from the row.

Chose to have the timeline and sync read lines through the store's chain
check over re-running engine anchoring. `signet` cannot import `engine`, and
the store already refuses a row whose id, version chain, or provenance does
not validate. The store's own selection gate does not re-anchor either; that
gap predates this unit and is a follow-up, not a fix in passing.

Landed the wire contract (daemon timeline and sync, the OpenAPI schema, the
generated Swift client, and the app's timeline row) as one commit. The
schema, its enum test, the contract digests, and the app's exhaustive switch
cannot pass separately, so splitting them would leave a red commit.

Bumped the `freesided inspect task` record to v11, because the embedded
timeline gained an event kind and a `line` field.

The owner set the timeline wording in review, over a first version that drew
every change as "<Role> agent · <left> → <taken>". That arrow read a router
fallback as a replacement, though the passed-over agent never ran. A row now
follows the timeline's subject, verb, detail grammar. "Set" is a role's
first agent and "changed" a replacement. The router is named ("set by
router") and the operator stays implied, as on other rows. Only a replacement
takes the arrow; a fallback reads "claude-b instead of claude-a".

The owner chose the participle for the row's subject ("Implementing agent")
over the role noun ("Implementer agent"), because it reads more correctly,
accepting a small mapping from the role name.

Rejected "routed" as the router's verb: the router routes the task, not the
agent, and the word needs the reader to know the component. Rejected naming
the reason ("claude-a exhausted") on the row, because priority with fallback
is routine and every new task would carry the note while a window is
exhausted; the reason stays in the provenance. Kept the fallback on the row
because wave 9's exit proof needs the switch visible on the timeline.

Kept the agent name as the row's detail over provider, model, and effort. A
line fixes only the name, and admission resolves it against the tree revision
it admits under, so printing a model here could show one that never ran. What
ran belongs on the run, from the admission record (#979).

## Refute-First Findings

One fresh-context reviewer attacked the diff before it was pushed.

- **Confirmed, fixed:** invalid UTF-8 in a reading's pool or window passed
  validation on write but could never decode on read, so the line would
  wedge the task. Validation now refuses it.
- **Confirmed, fixed (tests):** no test held the synced task's lines to role
  order. One now does.
- **Confirmed, narrowed (comment):** an edit to the stored provenance JSON
  that keeps its decoded value (key order, whitespace) still reads back. The
  migration comment now claims only that a change to a decoded field fails
  closed; the decoded value is what the id binds.
- **Declined:** a router line at version above 1 validates. Plan §5.4 lets
  the alternate-agent card re-pin a line (#869), so a superseding router
  line is a reachable future state, not an error.
- **Declined:** same-instant timeline ties order by kind. The schema already
  documents that tie order.

Revisit when the router (#2393) lands and anchors its lines, when the quota
model gives a reading a stable identity worth citing by reference, or when
real router rows show whether one switch's rows should merge into one.
