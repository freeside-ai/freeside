package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

var (
	// ErrImagePolicyAdoptionInvalid marks a malformed adoption record.
	ErrImagePolicyAdoptionInvalid = errors.New("image policy adoption is invalid")
	// ErrImagePolicyAdoptionInconsistent marks an adoption record whose ID is
	// not the content address of its facts.
	ErrImagePolicyAdoptionInconsistent = errors.New("image policy adoption id does not match its content")
	// ErrImagePolicyAdoptionUnbacked marks an adoption card the current
	// project and refusal record do not back.
	ErrImagePolicyAdoptionUnbacked = errors.New("image policy adoption card is not backed by its refusal record")
)

// imagePolicyAdoptionEncodingVersion addresses an adoption record. It must
// never change once a record is stored: every stored ID validates against
// this preimage.
const imagePolicyAdoptionEncodingVersion = "freeside.image-policy-adoption/v1"

// ImagePolicyAdoption is the immutable record of one approved adoption card:
// the recipe digest or registry hosts the daemon's own policy adopts for one
// repository (plan §5.7, Project-image upkeep; plan revision 101). Its ID is
// the content address of every fact but RecordedAt, so a repeated write for
// the same card converges on the first record. Repository is the name the
// project carried when the adoption was recorded; RepositoryID is the
// identity. RegistryAuthorities is strictly ascending and never nil.
type ImagePolicyAdoption struct {
	ID                  Digest             `json:"id"`
	RepositoryID        int64              `json:"repository_id"`
	Repository          string             `json:"repository"`
	ProjectID           ProjectID          `json:"project_id"`
	ProposalInstanceID  ProposalInstanceID `json:"proposal_instance_id"`
	RefusalRecordID     Digest             `json:"refusal_record_id"`
	RecipeDigest        *Digest            `json:"recipe_digest"`
	RegistryAuthorities []string           `json:"registry_authorities"`
	RecordedAt          time.Time          `json:"recorded_at"`
}

// canonicalImagePolicyAdoption is the preimage the ID addresses. Its field
// order and names are part of the v1 encoding.
type canonicalImagePolicyAdoption struct {
	Version             string             `json:"version"`
	RepositoryID        int64              `json:"repository_id"`
	Repository          string             `json:"repository"`
	ProjectID           ProjectID          `json:"project_id"`
	ProposalInstanceID  ProposalInstanceID `json:"proposal_instance_id"`
	RefusalRecordID     Digest             `json:"refusal_record_id"`
	RecipeDigest        *Digest            `json:"recipe_digest"`
	RegistryAuthorities []string           `json:"registry_authorities"`
}

// NewImagePolicyAdoption builds the adoption record for an approved card.
// The caller has gated the card (GateImagePolicyAdoption) and checked its
// approve decision; this binds the record's facts to the card's parameters
// and the project's repository so the two cannot diverge.
func NewImagePolicyAdoption(
	card ProposalInstance, project Project, recordedAt time.Time,
) (ImagePolicyAdoption, error) {
	params := card.Proposal.AdoptionProposal
	if card.Proposal.Kind != EffectImagePolicyAdoption || params == nil {
		return ImagePolicyAdoption{}, fmt.Errorf("image policy adoption from %s card: %w",
			card.Proposal.Kind, ErrEffectProposalInconsistent)
	}
	if params.ProjectID != project.ID {
		return ImagePolicyAdoption{}, fmt.Errorf("image policy adoption card project %q, project %q: %w",
			params.ProjectID, project.ID, ErrImagePolicyAdoptionUnbacked)
	}
	var recipe *Digest
	if params.RecipeDigest != nil {
		digest := *params.RecipeDigest
		recipe = &digest
	}
	adoption := ImagePolicyAdoption{
		RepositoryID: project.RepositoryID, Repository: project.Repo, ProjectID: project.ID,
		ProposalInstanceID: card.ID, RefusalRecordID: params.RefusalRecordID,
		RecipeDigest: recipe, RegistryAuthorities: slices.Clone(params.RegistryAuthorities),
		RecordedAt: recordedAt,
	}
	id, err := adoption.ComputeID()
	if err != nil {
		return ImagePolicyAdoption{}, err
	}
	adoption.ID = id
	if err := adoption.Validate(); err != nil {
		return ImagePolicyAdoption{}, err
	}
	return adoption, nil
}

// ComputeID returns the versioned content address of every fact but
// RecordedAt.
func (a ImagePolicyAdoption) ComputeID() (Digest, error) {
	body, err := json.Marshal(canonicalImagePolicyAdoption{
		Version:      imagePolicyAdoptionEncodingVersion,
		RepositoryID: a.RepositoryID, Repository: a.Repository, ProjectID: a.ProjectID,
		ProposalInstanceID: a.ProposalInstanceID, RefusalRecordID: a.RefusalRecordID,
		RecipeDigest: a.RecipeDigest, RegistryAuthorities: a.RegistryAuthorities,
	})
	if err != nil {
		return "", fmt.Errorf("image policy adoption id: %w", err)
	}
	return Digest(contentaddr.Sum(body)), nil
}

