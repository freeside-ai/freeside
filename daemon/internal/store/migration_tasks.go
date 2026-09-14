package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/fakepublication"
)

func backfillTasks(ctx context.Context, tx *sql.Tx) error {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM runs) + (SELECT COUNT(*) FROM attention_items)`).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		return nil
	}
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision), 0) + 1 FROM server_state`).Scan(&revision); err != nil {
		return err
	}
	w := &WriteTx{InternalTx: InternalTx{ReadTx: ReadTx{tx: tx}}, asOfRevision: revision}
	rows, err := tx.QueryContext(ctx, `SELECT r.id, r.project_id, r.body,
		(SELECT MIN(recorded_at) FROM run_milestones WHERE run_id = r.id)
		FROM runs r ORDER BY r.rowid`)
	if err != nil {
		return err
	}
	type legacyRun struct {
		id           domain.RunID
		projectID    domain.ProjectID
		body         []byte
		sourceDigest domain.Digest
		createdAt    sql.NullString
	}
	var runs []legacyRun
	for rows.Next() {
		var run legacyRun
		if err := rows.Scan(&run.id, &run.projectID, &run.body, &run.createdAt); err != nil {
			_ = rows.Close()
			return err
		}
		runs = append(runs, run)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, row := range runs {
		run, err := decode[domain.Run](row.body)
		coherent := err == nil && run.ID == row.id && run.ProjectID == row.projectID
		if !coherent {
			// Give the indexed run an identity without repairing corrupt bytes.
			// Reconstruction must still reject the original invalid body.
			run = domain.Run{ID: row.id, ProjectID: row.projectID}
		}
		var source *domain.SpecificationSource
		if coherent {
			row.sourceDigest, source, err = taskMigrationSource(ctx, &w.ReadTx, run)
			if err != nil {
				return err
			}
		}
		key := "orphan:" + string(run.ID)
		if source != nil {
			key, err = w.taskIntakeKey(ctx, *source)
			if err != nil {
				return err
			}
		} else if row.sourceDigest != "" {
			key = "source:" + string(row.sourceDigest)
			var artifactID domain.ArtifactID
			err := tx.QueryRowContext(ctx, `SELECT id FROM artifacts WHERE digest = ? AND json_extract(body, '$.type') = 'specification' ORDER BY id LIMIT 1`, row.sourceDigest).Scan(&artifactID)
			if err == nil {
				source = &domain.SpecificationSource{Kind: domain.SpecificationSourceWorkItemArtifact, WorkItemArtifactID: artifactID}
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		createdAt := time.Now().UTC()
		if row.createdAt.Valid {
			createdAt, err = parseTime(row.createdAt.String)
			if err != nil {
				return err
			}
		}
		task, err := w.getOrCreateTask(ctx, run.ProjectID, key, source, createdAt)
		if err != nil {
			return err
		}
		run.TaskID = task.ID
		body := string(row.body)
		if coherent {
			body, err = encode(run)
			if err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET task_id = ?, body = ?, entity_version = entity_version + 1, as_of_revision = ? WHERE id = ?`, task.ID, body, revision, run.ID); err != nil {
			return err
		}
		if err := w.recordTaskRun(ctx, run); err != nil {
			return err
		}
	}
	if err := backfillTaskSubjects(ctx, w); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE server_state SET revision = ? WHERE id = 1`, revision)
	return err
}

// backfillTaskLifecycleFacts seeds the lifecycle log for existing tasks so the
// WIP cap does not resurrect dead legacy work after the upgrade (issue #1318
// D7). A task with a non-final run is WIP (started); one with a recorded
// work-unit completion is not (started then completed); one whose runs are all
// concluded with no completion is not (started then abandoned, source
// "migration"). A task with only unstarted or snoozed proposals has no run and
// gets no fact. Run finality is the durable terminal_recorded milestone (the
// store-level signal; the engine's authenticated conclusion is unavailable
// here).
func backfillTaskLifecycleFacts(ctx context.Context, tx *sql.Tx) error {
	var taskCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks`).Scan(&taskCount); err != nil {
		return err
	}
	if taskCount == 0 {
		return nil
	}
	// Seed facts with raw inserts and no revision or entity-version bump: this
	// is a one-time transformation that adds a side table, not a body rewrite,
	// and clients re-bootstrap after a schema upgrade.
	r := &ReadTx{tx: tx}

	rows, err := tx.QueryContext(ctx, `SELECT id FROM tasks ORDER BY id`)
	if err != nil {
		return err
	}
	var taskIDs []domain.TaskID
	for rows.Next() {
		var id domain.TaskID
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		taskIDs = append(taskIDs, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}

	now := formatTime(time.Now().UTC())
	for _, id := range taskIDs {
		runIDs, err := r.TaskRunIDs(ctx, id)
		if err != nil {
			return err
		}
		if len(runIDs) == 0 {
			continue // only unstarted/snoozed proposals: no slot, no fact.
		}
		// A run counts as launched only when it was actually started: it has a
		// run_submitted milestone, or it reached a terminal (which implies it was
		// submitted). A reserved-but-unstarted run has neither, holds no slot, and
		// records no start, mirroring the runtime submission-milestone rule (issue
		// #1318 D2; docs/plan.md §5.11): equating every non-concluded run with a
		// start would let a reserved proposal consume WIP capacity.
		var launched []launchedRun
		for _, runID := range runIDs {
			concluded, err := runConcluded(ctx, r, runID)
			if err != nil {
				return err
			}
			submitted, err := runSubmitted(ctx, r, runID)
			if err != nil {
				return err
			}
			if !submitted && !concluded {
				continue // reserved but never launched.
			}
			campaign, err := runCampaign(ctx, tx, runID)
			if err != nil {
				return err
			}
			launched = append(launched, launchedRun{id: runID, campaign: campaign, concluded: concluded})
		}
		if len(launched) == 0 {
			continue // every run is reserved-but-unstarted: no slot, no fact.
		}
		// The current work episode is the newest launched run's campaign, not the
		// last of CampaignIDs: a reserved-but-unstarted proposal appends a campaign
		// there but records no start, and it must not shift currency (issue #1318
		// G2; docs/plan.md §5.11). The started fact and completion currency use it.
		start := launched[len(launched)-1]
		currentCampaign := start.campaign
		// A specification run never records terminal_recorded (only production runs
		// do), so run non-finality alone does not mean the task is active. Finality
		// is the newest launched run's state (the current episode), not any run
		// sharing its campaign: an earlier attempt in the same campaign may have
		// reached terminal while a newer retry is still live, and that live task
		// must stay WIP (issue #1318 H2). A concluded newest run means the
		// implementation finished, which is a completion or an abandonment.
		currentConcluded := start.concluded
		completedRun, completedCampaign, binding, completedFound, err := taskCompletionBinding(ctx, tx, id, currentCampaign)
		if err != nil {
			return err
		}
		if err := insertMigrationFact(ctx, tx, id, domain.TaskLifecycleStarted, start.id, currentCampaign, nil, "migration:start", now); err != nil {
			return err
		}
		switch {
		case completedFound:
			// A completion on the current campaign released the slot (D4).
			if err := insertMigrationFact(ctx, tx, id, domain.TaskLifecycleCompleted, completedRun, completedCampaign, &binding, "migration:complete", now); err != nil {
				return err
			}
		case currentConcluded:
			// The current campaign's implementation concluded with no completion:
			// abandoned, so a dead legacy task (or a superseded prior-campaign
			// completion) does not leave the synthetic start holding a slot.
			if err := insertMigrationFact(ctx, tx, id, domain.TaskLifecycleAbandoned, start.id, nil, nil, "migration", now); err != nil {
				return err
			}
		default:
			// The current campaign is still in progress: started only (WIP).
		}
	}
	return nil
}

// insertMigrationFact appends one backfill fact with the store's ordinal and
// idempotency rules but no revision or version bump.
func insertMigrationFact(ctx context.Context, tx *sql.Tx, taskID domain.TaskID, kind domain.TaskLifecycleFactKind, run domain.RunID, campaign *domain.CampaignID, bindingUnit *domain.WorkUnitID, source, recordedAt string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO task_lifecycle_facts
		(task_id, ordinal, kind, run_id, campaign_id, binding_unit_id, source_id, recorded_at)
		SELECT ?, COALESCE(MAX(ordinal), 0) + 1, ?, ?, ?, ?, ?, ?
		FROM task_lifecycle_facts WHERE task_id = ?
		ON CONFLICT (task_id, source_id) DO NOTHING`,
		taskID, kind, run, nullableCampaign(campaign), nullableBinding(bindingUnit), source, recordedAt, taskID)
	return err
}

// runCampaign returns a run's campaign as a pointer, nil when the run carries
// none (a legacy campaign-less run).
func runCampaign(ctx context.Context, tx *sql.Tx, runID domain.RunID) (*domain.CampaignID, error) {
	var campaign sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT campaign_id FROM runs WHERE id = ?`, runID).Scan(&campaign); err != nil {
		return nil, err
	}
	if !campaign.Valid || campaign.String == "" {
		return nil, nil
	}
	c := domain.CampaignID(campaign.String)
	return &c, nil
}

// runConcluded reports whether a run reached a committed terminal class,
// mirroring the production-attempt terminal check (production_attempt.go).
func runConcluded(ctx context.Context, tx *ReadTx, runID domain.RunID) (bool, error) {
	milestones, err := tx.ListRunMilestones(ctx, runID)
	if err != nil {
		return false, err
	}
	for _, milestone := range milestones {
		if milestone.Kind == domain.MilestoneTerminalRecorded &&
			milestone.Terminal != nil && milestone.Terminal.Concluded() {
			return true, nil
		}
	}
	return false, nil
}

// runSubmitted reports whether a run recorded a submission milestone, the
// durable signal that it was actually launched (issue #1318 D2). A
// reserved-but-unstarted proposal run has none.
func runSubmitted(ctx context.Context, tx *ReadTx, runID domain.RunID) (bool, error) {
	milestones, err := tx.ListRunMilestones(ctx, runID)
	if err != nil {
		return false, err
	}
	for _, milestone := range milestones {
		if milestone.Kind == domain.MilestoneRunSubmitted {
			return true, nil
		}
	}
	return false, nil
}

// launchedRun is a task run that was actually started (submitted or concluded),
// carrying the two facts the backfill classification needs: its campaign and
// whether it reached a committed terminal.
type launchedRun struct {
	id        domain.RunID
	campaign  *domain.CampaignID
	concluded bool
}

// taskCompletionBinding finds a recorded work-unit completion on the task's
// current campaign, returning the completed run, its campaign, and the binding.
// Selecting the current campaign (not the earliest completion by row order)
// keeps a superseded prior-campaign completion out of the backfill, so a
// revised task whose current campaign never completed is abandoned rather than
// left holding a slot (issue #1318 D4; docs/plan.md §5.11). A nil current
// campaign matches a campaign-less legacy run.
func taskCompletionBinding(ctx context.Context, tx *sql.Tx, taskID domain.TaskID, currentCampaign *domain.CampaignID) (domain.RunID, *domain.CampaignID, domain.WorkUnitID, bool, error) {
	var unit domain.WorkUnitID
	var run domain.RunID
	var campaign sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT c.unit_id, r.id, r.campaign_id
		FROM work_unit_completions c
		JOIN runs r ON c.unit_id = 'workunit-' || r.id
		JOIN task_runs tr ON tr.run_id = r.id
		WHERE tr.task_id = ? AND COALESCE(r.campaign_id, '') = COALESCE(?, '')
		ORDER BY tr.ordinal DESC LIMIT 1`, taskID, nullableCampaign(currentCampaign)).Scan(&unit, &run, &campaign)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, "", false, nil
	}
	if err != nil {
		return "", nil, "", false, err
	}
	var camp *domain.CampaignID
	if campaign.Valid && campaign.String != "" {
		c := domain.CampaignID(campaign.String)
		camp = &c
	}
	return run, camp, unit, true, nil
}

