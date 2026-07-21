// Package engine owns the durable workflow state machine. It composes the
// store, signet, and StageDriver boundaries; none of those packages needs to
// know the workflow that uses it.
//
// The Phase 1A walking skeleton is deliberately concrete: a fake run creates
// an approval item, an accepted approval opens a conversation-feedback item,
// and a fake invocation records one attempt before it starts, then completes
// the conversation from that durable recovery index. Reconcile derives every
// step from durable state, so retry and restart use the same path as first
// execution.
//
// See docs/plan.md §5.3 (execution), §5.12 (workflow definition), §5.14
// (conversations and effectively-once acceptance), and §11 (the 1A.0 exit).
package engine
