package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// FindingAdjudicationDecision reconstructs the current adjudication item and
// the immutable command that concluded it. A nil command means the item is
// still open. The command is selected from authenticated command history,
// rather than inferred from item-version arithmetic, so later delivery-driven
// item-version advances cannot detach a terminal decision from its cause.
func (tx *ReadTx) FindingAdjudicationDecision(
	ctx context.Context, itemID domain.ItemID,
) (domain.AttentionItem, *domain.Command, error) {
	item, err := tx.GetAttentionItem(ctx, itemID)
	if err != nil {
		return domain.AttentionItem{}, nil, err
	}
	if item.Type != domain.AttentionFindingAdjudication || item.FindingAdjudication == nil {
		return domain.AttentionItem{}, nil, domain.ErrParentKeyMismatch
	}
	commands, err := tx.ListCommandsForItem(ctx, itemID)
	if err != nil {
		return domain.AttentionItem{}, nil, err
	}
	var terminal *domain.Command
	for i := range commands {
		command := &commands[i]
		switch command.Action {
		case domain.ActionAcceptRecommendedRoute,
			domain.ActionChooseAlternativeRoute,
			domain.ActionStop:
			if terminal != nil {
				return domain.AttentionItem{}, nil, domain.ErrParentKeyMismatch
			}
			terminal = command
		case domain.ActionDiscuss:
			continue
		default:
			return domain.AttentionItem{}, nil, domain.ErrParentKeyMismatch
		}
	}
	if item.Status == domain.StatusOpen {
		if terminal != nil {
			return domain.AttentionItem{}, nil, domain.ErrParentKeyMismatch
		}
		return item, nil, nil
	}
	if terminal == nil || item.Status != domain.StatusResolved || item.DecidedAt == nil ||
		terminal.ItemVersion >= item.ItemVersion || !item.Offers(terminal.Action) ||
		terminal.ItemID != item.ID || terminal.PRHeadSHA != item.PRHeadSHA ||
		!slices.Equal(terminal.ArtifactDigests, item.ArtifactDigests) {
		return domain.AttentionItem{}, nil, domain.ErrParentKeyMismatch
	}
	deciding := *terminal
	return item, &deciding, nil
}

const putFindingAdjudicationSQL = `
INSERT INTO finding_adjudications
    (run_id, round, revision, predecessor_digest, content_digest,
     finding_batch_digest, approved_spec_digest, instruction_snapshot_digest,
     resolved_policy_digest, created_at, body_digest, body)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (run_id, round, revision) DO NOTHING`