func backfillTaskSubjects(ctx context.Context, tx *WriteTx) error {
	rows, err := tx.tx.QueryContext(ctx, `SELECT i.id, i.body, s.epoch, s.digest, s.body
		FROM attention_items i LEFT JOIN attention_decision_surfaces s ON s.item_id = i.id ORDER BY i.id`)
	if err != nil {
		return err
	}
	type candidate struct {
		id      domain.ItemID
		body    []byte
		epoch   sql.NullInt64
		digest  sql.NullString
		surface []byte
	}
	var items []candidate
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.id, &item.body, &item.epoch, &item.digest, &item.surface); err != nil {
			_ = rows.Close()
			return err
		}
		items = append(items, item)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, row := range items {
		var object map[string]json.RawMessage
		if err := decodeMigrationJSON(row.body, &object); err != nil {
			continue
		}
		if names, ok := object["display_names"]; ok && string(names) != "null" {
			var labels map[string]json.RawMessage
			if err := decodeMigrationJSON(names, &labels); err != nil {
				return err
			}
			if legacy, ok := labels["work_unit"]; ok {
				labels["task"] = legacy
				delete(labels, "work_unit")
			}
			object["display_names"], err = json.Marshal(labels)
			if err != nil {
				return err
			}
		}
		body, err := json.Marshal(object)
		if err != nil {
			return err
		}
		item, err := decode[domain.AttentionItem](body)
		if err != nil || item.ID != row.id {
			// Do not turn a pre-existing unreadable item into new authority.
			continue
		}
		var surface domain.DecisionSurface
		if !row.epoch.Valid || !row.digest.Valid || decodeMigrationJSON(row.surface, &surface) != nil {
			continue
		}
		if surface.ItemID != row.id || surface.Epoch != int(row.epoch.Int64) || string(surface.Digest) != row.digest.String ||
			item.DecisionSurface.Epoch != surface.Epoch || item.DecisionSurface.Digest != surface.Digest || !surface.Matches(item) {
			continue
		}
		if err := surface.Validate(); err != nil {
			continue
		}
		terminal, err := tx.taskMigrationFakeTerminal(ctx, item)
		if err != nil {
			if reviewConfigRecoveryEnvironmental(err) {
				return err
			}
			continue
		}
		if err := tx.bindSubjectTask(ctx, item.ProjectID, &item.Subject); err != nil {
			if reviewConfigRecoveryEnvironmental(err) {
				return err
			}
			// A dangling or corrupt legacy parent grants no new authority.
			continue
		}
		if item.Subject.TaskID != nil {
			task, err := tx.GetTask(ctx, *item.Subject.TaskID)
			if err != nil {
				return err
			}
			if item.DisplayNames != nil {
				item.DisplayNames.Task = task.Name
			}
		}
		if terminal != nil {
			item.Reason, _, _ = fakepublication.ParseTerminalReason(item.Reason)
			item, err = fakepublication.BindTerminal(*terminal, item)
			if err != nil {
				return err
			}
		}
		surface.Subject = item.Subject
		if err := surface.Validate(); err != nil {
			return err
		}
		bodyText, err := encode(item)
		if err != nil {
			return err
		}
		surfaceText, err := encode(surface)
		if err != nil {
			return err
		}
		if _, err := tx.tx.ExecContext(ctx, `UPDATE attention_items SET subject_task_id = ?, body = ?, entity_version = entity_version + 1, as_of_revision = ? WHERE id = ?`, item.Subject.TaskID, bodyText, tx.asOfRevision, item.ID); err != nil {
			return err
		}
		if _, err := tx.tx.ExecContext(ctx, `UPDATE attention_decision_surfaces SET epoch = ?, digest = ?, body = ? WHERE item_id = ?`, surface.Epoch, surface.Digest, surfaceText, item.ID); err != nil {
			return err
		}
	}
	return nil
}

