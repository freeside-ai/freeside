package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

// authorizationEncodingVersion tags the canonical encoding ComputeID digests.
// Any change to the encoding (field set, ordering, separator discipline) is a
// new version: two daemon builds must never derive different identities for
// the same bound facts, or a replay across an upgrade would mint a second
// authorization for one candidate.
const authorizationEncodingVersion = "freeside-candidate-authorization/v1"

// Waivable reports whether a finding of this class may carry a waived
// disposition at all. Plan §3.1 names control-plane modifications,
// automation-control and reviewer-instruction changes, artifact-integrity
// failure (the import_integrity class: §5.6 admits regular files only), and
// secret detection non-waivable: no decision record, human or otherwise,
// can make them publishable, so a waived finding of those classes is
// unrepresentable rather than merely rejected downstream. The switch omits
// default so a new class must decide its waivability; the trailing return
// fails closed for the invalid zero value.
func (c CandidateFindingClass) Waivable() bool {
	switch c {
	case FindingClassControlPlane, FindingClassImportIntegrity, FindingClassSecret:
		return false
	case FindingClassRepoChangePolicy:
		return true
	}
	return false
}

// WaiverRecord is the trusted decision record a waived finding binds to
// (plan §5.12: approvals bind to their digests). It is a shape only: the
// semantics of producing one (the human decision flow) belong to the
// publication gate's consumers. DecidedBy is never the agent: risk
// acceptance comes from a human or trusted daemon policy, and an
// agent-authored waiver of the agent's own candidate would be
// self-authorization.
type WaiverRecord struct {
	DecisionID     string    `json:"decision_id"`
	DecidedBy      Author    `json:"decided_by"`
	DecidedAt      time.Time `json:"decided_at"`
	Justification  string    `json:"justification"`
	DecisionDigest Digest    `json:"decision_digest"`
}

// Validate reports whether the waiver is well-formed and from a permitted
// author.
func (w WaiverRecord) Validate() error {
	if w.DecisionID == "" {
		return fmt.Errorf("waiver decision_id: %w", ErrEmptyID)
	}
	if !w.DecidedBy.valid() {
		return fmt.Errorf("waiver %s decided_by %q: %w", w.DecisionID, w.DecidedBy, ErrInvalidAuthor)
	}
	if w.DecidedBy == AuthorAgent {
		return fmt.Errorf("waiver %s: %w", w.DecisionID, ErrAgentWaiver)
	}
	if w.DecidedAt.IsZero() {
		return fmt.Errorf("waiver %s decided_at: %w", w.DecisionID, ErrMissingTimestamp)
	}
	// The waiver is part of the finding's canonical encoding, which the
	// authorization id addresses: a non-UTC instant is the same content in a
	// different byte encoding, so it would give one waiver two identities.
	if w.DecidedAt.Location() != time.UTC {
		return fmt.Errorf("waiver %s decided_at: %w", w.DecisionID, ErrTimestampNotUTC)
	}
	if w.Justification == "" {
		return fmt.Errorf("waiver %s justification: %w", w.DecisionID, ErrEmptyField)
	}
	if w.DecisionDigest == "" {
		return fmt.Errorf("waiver %s decision_digest: %w", w.DecisionID, ErrEmptyField)
	}
	return nil
}

// CandidateFinding is one candidate policy finding as the publication gate
// consumes it (plan §5.6, §5.8): classed for trust dispatch, categorized
// when it is control-plane content, and carrying the emitting package's own
// kind token so the importer and verifier vocabularies map in without this
// package enumerating them. Path and PathHex are mutually exclusive, as in
// the import manifest; no field ever carries candidate content bytes.
type CandidateFinding struct {
	Class    CandidateFindingClass  `json:"class"`
	Category *ControlPlaneCategory  `json:"category"`
	Origin   CandidateFindingOrigin `json:"origin"`
	Kind     string                 `json:"kind"`
	Path     string                 `json:"path,omitempty"`
	PathHex  string                 `json:"path_hex,omitempty"`
	Detail   string                 `json:"detail,omitempty"`
	// Disposition and Waiver travel together: waived requires a waiver
	// record and a waivable class; blocking forbids one.
	Disposition FindingDisposition `json:"disposition"`
	Waiver      *WaiverRecord      `json:"waiver"`
}

