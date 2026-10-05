package domain

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

const (
	// PolicyFollowUpFilingLabels and PolicyFollowUpFilingMilestone are the
	// resolved-policy keys that say what a filed follow-up issue carries
	// (plan §5.17). Labels is a comma-separated list. A missing key means no
	// labels or no milestone.
	PolicyFollowUpFilingLabels    = "follow_up_filing.labels"
	PolicyFollowUpFilingMilestone = "follow_up_filing.milestone"

	// MaxFollowUpFilingTitleBytes bounds the one-line issue title.
	MaxFollowUpFilingTitleBytes = 256
	// MaxFollowUpFilingBodyBytes bounds the issue body. It sits below the
	// candidate-body budget the issue-text screen enforces
	// (publicationtext.MaxCandidateBodyBytes), so a body this type admits is
	// never refused by the screen for size alone.
	MaxFollowUpFilingBodyBytes = 16 << 10
	// MaxFollowUpFilingLabels and MaxFollowUpFilingLabelBytes bound the label
	// set; the per-label bound is the forge's own label-name limit.
	MaxFollowUpFilingLabels     = 32
	MaxFollowUpFilingLabelBytes = 50
	// MaxFollowUpFilingMilestoneBytes bounds the milestone title.
	MaxFollowUpFilingMilestoneBytes = 255
)

// FollowUpFilingRepository names the repository a follow-up issue is filed
// in. RepositoryID is the forge's numeric identity and alone decides equality;
// Repo is the name current when the proposal was made and may predate a rename
// (#1537).
type FollowUpFilingRepository struct {
	Repo         string `json:"repo"`
	RepositoryID int64  `json:"repository_id"`
}

// Validate reports whether the repository reference is well-formed.
func (r FollowUpFilingRepository) Validate() error {
	if !projectRepositoryPattern.MatchString(r.Repo) || strings.Contains(r.Repo, "..") {
		return fmt.Errorf("follow-up filing repo %q: %w", r.Repo, ErrEffectProposalInconsistent)
	}
	if r.RepositoryID <= 0 {
		return fmt.Errorf("follow-up filing repository_id %d: %w", r.RepositoryID, ErrNonPositive)
	}
	return nil
}

// ScreenedIssueText is one agent-controlled text field with the ruleset it
// was screened under and the verdict (plan §5.17). The record is a claim, not
// authority: domain cannot run the screen (it lives in
// internal/publicationtext, which imports the github/1 rules), so the store
// gate screens the stored text again under the recorded ruleset on every
// admission and reconstruction.
type ScreenedIssueText struct {
	Text    string           `json:"text"`
	Ruleset IssueTextRuleset `json:"ruleset"`
	Verdict ScreeningVerdict `json:"verdict"`
}

// validate checks the record a proposal may carry: bounded, valid UTF-8,
// non-blank text, screened under a registered ruleset, with a passed verdict.
// The field name is fixed by the caller; the text is never interpolated.
func (t ScreenedIssueText) validate(field string, maxBytes int, singleLine bool) error {
	if !utf8.ValidString(t.Text) || strings.TrimSpace(t.Text) == "" {
		return fmt.Errorf("follow-up filing %s text: %w", field, ErrEffectProposalInconsistent)
	}
	if len(t.Text) > maxBytes {
		return fmt.Errorf("follow-up filing %s text: %w", field, ErrProposalParameterTooLarge)
	}
	if singleLine && strings.ContainsAny(t.Text, "\r\n") {
		return fmt.Errorf("follow-up filing %s spans lines: %w", field, ErrEffectProposalInconsistent)
	}
	if !t.Ruleset.valid() {
		return fmt.Errorf("follow-up filing %s ruleset %q: %w", field, t.Ruleset, ErrFollowUpFilingTextUnscreened)
	}
	if t.Verdict != ScreeningVerdictPassed {
		return fmt.Errorf("follow-up filing %s verdict %q: %w", field, t.Verdict, ErrFollowUpFilingTextUnscreened)
	}
	return nil
}

// FollowUpFilingSource links a filing proposal back to what it came from: the
// finding, the FindingAdjudication artifact that routed it, and whether the
// route was a deferred disposition or a separate-work verdict. The store
// re-reads the adjudication and disposition rows on every gate; these fields
// only name which rows to read.
type FollowUpFilingSource struct {
	FindingID          FindingID          `json:"finding_id"`
	AdjudicationDigest Digest             `json:"adjudication_digest"`
	Kind               FollowUpSourceKind `json:"kind"`
}

// Validate reports whether the source link is structurally sound.
func (s FollowUpFilingSource) Validate() error {
	if s.FindingID == "" {
		return fmt.Errorf("follow-up filing source finding_id: %w", ErrEmptyID)
	}
	if !contentaddr.Valid(string(s.AdjudicationDigest)) {
		return fmt.Errorf("follow-up filing source adjudication_digest %q: %w",
			s.AdjudicationDigest, ErrEffectProposalInconsistent)
	}
	if !s.Kind.valid() {
		return fmt.Errorf("follow-up filing source kind %q: %w", s.Kind, ErrEffectProposalInconsistent)
	}
	return nil
}