// taskMigrationFakeTerminal verifies the original reason commitment before its
// stored labels are replaced. Pending outbox entries include the crash seam
// after terminal persistence and before publication-task completion.
func (tx *ReadTx) taskMigrationFakeTerminal(ctx context.Context, item domain.AttentionItem) (*fakepublication.Task, error) {
	if item.Subject.RunID == nil {
		return nil, nil
	}
	runID := *item.Subject.RunID
	if item.ID != fakepublication.ReadyItemID(runID) && item.ID != fakepublication.BlockedItemID(runID) {
		return nil, nil
	}
	if item.Subject.Type != domain.SubjectRun || item.Subject.ID != domain.SubjectID(runID) ||
		(item.ID == fakepublication.ReadyItemID(runID) && item.Type != domain.AttentionReadyForFinalReview) ||
		(item.ID == fakepublication.BlockedItemID(runID) && item.Type != domain.AttentionPublishBlocked) {
		return nil, domain.ErrParentKeyMismatch
	}
	entry, err := tx.GetOutbox(ctx, fakepublication.TaskKey(runID))
	if err != nil {
		return nil, err
	}
	task, err := fakepublication.DecodeTask(entry.Payload)
	if err != nil {
		return nil, err
	}
	if entry.Kind != fakepublication.TaskKind || task.RunID != runID || task.ProjectID != item.ProjectID {
		return nil, domain.ErrParentKeyMismatch
	}
	if _, err := fakepublication.ValidateTerminalBinding(task, item); err != nil {
		return nil, err
	}
	return &task, nil
}