// validateFindingAdjudicationBinding re-runs every authoritative join instead of
// trusting the artifact's copied keys: the review round must exist, the artifact's
// entry finding set must equal that round's finding set exactly, each engine entry's
// evidence must equal what the deterministic fast path actually derives, its
// instruction snapshot must equal the round's authoritative instruction binding, and
// its approved-spec and resolved-policy digests must equal the run's authoritative
// values. A missing record, a foreign or missing finding, a duplicate entry, an
// engine entry whose evidence disagrees with its finding, or an
// instruction/spec/policy digest that disagrees with its authority fails with
// ErrParentKeyMismatch. Together these re-gate every caller-supplied trust bit the
// artifact carries: the finding batch, each engine entry's evidence, the instruction
// snapshot, and the spec and policy the adjudication's routing decisions rest on.
func (tx *ReadTx) validateFindingAdjudicationBinding(
	ctx context.Context, artifact domain.FindingAdjudication,
) error {
	record, err := tx.reviewRecordForRound(ctx, artifact.RunID, artifact.Round)
	if err != nil {
		return err
	}
	entryIDs := make([]domain.FindingID, 0, len(artifact.Entries))
	seen := make(map[domain.FindingID]struct{}, len(artifact.Entries))
	for _, entry := range artifact.Entries {
		if _, duplicate := seen[entry.FindingID]; duplicate {
			return domain.ErrParentKeyMismatch
		}
		seen[entry.FindingID] = struct{}{}
		entryIDs = append(entryIDs, entry.FindingID)
		// Validate's structural backstop confirms an engine entry carries the
		// one row the fast path can produce, but a hand-built or decoded entry
		// could still pair that row with arbitrary evidence text: nothing about
		// the shape catches wrong content. The card presents non-empty engine
		// evidence as a daemon-verified fact (#892, #984), so a present value
		// re-derives the fast path's only production invariant — evidence is
		// the finding's own containment location, and nothing else — against
		// the immutable stored Finding, exactly as the item-level re-gate does
		// for the message and location (entities.go's
		// gateFindingAdjudicationItem). Evidence is optional on the type and
		// the production fast path never emits it empty, so an absent value
		// carries nothing for the card to mislabel and is left to the
		// structural check alone.
		if entry.Producer == domain.AdjudicationProducerEngine && len(entry.Evidence) > 0 {
			finding, err := tx.GetFinding(ctx, entry.FindingID)
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					return domain.ErrParentKeyMismatch
				}
				return err
			}
			if finding.Location == nil ||
				!slices.Equal(entry.Evidence, []string{finding.Location.String()}) {
				return domain.ErrParentKeyMismatch
			}
		}
	}
	recordIDs := slices.Clone(record.FindingIDs)
	slices.Sort(recordIDs)
	slices.Sort(entryIDs)
	if !slices.Equal(entryIDs, recordIDs) {
		return domain.ErrParentKeyMismatch
	}
	// The instruction snapshot must equal the review round's authoritative
	// instruction binding (already loaded above): an adjudication naming a
	// different snapshot would be reconstructed as though it used trusted
	// repository instructions the bound reviewer never received, so its cited
	// rules and compatibility routing would rest on instructions outside the
	// round's trusted base.
	if artifact.InstructionSnapshotDigest != record.InstructionDigest {
		return domain.ErrParentKeyMismatch
	}
	// The run's spec and policy digests are fixed at creation, so an artifact
	// naming different ones would record an adjudication bound to a spec or
	// policy the run is not — authorizing routing under a different work contract
	// or policy. They are the authority right here; take them from the run rather
	// than trusting the artifact's copied digests (mirrors requireRecordedAttempt).
	run, err := tx.GetRun(ctx, artifact.RunID)
	if err != nil {
		return err
	}
	if artifact.ApprovedSpecDigest != run.SpecDigest || artifact.ResolvedPolicyDigest != run.PolicyDigest {
		return domain.ErrParentKeyMismatch
	}
	return nil
}

