package domain

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const (
	// FindingAdjudicationEncodingVersion tags the canonical encoding; a change
	// bumps it so every digest visibly changes rather than silently colliding.
	// Version 2 (#893) adds the digest-bound OfferedAlternatives typed set to
	// each entry; a version-1 body no longer decodes, which is a clean break for
	// this pre-release artifact type rather than a graceful upgrade.
	FindingAdjudicationEncodingVersion = 2
	// MaxFindingAdjudicationBytes bounds a decoded artifact body: a review batch
	// carries one entry per finding, each with bounded prose, so this cap is
	// generous headroom, not a design limit.
	MaxFindingAdjudicationBytes = 512 << 10
)

// FindingAdjudicationEntry is the per-finding adjudication of one review finding
// (plan §7 Finding Adjudication): a recommended route plus its two-axis
// evidence, rationale, cited repository rules, assumptions, viable alternatives,
// open questions, and the typed offered-alternative routes with their
// consequences. The route is the decision; the axes are its evidence.
//
// OfferedAlternatives is the durable authority for the executable routes an
// operator may choose instead of the recommendation, and their consequences
// (#893). It lives on this digest-bound artifact, not only on the sync-carried
// item payload, so persistence reconstruction and choose_alternative_route
// re-gate the item's offered set against a content-addressed source the item
// creator cannot forge. It is distinct from the free-text Alternatives prose,
// which stays the §7 viable-alternatives narrative.
//
// Two structural rules make the trust boundary unforgeable rather than merely
// checked. Compatibility is present exactly when the goal relationship is
// `required` — the only row where remediating here is on the table. And the
// producer distinguishes an engine fast-path routing fact (`engine`, no
// proposal confidence), a model-residue proposal (`model`, self-assessed
// confidence present), and their explicit composition (`engine_model`): the
// model supplies the goal judgment and confidence while the engine supplies
// `allowed` compatibility and remediation authority. A pure model entry can
// never carry `allowed`, because its constructor takes a ProposedCompatibility,
// whose widening has no `allowed` image.
type FindingAdjudicationEntry struct {
	FindingID        FindingID            `json:"finding_id"`
	Producer         AdjudicationProducer `json:"producer"`
	GoalRelationship GoalRelationship     `json:"goal_relationship"`
	// Compatibility is non-nil exactly when GoalRelationship is `required`.
	Compatibility *WorkUnitCompatibility `json:"compatibility"`
	Route         AdjudicationRoute      `json:"route"`
	Rationale     string                 `json:"rationale"`
	Evidence      []string               `json:"evidence"`
	CitedRules    []string               `json:"cited_rules"`
	Assumptions   []string               `json:"assumptions"`
	Alternatives  []string               `json:"alternatives"`
	OpenQuestions []string               `json:"open_questions"`
	// Confidence is non-nil exactly on model-backed (`model` or `engine_model`)
	// entries (plan §7). Its ordinal value is engine-judged against the dispatch
	// threshold by Accepted; a below-threshold value is a not-accepted proposal,
	// not an invalid entry.
	Confidence *AdjudicationConfidence `json:"confidence"`
	// OfferedAlternatives is the digest-bound set of executable routes other than
	// the recommendation, each with the consequence of choosing it (#893). It is
	// empty on every row whose (goal, compatibility) admits a single route, and
	// carries the one alternate route on the two-route `contradictory` row.
	OfferedAlternatives []OfferedAlternative `json:"offered_alternatives"`
}

// deriveOfferedAlternatives is the deterministic offered-alternative set for one
// entry's (goal, route) row. It is the single authority that populates the
// digest-bound OfferedAlternatives at construction (#893), moving the derivation
// out of the sync projection so the artifact — not the item payload — owns the
// offered routes and consequences. Only the two-route `contradictory` row offers
// an alternative: the route the recommendation did not take. Every single-route
// row offers nothing. A future model-authored offered set is a later contract
// change; the representation is already its forward-compatible home.
func deriveOfferedAlternatives(goal GoalRelationship, route AdjudicationRoute) []OfferedAlternative {
	if goal != GoalContradictory {
		return nil
	}
	if route == RouteDecline {
		return []OfferedAlternative{{
			Route:       RouteDispute,
			Consequence: "Keep the run parked for human adjudication.",
		}}
	}
	return []OfferedAlternative{{
		Route:       RouteDecline,
		Consequence: "Record the finding as declined under the artifact-bound contradiction.",
	}}
}

// NewEngineAdjudicationEntry builds a pure-engine fast-path routing fact. It may
// carry `allowed` but never proposal confidence; the separate engine-model
// constructor represents the confidence-gated composed shape. compat is non-nil
// exactly when goal is `required`.
func NewEngineAdjudicationEntry(
	findingID FindingID,
	goal GoalRelationship,
	compat *WorkUnitCompatibility,
	route AdjudicationRoute,
	rationale string,
	evidence, citedRules, assumptions, alternatives, openQuestions []string,
) (FindingAdjudicationEntry, error) {
	entry := FindingAdjudicationEntry{
		FindingID: findingID, Producer: AdjudicationProducerEngine,
		GoalRelationship: goal, Compatibility: compat, Route: route,
		Rationale: rationale, Evidence: evidence, CitedRules: citedRules,
		Assumptions: assumptions, Alternatives: alternatives, OpenQuestions: openQuestions,
		OfferedAlternatives: deriveOfferedAlternatives(goal, route),
	}
	if err := entry.Validate(); err != nil {
		return FindingAdjudicationEntry{}, err
	}
	return entry, nil
}

