package domain

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// ProductionReadyItemID and ProductionBlockedItemID are the workflow-owned
// attention-item identities that authenticate terminal publication
// observations at read boundaries.
func ProductionReadyItemID(runID RunID) ItemID {
	return ItemID("production-ready-" + string(runID))
}

func ProductionBlockedItemID(runID RunID) ItemID {
	return ItemID("production-publish-blocked-" + string(runID))
}

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
	Text       *ClaimText `json:"text"`
}

// MaxClaimTextBytes caps a text claim's inline content. The content rides
// inside the item blob (persisted per item, re-sent on every list and
// bootstrap read), unlike attachments fetched out of line, so the cap keeps
// item payloads bounded while staying far beyond a seconds-readable summary
// (plan §9).
const MaxClaimTextBytes = 1 << 16

// ClaimText is the inline renderable body of a text claim (plan §9's summary
// carrier): media-typed prose carried on the claim itself so the card's
// summary layer renders without a fetch. The enclosing claim's digest binds
// the content (Validate recomputes it), so inline text rides the same digest
// discipline as every referenced artifact.
type ClaimText struct {
	MediaType ClaimMediaType `json:"media_type"`
	Content   string         `json:"content"`
}

// ComputeDigest returns the content address of the text's UTF-8 bytes, in
// the repository-wide "sha256:<hex>" form, so a text claim's digest is also
// a valid attachment-store address for the same bytes.
func (t ClaimText) ComputeDigest() Digest {
	return Digest(contentaddr.Sum([]byte(t.Content)))
}

// clone returns a copy detached from the caller's provenance and text
// pointers.
func (c AgentClaim) clone() AgentClaim {
	c.Provenance = c.Provenance.clone()
	c.Text = clonePtr(c.Text)
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
	if c.Text != nil {
		// Inline content is barred from the high-sensitivity tier: clients
		// persist item metadata in disk caches under §5.14's
		// no-high-sensitivity-at-rest default, so prose that must stay
		// memory-only travels the referenced attachment path instead of
		// riding inside the item.
		if c.Provenance.SensitivityClass == SensitivityHigh {
			return fmt.Errorf("agent claim %q sensitivity_class %q: %w", c.Label, c.Provenance.SensitivityClass, ErrHighSensitivityClaimText)
		}
		// Cheap shape checks precede the hash so a malformed claim never
		// buys a digest computation; the mismatch check is the binding rule
		// itself: a claim cannot display one text while binding another
		// digest (the stale-approval class, plan §3.1).
		if !c.Text.MediaType.valid() {
			return fmt.Errorf("agent claim %q text media_type %q: %w", c.Label, c.Text.MediaType, ErrInvalidClaimMediaType)
		}
		if c.Text.Content == "" {
			return fmt.Errorf("agent claim %q text content: %w", c.Label, ErrEmptyField)
		}
		// A JSON decode admits invalid UTF-8 (escaped raw bytes survive
		// unmarshalling, #180), so validity is checked, never assumed.
		if !utf8.ValidString(c.Text.Content) {
			return fmt.Errorf("agent claim %q: %w", c.Label, ErrClaimTextNotUTF8)
		}
		if len(c.Text.Content) > MaxClaimTextBytes {
			return fmt.Errorf("agent claim %q text content %d bytes: %w", c.Label, len(c.Text.Content), ErrClaimTextTooLarge)
		}
		if computed := c.Text.ComputeDigest(); c.Digest != computed {
			return fmt.Errorf("agent claim %q digest %q, text content resolves to %q: %w", c.Label, c.Digest, computed, ErrClaimTextDigestMismatch)
		}
	}
	return nil
}

// SupersessionKind names the class of validated configuration that can
// supersede a system_health item's blocking effect (plan §4: "a validated
// configuration supersedes it"). The zero value is invalid.
type SupersessionKind string

const (
	// SupersessionBackupEncryptionWaiver: the §5.7 Phase 1A.2
	// backup-encryption waiver supersedes the degraded-posture notice raised
	// by an admission that ran under it.
	SupersessionBackupEncryptionWaiver SupersessionKind = "backup_encryption_waiver"
)

// AllSupersessionKinds is the single registration point for supersession
// kinds.
var AllSupersessionKinds = []SupersessionKind{SupersessionBackupEncryptionWaiver}

func (k SupersessionKind) valid() bool {
	switch k {
	case SupersessionBackupEncryptionWaiver:
		return true
	default:
		return false
	}
}

