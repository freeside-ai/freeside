package domain

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Transition validators answer a different question than the field-level
// Validate methods: not "is this value well-formed" but "is this a legal
// successor to what is already stored". They enforce the immutability and
// forward-only rules a persisted aggregate obeys between its stored version and
// an update (plan §5.3, §5.14, §4), so any writer (the store today, the engine
// or importer later) reuses one definition instead of re-deriving it.
//
// A transition is between two versions of the same aggregate: each validator
// first rejects a change of identity (the aggregate's key), so a caller that
// does not fetch old by key cannot pass a different aggregate as a successor.
//
// Each failure wraps one of two classes, so a caller maps it onto its own
// boundary errors without string matching:
//   - ErrImmutableTransition: identity, another fixed field, recorded
//     history, or a terminal lifecycle outcome would change.
//   - ErrStaleTransition: an update fails to advance a version or lifecycle.
//
// A byte-identical replay (a retried write) is the caller's concern, not these
// validators': callers that converge identical writes must short-circuit before
// calling a validator, since an unchanged update does not advance a version and
// these would reject it.

// ValidateRunTransition reports whether updated is a legal successor to the
// stored run old. Project, approved spec, and resolved policy are fixed at run
// creation (plan §5.3 binds a run to its spec and policy digests), and
// stages/attempts are recorded history: an update may only append.
func ValidateRunTransition(old, updated Run) error {
	if updated.ID != old.ID {
		return fmt.Errorf("run %s: identity would change from %s: %w", updated.ID, old.ID, ErrImmutableTransition)
	}
	if updated.ProjectID != old.ProjectID || updated.SpecDigest != old.SpecDigest ||
		updated.PolicyDigest != old.PolicyDigest || updated.CampaignID != old.CampaignID ||
		updated.AttemptNumber != old.AttemptNumber || updated.AttemptReason != old.AttemptReason ||
		updated.ParentRunID != old.ParentRunID || !stagesExtend(old.Stages, updated.Stages) {
		return fmt.Errorf("run %s: fixed bindings or recorded history would change: %w", updated.ID, ErrImmutableTransition)
	}
	return nil
}

// ValidateConversationTransition reports whether updated is a legal successor to
// the stored conversation old. Messages are immutable and corrections are new
// messages (plan §5.14): an update must carry every stored message unchanged and
// may only append.
func ValidateConversationTransition(old, updated Conversation) error {
	if updated.ID != old.ID {
		return fmt.Errorf("conversation %s: identity would change from %s: %w", updated.ID, old.ID, ErrImmutableTransition)
	}
	if len(updated.Messages) < len(old.Messages) {
		return fmt.Errorf("conversation %s: stored messages would be dropped: %w", updated.ID, ErrImmutableTransition)
	}
	same, err := jsonEqual(old.Messages, updated.Messages[:len(old.Messages)])
	if err != nil {
		return fmt.Errorf("conversation %s: %w", updated.ID, err)
	}
	if !same {
		return fmt.Errorf("conversation %s: stored messages would be rewritten: %w", updated.ID, ErrImmutableTransition)
	}
	return nil
}

// itemStatusSuccessors returns the statuses a version-advancing update may
// move status to. A same-status update is always legal; an unlisted pair is
// not. The terminal statuses (resolved, superseded, dismissed, expired) admit
// no successors: an item's recorded final outcome never reopens, a fresh
// decision is a new item (plan §4 lifecycle). A switch, not a map, so the
// exhaustive linter forces a future status to declare its successors instead
// of silently defaulting to terminal.
func itemStatusSuccessors(status ItemStatus) []ItemStatus {
	switch status {
	case StatusOpen:
		return []ItemStatus{StatusResolved, StatusSuperseded, StatusDismissed, StatusExpired}
	case StatusResolved, StatusSuperseded, StatusDismissed, StatusExpired:
		return nil
	}
	return nil
}

