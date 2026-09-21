package inference

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"time"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
)

// The publication-author role (plan §5.13, §5.15) runs as two judgment sites
// under one refinable role prompt: explain writes the PR prose, propose emits a
// bounded source-issue-closure recommendation. Both are advisory: their output
// never feeds trust computation or publish_eligible, and neither can be
// interpolated into a Closes/Refs line, a CI-skip marker, or a commit trailer.
// The reference-line writing and the closure proposal itself belong to the
// publish unit (#1419), which resolves these sites' inputs and stores the
// authoring artifact (domain.PublicationAuthoring).
const (
	PublicationAuthorExplainSiteID = "publication_author_explain"
	PublicationAuthorProposeSiteID = "publication_author_propose"
)

// Per-field input budgets. The diff and issue body are truncated rather than
// refused so a large change still gets authored text; a control file is never
// truncated, because half a rule set would mislead the model, so an oversized
// one fails the call to its deterministic fallback instead.
const (
	maxAuthorDiffBytes        = 256 << 10
	maxAuthorIssueBodyBytes   = 64 << 10
	maxAuthorControlFileBytes = 64 << 10

	diffTruncationMarker      = "\n\n[content truncated to fit the model input budget]"
	issueBodyTruncationMarker = "\n\n[content truncated to fit the model input budget]"
)

// RepositoryVisibility is a repository's public/private visibility as the
// caller resolves it (#1419 binds it to real repository state). It maps to the
// domain sensitivity class the plan derives from visibility. The zero value is
// invalid by design.
type RepositoryVisibility string

const (
	RepositoryPublic  RepositoryVisibility = "public"
	RepositoryPrivate RepositoryVisibility = "private"
)

// AllRepositoryVisibilities is the single registration point for the enum.
var AllRepositoryVisibilities = []RepositoryVisibility{RepositoryPublic, RepositoryPrivate}

func (v RepositoryVisibility) valid() bool {
	switch v {
	case RepositoryPublic, RepositoryPrivate:
		return true
	default:
		return false
	}
}

// sensitivityClass maps visibility to the derived domain class: public is
// normal, private is sensitive. The switch dispatches over the mapping and so
// omits default; the trailing empty (invalid) class fails closed for the zero
// value, which build() rejects before this is reached.
func (v RepositoryVisibility) sensitivityClass() domain.SensitivityClass {
	switch v {
	case RepositoryPublic:
		return domain.SensitivityNormal
	case RepositoryPrivate:
		return domain.SensitivitySensitive
	}
	return ""
}

// ControlFile is a digest-bound control-plane input resolved from the trusted
// base, never the candidate head: the PR template or the composed instruction
// snapshot. A candidate edit to either cannot change the resolved input,
// because the caller supplies the trusted-base content and its digest and the
// input check recomputes the digest.
type ControlFile struct {
	Content           string
	Digest            string
	TrustedBaseCommit string
}

// checked returns the control file's content only when it is digest-consistent,
// carries a trusted-base commit, is valid UTF-8, and fits the input budget. Any
// failure aborts the call to its fail-safe: the type has no field that could
// carry a candidate-head copy, so a rejected control file cannot be smuggled in.
func (cf ControlFile) checked() (string, error) {
	if cf.TrustedBaseCommit == "" {
		return "", errors.New("control file has no trusted-base commit")
	}
	if len(cf.Content) > maxAuthorControlFileBytes {
		return "", errors.New("control file exceeds the input budget")
	}
	if !utf8.ValidString(cf.Content) {
		return "", errors.New("control file is not valid UTF-8")
	}
	if cf.Digest != contentaddr.Sum([]byte(cf.Content)) {
		return "", errors.New("control file digest does not match its content")
	}
	return cf.Content, nil
}

// PublicationAuthorInput carries the fields both sites run over. It has no field
// for a private summary, a credential, or a candidate-head copy of a control
// file. The caller (#1419) resolves the values; this type defines the inputs
// and enforces their rules where the call is made.
type PublicationAuthorInput struct {
	Project     string
	RootLineage string

	TargetRepository string
	TargetVisibility RepositoryVisibility

	SourceIssueRef   string
	SourceIssueTitle string
	SourceIssueBody  string
	SourceVisibility RepositoryVisibility

	Diff                string
	VerificationOutcome string
	ReviewOutcome       string

	PRTemplate          ControlFile
	InstructionSnapshot ControlFile

	// Evidence is the candidate's artifacts; build() keeps only current,
	// policy-approved, publish-eligible artifacts no more restrictive than the
	// target class. ApprovedRecipes is the current approved-recipe set the
	// publish-eligibility gate re-runs against, never a trusted stored bit.
	Evidence        []domain.Artifact
	ApprovedRecipes map[domain.Digest]bool
}

