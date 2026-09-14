package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Keep in sync with export.SummaryEvidenceLabel without making the storage
// trust boundary depend on the higher-level evidence-export package.
const summaryEvidenceLabel = "freeside.summary"

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableInt(value int) any {
	if value == 0 {
		return nil
	}
	return value
}

func optionalStringEqual(column sql.NullString, value string) bool {
	return column.Valid == (value != "") && (!column.Valid || column.String == value)
}

func optionalIntEqual(column sql.NullInt64, value int) bool {
	return column.Valid == (value != 0) && (!column.Valid || column.Int64 == int64(value))
}

func derivedRetryImplementationRunID(campaignID domain.CampaignID, attemptNumber int) domain.RunID {
	sum := sha256.Sum256([]byte(fmt.Sprintf(
		"freeside.production-attempt/v1\x00%s\x00%d", campaignID, attemptNumber)))
	return domain.RunID("run-" + hex.EncodeToString(sum[:]))
}

func derivedInitialCampaignID(implementationRunID domain.RunID) domain.CampaignID {
	sum := sha256.Sum256([]byte("freeside.production-campaign/v1\x00" + string(implementationRunID)))
	return domain.CampaignID("campaign-" + hex.EncodeToString(sum[:]))
}

func (tx *ReadTx) authenticateRunProductionLineage(ctx context.Context, run domain.Run) error {
	if run.CampaignID == "" {
		return nil
	}
	attempt, err := tx.GetProductionAttempt(ctx, run.CampaignID, run.AttemptNumber)
	if err != nil {
		return fmt.Errorf("run %q production attempt: %w", run.ID, err)
	}
	if run.ID == attempt.SpecificationRunID {
		if attempt.Kind != domain.ProductionAttemptInitial || attempt.AttemptNumber != 1 ||
			run.SpecDigest != attempt.SourceDigest || run.AttemptReason != "" || run.ParentRunID != "" {
			return fmt.Errorf("run %q specification lineage: %w", run.ID, domain.ErrParentKeyMismatch)
		}
		return nil
	}
	if run.ID != attempt.ImplementationRunID || attempt.ApprovedSpecDigest == "" ||
		run.SpecDigest != attempt.ApprovedSpecDigest || run.AttemptReason != attempt.Reason ||
		run.ParentRunID != attempt.ParentRunID {
		return fmt.Errorf("run %q implementation lineage: %w", run.ID, domain.ErrParentKeyMismatch)
	}
	if attempt.AttemptNumber == 1 {
		if err := tx.authenticateInitialAttemptAuthority(ctx, attempt); err != nil {
			return fmt.Errorf("run %q initial attempt authority: %w", run.ID, err)
		}
	}
	return nil
}

// authenticateInitialAttemptAuthority binds the mutable run/attempt rows to
// the immutable specification dispatch intent written at admission. Store owns
// this narrow wire projection to avoid an engine import cycle at the
// reconstruction trust boundary.
func (tx *ReadTx) authenticateInitialAttemptAuthority(ctx context.Context, attempt domain.ProductionAttempt) error {
	if attempt.AttemptNumber != 1 {
		return nil
	}
	key := initialAttemptAuthorityKey{
		campaignID: attempt.CampaignID, specificationRunID: attempt.SpecificationRunID,
		implementationRunID: attempt.ImplementationRunID, sourceDigest: attempt.SourceDigest,
		publicationDigest: attempt.PublicationDigest, approvedSpecDigest: attempt.ApprovedSpecDigest,
	}
	if tx.initialAttemptAuthorities[key] {
		return ctx.Err()
	}
	if err := tx.authenticateInitialAttemptAuthorityUncached(ctx, attempt); err != nil {
		return err
	}
	if tx.initialAttemptAuthorities != nil {
		tx.initialAttemptAuthorities[key] = true
	}
	return nil
}

// These are every caller-supplied coordinate read by the initial authority
// checks. The remaining evidence comes from the same immutable read snapshot.
// No publication recursion or mutable returned object is cached.
type initialAttemptAuthorityKey struct {
	campaignID          domain.CampaignID
	specificationRunID  domain.RunID
	implementationRunID domain.RunID
	sourceDigest        domain.Digest
	publicationDigest   domain.Digest
	approvedSpecDigest  domain.Digest
}

func (tx *ReadTx) authenticateInitialAttemptAuthorityUncached(ctx context.Context, attempt domain.ProductionAttempt) error {
	present, err := tx.authenticateInitialAttemptSource(ctx, attempt)
	if err != nil || !present {
		return err
	}
	if attempt.ApprovedSpecDigest != "" {
		return tx.authenticateInitialApprovedSpec(ctx, attempt)
	}
	return nil
}