// ValidateAttentionItemTransition reports whether updated is a legal successor to
// the stored item old. What an item is about is fixed at creation: transitions
// bump item_version and evolve status/evidence on the same identity, and a
// different subject or type is a new (superseding) item, never a retarget (plan
// §4, §5.14). A changed body must move the version forward, or a stale copy could
// roll back a later transition (a resolved v2 overwritten by an open v1). Status
// moves follow itemStatusSuccessors: a terminal status is final.
func ValidateAttentionItemTransition(old, updated AttentionItem) error {
	if updated.ID != old.ID {
		return fmt.Errorf("attention item %s: identity would change from %s: %w", updated.ID, old.ID, ErrImmutableTransition)
	}
	sameSubject, err := jsonEqual(old.Subject, updated.Subject)
	if err != nil {
		return fmt.Errorf("attention item %s: %w", updated.ID, err)
	}
	if updated.ProjectID != old.ProjectID || updated.Type != old.Type || !sameSubject {
		return fmt.Errorf("attention item %s: fixed bindings would change: %w", updated.ID, ErrImmutableTransition)
	}
	if updated.ItemVersion <= old.ItemVersion {
		return fmt.Errorf("attention item %s: item_version %d does not advance stored %d: %w",
			updated.ID, updated.ItemVersion, old.ItemVersion, ErrStaleTransition)
	}
	if updated.Status != old.Status && !slices.Contains(itemStatusSuccessors(old.Status), updated.Status) {
		return fmt.Errorf("attention item %s: status %q is terminal and cannot become %q: %w",
			updated.ID, old.Status, updated.Status, ErrImmutableTransition)
	}
	// Creation is an identity-bearing fact, not lifecycle state: live items
	// receive it at construction, while a legacy nil remains nil forever.
	// Neither side may add, move, or erase the stamp during an update.
	if !timesEqual(updated.CreatedAt, old.CreatedAt) {
		return fmt.Errorf("attention item %s: recorded created_at would change: %w",
			updated.ID, ErrImmutableTransition)
	}
	sameReadiness, err := jsonEqual(old.Readiness, updated.Readiness)
	if err != nil {
		return fmt.Errorf("attention item %s: %w", updated.ID, err)
	}
	if !sameReadiness {
		return fmt.Errorf("attention item %s: readiness summary would change: %w",
			updated.ID, ErrImmutableTransition)
	}
	sameYieldHistory, err := jsonEqual(old.YieldHistory, updated.YieldHistory)
	if err != nil {
		return fmt.Errorf("attention item %s: %w", updated.ID, err)
	}
	if !sameYieldHistory {
		return fmt.Errorf("attention item %s: review yield history would change: %w",
			updated.ID, ErrImmutableTransition)
	}
	// A recorded decision instant is the durable endpoint of the
	// open-to-decision metric (issue #171): once stamped it never moves or
	// disappears, so a writer holding a constructor-built copy (which always
	// carries nil DecidedAt) cannot silently erase it. Stamping (nil → set)
	// is the concluding transaction's legal move.
	if old.DecidedAt != nil && !timesEqual(updated.DecidedAt, old.DecidedAt) {
		return fmt.Errorf("attention item %s: recorded decided_at would change: %w",
			updated.ID, ErrImmutableTransition)
	}
	// Health posture is part of the item's fixed meaning: changing it in place
	// would silently add or remove an admission gate without a new observation.
	samePosture, err := jsonEqual(old.Posture, updated.Posture)
	if err != nil {
		return fmt.Errorf("attention item %s: %w", updated.ID, err)
	}
	if !samePosture {
		return fmt.Errorf("attention item %s: health posture would change: %w",
			updated.ID, ErrImmutableTransition)
	}
	// The supersession condition is part of what the item is about, fixed at
	// creation like type and subject: a later write may neither add, remove,
	// nor retarget it, or a stale copy could turn a conditionally non-blocking
	// notice into an unconditional blocker (or the reverse) without any
	// configuration change.
	sameSupersession, err := jsonEqual(old.BlockingSupersession, updated.BlockingSupersession)
	if err != nil {
		return fmt.Errorf("attention item %s: %w", updated.ID, err)
	}
	if !sameSupersession {
		return fmt.Errorf("attention item %s: blocking supersession would change: %w",
			updated.ID, ErrImmutableTransition)
	}
	sameReviewRecovery, err := jsonEqual(old.ReviewRecoveryBinding, updated.ReviewRecoveryBinding)
	if err != nil {
		return fmt.Errorf("attention item %s: %w", updated.ID, err)
	}
	if !sameReviewRecovery {
		return fmt.Errorf("attention item %s: review recovery binding would change: %w",
			updated.ID, ErrImmutableTransition)
	}
	// A revoked-identity marker gains its verified re-enrollment binding only
	// after the journal reaches a verified terminal outcome. That projection is
	// one-way: replacing or removing the coordinates would retarget an already
	// rendered recovery action.
	if old.CodexReenrollmentRecoveryBinding != nil {
		sameReenrollment, err := jsonEqual(old.CodexReenrollmentRecoveryBinding, updated.CodexReenrollmentRecoveryBinding)
		if err != nil {
			return fmt.Errorf("attention item %s: %w", updated.ID, err)
		}
		if !sameReenrollment {
			return fmt.Errorf("attention item %s: codex re-enrollment recovery binding would change: %w",
				updated.ID, ErrImmutableTransition)
		}
	} else if updated.CodexReenrollmentRecoveryBinding != nil &&
		(old.Status != StatusOpen || updated.Status != StatusOpen || old.Type != AttentionSystemHealth) {
		return fmt.Errorf("attention item %s: codex re-enrollment recovery binding cannot be attached in this transition: %w",
			updated.ID, ErrImmutableTransition)
	}
	sameConfigRecovery, err := jsonEqual(old.ReviewConfigurationRecovery, updated.ReviewConfigurationRecovery)
	if err != nil {
		return fmt.Errorf("attention item %s: %w", updated.ID, err)
	}
	if !sameConfigRecovery {
		return fmt.Errorf("attention item %s: review configuration recovery binding would change: %w",
			updated.ID, ErrImmutableTransition)
	}
	sameFindingAdjudication, err := jsonEqual(old.FindingAdjudication, updated.FindingAdjudication)
	if err != nil {
		return fmt.Errorf("attention item %s: %w", updated.ID, err)
	}
	if !sameFindingAdjudication {
		return fmt.Errorf("attention item %s: finding adjudication binding would change: %w",
			updated.ID, ErrImmutableTransition)
	}
	if old.BillableCostSoFar != nil {
		if updated.BillableCostSoFar == nil ||
			updated.BillableCostSoFar.Currency != old.BillableCostSoFar.Currency ||
			updated.BillableCostSoFar.Invocations < old.BillableCostSoFar.Invocations ||
			compareCanonicalDecimal(
				updated.BillableCostSoFar.Amount, old.BillableCostSoFar.Amount,
			) < 0 ||
			(old.BillableCostSoFar.Complete && !updated.BillableCostSoFar.Complete &&
				updated.BillableCostSoFar.Invocations == old.BillableCostSoFar.Invocations) {
			return fmt.Errorf("attention item %s: billable cost observations would regress: %w",
				updated.ID, ErrImmutableTransition)
		}
	}
	cardFacts := []struct {
		name       string
		oldPresent bool
		old        any
		updated    any
	}{
		{"execution_failure", old.ExecutionFailure != nil, old.ExecutionFailure, updated.ExecutionFailure},
		{"diff_stats", old.DiffStats != nil, old.DiffStats, updated.DiffStats},
		{"blocked_on", old.BlockedOn != nil, old.BlockedOn, updated.BlockedOn},
		{"health_diagnostic", old.HealthDiagnostic != nil, old.HealthDiagnostic, updated.HealthDiagnostic},
		{"review_dispute", old.ReviewDispute != nil, old.ReviewDispute, updated.ReviewDispute},
		{"spec_revision", old.SpecRevision != nil, old.SpecRevision, updated.SpecRevision},
	}
	for _, fact := range cardFacts {
		if !fact.oldPresent {
			continue
		}
		same, err := jsonEqual(fact.old, fact.updated)
		if err != nil {
			return fmt.Errorf("attention item %s %s: %w", updated.ID, fact.name, err)
		}
		if !same {
			return fmt.Errorf("attention item %s: %s would change: %w",
				updated.ID, fact.name, ErrImmutableTransition)
		}
	}
	// Publication holds may move between canonical causes while their
	// deterministic publish-blocked item remains open. Keep the fact present,
	// but let the successor state describe the current hold rather than stale
	// operator context from an earlier version.
	if old.PublishBlock != nil && updated.PublishBlock == nil {
		return fmt.Errorf("attention item %s: publish_block would be removed: %w",
			updated.ID, ErrImmutableTransition)
	}
	samePRReference, err := jsonEqual(old.PRReference, updated.PRReference)
	if err != nil {
		return fmt.Errorf("attention item %s: %w", updated.ID, err)
	}
	if !samePRReference {
		return fmt.Errorf("attention item %s: pr reference would change: %w",
			updated.ID, ErrImmutableTransition)
	}
	return nil
}

