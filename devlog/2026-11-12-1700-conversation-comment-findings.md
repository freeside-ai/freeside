# A Pull Request Conversation Comment Is Final at Its First Finding

Issue #3127. Tier A by path (`daemon/internal/publish/`), so a mandatory
note.

Chose to read an admitted account's pull request conversation comment as a
review body with no inline comments, bound to the ready item's head at the
first read and final at its first stored finding in the run, over reusing
the review rule that makes every edit a new finding, because a conversation
comment has no commit of its own and the hand-back cycle (#3160) needs one
comment to start one round.

## Decision

- **Head at first read.** A review carries the commit it was left on and an
  inline comment its original commit. A conversation comment carries
  neither, so the intake binds it to the ready item's head when it first
  reads it.
- **First finding final per thread.** The finding ID covers the head and the
  text digest, so a later head or an edit would each make a new finding and
  start a new round on the same comment. The intake writes no finding for a
  conversation thread (`issue_comment/<id>`) the run already has one for.
  Reviews and inline comments keep their per-digest rule
  (`externalFindingID` in `daemon/internal/domain/finding.go`).
- **The App bot's comments are skipped by account ID.** The daemon's
  disposition replies are conversation comments, so read back they would be
  findings from the bot. The intake drops a conversation comment whose
  author is the App bot's user ID (`publish.AppBotIdentity`), the identity
  the replier posts as. Login is not used, since the allowlist and the
  replier both bind numeric IDs.
- **No identity, no conversation intake.** When the bot's identity can't be
  resolved, the pass stores the reviews and inline comments, stores no
  conversation comment, reports the failure, and evicts the review cache so
  the next pass reads the comments again. Storing them without the bot
  check could store a reply as a finding.
- **The hand-back itself is #3160.** `Plan #3127` found most hand-backs
  arrive with no ready item open, and an external cycle reviews at the
  reviewed base, so moving to the new default-branch tip needs its own
  authority decision. This unit only stores the comment.

## Consequences

- A reviewer who corrects a comment must post a new one to be heard; the
  edit is ignored.
- A comment whose finding can't start a round while its head is published
  (the trigger is gated) is never answered once the head moves: the finding
  stays on the old head and the comment is final. A review left on that head
  ends the same way, but a reviewer can see which commit it named.
- Every conversation comment the owner's account posts on a ready pull
  request, the lander's included, is an admitted finding and can start a
  round. Each comment starts at most one.
- A comment from an account the profile doesn't list is stored as history
  and drives nothing, as a review is today.
- The conversation list, like the reviews, drops its validator when it
  spans pages, so a long conversation is re-read every pass.

Revisit when GitHub gives conversation comments a commit, or when a reviewer
needs an edit to count.