// builtRequest is the resolved outbound payload plus the classes and kept
// evidence the client methods return so #1419 can label and resolve what it
// stores.
type builtRequest struct {
	fields       map[string]InputField
	targetClass  domain.SensitivityClass
	inputClasses map[string]domain.SensitivityClass
	evidence     []domain.Artifact
}

// build resolves the input into the outbound field allowlist, the target class,
// the per-content-input classes, and the kept evidence. It returns an error
// (which both methods turn into their fail-safe) when a visibility is invalid,
// a control file is not digest-consistent or lacks a trusted-base commit, or a
// control file exceeds its budget.
func (in PublicationAuthorInput) build() (builtRequest, error) {
	if !in.TargetVisibility.valid() || !in.SourceVisibility.valid() {
		return builtRequest{}, errors.New("invalid repository visibility")
	}
	targetClass := in.TargetVisibility.sensitivityClass()
	sourceClass := in.SourceVisibility.sensitivityClass()
	template, err := in.PRTemplate.checked()
	if err != nil {
		return builtRequest{}, err
	}
	snapshot, err := in.InstructionSnapshot.checked()
	if err != nil {
		return builtRequest{}, err
	}
	// Cross-visibility: a source issue more restrictive than the target
	// contributes only its reference. Its title and body never cross into a
	// less-restrictive publication; the reference is kept so the author can
	// still mention the issue by number.
	issueTitle, issueBody := in.SourceIssueTitle, in.SourceIssueBody
	if sourceClass.MoreRestrictiveThan(targetClass) {
		issueTitle, issueBody = "", ""
	}
	kept := keepPublishableEvidence(in.Evidence, in.ApprovedRecipes, targetClass)
	refs, err := encodeEvidenceReferences(kept)
	if err != nil {
		return builtRequest{}, err
	}
	fields := map[string]InputField{
		"target_repository":    {Value: in.TargetRepository, Sensitivity: SensitivityOperational},
		"target_visibility":    {Value: string(in.TargetVisibility), Sensitivity: SensitivityOperational},
		"source_issue_ref":     {Value: in.SourceIssueRef, Sensitivity: SensitivityOperational},
		"source_visibility":    {Value: string(in.SourceVisibility), Sensitivity: SensitivityOperational},
		"verification_outcome": {Value: in.VerificationOutcome, Sensitivity: SensitivityOperational},
		"review_outcome":       {Value: in.ReviewOutcome, Sensitivity: SensitivityOperational},
		"source_issue_title":   {Value: issueTitle, Sensitivity: SensitivityRepository},
		"source_issue_body":    {Value: truncateUTF8(issueBody, maxAuthorIssueBodyBytes, issueBodyTruncationMarker), Sensitivity: SensitivityRepository},
		"diff":                 {Value: truncateUTF8(in.Diff, maxAuthorDiffBytes, diffTruncationMarker), Sensitivity: SensitivityRepository},
		"pr_template":          {Value: template, Sensitivity: SensitivityRepository},
		"instruction_snapshot": {Value: snapshot, Sensitivity: SensitivityRepository},
		"evidence_refs":        {Value: refs, Sensitivity: SensitivityRepository},
	}
	// The per-input classes cover the repository-content inputs #1419 stores:
	// the diff and control files take the target repository's tier, the source
	// issue its own repository's tier. The identifier fields are operational
	// (repository names, outcomes) and carry no stored content class.
	inputClasses := map[string]domain.SensitivityClass{
		"diff":                 targetClass,
		"pr_template":          targetClass,
		"instruction_snapshot": targetClass,
		"source_issue_title":   sourceClass,
		"source_issue_body":    sourceClass,
	}
	return builtRequest{fields: fields, targetClass: targetClass, inputClasses: inputClasses, evidence: kept}, nil
}