// compareCanonicalDecimal compares the non-negative decimal representation
// CostSoFar.Validate accepts without converting through a floating-point type.
func compareCanonicalDecimal(left, right string) int {
	leftWhole, leftFraction, _ := strings.Cut(left, ".")
	rightWhole, rightFraction, _ := strings.Cut(right, ".")
	if len(leftWhole) != len(rightWhole) {
		if len(leftWhole) < len(rightWhole) {
			return -1
		}
		return 1
	}
	if compared := strings.Compare(leftWhole, rightWhole); compared != 0 {
		return compared
	}
	width := max(len(leftFraction), len(rightFraction))
	for i := range width {
		leftDigit, rightDigit := byte('0'), byte('0')
		if i < len(leftFraction) {
			leftDigit = leftFraction[i]
		}
		if i < len(rightFraction) {
			rightDigit = rightFraction[i]
		}
		if leftDigit < rightDigit {
			return -1
		}
		if leftDigit > rightDigit {
			return 1
		}
	}
	return 0
}

// ValidateAttentionDeliveryTransition reports whether updated is a legal
// successor to the stored delivery old. The lifecycle only moves forward: a
// stale retry must not roll an opened delivery back to submitted and drop the
// receipts timing aggregates depend on; and an advance preserves the receipts
// already recorded (plan §4).
func ValidateAttentionDeliveryTransition(old, updated AttentionDelivery) error {
	// A delivery's identity is its (item, device, channel, attempt) key; a
	// change to any of them is a different delivery, not a successor.
	if updated.ItemID != old.ItemID || updated.DeviceID != old.DeviceID ||
		updated.Channel != old.Channel || updated.Attempt != old.Attempt {
		return fmt.Errorf("delivery %s/%s/%s/%d: identity would change from %s/%s/%s/%d: %w",
			updated.ItemID, updated.DeviceID, updated.Channel, updated.Attempt,
			old.ItemID, old.DeviceID, old.Channel, old.Attempt, ErrImmutableTransition)
	}
	if deliveryRank(updated.Status) <= deliveryRank(old.Status) {
		return fmt.Errorf("delivery %s/%s/%s/%d: delivery_status %q does not advance stored %q: %w",
			updated.ItemID, updated.DeviceID, updated.Channel, updated.Attempt, updated.Status, old.Status, ErrStaleTransition)
	}
	if !updated.SubmittedAt.Equal(old.SubmittedAt) ||
		(old.ChannelAcceptedAt != nil && !timesEqual(updated.ChannelAcceptedAt, old.ChannelAcceptedAt)) ||
		(old.OpenedAt != nil && !timesEqual(updated.OpenedAt, old.OpenedAt)) {
		return fmt.Errorf("delivery %s/%s/%s/%d: recorded receipts would change: %w",
			updated.ItemID, updated.DeviceID, updated.Channel, updated.Attempt, ErrImmutableTransition)
	}
	return nil
}

