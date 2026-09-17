package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// GetOrCreateTask registers an intake key and a random identity in this write
// transaction. The source determines deduplication; it never determines the ID.
func (tx *WriteTx) GetOrCreateTask(ctx context.Context, projectID domain.ProjectID, source domain.SpecificationSource) (domain.Task, error) {
	key, err := tx.taskIntakeKey(ctx, source)
	if err != nil {
		return domain.Task{}, err
	}
	return tx.getOrCreateTask(ctx, projectID, key, &source, time.Now().UTC())
}

func (tx *ReadTx) taskIntakeKey(ctx context.Context, source domain.SpecificationSource) (string, error) {
	if err := source.Validate(); err != nil {
		return "", err
	}
	switch source.Kind {
	case domain.SpecificationSourceWorkItemArtifact:
		artifact, err := tx.GetArtifact(ctx, source.WorkItemArtifactID)
		if err != nil {
			return "", err
		}
		if artifact.Type != domain.ArtifactKindSpecification {
			return "", domain.ErrParentKeyMismatch
		}
		return "source:" + string(artifact.Digest), nil
	case domain.SpecificationSourceIssueSubject:
		return fmt.Sprintf("issue:%d:%d", source.IssueSubject.RepositoryID, source.IssueSubject.IssueNumber), nil
	}
	return "", domain.ErrInvalidSpecificationSourceKind
}

