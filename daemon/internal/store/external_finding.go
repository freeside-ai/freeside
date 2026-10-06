package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// PutExternalFinding persists a finding left on a published pull request by a
// reviewer outside Freeside (plan §5.19). It is the only write door for that
// record: PutFinding refuses one, so it can never be written as part of a
// review record or a shadow review record, and this door refuses an ID that
// either already links. Storing a finding grants nothing; nothing stored says
// its reviewer is admitted. A replayed write converges on byte-identical
// content.
func (tx *WriteTx) PutExternalFinding(ctx context.Context, finding domain.Finding) error {
	if finding.External == nil {
		return fmt.Errorf("put external finding %q: no external provenance: %w",
			finding.ID, domain.ErrExternalFindingInconsistent)
	}
	if err := tx.ensureFindingNotRoutedLinked(ctx, finding.ID); err != nil {
		return fmt.Errorf("put external finding %q routed link: %w", finding.ID, err)
	}
	if err := tx.ensureFindingNotShadowLinked(ctx, finding.ID); err != nil {
		return fmt.Errorf("put external finding %q shadow link: %w", finding.ID, err)
	}
	return tx.putFinding(ctx, finding)
}

// GetAdmittedExternalFinding is the one finding read that grants authority:
// it returns an external finding only while the trust profile active for the
// run's repository admits its reviewer to drive a round. The stored finding
// carries no admission to trust, so the check runs against current state on
// every read and fails closed with ErrExternalReviewNotAdmitted when the
// repository has no active profile, the reviewer is not listed, or the forge,
// account ID, or login differs from the listed entry. It answers who may
// drive a round, not whether this finding may: it does not compare the
// finding's head with the published one, which the re-entry gate does.
// GetFinding stays ungated: reading a finding as history never depends on the
// allowlist.
func (tx *ReadTx) GetAdmittedExternalFinding(ctx context.Context, id domain.FindingID) (domain.Finding, error) {
	finding, err := tx.GetFinding(ctx, id)
	if err != nil {
		return domain.Finding{}, err
	}
	if finding.External == nil {
		return domain.Finding{}, fmt.Errorf("get admitted external finding %q: no external provenance: %w",
			id, domain.ErrExternalFindingInconsistent)
	}
	itemID, err := tx.PublishedProductionReadyItemID(ctx, finding.RunID)
	if err != nil {
		return domain.Finding{}, fmt.Errorf("get admitted external finding %q: %w", id, err)
	}
	binding, err := tx.GetReadyItemPRBinding(ctx, itemID)
	if err != nil || binding.RunID != finding.RunID {
		return domain.Finding{}, fmt.Errorf("get admitted external finding %q: %w",
			id, errors.Join(err, domain.ErrParentKeyMismatch))
	}
	profile, err := tx.LatestTrustProfile(ctx, binding.Repo)
	if errors.Is(err, ErrNotFound) {
		err = errors.Join(err, domain.ErrExternalReviewNotAdmitted)
	}
	if err != nil {
		return domain.Finding{}, fmt.Errorf("get admitted external finding %q: %w", id, err)
	}
	if err := externalFindingAdmittedBy(finding, profile, binding); err != nil {
		return domain.Finding{}, fmt.Errorf("get admitted external finding %q: %w", id, err)
	}
	return finding, nil
}

// externalFindingAdmittedBy re-runs the allowlist check for one external
// finding against one validated profile. The profile must be for the bound
// repository by its immutable ID, not only its name, so a profile recorded
// for an earlier repository of the same owner/name admits nobody.
func externalFindingAdmittedBy(
	finding domain.Finding, profile domain.AutomationTrustProfile, binding domain.ReadyItemPRBinding,
) error {
	p := finding.External
	if p == nil {
		return domain.ErrExternalFindingInconsistent
	}
	if profile.Repo != binding.Repo || profile.RepositoryID != binding.RepositoryID {
		return fmt.Errorf("trust profile %s is not for repository %s (%d): %w",
			profile.ProfileDigest, binding.Repo, binding.RepositoryID, domain.ErrExternalReviewNotAdmitted)
	}
	authority, ok := profile.ExternalReviewAuthorityFor(p.Forge, p.ReviewerAccountID, p.ReviewerLogin)
	if !ok || authority != domain.ExternalReviewDriveRound {
		return fmt.Errorf("reviewer %s:%d %q under trust profile %s: %w",
			p.Forge, p.ReviewerAccountID, p.ReviewerLogin, profile.ProfileDigest, domain.ErrExternalReviewNotAdmitted)
	}
	return nil
}

