package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const (
	recordImagePolicyAdoptionSQL = `
INSERT INTO image_policy_adoptions (id, repository_id, instance_id, refusal_id, recorded_at, body)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT DO NOTHING`
	listImagePolicyAdoptionsSQL = `
SELECT id, repository_id, instance_id, refusal_id, recorded_at, body
FROM image_policy_adoptions ORDER BY recorded_at, id`
	imagePolicyAdoptionForInstanceSQL = `
SELECT id, repository_id, instance_id, refusal_id, recorded_at, body
FROM image_policy_adoptions WHERE instance_id = ?`
)

// RecordImagePolicyAdoption records the adoption an approved adoption card
// grants and returns the stored record. It re-gates the card against current
// rows (GetProposalInstance) and requires a recorded approve decision on it,
// so a declined or undecided card records nothing. One card records one
// adoption (instance_id is UNIQUE), so a repeated write converges on the
// first record and gets back its original RecordedAt and repository name,
// even across a project rename. The stored row is re-gated against the
// card, so one that decodes to other facts fails closed. It is a WriteTx
// method because an adoption widens the recipe set item evidence is gated
// against, which changes what a synchronized read returns.
func (tx *WriteTx) RecordImagePolicyAdoption(
	ctx context.Context, instanceID domain.ProposalInstanceID, recordedAt time.Time,
) (domain.ImagePolicyAdoption, error) {
	card, project, err := tx.approvedAdoptionCard(ctx, instanceID)
	if err != nil {
		return domain.ImagePolicyAdoption{}, fmt.Errorf("record image policy adoption %q: %w", instanceID, err)
	}
	adoption, err := domain.NewImagePolicyAdoption(card, project, recordedAt.UTC())
	if err != nil {
		return domain.ImagePolicyAdoption{}, fmt.Errorf("record image policy adoption %q: %w", instanceID, err)
	}
	body, err := encode(adoption)
	if err != nil {
		return domain.ImagePolicyAdoption{}, fmt.Errorf("record image policy adoption %q: %w", instanceID, err)
	}
	// The memo answers for the rows this transaction has read; a write changes
	// them.
	tx.adoptions = nil
	if _, err := tx.tx.ExecContext(ctx, recordImagePolicyAdoptionSQL,
		adoption.ID, adoption.RepositoryID, adoption.ProposalInstanceID, adoption.RefusalRecordID,
		formatTime(adoption.RecordedAt), body,
	); err != nil {
		return domain.ImagePolicyAdoption{}, fmt.Errorf("record image policy adoption %q: %w", instanceID, err)
	}
	stored, found, err := tx.ImagePolicyAdoptionForInstance(ctx, instanceID)
	if err != nil {
		return domain.ImagePolicyAdoption{}, fmt.Errorf("record image policy adoption %q: %w", instanceID, err)
	}
	if !found {
		return domain.ImagePolicyAdoption{}, fmt.Errorf("record image policy adoption %q: %w", instanceID, errRowInconsistent)
	}
	return stored, nil
}

// approvedAdoptionCard returns a gated adoption card that carries a recorded
// approve decision, with the project it names. The approve must have come
// through the card's own item: one in the card's project and batch that
// renders the card's digest, selecting that digest. A binding does not
// check the item against its card, so an approve on a mis-bound item
// adopts nothing. The item is read at the record tier, which runs no
// evidence gate, because the evidence gate reads adoptions.
func (tx *ReadTx) approvedAdoptionCard(
	ctx context.Context, instanceID domain.ProposalInstanceID,
) (domain.ProposalInstance, domain.Project, error) {
	card, err := tx.GetProposalInstance(ctx, instanceID)
	if err != nil {
		return domain.ProposalInstance{}, domain.Project{}, err
	}
	if card.Proposal.Kind != domain.EffectImagePolicyAdoption || card.Proposal.AdoptionProposal == nil {
		return domain.ProposalInstance{}, domain.Project{}, fmt.Errorf("instance is a %s card: %w",
			card.Proposal.Kind, domain.ErrEffectProposalInconsistent)
	}
	decision, err := tx.ProposalDecisionForInstance(ctx, instanceID)
	if err != nil {
		return domain.ProposalInstance{}, domain.Project{}, err
	}
	if decision == nil || decision.Action != domain.ActionApprove {
		return domain.ProposalInstance{}, domain.Project{}, fmt.Errorf("card is not approved: %w",
			domain.ErrTransitionCommandMismatch)
	}
	command, err := tx.GetCommand(ctx, decision.CommandID)
	if err != nil {
		return domain.ProposalInstance{}, domain.Project{}, err
	}
	item, err := tx.GetAttentionItemRecord(ctx, command.ItemID)
	if err != nil {
		return domain.ProposalInstance{}, domain.Project{}, err
	}
	if item.ProjectID != card.Proposal.AdoptionProposal.ProjectID ||
		item.Subject.Type != domain.SubjectProposalBatch ||
		item.Subject.ID != domain.SubjectID(card.ProposalBatchID) ||
		!slices.Equal(item.ArtifactDigests, []domain.Digest{card.Proposal.Digest}) ||
		decision.SelectedDigest == nil || *decision.SelectedDigest != card.Proposal.Digest {
		return domain.ProposalInstance{}, domain.Project{}, fmt.Errorf("card is not approved through its own item: %w",
			domain.ErrTransitionCommandMismatch)
	}
	project, err := tx.GetProject(ctx, card.Proposal.AdoptionProposal.ProjectID)
	if err != nil {
		return domain.ProposalInstance{}, domain.Project{}, err
	}
	return card, project, nil
}

