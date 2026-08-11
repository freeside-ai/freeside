package domain

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

const (
	requirementResolutionEncodingVersion = "freeside.requirement-resolution/v1"
	checkProofEncodingVersion            = "freeside.check-proof/v1"
	waiverLifecycleEncodingVersion       = "freeside.waiver-lifecycle/v1"
	evaluationSetEncodingVersion         = "freeside.evaluation-set/v2"
	productionRequirementSetVersion      = "freeside.production-requirement-set/v1"

	// CurrentVerificationFloorRegistryGeneration is the first registered
	// verification algebra. Later generations may tighten floors and waiver
	// eligibility, never reinterpret an older generation as current.
	CurrentVerificationFloorRegistryGeneration uint64 = 1
)

// RequirementKind records whether an applicable check is required for
// readiness or advisory only. The zero value is invalid.
type RequirementKind string

const (
	RequirementRequired RequirementKind = "required"
	RequirementOptional RequirementKind = "optional"
)

var AllRequirementKinds = []RequirementKind{RequirementRequired, RequirementOptional}

func (k RequirementKind) valid() bool {
	switch k {
	case RequirementRequired, RequirementOptional:
		return true
	default:
		return false
	}
}

// AdvisoryOutcome is a non-passing applicable result. Passed is deliberately
// absent because a pass is represented only by a CheckProof.
type AdvisoryOutcome string

const (
	AdvisoryFailed AdvisoryOutcome = "failed"
	AdvisoryNotRun AdvisoryOutcome = "not_run"
)

var AllAdvisoryOutcomes = []AdvisoryOutcome{AdvisoryFailed, AdvisoryNotRun}

func (o AdvisoryOutcome) valid() bool {
	switch o {
	case AdvisoryFailed, AdvisoryNotRun:
		return true
	default:
		return false
	}
}

// ReadinessVerdictClass is the aggregate verification result. There is no
// boolean projection: consumers must preserve clean versus degraded readiness.
type ReadinessVerdictClass string

const (
	ReadinessBlocked       ReadinessVerdictClass = "blocked"
	ReadinessReadyClean    ReadinessVerdictClass = "ready_clean"
	ReadinessReadyDegraded ReadinessVerdictClass = "ready_degraded"
)

var AllReadinessVerdictClasses = []ReadinessVerdictClass{
	ReadinessBlocked, ReadinessReadyClean, ReadinessReadyDegraded,
}

func (c ReadinessVerdictClass) valid() bool {
	switch c {
	case ReadinessBlocked, ReadinessReadyClean, ReadinessReadyDegraded:
		return true
	default:
		return false
	}
}

// WaiverGrantingAuthority is the closed set that can accept degraded
// readiness. Resolved policy and agent inference are intentionally absent.
type WaiverGrantingAuthority string

const (
	WaiverAuthorityHumanApproval WaiverGrantingAuthority = "explicit_human_approval"
	WaiverAuthorityTrustedConfig WaiverGrantingAuthority = "daemon_trusted_configuration"
)

var AllWaiverGrantingAuthorities = []WaiverGrantingAuthority{
	WaiverAuthorityHumanApproval, WaiverAuthorityTrustedConfig,
}

func (a WaiverGrantingAuthority) valid() bool {
	switch a {
	case WaiverAuthorityHumanApproval, WaiverAuthorityTrustedConfig:
		return true
	default:
		return false
	}
}

// VerificationCheckClass names a registered requirement class. Waivability
// is a closed dispatch below, so adding a class must make an explicit safety
// decision instead of inheriting a permissive default.
type VerificationCheckClass string

const (
	CheckClassCleanVerification VerificationCheckClass = "clean_verification"
	CheckClassIndependentReview VerificationCheckClass = "independent_review"
	CheckClassRepoChangePolicy  VerificationCheckClass = "repo_change_policy"
)

var AllVerificationCheckClasses = []VerificationCheckClass{
	CheckClassCleanVerification, CheckClassIndependentReview, CheckClassRepoChangePolicy,
}

func (c VerificationCheckClass) valid() bool {
	switch c {
	case CheckClassCleanVerification, CheckClassIndependentReview, CheckClassRepoChangePolicy:
		return true
	default:
		return false
	}
}

// WaiverEligible reports whether this check class has a waiver representation.
func (c VerificationCheckClass) WaiverEligible() bool {
	switch c {
	case CheckClassCleanVerification, CheckClassIndependentReview:
		return false
	case CheckClassRepoChangePolicy:
		return true
	}
	return false
}