// keepPublishableEvidence applies the same trust rule as the store's
// gatePublicationAuthoring: an artifact is kept only when its persisted
// publish_eligible bit agrees with policy over the current approved-recipe set,
// it is publish-eligible, and its class is no more restrictive than the target.
// So whatever the author may cite, the store will accept, and sensitive or
// non-publishable evidence is never an input.
func keepPublishableEvidence(
	evidence []domain.Artifact, approved map[domain.Digest]bool, target domain.SensitivityClass,
) []domain.Artifact {
	kept := make([]domain.Artifact, 0, len(evidence))
	for _, a := range evidence {
		if domain.ValidatePublishEligibility(a, approved) != nil {
			continue
		}
		if !a.PublishEligible {
			continue
		}
		if a.Provenance.SensitivityClass.MoreRestrictiveThan(target) {
			continue
		}
		kept = append(kept, a)
	}
	return kept
}

// encodeEvidenceReferences renders the kept evidence as references the model
// may cite: id and kind only, never the artifact body.
func encodeEvidenceReferences(evidence []domain.Artifact) (string, error) {
	type reference struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
	}
	refs := make([]reference, 0, len(evidence))
	for _, a := range evidence {
		refs = append(refs, reference{ID: string(a.ID), Kind: string(a.Type)})
	}
	body, err := json.Marshal(refs)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// truncateUTF8 caps s at max bytes on a valid UTF-8 boundary and appends a
// visible marker; a value already within the budget is returned unchanged.
// Truncating on a rune boundary keeps the field valid UTF-8, which Client.Call
// requires.
func truncateUTF8(s string, max int, marker string) string {
	if len(s) <= max {
		return s
	}
	b := s[:max]
	for len(b) > 0 && !utf8.ValidString(b) {
		b = b[:len(b)-1]
	}
	return b + marker
}

// publicationAuthorFields is the fixed outbound allowlist both sites declare.
// Client.Call requires an exact per-field sensitivity match on every call, so
// the tiers cannot vary per call: repository content is SensitivityRepository,
// identifiers are SensitivityOperational. The derived domain.SensitivityClass
// values travel beside the fields in the call result, not on this policy.
func publicationAuthorFields() []FieldPolicy {
	return []FieldPolicy{
		{Name: "target_repository", Sensitivity: SensitivityOperational},
		{Name: "target_visibility", Sensitivity: SensitivityOperational},
		{Name: "source_issue_ref", Sensitivity: SensitivityOperational},
		{Name: "source_visibility", Sensitivity: SensitivityOperational},
		{Name: "verification_outcome", Sensitivity: SensitivityOperational},
		{Name: "review_outcome", Sensitivity: SensitivityOperational},
		{Name: "source_issue_title", Sensitivity: SensitivityRepository},
		{Name: "source_issue_body", Sensitivity: SensitivityRepository},
		{Name: "diff", Sensitivity: SensitivityRepository},
		{Name: "pr_template", Sensitivity: SensitivityRepository},
		{Name: "instruction_snapshot", Sensitivity: SensitivityRepository},
		{Name: "evidence_refs", Sensitivity: SensitivityRepository},
	}
}

type publicationAuthorExplainOutput struct {
	Title          string   `json:"title"`
	Body           string   `json:"body"`
	ReviewerNotes  *string  `json:"reviewer_notes"`
	EvidenceRefs   []string `json:"evidence_refs"`
	OutcomeSummary string   `json:"outcome_summary"`
}

type publicationAuthorProposeOutput struct {
	// Resolves is a pointer so a missing field is rejected: the answer must be
	// exactly {"resolves":true} or {"resolves":false}.
	Resolves *bool `json:"resolves"`
}

// PublicationAuthorExplainSite writes the schema-valid, producer-labeled PR
// prose. Its answer fits domain.PublicationAuthoring's five fields and bounds,
// so every accepted answer can be stored. The 64 KiB output cap matches the
// advisory store's sampled-audit body limit; the 120 second timeout matches the
// per-root allowance sized on the adjudicator.
func PublicationAuthorExplainSite(budget Budget) Site {
	return Site{
		ID: PublicationAuthorExplainSiteID, Authority: AuthorityExplain,
		Fields:    publicationAuthorFields(),
		FailSafe:  `{"title":"","body":"","reviewer_notes":null,"evidence_refs":[],"outcome_summary":""}`,
		Retention: 30 * 24 * time.Hour, Timeout: 120 * time.Second,
		MaxInputBytes: 1 << 20, MaxOutputBytes: 64 << 10, MaxComputeUnits: 10_000,
		Budget: budget, AuditEvery: 10,
		ValidateOutput: validatePublicationAuthorExplain,
	}
}