// BlockingSupersession is the typed condition under which an open
// system_health item does not block unattended admission (plan §4, §5.7). It
// names the validated configuration that supersedes the item's blocking
// effect; it is never a stored verdict. Whether the condition currently holds
// is re-evaluated against live policy at every admission (Supersedes), so
// clearing or retargeting the configuration makes the still-open item
// blocking again without any write.
type BlockingSupersession struct {
	Kind SupersessionKind `json:"kind"`
	// RepositoryID is the exact trusted numeric repository ID the superseding
	// waiver must cover (backup_encryption_waiver). Written by the daemon
	// from the validated admission waiver, never client-supplied.
	RepositoryID int64 `json:"repository_id"`
}

// Validate reports whether the condition is structurally sound. Payload rules
// dispatch on kind, so a future kind must declare what its payload means; the
// trailing return rejects the invalid zero kind.
func (s BlockingSupersession) Validate() error {
	switch s.Kind {
	case SupersessionBackupEncryptionWaiver:
		if s.RepositoryID <= 0 {
			return fmt.Errorf("blocking supersession repository_id %d: %w", s.RepositoryID, ErrNonPositive)
		}
		return nil
	}
	return fmt.Errorf("blocking supersession kind %q: %w", s.Kind, ErrInvalidSupersessionKind)
}

// Supersedes reports whether the condition currently holds under policy: the
// item's blocking effect is superseded by healthy encrypted backup evidence,
// or, during legacy reconstruction only, while the old waiver configuration
// covers it. The stored condition names the obsolete risk, not a verdict; this
// re-derivation against live policy is what makes it hold, so a decoded item
// can never assert its own non-blocking state. It fails closed: an invalid
// condition supersedes nothing.
func (s BlockingSupersession) Supersedes(policy AdmissionPolicy) error {
	if err := s.Validate(); err != nil {
		return err
	}
	switch s.Kind {
	case SupersessionBackupEncryptionWaiver:
		if policy.BackupHealth != nil {
			if err := policy.BackupHealth.RequireHealthy(); err != nil {
				return fmt.Errorf("blocking supersession names repository %d under unhealthy encrypted backup: %w",
					s.RepositoryID, err)
			}
			return nil
		}
		if !waiverConfiguredFor(s.RepositoryID, policy) {
			return fmt.Errorf("blocking supersession names repository %d: %w",
				s.RepositoryID, ErrWaiverNotConfigured)
		}
		return nil
	}
	return fmt.Errorf("blocking supersession kind %q: %w", s.Kind, ErrInvalidSupersessionKind)
}

// BaseFreshness is the base-advance staleness watch's fact (plan §5.16,
// issue #442): the daemon's last observation of a ready_for_final_review
// item's target base against the base the publication was admitted for. It
// is maintained by the schedule's consuming handler, never client-supplied
// (AttentionItemInput carries no path to it), and updates only on material
// change, so a routine watch fire does not churn item versions — while a
// base advance does, correctly invalidating commands prepared against the
// stale base claim.
type BaseFreshness struct {
	BaseRef         string `json:"base_ref"`
	AdmittedBaseSHA string `json:"admitted_base_sha"`
	ObservedBaseSHA string `json:"observed_base_sha"`
	// Advanced is derived: the observed tip differs from the admitted base.
	// Stored and revalidated so a decoded fact cannot claim freshness its
	// own coordinates contradict.
	Advanced bool `json:"advanced"`
	// ObservedAt is the UTC instant of the observation.
	ObservedAt time.Time `json:"observed_at"`
}

// Validate reports whether the fact is structurally sound and internally
// consistent.
func (b BaseFreshness) Validate() error {
	for name, v := range map[string]string{
		"base_ref": b.BaseRef, "admitted_base_sha": b.AdmittedBaseSHA,
		"observed_base_sha": b.ObservedBaseSHA,
	} {
		if v == "" {
			return fmt.Errorf("base freshness %s: %w", name, ErrEmptyField)
		}
	}
	if b.Advanced != (b.ObservedBaseSHA != b.AdmittedBaseSHA) {
		return fmt.Errorf("base freshness advanced=%v with observed %q against admitted %q: %w",
			b.Advanced, b.ObservedBaseSHA, b.AdmittedBaseSHA, ErrBaseFreshnessInconsistent)
	}
	if b.ObservedAt.IsZero() {
		return fmt.Errorf("base freshness observed_at: %w", ErrMissingTimestamp)
	}
	if b.ObservedAt.Location() != time.UTC {
		return fmt.Errorf("base freshness observed_at: %w", ErrTimestampNotUTC)
	}
	return nil
}

// ReadinessInvalidationReason names the axis on which a ready_for_final_review
// item's exact binding diverged from the daemon's live observation, so a clean
// review pass no longer describes the current candidate (plan §7, issue #496).
// The zero value is invalid.
type ReadinessInvalidationReason string

