package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// systemHealthVersion names the read's JSON shape; a shape change changes it.
const systemHealthVersion = "freeside-system-health-v1"

// systemHealth is what `freesided inspect health` prints: every open
// system_health item on the root and whether each holds unattended admission
// now, read under one store revision. No other read lists them: the daemon's
// stop notices name no task, so a task's record never shows them.
//
// OperatorStop is null when no stop is in force, and also when the gate could
// not be evaluated, which AdmissionError then says.
type systemHealth struct {
	Version        string                   `json:"version"`
	AsOfRevision   int64                    `json:"as_of_revision"`
	AsOf           time.Time                `json:"as_of"`
	OperatorStop   *systemHealthStop        `json:"operator_stop"`
	AdmissionError *taskRecordError         `json:"admission_error"`
	Items          []systemHealthItem       `json:"items"`
	RefusedRows    []systemHealthRefusedRow `json:"refused_rows"`
}

type systemHealthStop struct {
	CommandID  *string   `json:"command_id"`
	Reason     string    `json:"reason"`
	OccurredAt time.Time `json:"occurred_at"`
}

// systemHealthItem leaves out the item's reason, evidence, and claims, as
// taskRecordItem does and for the same cause; the diagnostic says why the
// item exists.
//
// Admission is null exactly when the read could not say whether the item
// holds: AdmissionError on the item, or on the read when the gate itself
// failed, says why. HoldReason is the gate's refusal when Admission is holds.
type systemHealthItem struct {
	ID                domain.ItemID                `json:"id"`
	ProjectID         domain.ProjectID             `json:"project_id"`
	Subject           domain.Subject               `json:"subject"`
	Posture           domain.HealthPosture         `json:"posture"`
	HealthDiagnostic  *domain.HealthDiagnostic     `json:"health_diagnostic"`
	RequestedDecision []domain.Action              `json:"requested_decision"`
	CreatedAt         *time.Time                   `json:"created_at"`
	Supersession      *domain.BlockingSupersession `json:"supersession"`
	Admission         *systemHealthAdmission       `json:"admission"`
	HoldReason        *string                      `json:"hold_reason"`
	AdmissionError    *taskRecordError             `json:"admission_error"`
}

// systemHealthRefusedRow is a row the store refused to reconstruct. Its type
// column is unverified, so every refused row is listed, whatever its type.
type systemHealthRefusedRow struct {
	ID    domain.ItemID    `json:"id"`
	Error *taskRecordError `json:"error"`
}

// systemHealthAdmission says whether an open system_health item holds
// unattended admission, and if not, which of the gate's exemptions applies.
type systemHealthAdmission string

const (
	// systemHealthHolds: the gate lists the item, so it holds admission now.
	systemHealthHolds systemHealthAdmission = "holds"
	// systemHealthAdvisory: the item's posture never holds admission.
	systemHealthAdvisory systemHealthAdmission = "advisory"
	// systemHealthSuperseded: a blocking item whose supersession condition
	// the gate found met.
	systemHealthSuperseded systemHealthAdmission = "superseded"
	// systemHealthWorkflowClosed: a blocking item of a closed task workflow,
	// which the gate skips.
	systemHealthWorkflowClosed systemHealthAdmission = "workflow_closed"
)

var allSystemHealthAdmissions = []systemHealthAdmission{
	systemHealthHolds, systemHealthAdvisory, systemHealthSuperseded, systemHealthWorkflowClosed,
}

func (a systemHealthAdmission) valid() bool {
	switch a {
	case systemHealthHolds, systemHealthAdvisory, systemHealthSuperseded, systemHealthWorkflowClosed:
		return true
	default:
		return false
	}
}

// hasErrorKind reports whether a refused row was refused by an error of the
// given kind.
func (h systemHealth) hasErrorKind(kind taskRecordErrorKind) bool {
	for _, row := range h.RefusedRows {
		if row.Error.Kind == kind {
			return true
		}
	}
	return false
}