func (tx *WriteTx) getOrCreateTask(ctx context.Context, projectID domain.ProjectID, key string, source *domain.SpecificationSource, createdAt time.Time) (domain.Task, error) {
	id := domain.TaskID("task-" + rand.Text())
	result, err := tx.tx.ExecContext(ctx, `INSERT INTO task_intake_keys (project_id, intake_key, task_id)
		VALUES (?, ?, ?) ON CONFLICT (project_id, intake_key) DO NOTHING`, projectID, key, id)
	if err != nil {
		return domain.Task{}, fmt.Errorf("register task intake: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return domain.Task{}, err
	}
	if inserted != 0 {
		task := domain.Task{
			ID: id, ProjectID: projectID, Source: source,
			Name:      domain.DisplayName{Text: string(id), Source: domain.DisplayNameSourceIdentifier},
			CreatedAt: createdAt, CampaignIDs: []domain.CampaignID{},
		}
		body, err := encode(task)
		if err != nil {
			return domain.Task{}, err
		}
		if _, err := tx.tx.ExecContext(ctx, `INSERT INTO tasks (id, project_id, entity_version, as_of_revision, body)
			VALUES (?, ?, 1, ?, ?)`, id, projectID, tx.asOfRevision, body); err != nil {
			return domain.Task{}, err
		}
	}
	if err := tx.tx.QueryRowContext(ctx, `SELECT task_id FROM task_intake_keys WHERE project_id = ? AND intake_key = ?`, projectID, key).Scan(&id); err != nil {
		return domain.Task{}, err
	}
	return tx.GetTask(ctx, id)
}

func (tx *ReadTx) GetTask(ctx context.Context, id domain.TaskID) (domain.Task, error) {
	snapshot, err := tx.GetTaskSnapshot(ctx, id)
	return snapshot.Value, err
}

// GetTaskByIntakeKey returns the task registered under a project-scoped intake
// key, or a wrapped ErrNotFound when none is. It is the client task-submission
// path's idempotency guard: the same source in one project fetches the same
// task, so a second submission records the existing task and starts no second
// run. The key is derived by the caller (for a submitted source, "source:"
// plus the source digest) to match taskIntakeKey.
func (tx *ReadTx) GetTaskByIntakeKey(ctx context.Context, projectID domain.ProjectID, key string) (domain.Task, error) {
	var id domain.TaskID
	if err := tx.tx.QueryRowContext(ctx,
		`SELECT task_id FROM task_intake_keys WHERE project_id = ? AND intake_key = ?`, projectID, key).Scan(&id); err != nil {
		return domain.Task{}, notFoundOr(err)
	}
	return tx.GetTask(ctx, id)
}

func (tx *ReadTx) GetTaskSnapshot(ctx context.Context, id domain.TaskID) (Snapshotted[domain.Task], error) {
	var projectID domain.ProjectID
	var body []byte
	var snapshot Snapshot
	if err := tx.tx.QueryRowContext(ctx, `SELECT project_id, entity_version, as_of_revision, body FROM tasks WHERE id = ?`, id).
		Scan(&projectID, &snapshot.EntityVersion, &snapshot.AsOfRevision, &body); err != nil {
		return Snapshotted[domain.Task]{}, notFoundOr(err)
	}
	task, err := decode[domain.Task](body)
	if err != nil {
		return Snapshotted[domain.Task]{}, err
	}
	if task.ID != id || task.ProjectID != projectID || snapshot.EntityVersion < 1 || snapshot.AsOfRevision < 1 {
		return Snapshotted[domain.Task]{}, errRowInconsistent
	}
	var key string
	if err := tx.tx.QueryRowContext(ctx, `SELECT intake_key FROM task_intake_keys WHERE task_id = ? AND project_id = ?`, id, projectID).Scan(&key); err != nil {
		return Snapshotted[domain.Task]{}, notFoundOr(err)
	}
	if task.Source != nil {
		want, err := tx.taskIntakeKey(ctx, *task.Source)
		if err != nil {
			return Snapshotted[domain.Task]{}, err
		}
		if identity, manual := strings.CutPrefix(key, "submission:"); manual {
			submission, err := tx.GetManualSubmission(ctx, identity)
			if err != nil {
				return Snapshotted[domain.Task]{}, err
			}
			if submission.ProjectID != projectID || task.Source.Kind != domain.SpecificationSourceWorkItemArtifact ||
				submission.SourceArtifactID != task.Source.WorkItemArtifactID || want != "source:"+string(submission.SourceDigest) {
				return Snapshotted[domain.Task]{}, errRowInconsistent
			}
		} else if key != want {
			return Snapshotted[domain.Task]{}, errRowInconsistent
		}
	} else if strings.HasPrefix(key, "submission:") {
		return Snapshotted[domain.Task]{}, errRowInconsistent
	}
	// Lifecycle facts live in their own table, not the body. Load them and
	// re-validate the assembled task: a bad log fails closed at reconstruction
	// (the trust-boundary rule in entities.go; issue #1318 D1).
	facts, err := tx.taskLifecycleFacts(ctx, id)
	if err != nil {
		return Snapshotted[domain.Task]{}, err
	}
	task.LifecycleFacts = facts
	task.Cancellation, err = tx.currentTaskCancellation(ctx, id)
	if err != nil {
		return Snapshotted[domain.Task]{}, err
	}
	if err := task.Validate(); err != nil {
		return Snapshotted[domain.Task]{}, err
	}
	return Snapshotted[domain.Task]{Value: task, Snapshot: snapshot}, nil
}

func (tx *ReadTx) taskLifecycleFacts(ctx context.Context, id domain.TaskID) ([]domain.TaskLifecycleFact, error) {
	rows, err := tx.tx.QueryContext(ctx, `SELECT ordinal, kind, run_id, campaign_id, binding_unit_id, source_id, recorded_at
		FROM task_lifecycle_facts WHERE task_id = ? ORDER BY ordinal`, id)
	if err != nil {
		// The table is introduced in migration 0072; a task read during an
		// earlier migration's data step (backfillTasks calls GetTask) predates
		// it and therefore has no facts. Fall back only when the table is
		// genuinely absent, never to mask a real query failure.
		if exists, existsErr := tx.tableExists(ctx, "task_lifecycle_facts"); existsErr == nil && !exists {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	facts := []domain.TaskLifecycleFact{}
	for rows.Next() {
		var f domain.TaskLifecycleFact
		var campaign, binding sql.NullString
		var recordedAt string
		if err := rows.Scan(&f.Ordinal, &f.Kind, &f.RunID, &campaign, &binding, &f.SourceID, &recordedAt); err != nil {
			return nil, err
		}
		if campaign.Valid {
			c := domain.CampaignID(campaign.String)
			f.CampaignID = &c
		}
		if binding.Valid {
			b := domain.WorkUnitID(binding.String)
			f.BindingUnitID = &b
		}
		t, err := parseTime(recordedAt)
		if err != nil {
			return nil, err
		}
		f.RecordedAt = t
		facts = append(facts, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(facts) > 0 {
		if err := tx.regateTaskLifecycleFacts(ctx, id, facts); err != nil {
			return nil, err
		}
	}
	return facts, nil
}

// regateTaskLifecycleFacts corroborates each decoded fact against the
// authoritative tables before the WIP derivation trusts it: a current-campaign
// completion or an abandonment releases a WIP admission slot, so a tampered or
// corrupt lifecycle row is a decoded trust bit that must fail closed at
// reconstruction (the trust-boundary rule in entities.go; issue #1318 D1).
// Three corroborations, each failing closed on mismatch:
//   - Every fact's run must be one of the task's own runs.
//   - A fact's campaign (which drives WIP currency: the newest start's campaign
//     and a completion's currency, issue #1318 D4/G2) must equal that run's
//     persisted campaign, so a tampered fact cannot fabricate currency.
//   - A completion's binding must resolve, through work_unit_declarations, to a
//     recorded completion of the fact's own run (unit_id = "workunit-"+run); a
//     mere globally-existing completion of some other run or task is rejected.
//
// An abandonment carries no campaign and no binding, so only its run membership
// is checked. Legitimate facts always corroborate: RecordTaskStart and
// RecordTaskCompletion write only for the task's own run with that run's
// campaign, and a completion row is written with or before its fact.
func (tx *ReadTx) regateTaskLifecycleFacts(ctx context.Context, id domain.TaskID, facts []domain.TaskLifecycleFact) error {
	rows, err := tx.tx.QueryContext(ctx, `SELECT tr.run_id, r.campaign_id
		FROM task_runs tr JOIN runs r ON r.id = tr.run_id WHERE tr.task_id = ?`, id)
	if err != nil {
		return err
	}
	runCampaigns := map[domain.RunID]sql.NullString{}
	for rows.Next() {
		var runID domain.RunID
		var campaign sql.NullString
		if err := rows.Scan(&runID, &campaign); err != nil {
			_ = rows.Close()
			return err
		}
		runCampaigns[runID] = campaign
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, f := range facts {
		runCampaign, ok := runCampaigns[f.RunID]
		if !ok {
			return errRowInconsistent
		}
		// A started or completed fact's campaign drives WIP currency (the newest
		// start's campaign and a completion's currency), so it must match the run's
		// persisted campaign exactly, including nullability: clearing the campaign
		// to NULL would otherwise let two facts on different campaigns compare equal
		// as both-nil and release the slot (issue #1318 G3/H3). An abandonment
		// carries no campaign and releases unconditionally, so only its run
		// membership is checked.
		switch f.Kind {
		case domain.TaskLifecycleStarted, domain.TaskLifecycleCompleted:
			runHasCampaign := runCampaign.Valid && runCampaign.String != ""
			factHasCampaign := f.CampaignID != nil
			if runHasCampaign != factHasCampaign ||
				(factHasCampaign && runCampaign.String != string(*f.CampaignID)) {
				return errRowInconsistent
			}
		case domain.TaskLifecycleAbandoned:
		}
		if f.Kind != domain.TaskLifecycleCompleted {
			continue
		}
		if f.BindingUnitID == nil {
			return errRowInconsistent
		}
		var found int
		err := tx.tx.QueryRowContext(ctx, `SELECT 1 FROM work_unit_completions c
			JOIN work_unit_declarations d ON d.unit_id = c.unit_id
			WHERE c.unit_id = ? AND d.run_id = ?`, string(*f.BindingUnitID), string(f.RunID)).Scan(&found)
		if errors.Is(err, sql.ErrNoRows) {
			return errRowInconsistent
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// RecordTaskLifecycleFact appends one fact to the task's lifecycle log,
// idempotent on (task, source id) so a replayed event mints no twin and
// preserves recorded order (issue #1318 D1). The store assigns the next
// ordinal; the caller supplies the kind, producing run, optional campaign and
// binding, source id, and recorded time. It fails closed on a bad fact or an
// unknown task (the task_id foreign key).
func (tx *WriteTx) RecordTaskLifecycleFact(ctx context.Context, taskID domain.TaskID, fact domain.TaskLifecycleFact) error {
	check := fact
	check.Ordinal = 1 // the store assigns the real, contiguous ordinal below.
	if err := check.Validate(); err != nil {
		return fmt.Errorf("record task lifecycle fact: %w", err)
	}
	result, err := tx.tx.ExecContext(ctx, `INSERT INTO task_lifecycle_facts
		(task_id, ordinal, kind, run_id, campaign_id, binding_unit_id, source_id, recorded_at)
		SELECT ?, COALESCE(MAX(ordinal), 0) + 1, ?, ?, ?, ?, ?, ?
		FROM task_lifecycle_facts WHERE task_id = ?
		ON CONFLICT (task_id, source_id) DO NOTHING`,
		taskID, fact.Kind, fact.RunID, nullableCampaign(fact.CampaignID), nullableBinding(fact.BindingUnitID),
		fact.SourceID, formatTime(fact.RecordedAt), taskID)
	if err != nil {
		return fmt.Errorf("record task lifecycle fact: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if inserted == 0 {
		return nil // idempotent replay: the source id is already recorded.
	}
	// Advance the task row so its sync snapshot re-emits the changed WIP state.
	if _, err := tx.tx.ExecContext(ctx, `UPDATE tasks SET entity_version = entity_version + 1, as_of_revision = ? WHERE id = ?`, tx.asOfRevision, taskID); err != nil {
		return fmt.Errorf("advance task after lifecycle fact: %w", err)
	}
	return nil
}

// RecordTaskStart records an admitted workflow start for the run's task,
// unless the task already holds its WIP slot (issue #1318 D3): a spec run and
// its implementation run in one campaign share one start, and a retry or
// return on a still-WIP task records nothing. The source id is the run, so
// replay and a re-recorded submission converge. The campaign is the run's, so
// a later completion's currency is decidable from the log.
func (tx *WriteTx) RecordTaskStart(ctx context.Context, runID domain.RunID, recordedAt time.Time) error {
	run, err := tx.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if run.TaskID == "" {
		return nil
	}
	task, err := tx.GetTask(ctx, run.TaskID)
	if err != nil {
		return err
	}
	if domain.TaskWIP(task) {
		return nil
	}
	var campaign *domain.CampaignID
	if run.CampaignID != "" {
		c := run.CampaignID
		campaign = &c
	}
	return tx.RecordTaskLifecycleFact(ctx, run.TaskID, domain.TaskLifecycleFact{
		Kind: domain.TaskLifecycleStarted, RunID: run.ID, CampaignID: campaign,
		SourceID: "start:" + string(run.ID), RecordedAt: recordedAt.UTC(),
	})
}

// RecordTaskCompletion records a work-unit completion as a task lifecycle fact
// bound to the completed unit and its run's campaign (issue #1318 D3). The
// completion is always recorded, even against a superseded campaign; whether it
// releases the WIP slot is derived (D4). Idempotent on the completed unit.
func (tx *WriteTx) RecordTaskCompletion(ctx context.Context, runID domain.RunID, unitID domain.WorkUnitID, recordedAt time.Time) error {
	run, err := tx.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if run.TaskID == "" {
		return nil
	}
	var campaign *domain.CampaignID
	if run.CampaignID != "" {
		c := run.CampaignID
		campaign = &c
	}
	binding := unitID
	return tx.RecordTaskLifecycleFact(ctx, run.TaskID, domain.TaskLifecycleFact{
		Kind: domain.TaskLifecycleCompleted, RunID: run.ID, CampaignID: campaign,
		BindingUnitID: &binding, SourceID: "complete:" + string(unitID), RecordedAt: recordedAt.UTC(),
	})
}

// AbandonTask records an explicit abandonment for the task's current work
// episode, releasing its WIP slot (issue #1318 D6). The source id is derived
// from the newest recorded start, so a repeat abandonment of the same episode
// records nothing while an authorized restart can be abandoned again. A task
// with no recorded start holds no slot and records nothing. It reports whether
// the task was WIP before this call (i.e. whether a held slot was released).
func (tx *WriteTx) AbandonTask(ctx context.Context, taskID domain.TaskID, recordedAt time.Time) (bool, error) {
	task, err := tx.GetTask(ctx, taskID)
	if err != nil {
		return false, err
	}
	start := task.CurrentStart()
	if start == nil {
		return false, nil
	}
	held := domain.TaskWIP(task)
	fact := domain.TaskLifecycleFact{
		Kind: domain.TaskLifecycleAbandoned, RunID: start.RunID,
		SourceID: abandonSourceID(*start), RecordedAt: recordedAt.UTC(),
	}
	if err := tx.RecordTaskLifecycleFact(ctx, taskID, fact); err != nil {
		return false, err
	}
	return held, nil
}

// abandonSourceID ties an abandonment to the work episode its start opened, so
// an abandon records one idempotent fact per episode: repeating it is a no-op,
// while an authorized restart opens a new episode that can be abandoned again.
// The `freesided abandon` command (#1318 D6) is the caller today; a future
// no-artifact spec-run stop can reuse this sink once that path exists.
func abandonSourceID(start domain.TaskLifecycleFact) string {
	return "abandon:" + start.SourceID
}

// tableExists reports whether a table is present in the current schema. It
// lets a live reader used inside an earlier migration's data step tolerate a
// table that a later migration introduces.
func (tx *ReadTx) tableExists(ctx context.Context, name string) (bool, error) {
	var found string
	err := tx.tx.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func nullableCampaign(c *domain.CampaignID) any {
	if c == nil {
		return nil
	}
	return string(*c)
}

func nullableBinding(b *domain.WorkUnitID) any {
	if b == nil {
		return nil
	}
	return string(*b)
}

func (tx *ReadTx) ListTasks(ctx context.Context) ([]Snapshotted[domain.Task], error) {
	rows, err := tx.tx.QueryContext(ctx, `SELECT id FROM tasks ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var ids []domain.TaskID
	for rows.Next() {
		var id domain.TaskID
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	out := make([]Snapshotted[domain.Task], 0, len(ids))
	for _, id := range ids {
		task, err := tx.GetTaskSnapshot(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, task)
	}
	return out, nil
}

// AssignTask binds a new run before persistence and preserves a stored binding
// on replay. Production creators provide their intake source; source-less
// legacy/demo constructors receive an orphan identity. Neither path changes
// the content-addressed run or campaign IDs.
func (tx *WriteTx) AssignTask(ctx context.Context, run *domain.Run, source *domain.SpecificationSource) error {
	if run.TaskID == "" {
		err := tx.tx.QueryRowContext(ctx, `SELECT task_id FROM runs WHERE id = ?`, run.ID).Scan(&run.TaskID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	expected, sourceDigest, err := tx.campaignTask(ctx, *run)
	if err != nil {
		return err
	}
	if expected != "" {
		if run.TaskID != "" && run.TaskID != expected {
			return domain.ErrParentKeyMismatch
		}
		run.TaskID = expected
	}
	if run.TaskID == "" && source == nil && sourceDigest != "" {
		task, err := tx.getOrCreateTask(ctx, run.ProjectID, "source:"+string(sourceDigest), nil, time.Now().UTC())
		if err != nil {
			return err
		}
		run.TaskID = task.ID
	}
	if run.TaskID == "" {
		var task domain.Task
		var err error
		if source != nil {
			task, err = tx.GetOrCreateTask(ctx, run.ProjectID, *source)
		} else {
			task, err = tx.getOrCreateTask(ctx, run.ProjectID, "orphan:"+string(run.ID), nil, time.Now().UTC())
		}
		if err != nil {
			return err
		}
		run.TaskID = task.ID
	}
	task, err := tx.GetTask(ctx, run.TaskID)
	if err != nil {
		return err
	}
	if task.ProjectID != run.ProjectID {
		return domain.ErrParentKeyMismatch
	}
	return nil
}

func (tx *WriteTx) recordTaskRun(ctx context.Context, run domain.Run) error {
	if _, err := tx.tx.ExecContext(ctx, `INSERT INTO task_runs (task_id, ordinal, run_id)
		SELECT ?, COALESCE(MAX(ordinal), 0) + 1, ? FROM task_runs WHERE task_id = ?
		ON CONFLICT (run_id) DO NOTHING`, run.TaskID, run.ID, run.TaskID); err != nil {
		return err
	}
	task, err := tx.GetTask(ctx, run.TaskID)
	if err != nil {
		return err
	}
	if run.CampaignID != "" && !slices.Contains(task.CampaignIDs, run.CampaignID) {
		task.CampaignIDs = append(task.CampaignIDs, run.CampaignID)
	}
	return tx.updateTask(ctx, task)
}

func (tx *WriteTx) updateTask(ctx context.Context, task domain.Task) error {
	// Lifecycle facts are persisted in task_lifecycle_facts, never the body;
	// strip them so the body carries a single source of truth (issue #1318 D1).
	task.LifecycleFacts = nil
	task.Cancellation = nil
	body, err := encode(task)
	if err != nil {
		return err
	}
	_, err = tx.tx.ExecContext(ctx, `UPDATE tasks SET body = ?, entity_version = entity_version + 1, as_of_revision = ? WHERE id = ?`, body, tx.asOfRevision, task.ID)
	return err
}

// SetTaskName applies the task-name source order. Approval can replace an
// identifier or agent name; an operator or approved name is permanent. The
// engine limits agent refinement to the task's first accepted specification.
func (tx *WriteTx) SetTaskName(ctx context.Context, id domain.TaskID, name domain.DisplayName) error {
	task, err := tx.GetTask(ctx, id)
	if err != nil {
		return err
	}
	if task.Name == name || task.Name.Source == domain.DisplayNameSourceOperator || task.Name.Source == domain.DisplayNameSourceSpecification {
		return nil
	}
	if name.Source == domain.DisplayNameSourceIdentifier {
		return domain.ErrImmutableTransition
	}
	task.Name = name
	if err := tx.updateTask(ctx, task); err != nil {
		return err
	}
	// Attention snapshots project the current task name. Advance their own
	// versions atomically so a delayed read cannot restore the previous name.
	_, err = tx.tx.ExecContext(ctx, `UPDATE attention_items
		SET entity_version = entity_version + 1, as_of_revision = ?
		WHERE subject_task_id = ?`, tx.asOfRevision, id)
	return err
}

func (tx *ReadTx) TaskRunIDs(ctx context.Context, id domain.TaskID) ([]domain.RunID, error) {
	rows, err := tx.tx.QueryContext(ctx, `SELECT run_id FROM task_runs WHERE task_id = ? ORDER BY ordinal`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	ids := []domain.RunID{}
	for rows.Next() {
		var runID domain.RunID
		if err := rows.Scan(&runID); err != nil {
			return nil, err
		}
		ids = append(ids, runID)
	}
	return ids, rows.Err()
}
