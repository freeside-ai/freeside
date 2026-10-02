package store

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// ErrSupersessionRouteUnproven refuses a supersession whose reversal the route
// gate does not re-derive from stored records (plan §7 Review Drift). The
// automatic kind stores no reference, so the gate must report that the round
// routes automatically; a human command still needs the reversal list to pass
// the gate, because the command orders the round, not the list's validity.
var ErrSupersessionRouteUnproven = errors.New(
	"disposition supersession is not re-derived by the drift route gate")

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
// reversed, without following the authority (plan §7 Review Drift). Both loads
// re-run their own bindings.
func (tx *ReadTx) validateDispositionSupersessionScope(
	ctx context.Context, supersession domain.FindingDispositionSupersession,
) error {
	disposition, err := tx.GetFindingDisposition(ctx, supersession.FindingID, supersession.SupersededRound)
	if err != nil {
		return supersessionBindingFailure("superseded disposition", err)
	}
	audit, err := tx.GetDriftAudit(ctx, supersession.DriftAuditDigest)
	if err != nil {
		return supersessionBindingFailure("drift audit", err)
	}
	return checkDispositionSupersessionScope(supersession, disposition, audit)
}

// checkDispositionSupersessionScope is the scope rule over rows the caller has
// already authenticated. The superseded disposition must be this run's, be
// fixed, and name the copied remediation invocation. The audit must be this
// run's, be over_hardened, list the finding among its reversals, and have
// judged the reversing round.
//
// It is separate from the loads because the decision-time reads take the
// disposition from their own causal slice: GetFindingDisposition follows every
// deferred row's item authority, and that item's rebuild is what is reading.
func checkDispositionSupersessionScope(
	supersession domain.FindingDispositionSupersession,
	disposition domain.ReviewDispositionRecord,
	audit domain.DriftAudit,
) error {
	if disposition.FindingID != supersession.FindingID ||
		disposition.Round != supersession.SupersededRound ||
		disposition.RunID != supersession.RunID ||
		disposition.Disposition != domain.ReviewDispositionFixed ||
		disposition.RemediationInvocationID != supersession.RemediationInvocationID {
		return fmt.Errorf("superseded disposition: %w", domain.ErrParentKeyMismatch)
	}
	if audit.Digest != supersession.DriftAuditDigest ||
		audit.RunID != supersession.RunID || audit.Round != supersession.ReversingRound ||
		audit.Verdict != domain.DriftVerdictOverHardened ||
		!slices.ContainsFunc(audit.Reversals, func(reversal domain.DriftReversal) bool {
			return reversal.FindingID == supersession.FindingID
		}) {
		return fmt.Errorf("drift audit: %w", domain.ErrParentKeyMismatch)
	}
	return nil
}

// validateDispositionSupersessionAuthority re-proves who ordered the reversal
// against current state, and that the route gate still derives the reversal
// itself. Both kinds need the reversal list to pass the gate for the reversing
// round, which is what proves the superseded row was the finding's effective
// latest disposition and that the finding is outside the round's own batch.
//
// An automatic route stores no reference: the gate must report that the round
// routes automatically. A human authority must be the stored
// continue_under_policy command on the reversing round's diminishing-returns
// item, at the item version it was issued against, and that item must have
// parked on this drift audit with the promise that continuing runs the
// simplification round. A continue on any other stop, or on a card without
// the promise, orders an ordinary review round, never a reversal.
func (tx *ReadTx) validateDispositionSupersessionAuthority(
	ctx context.Context, supersession domain.FindingDispositionSupersession,
) error {
	// The switch dispatches on the kind, so it omits default.
	switch supersession.Authority.Kind {
	case domain.DispositionSupersessionAutoRoute:
		return tx.validateDispositionSupersessionRoute(ctx, supersession, true)
	case domain.DispositionSupersessionHumanCommand:
		// Validate guarantees the reference for this kind.
		reference := supersession.Authority.Command
		decision, err := tx.ReviewDiminishingDecision(ctx, reference.ItemID)
		if err != nil {
			return supersessionBindingFailure("authority item", err)
		}
		if decision.Binding.RunID != supersession.RunID ||
			decision.Binding.Round != supersession.ReversingRound ||
			decision.Binding.Cause != domain.ReviewDiminishingDriftAudit ||
			decision.Command == nil ||
			decision.Command.Action != domain.ActionContinueUnderPolicy ||
			decision.Command.CommandID != reference.CommandID ||
			decision.Command.ItemVersion != reference.ItemVersion {
			return fmt.Errorf("authority command: %w", domain.ErrParentKeyMismatch)
		}
		// The command orders a reversal only when the card it decided promised
		// one for this audit. A continue on a card that promised nothing orders
		// an ordinary review round.
		facts := decision.Item.ReviewDiminishing
		if facts == nil || facts.DriftAudit == nil || !facts.DriftAudit.SimplificationOnContinue ||
			facts.DriftAudit.AuditDigest != supersession.DriftAuditDigest {
			return fmt.Errorf("authority command promised no simplification round: %w",
				ErrSupersessionRouteUnproven)
		}
		return tx.validateDispositionSupersessionRoute(ctx, supersession, false)
	}
	return domain.ErrInvalidSupersessionAuthorityKind
}