// authenticateInitialAttemptSource binds the original source and run identities
// to the immutable dispatch. Migration uses this before task reconstruction is
// available; approval authentication remains a separate authority check.
func (tx *ReadTx) authenticateInitialAttemptSource(ctx context.Context, attempt domain.ProductionAttempt) (bool, error) {
	if attempt.RevisesRunID != nil {
		return tx.authenticateRevisionAttemptSource(ctx, attempt)
	}
	entry, err := tx.GetOutbox(ctx, string(domain.SpecificationInvocationID(attempt.SpecificationRunID, 1)))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// Admission reserves the attempt before a human or auto-start writes
			// the specification marker. A marker-absent row therefore has no
			// implementation run to expose yet; authenticate it once start makes
			// the immutable authority available.
			var exists int
			err := tx.tx.QueryRowContext(ctx, `SELECT 1 FROM runs WHERE id = ?`, attempt.ImplementationRunID).Scan(&exists)
			if errors.Is(err, sql.ErrNoRows) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			return false, domain.ErrParentKeyMismatch
		}
		return false, err
	}
	if entry.Kind != string(domain.SpecificationInvocationRequestedKind) {
		return false, domain.ErrParentKeyMismatch
	}
	var root struct {
		SpecificationRunID  domain.RunID        `json:"specification_run_id"`
		ImplementationRunID domain.RunID        `json:"implementation_run_id"`
		CampaignID          domain.CampaignID   `json:"campaign_id"`
		AttemptNumber       int                 `json:"attempt_number"`
		PublicationDigest   domain.Digest       `json:"publication_digest"`
		InputArtifactIDs    []domain.ArtifactID `json:"input_artifact_ids"`
	}
	if err := json.Unmarshal(entry.Payload, &root); err != nil ||
		root.SpecificationRunID != attempt.SpecificationRunID || root.ImplementationRunID != attempt.ImplementationRunID ||
		root.CampaignID != attempt.CampaignID || root.AttemptNumber != 1 ||
		root.PublicationDigest != attempt.PublicationDigest || len(root.InputArtifactIDs) != 1 {
		return false, domain.ErrParentKeyMismatch
	}
	source, err := tx.GetArtifact(ctx, root.InputArtifactIDs[0])
	if err != nil {
		return false, errors.Join(domain.ErrParentKeyMismatch, err)
	}
	if source.Digest != attempt.SourceDigest {
		return false, domain.ErrParentKeyMismatch
	}
	return true, nil
}

// FirstSpecificationMarkerKey returns the minimum-iteration specification
// dispatch marker for a run, or present=false when none is written yet. A
// revision campaign's specification run has no iteration-1 marker (its first
// request is the revision at iteration 2, #1083), so every root-marker lookup
// binds against this first marker rather than a fixed iteration 1.
func (tx *ReadTx) FirstSpecificationMarkerKey(
	ctx context.Context, specificationRunID domain.RunID,
) (string, bool, error) {
	prefix := domain.SpecificationInvocationIDPrefix(specificationRunID)
	rows, err := tx.tx.QueryContext(ctx, `
SELECT idempotency_key FROM outbox
WHERE kind IN (?, ?) AND idempotency_key LIKE ?`,
		string(domain.SpecificationInvocationRequestedKind),
		queueKindAlias(string(domain.SpecificationInvocationRequestedKind)), prefix+"%")
	if err != nil {
		return "", false, err
	}
	defer func() { _ = rows.Close() }()
	var (
		bestKey       string
		bestIteration int
		found         bool
	)
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return "", false, err
		}
		suffix := strings.TrimPrefix(key, prefix)
		if suffix == key {
			continue
		}
		iteration, convErr := strconv.Atoi(suffix)
		if convErr != nil || iteration < 1 {
			continue
		}
		if !found || iteration < bestIteration {
			bestKey, bestIteration, found = key, iteration, true
		}
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	return bestKey, found, nil
}