// validateAdjudicationFeedback re-derives a successor's feedback provenance
// from persisted state. The invocation's copied conversation binding, the
// dispatch intent's item binding, and the artifact's copied prefix digest are
// never trusted. A new successor must follow the one item-version advance and
// agent message committed by completion acceptance. Historical reconstruction
// permits later current versions because attention-item versions only advance
// and their adjudication binding is immutable.
func (tx *ReadTx) validateAdjudicationFeedback(
	ctx context.Context, predecessor domain.FindingAdjudication,
	feedback domain.AdjudicationFeedback, requireCurrentItemVersion bool,
) error {
	invocation, err := tx.GetAgentInvocation(ctx, feedback.InvocationID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return domain.ErrParentKeyMismatch
		}
		return err
	}
	if invocation.ConversationID == nil || *invocation.ConversationID != feedback.ConversationID ||
		invocation.ThroughSequence != feedback.ThroughSequence {
		return domain.ErrParentKeyMismatch
	}
	intent, err := tx.GetOutbox(ctx, string(feedback.InvocationID))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return domain.ErrParentKeyMismatch
		}
		return err
	}
	if intent.Quarantined() || intent.Kind != string(domain.AgentInvocationRequestedKind) ||
		intent.IdempotencyKey != string(feedback.InvocationID) {
		return domain.ErrParentKeyMismatch
	}
	request, err := domain.DecodeConversationInvocationIntent(intent.Payload)
	if err != nil || request.InvocationID != feedback.InvocationID ||
		request.ConversationID != feedback.ConversationID {
		return domain.ErrParentKeyMismatch
	}
	completion, err := tx.GetInbox(ctx, string(feedback.InvocationID))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return domain.ErrParentKeyMismatch
		}
		return err
	}
	if completion.Kind != "agent_completion" ||
		completion.IdempotencyKey != string(feedback.InvocationID) ||
		completion.Status != outboxStatusPending {
		return domain.ErrParentKeyMismatch
	}
	var accepted struct {
		InvocationID domain.InvocationID `json:"invocation_id"`
		Body         string              `json:"body"`
		Attachments  []domain.Digest     `json:"attachments"`
	}
	if err := strictjson.Decode(completion.Payload, &accepted,
		strictjson.RejectInvalidUTF8, strictjson.NoLimit); err != nil ||
		accepted.InvocationID != feedback.InvocationID {
		return domain.ErrParentKeyMismatch
	}
	item, err := tx.GetAttentionItem(ctx, request.ItemID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return domain.ErrParentKeyMismatch
		}
		return err
	}
	completedItemVersion := request.ItemVersion + 1
	if completedItemVersion <= request.ItemVersion ||
		item.Type != domain.AttentionFindingAdjudication || item.FindingAdjudication == nil ||
		item.ConversationID == nil || *item.ConversationID != request.ConversationID ||
		item.ItemVersion < completedItemVersion ||
		(requireCurrentItemVersion &&
			(item.ItemVersion != completedItemVersion || item.Status != domain.StatusOpen)) ||
		item.FindingAdjudication.AdjudicationDigest != predecessor.Digest ||
		item.FindingAdjudication.RunID != predecessor.RunID ||
		item.FindingAdjudication.Round != predecessor.Round {
		return domain.ErrParentKeyMismatch
	}
	conversation, err := tx.GetConversation(ctx, feedback.ConversationID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return domain.ErrParentKeyMismatch
		}
		return err
	}
	digest, _, err := conversation.PrefixContent(feedback.ThroughSequence)
	if err != nil {
		return domain.ErrParentKeyMismatch
	}
	if digest != feedback.PrefixDigest {
		return domain.ErrParentKeyMismatch
	}
	replyIndex := feedback.ThroughSequence
	if replyIndex >= len(conversation.Messages) {
		return domain.ErrParentKeyMismatch
	}
	reply := conversation.Messages[replyIndex]
	if reply.ID != domain.MessageID("msg-agent-"+string(feedback.InvocationID)) ||
		reply.Sequence != feedback.ThroughSequence+1 || reply.Author != domain.AuthorAgent ||
		reply.Body != accepted.Body || !slices.Equal(reply.Attachments, accepted.Attachments) {
		return domain.ErrParentKeyMismatch
	}
	return nil
}

// validateFindingAdjudicationSuccessor re-runs the append-only chain gate from
// durable predecessor, invocation, and conversation state.
func (tx *ReadTx) validateFindingAdjudicationSuccessor(
	ctx context.Context, artifact domain.FindingAdjudication,
	requireCurrentItemVersion bool,
) error {
	if artifact.Revision == 1 {
		return nil
	}
	if artifact.PredecessorDigest == nil || artifact.Feedback == nil {
		return domain.ErrParentKeyMismatch
	}
	predecessor, err := tx.GetFindingAdjudication(ctx, *artifact.PredecessorDigest)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return domain.ErrParentKeyMismatch
		}
		return err
	}
	if predecessor.RunID != artifact.RunID || predecessor.Round != artifact.Round ||
		predecessor.Revision+1 != artifact.Revision ||
		predecessor.FindingBatchDigest != artifact.FindingBatchDigest ||
		predecessor.ApprovedSpecDigest != artifact.ApprovedSpecDigest ||
		predecessor.InstructionSnapshotDigest != artifact.InstructionSnapshotDigest ||
		predecessor.ResolvedPolicyDigest != artifact.ResolvedPolicyDigest {
		return domain.ErrParentKeyMismatch
	}
	return tx.validateAdjudicationFeedback(
		ctx, predecessor, *artifact.Feedback, requireCurrentItemVersion,
	)
}