// FollowUpFilingParameters is the fixed parameter type for the
// follow_up_filing effect kind (plan §5.17). No field is authority: the store
// re-derives the filing target from current rows, re-reads the source, and
// screens the text again through its gate. There is deliberately no field that
// names who files; the filing identity is chosen at dispatch.
type FollowUpFilingParameters struct {
	// SubjectHandle is the opaque work-unit handle, as the other kinds carry
	// it, so the store's subject_handle column and policy lookup apply.
	SubjectHandle OpaqueSubjectHandle `json:"subject_handle"`
	// Repository, Labels, and Milestone are the daemon's determination of the
	// filing target. Labels is sorted, duplicate-free, and never nil, so one
	// policy value has one encoding. Milestone is nil when policy names none.
	Repository FollowUpFilingRepository `json:"repository"`
	Labels     []string                 `json:"labels"`
	Milestone  *string                  `json:"milestone"`
	// Title and Body are the agent text with their screening records.
	Title  ScreenedIssueText    `json:"title"`
	Body   ScreenedIssueText    `json:"body"`
	Source FollowUpFilingSource `json:"source"`
}

// Validate reports whether the parameters are structurally sound. It is the
// reconstruction backstop: a decoded row cannot carry an unscreened field, a
// non-canonical label set, or an out-of-bounds value.
func (p FollowUpFilingParameters) Validate() error {
	if p.SubjectHandle == "" {
		return fmt.Errorf("follow-up filing subject_handle: %w", ErrEmptyID)
	}
	if err := p.Repository.Validate(); err != nil {
		return err
	}
	if p.Labels == nil {
		return fmt.Errorf("follow-up filing labels absent: %w", ErrEffectProposalInconsistent)
	}
	if err := validateFollowUpFilingLabels(p.Labels); err != nil {
		return fmt.Errorf("follow-up filing labels: %w", err)
	}
	if p.Milestone != nil {
		if err := validateFollowUpFilingName(*p.Milestone, MaxFollowUpFilingMilestoneBytes); err != nil {
			return fmt.Errorf("follow-up filing milestone: %w", err)
		}
	}
	if err := p.Title.validate("title", MaxFollowUpFilingTitleBytes, true); err != nil {
		return err
	}
	if err := p.Body.validate("body", MaxFollowUpFilingBodyBytes, false); err != nil {
		return err
	}
	return p.Source.Validate()
}

// validateFollowUpFilingName checks one label or milestone name: non-empty,
// trimmed, bounded, valid UTF-8, and free of control characters.
func validateFollowUpFilingName(name string, maxBytes int) error {
	if name == "" || strings.TrimSpace(name) != name || !utf8.ValidString(name) {
		return ErrEffectProposalInconsistent
	}
	if len(name) > maxBytes {
		return ErrProposalParameterTooLarge
	}
	if strings.ContainsFunc(name, unicode.IsControl) {
		return ErrEffectProposalInconsistent
	}
	return nil
}

// validateFollowUpFilingLabels checks the canonical label set: bounded,
// strictly ascending (so sorted and duplicate-free), each a valid name with
// no comma, the policy list's separator.
func validateFollowUpFilingLabels(labels []string) error {
	if len(labels) > MaxFollowUpFilingLabels {
		return ErrProposalParameterTooLarge
	}
	for i, label := range labels {
		if err := validateFollowUpFilingName(label, MaxFollowUpFilingLabelBytes); err != nil {
			return err
		}
		if strings.Contains(label, ",") || (i > 0 && labels[i-1] >= label) {
			return ErrEffectProposalInconsistent
		}
	}
	return nil
}

// FollowUpFilingTarget is the daemon's trusted determination of where a
// follow-up issue is filed and what it carries: the project's own repository
// and the labels and milestone the run's resolved policy names. It comes only
// from DeriveFollowUpFilingTarget and is never taken from a proposal body.
type FollowUpFilingTarget struct {
	Repo         string
	RepositoryID int64
	// Labels is sorted and duplicate-free; empty when policy names none.
	Labels []string
	// Milestone is the policy's milestone title; empty when policy names none.
	Milestone string
}

// Validate reports whether the determination is internally consistent.
func (t FollowUpFilingTarget) Validate() error {
	repository := FollowUpFilingRepository{Repo: t.Repo, RepositoryID: t.RepositoryID}
	if err := repository.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrFollowUpFilingTargetInconsistent, err)
	}
	if err := validateFollowUpFilingLabels(t.Labels); err != nil {
		return fmt.Errorf("follow-up filing target labels: %w: %w", ErrFollowUpFilingTargetInconsistent, err)
	}
	if t.Milestone != "" {
		if err := validateFollowUpFilingName(t.Milestone, MaxFollowUpFilingMilestoneBytes); err != nil {
			return fmt.Errorf("follow-up filing target milestone: %w: %w", ErrFollowUpFilingTargetInconsistent, err)
		}
	}
	return nil
}