// ImagePolicyAdoptionForInstance returns the adoption one card recorded,
// reporting absence rather than erroring.
func (tx *ReadTx) ImagePolicyAdoptionForInstance(
	ctx context.Context, instanceID domain.ProposalInstanceID,
) (domain.ImagePolicyAdoption, bool, error) {
	adoption, err := scanImagePolicyAdoption(tx.tx.QueryRowContext(ctx, imagePolicyAdoptionForInstanceSQL, instanceID))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ImagePolicyAdoption{}, false, nil
	}
	if err != nil {
		return domain.ImagePolicyAdoption{}, false, fmt.Errorf("image policy adoption for %q: %w", instanceID, err)
	}
	if adoption.ProposalInstanceID != instanceID {
		return domain.ImagePolicyAdoption{}, false, fmt.Errorf("image policy adoption for %q: %w", instanceID, errRowInconsistent)
	}
	if err := tx.gateImagePolicyAdoption(ctx, adoption); err != nil {
		return domain.ImagePolicyAdoption{}, false, fmt.Errorf("image policy adoption for %q: %w", instanceID, err)
	}
	return adoption, true, nil
}

// ListImagePolicyAdoptions returns one repository's adoptions in the order
// they were recorded.
func (tx *ReadTx) ListImagePolicyAdoptions(
	ctx context.Context, repositoryID int64,
) ([]domain.ImagePolicyAdoption, error) {
	adoptions, err := tx.imagePolicyAdoptions(ctx)
	if err != nil {
		return nil, err
	}
	var out []domain.ImagePolicyAdoption
	for _, adoption := range adoptions {
		if adoption.RepositoryID == repositoryID {
			out = append(out, adoption)
		}
	}
	return out, nil
}

// EffectiveApprovedRecipes returns the recipe set one repository's evidence
// is gated against: the operator's approved recipes plus the digests that
// repository's adoptions approved. The map is the caller's to keep.
func (tx *ReadTx) EffectiveApprovedRecipes(ctx context.Context, repositoryID int64) (map[domain.Digest]bool, error) {
	adoptions, err := tx.imagePolicyAdoptions(ctx)
	if err != nil {
		return nil, err
	}
	return tx.recipesWith(adoptions, func(a domain.ImagePolicyAdoption) bool {
		return a.RepositoryID == repositoryID
	}), nil
}

// AdoptedRegistryHosts returns the registry hosts one repository's adoptions
// added, deduplicated, in canonical order, for domain.EffectiveRegistrySet.
func (tx *ReadTx) AdoptedRegistryHosts(ctx context.Context, repositoryID int64) ([]string, error) {
	adoptions, err := tx.ListImagePolicyAdoptions(ctx, repositoryID)
	if err != nil {
		return nil, err
	}
	var hosts []string
	for _, adoption := range adoptions {
		hosts = append(hosts, adoption.RegistryAuthorities...)
	}
	slices.Sort(hosts)
	return slices.Compact(hosts), nil
}

// storeRecipeSet is the recipe set a boundary that knows only the artifact
// row gates against: the operator's set plus every digest adopted in this
// store. An artifact carries no repository, so the per-repository rule is
// enforced where evidence gains authority (gateEvidence, and the consumers
// of EffectiveApprovedRecipes); this boundary stays the anti-forgery gate, so
// a publish_eligible bit under a digest no policy approved is still refused.
// The returned map is read-only.
func (tx *ReadTx) storeRecipeSet(ctx context.Context) (map[domain.Digest]bool, error) {
	adoptions, err := tx.imagePolicyAdoptions(ctx)
	if err != nil {
		return nil, err
	}
	if len(adoptions) == 0 {
		return tx.approvedRecipes, nil
	}
	return tx.recipesWith(adoptions, func(domain.ImagePolicyAdoption) bool { return true }), nil
}

