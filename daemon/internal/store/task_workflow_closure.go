package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// ErrTaskWorkflowClosed forbids advancement, not observation or resource
// release. It never means the provider stopped or an external effect rolled back.
var ErrTaskWorkflowClosed = errors.New("task workflow episode is closed")

const abandonedWorkflowReason = "Operator abandoned this workflow episode."

// TaskWorkflowClosure identifies the exact admitted start closed by the
// operator. Runs are captured from authoritative membership, not supplied by
// the caller. A later admission never clears this record.
type TaskWorkflowClosure struct {
	TaskID       domain.TaskID  `json:"task_id"`
	StartOrdinal int            `json:"start_ordinal"`
	StartRunID   domain.RunID   `json:"start_run_id"`
	RunIDs       []domain.RunID `json:"run_ids"`
	Revision     int64          `json:"revision"`
	ClosedAt     time.Time      `json:"closed_at"`
	Reason       string         `json:"reason"`
}

// TaskWorkflowEpisode returns the start owning a run in authoritative
// task-run order. Campaign identity alone cannot distinguish reattempts.
// It uses the structural run binding, avoiding the approval readers that
// themselves ask whether a workflow is closed.
func (tx *ReadTx) TaskWorkflowEpisode(ctx context.Context, runID domain.RunID) (domain.TaskID, *domain.TaskLifecycleFact, error) {
	var taskID domain.TaskID
	var body []byte
	if err := tx.tx.QueryRowContext(ctx, `SELECT task_id, body FROM runs WHERE id = ?`, runID).Scan(&taskID, &body); err != nil {
		return "", nil, notFoundOr(err)
	}
	run, err := decode[domain.Run](body)
	if err != nil {
		return "", nil, err
	}
	if run.ID != runID || run.TaskID != taskID {
		return "", nil, errRowInconsistent
	}
	if err := tx.gateRunTask(ctx, run); err != nil {
		return "", nil, err
	}
	task, err := tx.GetTask(ctx, taskID)
	if err != nil {
		return "", nil, err
	}
	ids, err := tx.TaskRunIDs(ctx, taskID)
	if err != nil {
		return "", nil, err
	}
	index := slices.Index(ids, runID)
	if index < 0 {
		return "", nil, errRowInconsistent
	}
	var episode *domain.TaskLifecycleFact
	for _, fact := range task.LifecycleFacts {
		if fact.Kind != domain.TaskLifecycleStarted {
			continue
		}
		startIndex := slices.Index(ids, fact.RunID)
		if startIndex < 0 {
			return "", nil, errRowInconsistent
		}
		// Migration 0072 represented an existing campaign with its newest
		// launched run. Earlier runs in that campaign belong to that same
		// synthetic episode, not to an unowned pre-start interval.
		migratedCampaign := fact.SourceID == "migration:start" &&
			fact.CampaignID != nil && run.CampaignID == *fact.CampaignID
		if startIndex <= index || migratedCampaign {
			start := fact
			episode = &start
		}
	}
	return taskID, episode, nil
}

func (tx *ReadTx) abandonedWorkflowStart(ctx context.Context, taskID domain.TaskID, ordinal int) (domain.TaskLifecycleFact, error) {
	task, err := tx.GetTask(ctx, taskID)
	if err != nil {
		return domain.TaskLifecycleFact{}, err
	}
	var start domain.TaskLifecycleFact
	for _, fact := range task.LifecycleFacts {
		if fact.Ordinal == ordinal && fact.Kind == domain.TaskLifecycleStarted {
			start = fact
		}
		if start.Ordinal != 0 && fact.Kind == domain.TaskLifecycleAbandoned &&
			fact.RunID == start.RunID && fact.SourceID == abandonSourceID(start) {
			return start, nil
		}
	}
	return domain.TaskLifecycleFact{}, fmt.Errorf("task %q episode %d has no bound abandonment: %w", taskID, ordinal, domain.ErrParentKeyMismatch)
}