// NewModelAdjudicationEntry builds a model-residue proposal. compat is a
// ProposedCompatibility (the `allowed`-free subset), so `allowed` is
// unreachable through this constructor rather than merely rejected: the
// widening has no `allowed` image. compat is non-nil exactly when goal is
// `required`. confidence is the self-assessed ordinal proposal confidence.
func NewModelAdjudicationEntry(
	findingID FindingID,
	goal GoalRelationship,
	compat *ProposedCompatibility,
	route AdjudicationRoute,
	confidence AdjudicationConfidence,
	rationale string,
	evidence, citedRules, assumptions, alternatives, openQuestions []string,
) (FindingAdjudicationEntry, error) {
	var widened *WorkUnitCompatibility
	if compat != nil {
		if !compat.valid() {
			return FindingAdjudicationEntry{}, fmt.Errorf(
				"model adjudication entry %q compatibility %q: %w",
				findingID, *compat, ErrInvalidProposedCompatibility)
		}
		w := compat.compatibility()
		widened = &w
	}
	c := confidence
	entry := FindingAdjudicationEntry{
		FindingID: findingID, Producer: AdjudicationProducerModel,
		GoalRelationship: goal, Compatibility: widened, Route: route,
		Confidence: &c,
		Rationale:  rationale, Evidence: evidence, CitedRules: citedRules,
		Assumptions: assumptions, Alternatives: alternatives, OpenQuestions: openQuestions,
		OfferedAlternatives: deriveOfferedAlternatives(goal, route),
	}
	if err := entry.Validate(); err != nil {
		return FindingAdjudicationEntry{}, err
	}
	return entry, nil
}

// NewEngineModelAdjudicationEntry composes a model-produced goal judgment and
// confidence with the engine's independently derived allowed-remediation
// authority. The engine-owned half is deliberately not parameterized: this
// constructor can represent only the required/allowed/remediate row, while the
// pure model constructor's ProposedCompatibility input remains allowed-free.
func NewEngineModelAdjudicationEntry(
	findingID FindingID,
	goal GoalRelationship,
	confidence AdjudicationConfidence,
	rationale string,
	evidence, citedRules, assumptions, alternatives, openQuestions []string,
) (FindingAdjudicationEntry, error) {
	allowed := CompatibilityAllowed
	c := confidence
	entry := FindingAdjudicationEntry{
		FindingID: findingID, Producer: AdjudicationProducerEngineModel,
		GoalRelationship: goal, Compatibility: &allowed, Route: RouteRemediate,
		Confidence: &c,
		Rationale:  rationale, Evidence: evidence, CitedRules: citedRules,
		Assumptions: assumptions, Alternatives: alternatives, OpenQuestions: openQuestions,
		OfferedAlternatives: deriveOfferedAlternatives(goal, RouteRemediate),
	}
	if err := entry.Validate(); err != nil {
		return FindingAdjudicationEntry{}, err
	}
	return entry, nil
}