func (tx *ReadTx) reviewRecordForRound(
	ctx context.Context, runID domain.RunID, round int,
) (domain.ReviewRecord, error) {
	var invocationID string
	if err := tx.tx.QueryRowContext(ctx, `SELECT invocation_id FROM review_records
		WHERE run_id = ? AND round = ?`, runID, round).Scan(&invocationID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ReviewRecord{}, domain.ErrParentKeyMismatch
		}
		return domain.ReviewRecord{}, err
	}
	return tx.GetReviewRecord(ctx, domain.InvocationID(invocationID))
}

// PutFindingAdjudication appends one immutable revision to a review round. A
// byte-identical replay converges; a different artifact at the same revision,
// a skipped revision, or a stale-parent fork is an immutable conflict.
func (tx *WriteTx) PutFindingAdjudication(
	ctx context.Context, artifact domain.FindingAdjudication,
) error {
	if err := artifact.Validate(); err != nil {
		return fmt.Errorf("put finding adjudication %q round %d: %w", artifact.RunID, artifact.Round, err)
	}
	if err := tx.validateFindingAdjudicationBinding(ctx, artifact); err != nil {
		return fmt.Errorf("put finding adjudication %q round %d binding: %w", artifact.RunID, artifact.Round, err)
	}
	body, err := encode(artifact)
	if err != nil {
		return fmt.Errorf("put finding adjudication %q round %d: %w", artifact.RunID, artifact.Round, err)
	}
	// Reconstruct an existing exact revision before replay comparison. Otherwise
	// corruption in a copied lookup column could hide behind an unchanged body.
	if _, err := tx.getFindingAdjudicationRevision(ctx, artifact.RunID, artifact.Round, artifact.Revision); err == nil {
		return tx.putFindingAdjudicationBody(ctx, artifact, body)
	} else if !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("put finding adjudication %q round %d revision %d existing row: %w",
			artifact.RunID, artifact.Round, artifact.Revision, err)
	}

	head, err := tx.GetFindingAdjudicationForRound(ctx, artifact.RunID, artifact.Round)
	if artifact.Revision == 1 {
		if err == nil {
			return fmt.Errorf("put finding adjudication %q round %d initial after revision %d: %w",
				artifact.RunID, artifact.Round, head.Revision, ErrImmutableConflict)
		}
		if !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("put finding adjudication %q round %d current head: %w", artifact.RunID, artifact.Round, err)
		}
	} else {
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return fmt.Errorf("put finding adjudication %q round %d successor without head: %w",
					artifact.RunID, artifact.Round, domain.ErrParentKeyMismatch)
			}
			return fmt.Errorf("put finding adjudication %q round %d current head: %w", artifact.RunID, artifact.Round, err)
		}
		if artifact.PredecessorDigest == nil || *artifact.PredecessorDigest != head.Digest ||
			artifact.Revision != head.Revision+1 {
			return fmt.Errorf("put finding adjudication %q round %d revision %d after head revision %d: %w",
				artifact.RunID, artifact.Round, artifact.Revision, head.Revision, ErrImmutableConflict)
		}
		if err := tx.validateFindingAdjudicationSuccessor(ctx, artifact, true); err != nil {
			return fmt.Errorf("put finding adjudication %q round %d successor binding: %w", artifact.RunID, artifact.Round, err)
		}
	}
	return tx.putFindingAdjudicationBody(ctx, artifact, body)
}