// GetTaskWorkflowClosure rechecks the closure's episode and captured membership
// on every read. Missing closure is ErrNotFound; malformed authority fails loud.
func (tx *ReadTx) GetTaskWorkflowClosure(ctx context.Context, taskID domain.TaskID, ordinal int) (TaskWorkflowClosure, error) {
	var runID domain.RunID
	var revision int64
	var body []byte
	err := tx.tx.QueryRowContext(ctx, `SELECT start_run_id, revision, body
		FROM task_workflow_closures WHERE task_id = ? AND start_ordinal = ?`, taskID, ordinal).
		Scan(&runID, &revision, &body)
	if err != nil {
		return TaskWorkflowClosure{}, notFoundOr(err)
	}
	var closure TaskWorkflowClosure
	if err := strictjson.Decode(body, &closure, strictjson.RejectInvalidUTF8, strictjson.NoLimit); err != nil {
		return TaskWorkflowClosure{}, err
	}
	if closure.TaskID != taskID || closure.StartOrdinal != ordinal ||
		closure.StartRunID != runID || closure.Revision != revision || revision < 1 ||
		closure.ClosedAt.IsZero() || len(closure.RunIDs) == 0 ||
		closure.Reason != abandonedWorkflowReason {
		return TaskWorkflowClosure{}, errRowInconsistent
	}
	start, err := tx.abandonedWorkflowStart(ctx, taskID, ordinal)
	if err != nil {
		return TaskWorkflowClosure{}, err
	}
	if start.RunID != runID {
		return TaskWorkflowClosure{}, errRowInconsistent
	}
	ids, err := tx.workflowEpisodeRuns(ctx, taskID, ordinal)
	if err != nil {
		return TaskWorkflowClosure{}, err
	}
	if !slices.Equal(closure.RunIDs, ids) {
		return TaskWorkflowClosure{}, errRowInconsistent
	}
	return closure, nil
}

func (tx *ReadTx) workflowEpisodeRuns(ctx context.Context, taskID domain.TaskID, ordinal int) ([]domain.RunID, error) {
	ids, err := tx.TaskRunIDs(ctx, taskID)
	if err != nil {
		return nil, err
	}
	result := []domain.RunID{}
	for _, id := range ids {
		owner, start, err := tx.TaskWorkflowEpisode(ctx, id)
		if err != nil {
			return nil, err
		}
		if owner != taskID {
			return nil, errRowInconsistent
		}
		if start != nil && start.Ordinal == ordinal {
			result = append(result, id)
		}
	}
	return result, nil
}

// CloseTaskWorkflow commits closure only for an already-abandoned, explicitly
// selected episode. Slot release belongs in a preceding committed transaction.
// A repair can therefore never abandon whichever episode happens to be newest.
func (tx *WriteTx) CloseTaskWorkflow(ctx context.Context, taskID domain.TaskID, ordinal int, now time.Time) (TaskWorkflowClosure, bool, error) {
	existing, err := tx.GetTaskWorkflowClosure(ctx, taskID, ordinal)
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return TaskWorkflowClosure{}, false, err
	}
	start, err := tx.abandonedWorkflowStart(ctx, taskID, ordinal)
	if err != nil {
		return TaskWorkflowClosure{}, false, err
	}
	ids, err := tx.workflowEpisodeRuns(ctx, taskID, ordinal)
	if err != nil {
		return TaskWorkflowClosure{}, false, err
	}
	items, err := tx.ListOpenAttentionItemRecordsForTask(ctx, taskID)
	if err != nil {
		return TaskWorkflowClosure{}, false, err
	}
	for _, item := range items {
		if item.Subject.Type != domain.SubjectTask {
			continue
		}
		owner, episode, err := tx.AttentionItemWorkflowEpisode(ctx, item)
		if err != nil {
			return TaskWorkflowClosure{}, false, err
		}
		if owner != taskID || episode != ordinal {
			continue
		}
		if _, err := tx.taskAttentionWorkflowRun(ctx, item); errors.Is(err, ErrNotFound) {
			if err := tx.bindTaskAttentionWorkflow(ctx, item, start.RunID); err != nil {
				return TaskWorkflowClosure{}, false, err
			}
		} else if err != nil {
			return TaskWorkflowClosure{}, false, err
		}
	}
	closure := TaskWorkflowClosure{
		TaskID: taskID, StartOrdinal: ordinal, StartRunID: start.RunID,
		RunIDs: ids, Revision: tx.asOfRevision, ClosedAt: now.UTC(), Reason: abandonedWorkflowReason,
	}
	body, err := json.Marshal(closure)
	if err != nil {
		return TaskWorkflowClosure{}, false, err
	}
	if _, err := tx.tx.ExecContext(ctx, `INSERT INTO task_workflow_closures
		(task_id, start_ordinal, start_run_id, revision, body) VALUES (?, ?, ?, ?, ?)`,
		taskID, ordinal, start.RunID, tx.asOfRevision, string(body)); err != nil {
		return TaskWorkflowClosure{}, false, err
	}
	closure, err = tx.GetTaskWorkflowClosure(ctx, taskID, ordinal)
	return closure, true, err
}