// RequirementDefinition is one daemon-owned requirement in a trusted
// requirement set. The store's registration gate compares a persisted or
// decoded resolution's policy-bearing fields against the definition its set
// and key name, so requiredness and applicability are never caller-asserted.
type RequirementDefinition struct {
	Key           RequirementKey
	Class         VerificationCheckClass
	Kind          RequirementKind
	Applicable    bool
	BaseDependent bool
}

// ProductionRequirementDefinitions is the compiled §6 production requirement
// set. Order is part of the contract: consumers may address members by index.
func ProductionRequirementDefinitions() []RequirementDefinition {
	return []RequirementDefinition{
		{
			Key: "clean-verification", Class: CheckClassCleanVerification,
			Kind: RequirementRequired, Applicable: true, BaseDependent: true,
		},
		{
			Key: "independent-review", Class: CheckClassIndependentReview,
			Kind: RequirementRequired, Applicable: true, BaseDependent: true,
		},
	}
}

// ProductionRequirementSetDigest derives the production set's identity for a
// floor/registry generation. The one derivation is shared by the engine's
// resolution construction and the store's registration gate, so the two can
// never drift apart.
func ProductionRequirementSetDigest(generation uint64) (Digest, error) {
	type requirementDefinition struct {
		Key           RequirementKey         `json:"key"`
		Class         VerificationCheckClass `json:"class"`
		Kind          RequirementKind        `json:"kind"`
		BaseDependent bool                   `json:"base_dependent"`
	}
	definitions := ProductionRequirementDefinitions()
	checks := make([]requirementDefinition, 0, len(definitions))
	for _, definition := range definitions {
		checks = append(checks, requirementDefinition{
			Key: definition.Key, Class: definition.Class,
			Kind: definition.Kind, BaseDependent: definition.BaseDependent,
		})
	}
	body, err := json.Marshal(struct {
		Version    string                  `json:"version"`
		Generation uint64                  `json:"floor_registry_generation"`
		Checks     []requirementDefinition `json:"checks"`
	}{productionRequirementSetVersion, generation, checks})
	if err != nil {
		return "", err
	}
	return Digest(contentaddr.Sum(body)), nil
}

// WaiverLifecycleStatus is one append-only waiver event state.
type WaiverLifecycleStatus string

const (
	WaiverLifecycleGranted WaiverLifecycleStatus = "granted"
	WaiverLifecycleRevoked WaiverLifecycleStatus = "revoked"
	WaiverLifecycleExpired WaiverLifecycleStatus = "expired"
)

var AllWaiverLifecycleStatuses = []WaiverLifecycleStatus{
	WaiverLifecycleGranted, WaiverLifecycleRevoked, WaiverLifecycleExpired,
}

func (s WaiverLifecycleStatus) valid() bool {
	switch s {
	case WaiverLifecycleGranted, WaiverLifecycleRevoked, WaiverLifecycleExpired:
		return true
	default:
		return false
	}
}

// RequirementResolution is the shared binding for both applicability
// branches. Digest authenticates every fact that could change requiredness.
type RequirementResolution struct {
	Digest                  Digest                 `json:"digest"`
	RequirementKey          RequirementKey         `json:"requirement_key"`
	CheckClass              VerificationCheckClass `json:"check_class"`
	Kind                    RequirementKind        `json:"kind"`
	Applicable              bool                   `json:"applicable"`
	BaseDependent           bool                   `json:"base_dependent"`
	RequirementSetDigest    Digest                 `json:"requirement_set_digest"`
	FloorRegistryGeneration uint64                 `json:"floor_registry_generation"`
	ResolvedPolicyDigest    Digest                 `json:"resolved_policy_digest"`
	SamplingDecision        *Digest                `json:"sampling_decision"`
}

type RequirementResolutionInput struct {
	RequirementKey          RequirementKey
	CheckClass              VerificationCheckClass
	Kind                    RequirementKind
	Applicable              bool
	BaseDependent           bool
	RequirementSetDigest    Digest
	FloorRegistryGeneration uint64
	ResolvedPolicyDigest    Digest
	SamplingDecision        *Digest
}