// taskMigrationSource accepts source grouping only when the run/attempt tuple
// agrees with the immutable original dispatch. A rejected row gets an orphan
// identity; an operational read failure aborts the migration transaction.
func taskMigrationSource(ctx context.Context, tx *ReadTx, run domain.Run) (domain.Digest, *domain.SpecificationSource, error) {
	if run.CampaignID == "" {
		return "", nil, nil
	}
	attempt, err := tx.scanProductionAttempt(tx.tx.QueryRowContext(ctx,
		`SELECT `+productionAttemptColumns+` FROM production_attempts WHERE campaign_id = ? AND attempt_number = ?`, run.CampaignID, run.AttemptNumber))
	if err != nil {
		if reviewConfigRecoveryEnvironmental(err) {
			return "", nil, err
		}
		return "", nil, nil
	}
	isSpecification := run.ID == attempt.SpecificationRunID && run.SpecDigest == attempt.SourceDigest
	isImplementation := run.ID == attempt.ImplementationRunID && run.SpecDigest == attempt.ApprovedSpecDigest && run.ParentRunID == attempt.ParentRunID
	if !isSpecification && !isImplementation {
		return "", nil, nil
	}
	initial, err := tx.scanProductionAttempt(tx.tx.QueryRowContext(ctx,
		`SELECT `+productionAttemptColumns+` FROM production_attempts WHERE campaign_id = ? AND attempt_number = 1`, run.CampaignID))
	if err != nil {
		if reviewConfigRecoveryEnvironmental(err) {
			return "", nil, err
		}
		return "", nil, nil
	}
	if initial.SourceDigest != attempt.SourceDigest || initial.SpecificationRunID != attempt.SpecificationRunID {
		return "", nil, nil
	}
	source, err := tx.taskMigrationLabelSource(ctx, run, initial)
	if err != nil {
		if reviewConfigRecoveryEnvironmental(err) {
			return "", nil, err
		}
	}
	if err == nil && source != nil {
		return attempt.SourceDigest, source, nil
	}
	present, err := tx.authenticateInitialAttemptSource(ctx, initial)
	if err != nil {
		if reviewConfigRecoveryEnvironmental(err) {
			return "", nil, err
		}
		return "", nil, nil
	}
	if !present {
		return "", nil, nil
	}
	return attempt.SourceDigest, nil, nil
}