// ListExternalFindings returns the external findings stored for one run,
// earliest first by the forge's timestamp and then by ID. Like GetFinding it
// is a history read and grants nothing: each finding still has to pass
// GetAdmittedExternalFinding before it may drive a round. Every finding row
// of the run is reconstructed and validated before the filter, so a damaged
// row fails the read instead of dropping out of it.
func (tx *ReadTx) ListExternalFindings(ctx context.Context, runID domain.RunID) ([]domain.Finding, error) {
	rows, err := tx.tx.QueryContext(ctx,
		`SELECT id, body FROM findings WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, fmt.Errorf("list external findings %q: %w", runID, err)
	}
	var out []domain.Finding
	for rows.Next() {
		var (
			id   string
			body []byte
		)
		if err := rows.Scan(&id, &body); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("list external findings %q: %w", runID, err)
		}
		finding, err := decode[domain.Finding](body)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("list external findings %q row %q: %w", runID, id, err)
		}
		if finding.ID != domain.FindingID(id) || finding.RunID != runID {
			_ = rows.Close()
			return nil, fmt.Errorf("list external findings %q row %q: %w", runID, id, errRowInconsistent)
		}
		if finding.External != nil {
			out = append(out, finding)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("list external findings %q: %w", runID, err)
	}
	slices.SortStableFunc(out, func(a, b domain.Finding) int {
		return a.CreatedAt.Compare(b.CreatedAt)
	})
	return out, nil
}

// ExternalReviewCycleFindings returns the external findings one
// external_review cycle answers: the run's external findings on the cycle's
// head whose reviewer the profile the authority names admits, earliest first.
// The authority is read through its gate, and the named profile is used, not
// the active one, so the set reads the same after the owner edits the
// allowlist. The finding the authority names is always in it.
func (tx *ReadTx) ExternalReviewCycleFindings(
	ctx context.Context, runID domain.RunID, publication domain.InvocationID,
) ([]domain.Finding, error) {
	successor, err := tx.GetPublicationSuccessor(ctx, runID, publication)
	if err != nil {
		return nil, fmt.Errorf("external review cycle findings %q: %w", publication, err)
	}
	admission, err := tx.externalReviewCycleAdmission(ctx, successor)
	if err != nil {
		return nil, fmt.Errorf("external review cycle findings %q: %w", publication, err)
	}
	all, err := tx.ListExternalFindings(ctx, runID)
	if err != nil {
		return nil, err
	}
	var (
		out   []domain.Finding
		named bool
	)
	for _, finding := range all {
		if admission.admits(finding) != nil {
			continue
		}
		named = named || finding.ID == successor.ExternalFindingID
		out = append(out, finding)
	}
	if !named {
		return nil, fmt.Errorf("external review cycle findings %q: triggering finding %q is not among them: %w",
			publication, successor.ExternalFindingID, domain.ErrParentKeyMismatch)
	}
	return out, nil
}

// externalReviewCycleAdmission is what decides whether an external finding
// belongs to one external_review cycle: the authority, already read through
// its gate, the binding of the ready item it superseded, and the trust
// profile it names.
type externalReviewCycleAdmission struct {
	authority domain.PublicationSuccessor
	ready     domain.ReadyItemPRBinding
	profile   domain.AutomationTrustProfile
}

func (tx *ReadTx) externalReviewCycleAdmission(
	ctx context.Context, authority domain.PublicationSuccessor,
) (externalReviewCycleAdmission, error) {
	none := externalReviewCycleAdmission{}
	if authority.EffectiveOrigin() != domain.PublicationSuccessorExternalReview || authority.Reentry == nil {
		return none, fmt.Errorf("not an external review cycle: %w", domain.ErrParentKeyMismatch)
	}
	ready, err := tx.GetReadyItemPRBinding(ctx, authority.PredecessorItemID)
	if err != nil || ready.RunID != authority.RunID {
		return none, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	profile, err := tx.GetTrustProfile(ctx, authority.AdmittingProfileDigest)
	if err != nil {
		return none, errors.Join(err, domain.ErrExternalReviewNotAdmitted)
	}
	return externalReviewCycleAdmission{authority: authority, ready: ready, profile: profile}, nil
}

// admits reports whether the cycle answers the finding: an external finding
// of the cycle's run, on the cycle's head, whose reviewer the named profile
// admits. The named profile is used, not the active one, so the answer reads
// the same after the owner edits the allowlist.
func (a externalReviewCycleAdmission) admits(finding domain.Finding) error {
	if finding.External == nil {
		return domain.ErrExternalFindingInconsistent
	}
	if finding.RunID != a.authority.RunID || finding.External.HeadSHA != a.authority.Reentry.HeadSHA {
		return domain.ErrParentKeyMismatch
	}
	return externalFindingAdmittedBy(finding, a.profile, a.ready)
}

// externalReviewAuthorityForRound picks, from a run's linked successors, the
// external_review one whose cycle starts at the given round, and false when
// there is none. Two for one round are refused: nothing says which of them
// names the findings the round answers.
//
// The round is the one the authority names. A cycle whose first review
// attempt fails reviews one round later and so has no first round here
// (#1781).
func externalReviewAuthorityForRound(
	chain []domain.PublicationSuccessor, round int,
) (domain.PublicationSuccessor, bool, error) {
	var (
		authority domain.PublicationSuccessor
		found     bool
	)
	for _, successor := range chain {
		if successor.EffectiveOrigin() != domain.PublicationSuccessorExternalReview ||
			successor.ReviewRound != round {
			continue
		}
		if found {
			return domain.PublicationSuccessor{}, false, domain.ErrParentKeyMismatch
		}
		authority, found = successor, true
	}
	return authority, found, nil
}

// externalReviewCycleForRound returns the external_review authority whose
// cycle starts at the given review record's round, read through its gate, and
// false when the run has none. A cycle that does start there must have
// reviewed what its authority names: a record on another base or head fails
// with ErrParentKeyMismatch, because everything bound to "the cycle's first
// round" would otherwise rest on a review of some other commit.
//
// The authority must be on the run's one authenticated successor chain. Its
// own gate is not enough: it admits any superseded ready item, including one
// a feedback return already superseded, and only sealing checks that the item
// is the run's current one. So the whole chain is read through
// PublicationSuccessorChain, which gates every sealed row and refuses a
// branch or an orphan.
//
// A read that is already reconstructing a successor or ready item cannot do
// that. A remediation successor's gate reads the adjudication of this round,
// and that read reaches here: gating the chain again would re-enter the
// successor being authenticated and fail its read as a cycle. Inside such a
// read the sealed rows are linked and only the authority is gated. The read
// that started the reconstruction decides what it trusts, and the chain walk
// is the one every current-successor read starts from.
func (tx *ReadTx) externalReviewCycleForRound(
	ctx context.Context, record domain.ReviewRecord,
) (domain.PublicationSuccessor, bool, error) {
	none := domain.PublicationSuccessor{}
	var chain []domain.PublicationSuccessor
	if publicationReadActive(ctx) {
		sealed, err := tx.dispatchedPublicationSuccessors(ctx, record.RunID)
		if err != nil {
			return none, false, err
		}
		if chain, err = linkPublicationSuccessors(record.RunID, sealed); err != nil {
			return none, false, err
		}
	} else {
		var err error
		if chain, err = tx.PublicationSuccessorChain(ctx, record.RunID); err != nil {
			return none, false, err
		}
	}
	sealedAuthority, found, err := externalReviewAuthorityForRound(chain, record.Round)
	if err != nil || !found {
		return none, false, err
	}
	authority, err := tx.GetPublicationSuccessor(ctx, record.RunID, sealedAuthority.PublicationID())
	if err != nil || !reflect.DeepEqual(authority, sealedAuthority) {
		return none, false, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	if authority.Reentry == nil || record.BaseSHA != authority.Reentry.BaseSHA ||
		record.HeadSHA != authority.Reentry.HeadSHA {
		return none, false, domain.ErrParentKeyMismatch
	}
	return authority, true, nil
}