// validateDispositionSupersessionRoute re-derives the route gate for the
// record's reversing round. automatic also requires that the round routes
// without a human.
func (tx *ReadTx) validateDispositionSupersessionRoute(
	ctx context.Context, supersession domain.FindingDispositionSupersession, automatic bool,
) error {
	gate, err := tx.DriftAuditRouteGate(ctx, supersession.RunID, supersession.ReversingRound)
	if err != nil {
		return supersessionBindingFailure("route gate", err)
	}
	if gate.Audit.Digest != supersession.DriftAuditDigest {
		return fmt.Errorf("route gate audit: %w", domain.ErrParentKeyMismatch)
	}
	if !gate.ListValid || (automatic && !gate.Auto) {
		return ErrSupersessionRouteUnproven
	}
	return nil
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
// on, and each stored item's stop cause is re-derived through the records of
// earlier rounds. A record that appeared for a round the run has already moved past
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

// decodeDispositionSupersessionRow re-runs the body integrity digest, the
// decode with the record's validation, and the agreement of the copied keys
// with the decoded body. It follows no join.
func decodeDispositionSupersessionRow(
	row dispositionSupersessionRow,
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
	return supersession, nil
}

// reconstructDispositionSupersession is the one rebuild path for every public
// read. It decodes the row and re-runs the same scope and authority checks a
// write runs. No copied column and no stored round, invocation, digest, or
// command reference is trusted.
func (tx *ReadTx) reconstructDispositionSupersession(
	ctx context.Context, row dispositionSupersessionRow,
) (domain.FindingDispositionSupersession, error) {
	supersession, err := decodeDispositionSupersessionRow(row)
	if err != nil {
		return domain.FindingDispositionSupersession{}, err
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
	raw, err := tx.dispositionSupersessionRows(ctx)
	if err != nil {
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

func (tx *ReadTx) dispositionSupersessionRows(ctx context.Context) ([]dispositionSupersessionRow, error) {
	rows, err := tx.tx.QueryContext(ctx,
		`SELECT `+selectDispositionSupersessionColumns+` FROM finding_disposition_supersessions
			ORDER BY run_id, reversing_round, finding_id, superseded_round`)
	if err != nil {
		return nil, err
	}
	var raw []dispositionSupersessionRow
	for rows.Next() {
		row, err := scanDispositionSupersessionRow(rows)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("row %d: %w", len(raw)+1, err)
		}
		raw = append(raw, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	return raw, nil
}

// loadDispositionSupersessionsAtDecision returns the run's records whose
// reversing round is before boundaryRound, checked against dispositions, the
// run's rows before that round as loadFindingDispositionsAtDecision returned
// them. It is the read for a decision-time rebuild (the recurrence rule and
// the route gate), and differs from the public reads in two ways.
//
// It checks a record's scope and never follows its authority. A human
// authority names the diminishing-returns item of the record's own reversing
// round, and loading that item rebuilds its convergence state; the rebuild of a
// later round reads this record, so following the authority here would recurse
// through every reversing round. The authority is proven when the record is
// written and on every public read.
//
// It takes the superseded disposition from the caller's slice for the same
// reason: a superseded row is always older than its reversing round, so it is
// in the slice, and a record whose row is missing fails closed.
//
// Every row is decoded and cross-checked before the run and round filter, so
// a corrupted copied key cannot move a record out of the result.
func (tx *ReadTx) loadDispositionSupersessionsAtDecision(
	ctx context.Context,
	runID domain.RunID,
	boundaryRound int,
	dispositions []domain.ReviewDispositionRecord,
) ([]domain.FindingDispositionSupersession, error) {
	raw, err := tx.dispositionSupersessionRows(ctx)
	if err != nil {
		return nil, fmt.Errorf("disposition supersessions %q before round %d: %w", runID, boundaryRound, err)
	}
	var out []domain.FindingDispositionSupersession
	for i, row := range raw {
		fail := func(err error) error {
			return fmt.Errorf("disposition supersessions %q before round %d row %d: %w",
				runID, boundaryRound, i+1, err)
		}
		supersession, err := decodeDispositionSupersessionRow(row)
		if err != nil {
			return nil, fail(err)
		}
		if supersession.RunID != runID || supersession.ReversingRound >= boundaryRound {
			continue
		}
		index := slices.IndexFunc(dispositions, func(disposition domain.ReviewDispositionRecord) bool {
			return disposition.FindingID == supersession.FindingID &&
				disposition.Round == supersession.SupersededRound
		})
		if index < 0 {
			return nil, fail(fmt.Errorf("superseded disposition: %w", domain.ErrParentKeyMismatch))
		}
		audit, err := tx.GetDriftAudit(ctx, supersession.DriftAuditDigest)
		if err != nil {
			return nil, fail(supersessionBindingFailure("drift audit", err))
		}
		if err := checkDispositionSupersessionScope(supersession, dispositions[index], audit); err != nil {
			return nil, fail(err)
		}
		out = append(out, supersession)
	}
	return out, nil
}