// deviceStatusSuccessors returns the statuses a device update may move status
// to. A same-status update is always legal; revoked is terminal (plan §5.14:
// revocation stops future access only, and test 16 relies on the recorded
// state surviving), so regaining access is a new pairing, never a reopened
// device. A switch, not a map, so the exhaustive linter forces a future status
// to declare its successors instead of silently defaulting to terminal.
func deviceStatusSuccessors(status DeviceStatus) []DeviceStatus {
	switch status {
	case DeviceActive:
		return []DeviceStatus{DeviceRevoked}
	case DeviceRevoked:
		return nil
	}
	return nil
}

// ValidateDeviceTransition reports whether updated is a legal successor to the
// stored device old. Identity and paired_at are fixed at pairing; display_name
// may change. Revocation is one-way: revoked admits no successor, and a
// recorded revoked_at never changes, so a stale write can neither reactivate a
// revoked device nor move its recorded revocation instant.
func ValidateDeviceTransition(old, updated Device) error {
	if updated.ID != old.ID {
		return fmt.Errorf("device %s: identity would change from %s: %w", updated.ID, old.ID, ErrImmutableTransition)
	}
	if !updated.PairedAt.Equal(old.PairedAt) {
		return fmt.Errorf("device %s: paired_at would change: %w", updated.ID, ErrImmutableTransition)
	}
	if updated.Status != old.Status && !slices.Contains(deviceStatusSuccessors(old.Status), updated.Status) {
		return fmt.Errorf("device %s: status %q is terminal and cannot become %q: %w",
			updated.ID, old.Status, updated.Status, ErrImmutableTransition)
	}
	if old.RevokedAt != nil && !timesEqual(updated.RevokedAt, old.RevokedAt) {
		return fmt.Errorf("device %s: recorded revoked_at would change: %w", updated.ID, ErrImmutableTransition)
	}
	return nil
}

