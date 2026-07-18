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
// The provenance is typed and agent-pinned: the evidence channel (#173) routes
// agent workspace artifacts only into these claims, and a claim asserting any
// other producer class is invalid by construction, so a decoded claim cannot
// launder agent output into a trusted producer class.
type AgentClaim struct {
	Label      string     `json:"label"`
	Artifact   ArtifactID `json:"artifact_id"`
	Digest     Digest     `json:"digest"`
	Provenance Provenance `json:"provenance"`
}

// clone returns a copy detached from the caller's provenance pointer.
func (c AgentClaim) clone() AgentClaim {
	c.Provenance = c.Provenance.clone()
	return c
}

// cloneAgentClaims deep-copies a claim slice for the same reason.
func cloneAgentClaims(in []AgentClaim) []AgentClaim {
	if in == nil {
		return nil
	}
	out := make([]AgentClaim, len(in))
	for i, c := range in {
		out[i] = c.clone()
	}
	return out
}

// Validate reports whether the claim is well-formed: a claim identifies itself
// by label, references the artifact it is asserting about, and carries valid
// agent provenance.
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
	if err := c.Provenance.Validate(); err != nil {
		return fmt.Errorf("agent claim %q: %w", c.Label, err)
	}
	// Provenance.Validate admits any producer class; a claim additionally pins
	// the agent class, the only producer whose artifacts route through claims.
	if c.Provenance.ProducerClass != ProducerAgent {
		return fmt.Errorf("agent claim %q producer_class %q: %w", c.Label, c.Provenance.ProducerClass, ErrNonAgentClaim)
	}
	return nil
}

// AttentionItem is a single request for human judgement (plan §4). Its timing
// aggregates are derived from deliveries via WithTiming, never constructed
// directly; its evidence snapshot admits only verifier/daemon artifacts under
// an approved recipe (enforced by NewAttentionItem).
type AttentionItem struct {
	ID                ItemID        `json:"id"`
	ProjectID         ProjectID     `json:"project_id"`
	Subject           Subject       `json:"subject"`
	Type              AttentionType `json:"type"`
	Priority          Priority      `json:"priority"`
	Reason            string        `json:"reason"`
	RequestedDecision []Action      `json:"requested_decision"`
	EvidenceSnapshot  []Artifact    `json:"evidence_snapshot"`
	AgentClaims       []AgentClaim  `json:"agent_claims"`
	// ArtifactDigests is the item's approval binding set: the canonical (sorted,
	// deduplicated) union of every digest rendered in EvidenceSnapshot and
	// AgentClaims. It is derived by NewAttentionItem and enforced by Validate,
	// never caller-supplied, so an item cannot display one digest while binding
	// another (the stale-approval class, plan §3.1; §4 "approvals bind to
	// digests"). A prepared command pins this set and is invalidated if it
	// changes.
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
// It deliberately omits Timing (derived from deliveries) and ArtifactDigests
// (the binding set, derived from the rendered evidence and claims): there is no
// input path that sets either, so a caller cannot bind a digest it did not
// render (plan §4).
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
		AgentClaims:       cloneAgentClaims(in.AgentClaims),
		PRHeadSHA:         in.PRHeadSHA,
		ItemVersion:       in.ItemVersion,
		InterruptionClass: in.InterruptionClass,
		ConversationID:    clonePtr(in.ConversationID),
		ExpiresWhen:       clonePtr(in.ExpiresWhen),
		Status:            in.Status,
	}
	// Derive the binding set from the rendered evidence and claims, so the
	// approval binds exactly what was shown (plan §3.1, §4). Validate re-derives
	// and requires equality, which is what enforces this on the store-decode path
	// that bypasses this constructor.
	item.ArtifactDigests = bindingDigests(item.EvidenceSnapshot, item.AgentClaims)
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
	// An empty requested_decision is structurally valid: the read-only blocked
	// type offers no action (plan §4), and which types must offer at least one
	// is per-type signet policy, not domain vocabulary.
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
		// Head-bound evidence binds to the candidate head it was produced
		// against: a new remediation head invalidates prior-head evidence, so
		// when the item names a head every head-bound evidence artifact must
		// match it. Head-independent evidence is intentionally decoupled from
		// head (plan §5.15 rule 2) and is preserved across a remediation head;
		// its provenance carries no source head (enforced by Provenance.Validate
		// above), so it is exempt here rather than compared.
		if a.Provenance.HeadBinding == HeadBound && i.PRHeadSHA != "" && a.Provenance.SourceHeadSHA != i.PRHeadSHA {
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
	// The binding set must equal exactly the digests rendered in the evidence
	// snapshot and the agent claims: an item may not display one digest while
	// binding another (the stale-approval class, plan §3.1; §4 "approvals bind to
	// digests"). bindingDigests is canonical, so this equality also fixes the
	// field's order and rejects a duplicate, an omission, or an extra unrendered
	// entry. NewAttentionItem derives the set; a store-decoded item is held to
	// the same equality here, since Validate is the reconstruction backstop.
	if want := bindingDigests(i.EvidenceSnapshot, i.AgentClaims); !slices.Equal(i.ArtifactDigests, want) {
		return fmt.Errorf("item %s artifact_digests %v, rendered digests resolve to %v: %w", i.ID, i.ArtifactDigests, want, ErrBindingMismatch)
	}
	return nil
}

// bindingDigests returns an item's canonical binding set: the sorted,
// deduplicated union of the digests rendered in its evidence snapshot and its
// agent claims. It is the single definition of what an approval binds, so
// NewAttentionItem derives ArtifactDigests from it and Validate requires
// equality. It always returns a non-nil slice, so an item that renders no
// artifacts (e.g. a system_health acknowledgement) serializes artifact_digests
// as "[]", matching the required, non-null array the wire contract declares
// (api/openapi.yaml). slices.Equal treats nil and empty as equal, so a value
// decoded from a legacy null still satisfies the equality check.
func bindingDigests(evidence []Artifact, claims []AgentClaim) []Digest {
	out := make([]Digest, 0, len(evidence)+len(claims))
	for _, a := range evidence {
		out = append(out, a.Digest)
	}
	for _, c := range claims {
		out = append(out, c.Digest)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Offers reports whether the item currently offers action as one of its
// requested decisions (plan §4 Actions). A recorded command may only accept an
// action the item actually rendered as a choice: the offered set is
// item-specific ("approve" is not universal), so a valid enum value that the
// item did not offer is not a legitimate decision on it.
func (i AttentionItem) Offers(a Action) bool {
	return slices.Contains(i.RequestedDecision, a)
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
