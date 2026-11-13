package signet

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// SetWorkflowPolicyPayload mirrors the API's SetWorkflowPolicyPayload: one
// layer of one workflow policy key. ProjectID is set exactly for the project
// scope, a nil Value clears the layer, and a nil ExpectedRevision says the
// client saw no revision.
type SetWorkflowPolicyPayload struct {
	Scope            domain.WorkflowPolicyScope `json:"scope"`
	ProjectID        *domain.ProjectID          `json:"project_id"`
	Key              domain.WorkflowPolicyKey   `json:"key"`
	Value            *string                    `json:"value"`
	ExpectedRevision *int                       `json:"expected_revision"`
}

// WorkflowPolicyChange is one layer change for the WorkflowPolicySetter. Its
// shape is valid and a project scope names a project the daemon serves.
type WorkflowPolicyChange struct {
	Scope     domain.WorkflowPolicyScope
	ProjectID *domain.ProjectID
	Key       domain.WorkflowPolicyKey
	Value     *string
	SetBy     string
}

// WorkflowPolicyOutcome is the workflow policy revision current after the
// change. Recorded is false when the layer already held the value.
type WorkflowPolicyOutcome struct {
	Recorded bool
	Revision int
}

// WorkflowPolicySetter applies a layer change in the accepting transaction
// (engine.SetWorkflowPolicyTx, bound by WithWorkflowPolicySetter).
type WorkflowPolicySetter func(
	ctx context.Context, tx *store.WriteTx, change WorkflowPolicyChange, now time.Time,
) (WorkflowPolicyOutcome, error)

func WithWorkflowPolicySetter(setter WorkflowPolicySetter) Option {
	return func(s *Service) { s.workflowPolicySetter = setter }
}

// WithWorkflowPolicyProjects names the projects the daemon serves: the
// bootstrap projection lists one section for each, and a set_workflow_policy
// at project scope must name one of them. None leaves only the general
// layer settable.
func WithWorkflowPolicyProjects(projects []domain.ProjectID) Option {
	return func(s *Service) { s.workflowPolicyProjects = slices.Clone(projects) }
}

// WorkflowPolicyRejectionError refuses a change prepared against a revision
// that is no longer current, with the current projection.
type WorkflowPolicyRejectionError struct {
	CurrentWorkflowPolicy WorkflowPolicySnapshot `json:"current_workflow_policy"`
}

func (*WorkflowPolicyRejectionError) Error() string { return "the workflow policy changed" }

// WorkflowPolicySnapshot is the workflow policy layers projected for the
// client, matching api/openapi.yaml: each exposed key's effective value for
// the general section and for every project the daemon serves. Revision is
// the newest workflow policy revision, 0 when none was ever set, and every
// change advances it.
type WorkflowPolicySnapshot struct {
	AsOfRevision int64                       `json:"as_of_revision"`
	Revision     int                         `json:"revision"`
	General      []WorkflowPolicySettingWire `json:"general"`
	Projects     []WorkflowPolicyProjectWire `json:"projects"`
}

// WorkflowPolicyProjectWire is one served project's effective settings.
type WorkflowPolicyProjectWire struct {
	ProjectID domain.ProjectID            `json:"project_id"`
	Settings  []WorkflowPolicySettingWire `json:"settings"`
}

// WorkflowPolicySettingWire is one key's effective value and the layer it
// came from. SetBy and SetAt name the revision that set the winning layer,
// nil for the default.
type WorkflowPolicySettingWire struct {
	Key    domain.WorkflowPolicyKey    `json:"key"`
	Value  string                      `json:"value"`
	Origin domain.WorkflowPolicyOrigin `json:"origin"`
	SetBy  *string                     `json:"set_by"`
	SetAt  *time.Time                  `json:"set_at"`
}

func renderWorkflowPolicySetting(setting domain.WorkflowPolicySetting) WorkflowPolicySettingWire {
	wire := WorkflowPolicySettingWire{Key: setting.Key, Value: setting.Value, Origin: setting.Origin}
	if setting.Revision != nil {
		wire.SetBy = &setting.Revision.SetBy
		wire.SetAt = &setting.Revision.SetAt
	}
	return wire
}

// workflowPolicySnapshot projects the layers read in tx at the given server
// revision.
func (s *Service) workflowPolicySnapshot(
	ctx context.Context, tx *store.ReadTx, asOfRevision int64,
) (WorkflowPolicySnapshot, error) {
	layers, err := tx.WorkflowPolicyLayers(ctx)
	if err != nil {
		return WorkflowPolicySnapshot{}, err
	}
	return renderWorkflowPolicy(layers, s.workflowPolicyProjects, asOfRevision), nil
}