// Validate is the structural and trust-boundary backstop for one entry,
// including reconstructed values that bypassed a constructor.
func (e FindingAdjudicationEntry) Validate() error {
	if e.FindingID == "" {
		return fmt.Errorf("adjudication entry finding_id: %w", ErrEmptyID)
	}
	if !e.Producer.valid() {
		return fmt.Errorf("adjudication entry %q producer %q: %w", e.FindingID, e.Producer, ErrInvalidAdjudicationProducer)
	}
	if !e.GoalRelationship.valid() {
		return fmt.Errorf("adjudication entry %q goal_relationship %q: %w", e.FindingID, e.GoalRelationship, ErrInvalidGoalRelationship)
	}
	if !e.Route.valid() {
		return fmt.Errorf("adjudication entry %q route %q: %w", e.FindingID, e.Route, ErrInvalidAdjudicationRoute)
	}
	if e.Compatibility != nil && !e.Compatibility.valid() {
		return fmt.Errorf("adjudication entry %q compatibility %q: %w", e.FindingID, *e.Compatibility, ErrInvalidWorkUnitCompatibility)
	}
	if err := validAdjudicationRow(e.GoalRelationship, e.Compatibility, e.Route); err != nil {
		return fmt.Errorf("adjudication entry %q: %w", e.FindingID, err)
	}
	// A model entry can never mint `allowed`; the constructor makes it
	// unreachable and this backstops a hand-built or decoded value.
	if e.Producer == AdjudicationProducerModel && e.Compatibility != nil && *e.Compatibility == CompatibilityAllowed {
		return fmt.Errorf("adjudication entry %q: %w", e.FindingID, ErrModelEntryMintsAllowed)
	}
	// Symmetrically, an engine entry is a fast-path routing fact, and the no-model
	// fast path is one-directional toward remediation (plan §7), so it carries the
	// single `required`/`allowed`/`remediate` row and no other. Every other valid
	// row is model residue: `adjacent` "always takes the model adjudication", a
	// spec `contradictory` and an `unclear` goal are spec-relative, and the
	// `unknown` park is not an engine fact but the not-accepted representation of a
	// model proposal ("`unknown` ... only where compatibility exists (`required`)")
	// or an attention routing — EngineCompatibility's `unknown` return is a derived
	// compatibility value that fails the finding into that model residue, never an
	// engine-produced entry. An engine label on any non-remediation row forges a
	// spec-blind fast-path fact the engine cannot produce, so this backstops a
	// hand-built or decoded value just as the model rule above does.
	if e.Producer == AdjudicationProducerEngine {
		fastPath := e.GoalRelationship == GoalRequired && e.Compatibility != nil &&
			*e.Compatibility == CompatibilityAllowed
		if !fastPath {
			return fmt.Errorf("adjudication entry %q: %w", e.FindingID, ErrEngineEntryNonDeterministicRow)
		}
	}
	// The mixed provenance is just as narrow as the pure engine fast path, but
	// remains confidence-gated because its goal relationship came from the model.
	if e.Producer == AdjudicationProducerEngineModel {
		remediation := e.GoalRelationship == GoalRequired && e.Compatibility != nil &&
			*e.Compatibility == CompatibilityAllowed && e.Route == RouteRemediate
		if !remediation {
			return fmt.Errorf("adjudication entry %q: %w", e.FindingID, ErrEngineModelEntryNonRemediationRow)
		}
	}
	// Proposal confidence is present exactly on model-backed entries.
	switch e.Producer {
	case AdjudicationProducerModel, AdjudicationProducerEngineModel:
		if e.Confidence == nil {
			return fmt.Errorf("adjudication entry %q: %w", e.FindingID, ErrAdjudicationConfidenceMisplaced)
		}
		if !e.Confidence.valid() {
			return fmt.Errorf("adjudication entry %q confidence %q: %w", e.FindingID, *e.Confidence, ErrInvalidAdjudicationConfidence)
		}
	case AdjudicationProducerEngine:
		if e.Confidence != nil {
			return fmt.Errorf("adjudication entry %q: %w", e.FindingID, ErrAdjudicationConfidenceMisplaced)
		}
	}
	if strings.TrimSpace(e.Rationale) == "" {
		return fmt.Errorf("adjudication entry %q rationale: %w", e.FindingID, ErrEmptyField)
	}
	// Every free-text field is UTF-8-validated before it can be hashed or
	// persisted: json.Marshal silently rewrites invalid bytes to U+FFFD, so an
	// unguarded field would let the stored, digest-addressed artifact differ from
	// the content the caller submitted while still validating against the
	// rewritten form. Rationale is guarded here alongside the list fields below.
	if !utf8.ValidString(e.Rationale) {
		return fmt.Errorf("adjudication entry %q rationale: %w", e.FindingID, ErrFindingAdjudicationInconsistent)
	}
	for _, list := range [][]string{e.Evidence, e.CitedRules, e.Assumptions, e.Alternatives, e.OpenQuestions} {
		for _, item := range list {
			if !utf8.ValidString(item) {
				return fmt.Errorf("adjudication entry %q: %w", e.FindingID, ErrFindingAdjudicationInconsistent)
			}
		}
	}
	if err := validateOfferedAlternatives(
		e.FindingID, e.GoalRelationship, e.Compatibility, e.Route, e.OfferedAlternatives,
	); err != nil {
		return err
	}
	return nil
}

// validateOfferedAlternatives is the structural backstop for one entry's
// digest-bound offered set (#893), including a reconstructed value that bypassed
// deriveOfferedAlternatives. Each offered route must be a valid route for the
// entry's (goal, compatibility) row, distinct, and different from the
// recommendation, and each consequence non-empty valid UTF-8. It does not
// require the deterministic derivation: the digest is the authority, and a
// future model-authored offered set stays representable. The binding projection
// reuses this by validating the entry it reconstructs from each proposal.
func validateOfferedAlternatives(
	findingID FindingID,
	goal GoalRelationship,
	compat *WorkUnitCompatibility,
	recommendation AdjudicationRoute,
	offered []OfferedAlternative,
) error {
	seenRoutes := make(map[AdjudicationRoute]struct{}, len(offered))
	for _, alternative := range offered {
		if !alternative.Route.valid() {
			return fmt.Errorf("adjudication entry %q alternative route %q: %w",
				findingID, alternative.Route, ErrInvalidAdjudicationRoute)
		}
		if err := validAdjudicationRow(goal, compat, alternative.Route); err != nil {
			return fmt.Errorf("adjudication entry %q alternative route %q: %w",
				findingID, alternative.Route, err)
		}
		if alternative.Route == recommendation {
			return fmt.Errorf("adjudication entry %q alternative repeats recommendation: %w",
				findingID, ErrDuplicate)
		}
		if _, duplicate := seenRoutes[alternative.Route]; duplicate {
			return fmt.Errorf("adjudication entry %q alternative route %q: %w",
				findingID, alternative.Route, ErrDuplicate)
		}
		seenRoutes[alternative.Route] = struct{}{}
		if strings.TrimSpace(alternative.Consequence) == "" {
			return fmt.Errorf("adjudication entry %q alternative consequence: %w",
				findingID, ErrEmptyField)
		}
		if !utf8.ValidString(alternative.Consequence) {
			return fmt.Errorf("adjudication entry %q alternative consequence: %w",
				findingID, ErrFindingAdjudicationInconsistent)
		}
	}
	return nil
}