// recipesWith returns a copy of the operator's set plus the adopted digests
// of the selected adoptions that the transaction's recipe scope admits.
func (tx *ReadTx) recipesWith(
	adoptions []domain.ImagePolicyAdoption, selected func(domain.ImagePolicyAdoption) bool,
) map[domain.Digest]bool {
	recipes := maps.Clone(tx.approvedRecipes)
	if recipes == nil {
		recipes = map[domain.Digest]bool{}
	}
	for _, adoption := range adoptions {
		if adoption.RecipeDigest == nil || !selected(adoption) {
			continue
		}
		if tx.recipeScope != nil && !tx.recipeScope[*adoption.RecipeDigest] {
			continue
		}
		recipes[*adoption.RecipeDigest] = true
	}
	return recipes
}

// imagePolicyAdoptions loads and gates every adoption once per transaction.
// Every row it re-gates against (the card, its decision, its refusal record,
// and its project's repository) is immutable, so only an adoption write can
// change the answer, and RecordImagePolicyAdoption clears the memo. A
// database without the table (a backup file older than migration 0110, or a
// data migration that runs before it) has no adoptions.
func (tx *ReadTx) imagePolicyAdoptions(ctx context.Context) ([]domain.ImagePolicyAdoption, error) {
	if tx.adoptions != nil {
		return *tx.adoptions, nil
	}
	exists, err := tx.tableExists(ctx, "image_policy_adoptions")
	if err != nil {
		return nil, fmt.Errorf("list image policy adoptions: %w", err)
	}
	var adoptions []domain.ImagePolicyAdoption
	if exists {
		adoptions, err = tx.scanImagePolicyAdoptions(ctx)
		if err != nil {
			return nil, fmt.Errorf("list image policy adoptions: %w", err)
		}
	}
	for _, adoption := range adoptions {
		if err := tx.gateImagePolicyAdoption(ctx, adoption); err != nil {
			return nil, fmt.Errorf("image policy adoption %s: %w", adoption.ID, err)
		}
	}
	tx.adoptions = &adoptions
	return adoptions, nil
}

func (tx *ReadTx) scanImagePolicyAdoptions(ctx context.Context) ([]domain.ImagePolicyAdoption, error) {
	rows, err := tx.tx.QueryContext(ctx, listImagePolicyAdoptionsSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // rows.Err reports deferred-close failures
	var adoptions []domain.ImagePolicyAdoption
	for rows.Next() {
		adoption, err := scanImagePolicyAdoption(rows)
		if err != nil {
			return nil, err
		}
		adoptions = append(adoptions, adoption)
	}
	return adoptions, rows.Err()
}

// gateImagePolicyAdoption re-gates a decoded adoption: a decoded recipe
// approval is never trusted on its own. The record must repeat its card's
// parameters and its project's repository, and the card must still pass its
// gate and carry an approve decision.
func (tx *ReadTx) gateImagePolicyAdoption(ctx context.Context, adoption domain.ImagePolicyAdoption) error {
	card, project, err := tx.approvedAdoptionCard(ctx, adoption.ProposalInstanceID)
	if err != nil {
		return err
	}
	params, recorded := card.Proposal.AdoptionProposal, adoption.Parameters()
	if params.ProjectID != recorded.ProjectID || params.RefusalRecordID != recorded.RefusalRecordID ||
		!optionalDigestEqual(params.RecipeDigest, recorded.RecipeDigest) ||
		!slices.Equal(params.RegistryAuthorities, recorded.RegistryAuthorities) ||
		project.RepositoryID != adoption.RepositoryID {
		return errRowInconsistent
	}
	return nil
}

func optionalDigestEqual(a, b *domain.Digest) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// scanImagePolicyAdoption decodes one row. Decoding recomputes the content
// address, and the indexed columns must agree with the body, so neither can
// be altered alone.
func scanImagePolicyAdoption(row scanner) (domain.ImagePolicyAdoption, error) {
	var (
		id, instanceID, refusalID, recordedAt string
		repositoryID                          int64
		body                                  []byte
	)
	if err := row.Scan(&id, &repositoryID, &instanceID, &refusalID, &recordedAt, &body); err != nil {
		return domain.ImagePolicyAdoption{}, err
	}
	adoption, err := decode[domain.ImagePolicyAdoption](body)
	if err != nil {
		return domain.ImagePolicyAdoption{}, fmt.Errorf("%w: %w", errRowInconsistent, err)
	}
	if string(adoption.ID) != id || adoption.RepositoryID != repositoryID ||
		string(adoption.ProposalInstanceID) != instanceID || string(adoption.RefusalRecordID) != refusalID ||
		formatTime(adoption.RecordedAt) != recordedAt {
		return domain.ImagePolicyAdoption{}, errRowInconsistent
	}
	return adoption, nil
}
