package domain

import (
	"fmt"
	"slices"
	"time"
)

// Subject is what an AttentionItem is about (plan §4). RunID is set only when
// the subject is a run (or a run-scoped proposal); it is nil otherwise.
type Subject struct {
	Type  SubjectType `json:"subject_type"`
	ID    SubjectID   `json:"subject_id"`
	RunID *RunID      `json:"run_id"`
}

// Validate reports whether the subject is well-formed.
func (s Subject) Validate() error {
	if !s.Type.valid() {
		return fmt.Errorf("subject type %q: %w", s.Type, ErrInvalidSubjectType)
	}
	if s.ID == "" {
		return fmt.Errorf("subject id: %w", ErrEmptyID)
	}
	// run_id is meaningful only for a run or a run-scoped proposal batch; a
	// project- or system-scoped subject carrying one is mis-scoped. Behaviour
	// dispatch, so no default: a new subject type must decide its run_id rule.
	switch s.Type {
	case SubjectRun, SubjectProposalBatch:
		// run_id is optional context here, but a present pointer must carry a
		// real id: nil means absent, a pointer to "" is a malformed run scope.
		if s.RunID != nil && *s.RunID == "" {
			return fmt.Errorf("subject run_id: %w", ErrEmptyID)
		}
	case SubjectProject, SubjectSystem:
		if s.RunID != nil {
			return fmt.Errorf("subject type %q with a run_id: %w", s.Type, ErrSubjectRunIDMismatch)
		}
	}
	return nil
}

// AgentClaim is an agent-asserted piece of context attached to an item, always
// labeled as such and kept out of the engine's evidence snapshot (plan §4,
// §5.15 rule 2): agent-produced artifacts appear only here, never as evidence.
type AgentClaim struct {
	Label    string     `json:"label"`
	Artifact ArtifactID `json:"artifact_id"`
	Digest   Digest     `json:"digest"`
}

// Validate reports whether the claim is well-formed: a claim identifies itself
// by label and references the artifact it is asserting about.
func (c AgentClaim) Validate() error {
	if c.Label == "" {
		return fmt.Errorf("agent claim label: %w", ErrEmptyField)
	}
	if c.Artifact == "" {
		return fmt.Errorf("agent claim %q artifact_id: %w", c.Label, ErrEmptyID)
	}
	// The digest is the claim's content address: agent claims carry
	// agent-generated images (plan §5.15), and an unbound claim cannot be
	// rendered or audited against immutable content.
	if c.Digest == "" {
		return fmt.Errorf("agent claim %q digest: %w", c.Label, ErrEmptyField)
	}
	return nil
}

// AttentionItem is a single request for human judgement (plan §4). Its timing
// aggregates are derived from deliveries via WithTiming, never constructed
// directly; its evidence snapshot admits only verifier/daemon artifacts under
// an approved recipe (enforced by NewAttentionItem).
type AttentionItem struct {
	ID                ItemID            `json:"id"`
	ProjectID         ProjectID         `json:"project_id"`
	Subject           Subject           `json:"subject"`
	Type              AttentionType     `json:"type"`
	Priority          Priority          `json:"priority"`
	Reason            string            `json:"reason"`
	RequestedDecision []Action          `json:"requested_decision"`
	EvidenceSnapshot  []Artifact        `json:"evidence_snapshot"`
	AgentClaims       []AgentClaim      `json:"agent_claims"`
	ArtifactDigests   []Digest          `json:"artifact_digests"`
	PRHeadSHA         string            `json:"pr_head_sha"`
	ItemVersion       int               `json:"item_version"`
	InterruptionClass InterruptionClass `json:"interruption_class"`
	ConversationID    *ConversationID   `json:"conversation_id"`
	Timing            TimingSummary     `json:"timing"`
	ExpiresWhen       *time.Time        `json:"expires_when"`
	Status            ItemStatus        `json:"status"`
}

// AttentionItemInput carries the caller-supplied fields of an AttentionItem.
// It deliberately omits Timing: timing is derived from deliveries, so there is
// no input path that sets it (plan §4).
type AttentionItemInput struct {
	ID                ItemID
	ProjectID         ProjectID
	Subject           Subject
	Type              AttentionType
	Priority          Priority
	Reason            string
	RequestedDecision []Action
	EvidenceSnapshot  []Artifact
	AgentClaims       []AgentClaim
	ArtifactDigests   []Digest
	PRHeadSHA         string
	ItemVersion       int
	InterruptionClass InterruptionClass
	ConversationID    *ConversationID
	ExpiresWhen       *time.Time
	Status            ItemStatus
}