func readSystemHealthFrom(ctx context.Context, st *store.Store) (systemHealth, error) {
	var health systemHealth
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		health, err = readSystemHealth(ctx, tx, time.Now().UTC())
		return err
	})
	return health, err
}

// readSystemHealth builds the read in the caller's transaction from the
// store's own reads, so what it says holds is what admission enforces at
// that revision. Only a failed enumeration fails it: a refused row and an
// unevaluable gate are reported in place.
func readSystemHealth(ctx context.Context, tx *store.ReadTx, asOf time.Time) (systemHealth, error) {
	state, err := tx.ServerState(ctx)
	if err != nil {
		return systemHealth{}, err
	}
	rows, err := tx.ListAttentionItemRows(ctx)
	if err != nil {
		return systemHealth{}, err
	}
	health := systemHealth{
		Version: systemHealthVersion, AsOfRevision: state.Revision, AsOf: asOf,
		Items: []systemHealthItem{}, RefusedRows: []systemHealthRefusedRow{},
	}
	gate, gateErr := tx.UnattendedOperationGate(ctx)
	if gateErr != nil {
		health.AdmissionError = newTaskRecordError(gateErr)
	} else if stop := gate.OperatorStop; stop != nil {
		health.OperatorStop = &systemHealthStop{CommandID: stop.CommandID, Reason: stop.Reason, OccurredAt: stop.OccurredAt}
	}
	for _, row := range rows {
		if row.Err != nil {
			health.RefusedRows = append(health.RefusedRows, systemHealthRefusedRow{ID: row.ID, Error: newTaskRecordError(row.Err)})
			continue
		}
		item := *row.Item
		if item.Type != domain.AttentionSystemHealth || item.Status != domain.StatusOpen {
			continue
		}
		out := systemHealthItem{
			ID: item.ID, ProjectID: item.ProjectID, Subject: item.Subject, Posture: *item.Posture,
			HealthDiagnostic: item.HealthDiagnostic, RequestedDecision: item.RequestedDecision,
			CreatedAt: item.CreatedAt, Supersession: item.BlockingSupersession,
		}
		if gateErr == nil {
			admission, reason, err := classifySystemHealthItem(gate, item, func() error {
				return tx.RequireItemWorkflowOpen(ctx, item)
			})
			if err != nil {
				out.AdmissionError = newTaskRecordError(err)
			} else {
				out.Admission, out.HoldReason = &admission, reason
			}
		}
		health.Items = append(health.Items, out)
	}
	return health, nil
}

// errSystemHealthUnexplained is an item the gate should have listed and did
// not. In one transaction it cannot happen; the read reports it rather than
// guess which exemption applied.
var errSystemHealthUnexplained = errors.New("blocking item in an open workflow with no supersession, but the gate does not list it")

// classifySystemHealthItem says why the gate listed an open system_health
// item or passed over it. The gate's verdict decides holds; the exemptions
// are read in the cheapest order, since an item the gate did not list met
// one of them. workflowOpen is RequireItemWorkflowOpen for the item, called
// only for a blocking item the gate passed over.
func classifySystemHealthItem(
	gate domain.UnattendedOperationGate, item domain.AttentionItem, workflowOpen func() error,
) (systemHealthAdmission, *string, error) {
	for _, blocking := range gate.Blocking {
		if blocking.Item.ID == item.ID {
			reason := blocking.Err.Error()
			return systemHealthHolds, &reason, nil
		}
	}
	if *item.Posture == domain.HealthPostureAdvisory {
		return systemHealthAdvisory, nil, nil
	}
	switch err := workflowOpen(); {
	case errors.Is(err, store.ErrTaskWorkflowClosed):
		return systemHealthWorkflowClosed, nil, nil
	case err != nil:
		return "", nil, err
	}
	if item.BlockingSupersession != nil {
		return systemHealthSuperseded, nil, nil
	}
	return "", nil, fmt.Errorf("item %q: %w", item.ID, errSystemHealthUnexplained)
}