func (tx *WriteTx) putFindingAdjudicationBody(
	ctx context.Context, artifact domain.FindingAdjudication, body string,
) error {
	var predecessor any
	if artifact.PredecessorDigest != nil {
		predecessor = *artifact.PredecessorDigest
	}
	if err := tx.putImmutable(ctx, putFindingAdjudicationSQL,
		[]any{
			artifact.RunID, artifact.Round, artifact.Revision, predecessor,
			artifact.Digest, artifact.FindingBatchDigest,
			artifact.ApprovedSpecDigest, artifact.InstructionSnapshotDigest,
			artifact.ResolvedPolicyDigest, formatTime(artifact.CreatedAt),
			reviewBodyDigest(body), body,
		},
		`SELECT body_digest || body FROM finding_adjudications WHERE run_id = ? AND round = ? AND revision = ?`,
		[]any{artifact.RunID, artifact.Round, artifact.Revision}, reviewBodyAuthority(body)); err != nil {
		return fmt.Errorf("put finding adjudication %q round %d revision %d: %w",
			artifact.RunID, artifact.Round, artifact.Revision, err)
	}
	return nil
}

// findingAdjudicationRow holds the extracted lookup and integrity columns of one
// stored artifact, cross-checked against the decoded body in reconstruct.
type findingAdjudicationRow struct {
	runID                     string
	round                     int
	revision                  int
	predecessorDigest         sql.NullString
	contentDigest             string
	findingBatchDigest        string
	approvedSpecDigest        string
	instructionSnapshotDigest string
	resolvedPolicyDigest      string
	createdAt                 string
	bodyDigest                string
	body                      []byte
}

const selectFindingAdjudicationColumns = `run_id, round, revision, predecessor_digest, content_digest,
    finding_batch_digest, approved_spec_digest, instruction_snapshot_digest,
    resolved_policy_digest, created_at, body_digest, body`

func scanFindingAdjudicationRow(sc scanner) (findingAdjudicationRow, error) {
	var row findingAdjudicationRow
	err := sc.Scan(&row.runID, &row.round, &row.revision, &row.predecessorDigest,
		&row.contentDigest, &row.findingBatchDigest,
		&row.approvedSpecDigest, &row.instructionSnapshotDigest, &row.resolvedPolicyDigest,
		&row.createdAt, &row.bodyDigest, &row.body)
	return row, err
}

// reconstructFindingAdjudication decodes one row and re-runs every check a
// decoded trust bit demands: the body integrity digest, the full validation
// backstop (which recomputes the content and finding-batch digests), the
// agreement of every extracted lookup column with the decoded body, and the
// finding-set binding against current review state. No copied column is trusted.
func (tx *ReadTx) reconstructFindingAdjudication(
	ctx context.Context, row findingAdjudicationRow,
) (domain.FindingAdjudication, error) {
	if row.bodyDigest != reviewBodyDigest(string(row.body)) {
		return domain.FindingAdjudication{}, errRowInconsistent
	}
	artifact, err := decode[domain.FindingAdjudication](row.body)
	if err != nil {
		return domain.FindingAdjudication{}, err
	}
	if string(artifact.RunID) != row.runID || artifact.Round != row.round || artifact.Revision != row.revision ||
		string(artifact.Digest) != row.contentDigest ||
		string(artifact.FindingBatchDigest) != row.findingBatchDigest ||
		string(artifact.ApprovedSpecDigest) != row.approvedSpecDigest ||
		string(artifact.InstructionSnapshotDigest) != row.instructionSnapshotDigest ||
		string(artifact.ResolvedPolicyDigest) != row.resolvedPolicyDigest ||
		formatTime(artifact.CreatedAt) != row.createdAt {
		return domain.FindingAdjudication{}, errRowInconsistent
	}
	if (artifact.PredecessorDigest != nil) != row.predecessorDigest.Valid ||
		(artifact.PredecessorDigest != nil && string(*artifact.PredecessorDigest) != row.predecessorDigest.String) {
		return domain.FindingAdjudication{}, errRowInconsistent
	}
	if err := tx.validateFindingAdjudicationBinding(ctx, artifact); err != nil {
		return domain.FindingAdjudication{}, err
	}
	if err := tx.validateFindingAdjudicationSuccessor(ctx, artifact, false); err != nil {
		return domain.FindingAdjudication{}, err
	}
	return artifact, nil
}

