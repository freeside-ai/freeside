package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// ResolveWorkflowPolicyKeys overlays the store's workflow policy layers on a
// project's base keys at task creation: every key the Workflow pane exposes
// resolves project over general over default, with the winning layer as its
// provenance, and never from base; every other base key passes through. A
// run already created keeps the policy it was resolved with, since a
// successor copies its parent's keys.
func ResolveWorkflowPolicyKeys(
	ctx context.Context, tx *store.ReadTx, project domain.ProjectID, base []domain.PolicyKey,
) ([]domain.PolicyKey, error) {
	layers, err := tx.WorkflowPolicyLayers(ctx)
	if err != nil {
		return nil, err
	}
	return layers.Overlay(project, base), nil
}

// SetWorkflowPolicyRequest is one operator change to one layer of one
// workflow policy key; a nil Value clears the layer. The caller has checked
// that the daemon serves a project scope's project.
type SetWorkflowPolicyRequest struct {
	Scope     domain.WorkflowPolicyScope
	ProjectID *domain.ProjectID
	Key       domain.WorkflowPolicyKey
	Value     *string
	SetBy     string
}

// SetWorkflowPolicyResult reports a change. Recorded is false when the layer
// already held the value asked for. Revision is the workflow policy revision
// current after the request.
type SetWorkflowPolicyResult struct {
	Recorded bool
	Revision int
}

// SetWorkflowPolicyTx records a new revision for the layer inside the
// caller's transaction, so a command commits its receipt with it, unless the
// layer already holds the value, when it returns Recorded false and records
// nothing. A malformed request is refused by the domain's validation.
func SetWorkflowPolicyTx(
	ctx context.Context, tx *store.WriteTx, req SetWorkflowPolicyRequest, now time.Time,
) (SetWorkflowPolicyResult, error) {
	layers, err := tx.WorkflowPolicyLayers(ctx)
	if err != nil {
		return SetWorkflowPolicyResult{}, err
	}
	held, set := layers.Layer(req.Scope, req.ProjectID, req.Key)
	if set == (req.Value != nil) && (!set || *held.Value == *req.Value) {
		return SetWorkflowPolicyResult{Revision: layers.Revision()}, nil
	}
	revision, err := tx.AppendWorkflowPolicyRevision(ctx, domain.WorkflowPolicyInput{
		Scope: req.Scope, ProjectID: req.ProjectID, Key: req.Key, Value: req.Value, SetBy: req.SetBy,
	}, now)
	if err != nil {
		return SetWorkflowPolicyResult{}, fmt.Errorf("set workflow policy: %w", err)
	}
	return SetWorkflowPolicyResult{Recorded: true, Revision: revision.Revision}, nil
}