// DeriveFollowUpFilingTarget is the single derivation of the filing target,
// shared by the proposal producer and the store gate so the two cannot
// disagree. The repository is the project's own; labels and milestone come
// from the run's resolved policy. A missing key means none. A present key
// with a malformed value fails closed instead of silently filing without it.
func DeriveFollowUpFilingTarget(project Project, policy ResolvedPolicy) (FollowUpFilingTarget, error) {
	if err := project.Validate(); err != nil {
		return FollowUpFilingTarget{}, fmt.Errorf("follow-up filing target: %w", err)
	}
	target := FollowUpFilingTarget{Repo: project.Repo, RepositoryID: project.RepositoryID}
	for _, key := range policy.Keys {
		switch key.Key {
		case PolicyFollowUpFilingLabels:
			for _, part := range strings.Split(key.Value, ",") {
				label := strings.TrimSpace(part)
				if label == "" {
					return FollowUpFilingTarget{}, fmt.Errorf("policy %s contains an empty entry: %w",
						PolicyFollowUpFilingLabels, ErrFollowUpFilingPolicyInvalid)
				}
				target.Labels = append(target.Labels, label)
			}
			slices.Sort(target.Labels)
			target.Labels = slices.Compact(target.Labels)
			if err := validateFollowUpFilingLabels(target.Labels); err != nil {
				return FollowUpFilingTarget{}, fmt.Errorf("policy %s: %w: %w",
					PolicyFollowUpFilingLabels, ErrFollowUpFilingPolicyInvalid, err)
			}
		case PolicyFollowUpFilingMilestone:
			milestone := strings.TrimSpace(key.Value)
			if err := validateFollowUpFilingName(milestone, MaxFollowUpFilingMilestoneBytes); err != nil {
				return FollowUpFilingTarget{}, fmt.Errorf("policy %s: %w: %w",
					PolicyFollowUpFilingMilestone, ErrFollowUpFilingPolicyInvalid, err)
			}
			target.Milestone = milestone
		}
	}
	return target, nil
}

// FollowUpFilingInput is the trusted construction input. Repository, labels,
// and milestone come from the daemon's Target determination alone: no field
// lets caller or agent text set them.
type FollowUpFilingInput struct {
	// SubjectHandle is the opaque work-unit handle.
	SubjectHandle OpaqueSubjectHandle
	// Target is the daemon's determination from DeriveFollowUpFilingTarget.
	Target FollowUpFilingTarget
	// Source links the proposal to the finding and adjudication it came from.
	Source FollowUpFilingSource
	// Title and Body are the agent text with the records of their screening.
	Title ScreenedIssueText
	Body  ScreenedIssueText
}

// parameters derives the stored parameters from the trusted input. It rejects
// a title or body whose record is not passed under a registered ruleset
// (through Validate), so an unscreened field never reaches a proposal.
func (in FollowUpFilingInput) parameters() (FollowUpFilingParameters, error) {
	if err := in.Target.Validate(); err != nil {
		return FollowUpFilingParameters{}, err
	}
	params := FollowUpFilingParameters{
		SubjectHandle: in.SubjectHandle,
		Repository:    FollowUpFilingRepository{Repo: in.Target.Repo, RepositoryID: in.Target.RepositoryID},
		// A fresh non-nil slice: the canonical encoding of no labels is [], and
		// the copy detaches the proposal from the caller's backing array.
		Labels: append([]string{}, in.Target.Labels...),
		Title:  in.Title, Body: in.Body, Source: in.Source,
	}
	if in.Target.Milestone != "" {
		milestone := in.Target.Milestone
		params.Milestone = &milestone
	}
	if err := params.Validate(); err != nil {
		return FollowUpFilingParameters{}, err
	}
	return params, nil
}

// GateFollowUpFiling re-gates a filing proposal against the daemon's current
// filing-target determination and rejects any stored repository, label set,
// or milestone that differs. The repository compares by id alone: the id is
// the identity, and a proposal made before a rename keeps the old name
// (#1537). The source link and the text screen are re-checked by the store,
// which holds the rows and the screen this package cannot reach.
func GateFollowUpFiling(proposal EffectProposal, target FollowUpFilingTarget) error {
	if proposal.Kind != EffectFollowUpFiling || proposal.FilingProposal == nil {
		return ErrEffectProposalInconsistent
	}
	filing := proposal.FilingProposal
	if err := filing.Validate(); err != nil {
		return err
	}
	if err := target.Validate(); err != nil {
		return err
	}
	if filing.Repository.RepositoryID != target.RepositoryID {
		return fmt.Errorf("follow-up filing repository: %w", ErrFollowUpFilingTargetMismatch)
	}
	if !slices.Equal(filing.Labels, target.Labels) {
		return fmt.Errorf("follow-up filing labels: %w", ErrFollowUpFilingTargetMismatch)
	}
	milestone := ""
	if filing.Milestone != nil {
		milestone = *filing.Milestone
	}
	if milestone != target.Milestone {
		return fmt.Errorf("follow-up filing milestone: %w", ErrFollowUpFilingTargetMismatch)
	}
	return nil
}