// GetFindingAdjudication reconstructs one artifact by its content digest.
func (tx *ReadTx) GetFindingAdjudication(
	ctx context.Context, digest domain.Digest,
) (domain.FindingAdjudication, error) {
	row, err := scanFindingAdjudicationRow(tx.tx.QueryRowContext(ctx,
		`SELECT `+selectFindingAdjudicationColumns+
			` FROM finding_adjudications WHERE content_digest = ?`, digest))
	if err != nil {
		return domain.FindingAdjudication{}, fmt.Errorf("get finding adjudication %q: %w", digest, notFoundOr(err))
	}
	artifact, err := tx.reconstructFindingAdjudication(ctx, row)
	if err != nil {
		return domain.FindingAdjudication{}, fmt.Errorf("get finding adjudication %q: %w", digest, err)
	}
	return artifact, nil
}

func (tx *ReadTx) getFindingAdjudicationRevision(
	ctx context.Context, runID domain.RunID, round, revision int,
) (domain.FindingAdjudication, error) {
	row, err := scanFindingAdjudicationRow(tx.tx.QueryRowContext(ctx,
		`SELECT `+selectFindingAdjudicationColumns+
			` FROM finding_adjudications WHERE run_id = ? AND round = ? AND revision = ?`,
		runID, round, revision))
	if err != nil {
		return domain.FindingAdjudication{}, fmt.Errorf("get finding adjudication %q round %d revision %d: %w",
			runID, round, revision, notFoundOr(err))
	}
	artifact, err := tx.reconstructFindingAdjudication(ctx, row)
	if err != nil {
		return domain.FindingAdjudication{}, fmt.Errorf("get finding adjudication %q round %d revision %d: %w",
			runID, round, revision, err)
	}
	return artifact, nil
}

// GetFindingAdjudicationForRound reconstructs the artifact for one review round.
func (tx *ReadTx) GetFindingAdjudicationForRound(
	ctx context.Context, runID domain.RunID, round int,
) (domain.FindingAdjudication, error) {
	row, err := scanFindingAdjudicationRow(tx.tx.QueryRowContext(ctx,
		`SELECT `+selectFindingAdjudicationColumns+
			` FROM finding_adjudications WHERE run_id = ? AND round = ? ORDER BY revision DESC LIMIT 1`, runID, round))
	if err != nil {
		return domain.FindingAdjudication{}, fmt.Errorf("get finding adjudication %q round %d: %w", runID, round, notFoundOr(err))
	}
	artifact, err := tx.reconstructFindingAdjudication(ctx, row)
	if err != nil {
		return domain.FindingAdjudication{}, fmt.Errorf("get finding adjudication %q round %d: %w", runID, round, err)
	}
	return artifact, nil
}

// ListFindingAdjudications returns one run's adjudication artifacts in round,
// revision order. It enumerates the whole table and reconstructs every row
// before the run filter, so a corrupted copied run key cannot move a row out of
// all keyed reads and make a run's history look complete by omission (the
// review-record list pattern).
func (tx *ReadTx) ListFindingAdjudications(
	ctx context.Context, runID domain.RunID,
) ([]domain.FindingAdjudication, error) {
	rows, err := tx.tx.QueryContext(ctx,
		`SELECT `+selectFindingAdjudicationColumns+
			` FROM finding_adjudications ORDER BY run_id, round, revision`)
	if err != nil {
		return nil, fmt.Errorf("list finding adjudications %q: %w", runID, err)
	}
	var raw []findingAdjudicationRow
	for rows.Next() {
		row, err := scanFindingAdjudicationRow(rows)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("list finding adjudications %q row %d: %w", runID, len(raw)+1, err)
		}
		raw = append(raw, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("list finding adjudications %q: %w", runID, err)
	}
	out := make([]domain.FindingAdjudication, 0, len(raw))
	for i, row := range raw {
		artifact, err := tx.reconstructFindingAdjudication(ctx, row)
		if err != nil {
			return nil, fmt.Errorf("list finding adjudications %q row %d: %w", runID, i+1, err)
		}
		if artifact.RunID == runID {
			out = append(out, artifact)
		}
	}
	return out, nil
}