// PublicationAuthorProposeSite emits the bounded source-issue-closure
// recommendation as a single boolean, so its answer carries no text that could
// reach a Closes line.
func PublicationAuthorProposeSite(budget Budget) Site {
	return Site{
		ID: PublicationAuthorProposeSiteID, Authority: AuthorityPropose,
		Fields:    publicationAuthorFields(),
		FailSafe:  `{"resolves":false}`,
		Retention: 30 * 24 * time.Hour, Timeout: 60 * time.Second,
		MaxInputBytes: 1 << 20, MaxOutputBytes: 1 << 10, MaxComputeUnits: 10_000,
		Budget: budget, AuditEvery: 10,
		ValidateOutput: validatePublicationAuthorPropose,
	}
}

func validatePublicationAuthorExplain(data []byte) error {
	var output publicationAuthorExplainOutput
	if err := decodeStrictObject(data, &output, 64<<10); err != nil {
		return err
	}
	for _, f := range []struct {
		value string
		max   int
	}{
		{output.Title, domain.MaxPublicationAuthoringTitleBytes},
		{output.Body, domain.MaxPublicationAuthoringBodyBytes},
		{output.OutcomeSummary, domain.MaxPublicationAuthoringOutcomeSummaryBytes},
	} {
		if f.value == "" {
			return errors.New("empty publication author text field")
		}
		if len(f.value) > f.max {
			return errors.New("publication author text field exceeds its bound")
		}
		if err := screenAuthorText(f.value, f.max); err != nil {
			return err
		}
	}
	// reviewer_notes is optional; an empty string is treated as absent by the
	// client method. A present, non-empty value is bounded and screened.
	if output.ReviewerNotes != nil && *output.ReviewerNotes != "" {
		if len(*output.ReviewerNotes) > domain.MaxPublicationAuthoringReviewerNotesBytes {
			return errors.New("publication author reviewer_notes exceeds its bound")
		}
		if err := screenAuthorText(*output.ReviewerNotes, domain.MaxPublicationAuthoringReviewerNotesBytes); err != nil {
			return err
		}
	}
	return validateEvidenceRefIDs(output.EvidenceRefs)
}

func validatePublicationAuthorPropose(data []byte) error {
	var output publicationAuthorProposeOutput
	if err := decodeStrictObject(data, &output, 1<<10); err != nil {
		return err
	}
	if output.Resolves == nil {
		return errors.New("propose output is missing resolves")
	}
	return nil
}

// validateEvidenceRefIDs enforces the domain bound, non-emptiness, and
// duplicate-freedom of the cited ids. The client method separately rejects an
// id the call did not supply.
func validateEvidenceRefIDs(ids []string) error {
	if len(ids) > domain.MaxPublicationEvidenceRefs {
		return errors.New("too many evidence references")
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" {
			return errors.New("empty evidence reference id")
		}
		if seen[id] {
			return errors.New("duplicate evidence reference id")
		}
		seen[id] = true
	}
	return nil
}

// screenAuthorText rejects close keywords, CI-skip markers, spoofable trailers,
// control characters (every one except line feed), and secrets, re-screening
// after each HTML unescape so a directive hidden behind an entity cannot slip
// through. It mirrors the engine's screenPublicationText loop without importing
// engine (which imports inference).
func screenAuthorText(text string, maxBytes int) error {
	for {
		if importer.ScreenMessage(text, importer.Policy{
			MaxCommitMessageBytes: maxBytes, MessageRuleset: domain.MessageRulesetGitHub1,
		}) != nil || importer.ContainsSecret([]byte(text)) {
			return errors.New("publication author text carries unsafe content or an automation directive")
		}
		decoded := html.UnescapeString(text)
		if decoded == text {
			return nil
		}
		text = decoded
	}
}