// Validate reports whether the finding is well-formed: a control-plane
// finding must name its §5.8 category (and only control-plane findings may),
// and a waived finding must carry a waiver record for a waivable class.
func (f CandidateFinding) Validate() error {
	if !f.Class.valid() {
		return fmt.Errorf("finding class %q: %w", f.Class, ErrInvalidFindingClass)
	}
	// The category is the §5.8 axis and exists only on that axis: requiring
	// it for control-plane findings keeps the complete class enumerable, and
	// forbidding it elsewhere keeps one representation per finding.
	switch f.Class {
	case FindingClassControlPlane:
		if f.Category == nil {
			return fmt.Errorf("control-plane finding %q category: %w", f.Kind, ErrCategoryInconsistent)
		}
		if !f.Category.valid() {
			return fmt.Errorf("finding %q category %q: %w", f.Kind, *f.Category, ErrInvalidFindingCategory)
		}
	case FindingClassImportIntegrity, FindingClassRepoChangePolicy, FindingClassSecret:
		if f.Category != nil {
			return fmt.Errorf("%s finding %q carries a control-plane category: %w", f.Class, f.Kind, ErrCategoryInconsistent)
		}
	}
	if !f.Origin.valid() {
		return fmt.Errorf("finding %q origin %q: %w", f.Kind, f.Origin, ErrInvalidFindingOrigin)
	}
	if f.Kind == "" {
		return fmt.Errorf("finding kind: %w", ErrEmptyField)
	}
	if f.Path != "" && f.PathHex != "" {
		return fmt.Errorf("finding %q: %w", f.Kind, ErrFindingPathConflict)
	}
	if !f.Disposition.valid() {
		return fmt.Errorf("finding %q disposition %q: %w", f.Kind, f.Disposition, ErrInvalidFindingDisposition)
	}
	switch f.Disposition {
	case DispositionWaived:
		if !f.Class.Waivable() {
			return fmt.Errorf("finding %q class %s: %w", f.Kind, f.Class, ErrNonWaivableFinding)
		}
		if f.Waiver == nil {
			return fmt.Errorf("waived finding %q: %w", f.Kind, ErrWaiverInconsistent)
		}
		if err := f.Waiver.Validate(); err != nil {
			return fmt.Errorf("finding %q: %w", f.Kind, err)
		}
	case DispositionBlocking:
		if f.Waiver != nil {
			return fmt.Errorf("blocking finding %q carries a waiver: %w", f.Kind, ErrWaiverInconsistent)
		}
	}
	return nil
}

// clone returns a copy detached from caller-owned pointers.
func (f CandidateFinding) clone() CandidateFinding {
	f.Category = clonePtr(f.Category)
	f.Waiver = clonePtr(f.Waiver)
	return f
}

// CandidateAuthorization is the immutable, daemon-authored record that binds
// everything publication is allowed to trust about one candidate (plan §5.6):
// the exact import result, the candidate head and base, the verification
// recipe and outcome, the findings and their dispositions, and the automation
// trust profile the whole run was bound to. The publication gate consumes it;
// nothing agent-authored can produce or alter one. ID and
// AuthorizesPublication are exported so the type serializes, but both are
// computed from the bound facts in NewCandidateAuthorization and never taken
// from caller input; Validate recomputes both, so a decoded or exported
// record with a forged identity or trust bit fails closed at every boundary
// that re-runs it.
type CandidateAuthorization struct {
	ID                       Digest              `json:"id"`
	Repo                     string              `json:"repo"`
	BaseSHA                  string              `json:"base_sha"`
	HeadSHA                  string              `json:"head_sha"`
	ImportResultDigest       Digest              `json:"import_result_digest"`
	VerificationRecipeDigest Digest              `json:"verification_recipe_digest"`
	VerificationOutcome      VerificationOutcome `json:"verification_outcome"`
	Findings                 []CandidateFinding  `json:"findings"`
	TrustProfileDigest       Digest              `json:"trust_profile_digest"`
	InvocationID             InvocationID        `json:"invocation_id"`
	CreatedAt                time.Time           `json:"created_at"`
	AuthorizesPublication    bool                `json:"authorizes_publication"`
}

// CandidateAuthorizationInput carries the caller-supplied fields of a
// CandidateAuthorization. It has no ID and no AuthorizesPublication field:
// the identity is a content address and the trust bit is a policy
// computation, so no input path can set either.
type CandidateAuthorizationInput struct {
	Repo                     string
	BaseSHA                  string
	HeadSHA                  string
	ImportResultDigest       Digest
	VerificationRecipeDigest Digest
	VerificationOutcome      VerificationOutcome
	Findings                 []CandidateFinding
	TrustProfileDigest       Digest
	InvocationID             InvocationID
	CreatedAt                time.Time
}