func NewRequirementResolution(in RequirementResolutionInput) (RequirementResolution, error) {
	r := RequirementResolution{
		RequirementKey: in.RequirementKey, CheckClass: in.CheckClass, Kind: in.Kind,
		Applicable: in.Applicable, BaseDependent: in.BaseDependent,
		RequirementSetDigest:    in.RequirementSetDigest,
		FloorRegistryGeneration: in.FloorRegistryGeneration,
		ResolvedPolicyDigest:    in.ResolvedPolicyDigest,
		SamplingDecision:        clonePtr(in.SamplingDecision),
	}
	digest, err := r.computeDigest()
	if err != nil {
		return RequirementResolution{}, err
	}
	r.Digest = digest
	if err := r.Validate(); err != nil {
		return RequirementResolution{}, err
	}
	return r, nil
}

func (r RequirementResolution) canonical() any {
	return struct {
		Version                 string                 `json:"version"`
		RequirementKey          RequirementKey         `json:"requirement_key"`
		CheckClass              VerificationCheckClass `json:"check_class"`
		Kind                    RequirementKind        `json:"kind"`
		Applicable              bool                   `json:"applicable"`
		BaseDependent           bool                   `json:"base_dependent"`
		RequirementSetDigest    Digest                 `json:"requirement_set_digest"`
		FloorRegistryGeneration uint64                 `json:"floor_registry_generation"`
		ResolvedPolicyDigest    Digest                 `json:"resolved_policy_digest"`
		SamplingDecision        *Digest                `json:"sampling_decision"`
	}{
		requirementResolutionEncodingVersion, r.RequirementKey, r.CheckClass, r.Kind,
		r.Applicable, r.BaseDependent, r.RequirementSetDigest, r.FloorRegistryGeneration,
		r.ResolvedPolicyDigest, r.SamplingDecision,
	}
}

func (r RequirementResolution) computeDigest() (Digest, error) {
	body, err := json.Marshal(r.canonical())
	if err != nil {
		return "", err
	}
	return Digest(contentaddr.Sum(body)), nil
}

func (r RequirementResolution) Validate() error {
	switch {
	case r.RequirementKey == "":
		return fmt.Errorf("requirement resolution key: %w", ErrEmptyID)
	case !r.CheckClass.valid():
		return fmt.Errorf("requirement resolution class %q: %w", r.CheckClass, ErrInvalidVerificationCheckClass)
	case !r.Kind.valid():
		return fmt.Errorf("requirement resolution kind %q: %w", r.Kind, ErrInvalidRequirementKind)
	case r.RequirementSetDigest == "" || r.ResolvedPolicyDigest == "":
		return fmt.Errorf("requirement resolution binding: %w", ErrEmptyField)
	case r.SamplingDecision != nil && *r.SamplingDecision == "":
		return fmt.Errorf("requirement resolution sampling decision: %w", ErrEmptyField)
	case r.FloorRegistryGeneration == 0:
		return fmt.Errorf("requirement resolution floor generation: %w", ErrNonPositive)
	}
	digest, err := r.computeDigest()
	if err != nil || digest != r.Digest {
		return fmt.Errorf("requirement resolution digest: %w", ErrParentKeyMismatch)
	}
	return nil
}

// CheckProof proves one applicable requirement passed. Base is required
// exactly when the resolution declares that its evidence depends on base.
type CheckProof struct {
	Digest                      Digest        `json:"digest"`
	RequirementResolutionDigest Digest        `json:"requirement_resolution_digest"`
	CandidateHead               string        `json:"candidate_head"`
	Base                        *BaseRevision `json:"base"`
	RecipeDigest                Digest        `json:"recipe_digest"`
}

func NewCheckProof(resolution RequirementResolution, candidateHead string, base *BaseRevision, recipe Digest) (CheckProof, error) {
	p := CheckProof{
		RequirementResolutionDigest: resolution.Digest, CandidateHead: candidateHead,
		Base: clonePtr(base), RecipeDigest: recipe,
	}
	digest, err := p.computeDigest()
	if err != nil {
		return CheckProof{}, err
	}
	p.Digest = digest
	if err := p.validateFor(resolution); err != nil {
		return CheckProof{}, err
	}
	return p, nil
}