// validAdjudicationRow enforces the §7 validity table: compatibility is present
// exactly under `required`, and each (goal, compatibility) row admits exactly
// its listed route(s). `contradictory` admits two routes (decline or dispute);
// every other row admits one. The switches enumerate every enum member with no
// default — so the exhaustive linter forces a new axis member to be handled —
// and any row whose route does not match falls through to the trailing reject.
func validAdjudicationRow(goal GoalRelationship, compat *WorkUnitCompatibility, route AdjudicationRoute) error {
	if (goal == GoalRequired) != (compat != nil) {
		return ErrAdjudicationAxisMismatch
	}
	switch goal {
	case GoalRequired:
		switch *compat {
		case CompatibilityAllowed:
			if route == RouteRemediate {
				return nil
			}
		case CompatibilityWorkUnitRevision:
			if route == RouteParkRevision {
				return nil
			}
		case CompatibilitySeparateWork:
			if route == RouteParkSeparateWork {
				return nil
			}
		case CompatibilityHumanDecision:
			if route == RouteAttentionHumanDecision {
				return nil
			}
		case CompatibilityUnknown:
			if route == RouteParkUnknown {
				return nil
			}
		}
	case GoalAdjacent:
		if route == RouteDefer {
			return nil
		}
	case GoalContradictory:
		if route == RouteDecline || route == RouteDispute {
			return nil
		}
	case GoalUnclear:
		if route == RouteAttentionUnclear {
			return nil
		}
	}
	return ErrAdjudicationAxisMismatch
}

// Accepted reports whether the entry's proposal is accepted at the dispatch
// threshold. An engine entry is always accepted (it is an engine fact). A
// model-backed entry is accepted iff its confidence meets the threshold; a
// below-threshold confidence is the not-accepted case (plan §7). An invalid
// threshold is an error.
//
// Accepted is authority-bearing: a true result routes the finding to dispatch,
// so it re-runs the full entry validation and fails closed before deciding.
// Otherwise a caller-supplied or decoded entry that bypassed a constructor —
// a model-minted `allowed`, a forged engine row, an out-of-scale confidence —
// would be accepted here even though Validate rejects it upstream. After
// Validate a model-backed entry always carries a valid confidence, so the
// ordinal comparison is total.
func (e FindingAdjudicationEntry) Accepted(threshold DispatchThreshold) (bool, error) {
	if err := e.Validate(); err != nil {
		return false, err
	}
	if !threshold.valid() {
		return false, fmt.Errorf("adjudication accept threshold %q: %w", threshold, ErrInvalidDispatchThreshold)
	}
	switch e.Producer {
	case AdjudicationProducerEngine:
		return true, nil
	case AdjudicationProducerModel, AdjudicationProducerEngineModel:
		return e.Confidence.meets(threshold), nil
	}
	return false, fmt.Errorf("adjudication accept producer %q: %w", e.Producer, ErrInvalidAdjudicationProducer)
}

// NotAcceptedRepresentation returns the compatibility a not-accepted proposal
// takes: `unknown` where compatibility exists (under `required`), and nil
// otherwise — the "unknown only where compatibility exists" rule (plan §7).
func (e FindingAdjudicationEntry) NotAcceptedRepresentation() *WorkUnitCompatibility {
	if e.GoalRelationship == GoalRequired {
		unknown := CompatibilityUnknown
		return &unknown
	}
	return nil
}

// RemediationSurface is the opaque, engine-derived presumptive fix surface of
// one finding: the canonical repository-relative path its normalized location
// points at, resolved to exist on at least one side of the bound base and
// candidate trees. It is the sole input to the `allowed` declared-path
// containment check. Its fields stay private so callers cannot mint the
// authority-bearing derivation result themselves; the zero value is invalid.
type RemediationSurface struct {
	path    string
	derived bool
}

// DeriveRemediationSurface derives one finding's presumptive remediation surface
// from its location, failing closed to no surface (never a vacuously contained
// `allowed`) whenever the surface is undecidable: a nil location, a path that is
// not canonical repository-relative syntax, or a path that resolves in neither
// the base nor the candidate tree. resolve reports the path's existence on each
// side of the bound trees; only a resolve error propagates as an error.
func DeriveRemediationSurface(
	location *FindingLocation,
	resolve func(path string) (existsInBase, existsInCandidate bool, err error),
) (*RemediationSurface, error) {
	if location == nil {
		return nil, nil
	}
	if !canonicalRepoPath(location.Path) {
		return nil, nil
	}
	inBase, inCandidate, err := resolve(location.Path)
	if err != nil {
		return nil, fmt.Errorf("derive remediation surface %q: %w", location.Path, err)
	}
	if !inBase && !inCandidate {
		return nil, nil
	}
	return &RemediationSurface{path: location.Path, derived: true}, nil
}

// EngineCompatibility is the sole producer of `allowed`. It returns `allowed`
// only when the presumptive surface is contained in the work unit's declared
// paths, and the fail-closed `unknown` on a nil surface or a surface that exits
// the declared paths (which requires rule interpretation, the model residue).
//
// Containment is not decided here: match is injected so the domain package stays
// a dependency-light leaf (importing pathfold would pull golang.org/x/text into
// the contract package). Callers MUST inject the importer's own allowlist
// matcher exactly, so an `allowed` verdict never diverges from the import
// boundary's path-scope enforcement:
//
//	domain.EngineCompatibility(surface, declaredPaths,
//	    func(patterns []string, p string) bool { return pathfold.MatchAny(patterns, p, false) })
//
// The matcher uses no case fold, matching internal/importer/policy.go, because
// the allowlist names this repository's declared paths exactly.
func EngineCompatibility(
	surface *RemediationSurface,
	declaredPaths []string,
	match func(declaredPaths []string, path string) bool,
) WorkUnitCompatibility {
	if surface == nil || !surface.derived || !canonicalRepoPath(surface.path) {
		return CompatibilityUnknown
	}
	if match(declaredPaths, surface.path) {
		return CompatibilityAllowed
	}
	return CompatibilityUnknown
}

