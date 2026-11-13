package store

import (
	"context"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const insertWorkflowPolicyRevisionSQL = `
INSERT INTO workflow_policy_revisions (revision, scope, project_id, key, set_by, set_at, body)
VALUES (?, ?, ?, ?, ?, ?, ?)`

// AppendWorkflowPolicyRevision records the next workflow policy revision. The
// store assigns the revision here, numbering every layer's changes in one
// sequence. It is on WriteTx because the layers are client-visible state, and
// such a change must ride a revision bump.
func (tx *WriteTx) AppendWorkflowPolicyRevision(
	ctx context.Context, in domain.WorkflowPolicyInput, setAt time.Time,
) (domain.WorkflowPolicyRevision, error) {
	var last int
	if err := tx.tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(revision), 0) FROM workflow_policy_revisions`).Scan(&last); err != nil {
		return domain.WorkflowPolicyRevision{}, fmt.Errorf("append workflow policy revision: %w", err)
	}
	revision, err := domain.NewWorkflowPolicyRevision(in, last+1, setAt)
	if err != nil {
		return domain.WorkflowPolicyRevision{}, fmt.Errorf("append workflow policy revision: %w", err)
	}
	body, err := encode(revision)
	if err != nil {
		return domain.WorkflowPolicyRevision{}, fmt.Errorf("append workflow policy revision: %w", err)
	}
	var project string
	if revision.ProjectID != nil {
		project = string(*revision.ProjectID)
	}
	if _, err := tx.tx.ExecContext(ctx, insertWorkflowPolicyRevisionSQL, revision.Revision, revision.Scope,
		project, revision.Key, revision.SetBy, formatTime(revision.SetAt), body); err != nil {
		return domain.WorkflowPolicyRevision{}, fmt.Errorf("append workflow policy revision: %w", err)
	}
	return revision, nil
}

// WorkflowPolicyLayers folds every revision into the current value of each
// layer. None set is empty layers at revision 0, where every key resolves to
// its default. A row that fails its decode fails the read: a bad row is a bug.
func (tx *ReadTx) WorkflowPolicyLayers(ctx context.Context) (domain.WorkflowPolicyLayers, error) {
	revisions, err := queryAll(ctx, tx.tx, scanWorkflowPolicyRevision,
		`SELECT body FROM workflow_policy_revisions ORDER BY revision`)
	if err != nil {
		return domain.WorkflowPolicyLayers{}, fmt.Errorf("workflow policy layers: %w", err)
	}
	layers, err := domain.NewWorkflowPolicyLayers(revisions)
	if err != nil {
		return domain.WorkflowPolicyLayers{}, fmt.Errorf("workflow policy layers: %w", err)
	}
	return layers, nil
}

// scanWorkflowPolicyRevision decodes one row's body, which Validate checks on
// decode. The row is ordinary state (Tier B): its columns are not compared
// with the body.
func scanWorkflowPolicyRevision(row scanner) (domain.WorkflowPolicyRevision, error) {
	var body []byte
	if err := row.Scan(&body); err != nil {
		return domain.WorkflowPolicyRevision{}, err
	}
	return decode[domain.WorkflowPolicyRevision](body)
}