// A label reservation commits its declaration before proposal admission or
// dispatch. Recover the issue source from that declaration and the original
// occurrence coordinates, never from a mutable admission's claimed subject.
func (tx *ReadTx) taskMigrationLabelSource(ctx context.Context, run domain.Run, initial domain.ProductionAttempt) (*domain.SpecificationSource, error) {
	declaration, err := tx.scanWorkUnitDeclaration(tx.tx.QueryRowContext(ctx, getWorkUnitDeclarationByRunSQL, initial.SpecificationRunID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if declaration.BoundIssue == nil {
		return nil, nil
	}
	if declaration.RunID != initial.SpecificationRunID || declaration.ProjectID != run.ProjectID {
		return nil, domain.ErrParentKeyMismatch
	}
	var projectID domain.ProjectID
	var policyDigest domain.Digest
	var body []byte
	if err := tx.tx.QueryRowContext(ctx, `SELECT project_id, policy_digest, body FROM runs WHERE id = ?`, initial.SpecificationRunID).Scan(&projectID, &policyDigest, &body); err != nil {
		return nil, err
	}
	specification, err := decode[domain.Run](body)
	if err != nil {
		return nil, err
	}
	if specification.ID != initial.SpecificationRunID || specification.ProjectID != projectID || projectID != run.ProjectID || specification.PolicyDigest != policyDigest || specification.SpecDigest != initial.SourceDigest || specification.CampaignID != initial.CampaignID {
		return nil, domain.ErrParentKeyMismatch
	}
	policy, err := tx.GetResolvedPolicy(ctx, specification.ID)
	if err != nil {
		return nil, err
	}
	if !slices.Equal(declaration.DeclaredPaths, domain.CanonicalDeclaredPaths(policy)) {
		return nil, ErrDeclarationUnsupported
	}
	project, err := tx.GetProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	rows, err := tx.tx.QueryContext(ctx, `SELECT `+intakeOccurrenceColumns+` FROM intake_occurrences WHERE repository_id = ? AND issue_number = ? ORDER BY label, ordinal`, project.RepositoryID, *declaration.BoundIssue)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		occurrence, err := tx.scanIntakeOccurrenceCoordinates(rows)
		if err != nil {
			return nil, err
		}
		if domain.IntakeImplementationRunID(occurrence) != initial.ImplementationRunID || !domain.SpecificationRunIDMatchesImplementation(specification.ID, initial.ImplementationRunID) {
			continue
		}
		source := intakeIssueSubjectSource(occurrence)
		return &source, nil
	}
	return nil, rows.Err()
}