// Parameters returns the card parameters the record adopts, for comparing a
// stored record against its card.
func (a ImagePolicyAdoption) Parameters() ImagePolicyAdoptionParameters {
	return ImagePolicyAdoptionParameters{
		ProjectID: a.ProjectID, RefusalRecordID: a.RefusalRecordID,
		RecipeDigest: a.RecipeDigest, RegistryAuthorities: a.RegistryAuthorities,
	}
}

// Validate checks the record's shape and that its ID addresses its facts.
// The adopted facts follow the card parameters' rules: at least one of a
// recipe digest or a host, and the hosts canonical.
func (a ImagePolicyAdoption) Validate() error {
	if a.RepositoryID <= 0 {
		return fmt.Errorf("image policy adoption repository_id %d: %w", a.RepositoryID, ErrNonPositive)
	}
	if !projectRepositoryPattern.MatchString(a.Repository) || strings.Contains(a.Repository, "..") {
		return fmt.Errorf("image policy adoption repository %q: %w", a.Repository, ErrImagePolicyAdoptionInvalid)
	}
	if a.ProposalInstanceID == "" {
		return fmt.Errorf("image policy adoption proposal_instance_id: %w", ErrEmptyID)
	}
	if err := a.Parameters().Validate(); err != nil {
		return fmt.Errorf("image policy adoption: %w", errors.Join(err, ErrImagePolicyAdoptionInvalid))
	}
	if a.RecordedAt.IsZero() {
		return fmt.Errorf("image policy adoption recorded_at: %w", ErrMissingTimestamp)
	}
	if a.RecordedAt.Location() != time.UTC {
		return fmt.Errorf("image policy adoption recorded_at: %w", ErrTimestampNotUTC)
	}
	if !contentaddr.Valid(string(a.ID)) {
		return fmt.Errorf("image policy adoption id %q: %w", a.ID, ErrImagePolicyAdoptionInvalid)
	}
	computed, err := a.ComputeID()
	if err != nil {
		return err
	}
	if a.ID != computed {
		return fmt.Errorf("image policy adoption %s, content resolves to %s: %w",
			a.ID, computed, ErrImagePolicyAdoptionInconsistent)
	}
	return nil
}

// GateImagePolicyAdoption re-gates an adoption card against the current
// project it names and the refusal record it answers: the refusal is
// policy-class and belongs to the project's repository, and each proposed
// fact has a refused clause behind it. The refusal keeps clause names and
// free-text detail only, so the match is by clause; the exact digest and
// hosts are the producer's re-derivation from the tree at the refusal's tip.
func GateImagePolicyAdoption(proposal EffectProposal, project Project, refusal ImageUpkeepRefusal) error {
	if proposal.Kind != EffectImagePolicyAdoption || proposal.AdoptionProposal == nil {
		return ErrEffectProposalInconsistent
	}
	if err := proposal.Validate(); err != nil {
		return err
	}
	if err := project.Validate(); err != nil {
		return err
	}
	if err := refusal.Validate(); err != nil {
		return err
	}
	params := proposal.AdoptionProposal
	unbacked := func(reason string) error {
		return fmt.Errorf("image policy adoption card %s: %s: %w", proposal.Digest, reason, ErrImagePolicyAdoptionUnbacked)
	}
	switch {
	case project.ID != params.ProjectID:
		return unbacked("project differs")
	case refusal.ID != params.RefusalRecordID:
		return unbacked("refusal record differs")
	case refusal.Class != ImageUpkeepRefusalPolicy:
		return unbacked("refusal is class " + string(refusal.Class))
	case refusal.RepositoryID != project.RepositoryID:
		return unbacked("refusal belongs to another repository")
	}
	if params.RecipeDigest != nil && !refusalNames(refusal, RebuildRefusalRecipeChanged) {
		return unbacked("recipe digest proposed without a recipe_changed refusal")
	}
	if len(params.RegistryAuthorities) > 0 &&
		!refusalNames(refusal, RebuildRefusalUndeclaredAuthority, RebuildRefusalNoRegistrySet) {
		return unbacked("registry hosts proposed without an undeclared_authority or no_registry_set refusal")
	}
	return nil
}

// refusalNames reports whether any of the refusal's findings names one of
// the clauses.
func refusalNames(refusal ImageUpkeepRefusal, clauses ...RebuildRefusal) bool {
	return slices.ContainsFunc(refusal.Findings, func(finding ImageUpkeepClauseFinding) bool {
		return slices.Contains(clauses, finding.Clause)
	})
}