const (
	// ReadinessInvalidationHeadChanged: the observed PR head SHA differs from
	// the head the ready item was earned against (a force-push or new commit).
	ReadinessInvalidationHeadChanged ReadinessInvalidationReason = "head_changed"
	// ReadinessInvalidationBaseAdvanced: the observed target-base tip advanced
	// past the base the publication was admitted for.
	ReadinessInvalidationBaseAdvanced ReadinessInvalidationReason = "base_advanced"
	// ReadinessInvalidationRetargeted: the PR's base ref changed (the pull was
	// retargeted at a different branch).
	ReadinessInvalidationRetargeted ReadinessInvalidationReason = "retargeted"
	// ReadinessInvalidationIdentityChanged: the observed repository ID or PR
	// number differs from the ready item's immutable binding.
	ReadinessInvalidationIdentityChanged ReadinessInvalidationReason = "identity_changed"
)

// AllReadinessInvalidationReasons is the single registration point for
// readiness invalidation reasons. An observed repository/PR identity mismatch
// withdraws the ready item's standing claim without treating the foreign pull
// as authority for the bound PR (issue #514;
// devlog/2026-08-05-0113-ready-identity-fail-closed.md).
var AllReadinessInvalidationReasons = []ReadinessInvalidationReason{
	ReadinessInvalidationHeadChanged,
	ReadinessInvalidationBaseAdvanced,
	ReadinessInvalidationRetargeted,
	ReadinessInvalidationIdentityChanged,
}

func (r ReadinessInvalidationReason) valid() bool {
	switch r {
	case ReadinessInvalidationHeadChanged, ReadinessInvalidationBaseAdvanced,
		ReadinessInvalidationRetargeted, ReadinessInvalidationIdentityChanged:
		return true
	default:
		return false
	}
}

// ReadinessInvalidation is the daemon-recorded fact that a
// ready_for_final_review item's clean-pass claim was invalidated by a change
// the review pass was not run against (plan §7, issue #496). It is written in
// the same transaction that supersedes the item, so the staleness is
// item-visible to signet clients and the version bump stales any command
// prepared against the old ready claim. It is never client-supplied
// (AttentionItemInput carries no path to it), and it is written once: the
// superseding transition is terminal, so the fact never changes afterward.
type ReadinessInvalidation struct {
	Reason ReadinessInvalidationReason `json:"reason"`
	// Bound is the ready item's binding coordinate on the reason's axis (the
	// value the review pass was earned against): the head SHA for head_changed,
	// the admitted base SHA for base_advanced, the base ref for retargeted, the
	// "repository_id#pr_number" identity for identity_changed.
	Bound string `json:"bound"`
	// Observed is the daemon's live observation on that same axis. It differs
	// from Bound by construction; that divergence is what invalidates the pass,
	// so it is revalidated (a decoded fact cannot claim an invalidation its own
	// coordinates contradict), mirroring BaseFreshness.Advanced.
	Observed string `json:"observed"`
	// ObservedAt is the UTC instant of the observation.
	ObservedAt time.Time `json:"observed_at"`
}

// PRReference is the structured identity of the published pull request a
// ready_for_final_review item links to. The repository is GitHub's canonical
// owner/name coordinate; clients may render it or compose its browser URL
// without parsing presentation prose.
type PRReference struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
}

// Validate reports whether the reference is a safe GitHub owner/name
// coordinate with a positive pull-request number.
func (r PRReference) Validate() error {
	parts := strings.Split(r.Repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" ||
		parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." {
		return fmt.Errorf("pull request repo %q: %w", r.Repo, ErrPRReferenceInconsistent)
	}
	if r.Number <= 0 {
		return fmt.Errorf("pull request number %d: %w", r.Number, ErrNonPositive)
	}
	return nil
}