// AuthoredPublication is the validated, producer-labeled explain result. #1419
// builds and stores domain.PublicationAuthoring from it; this unit writes no
// advisory-store entry for the prose.
type AuthoredPublication struct {
	Title          string
	Body           string
	ReviewerNotes  *string
	OutcomeSummary string
	EvidenceRefs   []domain.PublicationEvidenceReference
	Producer       string
	InputDigest    string
	TargetClass    domain.SensitivityClass
	InputClasses   map[string]domain.SensitivityClass
	Fallback       bool
}

// ProposedClosure is the propose site's bounded recommendation. It fails safe
// to "no close" independently of the explain site.
type ProposedClosure struct {
	Resolves    bool
	Producer    string
	InputDigest string
	Fallback    bool
}

// AuthorPublication runs the explain site and returns the validated prose plus
// each cited reference resolved to the supplied artifact's digest. It falls
// back (never blocking publication) when the input cannot be built, inference
// is unavailable, or the answer cites an id the call did not supply. An error
// that arrives with a fallback result (an audit-store failure) is treated as a
// fallback, like DiscussAttentionItem.
func (c *Client) AuthorPublication(ctx context.Context, input PublicationAuthorInput) (AuthoredPublication, error) {
	built, err := input.build()
	if err != nil {
		return AuthoredPublication{Fallback: true}, nil
	}
	result, callErr := c.Call(ctx, PublicationAuthorExplainSiteID, input.Project, input.RootLineage, built.fields)
	if callErr != nil && !result.Fallback {
		return AuthoredPublication{}, callErr
	}
	if result.Fallback {
		return AuthoredPublication{Fallback: true}, nil
	}
	var output publicationAuthorExplainOutput
	if err := json.Unmarshal(result.Output, &output); err != nil {
		return AuthoredPublication{}, fmt.Errorf("decode validated publication author output: %w", err)
	}
	refs, ok := resolveEvidenceReferences(output.EvidenceRefs, built.evidence)
	if !ok {
		return AuthoredPublication{Fallback: true}, nil
	}
	notes := output.ReviewerNotes
	if notes != nil && *notes == "" {
		notes = nil
	}
	return AuthoredPublication{
		Title: output.Title, Body: output.Body, ReviewerNotes: notes,
		OutcomeSummary: output.OutcomeSummary, EvidenceRefs: refs,
		Producer: result.Producer, InputDigest: result.InputDigest,
		TargetClass: built.targetClass, InputClasses: built.inputClasses,
	}, nil
}

// ProposeSourceIssueClosure runs the propose site and returns its boolean
// recommendation. It shares the input builder but fails on its own: an explain
// failure does not change its result, and its own failure returns resolves
// false.
func (c *Client) ProposeSourceIssueClosure(ctx context.Context, input PublicationAuthorInput) (ProposedClosure, error) {
	built, err := input.build()
	if err != nil {
		return ProposedClosure{Fallback: true}, nil
	}
	result, callErr := c.Call(ctx, PublicationAuthorProposeSiteID, input.Project, input.RootLineage, built.fields)
	if callErr != nil && !result.Fallback {
		return ProposedClosure{}, callErr
	}
	if result.Fallback {
		return ProposedClosure{Fallback: true}, nil
	}
	var output publicationAuthorProposeOutput
	if err := json.Unmarshal(result.Output, &output); err != nil {
		return ProposedClosure{}, fmt.Errorf("decode validated propose output: %w", err)
	}
	return ProposedClosure{
		Resolves:    output.Resolves != nil && *output.Resolves,
		Producer:    result.Producer,
		InputDigest: result.InputDigest,
	}, nil
}

// resolveEvidenceReferences maps each id the model cited to the supplied
// artifact's digest. It reports false when the answer names an id the call did
// not supply, which the caller treats as a fallback. Duplicate ids are already
// rejected by the site validator.
func resolveEvidenceReferences(
	ids []string, evidence []domain.Artifact,
) ([]domain.PublicationEvidenceReference, bool) {
	byID := make(map[domain.ArtifactID]domain.Digest, len(evidence))
	for _, a := range evidence {
		byID[a.ID] = a.Digest
	}
	refs := make([]domain.PublicationEvidenceReference, 0, len(ids))
	for _, id := range ids {
		digest, ok := byID[domain.ArtifactID(id)]
		if !ok {
			return nil, false
		}
		refs = append(refs, domain.PublicationEvidenceReference{ArtifactID: domain.ArtifactID(id), Digest: digest})
	}
	return refs, true
}