func (p CheckProof) computeDigest() (Digest, error) {
	body, err := json.Marshal(struct {
		Version                     string        `json:"version"`
		RequirementResolutionDigest Digest        `json:"requirement_resolution_digest"`
		CandidateHead               string        `json:"candidate_head"`
		Base                        *BaseRevision `json:"base"`
		RecipeDigest                Digest        `json:"recipe_digest"`
	}{checkProofEncodingVersion, p.RequirementResolutionDigest, p.CandidateHead, p.Base, p.RecipeDigest})
	if err != nil {
		return "", err
	}
	return Digest(contentaddr.Sum(body)), nil
}

func (p CheckProof) Validate() error {
	if p.RequirementResolutionDigest == "" || p.CandidateHead == "" || p.RecipeDigest == "" {
		return fmt.Errorf("check proof binding: %w", ErrEmptyField)
	}
	if p.Base != nil {
		if err := p.Base.Validate(); err != nil {
			return fmt.Errorf("check proof base: %w", err)
		}
	}
	digest, err := p.computeDigest()
	if err != nil || digest != p.Digest {
		return fmt.Errorf("check proof digest: %w", ErrParentKeyMismatch)
	}
	return nil
}

func (p CheckProof) validateFor(r RequirementResolution) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if p.RequirementResolutionDigest != r.Digest || (p.Base != nil) != r.BaseDependent {
		return ErrParentKeyMismatch
	}
	return nil
}

// WaiverLifecycleEvent is one immutable event in a waiver's append-only
// lifecycle. EventDigest chains the previous event, preserving historical
// applicability without making a mutable ready bit authoritative.
type WaiverLifecycleEvent struct {
	WaiverID       WaiverID              `json:"waiver_id"`
	Sequence       uint64                `json:"sequence"`
	Status         WaiverLifecycleStatus `json:"status"`
	PreviousDigest *Digest               `json:"previous_digest"`
	EventDigest    Digest                `json:"event_digest"`
	RecordedAt     time.Time             `json:"recorded_at"`
}

func NewWaiverLifecycleEvent(id WaiverID, sequence uint64, status WaiverLifecycleStatus, previous *Digest, at time.Time) (WaiverLifecycleEvent, error) {
	e := WaiverLifecycleEvent{
		WaiverID: id, Sequence: sequence, Status: status,
		PreviousDigest: clonePtr(previous), RecordedAt: at.UTC(),
	}
	digest, err := e.computeDigest()
	if err != nil {
		return WaiverLifecycleEvent{}, err
	}
	e.EventDigest = digest
	if err := e.Validate(); err != nil {
		return WaiverLifecycleEvent{}, err
	}
	return e, nil
}

func (e WaiverLifecycleEvent) computeDigest() (Digest, error) {
	body, err := json.Marshal(struct {
		Version        string                `json:"version"`
		WaiverID       WaiverID              `json:"waiver_id"`
		Sequence       uint64                `json:"sequence"`
		Status         WaiverLifecycleStatus `json:"status"`
		PreviousDigest *Digest               `json:"previous_digest"`
		RecordedAt     time.Time             `json:"recorded_at"`
	}{waiverLifecycleEncodingVersion, e.WaiverID, e.Sequence, e.Status, e.PreviousDigest, e.RecordedAt})
	if err != nil {
		return "", err
	}
	return Digest(contentaddr.Sum(body)), nil
}

func (e WaiverLifecycleEvent) Validate() error {
	if e.WaiverID == "" {
		return fmt.Errorf("waiver lifecycle id: %w", ErrEmptyID)
	}
	if e.Sequence == 0 {
		return fmt.Errorf("waiver lifecycle sequence: %w", ErrNonPositive)
	}
	if !e.Status.valid() {
		return fmt.Errorf("waiver lifecycle status %q: %w", e.Status, ErrInvalidWaiverLifecycleStatus)
	}
	if (e.Sequence == 1) != (e.PreviousDigest == nil) ||
		(e.Sequence == 1 && e.Status != WaiverLifecycleGranted) ||
		(e.Sequence > 1 && e.Status == WaiverLifecycleGranted) {
		return fmt.Errorf("waiver lifecycle chain: %w", ErrCheckStateInconsistent)
	}
	if e.RecordedAt.IsZero() || e.RecordedAt.Location() != time.UTC {
		return fmt.Errorf("waiver lifecycle recorded_at: %w", ErrTimestampNotUTC)
	}
	digest, err := e.computeDigest()
	if err != nil || digest != e.EventDigest {
		return fmt.Errorf("waiver lifecycle digest: %w", ErrParentKeyMismatch)
	}
	return nil
}