// NewAttentionItem builds a validated AttentionItem. Every artifact placed in
// the evidence snapshot is gated by EligibleForEvidenceSnapshot against
// approvedRecipes (plan §5.15 rule 2): a verifier/daemon artifact under an
// approved recipe passes, an agent artifact is rejected and belongs only in
// AgentClaims. Timing is left zero; fill it with WithTiming once deliveries
// exist.
func NewAttentionItem(in AttentionItemInput, approvedRecipes map[Digest]bool) (AttentionItem, error) {
	// Detach the returned item from every caller-owned reference (the subject's
	// run-id pointer, the four slices, the conversation and expiry pointers), so
	// a caller cannot mutate its input to slip an agent artifact or invalid
	// action past the gate after the item has been validated.
	subject := in.Subject
	subject.RunID = clonePtr(in.Subject.RunID)
	item := AttentionItem{
		ID:                in.ID,
		ProjectID:         in.ProjectID,
		Subject:           subject,
		Type:              in.Type,
		Priority:          in.Priority,
		Reason:            in.Reason,
		RequestedDecision: slices.Clone(in.RequestedDecision),
		EvidenceSnapshot:  cloneArtifacts(in.EvidenceSnapshot),
		AgentClaims:       slices.Clone(in.AgentClaims),
		ArtifactDigests:   slices.Clone(in.ArtifactDigests),
		PRHeadSHA:         in.PRHeadSHA,
		ItemVersion:       in.ItemVersion,
		InterruptionClass: in.InterruptionClass,
		ConversationID:    clonePtr(in.ConversationID),
		ExpiresWhen:       clonePtr(in.ExpiresWhen),
		Status:            in.Status,
	}
	if err := item.Validate(); err != nil {
		return AttentionItem{}, err
	}
	for idx := range item.EvidenceSnapshot {
		// Normalize the trusted-policy bit before gating: publish_eligible is
		// computed by policy, never trusted from the supplied artifact, so a
		// caller-set value cannot survive construction. The gate then verifies
		// the (now policy-computed) bit, which is what a store-reconstruction
		// path re-running the gate directly relies on.
		item.EvidenceSnapshot[idx].PublishEligible = computePublishEligible(item.EvidenceSnapshot[idx].Provenance, approvedRecipes)
		if err := EligibleForEvidenceSnapshot(item.EvidenceSnapshot[idx], approvedRecipes); err != nil {
			return AttentionItem{}, err
		}
	}
	return item, nil
}