// canonicalRepoPath reports whether p is a canonical repository-relative path:
// non-empty valid UTF-8, no C0 control character or DEL (which git itself
// rejects in a tree path), no backslash, no leading/trailing/double slash, and
// no `.` or `..` segment. Containment runs only over such paths so a traversal
// or absolute path can never be vacuously contained (plan §7).
func canonicalRepoPath(p string) bool {
	if p == "" || !utf8.ValidString(p) {
		return false
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	for _, segment := range strings.Split(p, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

// FindingAdjudication is the immutable, digest-addressed artifact one review
// round with findings produces (plan §7). It binds the run, the exact finding
// batch and round, the approved specification digest, the trusted
// repository-instruction snapshot digest, and the resolved policy digest, and
// carries one adjudication entry per finding. Digest is computed by the
// constructor, never caller-supplied.
type FindingAdjudication struct {
	EncodingVersion int   `json:"encoding_version"`
	RunID           RunID `json:"run_id"`
	Round           int   `json:"round"`
	// Revision is the positive, per-round position in the immutable adjudication
	// chain. Revision 1 is encoded in the legacy byte shape for upgrade
	// compatibility; successors carry it explicitly with their provenance.
	Revision          int                   `json:"revision"`
	PredecessorDigest *Digest               `json:"predecessor_digest"`
	Feedback          *AdjudicationFeedback `json:"feedback"`
	// FindingBatchDigest is the content address of the sorted entry finding-ID
	// list, so the "exact batch" binding is itself checkable.
	FindingBatchDigest        Digest                     `json:"finding_batch_digest"`
	ApprovedSpecDigest        Digest                     `json:"approved_spec_digest"`
	InstructionSnapshotDigest Digest                     `json:"instruction_snapshot_digest"`
	ResolvedPolicyDigest      Digest                     `json:"resolved_policy_digest"`
	Entries                   []FindingAdjudicationEntry `json:"entries"`
	CreatedAt                 time.Time                  `json:"created_at"`
	Digest                    Digest                     `json:"digest"`
}

// AdjudicationFeedback binds a successor revision to the accepted agent turn
// and exact immutable conversation prefix that caused it. The store re-gates
// every copied value against the persisted invocation and conversation.
type AdjudicationFeedback struct {
	InvocationID    InvocationID   `json:"invocation_id"`
	ConversationID  ConversationID `json:"conversation_id"`
	ThroughSequence int            `json:"through_sequence"`
	PrefixDigest    Digest         `json:"prefix_digest"`
}

// Validate reports whether the feedback binding is structurally complete.
// Persistence is the authority for whether these copied values are true.
func (f AdjudicationFeedback) Validate() error {
	if f.InvocationID == "" {
		return fmt.Errorf("adjudication feedback invocation_id: %w", ErrEmptyID)
	}
	if f.ConversationID == "" {
		return fmt.Errorf("adjudication feedback conversation_id: %w", ErrEmptyID)
	}
	if f.ThroughSequence < 1 {
		return fmt.Errorf("adjudication feedback through_sequence %d: %w", f.ThroughSequence, ErrNonPositiveSeq)
	}
	if !contentaddr.Valid(string(f.PrefixDigest)) {
		return fmt.Errorf("adjudication feedback prefix_digest %q: %w", f.PrefixDigest, ErrFindingAdjudicationInconsistent)
	}
	return nil
}

type canonicalFindingAdjudication struct {
	EncodingVersion           int                        `json:"encoding_version"`
	RunID                     RunID                      `json:"run_id"`
	Round                     int                        `json:"round"`
	FindingBatchDigest        Digest                     `json:"finding_batch_digest"`
	ApprovedSpecDigest        Digest                     `json:"approved_spec_digest"`
	InstructionSnapshotDigest Digest                     `json:"instruction_snapshot_digest"`
	ResolvedPolicyDigest      Digest                     `json:"resolved_policy_digest"`
	Entries                   []FindingAdjudicationEntry `json:"entries"`
	CreatedAt                 time.Time                  `json:"created_at"`
}

type canonicalSuccessorFindingAdjudication struct {
	EncodingVersion           int                        `json:"encoding_version"`
	RunID                     RunID                      `json:"run_id"`
	Round                     int                        `json:"round"`
	Revision                  int                        `json:"revision"`
	PredecessorDigest         *Digest                    `json:"predecessor_digest"`
	Feedback                  *AdjudicationFeedback      `json:"feedback"`
	FindingBatchDigest        Digest                     `json:"finding_batch_digest"`
	ApprovedSpecDigest        Digest                     `json:"approved_spec_digest"`
	InstructionSnapshotDigest Digest                     `json:"instruction_snapshot_digest"`
	ResolvedPolicyDigest      Digest                     `json:"resolved_policy_digest"`
	Entries                   []FindingAdjudicationEntry `json:"entries"`
	CreatedAt                 time.Time                  `json:"created_at"`
}

type encodedFindingAdjudication struct {
	EncodingVersion           int                        `json:"encoding_version"`
	RunID                     RunID                      `json:"run_id"`
	Round                     int                        `json:"round"`
	FindingBatchDigest        Digest                     `json:"finding_batch_digest"`
	ApprovedSpecDigest        Digest                     `json:"approved_spec_digest"`
	InstructionSnapshotDigest Digest                     `json:"instruction_snapshot_digest"`
	ResolvedPolicyDigest      Digest                     `json:"resolved_policy_digest"`
	Entries                   []FindingAdjudicationEntry `json:"entries"`
	CreatedAt                 time.Time                  `json:"created_at"`
	Digest                    Digest                     `json:"digest"`
}

type encodedSuccessorFindingAdjudication struct {
	EncodingVersion           int                        `json:"encoding_version"`
	RunID                     RunID                      `json:"run_id"`
	Round                     int                        `json:"round"`
	Revision                  int                        `json:"revision"`
	PredecessorDigest         *Digest                    `json:"predecessor_digest"`
	Feedback                  *AdjudicationFeedback      `json:"feedback"`
	FindingBatchDigest        Digest                     `json:"finding_batch_digest"`
	ApprovedSpecDigest        Digest                     `json:"approved_spec_digest"`
	InstructionSnapshotDigest Digest                     `json:"instruction_snapshot_digest"`
	ResolvedPolicyDigest      Digest                     `json:"resolved_policy_digest"`
	Entries                   []FindingAdjudicationEntry `json:"entries"`
	CreatedAt                 time.Time                  `json:"created_at"`
	Digest                    Digest                     `json:"digest"`
}

// NewFindingAdjudication builds one round's adjudication artifact. It sorts the
// entries by finding id, computes the finding-batch and content digests, and
// validates. createdAt must be a UTC instant.
func NewFindingAdjudication(
	runID RunID,
	round int,
	approvedSpecDigest, instructionSnapshotDigest, resolvedPolicyDigest Digest,
	entries []FindingAdjudicationEntry,
	createdAt time.Time,
) (FindingAdjudication, error) {
	sorted := slices.Clone(entries)
	slices.SortFunc(sorted, func(a, b FindingAdjudicationEntry) int {
		return strings.Compare(string(a.FindingID), string(b.FindingID))
	})
	batchDigest, err := computeFindingBatchDigest(sorted)
	if err != nil {
		return FindingAdjudication{}, err
	}
	artifact := FindingAdjudication{
		EncodingVersion:           FindingAdjudicationEncodingVersion,
		RunID:                     runID,
		Round:                     round,
		Revision:                  1,
		FindingBatchDigest:        batchDigest,
		ApprovedSpecDigest:        approvedSpecDigest,
		InstructionSnapshotDigest: instructionSnapshotDigest,
		ResolvedPolicyDigest:      resolvedPolicyDigest,
		Entries:                   sorted,
		CreatedAt:                 createdAt,
	}
	digest, err := artifact.ComputeDigest()
	if err != nil {
		return FindingAdjudication{}, err
	}
	artifact.Digest = digest
	if err := artifact.Validate(); err != nil {
		return FindingAdjudication{}, err
	}
	return artifact, nil
}

// NewSuccessorFindingAdjudication builds the next immutable adjudication for a
// review round after Discuss feedback. It preserves every version binding and
// requires the exact finding set of its predecessor.
func NewSuccessorFindingAdjudication(
	prior FindingAdjudication,
	feedback AdjudicationFeedback,
	entries []FindingAdjudicationEntry,
	createdAt time.Time,
) (FindingAdjudication, error) {
	if err := prior.Validate(); err != nil {
		return FindingAdjudication{}, fmt.Errorf("finding adjudication predecessor: %w", err)
	}
	if prior.Revision == math.MaxInt {
		return FindingAdjudication{}, fmt.Errorf("finding adjudication revision overflow: %w", ErrFindingAdjudicationInconsistent)
	}
	if err := feedback.Validate(); err != nil {
		return FindingAdjudication{}, err
	}
	sorted := slices.Clone(entries)
	slices.SortFunc(sorted, func(a, b FindingAdjudicationEntry) int {
		return strings.Compare(string(a.FindingID), string(b.FindingID))
	})
	batchDigest, err := computeFindingBatchDigest(sorted)
	if err != nil {
		return FindingAdjudication{}, err
	}
	if batchDigest != prior.FindingBatchDigest {
		return FindingAdjudication{}, fmt.Errorf("finding adjudication successor batch %q, predecessor has %q: %w",
			batchDigest, prior.FindingBatchDigest, ErrFindingAdjudicationInconsistent)
	}
	predecessor := prior.Digest
	artifact := FindingAdjudication{
		EncodingVersion:           FindingAdjudicationEncodingVersion,
		RunID:                     prior.RunID,
		Round:                     prior.Round,
		Revision:                  prior.Revision + 1,
		PredecessorDigest:         &predecessor,
		Feedback:                  &feedback,
		FindingBatchDigest:        batchDigest,
		ApprovedSpecDigest:        prior.ApprovedSpecDigest,
		InstructionSnapshotDigest: prior.InstructionSnapshotDigest,
		ResolvedPolicyDigest:      prior.ResolvedPolicyDigest,
		Entries:                   sorted,
		CreatedAt:                 createdAt,
	}
	digest, err := artifact.ComputeDigest()
	if err != nil {
		return FindingAdjudication{}, err
	}
	artifact.Digest = digest
	if err := artifact.Validate(); err != nil {
		return FindingAdjudication{}, err
	}
	return artifact, nil
}

// computeFindingBatchDigest is the content address of the sorted finding-ID
// list. Entry order is the caller's responsibility (Validate enforces sorted,
// unique entries); this hashes the ids in the order given.
func computeFindingBatchDigest(entries []FindingAdjudicationEntry) (Digest, error) {
	ids := make([]FindingID, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.FindingID)
	}
	body, err := json.Marshal(ids)
	if err != nil {
		return "", fmt.Errorf("finding batch digest: %w", err)
	}
	return Digest(contentaddr.Sum(body)), nil
}

// Validate is the structural and content-address backstop for a reconstructed
// artifact. Entries must be non-empty, each valid, and strictly ascending by
// finding id (sorted and unique). The finding-batch and content digests must
// match their canonical content.
func (a FindingAdjudication) Validate() error {
	if a.EncodingVersion != FindingAdjudicationEncodingVersion {
		return fmt.Errorf("finding adjudication encoding_version %d: %w", a.EncodingVersion, ErrFindingAdjudicationInconsistent)
	}
	if a.RunID == "" {
		return fmt.Errorf("finding adjudication run_id: %w", ErrEmptyID)
	}
	if a.Round < 1 {
		return fmt.Errorf("finding adjudication round %d: %w", a.Round, ErrNonPositive)
	}
	if a.Revision < 1 {
		return fmt.Errorf("finding adjudication revision %d: %w", a.Revision, ErrFindingAdjudicationInconsistent)
	}
	if a.Revision == 1 {
		if a.PredecessorDigest != nil || a.Feedback != nil {
			return fmt.Errorf("finding adjudication initial revision provenance: %w", ErrFindingAdjudicationInconsistent)
		}
	} else {
		if a.PredecessorDigest == nil || !contentaddr.Valid(string(*a.PredecessorDigest)) || a.Feedback == nil {
			return fmt.Errorf("finding adjudication successor revision provenance: %w", ErrFindingAdjudicationInconsistent)
		}
		if err := a.Feedback.Validate(); err != nil {
			return err
		}
	}
	for label, digest := range map[string]Digest{
		"approved_spec_digest":        a.ApprovedSpecDigest,
		"instruction_snapshot_digest": a.InstructionSnapshotDigest,
		"resolved_policy_digest":      a.ResolvedPolicyDigest,
	} {
		if !contentaddr.Valid(string(digest)) {
			return fmt.Errorf("finding adjudication %s %q: %w", label, digest, ErrFindingAdjudicationInconsistent)
		}
	}
	if len(a.Entries) == 0 {
		return fmt.Errorf("finding adjudication entries: %w", ErrFindingAdjudicationInconsistent)
	}
	for i, entry := range a.Entries {
		if err := entry.Validate(); err != nil {
			return err
		}
		if i > 0 && entry.FindingID <= a.Entries[i-1].FindingID {
			return fmt.Errorf("finding adjudication entry %q order: %w", entry.FindingID, ErrFindingsNotCanonical)
		}
	}
	computedBatch, err := computeFindingBatchDigest(a.Entries)
	if err != nil {
		return err
	}
	if a.FindingBatchDigest != computedBatch {
		return fmt.Errorf("finding adjudication batch digest %q, content resolves to %q: %w",
			a.FindingBatchDigest, computedBatch, ErrFindingAdjudicationInconsistent)
	}
	if a.CreatedAt.IsZero() {
		return fmt.Errorf("finding adjudication created_at: %w", ErrMissingTimestamp)
	}
	if a.CreatedAt.Location() != time.UTC {
		return fmt.Errorf("finding adjudication created_at: %w", ErrTimestampNotUTC)
	}
	if !contentaddr.Valid(string(a.Digest)) {
		return fmt.Errorf("finding adjudication digest %q: %w", a.Digest, ErrFindingAdjudicationDigestMismatch)
	}
	computed, err := a.ComputeDigest()
	if err != nil {
		return err
	}
	if a.Digest != computed {
		return fmt.Errorf("finding adjudication digest %q, content resolves to %q: %w",
			a.Digest, computed, ErrFindingAdjudicationDigestMismatch)
	}
	return nil
}

// AuthorizesFinalDisposition reports whether this artifact's typed axes admit
// the final declined or deferred disposition for findingID. It revalidates the
// artifact before consulting an entry so a caller-supplied or decoded value
// cannot grant authority without passing the content-addressed contract.
func (a FindingAdjudication) AuthorizesFinalDisposition(
	findingID FindingID, disposition ReviewDisposition,
) error {
	if err := a.Validate(); err != nil {
		return err
	}
	var route AdjudicationRoute
	switch disposition {
	case ReviewDispositionDeclined:
		route = RouteDecline
	case ReviewDispositionDeferred:
		route = RouteDefer
	case ReviewDispositionFixed:
		return fmt.Errorf("finding %q disposition %q: %w",
			findingID, disposition, ErrInvalidDispositionAdjudication)
	}
	for _, entry := range a.Entries {
		if entry.FindingID != findingID {
			continue
		}
		if err := validAdjudicationRow(entry.GoalRelationship, entry.Compatibility, route); err != nil {
			return fmt.Errorf("finding %q disposition %q: %w",
				findingID, disposition, ErrInvalidDispositionAdjudication)
		}
		return nil
	}
	return fmt.Errorf("finding %q absent from adjudication artifact: %w",
		findingID, ErrInvalidDispositionAdjudication)
}

func (a FindingAdjudication) canonical() any {
	initial := canonicalFindingAdjudication{
		EncodingVersion:           a.EncodingVersion,
		RunID:                     a.RunID,
		Round:                     a.Round,
		FindingBatchDigest:        a.FindingBatchDigest,
		ApprovedSpecDigest:        a.ApprovedSpecDigest,
		InstructionSnapshotDigest: a.InstructionSnapshotDigest,
		ResolvedPolicyDigest:      a.ResolvedPolicyDigest,
		Entries:                   a.Entries,
		CreatedAt:                 a.CreatedAt,
	}
	if a.Revision == 1 && a.PredecessorDigest == nil && a.Feedback == nil {
		return initial
	}
	return canonicalSuccessorFindingAdjudication{
		EncodingVersion:           a.EncodingVersion,
		RunID:                     a.RunID,
		Round:                     a.Round,
		Revision:                  a.Revision,
		PredecessorDigest:         a.PredecessorDigest,
		Feedback:                  a.Feedback,
		FindingBatchDigest:        a.FindingBatchDigest,
		ApprovedSpecDigest:        a.ApprovedSpecDigest,
		InstructionSnapshotDigest: a.InstructionSnapshotDigest,
		ResolvedPolicyDigest:      a.ResolvedPolicyDigest,
		Entries:                   a.Entries,
		CreatedAt:                 a.CreatedAt,
	}
}

// MarshalJSON preserves the pre-revision byte shape for revision 1. Successors
// carry revision identity and feedback provenance explicitly.
func (a FindingAdjudication) MarshalJSON() ([]byte, error) {
	if a.Revision == 1 && a.PredecessorDigest == nil && a.Feedback == nil {
		return json.Marshal(encodedFindingAdjudication{
			EncodingVersion: a.EncodingVersion, RunID: a.RunID, Round: a.Round,
			FindingBatchDigest: a.FindingBatchDigest, ApprovedSpecDigest: a.ApprovedSpecDigest,
			InstructionSnapshotDigest: a.InstructionSnapshotDigest, ResolvedPolicyDigest: a.ResolvedPolicyDigest,
			Entries: a.Entries, CreatedAt: a.CreatedAt, Digest: a.Digest,
		})
	}
	return json.Marshal(encodedSuccessorFindingAdjudication(a))
}

// UnmarshalJSON accepts the legacy revision-1 encoding only when all revision
// fields are absent. Explicit zero or null revision fields remain malformed and
// fail Validate rather than being silently upgraded.
func (a *FindingAdjudication) UnmarshalJSON(body []byte) error {
	type wireFindingAdjudication struct {
		EncodingVersion           int                        `json:"encoding_version"`
		RunID                     RunID                      `json:"run_id"`
		Round                     int                        `json:"round"`
		Revision                  json.RawMessage            `json:"revision"`
		PredecessorDigest         json.RawMessage            `json:"predecessor_digest"`
		Feedback                  json.RawMessage            `json:"feedback"`
		FindingBatchDigest        Digest                     `json:"finding_batch_digest"`
		ApprovedSpecDigest        Digest                     `json:"approved_spec_digest"`
		InstructionSnapshotDigest Digest                     `json:"instruction_snapshot_digest"`
		ResolvedPolicyDigest      Digest                     `json:"resolved_policy_digest"`
		Entries                   []FindingAdjudicationEntry `json:"entries"`
		CreatedAt                 time.Time                  `json:"created_at"`
		Digest                    Digest                     `json:"digest"`
	}
	var wire wireFindingAdjudication
	if err := strictjson.Decode(body, &wire, strictjson.RejectInvalidUTF8, strictjson.NoLimit); err != nil {
		return err
	}
	*a = FindingAdjudication{
		EncodingVersion: wire.EncodingVersion, RunID: wire.RunID, Round: wire.Round,
		FindingBatchDigest: wire.FindingBatchDigest, ApprovedSpecDigest: wire.ApprovedSpecDigest,
		InstructionSnapshotDigest: wire.InstructionSnapshotDigest, ResolvedPolicyDigest: wire.ResolvedPolicyDigest,
		Entries: wire.Entries, CreatedAt: wire.CreatedAt, Digest: wire.Digest,
	}
	if len(wire.Revision) == 0 && len(wire.PredecessorDigest) == 0 && len(wire.Feedback) == 0 {
		a.Revision = 1
		return nil
	}
	if len(wire.Revision) > 0 {
		if err := json.Unmarshal(wire.Revision, &a.Revision); err != nil {
			return fmt.Errorf("finding adjudication revision: %w", err)
		}
	}
	if len(wire.PredecessorDigest) > 0 {
		if err := json.Unmarshal(wire.PredecessorDigest, &a.PredecessorDigest); err != nil {
			return fmt.Errorf("finding adjudication predecessor_digest: %w", err)
		}
	}
	if len(wire.Feedback) > 0 {
		var feedback AdjudicationFeedback
		if err := strictjson.Decode(wire.Feedback, &feedback,
			strictjson.RejectInvalidUTF8, strictjson.NoLimit); err != nil {
			return fmt.Errorf("finding adjudication feedback: %w", err)
		}
		a.Feedback = &feedback
	}
	return nil
}

// ComputeDigest hashes the explicit-version canonical encoding, which excludes
// only the Digest field. Struct field order is part of the contract and is
// pinned by a golden.
func (a FindingAdjudication) ComputeDigest() (Digest, error) {
	body, err := json.Marshal(a.canonical())
	if err != nil {
		return "", fmt.Errorf("finding adjudication canonical encoding: %w", err)
	}
	return Digest(contentaddr.Sum(body)), nil
}

// Encode emits the validated canonical persisted form.
func (a FindingAdjudication) Encode() ([]byte, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(a)
	if err != nil {
		return nil, fmt.Errorf("finding adjudication encode: %w", err)
	}
	return body, nil
}

// DecodeFindingAdjudication rejects oversized, unknown-field, invalid-UTF8, and
// trailing-data payloads before revalidating the content address.
func DecodeFindingAdjudication(body []byte) (FindingAdjudication, error) {
	var artifact FindingAdjudication
	if err := strictjson.Decode(body, &artifact, strictjson.RejectInvalidUTF8, MaxFindingAdjudicationBytes); err != nil {
		return FindingAdjudication{}, fmt.Errorf("finding adjudication decode: %w", err)
	}
	if err := artifact.Validate(); err != nil {
		return FindingAdjudication{}, err
	}
	return artifact, nil
}