// RequireRunWorkflowOpen gates advancement while allowing callers to record
// authenticated late results through their separate observation paths.
func (tx *ReadTx) RequireRunWorkflowOpen(ctx context.Context, runID domain.RunID) error {
	// Earlier migration data steps may use current readers.
	if exists, err := tx.tableExists(ctx, "task_workflow_closures"); err != nil || !exists {
		return err
	}
	// Authenticate every row before filtering. A rewritten copied task/start
	// key must not hide a closure and turn a corrupt row into permission.
	closures, err := tx.ListTaskWorkflowClosures(ctx)
	if err != nil || len(closures) == 0 {
		return err
	}
	taskID, start, err := tx.TaskWorkflowEpisode(ctx, runID)
	if err != nil || start == nil {
		return err
	}
	for _, closure := range closures {
		if closure.TaskID == taskID && closure.StartOrdinal == start.Ordinal {
			return ErrTaskWorkflowClosed
		}
	}
	return nil
}

type taskAttentionWorkflowOrigin struct {
	ItemID domain.ItemID `json:"item_id"`
	TaskID domain.TaskID `json:"task_id"`
	RunID  domain.RunID  `json:"run_id"`
}

// PutTaskAttentionItem preserves a task-subject notice's actual producing run
// without changing its public subject. Callers must carry the original run,
// including after a delayed observation or a new episode's admission.
func (tx *WriteTx) PutTaskAttentionItem(ctx context.Context, item domain.AttentionItem, runID domain.RunID) error {
	taskID, _, err := tx.TaskWorkflowEpisode(ctx, runID)
	if err != nil {
		return err
	}
	if item.Subject.Type != domain.SubjectTask || item.Subject.TaskID == nil ||
		*item.Subject.TaskID != taskID {
		return domain.ErrParentKeyMismatch
	}
	if err := tx.RequireRunWorkflowOpen(ctx, runID); err != nil {
		return err
	}
	return tx.putAttentionItem(ctx, item, false, runID)
}

func (tx *WriteTx) bindTaskAttentionWorkflow(ctx context.Context, item domain.AttentionItem, runID domain.RunID) error {
	taskID, _, err := tx.TaskWorkflowEpisode(ctx, runID)
	if err != nil {
		return err
	}
	origin := taskAttentionWorkflowOrigin{ItemID: item.ID, TaskID: taskID, RunID: runID}
	body, err := json.Marshal(origin)
	if err != nil {
		return err
	}
	_, err = tx.tx.ExecContext(ctx, `INSERT INTO task_attention_workflow_origins
		(item_id, task_id, run_id, body) VALUES (?, ?, ?, ?) ON CONFLICT (item_id) DO NOTHING`,
		item.ID, taskID, runID, string(body))
	if err != nil {
		return err
	}
	stored, err := tx.taskAttentionWorkflowRun(ctx, item)
	if err != nil {
		return err
	}
	if stored != runID {
		return ErrImmutableConflict
	}
	return nil
}

func (tx *ReadTx) taskAttentionWorkflowRun(ctx context.Context, item domain.AttentionItem) (domain.RunID, error) {
	var taskID domain.TaskID
	var runID domain.RunID
	var body []byte
	err := tx.tx.QueryRowContext(ctx, `SELECT task_id, run_id, body
		FROM task_attention_workflow_origins WHERE item_id = ?`, item.ID).Scan(&taskID, &runID, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return tx.legacyHeldWorkNoticeRun(ctx, item)
	}
	if err != nil {
		return "", err
	}
	var origin taskAttentionWorkflowOrigin
	if err := strictjson.Decode(body, &origin, strictjson.RejectInvalidUTF8, strictjson.NoLimit); err != nil {
		return "", err
	}
	if origin.ItemID != item.ID || origin.TaskID != taskID || origin.RunID != runID ||
		item.Subject.TaskID == nil || *item.Subject.TaskID != taskID {
		return "", errRowInconsistent
	}
	owner, _, err := tx.TaskWorkflowEpisode(ctx, runID)
	if err != nil {
		return "", err
	}
	if owner != taskID {
		return "", errRowInconsistent
	}
	return runID, nil
}