// Validate reports whether the item is structurally sound. It enforces the
// producer-class half of the evidence rule (no agent artifact in the snapshot)
// without needing the approved-recipe set; NewAttentionItem adds the
// recipe-approval half. A value reconstructed from storage that did not pass
// NewAttentionItem must re-run EligibleForEvidenceSnapshot over its evidence
// against the approved-recipe policy to enforce that half: Validate alone
// cannot, since it holds no policy, and so does not admit an unapproved-recipe
// artifact by omission.
func (i AttentionItem) Validate() error {
	if i.ID == "" {
		return fmt.Errorf("item id: %w", ErrEmptyID)
	}
	if i.ProjectID == "" {
		return fmt.Errorf("item %s project_id: %w", i.ID, ErrEmptyID)
	}
	if !i.Type.valid() {
		return fmt.Errorf("item type %q: %w", i.Type, ErrUnknownAttentionType)
	}
	if err := i.Subject.Validate(); err != nil {
		return err
	}
	if !i.Priority.valid() {
		return fmt.Errorf("item priority %q: %w", i.Priority, ErrInvalidPriority)
	}
	if !i.InterruptionClass.valid() {
		return fmt.Errorf("item interruption_class %q: %w", i.InterruptionClass, ErrInvalidInterruptionClass)
	}
	if !i.Status.valid() {
		return fmt.Errorf("item status %q: %w", i.Status, ErrInvalidItemStatus)
	}
	// Optional pointers mean "absent" when nil; a present pointer must carry a
	// real value, never an empty id or a zero time that serializes as a
	// present-but-unusable field.
	if i.ConversationID != nil && *i.ConversationID == "" {
		return fmt.Errorf("item %s conversation_id: %w", i.ID, ErrEmptyID)
	}
	if i.ExpiresWhen != nil && i.ExpiresWhen.IsZero() {
		return fmt.Errorf("item %s expires_when: %w", i.ID, ErrMissingTimestamp)
	}
	// Timing is trusted card telemetry produced only by WithTiming; a
	// reconstructed item must still carry an internally consistent shape.
	if err := i.Timing.Validate(); err != nil {
		return fmt.Errorf("item %s: %w", i.ID, err)
	}
	if i.ItemVersion < 1 {
		return fmt.Errorf("item %s item_version %d: %w", i.ID, i.ItemVersion, ErrNonPositive)
	}
	for idx, d := range i.ArtifactDigests {
		if d == "" {
			return fmt.Errorf("item %s artifact_digests[%d]: %w", i.ID, idx, ErrEmptyField)
		}
	}
	if len(i.RequestedDecision) == 0 {
		return fmt.Errorf("item %s: %w", i.ID, ErrNoActions)
	}
	for _, a := range i.RequestedDecision {
		if !a.valid() {
			return fmt.Errorf("item action %q: %w", a, ErrInvalidAction)
		}
	}
	evidenceIDs := make(map[ArtifactID]struct{}, len(i.EvidenceSnapshot))
	for _, a := range i.EvidenceSnapshot {
		if err := a.Validate(); err != nil {
			return err
		}
		if a.Provenance.ProducerClass == ProducerAgent {
			return fmt.Errorf("evidence artifact %s: %w", a.ID, ErrAgentArtifactInEvidence)
		}
		// Evidence binds to the candidate head it was produced against: a new
		// remediation head invalidates prior-head evidence, and this package
		// does not yet model head-independent evidence (plan §5.15 rule 2), so
		// when the item names a head every evidence artifact must match it.
		if i.PRHeadSHA != "" && a.Provenance.SourceHeadSHA != i.PRHeadSHA {
			return fmt.Errorf("evidence artifact %s head %q, want %q: %w", a.ID, a.Provenance.SourceHeadSHA, i.PRHeadSHA, ErrEvidenceHeadMismatch)
		}
		if _, dup := evidenceIDs[a.ID]; dup {
			return fmt.Errorf("evidence artifact %s: %w", a.ID, ErrDuplicate)
		}
		evidenceIDs[a.ID] = struct{}{}
	}
	// An artifact id is a content address, so it maps to one digest across the
	// whole item and does not span the two trust channels: a claim may not
	// reuse an evidence id, nor give one id two digests (different labels may
	// still share one id and digest).
	claimDigests := make(map[ArtifactID]Digest, len(i.AgentClaims))
	for _, c := range i.AgentClaims {
		if err := c.Validate(); err != nil {
			return err
		}
		if _, isEvidence := evidenceIDs[c.Artifact]; isEvidence {
			return fmt.Errorf("agent claim %q reuses evidence artifact id %s: %w", c.Label, c.Artifact, ErrArtifactIdentityConflict)
		}
		if d, seen := claimDigests[c.Artifact]; seen && d != c.Digest {
			return fmt.Errorf("agent claim artifact %s maps to two digests: %w", c.Artifact, ErrArtifactIdentityConflict)
		}
		claimDigests[c.Artifact] = c.Digest
	}
	return nil
}

// WithTiming returns a copy of the item with its timing aggregates derived from
// deliveries. It is the only writer of Timing (plan §4). Every delivery must be
// valid and belong to this item: timing becomes trusted card data, so a foreign
// or malformed delivery must not be counted as the item's own receipt history.
func (i AttentionItem) WithTiming(deliveries []AttentionDelivery) (AttentionItem, error) {
	seen := make(map[deliveryKey]struct{}, len(deliveries))
	for idx, d := range deliveries {
		if err := d.Validate(); err != nil {
			return AttentionItem{}, fmt.Errorf("timing delivery[%d]: %w", idx, err)
		}
		if d.ItemID != i.ID {
			return AttentionItem{}, fmt.Errorf("timing delivery[%d] item_id %q, want %q: %w", idx, d.ItemID, i.ID, ErrForeignDelivery)
		}
		// A duplicated attempt (same device/channel/attempt) would inflate the
		// aggregates, so reject it rather than count a store/outbox retry twice.
		k := deliveryKey{device: d.DeviceID, channel: d.Channel, attempt: d.Attempt}
		if _, dup := seen[k]; dup {
			return AttentionItem{}, fmt.Errorf("timing delivery[%d] duplicate attempt %s/%s/%d: %w", idx, d.DeviceID, d.Channel, d.Attempt, ErrDuplicate)
		}
		seen[k] = struct{}{}
	}
	i.Timing = TimingAggregates(deliveries)
	return i, nil
}