// authenticateRevisionAttemptSource authenticates a revision campaign's initial
// attempt against its first specification dispatch marker (#1083 D2, D3). Its
// first request seeds three inputs in role-canonical order (the original
// source, then the revised campaign's approved specification, then the answer's
// feedback artifact) at an iteration that continues the prior campaign's count.
// The marker must name exactly those seeded inputs, and each is bound to
// durable evidence: the source by digest, the prior specification by digest to
// the revised campaign's approved spec, and the feedback to the daemon-produced
// artifact of the linked command.
func (tx *ReadTx) authenticateRevisionAttemptSource(
	ctx context.Context, attempt domain.ProductionAttempt,
) (bool, error) {
	firstKey, present, err := tx.FirstSpecificationMarkerKey(ctx, attempt.SpecificationRunID)
	if err != nil {
		return false, err
	}
	if !present {
		// No marker yet: a revision campaign writes its attempt and run before
		// its iteration-2 marker within one transaction, so this pre-dispatch
		// state is legitimate while the implementation run is still absent
		// (matching the ordinary initial path). Once the implementation run
		// exists the immutable authority must too, so a marker-less attempt then
		// is inconsistent. The reverse projection additionally requires the
		// marker before treating a revision as a successor.
		var exists int
		err := tx.tx.QueryRowContext(ctx, `SELECT 1 FROM runs WHERE id = ?`, attempt.ImplementationRunID).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return false, domain.ErrParentKeyMismatch
	}
	entry, err := tx.GetOutbox(ctx, firstKey)
	if err != nil {
		return false, err
	}
	if entry.Kind != string(domain.SpecificationInvocationRequestedKind) {
		return false, domain.ErrParentKeyMismatch
	}
	var root struct {
		SpecificationRunID  domain.RunID        `json:"specification_run_id"`
		ImplementationRunID domain.RunID        `json:"implementation_run_id"`
		CampaignID          domain.CampaignID   `json:"campaign_id"`
		AttemptNumber       int                 `json:"attempt_number"`
		PublicationDigest   domain.Digest       `json:"publication_digest"`
		InputArtifactIDs    []domain.ArtifactID `json:"input_artifact_ids"`
		PriorSpecArtifactID *domain.ArtifactID  `json:"prior_spec_artifact_id"`
		FeedbackArtifactIDs []domain.ArtifactID `json:"feedback_artifact_ids"`
	}
	if err := json.Unmarshal(entry.Payload, &root); err != nil ||
		root.SpecificationRunID != attempt.SpecificationRunID || root.ImplementationRunID != attempt.ImplementationRunID ||
		root.CampaignID != attempt.CampaignID || root.AttemptNumber != 1 ||
		root.PublicationDigest != attempt.PublicationDigest || root.PriorSpecArtifactID == nil ||
		len(root.FeedbackArtifactIDs) != 1 || len(root.InputArtifactIDs) != 3 {
		return false, domain.ErrParentKeyMismatch
	}
	feedbackID := domain.ArtifactID("spec-feedback-" + *attempt.RevisionCommandID)
	if root.FeedbackArtifactIDs[0] != feedbackID ||
		root.InputArtifactIDs[1] != *root.PriorSpecArtifactID || root.InputArtifactIDs[2] != feedbackID {
		return false, domain.ErrParentKeyMismatch
	}
	source, err := tx.GetArtifact(ctx, root.InputArtifactIDs[0])
	if err != nil {
		return false, errors.Join(domain.ErrParentKeyMismatch, err)
	}
	if source.Digest != attempt.SourceDigest {
		return false, domain.ErrParentKeyMismatch
	}
	// Read the revised run's attempt without re-entering full reconstruction:
	// the cross-campaign revision link is not bounded by decreasing attempt
	// numbers the way the parent chain is, so recursing here would let mutually
	// referencing (corrupted) rows exhaust the stack instead of failing closed
	// (#1083). The scan still validates the row; the revised run authenticates
	// its own ancestry when it is read.
	revised, err := tx.productionAttemptByRun(ctx, *attempt.RevisesRunID)
	if err != nil {
		return false, err
	}
	priorSpec, err := tx.GetArtifact(ctx, *root.PriorSpecArtifactID)
	if err != nil {
		return false, errors.Join(domain.ErrParentKeyMismatch, err)
	}
	if revised.ApprovedSpecDigest == "" || priorSpec.Type != domain.ArtifactKindSpecification ||
		priorSpec.Digest != revised.ApprovedSpecDigest {
		return false, domain.ErrParentKeyMismatch
	}
	feedback, err := tx.GetArtifact(ctx, feedbackID)
	if err != nil {
		return false, errors.Join(domain.ErrParentKeyMismatch, err)
	}
	// Pin the full daemon-minted provenance tuple, mirroring the engine's
	// requireSpecificationOutputProvenance: the feedback artifact is minted at
	// the revision run's iteration-2 dispatch invocation, head-independent, with
	// no source head, no verification recipe, and normal sensitivity. A forged
	// row differing in any of those fields fails closed. The body-digest binding
	// stays with the engine, which owns the composition (see the note below).
	if feedback.Type != domain.ArtifactKindResearch ||
		feedback.Provenance.ProducerClass != domain.ProducerDaemon ||
		feedback.Provenance.ProducerInvocationID != domain.SpecificationInvocationID(attempt.SpecificationRunID, 2) ||
		feedback.Provenance.HeadBinding != domain.HeadIndependent ||
		feedback.Provenance.SourceHeadSHA != "" ||
		feedback.Provenance.VerificationRecipeDigest != nil ||
		feedback.Provenance.SensitivityClass != domain.SensitivityNormal {
		return false, domain.ErrParentKeyMismatch
	}
	// Bind the answering command as the blocked question's effective decision
	// (#1083, the store twin of the engine's verifyRevisionRoot re-gate): the
	// revision command must be an answer_and_retry that chose revise_specification
	// on the blocked run's implementation-stage agent_question, and be that item's
	// superseding decision. Fails closed so a reconstructed attempt cannot project
	// a revision the answer never authorized. The feedback body's digest binding
	// to the composed decision-and-answer stays with the engine, which owns the
	// composition; here the structural command and item bindings are the boundary.
	command, err := tx.GetCommand(ctx, *attempt.RevisionCommandID)
	if err != nil {
		return false, errors.Join(domain.ErrParentKeyMismatch, err)
	}
	item, err := tx.GetAttentionItemRecord(ctx, command.ItemID)
	if err != nil {
		return false, errors.Join(domain.ErrParentKeyMismatch, err)
	}
	if command.Action != domain.ActionAnswerAndRetry || command.AnswerRoute == nil ||
		*command.AnswerRoute != domain.AnswerRouteReviseSpecification ||
		item.Type != domain.AttentionAgentQuestion || item.AgentQuestion == nil ||
		item.AgentQuestion.Stage != domain.StageNameImplementation ||
		item.Subject.RunID == nil || *item.Subject.RunID != *attempt.RevisesRunID ||
		command.ItemVersion+1 != item.ItemVersion || command.PRHeadSHA != item.PRHeadSHA ||
		!slices.Equal(command.ArtifactDigests, item.ArtifactDigests) ||
		item.Status != domain.StatusSuperseded || item.DecidedAt == nil {
		return false, domain.ErrParentKeyMismatch
	}
	return true, nil
}