// Before workflow origins were recorded, both held-work producers encoded the
// producing run in their IDs. Authenticate that historical binding without
// depending on a hold row, which may already have cleared, or inferring closure.
func (tx *ReadTx) legacyHeldWorkNoticeRun(ctx context.Context, item domain.AttentionItem) (domain.RunID, error) {
	id := string(item.ID)
	encoded, raised := strings.CutPrefix(id, "raised-work-held-")
	if !raised {
		var known bool
		encoded, known = strings.CutPrefix(id, "work-held-")
		if !known {
			return "", ErrNotFound
		}
	}
	separator := strings.LastIndexByte(encoded, '-')
	if separator <= 0 {
		return "", errRowInconsistent
	}
	suffix, err := strconv.ParseInt(encoded[separator+1:], 10, 64)
	if err != nil || suffix <= 0 || strconv.FormatInt(suffix, 10) != encoded[separator+1:] {
		return "", errRowInconsistent
	}
	runID := domain.RunID(encoded[:separator])
	owner, _, err := tx.TaskWorkflowEpisode(ctx, runID)
	if errors.Is(err, ErrNotFound) {
		return "", errRowInconsistent
	}
	if err != nil {
		return "", err
	}
	task, err := tx.GetTask(ctx, owner)
	if err != nil {
		return "", err
	}
	if item.Subject.Type != domain.SubjectTask || item.Subject.TaskID == nil ||
		*item.Subject.TaskID != owner || item.Subject.ID != domain.SubjectID(owner) ||
		item.Subject.RunID != nil || item.ProjectID != task.ProjectID ||
		item.Type != domain.AttentionSystemHealth || item.Priority != domain.PriorityHigh ||
		item.HealthDiagnostic == nil || item.HealthDiagnostic.Code != "work_held" ||
		item.HealthDiagnostic.Impairs != domain.ImpairedCapabilityUnattendedAdmission ||
		item.Posture == nil || *item.Posture != domain.HealthPostureAdvisory ||
		item.InterruptionClass != domain.InterruptionExceptional || item.CreatedAt == nil ||
		!slices.Equal(item.RequestedDecision, []domain.Action{domain.ActionAcknowledge}) {
		return "", errRowInconsistent
	}
	// This is the pre-0100 HeldWorkNoticeItem body. It corroborates the ID;
	// free-form reason text never selects the owner.
	held, ok := strings.CutPrefix(item.Reason, fmt.Sprintf("Run %s has been held since ", runID))
	if !ok {
		return "", errRowInconsistent
	}
	observed, _, ok := strings.Cut(held, " with hold reason ")
	firstObserved, err := time.Parse(time.RFC3339, observed)
	if !ok || err != nil || (!raised && firstObserved.Unix() != suffix) ||
		(raised && observed != item.CreatedAt.Format(time.RFC3339)) {
		return "", errRowInconsistent
	}
	for _, reason := range []domain.RunHoldReason{
		domain.HoldBackendNotConformant, domain.HoldAdmissionPolicyRefused,
		domain.HoldBackupProtectionUnready, domain.HoldRepositoryUntrusted,
		domain.HoldProviderAuthorityUnavailable,
	} {
		if raised && reason != domain.HoldAdmissionPolicyRefused {
			continue
		}
		expected := fmt.Sprintf("Run %s has been held since %s with hold reason %s. "+
			"Freeside keeps retrying it. "+
			"This notice resolves itself when the hold clears, its cause changes, or the task is stopped.",
			runID, observed, reason)
		if item.Reason == expected {
			return runID, nil
		}
	}
	return "", errRowInconsistent
}

// AttentionItemWorkflowEpisode resolves ownership from the stored subject or
// proposal binding. A displayed project or a mention of a task grants none.
func (tx *ReadTx) AttentionItemWorkflowEpisode(ctx context.Context, item domain.AttentionItem) (domain.TaskID, int, error) {
	var runID domain.RunID
	switch {
	case item.Type == domain.AttentionEffectProposal || item.Type == domain.AttentionTaskProposal:
		_, proposal, err := tx.ProposalForItem(ctx, item.ID)
		if err != nil {
			return "", 0, err
		}
		subject, err := tx.gateProposalSubject(ctx, proposal)
		if err != nil {
			return "", 0, err
		}
		// An adoption card has no run and so no workflow episode.
		if subject.declaration != nil {
			runID = subject.declaration.RunID
		}
	case item.Subject.Type == domain.SubjectRun:
		runID = domain.RunID(item.Subject.ID)
	case item.Subject.RunID != nil:
		runID = *item.Subject.RunID
	case item.Subject.Type == domain.SubjectTask:
		var err error
		runID, err = tx.taskAttentionWorkflowRun(ctx, item)
		if errors.Is(err, ErrNotFound) {
			// Legacy task-only notices with one start have only one possible
			// owner. Multiple episodes require the producer's explicit binding;
			// never guess the newest start after a reattempt.
			task, readErr := tx.GetTask(ctx, *item.Subject.TaskID)
			if readErr != nil {
				return "", 0, readErr
			}
			for _, fact := range task.LifecycleFacts {
				if fact.Kind != domain.TaskLifecycleStarted {
					continue
				}
				if runID != "" {
					return "", 0, fmt.Errorf("task item %q has ambiguous episode ownership: %w", item.ID, domain.ErrParentKeyMismatch)
				}
				runID = fact.RunID
			}
		} else if err != nil {
			return "", 0, err
		}
	}
	if runID == "" {
		return "", 0, nil
	}
	taskID, start, err := tx.TaskWorkflowEpisode(ctx, runID)
	if err != nil || start == nil {
		return taskID, 0, err
	}
	return taskID, start.Ordinal, nil
}