// Validate reports whether the fact is structurally sound and internally
// consistent.
func (r ReadinessInvalidation) Validate() error {
	if !r.Reason.valid() {
		return fmt.Errorf("readiness invalidation reason %q: %w", r.Reason, ErrInvalidReadinessInvalidationReason)
	}
	for name, v := range map[string]string{"bound": r.Bound, "observed": r.Observed} {
		if v == "" {
			return fmt.Errorf("readiness invalidation %s: %w", name, ErrEmptyField)
		}
	}
	if r.Bound == r.Observed {
		return fmt.Errorf("readiness invalidation bound and observed both %q: %w",
			r.Bound, ErrReadinessInvalidationNotDivergent)
	}
	if r.ObservedAt.IsZero() {
		return fmt.Errorf("readiness invalidation observed_at: %w", ErrMissingTimestamp)
	}
	if r.ObservedAt.Location() != time.UTC {
		return fmt.Errorf("readiness invalidation observed_at: %w", ErrTimestampNotUTC)
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
	ArtifactDigests []Digest `json:"artifact_digests"`
	PRHeadSHA       string   `json:"pr_head_sha"`
	// PRReference identifies the published pull request behind a
	// ready_for_final_review item. It is required exactly on that type and
	// renders explicit null on every other item.
	PRReference *PRReference `json:"pr_reference"`
	// Readiness preserves the daemon-evaluated ready class and exact
	// evaluation-set digest on ready_for_final_review items. Production creators
	// always set it; nil remains valid for legacy persisted items and fake-mode
	// items that never ran Section 6 verification. Once created it is immutable.
	Readiness *ReadinessSummary `json:"readiness"`
	// CommitPlanNotice is the daemon-derived commit-plan notice (plan §5.6;
	// CommitPlanNoticeReason): set when the reserved plan channel was
	// consumed without a plan structuring the import, nil otherwise. The
	// reason is classified by the daemon and never supplied by the
	// workspace; emission is #212's, so nothing sets it until that unit
	// lands.
	CommitPlanNotice *CommitPlanNoticeReason `json:"commit_plan_notice"`
	// BaseFreshness is the base-advance staleness watch's maintained fact,
	// present only on ready_for_final_review items once the watch has
	// observed the base (plan §5.16); nil renders explicit null. Daemon-set
	// by the consuming handler; there is no client input path.
	BaseFreshness *BaseFreshness `json:"base_freshness"`
	// ReadinessInvalidation is the daemon-recorded fact that a
	// ready_for_final_review item's clean-pass claim was invalidated by a
	// base/head/identity change (plan §7, issue #496), present only on a
	// superseded ready item; nil renders explicit null. Daemon-set in the
	// superseding transaction, there is no client input path.
	ReadinessInvalidation *ReadinessInvalidation `json:"readiness_invalidation"`
	// ReviewRecoveryBinding identifies the exact persisted contradiction the
	// item offers to recover. It is present only on review_contradiction items
	// and is immutable across the item's lifecycle.
	ReviewRecoveryBinding *ReviewRecoveryBinding `json:"review_recovery_binding"`
	// CodexReenrollmentRecoveryBinding identifies the verified auth-store
	// replacement that may resolve a revoked-identity system-health item. It is
	// absent until verification and immutable once projected onto the item.
	CodexReenrollmentRecoveryBinding *CodexReenrollmentRecoveryBinding `json:"codex_reenrollment_recovery_binding"`
	// ReviewConfigurationRecovery identifies the exact parked
	// configuration-class failure the item offers to recover and the
	// admission-pinned profile it was parked under. It is present only on
	// review_configuration items and is immutable across the item's
	// lifecycle; the adoption target is resolved at decision time, not here.
	ReviewConfigurationRecovery *ReviewConfigurationRecoveryBinding `json:"review_configuration_recovery"`
	// FindingAdjudication carries the proposal projection bound to the exact
	// immutable artifact digest. It is present only on finding_adjudication.
	FindingAdjudication *FindingAdjudicationBinding `json:"finding_adjudication"`
	ItemVersion         int                         `json:"item_version"`
	InterruptionClass   InterruptionClass           `json:"interruption_class"`
	ConversationID      *ConversationID             `json:"conversation_id"`
	Timing              TimingSummary               `json:"timing"`
	// CreatedAt is the daemon-stamped instant this item was created. It is
	// immutable across the item's lifecycle and nil only for legacy items
	// persisted before the field existed.
	CreatedAt   *time.Time `json:"created_at"`
	ExpiresWhen *time.Time `json:"expires_when"`
	// DecidedAt is the daemon-stamped instant the item's first concluding
	// decision was accepted (plan §4: open-to-decision time is the headline
	// attention-latency metric, with the §1 per-unit measure governing;
	// issue #171). It is set only by WithDecidedAt in the transaction that
	// records the concluding command, never caller-supplied, and nil for items
	// that were not concluded by a decision (open, expired, superseded). Once
	// recorded it is immutable (ValidateAttentionItemTransition): an
	// idempotent command replay or a later re-put must not move or erase it.
	DecidedAt *time.Time `json:"decided_at"`
	// Posture is required exactly on system_health items. Blocking preserves
	// the historical admission behavior; advisory keeps the observation open
	// and visible without blocking unrelated unattended admission. It is fixed
	// at creation (ValidateAttentionItemTransition) and nil for every other
	// item type.
	Posture *HealthPosture `json:"posture"`
	// BlockingSupersession is the typed condition under which this open
	// system_health item does not block unattended admission (plan §4, §5.7),
	// nil for every other item and for a health item that blocks
	// unconditionally. Set at creation by the daemon from the validated
	// configuration the item reports on; fixed once set
	// (ValidateAttentionItemTransition), and never read as a verdict — the
	// admission gate re-validates it against live policy (Supersedes).
	BlockingSupersession *BlockingSupersession `json:"blocking_supersession"`
	Status               ItemStatus            `json:"status"`
}

// AttentionItemInput carries the caller-supplied fields of an AttentionItem.
// It deliberately omits Timing (derived from deliveries) and ArtifactDigests
// (the binding set, derived from the rendered evidence and claims): there is no
// input path that sets either, so a caller cannot bind a digest it did not
// render (plan §4).
type AttentionItemInput struct {
	ID                               ItemID
	ProjectID                        ProjectID
	Subject                          Subject
	Type                             AttentionType
	Priority                         Priority
	Reason                           string
	RequestedDecision                []Action
	EvidenceSnapshot                 []Artifact
	AgentClaims                      []AgentClaim
	PRHeadSHA                        string
	PRReference                      *PRReference
	Readiness                        *ReadinessSummary
	CommitPlanNotice                 *CommitPlanNoticeReason
	ReviewRecoveryBinding            *ReviewRecoveryBinding
	CodexReenrollmentRecoveryBinding *CodexReenrollmentRecoveryBinding
	ReviewConfigurationRecovery      *ReviewConfigurationRecoveryBinding
	FindingAdjudication              *FindingAdjudicationBinding
	ItemVersion                      int
	InterruptionClass                InterruptionClass
	ConversationID                   *ConversationID
	CreatedAt                        *time.Time
	ExpiresWhen                      *time.Time
	// Posture must be set exactly by daemon-internal creators of system_health
	// items; there is no client input path to it.
	Posture *HealthPosture
	// BlockingSupersession may be set only by daemon-internal creators of
	// system_health items; there is no client input path to it.
	BlockingSupersession *BlockingSupersession
	Status               ItemStatus
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
		ID:                               in.ID,
		ProjectID:                        in.ProjectID,
		Subject:                          subject,
		Type:                             in.Type,
		Priority:                         in.Priority,
		Reason:                           in.Reason,
		RequestedDecision:                slices.Clone(in.RequestedDecision),
		EvidenceSnapshot:                 cloneArtifacts(in.EvidenceSnapshot),
		AgentClaims:                      cloneAgentClaims(in.AgentClaims),
		PRHeadSHA:                        in.PRHeadSHA,
		PRReference:                      clonePtr(in.PRReference),
		Readiness:                        clonePtr(in.Readiness),
		CommitPlanNotice:                 clonePtr(in.CommitPlanNotice),
		ReviewRecoveryBinding:            clonePtr(in.ReviewRecoveryBinding),
		CodexReenrollmentRecoveryBinding: clonePtr(in.CodexReenrollmentRecoveryBinding),
		ReviewConfigurationRecovery:      clonePtr(in.ReviewConfigurationRecovery),
		FindingAdjudication:              cloneFindingAdjudicationBinding(in.FindingAdjudication),
		ItemVersion:                      in.ItemVersion,
		InterruptionClass:                in.InterruptionClass,
		ConversationID:                   clonePtr(in.ConversationID),
		CreatedAt:                        clonePtr(in.CreatedAt),
		ExpiresWhen:                      clonePtr(in.ExpiresWhen),
		Posture:                          clonePtr(in.Posture),
		BlockingSupersession:             clonePtr(in.BlockingSupersession),
		Status:                           in.Status,
	}
	// Normalize the optional expiry to UTC so the producer path stores one
	// canonical spelling (a dev-CLI producer stamps it from the host clock);
	// Validate below then rejects any non-UTC that bypassed this constructor,
	// mirroring DecidedAt (issue #553).
	if item.ExpiresWhen != nil {
		utc := item.ExpiresWhen.UTC()
		item.ExpiresWhen = &utc
	}
	// Derive the binding set from the rendered evidence and claims, so the
	// approval binds exactly what was shown (plan §3.1, §4). Validate re-derives
	// and requires equality, which is what enforces this on the store-decode path
	// that bypasses this constructor.
	item.ArtifactDigests = bindingDigests(item.EvidenceSnapshot, item.AgentClaims, item.FindingAdjudication)
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
	if i.CreatedAt != nil {
		if i.CreatedAt.IsZero() {
			return fmt.Errorf("item %s created_at: %w", i.ID, ErrMissingTimestamp)
		}
		if i.CreatedAt.Location() != time.UTC {
			return fmt.Errorf("item %s created_at: %w", i.ID, ErrTimestampNotUTC)
		}
	}
	if i.ExpiresWhen != nil {
		if i.ExpiresWhen.IsZero() {
			return fmt.Errorf("item %s expires_when: %w", i.ID, ErrMissingTimestamp)
		}
		// A canonical persisted encoding whose re-put convergence is a byte
		// compare: a non-UTC instant is the same moment in a different byte form,
		// so it would give one stamp two encodings (mirrors DecidedAt below).
		if i.ExpiresWhen.Location() != time.UTC {
			return fmt.Errorf("item %s expires_when: %w", i.ID, ErrTimestampNotUTC)
		}
	}
	if i.CommitPlanNotice != nil && !i.CommitPlanNotice.valid() {
		return fmt.Errorf("item %s commit_plan_notice %q: %w", i.ID, *i.CommitPlanNotice, ErrInvalidCommitPlanNotice)
	}
	if i.PRReference == nil {
		if i.Type == AttentionReadyForFinalReview {
			return fmt.Errorf("item %s type %q has no pr reference: %w",
				i.ID, i.Type, ErrPRReferenceInconsistent)
		}
	} else {
		if i.Type != AttentionReadyForFinalReview {
			return fmt.Errorf("item %s type %q carries a pr reference: %w",
				i.ID, i.Type, ErrPRReferenceInconsistent)
		}
		if err := i.PRReference.Validate(); err != nil {
			return fmt.Errorf("item %s: %w", i.ID, err)
		}
	}
	if i.Readiness != nil {
		if i.Type != AttentionReadyForFinalReview {
			return fmt.Errorf("item %s type %q carries readiness: %w",
				i.ID, i.Type, ErrReadinessSummaryInconsistent)
		}
		if err := i.Readiness.Validate(); err != nil {
			return fmt.Errorf("item %s: %w", i.ID, err)
		}
	}
	if i.ReviewRecoveryBinding == nil {
		if i.Type == AttentionReviewContradiction {
			return fmt.Errorf("item %s: %w", i.ID, ErrReviewRecoveryBindingMissing)
		}
	} else {
		if i.Type != AttentionReviewContradiction {
			return fmt.Errorf("item %s type %q carries a review recovery binding: %w",
				i.ID, i.Type, ErrReviewRecoveryBindingOutsideItem)
		}
		if err := i.ReviewRecoveryBinding.Validate(); err != nil {
			return fmt.Errorf("item %s: %w", i.ID, err)
		}
		if i.Subject.Type != SubjectRun || i.Subject.RunID == nil ||
			*i.Subject.RunID != i.ReviewRecoveryBinding.RunID ||
			i.Subject.ID != SubjectID(i.ReviewRecoveryBinding.RunID) ||
			i.PRHeadSHA != i.ReviewRecoveryBinding.HeadSHA {
			return fmt.Errorf("item %s recovery binding disagrees with its subject or head: %w",
				i.ID, ErrReviewRecoveryBindingMismatch)
		}
	}
	if i.ReviewConfigurationRecovery == nil {
		if i.Type == AttentionReviewConfiguration {
			return fmt.Errorf("item %s: %w", i.ID, ErrReviewConfigRecoveryBindingMissing)
		}
	} else {
		if i.Type != AttentionReviewConfiguration {
			return fmt.Errorf("item %s type %q carries a review configuration recovery binding: %w",
				i.ID, i.Type, ErrReviewConfigRecoveryBindingOutsideItem)
		}
		if err := i.ReviewConfigurationRecovery.Validate(); err != nil {
			return fmt.Errorf("item %s: %w", i.ID, err)
		}
		if i.Subject.Type != SubjectRun || i.Subject.RunID == nil ||
			*i.Subject.RunID != i.ReviewConfigurationRecovery.RunID ||
			i.Subject.ID != SubjectID(i.ReviewConfigurationRecovery.RunID) ||
			i.PRHeadSHA != i.ReviewConfigurationRecovery.HeadSHA {
			return fmt.Errorf("item %s configuration recovery binding disagrees with its subject or head: %w",
				i.ID, ErrReviewConfigRecoveryBindingMismatch)
		}
	}
	if i.FindingAdjudication == nil {
		if i.Type == AttentionFindingAdjudication {
			return fmt.Errorf("item %s: %w", i.ID, ErrFindingAdjudicationBindingMissing)
		}
	} else {
		if i.Type != AttentionFindingAdjudication {
			return fmt.Errorf("item %s type %q carries a finding adjudication binding: %w",
				i.ID, i.Type, ErrFindingAdjudicationBindingOutsideItem)
		}
		if err := i.FindingAdjudication.Validate(); err != nil {
			return fmt.Errorf("item %s: %w", i.ID, err)
		}
		if i.Subject.Type != SubjectRun || i.Subject.RunID == nil ||
			*i.Subject.RunID != i.FindingAdjudication.RunID ||
			i.Subject.ID != SubjectID(i.FindingAdjudication.RunID) {
			return fmt.Errorf("item %s finding adjudication binding disagrees with its subject: %w",
				i.ID, ErrFindingAdjudicationBindingMismatch)
		}
		if i.Offers(ActionChooseAlternativeRoute) &&
			!i.FindingAdjudication.hasOfferedAlternative() {
			return fmt.Errorf("item %s offers choose_alternative_route without an offered alternative: %w",
				i.ID, ErrFindingAdjudicationBindingMismatch)
		}
	}
	if i.CodexReenrollmentRecoveryBinding != nil {
		if i.Type != AttentionSystemHealth {
			return fmt.Errorf("item %s type %q carries a codex re-enrollment recovery binding: %w",
				i.ID, i.Type, ErrCodexReenrollmentBindingOutsideItem)
		}
		if err := i.CodexReenrollmentRecoveryBinding.Validate(); err != nil {
			return fmt.Errorf("item %s: %w", i.ID, err)
		}
		if !i.Offers(ActionResolveReenrollment) {
			return fmt.Errorf("item %s codex re-enrollment binding has no recovery action: %w",
				i.ID, ErrCodexReenrollmentBindingMismatch)
		}
	} else if i.Type == AttentionSystemHealth && i.Offers(ActionResolveReenrollment) {
		return fmt.Errorf("item %s offers codex re-enrollment recovery without a binding: %w",
			i.ID, ErrCodexReenrollmentBindingMissing)
	}
	if i.Posture == nil {
		if i.Type == AttentionSystemHealth {
			return fmt.Errorf("item %s type %q has no health posture: %w",
				i.ID, i.Type, ErrHealthPostureInconsistent)
		}
	} else {
		if i.Type != AttentionSystemHealth {
			return fmt.Errorf("item %s type %q carries a health posture: %w",
				i.ID, i.Type, ErrHealthPostureInconsistent)
		}
		if !i.Posture.valid() {
			return fmt.Errorf("item %s health posture %q: %w",
				i.ID, *i.Posture, ErrInvalidHealthPosture)
		}
	}
	if i.BlockingSupersession != nil {
		// Blocking is a system_health semantic (plan §4): only that type's
		// blocking effect can be superseded, so a condition on any other type
		// is a malformed item, not a benign extra.
		if i.Type != AttentionSystemHealth {
			return fmt.Errorf("item %s type %q carries a blocking supersession: %w",
				i.ID, i.Type, ErrSupersessionOutsideSystemHealth)
		}
		if i.Posture == nil || *i.Posture != HealthPostureBlocking {
			return fmt.Errorf("item %s posture cannot carry a blocking supersession: %w",
				i.ID, ErrSupersessionOnAdvisoryHealth)
		}
		if err := i.BlockingSupersession.Validate(); err != nil {
			return fmt.Errorf("item %s: %w", i.ID, err)
		}
	}
	if i.BaseFreshness != nil {
		// Base freshness is a ready_for_final_review semantic (plan §5.16):
		// the watch binds to a published PR awaiting review, so the fact on
		// any other type is a malformed item.
		if i.Type != AttentionReadyForFinalReview {
			return fmt.Errorf("item %s type %q carries base freshness: %w",
				i.ID, i.Type, ErrBaseFreshnessOutsideReview)
		}
		if err := i.BaseFreshness.Validate(); err != nil {
			return fmt.Errorf("item %s: %w", i.ID, err)
		}
	}
	if i.ReadinessInvalidation != nil {
		// Readiness invalidation is a ready_for_final_review semantic (plan §7,
		// issue #496): the fact records why that item's clean pass no longer
		// binds, so it on any other type is a malformed item.
		if i.Type != AttentionReadyForFinalReview {
			return fmt.Errorf("item %s type %q carries a readiness invalidation: %w",
				i.ID, i.Type, ErrReadinessInvalidationOutsideReview)
		}
		// The fact is produced only in the same transition that supersedes the
		// item (§7, issue #496), so it is valid only on a superseded item. Fail
		// closed at this reconstruction/caller boundary: a decoded row or an
		// exported struct carrying the stale-pass fact on a still-actionable
		// item would otherwise leave clients free to submit decisions, since
		// command rejection is keyed to status/version, not to this fact.
		if i.Status != StatusSuperseded {
			return fmt.Errorf("item %s status %q carries a readiness invalidation: %w",
				i.ID, i.Status, ErrReadinessInvalidationNotSuperseded)
		}
		if err := i.ReadinessInvalidation.Validate(); err != nil {
			return fmt.Errorf("item %s: %w", i.ID, err)
		}
	}
	if i.DecidedAt != nil {
		if i.DecidedAt.IsZero() {
			return fmt.Errorf("item %s decided_at: %w", i.ID, ErrMissingTimestamp)
		}
		// The item body is a canonical persisted encoding whose re-put
		// convergence is a byte compare: a non-UTC instant is the same moment
		// in a different byte form, so it would give one stamp two encodings.
		if i.DecidedAt.Location() != time.UTC {
			return fmt.Errorf("item %s decided_at: %w", i.ID, ErrTimestampNotUTC)
		}
	}
	// DecidedAt presence is deliberately not coupled to Status here: terminal
	// items may legitimately carry no decision (expiry, supersession, rows
	// persisted before the field existed, and this Validate re-runs on every
	// store decode), and the lifecycle rule (only a concluding command stamps)
	// is the signet transaction's, not a structural invariant.
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
	if want := bindingDigests(i.EvidenceSnapshot, i.AgentClaims, i.FindingAdjudication); !slices.Equal(i.ArtifactDigests, want) {
		return fmt.Errorf("item %s artifact_digests %v, rendered digests resolve to %v: %w", i.ID, i.ArtifactDigests, want, ErrBindingMismatch)
	}
	return nil
}