// NewCandidateAuthorization builds a validated authorization whose findings
// are in canonical order and whose ID and AuthorizesPublication are computed
// from the bound facts, so all three are authentic by construction. A failed
// verification still yields a record — a truthful, non-authorizing one — so
// the outcome is durably bound either way; only the computed bit opens the
// publication gate.
func NewCandidateAuthorization(in CandidateAuthorizationInput) (CandidateAuthorization, error) {
	findings, err := canonicalFindings(in.Findings)
	if err != nil {
		return CandidateAuthorization{}, err
	}
	a := CandidateAuthorization{
		Repo:                     in.Repo,
		BaseSHA:                  in.BaseSHA,
		HeadSHA:                  in.HeadSHA,
		ImportResultDigest:       in.ImportResultDigest,
		VerificationRecipeDigest: in.VerificationRecipeDigest,
		VerificationOutcome:      in.VerificationOutcome,
		Findings:                 findings,
		TrustProfileDigest:       in.TrustProfileDigest,
		InvocationID:             in.InvocationID,
		CreatedAt:                in.CreatedAt.UTC(),
	}
	id, err := a.ComputeID()
	if err != nil {
		return CandidateAuthorization{}, err
	}
	a.ID = id
	a.AuthorizesPublication = computeAuthorizesPublication(a.VerificationOutcome, a.Findings)
	if err := a.Validate(); err != nil {
		return CandidateAuthorization{}, err
	}
	return a, nil
}

// canonicalFindings deep-copies the findings and sorts them by their
// canonical JSON encoding, so the same finding set arrives at one body and
// one identity regardless of emission order; empty collapses to nil so "no
// findings" has a single representation. Duplicates survive the sort and are
// rejected by Validate rather than silently collapsed.
func canonicalFindings(in []CandidateFinding) ([]CandidateFinding, error) {
	if len(in) == 0 {
		return nil, nil
	}
	type encoded struct {
		finding  CandidateFinding
		encoding []byte
	}
	sorted := make([]encoded, len(in))
	for i, f := range in {
		clone := f.clone()
		enc, err := json.Marshal(clone)
		if err != nil {
			return nil, fmt.Errorf("candidate finding %q: %w", f.Kind, err)
		}
		sorted[i] = encoded{finding: clone, encoding: enc}
	}
	slices.SortStableFunc(sorted, func(a, b encoded) int {
		return bytes.Compare(a.encoding, b.encoding)
	})
	out := make([]CandidateFinding, len(sorted))
	for i, e := range sorted {
		out[i] = e.finding
	}
	return out, nil
}

// canonicalAuthorization is the versioned canonical form whose JSON encoding
// is digested. Field order is pinned by the struct declaration and the
// authorization golden test; changing either is an encoding-version bump.
type canonicalAuthorization struct {
	Version                  string              `json:"version"`
	Repo                     string              `json:"repo"`
	BaseSHA                  string              `json:"base_sha"`
	HeadSHA                  string              `json:"head_sha"`
	ImportResultDigest       Digest              `json:"import_result_digest"`
	VerificationRecipeDigest Digest              `json:"verification_recipe_digest"`
	VerificationOutcome      VerificationOutcome `json:"verification_outcome"`
	Findings                 []CandidateFinding  `json:"findings"`
	TrustProfileDigest       Digest              `json:"trust_profile_digest"`
	InvocationID             InvocationID        `json:"invocation_id"`
	CreatedAt                time.Time           `json:"created_at"`
}

// ComputeID returns the content address of the authorization: a sha256 over
// its versioned canonical serialization, every bound fact and nothing
// derived (ID and AuthorizesPublication are excluded — the former is this
// value, the latter is a policy computation over the same facts). The
// producing invocation is deliberately included: an authorization attests
// what one specific verification run observed, so a re-run over the same
// candidate is a distinct record, and the store's per-profile uniqueness key
// keeps distinct records from silently coexisting for one head. It sorts the
// findings defensively so it is a true content address for any input; a
// value that also passes Validate is already canonical.
func (a CandidateAuthorization) ComputeID() (Digest, error) {
	findings, err := canonicalFindings(a.Findings)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(canonicalAuthorization{
		Version:                  authorizationEncodingVersion,
		Repo:                     a.Repo,
		BaseSHA:                  a.BaseSHA,
		HeadSHA:                  a.HeadSHA,
		ImportResultDigest:       a.ImportResultDigest,
		VerificationRecipeDigest: a.VerificationRecipeDigest,
		VerificationOutcome:      a.VerificationOutcome,
		Findings:                 findings,
		TrustProfileDigest:       a.TrustProfileDigest,
		InvocationID:             a.InvocationID,
		CreatedAt:                a.CreatedAt,
	})
	if err != nil {
		return "", fmt.Errorf("candidate authorization id: %w", err)
	}
	return Digest(fmt.Sprintf("sha256:%x", sha256.Sum256(body))), nil
}