// authenticateInitialApprovedSpec reconstructs the specification terminal that
// authorized the initial implementation. The attempt and implementation run
// are mutually editable rows, so agreement between them is not approval
// evidence: the output artifact and, when approval is enabled, the resolved
// digest-bound command must independently name the stored digest.
func (tx *ReadTx) authenticateInitialApprovedSpec(
	ctx context.Context, attempt domain.ProductionAttempt,
) error {
	policy, err := tx.GetResolvedPolicy(ctx, attempt.SpecificationRunID)
	if err != nil {
		return err
	}
	var (
		specApproval bool
		foundGate    bool
	)
	for _, key := range policy.Keys {
		if key.Key != "gates.spec_approval" {
			continue
		}
		foundGate = true
		switch key.Value {
		case "true":
			specApproval = true
		case "false":
			specApproval = false
		default:
			return domain.ErrParentKeyMismatch
		}
	}
	if !foundGate {
		return domain.ErrParentKeyMismatch
	}
	rows, err := tx.tx.QueryContext(ctx, `
SELECT idempotency_key FROM outbox
WHERE kind IN (?, ?) AND idempotency_key LIKE ?`,
		string(domain.SpecificationInvocationRequestedKind), queueKindAlias(string(domain.SpecificationInvocationRequestedKind)),
		domain.SpecificationInvocationIDPrefix(attempt.SpecificationRunID)+"%")
	if err != nil {
		return err
	}
	keys := []string{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			_ = rows.Close()
			return err
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	for _, key := range keys {
		requestEntry, err := tx.GetOutbox(ctx, key)
		if err != nil || requestEntry.Kind != string(domain.SpecificationInvocationRequestedKind) ||
			!requestEntry.Dispatched() {
			continue
		}
		var request struct {
			SpecificationRunID  domain.RunID        `json:"specification_run_id"`
			ImplementationRunID domain.RunID        `json:"implementation_run_id"`
			InvocationID        domain.InvocationID `json:"invocation_id"`
			Iteration           int                 `json:"iteration"`
			CampaignID          domain.CampaignID   `json:"campaign_id"`
			AttemptNumber       int                 `json:"attempt_number"`
		}
		if err := json.Unmarshal(requestEntry.Payload, &request); err != nil || request.Iteration < 1 ||
			key != string(domain.SpecificationInvocationID(attempt.SpecificationRunID, request.Iteration)) ||
			request.SpecificationRunID != attempt.SpecificationRunID ||
			request.ImplementationRunID != attempt.ImplementationRunID ||
			request.InvocationID != domain.InvocationID(key) || request.CampaignID != attempt.CampaignID ||
			request.AttemptNumber != 1 {
			continue
		}

		terminalEntry, err := tx.GetInbox(ctx, key)
		if err != nil || terminalEntry.Kind != "specification_stage_terminal" {
			continue
		}
		var terminal struct {
			InvocationID   domain.InvocationID `json:"invocation_id"`
			Iteration      int                 `json:"iteration"`
			Status         string              `json:"status"`
			SpecArtifactID *domain.ArtifactID  `json:"spec_artifact_id"`
			ApprovalItemID *domain.ItemID      `json:"approval_item_id"`
			SummaryDigest  *domain.Digest      `json:"summary_digest"`
		}
		if err := json.Unmarshal(terminalEntry.Payload, &terminal); err != nil ||
			terminal.InvocationID != request.InvocationID || terminal.Iteration != request.Iteration ||
			terminal.Status != "completed" || terminal.SpecArtifactID == nil ||
			*terminal.SpecArtifactID != domain.ArtifactID(fmt.Sprintf(
				"spec-%s-%d", attempt.ImplementationRunID, request.Iteration)) {
			continue
		}
		specification, err := tx.GetArtifact(ctx, *terminal.SpecArtifactID)
		if err != nil || specification.Type != domain.ArtifactKindSpecification ||
			specification.Digest != attempt.ApprovedSpecDigest ||
			specification.Provenance.ProducerClass != domain.ProducerAgent ||
			specification.Provenance.ProducerInvocationID != request.InvocationID ||
			specification.Provenance.HeadBinding != domain.HeadIndependent {
			continue
		}
		if terminal.ApprovalItemID == nil && !specApproval {
			return nil
		}
		if terminal.ApprovalItemID == nil || !specApproval {
			continue
		}
		expectedItemID := domain.ItemID(fmt.Sprintf(
			"spec-approval-%s-%d", attempt.ImplementationRunID, request.Iteration))
		if *terminal.ApprovalItemID != expectedItemID {
			continue
		}
		item, err := tx.GetAttentionItem(ctx, expectedItemID)
		if err != nil || item.Type != domain.AttentionSpecApproval || item.Status != domain.StatusResolved ||
			item.Subject.Type != domain.SubjectRun || item.Subject.ID != domain.SubjectID(attempt.SpecificationRunID) ||
			item.Subject.RunID == nil || *item.Subject.RunID != attempt.SpecificationRunID ||
			!authenticSpecificationApprovalDecisionSet(item.RequestedDecision) ||
			len(item.EvidenceSnapshot) != 0 ||
			item.PRHeadSHA != "" {
			continue
		}
		if !authenticatesInitialApprovalClaims(
			item, specification, terminal.SummaryDigest, attempt.ImplementationRunID, request.Iteration,
		) {
			continue
		}
		commands, err := tx.ListCommandsForItem(ctx, item.ID)
		if err != nil {
			continue
		}
		var decision *domain.Command
		for index := range commands {
			command := &commands[index]
			if command.Action != domain.ActionApprove && command.Action != domain.ActionRequestChanges &&
				command.Action != domain.ActionStop {
				continue
			}
			if decision != nil {
				decision = nil
				break
			}
			decision = command
		}
		if decision != nil && decision.Action == domain.ActionApprove && decision.ItemID == item.ID &&
			decision.ItemVersion+1 == item.ItemVersion && decision.PRHeadSHA == item.PRHeadSHA &&
			slices.Equal(decision.ArtifactDigests, item.ArtifactDigests) && decision.Message == "" &&
			len(decision.Attachments) == 0 {
			return nil
		}
	}
	return domain.ErrParentKeyMismatch
}

func authenticSpecificationApprovalDecisionSet(actions []domain.Action) bool {
	return slices.Equal(actions,
		[]domain.Action{domain.ActionApprove, domain.ActionRequestChanges, domain.ActionStop}) ||
		slices.Equal(actions,
			[]domain.Action{domain.ActionApprove, domain.ActionRequestChanges, domain.ActionDiscuss, domain.ActionStop})
}

func authenticatesInitialApprovalClaims(
	item domain.AttentionItem,
	specification domain.Artifact,
	summaryDigest *domain.Digest,
	implementationRunID domain.RunID,
	iteration int,
) bool {
	if len(item.AgentClaims) < 1 || len(item.AgentClaims) > 3 {
		return false
	}
	claim := item.AgentClaims[0]
	if claim.Label != "Specification" || claim.Artifact != specification.ID ||
		claim.Digest != specification.Digest ||
		!reflect.DeepEqual(claim.Provenance, specification.Provenance) {
		return false
	}
	expectedDigests := []domain.Digest{specification.Digest}
	if len(item.AgentClaims) >= 2 {
		summary := item.AgentClaims[1]
		expectedSummaryID := domain.ArtifactID(fmt.Sprintf(
			"spec-summary-%s-%d", implementationRunID, iteration))
		if summaryDigest == nil || summary.Digest != *summaryDigest ||
			summary.Label != summaryEvidenceLabel || summary.Artifact != expectedSummaryID ||
			summary.Text == nil || summary.Text.MediaType != domain.MediaTypeTextMarkdown ||
			summary.Provenance != specification.Provenance {
			return false
		}
		expectedDigests = append(expectedDigests, summary.Digest)
	} else if summaryDigest != nil {
		return false
	}
	if len(item.AgentClaims) == 3 {
		addressals := item.AgentClaims[2]
		expectedAddressalsID := domain.ArtifactID(fmt.Sprintf(
			"spec-addressals-%s-%d", implementationRunID, iteration))
		if item.SpecRevision == nil || addressals.Label != "Addressals" ||
			addressals.Artifact != expectedAddressalsID || addressals.Text != nil ||
			addressals.Digest != item.SpecRevision.AddressalsDigest ||
			addressals.Provenance != specification.Provenance {
			return false
		}
		expectedDigests = append(expectedDigests, addressals.Digest)
	}
	slices.Sort(expectedDigests)
	expectedDigests = slices.Compact(expectedDigests)
	return slices.Equal(item.ArtifactDigests, expectedDigests)
}

// PutProductionAttempt records one campaign attempt. A new ordinal must be
// exactly the next ordinal currently stored for the campaign; SQLite's write
// transaction serializes concurrent allocators, so clients never choose or
// race the monotonic number. An exact existing record is an idempotent replay.
func (tx *WriteTx) PutProductionAttempt(ctx context.Context, attempt domain.ProductionAttempt) error {
	if err := attempt.Validate(); err != nil {
		return fmt.Errorf("put production attempt: %w", err)
	}
	existing, err := tx.GetProductionAttempt(ctx, attempt.CampaignID, attempt.AttemptNumber)
	if err == nil {
		if reflect.DeepEqual(existing, attempt) {
			return nil
		}
		return fmt.Errorf("put production attempt %s/%d: %w",
			attempt.CampaignID, attempt.AttemptNumber, ErrImmutableConflict)
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	if attempt.PublicationDigest == "" {
		return fmt.Errorf("put production attempt %s/%d publication_digest: %w",
			attempt.CampaignID, attempt.AttemptNumber, domain.ErrEmptyField)
	}
	var latest int
	if err := tx.tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(attempt_number), 0) FROM production_attempts WHERE campaign_id = ?`,
		attempt.CampaignID).Scan(&latest); err != nil {
		return fmt.Errorf("put production attempt %s/%d: %w", attempt.CampaignID, attempt.AttemptNumber, err)
	}
	if attempt.AttemptNumber != latest+1 {
		return fmt.Errorf("put production attempt %s/%d after %d: %w",
			attempt.CampaignID, attempt.AttemptNumber, latest, domain.ErrNonContiguous)
	}
	if attempt.AttemptNumber == 1 && attempt.ApprovedSpecDigest != "" {
		return fmt.Errorf("put production attempt %s/%d pre-approved: %w",
			attempt.CampaignID, attempt.AttemptNumber, domain.ErrImmutableTransition)
	}
	if attempt.AttemptNumber == 1 && (attempt.CampaignID != derivedInitialCampaignID(attempt.ImplementationRunID) ||
		!domain.SpecificationRunIDMatchesImplementation(attempt.SpecificationRunID, attempt.ImplementationRunID)) {
		return fmt.Errorf("put production attempt %s/%d initial identity: %w",
			attempt.CampaignID, attempt.AttemptNumber, domain.ErrParentKeyMismatch)
	}
	if attempt.AttemptNumber > 1 {
		if attempt.ImplementationRunID != derivedRetryImplementationRunID(attempt.CampaignID, attempt.AttemptNumber) {
			return fmt.Errorf("put production attempt %s/%d implementation run identity: %w",
				attempt.CampaignID, attempt.AttemptNumber, domain.ErrParentKeyMismatch)
		}
		parent, err := tx.GetProductionAttemptByRun(ctx, attempt.ParentRunID)
		if err != nil {
			return fmt.Errorf("put production attempt %s/%d parent: %w",
				attempt.CampaignID, attempt.AttemptNumber, err)
		}
		if parent.AttemptNumber >= attempt.AttemptNumber || parent.CampaignID != attempt.CampaignID || parent.ImplementationRunID != attempt.ParentRunID ||
			parent.SpecificationRunID != attempt.SpecificationRunID ||
			parent.ApprovedSpecDigest == "" || parent.SourceDigest != attempt.SourceDigest ||
			parent.PublicationDigest != attempt.PublicationDigest ||
			parent.ApprovedSpecDigest != attempt.ApprovedSpecDigest {
			return fmt.Errorf("put production attempt %s/%d parent lineage: %w",
				attempt.CampaignID, attempt.AttemptNumber, domain.ErrParentKeyMismatch)
		}
	}
	if err := tx.authenticateCapabilityRetryAttempt(ctx, attempt); err != nil {
		return fmt.Errorf("put production attempt %s/%d capability retry: %w",
			attempt.CampaignID, attempt.AttemptNumber, err)
	}
	if err := tx.gateRevisionRevisedRun(ctx, attempt); err != nil {
		return fmt.Errorf("put production attempt %s/%d revision link: %w",
			attempt.CampaignID, attempt.AttemptNumber, err)
	}
	body, err := encode(attempt)
	if err != nil {
		return fmt.Errorf("put production attempt %s/%d: %w", attempt.CampaignID, attempt.AttemptNumber, err)
	}
	_, err = tx.tx.ExecContext(ctx, `
INSERT INTO production_attempts (
    campaign_id, attempt_number, kind, parent_run_id, source_digest, publication_digest,
    approved_spec_digest, specification_run_id, implementation_run_id,
    reason, as_of_revision, body
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		attempt.CampaignID, attempt.AttemptNumber, attempt.Kind,
		nullableString(string(attempt.ParentRunID)), attempt.SourceDigest, nullableString(string(attempt.PublicationDigest)),
		nullableString(string(attempt.ApprovedSpecDigest)), attempt.SpecificationRunID,
		attempt.ImplementationRunID, attempt.Reason, tx.asOfRevision, body)
	if err != nil {
		return fmt.Errorf("put production attempt %s/%d: %w", attempt.CampaignID, attempt.AttemptNumber, err)
	}
	// Mirror the revision link into its reverse index. A revision attempt exists
	// only after the column's migration, so the update is skipped for the
	// ordinary attempts that legacy backfills reconstruct, keeping the base
	// insert schema-agnostic.
	if attempt.RevisesRunID != nil {
		if _, err := tx.tx.ExecContext(ctx,
			`UPDATE production_attempts SET revises_run_id = ? WHERE campaign_id = ? AND attempt_number = ?`,
			string(*attempt.RevisesRunID), attempt.CampaignID, attempt.AttemptNumber); err != nil {
			return fmt.Errorf("put production attempt %s/%d revision index: %w",
				attempt.CampaignID, attempt.AttemptNumber, err)
		}
	}
	return nil
}

// gateRevisionRevisedRun binds a revision campaign's initial attempt to the
// blocked implementation run it revises (#1083 D2). The revised run must be a
// terminal implementation run carrying its own recorded production attempt, and
// in a different campaign than the one being written. Task equality is not
// checked here: at write time the new run's task is not yet assigned, so
// campaignTask enforces it when the new specification and implementation runs
// are gated, deriving their expected task from this link. Returned rows are
// never trusted: the revised run's attempt is reconstructed through
// GetProductionAttemptByRun and its terminal through the run's own milestones.
func (tx *ReadTx) gateRevisionRevisedRun(ctx context.Context, attempt domain.ProductionAttempt) error {
	if attempt.RevisesRunID == nil {
		return nil
	}
	// Read the revised run's attempt without re-entering full reconstruction:
	// the cross-campaign revision link is not bounded by decreasing attempt
	// numbers the way the parent chain is, so recursing here would let mutually
	// referencing (corrupted) rows exhaust the stack instead of failing closed
	// (#1083). The scan still validates the row; the revised run authenticates
	// its own ancestry when it is read.
	revised, err := tx.productionAttemptByRun(ctx, *attempt.RevisesRunID)
	if err != nil {
		return err
	}
	if revised.ImplementationRunID != *attempt.RevisesRunID || revised.CampaignID == attempt.CampaignID {
		return domain.ErrParentKeyMismatch
	}
	milestones, err := tx.ListRunMilestones(ctx, *attempt.RevisesRunID)
	if err != nil {
		return err
	}
	for _, milestone := range milestones {
		if milestone.Kind == domain.MilestoneTerminalRecorded &&
			milestone.Terminal != nil && milestone.Terminal.Concluded() {
			return nil
		}
	}
	return domain.ErrParentKeyMismatch
}

// ApproveProductionAttempt fills the one field unavailable at initial submit.
// It may move only from empty to the authenticated approved digest; exact
// replay converges and every other rewrite fails closed.
func (tx *WriteTx) ApproveProductionAttempt(
	ctx context.Context, campaignID domain.CampaignID, number int, approved domain.Digest,
) (domain.ProductionAttempt, error) {
	attempt, err := tx.GetProductionAttempt(ctx, campaignID, number)
	if err != nil {
		return domain.ProductionAttempt{}, err
	}
	if approved == "" {
		return domain.ProductionAttempt{}, fmt.Errorf("approve production attempt %s/%d: %w",
			campaignID, number, domain.ErrEmptyField)
	}
	if attempt.ApprovedSpecDigest == approved {
		return attempt, nil
	}
	if attempt.ApprovedSpecDigest != "" {
		return domain.ProductionAttempt{}, fmt.Errorf("approve production attempt %s/%d: %w",
			campaignID, number, ErrImmutableConflict)
	}
	attempt.ApprovedSpecDigest = approved
	if err := attempt.Validate(); err != nil {
		return domain.ProductionAttempt{}, fmt.Errorf("approve production attempt %s/%d: %w", campaignID, number, err)
	}
	body, err := encode(attempt)
	if err != nil {
		return domain.ProductionAttempt{}, err
	}
	result, err := tx.tx.ExecContext(ctx, `
UPDATE production_attempts
SET approved_spec_digest = ?, body = ?
WHERE campaign_id = ? AND attempt_number = ? AND approved_spec_digest IS NULL`,
		approved, body, campaignID, number)
	if err != nil {
		return domain.ProductionAttempt{}, fmt.Errorf("approve production attempt %s/%d: %w", campaignID, number, err)
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return domain.ProductionAttempt{}, fmt.Errorf("approve production attempt %s/%d changed %d rows: %w",
			campaignID, number, changed, ErrImmutableConflict)
	}
	return attempt, nil
}

func (tx *ReadTx) scanProductionAttempt(sc scanner) (domain.ProductionAttempt, error) {
	var (
		campaignID, kind, sourceDigest, specificationRunID, implementationRunID, reason string
		number                                                                          int
		parentRunID, publicationDigest, approvedSpecDigest                              sql.NullString
		asOfRevision                                                                    int64
		body                                                                            []byte
	)
	if err := sc.Scan(&campaignID, &number, &kind, &parentRunID, &sourceDigest, &publicationDigest,
		&approvedSpecDigest, &specificationRunID, &implementationRunID, &reason,
		&asOfRevision, &body); err != nil {
		return domain.ProductionAttempt{}, err
	}
	attempt, err := decode[domain.ProductionAttempt](body)
	if err != nil {
		return domain.ProductionAttempt{}, err
	}
	// RevisesRunID/RevisionCommandID stay in the body only, like the operator
	// retry bindings. The revises_run_id column is an unread reverse index that
	// PutProductionAttempt mirrors from the body; RevisionSpecificationRunFor
	// re-reads and re-verifies the body, so the scan needs no column cross-check
	// here and the shared scanner stays migration-safe for the version-70
	// backfill that predates the column.
	if attempt.CampaignID != domain.CampaignID(campaignID) || attempt.AttemptNumber != number ||
		attempt.Kind != domain.ProductionAttemptKind(kind) ||
		!optionalStringEqual(parentRunID, string(attempt.ParentRunID)) ||
		attempt.SourceDigest != domain.Digest(sourceDigest) ||
		!optionalStringEqual(publicationDigest, string(attempt.PublicationDigest)) ||
		!optionalStringEqual(approvedSpecDigest, string(attempt.ApprovedSpecDigest)) ||
		attempt.SpecificationRunID != domain.RunID(specificationRunID) ||
		attempt.ImplementationRunID != domain.RunID(implementationRunID) ||
		attempt.Reason != reason || asOfRevision < 1 {
		return domain.ProductionAttempt{}, errRowInconsistent
	}
	if err := attempt.Validate(); err != nil {
		return domain.ProductionAttempt{}, errRowInconsistent
	}
	if attempt.AttemptNumber > 1 &&
		attempt.ImplementationRunID != derivedRetryImplementationRunID(attempt.CampaignID, attempt.AttemptNumber) {
		return domain.ProductionAttempt{}, errRowInconsistent
	}
	if attempt.AttemptNumber == 1 && (attempt.CampaignID != derivedInitialCampaignID(attempt.ImplementationRunID) ||
		!domain.SpecificationRunIDMatchesImplementation(attempt.SpecificationRunID, attempt.ImplementationRunID)) {
		return domain.ProductionAttempt{}, errRowInconsistent
	}
	return attempt, nil
}

const productionAttemptColumns = `campaign_id, attempt_number, kind, parent_run_id,
source_digest, publication_digest, approved_spec_digest, specification_run_id, implementation_run_id,
reason, as_of_revision, body`

func (tx *ReadTx) GetProductionAttempt(
	ctx context.Context, campaignID domain.CampaignID, number int,
) (domain.ProductionAttempt, error) {
	attempt, err := tx.scanProductionAttempt(tx.tx.QueryRowContext(ctx,
		`SELECT `+productionAttemptColumns+` FROM production_attempts
         WHERE campaign_id = ? AND attempt_number = ?`, campaignID, number))
	if err != nil {
		return domain.ProductionAttempt{}, fmt.Errorf("get production attempt %s/%d: %w",
			campaignID, number, notFoundOr(err))
	}
	if err := tx.authenticateReconstructedProductionAttempt(ctx, attempt); err != nil {
		return domain.ProductionAttempt{}, fmt.Errorf("get production attempt %s/%d: %w", campaignID, number, err)
	}
	return attempt, nil
}

func (tx *ReadTx) authenticateReconstructedProductionAttempt(ctx context.Context, attempt domain.ProductionAttempt) error {
	if attempt.AttemptNumber == 1 {
		if err := tx.gateRevisionRevisedRun(ctx, attempt); err != nil {
			return err
		}
		return tx.authenticateInitialAttemptAuthority(ctx, attempt)
	}
	if attempt.AttemptNumber < 2 {
		return domain.ErrParentKeyMismatch
	}
	if err := tx.authenticateCapabilityRetryAttempt(ctx, attempt); err != nil {
		return err
	}
	parent, err := tx.productionAttemptByRun(ctx, attempt.ParentRunID)
	if err != nil {
		return err
	}
	if parent.AttemptNumber >= attempt.AttemptNumber || parent.CampaignID != attempt.CampaignID || parent.ImplementationRunID != attempt.ParentRunID ||
		parent.SpecificationRunID != attempt.SpecificationRunID || parent.ApprovedSpecDigest == "" ||
		parent.SourceDigest != attempt.SourceDigest || parent.PublicationDigest != attempt.PublicationDigest ||
		parent.ApprovedSpecDigest != attempt.ApprovedSpecDigest {
		return domain.ErrParentKeyMismatch
	}
	return tx.authenticateReconstructedProductionAttempt(ctx, parent)
}

func (tx *ReadTx) authenticateCapabilityRetryAttempt(
	ctx context.Context, attempt domain.ProductionAttempt,
) error {
	if attempt.OperatorCommandID == nil {
		return nil
	}
	command, err := tx.GetCommand(ctx, *attempt.OperatorCommandID)
	if err != nil {
		return err
	}
	item, err := tx.GetAttentionItemRecord(ctx, command.ItemID)
	if err != nil {
		return err
	}
	if command.Action != domain.ActionRetryWithCapability ||
		attempt.RetryOfInvocationID == nil || attempt.CapabilityManifestDigest == nil ||
		command.Message != string(*attempt.CapabilityManifestDigest) ||
		command.ItemID != item.ID || command.ItemVersion+1 != item.ItemVersion ||
		command.PRHeadSHA != item.PRHeadSHA ||
		!slices.Equal(command.ArtifactDigests, item.ArtifactDigests) ||
		item.Status != domain.StatusSuperseded || item.DecidedAt == nil ||
		item.Type != domain.AttentionExecutionFailure || item.ExecutionFailure == nil ||
		item.ExecutionFailure.Stage != domain.StageNameImplementation ||
		item.ExecutionFailure.InvocationID != *attempt.RetryOfInvocationID ||
		item.Subject.RunID == nil || *item.Subject.RunID != attempt.ParentRunID ||
		!slices.ContainsFunc(item.ExecutionFailure.OfferedManifests,
			func(offer domain.CapabilityManifestOffer) bool {
				return offer.Digest == *attempt.CapabilityManifestDigest
			}) {
		return domain.ErrParentKeyMismatch
	}
	policy, err := tx.GetResolvedPolicy(ctx, attempt.ParentRunID)
	if err != nil {
		return err
	}
	manifests, err := domain.CapabilityManifestsFromPolicy(policy)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(manifests, func(manifest domain.CapabilityManifest) bool {
		return manifest.Digest == *attempt.CapabilityManifestDigest &&
			slices.Contains(item.ExecutionFailure.OfferedManifests, manifest.Offer())
	}) {
		return domain.ErrParentKeyMismatch
	}
	return nil
}

func (tx *ReadTx) GetProductionAttemptByRun(
	ctx context.Context, runID domain.RunID,
) (domain.ProductionAttempt, error) {
	attempt, err := tx.productionAttemptByRun(ctx, runID)
	if err != nil {
		return domain.ProductionAttempt{}, err
	}
	if err := tx.authenticateReconstructedProductionAttempt(ctx, attempt); err != nil {
		return domain.ProductionAttempt{}, fmt.Errorf("get production attempt for run %q: %w", runID, err)
	}
	return attempt, nil
}

func (tx *ReadTx) productionAttemptByRun(
	ctx context.Context, runID domain.RunID,
) (domain.ProductionAttempt, error) {
	attempt, err := tx.scanProductionAttempt(tx.tx.QueryRowContext(ctx,
		`SELECT `+productionAttemptColumns+` FROM production_attempts WHERE implementation_run_id = ?`, runID))
	if err != nil {
		return domain.ProductionAttempt{}, fmt.Errorf("get production attempt for run %q: %w", runID, notFoundOr(err))
	}
	return attempt, nil
}

// RevisionSpecificationRunFor returns the specification run of the revision
// campaign that revises the given implementation run, if one exists (#1083 D4).
// The link lives on the revision campaign's initial attempt; the attempt is
// re-authenticated through GetProductionAttempt (returned-object trust
// boundary), so a forged row cannot fabricate a successor.
func (tx *ReadTx) RevisionSpecificationRunFor(
	ctx context.Context, revisedRunID domain.RunID,
) (domain.RunID, bool, error) {
	var (
		campaignID domain.CampaignID
		number     int
	)
	err := tx.tx.QueryRowContext(ctx,
		`SELECT campaign_id, attempt_number FROM production_attempts WHERE revises_run_id = ?`,
		string(revisedRunID)).Scan(&campaignID, &number)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	attempt, err := tx.GetProductionAttempt(ctx, campaignID, number)
	if err != nil {
		return "", false, err
	}
	if attempt.RevisesRunID == nil || *attempt.RevisesRunID != revisedRunID {
		return "", false, errRowInconsistent
	}
	// Only project a materialized revision as a successor. GetProductionAttempt
	// accepts a pre-dispatch attempt (marker not yet written), which is a
	// legitimate transient during creation but, on a committed row, means an
	// inconsistent revision whose specification run was never dispatched. Require
	// the marker so a corrupt marker-less row is not projected as a nonexistent
	// superseded_by (#1083).
	_, present, err := tx.FirstSpecificationMarkerKey(ctx, attempt.SpecificationRunID)
	if err != nil {
		return "", false, err
	}
	if !present {
		return "", false, nil
	}
	return attempt.SpecificationRunID, true, nil
}

func (tx *ReadTx) LatestProductionAttempt(
	ctx context.Context, campaignID domain.CampaignID,
) (domain.ProductionAttempt, error) {
	attempt, err := tx.scanProductionAttempt(tx.tx.QueryRowContext(ctx,
		`SELECT `+productionAttemptColumns+` FROM production_attempts
         WHERE campaign_id = ? ORDER BY attempt_number DESC LIMIT 1`, campaignID))
	if err != nil {
		return domain.ProductionAttempt{}, fmt.Errorf("latest production attempt %q: %w",
			campaignID, notFoundOr(err))
	}
	if err := tx.authenticateReconstructedProductionAttempt(ctx, attempt); err != nil {
		return domain.ProductionAttempt{}, fmt.Errorf("latest production attempt %q: %w", campaignID, err)
	}
	return attempt, nil
}