// ValidatePairingCodeTransition reports whether updated is a legal successor
// to the stored pairing code old. The code's identity and validity window are
// fixed at mint; consumption is recorded once and never changes or clears, so
// a consumed code can never be re-pointed at a second device (§5.14 tests
// 13-14).
func ValidatePairingCodeTransition(old, updated PairingCode) error {
	if updated.CodeHash != old.CodeHash {
		return fmt.Errorf("pairing code %s: identity would change from %s: %w",
			updated.CodeHash, old.CodeHash, ErrImmutableTransition)
	}
	if !updated.CreatedAt.Equal(old.CreatedAt) || !updated.ExpiresAt.Equal(old.ExpiresAt) {
		return fmt.Errorf("pairing code %s: validity window would change: %w", updated.CodeHash, ErrImmutableTransition)
	}
	if old.ConsumedAt != nil {
		sameDevice := old.DeviceID != nil && updated.DeviceID != nil && *updated.DeviceID == *old.DeviceID
		if !timesEqual(updated.ConsumedAt, old.ConsumedAt) || !sameDevice {
			return fmt.Errorf("pairing code %s: recorded consumption would change: %w", updated.CodeHash, ErrImmutableTransition)
		}
	}
	return nil
}

// stagesExtend reports whether updated preserves old's recorded execution
// history: every existing stage keeps its identity and name, every existing
// attempt is unchanged, and growth is append-only.
func stagesExtend(old, updated []Stage) bool {
	if len(updated) < len(old) {
		return false
	}
	for i, os := range old {
		ns := updated[i]
		if ns.ID != os.ID || ns.RunID != os.RunID || ns.Name != os.Name {
			return false
		}
		if len(ns.Attempts) < len(os.Attempts) {
			return false
		}
		for j, oa := range os.Attempts {
			if ns.Attempts[j] != oa {
				return false
			}
		}
	}
	return true
}

// jsonEqual compares two values by their canonical JSON, the byte form the store
// persists.
func jsonEqual(a, b any) (bool, error) {
	ab, err := json.Marshal(a)
	if err != nil {
		return false, err
	}
	bb, err := json.Marshal(b)
	if err != nil {
		return false, err
	}
	return string(ab) == string(bb), nil
}

// deliveryRank orders the delivery lifecycle so a transition can require it to
// strictly advance. An unknown status ranks 0, below every real status.
func deliveryRank(status DeliveryStatus) int {
	switch status {
	case DeliverySubmitted:
		return 1
	case DeliveryChannelAccepted:
		return 2
	case DeliveryOpened:
		return 3
	}
	return 0
}

// timesEqual compares an optional receipt pair, nil meaning not yet recorded.
func timesEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// ValidateDecisionSurfaceTransition reports whether updated is a legal
// successor to the stored record old (plan §4 epoch rule). The item is fixed;
// the epoch stays when the surface is unchanged and advances by exactly one
// when it changes, and nothing else: a regressed epoch or a changed surface
// under the same epoch is a stale write, while a skipped epoch or an advance
// with no surface change would strand every committed source record for
// nothing. updated's digest must equal its recomputation.
func ValidateDecisionSurfaceTransition(old, updated DecisionSurface) error {
	if err := updated.Validate(); err != nil {
		return err
	}
	if updated.ItemID != old.ItemID {
		return fmt.Errorf("decision surface %s: item would change from %s: %w",
			updated.ItemID, old.ItemID, ErrImmutableTransition)
	}
	same, err := jsonEqual(decisionSurfaceFields(old), decisionSurfaceFields(updated))
	if err != nil {
		return fmt.Errorf("decision surface %s: %w", updated.ItemID, err)
	}
	switch {
	case updated.Epoch < old.Epoch:
		return fmt.Errorf("decision surface %s: epoch %d regresses stored %d: %w",
			updated.ItemID, updated.Epoch, old.Epoch, ErrStaleTransition)
	case updated.Epoch == old.Epoch && !same:
		return fmt.Errorf("decision surface %s: surface changed without advancing epoch %d: %w",
			updated.ItemID, old.Epoch, ErrStaleTransition)
	case updated.Epoch == old.Epoch+1 && same:
		return fmt.Errorf("decision surface %s: epoch %d advances with no surface change: %w",
			updated.ItemID, updated.Epoch, ErrDecisionSurfaceEpoch)
	case updated.Epoch > old.Epoch+1:
		return fmt.Errorf("decision surface %s: epoch %d skips stored %d: %w",
			updated.ItemID, updated.Epoch, old.Epoch, ErrDecisionSurfaceEpoch)
	}
	return nil
}

// decisionSurfaceFields projects the fields whose change opens an epoch, so
// the transition compare ignores the epoch and digest that change with them.
// Set fields are normalized to non-nil so a legacy null and an empty set
// compare equal here as they do under Matches.
func decisionSurfaceFields(s DecisionSurface) DecisionSurface {
	s.Epoch = 0
	s.Digest = ""
	s.RequestedDecision = canonicalActions(s.RequestedDecision)
	s.PresentedArtifactDigests = canonicalDigests(s.PresentedArtifactDigests)
	return s
}