// computeAuthorizesPublication implements the publication-authorization
// policy (plan §5.6): only a passed verification with no effectively
// blocking finding authorizes publication. It is unexported so only trusted
// construction and validation reach it. Both switches omit default so a new
// outcome or disposition must decide its stance here (exhaustive lint); the
// trailing returns fail closed for the invalid zero values.
func computeAuthorizesPublication(outcome VerificationOutcome, findings []CandidateFinding) bool {
	switch outcome {
	case VerificationFailed:
		return false
	case VerificationPassed:
		for _, f := range findings {
			if findingBlocks(f.Disposition) {
				return false
			}
		}
		return true
	}
	return false
}

// findingBlocks reports whether a finding's disposition leaves it
// publication-blocking. Validate has already bound waived to a waivable
// class and a waiver record, so the disposition alone decides here.
func findingBlocks(d FindingDisposition) bool {
	switch d {
	case DispositionBlocking:
		return true
	case DispositionWaived:
		return false
	}
	return true
}

// Validate reports whether the authorization is well-formed and its derived
// fields authentic. The ID is a content address and AuthorizesPublication a
// policy computation, never caller labels: both are recomputed and a
// mismatch rejected, so a decoded row or exported struct carrying a forged
// identity or trust bit fails closed at every boundary that re-runs Validate
// (the store's encode/decode both do), with no external policy input needed
// — the derivations close over the record's own bound facts.
func (a CandidateAuthorization) Validate() error {
	if a.Repo == "" {
		return fmt.Errorf("authorization repo: %w", ErrEmptyField)
	}
	if a.BaseSHA == "" {
		return fmt.Errorf("authorization base_sha: %w", ErrEmptyField)
	}
	if a.HeadSHA == "" {
		return fmt.Errorf("authorization head_sha: %w", ErrEmptyField)
	}
	if a.ImportResultDigest == "" {
		return fmt.Errorf("authorization import_result_digest: %w", ErrEmptyField)
	}
	if a.VerificationRecipeDigest == "" {
		return fmt.Errorf("authorization verification_recipe_digest: %w", ErrEmptyField)
	}
	if !a.VerificationOutcome.valid() {
		return fmt.Errorf("authorization verification_outcome %q: %w", a.VerificationOutcome, ErrInvalidOutcome)
	}
	if a.TrustProfileDigest == "" {
		return fmt.Errorf("authorization trust_profile_digest: %w", ErrEmptyField)
	}
	if a.InvocationID == "" {
		return fmt.Errorf("authorization invocation_id: %w", ErrEmptyID)
	}
	if a.CreatedAt.IsZero() {
		return fmt.Errorf("authorization created_at: %w", ErrMissingTimestamp)
	}
	// created_at is part of the canonical encoding the id addresses: the
	// constructor normalizes to UTC, and the literal/decode paths must hold
	// the same form, or one instant would yield two valid identities.
	if a.CreatedAt.Location() != time.UTC {
		return fmt.Errorf("authorization created_at: %w", ErrTimestampNotUTC)
	}
	// A non-nil empty finding list is the same content as nil in a different
	// byte encoding ("[]" vs null); one representation per content is what
	// write-once replay convergence depends on.
	if a.Findings != nil && len(a.Findings) == 0 {
		return fmt.Errorf("authorization findings: empty list must be nil: %w", ErrFindingsNotCanonical)
	}
	// Findings must be canonically ordered and distinct: canonical order is
	// what makes the persisted body carry exactly the finding set the ID
	// addresses, and an exact duplicate is one finding recorded twice.
	var prev []byte
	for i := range a.Findings {
		f := a.Findings[i]
		if err := f.Validate(); err != nil {
			return err
		}
		enc, err := json.Marshal(f)
		if err != nil {
			return fmt.Errorf("authorization finding %q: %w", f.Kind, err)
		}
		if i > 0 {
			switch bytes.Compare(prev, enc) {
			case 0:
				return fmt.Errorf("authorization finding %q: %w", f.Kind, ErrDuplicate)
			case 1:
				return fmt.Errorf("authorization finding %q: %w", f.Kind, ErrFindingsNotCanonical)
			}
		}
		prev = enc
	}
	if a.ID == "" {
		return fmt.Errorf("authorization id: %w", ErrEmptyID)
	}
	computed, err := a.ComputeID()
	if err != nil {
		return err
	}
	if a.ID != computed {
		return fmt.Errorf("authorization %s id, content resolves to %s: %w", a.ID, computed, ErrAuthorizationInconsistent)
	}
	if a.AuthorizesPublication != computeAuthorizesPublication(a.VerificationOutcome, a.Findings) {
		return fmt.Errorf("authorization %s authorizes_publication: %w", a.ID, ErrAuthorizationInconsistent)
	}
	return nil
}