// renderWorkflowPolicy projects layers for the general section and each
// served project, with the domain's one precedence rule.
func renderWorkflowPolicy(
	layers domain.WorkflowPolicyLayers, projects []domain.ProjectID, asOfRevision int64,
) WorkflowPolicySnapshot {
	snapshot := WorkflowPolicySnapshot{
		AsOfRevision: asOfRevision, Revision: layers.Revision(),
		General:  make([]WorkflowPolicySettingWire, 0, len(domain.AllWorkflowPolicyKeys)),
		Projects: make([]WorkflowPolicyProjectWire, 0, len(projects)),
	}
	for _, key := range domain.AllWorkflowPolicyKeys {
		snapshot.General = append(snapshot.General, renderWorkflowPolicySetting(layers.GeneralSetting(key)))
	}
	for _, project := range projects {
		wire := WorkflowPolicyProjectWire{
			ProjectID: project, Settings: make([]WorkflowPolicySettingWire, 0, len(domain.AllWorkflowPolicyKeys)),
		}
		for _, key := range domain.AllWorkflowPolicyKeys {
			wire.Settings = append(wire.Settings, renderWorkflowPolicySetting(layers.ProjectSetting(project, key)))
		}
		snapshot.Projects = append(snapshot.Projects, wire)
	}
	return snapshot
}

func (s *Service) setWorkflowPolicy(ctx context.Context, in ClientCommand) (CommandResult, error) {
	p := in.SetWorkflowPolicy
	request := domain.WorkflowPolicyChangeRequest{
		CommandIdentity: domain.CommandIdentity{CommandID: in.CommandID, DeviceID: in.DeviceID},
		Scope:           p.Scope, ProjectID: p.ProjectID, Key: p.Key, Value: p.Value,
		ExpectedRevision: p.ExpectedRevision,
	}
	if !in.operatorArmOnly() {
		return CommandResult{}, fmt.Errorf("submit command %q: %w", in.CommandID, ErrInvalidOperatorCommandPayload)
	}
	if err := request.Validate(); err != nil {
		return CommandResult{}, fmt.Errorf("submit command %q: %w: %w", in.CommandID, ErrInvalidOperatorCommandPayload, err)
	}
	return s.commitReceipt(ctx, in,
		func(old domain.CommandReceipt) bool {
			return old.WorkflowPolicyChange != nil && old.WorkflowPolicyChange.Same(request)
		},
		func(tx *store.WriteTx, now time.Time) (domain.CommandReceipt, error) {
			if s.workflowPolicySetter == nil {
				return domain.CommandReceipt{}, ErrOperatorSettingUnavailable
			}
			// Checked only for a new command, so a replay still answers after
			// the daemon stops serving the project. The error never repeats
			// the id, which is request text.
			if p.ProjectID != nil && !slices.Contains(s.workflowPolicyProjects, *p.ProjectID) {
				return domain.CommandReceipt{}, domain.ErrWorkflowPolicyProjectUnserved
			}
			layers, err := tx.WorkflowPolicyLayers(ctx)
			if err != nil {
				return domain.CommandReceipt{}, err
			}
			expected := 0
			if p.ExpectedRevision != nil {
				expected = *p.ExpectedRevision
			}
			if layers.Revision() != expected {
				return domain.CommandReceipt{}, s.workflowPolicyChanged(ctx, tx)
			}
			outcome, err := s.workflowPolicySetter(ctx, tx, WorkflowPolicyChange{
				Scope: p.Scope, ProjectID: p.ProjectID, Key: p.Key, Value: p.Value,
				SetBy: "device:" + string(in.DeviceID),
			}, now)
			if err != nil {
				return domain.CommandReceipt{}, err
			}
			return domain.CommandReceipt{WorkflowPolicyChange: &domain.WorkflowPolicyChangeReceipt{
				WorkflowPolicyChangeRequest: request, Recorded: outcome.Recorded, Revision: outcome.Revision,
			}}, nil
		})
}

// workflowPolicyChanged builds the 409 for a stale expected revision, with the
// projection as of the last committed revision: the command's transaction
// rolls back.
func (s *Service) workflowPolicyChanged(ctx context.Context, tx *store.WriteTx) error {
	state, err := tx.ServerState(ctx)
	if err != nil {
		return err
	}
	snapshot, err := s.workflowPolicySnapshot(ctx, &tx.ReadTx, state.Revision)
	if err != nil {
		return err
	}
	return &WorkflowPolicyRejectionError{CurrentWorkflowPolicy: snapshot}
}
