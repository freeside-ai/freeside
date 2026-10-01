package store

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// ErrSupersessionAutoRouteUnproven refuses an automatic-route supersession
// authority. The record's automatic kind stores no reference: the route gate
// re-derives it from the audit, the run's policy, and the stored dispositions,
// and that gate does not exist yet (#1051). Until it does, nothing can
// re-prove the authority, so writes and reads both refuse it.
var ErrSupersessionAutoRouteUnproven = errors.New(
	"automatic-route disposition supersession cannot be re-derived")

const putDispositionSupersessionSQL = `
INSERT INTO finding_disposition_supersessions
    (finding_id, superseded_round, run_id, reversing_round, body_digest, body)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (finding_id, superseded_round) DO NOTHING`

// supersessionBindingFailure keeps a missing parent from reading as a missing
// supersession. A caller treats ErrNotFound from a supersession read as "this
// disposition was never reversed", so a record whose disposition, audit, or
// item is gone must fail closed instead.
func supersessionBindingFailure(what string, err error) error {
	if errors.Is(err, ErrNotFound) {
		// The cause is kept as text only, so the chain no longer holds
		// ErrNotFound.
		return fmt.Errorf("%s: %s: %w", what, err.Error(), domain.ErrParentKeyMismatch)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// validateDispositionSupersessionScope re-runs the joins that say what was
// reversed, without following the authority (plan §7 Review Drift). The
// superseded disposition must be this run's, be fixed, and name the copied
// remediation invocation. The audit must be this run's, be over_hardened, list
// the finding among its reversals, and have judged the reversing round. Both
// loads re-run their own bindings.
//
// It is separate from the authority check because the convergence rebuild
// (#1051) must read a record's scope while it is rebuilding the item the
// record's human authority names.
func (tx *ReadTx) validateDispositionSupersessionScope(
	ctx context.Context, supersession domain.FindingDispositionSupersession,
) error {
	disposition, err := tx.GetFindingDisposition(ctx, supersession.FindingID, supersession.SupersededRound)
	if err != nil {
		return supersessionBindingFailure("superseded disposition", err)
	}
	if disposition.RunID != supersession.RunID ||
		disposition.Disposition != domain.ReviewDispositionFixed ||
		disposition.RemediationInvocationID != supersession.RemediationInvocationID {
		return fmt.Errorf("superseded disposition: %w", domain.ErrParentKeyMismatch)
	}
	audit, err := tx.GetDriftAudit(ctx, supersession.DriftAuditDigest)
	if err != nil {
		return supersessionBindingFailure("drift audit", err)
	}
	if audit.RunID != supersession.RunID || audit.Round != supersession.ReversingRound ||
		audit.Verdict != domain.DriftVerdictOverHardened ||
		!slices.ContainsFunc(audit.Reversals, func(reversal domain.DriftReversal) bool {
			return reversal.FindingID == supersession.FindingID
		}) {
		return fmt.Errorf("drift audit: %w", domain.ErrParentKeyMismatch)
	}
	return nil
}

// validateDispositionSupersessionAuthority re-proves who ordered the reversal
// against current state. A human authority must be the stored
// continue_under_policy command on the reversing round's diminishing-returns
// item, at the item version it was issued against.
//
// It does not check that the item parked on a drift audit: that cause arrives
// with #1667. Nothing writes a record before #1051.
func (tx *ReadTx) validateDispositionSupersessionAuthority(
	ctx context.Context, supersession domain.FindingDispositionSupersession,
) error {
	switch supersession.Authority.Kind {
	case domain.DispositionSupersessionAutoRoute:
		return ErrSupersessionAutoRouteUnproven
	case domain.DispositionSupersessionHumanCommand:
		// Validate guarantees the reference for this kind.
		reference := supersession.Authority.Command
		decision, err := tx.ReviewDiminishingDecision(ctx, reference.ItemID)
		if err != nil {
			return supersessionBindingFailure("authority item", err)
		}
		if decision.Binding.RunID != supersession.RunID ||
			decision.Binding.Round != supersession.ReversingRound ||
			decision.Command == nil ||
			decision.Command.Action != domain.ActionContinueUnderPolicy ||
			decision.Command.CommandID != reference.CommandID ||
			decision.Command.ItemVersion != reference.ItemVersion {
			return fmt.Errorf("authority command: %w", domain.ErrParentKeyMismatch)
		}
		return nil
	}
	return domain.ErrInvalidSupersessionAuthorityKind
}

func (tx *ReadTx) validateDispositionSupersession(
	ctx context.Context, supersession domain.FindingDispositionSupersession,
) error {
	if err := tx.validateDispositionSupersessionScope(ctx, supersession); err != nil {
		return err
	}
	return tx.validateDispositionSupersessionAuthority(ctx, supersession)
}

// PutFindingDispositionSupersession records that a drift audit's reversal
// undid one fixed disposition. A byte-identical replay converges; a different
// record for the same disposition is an immutable conflict.
//
// A record changes a finding's effective disposition from its reversing round
// on, and #1051 re-derives each stored item's stop cause through these
// records. A record that appeared for a round the run has already moved past
// would change a cause an item already stored, so a new record is refused once
// a later round is recorded or failed (the PutDriftAudit rule). Reads don't
// repeat this check: later rounds are expected by the time a record is read.
func (tx *WriteTx) PutFindingDispositionSupersession(
	ctx context.Context, supersession domain.FindingDispositionSupersession,
) error {
	fail := func(stage string, err error) error {
		return fmt.Errorf("put disposition supersession %q round %d%s: %w",
			supersession.FindingID, supersession.SupersededRound, stage, err)
	}
	body, err := encode(supersession)
	if err != nil {
		return fail("", err)
	}
	if err := tx.validateDispositionSupersession(ctx, supersession); err != nil {
		return fail(" binding", err)
	}
	// Reconstruct an existing row before the replay comparison, so corruption
	// in a copied lookup column cannot hide behind an unchanged body.
	if _, err := tx.GetFindingDispositionSupersession(
		ctx, supersession.FindingID, supersession.SupersededRound,
	); errors.Is(err, ErrNotFound) {
		var successors int
		// Failed rounds share the run's round numbers, so a later failure
		// proves the run moved past the reversing round as a later record does.
		if err := tx.tx.QueryRowContext(ctx, `SELECT
			(SELECT COUNT(*) FROM review_records WHERE run_id = ? AND round > ?) +
			(SELECT COUNT(*) FROM review_failures WHERE run_id = ? AND round > ?)`,
			supersession.RunID, supersession.ReversingRound,
			supersession.RunID, supersession.ReversingRound).Scan(&successors); err != nil {
			return fail("", err)
		}
		if successors > 0 {
			return fail(" after a later round", ErrImmutableConflict)
		}
	} else if err != nil {
		return fail(" existing row", err)
	}
	if err := tx.putImmutable(ctx, putDispositionSupersessionSQL,
		[]any{
			supersession.FindingID, supersession.SupersededRound,
			supersession.RunID, supersession.ReversingRound, reviewBodyDigest(body), body,
		},
		`SELECT body_digest || body FROM finding_disposition_supersessions
			WHERE finding_id = ? AND superseded_round = ?`,
		[]any{supersession.FindingID, supersession.SupersededRound},
		reviewBodyAuthority(body)); err != nil {
		return fail("", err)
	}
	return nil
}

type dispositionSupersessionRow struct {
	findingID       string
	supersededRound int
	runID           string
	reversingRound  int
	bodyDigest      string
	body            []byte
}

const selectDispositionSupersessionColumns = `finding_id, superseded_round, run_id, reversing_round,
	body_digest, body`

func scanDispositionSupersessionRow(sc scanner) (dispositionSupersessionRow, error) {
	var row dispositionSupersessionRow
	err := sc.Scan(&row.findingID, &row.supersededRound, &row.runID, &row.reversingRound,
		&row.bodyDigest, &row.body)
	return row, err
}

// reconstructDispositionSupersession is the one rebuild path for every read.
// It re-runs the body integrity digest, the decode with the record's
// validation, the agreement of the copied keys with the decoded body, and the
// same scope and authority checks a write runs. No copied column and no stored
// round, invocation, digest, or command reference is trusted.
func (tx *ReadTx) reconstructDispositionSupersession(
	ctx context.Context, row dispositionSupersessionRow,
) (domain.FindingDispositionSupersession, error) {
	if row.bodyDigest != reviewBodyDigest(string(row.body)) {
		return domain.FindingDispositionSupersession{}, errRowInconsistent
	}
	supersession, err := decode[domain.FindingDispositionSupersession](row.body)
	if err != nil {
		return domain.FindingDispositionSupersession{}, err
	}
	if string(supersession.FindingID) != row.findingID ||
		supersession.SupersededRound != row.supersededRound ||
		string(supersession.RunID) != row.runID || supersession.ReversingRound != row.reversingRound {
		return domain.FindingDispositionSupersession{}, errRowInconsistent
	}
	if err := tx.validateDispositionSupersession(ctx, supersession); err != nil {
		return domain.FindingDispositionSupersession{}, err
	}
	return supersession, nil
}

// GetFindingDispositionSupersession reconstructs the record that supersedes
// one disposition. ErrNotFound means the disposition has no record.
func (tx *ReadTx) GetFindingDispositionSupersession(
	ctx context.Context, findingID domain.FindingID, supersededRound int,
) (domain.FindingDispositionSupersession, error) {
	row, err := scanDispositionSupersessionRow(tx.tx.QueryRowContext(ctx,
		`SELECT `+selectDispositionSupersessionColumns+` FROM finding_disposition_supersessions
			WHERE finding_id = ? AND superseded_round = ?`, findingID, supersededRound))
	if err != nil {
		return domain.FindingDispositionSupersession{}, fmt.Errorf(
			"get disposition supersession %q round %d: %w", findingID, supersededRound, notFoundOr(err))
	}
	supersession, err := tx.reconstructDispositionSupersession(ctx, row)
	if err != nil {
		return domain.FindingDispositionSupersession{}, fmt.Errorf(
			"get disposition supersession %q round %d: %w", findingID, supersededRound, err)
	}
	return supersession, nil
}

// ListFindingDispositionSupersessions returns one run's records in reversing
// round then finding order. It enumerates the whole table and reconstructs
// every row before the run filter, so a corrupted copied run key cannot move a
// row out of the list and make a reversed fix read as still fixed (the
// review-record list pattern).
func (tx *ReadTx) ListFindingDispositionSupersessions(
	ctx context.Context, runID domain.RunID,
) ([]domain.FindingDispositionSupersession, error) {
	rows, err := tx.tx.QueryContext(ctx,
		`SELECT `+selectDispositionSupersessionColumns+` FROM finding_disposition_supersessions
			ORDER BY run_id, reversing_round, finding_id, superseded_round`)
	if err != nil {
		return nil, fmt.Errorf("list disposition supersessions %q: %w", runID, err)
	}
	var raw []dispositionSupersessionRow
	for rows.Next() {
		row, err := scanDispositionSupersessionRow(rows)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("list disposition supersessions %q row %d: %w", runID, len(raw)+1, err)
		}
		raw = append(raw, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("list disposition supersessions %q: %w", runID, err)
	}
	out := make([]domain.FindingDispositionSupersession, 0, len(raw))
	for i, row := range raw {
		supersession, err := tx.reconstructDispositionSupersession(ctx, row)
		if err != nil {
			return nil, fmt.Errorf("list disposition supersessions %q row %d: %w", runID, i+1, err)
		}
		if supersession.RunID == runID {
			out = append(out, supersession)
		}
	}
	return out, nil
}