func (tx *ReadTx) RequireItemWorkflowOpen(ctx context.Context, item domain.AttentionItem) error {
	if exists, err := tx.tableExists(ctx, "task_workflow_closures"); err != nil || !exists {
		return err
	}
	// Before the first closure there is no new authority to enforce. This
	// also permits a proposal's existing two-step creation/binding protocol;
	// BindProposalItem checks the completed authoritative binding.
	closures, err := tx.ListTaskWorkflowClosures(ctx)
	if err != nil || len(closures) == 0 {
		return err
	}
	taskID, ordinal, err := tx.AttentionItemWorkflowEpisode(ctx, item)
	if err != nil || ordinal == 0 {
		return err
	}
	for _, closure := range closures {
		if closure.TaskID == taskID && closure.StartOrdinal == ordinal {
			return ErrTaskWorkflowClosed
		}
	}
	return nil
}

func (tx *WriteTx) captureClosedWorkflowMembership(ctx context.Context) error {
	if tx.workflowMembershipCaptured {
		return nil
	}
	if exists, err := tx.tableExists(ctx, "task_workflow_closures"); err != nil || !exists {
		return err
	}
	var err error
	tx.workflowClosures, err = tx.ListTaskWorkflowClosures(ctx)
	tx.workflowMembershipCaptured = err == nil
	return err
}

// A fresh run and its explicit cap-checked admission share one transaction.
// Validate at commit so that order can stay atomic, while an ordinary new run
// cannot silently extend a closed episode and invalidate its captured members.
func (tx *WriteTx) validateClosedWorkflowMembership(ctx context.Context) error {
	for _, closure := range tx.workflowClosures {
		ids, err := tx.workflowEpisodeRuns(ctx, closure.TaskID, closure.StartOrdinal)
		if err != nil {
			return err
		}
		if !slices.Equal(closure.RunIDs, ids) {
			return ErrTaskWorkflowClosed
		}
	}
	return nil
}

// RequireProposalWorkflowOpen uses the proposal's authenticated target, not
// its display subject, to fence approval and effect dispatch.
func (tx *ReadTx) RequireProposalWorkflowOpen(ctx context.Context, id domain.ProposalInstanceID) error {
	instance, err := tx.GetProposalInstance(ctx, id)
	if err != nil {
		return err
	}
	subject, err := tx.gateProposalSubject(ctx, instance.Proposal)
	if err != nil {
		return err
	}
	// An adoption card has no run, so no run workflow fences it.
	if subject.declaration == nil {
		return nil
	}
	return tx.RequireRunWorkflowOpen(ctx, subject.declaration.RunID)
}

func (tx *ReadTx) ListTaskWorkflowClosures(ctx context.Context) ([]TaskWorkflowClosure, error) {
	rows, err := tx.tx.QueryContext(ctx, `SELECT task_id, start_ordinal FROM task_workflow_closures ORDER BY revision, task_id, start_ordinal`)
	if err != nil {
		return nil, err
	}
	var keys []TaskWorkflowClosure
	for rows.Next() {
		var key TaskWorkflowClosure
		if err := rows.Scan(&key.TaskID, &key.StartOrdinal); err != nil {
			_ = rows.Close()
			return nil, err
		}
		keys = append(keys, key)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	for i, key := range keys {
		closure, err := tx.GetTaskWorkflowClosure(ctx, key.TaskID, key.StartOrdinal)
		if err != nil {
			return nil, err
		}
		keys[i] = closure
	}
	return keys, nil
}