// ValidatedDegradedWaiver is a grant bound to a required resolution and the
// exact active lifecycle frontier. It can enter a CheckState only through the
// package constructor below.
type ValidatedDegradedWaiver struct {
	ID                          WaiverID                `json:"id"`
	RequirementResolutionDigest Digest                  `json:"requirement_resolution_digest"`
	Dimension                   string                  `json:"dimension"`
	Authority                   WaiverGrantingAuthority `json:"authority"`
	GrantDigest                 Digest                  `json:"grant_digest"`
	LifecycleDigest             Digest                  `json:"lifecycle_digest"`
	FloorRegistryGeneration     uint64                  `json:"floor_registry_generation"`
	GrantedAt                   time.Time               `json:"granted_at"`
}

func NewValidatedDegradedWaiver(resolution RequirementResolution, id WaiverID, dimension string, authority WaiverGrantingAuthority, grantDigest Digest, lifecycle WaiverLifecycleEvent, grantedAt time.Time) (ValidatedDegradedWaiver, error) {
	w := ValidatedDegradedWaiver{
		ID: id, RequirementResolutionDigest: resolution.Digest,
		Dimension: dimension, Authority: authority, GrantDigest: grantDigest,
		LifecycleDigest:         lifecycle.EventDigest,
		FloorRegistryGeneration: resolution.FloorRegistryGeneration, GrantedAt: grantedAt.UTC(),
	}
	if err := w.validateFor(resolution, lifecycle); err != nil {
		return ValidatedDegradedWaiver{}, err
	}
	return w, nil
}

func (w ValidatedDegradedWaiver) Validate() error {
	if w.ID == "" || w.RequirementResolutionDigest == "" {
		return fmt.Errorf("degraded waiver identity: %w", ErrEmptyID)
	}
	if w.Dimension == "" || w.GrantDigest == "" || w.LifecycleDigest == "" {
		return fmt.Errorf("degraded waiver binding: %w", ErrEmptyField)
	}
	if !w.Authority.valid() {
		return fmt.Errorf("degraded waiver authority %q: %w", w.Authority, ErrInvalidWaiverGrantingAuthority)
	}
	if w.FloorRegistryGeneration == 0 {
		return fmt.Errorf("degraded waiver floor generation: %w", ErrNonPositive)
	}
	if w.GrantedAt.IsZero() || w.GrantedAt.Location() != time.UTC {
		return fmt.Errorf("degraded waiver granted_at: %w", ErrTimestampNotUTC)
	}
	return nil
}

func (w ValidatedDegradedWaiver) validateFor(r RequirementResolution, lifecycle WaiverLifecycleEvent) error {
	if err := w.Validate(); err != nil {
		return err
	}
	if err := lifecycle.Validate(); err != nil {
		return err
	}
	if r.Kind != RequirementRequired || !r.CheckClass.WaiverEligible() ||
		w.RequirementResolutionDigest != r.Digest ||
		w.FloorRegistryGeneration != r.FloorRegistryGeneration ||
		lifecycle.WaiverID != w.ID || lifecycle.Status != WaiverLifecycleGranted ||
		lifecycle.EventDigest != w.LifecycleDigest {
		return ErrWaiverInconsistent
	}
	return nil
}

// ValidateDegradedWaiver re-runs the complete grant gate at a reconstruction
// boundary, including the current lifecycle frontier supplied by the store.
func ValidateDegradedWaiver(r RequirementResolution, lifecycle WaiverLifecycleEvent, waiver ValidatedDegradedWaiver) error {
	return waiver.validateFor(r, lifecycle)
}

// CheckFailure hides the waiver slot. Code outside domain can observe it but
// cannot attach a waiver to an optional or non-waivable requirement directly.
type CheckFailure struct {
	outcome AdvisoryOutcome
	waiver  *ValidatedDegradedWaiver
}

func (f CheckFailure) Outcome() AdvisoryOutcome         { return f.outcome }
func (f CheckFailure) Waiver() *ValidatedDegradedWaiver { return clonePtr(f.waiver) }

func (f CheckFailure) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Outcome AdvisoryOutcome          `json:"outcome"`
		Waiver  *ValidatedDegradedWaiver `json:"waiver"`
	}{f.outcome, f.waiver})
}