// bindingDigests returns an item's canonical binding set: the sorted,
// deduplicated union of the digests rendered in its evidence snapshot, agent
// claims, and typed finding-adjudication binding. It is the single definition
// of what an approval binds, so
// NewAttentionItem derives ArtifactDigests from it and Validate requires
// equality. It always returns a non-nil slice, so an item that renders no
// artifacts (e.g. a system_health acknowledgement) serializes artifact_digests
// as "[]", matching the required, non-null array the wire contract declares
// (api/openapi.yaml). slices.Equal treats nil and empty as equal, so a value
// decoded from a legacy null still satisfies the equality check.
func bindingDigests(
	evidence []Artifact, claims []AgentClaim, adjudication *FindingAdjudicationBinding,
) []Digest {
	out := make([]Digest, 0, len(evidence)+len(claims)+1)
	for _, a := range evidence {
		out = append(out, a.Digest)
	}
	for _, c := range claims {
		out = append(out, c.Digest)
	}
	if adjudication != nil {
		out = append(out, adjudication.AdjudicationDigest)
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

// WithDecidedAt returns a copy of the item stamped with the instant its first
// concluding decision was accepted. It is the only writer of DecidedAt: the
// signet accepting transaction calls it when a concluding command commits, so
// the stamp and the terminal status land in one write (issue #171). The
// instant must be a real UTC time (the same canonicality Validate enforces);
// re-stamping an already-decided item is refused here and erasure is refused
// by ValidateAttentionItemTransition, so the recorded decision endpoint of the
// open-to-decision metric cannot drift under replays or later re-puts.
func (i AttentionItem) WithDecidedAt(t time.Time) (AttentionItem, error) {
	if t.IsZero() {
		return AttentionItem{}, fmt.Errorf("item %s decided_at: %w", i.ID, ErrMissingTimestamp)
	}
	if t.Location() != time.UTC {
		return AttentionItem{}, fmt.Errorf("item %s decided_at: %w", i.ID, ErrTimestampNotUTC)
	}
	if i.DecidedAt != nil {
		return AttentionItem{}, fmt.Errorf("item %s decided_at already recorded: %w", i.ID, ErrImmutableTransition)
	}
	i.DecidedAt = &t
	return i, nil
}

// CanonicalizeStoredRow rewrites a decoded stored row's fields to their
// canonical spelling before Validate runs, so a row written under an older,
// looser encoding converges instead of being refused by the current, stricter
// Validate. It must stay lossless (the same instant in canonical byte form):
// the store's put-idempotence compare is a canonical re-encode, so a legacy row
// and its fresh equivalent have to marshal identically once this has run.
//
// Today the only such field is ExpiresWhen, whose UTC check (issue #553)
// post-dates a dev-CLI producer that persisted a host-local offset; rewriting
// it to UTC is the same instant in the spelling Validate now demands. DecidedAt
// needs no entry: its write gate has always rejected non-UTC, so no offset row
// of it can exist to canonicalize.
func (i *AttentionItem) CanonicalizeStoredRow() {
	if i.ExpiresWhen != nil {
		utc := i.ExpiresWhen.UTC()
		i.ExpiresWhen = &utc
	}
}