func (f *CheckFailure) UnmarshalJSON(body []byte) error {
	var wire struct {
		Outcome AdvisoryOutcome          `json:"outcome"`
		Waiver  *ValidatedDegradedWaiver `json:"waiver"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return err
	}
	f.outcome, f.waiver = wire.Outcome, clonePtr(wire.Waiver)
	return nil
}

// ApplicableCheckState is exactly one passed proof or one honest failure.
type ApplicableCheckState struct {
	Proof   *CheckProof   `json:"proof,omitempty"`
	Failure *CheckFailure `json:"failure,omitempty"`
}

// CheckState is the applicability sum. The resolution is repeated in both
// branches so one proof cannot occupy another requirement's state.
type CheckState struct {
	Resolution    RequirementResolution `json:"resolution"`
	NotApplicable bool                  `json:"not_applicable,omitempty"`
	Applicable    *ApplicableCheckState `json:"applicable,omitempty"`
}

func NewNotApplicableCheckState(r RequirementResolution) (CheckState, error) {
	s := CheckState{Resolution: r, NotApplicable: true}
	if err := s.Validate(); err != nil {
		return CheckState{}, err
	}
	return s, nil
}

func NewPassedCheckState(r RequirementResolution, proof CheckProof) (CheckState, error) {
	s := CheckState{Resolution: r, Applicable: &ApplicableCheckState{Proof: clonePtr(&proof)}}
	if err := s.Validate(); err != nil {
		return CheckState{}, err
	}
	return s, nil
}

func NewNonPassingCheckState(r RequirementResolution, outcome AdvisoryOutcome, waiver *ValidatedDegradedWaiver) (CheckState, error) {
	failure := &CheckFailure{outcome: outcome, waiver: clonePtr(waiver)}
	s := CheckState{Resolution: r, Applicable: &ApplicableCheckState{Failure: failure}}
	if err := s.Validate(); err != nil {
		return CheckState{}, err
	}
	return s, nil
}

func (s CheckState) Validate() error {
	if err := s.Resolution.Validate(); err != nil {
		return err
	}
	if s.NotApplicable == (s.Applicable != nil) || s.NotApplicable == s.Resolution.Applicable {
		return ErrCheckStateInconsistent
	}
	if s.NotApplicable {
		return nil
	}
	a := s.Applicable
	if (a.Proof == nil) == (a.Failure == nil) {
		return ErrCheckStateInconsistent
	}
	if a.Proof != nil {
		return a.Proof.validateFor(s.Resolution)
	}
	if !a.Failure.outcome.valid() {
		return fmt.Errorf("check failure outcome %q: %w", a.Failure.outcome, ErrInvalidAdvisoryOutcome)
	}
	if a.Failure.waiver != nil {
		if err := a.Failure.waiver.Validate(); err != nil {
			return err
		}
		if s.Resolution.Kind != RequirementRequired || !s.Resolution.CheckClass.WaiverEligible() ||
			a.Failure.waiver.RequirementResolutionDigest != s.Resolution.Digest ||
			a.Failure.waiver.FloorRegistryGeneration != s.Resolution.FloorRegistryGeneration {
			return ErrWaiverInconsistent
		}
	}
	return nil
}

// ReadinessBlockReason identifies one requirement that prevents readiness.
type ReadinessBlockReason struct {
	RequirementResolutionDigest Digest          `json:"requirement_resolution_digest"`
	Outcome                     AdvisoryOutcome `json:"outcome"`
	AbsentRecord                bool            `json:"absent_record"`
}

// AdvisoryOutcomeRecord preserves an optional non-clean result in a degraded verdict.
type AdvisoryOutcomeRecord struct {
	RequirementResolutionDigest Digest          `json:"requirement_resolution_digest"`
	Outcome                     AdvisoryOutcome `json:"outcome"`
}

// ReadinessVerdict carries one of the three aggregate classes without a
// flattened boolean. EvaluationSetDigest is present for both ready classes.
type ReadinessVerdict struct {
	Class               ReadinessVerdictClass   `json:"class"`
	EvaluationSetDigest Digest                  `json:"evaluation_set_digest,omitempty"`
	Reasons             []ReadinessBlockReason  `json:"reasons,omitempty"`
	WaiverIDs           []WaiverID              `json:"waiver_ids,omitempty"`
	AdvisoryOutcomes    []AdvisoryOutcomeRecord `json:"advisory_outcomes,omitempty"`
}

func (v ReadinessVerdict) Validate() error {
	if !v.Class.valid() {
		return fmt.Errorf("readiness verdict class %q: %w", v.Class, ErrInvalidReadinessVerdictClass)
	}
	switch v.Class {
	case ReadinessBlocked:
		if v.EvaluationSetDigest != "" || len(v.Reasons) == 0 || len(v.WaiverIDs) != 0 || len(v.AdvisoryOutcomes) != 0 {
			return ErrReadinessVerdictInconsistent
		}
	case ReadinessReadyClean:
		if v.EvaluationSetDigest == "" || len(v.Reasons) != 0 || len(v.WaiverIDs) != 0 || len(v.AdvisoryOutcomes) != 0 {
			return ErrReadinessVerdictInconsistent
		}
	case ReadinessReadyDegraded:
		if v.EvaluationSetDigest == "" || len(v.Reasons) != 0 || len(v.WaiverIDs)+len(v.AdvisoryOutcomes) == 0 {
			return ErrReadinessVerdictInconsistent
		}
	}
	seenReasons := make(map[Digest]struct{}, len(v.Reasons))
	for _, reason := range v.Reasons {
		if reason.RequirementResolutionDigest == "" || !reason.Outcome.valid() {
			return ErrReadinessVerdictInconsistent
		}
		if _, duplicate := seenReasons[reason.RequirementResolutionDigest]; duplicate {
			return ErrReadinessVerdictInconsistent
		}
		seenReasons[reason.RequirementResolutionDigest] = struct{}{}
	}
	seenWaivers := make(map[WaiverID]struct{}, len(v.WaiverIDs))
	for _, id := range v.WaiverIDs {
		if id == "" {
			return ErrReadinessVerdictInconsistent
		}
		if _, duplicate := seenWaivers[id]; duplicate {
			return ErrReadinessVerdictInconsistent
		}
		seenWaivers[id] = struct{}{}
	}
	seenAdvisories := make(map[Digest]struct{}, len(v.AdvisoryOutcomes))
	for _, advisory := range v.AdvisoryOutcomes {
		if advisory.RequirementResolutionDigest == "" || !advisory.Outcome.valid() {
			return ErrReadinessVerdictInconsistent
		}
		if _, duplicate := seenAdvisories[advisory.RequirementResolutionDigest]; duplicate {
			return ErrReadinessVerdictInconsistent
		}
		seenAdvisories[advisory.RequirementResolutionDigest] = struct{}{}
	}
	return nil
}

type evaluationEntry struct {
	Resolution RequirementResolution `json:"resolution"`
	Missing    bool                  `json:"missing"`
	State      *CheckState           `json:"state"`
}

// DegradedWaiverGate re-authenticates a waiver at evaluation time. Its owner
// supplies the current lifecycle frontier and grant authority, so a decoded or
// cached CheckState cannot retain a previously valid waiver after either
// changes.
type DegradedWaiverGate func(RequirementResolution, ValidatedDegradedWaiver) error

// EvaluationTarget names the one candidate an evaluation covers. Every
// passed proof must bind exactly this head, and every base-dependent proof
// exactly this base, so individually valid evidence gathered against another
// candidate can never combine into readiness for this one.
type EvaluationTarget struct {
	CandidateHead string        `json:"candidate_head"`
	Base          *BaseRevision `json:"base"`
}

func (t EvaluationTarget) covers(resolution RequirementResolution, proof CheckProof) error {
	if proof.CandidateHead != t.CandidateHead {
		return fmt.Errorf("check %q candidate head: %w", resolution.RequirementKey, ErrEvaluationTargetMismatch)
	}
	if resolution.BaseDependent && (t.Base == nil || *proof.Base != *t.Base) {
		return fmt.Errorf("check %q base: %w", resolution.RequirementKey, ErrEvaluationTargetMismatch)
	}
	return nil
}

// EvaluateReadiness evaluates the target from the complete current resolution
// set. An empty set is rejected rather than trivially clean, and a missing
// state is explicitly committed as required NotRun and blocks.
func EvaluateReadiness(target EvaluationTarget, resolutions []RequirementResolution, recorded []CheckState, gate DegradedWaiverGate) (ReadinessVerdict, error) {
	if target.CandidateHead == "" {
		return ReadinessVerdict{}, fmt.Errorf("evaluation target head: %w", ErrEmptyField)
	}
	if len(resolutions) == 0 {
		return ReadinessVerdict{}, ErrRequirementSetEmpty
	}
	byDigest := make(map[Digest]CheckState, len(recorded))
	for _, state := range recorded {
		if err := state.Validate(); err != nil {
			return ReadinessVerdict{}, err
		}
		if _, duplicate := byDigest[state.Resolution.Digest]; duplicate {
			return ReadinessVerdict{}, ErrDuplicate
		}
		byDigest[state.Resolution.Digest] = state
	}
	canonical := slices.Clone(resolutions)
	sort.Slice(canonical, func(i, j int) bool { return canonical[i].RequirementKey < canonical[j].RequirementKey })
	entries := make([]evaluationEntry, 0, len(canonical))
	var reasons []ReadinessBlockReason
	var waiverIDs []WaiverID
	var advisories []AdvisoryOutcomeRecord
	seen := map[RequirementKey]struct{}{}
	var requirementSet Digest
	var floorGeneration uint64
	for index, resolution := range canonical {
		if err := resolution.Validate(); err != nil {
			return ReadinessVerdict{}, err
		}
		if index == 0 {
			requirementSet = resolution.RequirementSetDigest
			floorGeneration = resolution.FloorRegistryGeneration
		} else if resolution.RequirementSetDigest != requirementSet ||
			resolution.FloorRegistryGeneration != floorGeneration {
			return ReadinessVerdict{}, ErrParentKeyMismatch
		}
		if _, duplicate := seen[resolution.RequirementKey]; duplicate {
			return ReadinessVerdict{}, ErrDuplicate
		}
		seen[resolution.RequirementKey] = struct{}{}
		state, ok := byDigest[resolution.Digest]
		if !ok {
			entries = append(entries, evaluationEntry{Resolution: resolution, Missing: true})
			reasons = append(reasons, ReadinessBlockReason{RequirementResolutionDigest: resolution.Digest, Outcome: AdvisoryNotRun, AbsentRecord: true})
			continue
		}
		delete(byDigest, resolution.Digest)
		stateCopy := state
		entries = append(entries, evaluationEntry{Resolution: resolution, State: &stateCopy})
		if state.NotApplicable {
			continue
		}
		if state.Applicable.Proof != nil {
			if err := target.covers(resolution, *state.Applicable.Proof); err != nil {
				return ReadinessVerdict{}, err
			}
			continue
		}
		failure := state.Applicable.Failure
		if resolution.Kind == RequirementRequired {
			if failure.waiver == nil {
				reasons = append(reasons, ReadinessBlockReason{RequirementResolutionDigest: resolution.Digest, Outcome: failure.outcome})
			} else {
				if gate == nil {
					return ReadinessVerdict{}, ErrWaiverInconsistent
				}
				if err := gate(resolution, *failure.waiver); err != nil {
					return ReadinessVerdict{}, fmt.Errorf("degraded waiver %q current gate: %w", failure.waiver.ID, err)
				}
				waiverIDs = append(waiverIDs, failure.waiver.ID)
			}
		} else {
			advisories = append(advisories, AdvisoryOutcomeRecord{RequirementResolutionDigest: resolution.Digest, Outcome: failure.outcome})
		}
	}
	if len(byDigest) != 0 {
		return ReadinessVerdict{}, ErrParentKeyMismatch
	}
	if len(reasons) != 0 {
		v := ReadinessVerdict{Class: ReadinessBlocked, Reasons: reasons}
		return v, v.Validate()
	}
	body, err := json.Marshal(struct {
		Version string            `json:"version"`
		Target  EvaluationTarget  `json:"target"`
		Entries []evaluationEntry `json:"entries"`
	}{evaluationSetEncodingVersion, target, entries})
	if err != nil {
		return ReadinessVerdict{}, err
	}
	digest := Digest(contentaddr.Sum(body))
	sort.Slice(waiverIDs, func(i, j int) bool { return waiverIDs[i] < waiverIDs[j] })
	sort.Slice(advisories, func(i, j int) bool {
		return advisories[i].RequirementResolutionDigest < advisories[j].RequirementResolutionDigest
	})
	class := ReadinessReadyClean
	if len(waiverIDs)+len(advisories) != 0 {
		class = ReadinessReadyDegraded
	}
	v := ReadinessVerdict{
		Class: class, EvaluationSetDigest: digest,
		WaiverIDs: waiverIDs, AdvisoryOutcomes: advisories,
	}
	return v, v.Validate()
}
