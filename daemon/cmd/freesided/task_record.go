package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// taskRecordVersion names the record's JSON shape. Any change to that shape,
// including to signet.TaskTimeline, which the record embeds, changes it.
const taskRecordVersion = "freeside-task-record-v1"

// taskRecord is what `freesided inspect task` prints: one task's recorded
// facts, read under one store revision, for an agent to read. It is built
// here, not in internal/observe, because that package may not import the
// store.
//
// A section that was not read renders null, and the error beside it says
// why. An empty slice means "read, and there are none".
type taskRecord struct {
	Version       string               `json:"version"`
	AsOfRevision  int64                `json:"as_of_revision"`
	AsOf          time.Time            `json:"as_of"`
	TaskID        domain.TaskID        `json:"task_id"`
	Task          *taskRecordTask      `json:"task"`
	TaskError     *taskRecordError     `json:"task_error"`
	Timeline      *signet.TaskTimeline `json:"timeline"`
	TimelineError *taskRecordError     `json:"timeline_error"`
	Runs          []taskRecordRun      `json:"runs"`
	Items         []taskRecordItem     `json:"items"`
	ItemsError    *taskRecordError     `json:"items_error"`
	Counts        taskRecordCounts     `json:"counts"`
}

// taskRecordTask lists the task's fields instead of embedding domain.Task,
// which also carries the specification source.
type taskRecordTask struct {
	ProjectID      domain.ProjectID           `json:"project_id"`
	CreatedAt      time.Time                  `json:"created_at"`
	LifecycleFacts []domain.TaskLifecycleFact `json:"lifecycle_facts"`
	Cancellation   *taskRecordCancellation    `json:"cancellation"`
}

type taskRecordCancellation struct {
	RequestID       string                       `json:"request_id"`
	State           domain.TaskCancellationState `json:"state"`
	RequestedAt     time.Time                    `json:"requested_at"`
	Acknowledgement *taskRecordAcknowledgement   `json:"acknowledgement"`
}

type taskRecordAcknowledgement struct {
	State          domain.TaskCancellationState `json:"state"`
	EvidenceDigest domain.Digest                `json:"evidence_digest"`
	RecordedAt     time.Time                    `json:"recorded_at"`
}

// taskRecordRun holds one run's facts. Each is read on its own, so a failed
// check leaves the others in place. A null field was not read exactly when
// Errors names its section; otherwise null means the run has none.
type taskRecordRun struct {
	RunID                domain.RunID                   `json:"run_id"`
	Conclusion           *taskRecordConclusion          `json:"conclusion"`
	Hold                 *domain.RunHoldObservation     `json:"hold"`
	Invocations          []domain.InvocationObservation `json:"invocations"`
	Admissions           []taskRecordAdmission          `json:"admissions"`
	ResolvedPolicyDigest *domain.Digest                 `json:"resolved_policy_digest"`
	BillableCost         *domain.CostSoFar              `json:"billable_cost"`
	Errors               []taskRecordRunError           `json:"errors"`
}

// taskRecordRunSection names one of a run's reads.
type taskRecordRunSection string

const (
	taskRecordRunAdmissions taskRecordRunSection = "admissions"
	taskRecordRunPolicy     taskRecordRunSection = "resolved_policy_digest"
	taskRecordRunCost       taskRecordRunSection = "billable_cost"
	// taskRecordRunObservation covers Hold and Invocations, which one read
	// returns.
	taskRecordRunObservation taskRecordRunSection = "observation"
	taskRecordRunConclusion  taskRecordRunSection = "conclusion"
)

var allTaskRecordRunSections = []taskRecordRunSection{
	taskRecordRunAdmissions, taskRecordRunPolicy, taskRecordRunCost,
	taskRecordRunObservation, taskRecordRunConclusion,
}

func (s taskRecordRunSection) valid() bool {
	switch s {
	case taskRecordRunAdmissions, taskRecordRunPolicy, taskRecordRunCost,
		taskRecordRunObservation, taskRecordRunConclusion:
		return true
	default:
		return false
	}
}

type taskRecordRunError struct {
	Section taskRecordRunSection `json:"section"`
	Kind    taskRecordErrorKind  `json:"kind"`
	Message string               `json:"message"`
}

type taskRecordConclusion struct {
	Outcome        domain.RunOutcome                `json:"outcome"`
	HoldReason     *domain.RunHoldReason            `json:"hold_reason"`
	TerminalStatus *domain.ObservedInvocationStatus `json:"terminal_status"`
	Final          bool                             `json:"final"`
}

// taskRecordAdmission is the governing identity of one recorded admission.
// It leaves out the workspace, the auth identity, and the binding's
// enrollment and store-manifest fields.
type taskRecordAdmission struct {
	ID                 domain.Digest         `json:"id"`
	InvocationID       domain.InvocationID   `json:"invocation_id"`
	StageID            domain.StageID        `json:"stage_id"`
	AttemptID          domain.AttemptID      `json:"attempt_id"`
	AdmittedAt         time.Time             `json:"admitted_at"`
	OperatingMode      domain.OperatingMode  `json:"operating_mode"`
	CredentialMode     domain.CredentialMode `json:"credential_mode"`
	EgressProfile      domain.EgressProfile  `json:"egress_profile"`
	ImageRef           domain.ImageRef       `json:"image_ref"`
	Base               domain.BaseRevision   `json:"base"`
	SpecDigest         domain.Digest         `json:"spec_digest"`
	PolicyDigest       domain.Digest         `json:"policy_digest"`
	InputDigest        domain.Digest         `json:"input_digest"`
	TrustProfileDigest *domain.Digest        `json:"trust_profile_digest"`
	Agent              *taskRecordAgent      `json:"agent"`
}

type taskRecordAgent struct {
	AgentDigest     domain.Digest `json:"agent_digest"`
	LaunchDigest    domain.Digest `json:"launch_digest"`
	TreatmentDigest domain.Digest `json:"treatment_digest"`
	LineupRevision  domain.Digest `json:"lineup_revision"`
	RouteModelID    *string       `json:"route_model_id"`
}

// taskRecordItem leaves out the item's evidence and claims, and its reason:
// several writers compose the reason from a claim's text, an agent's
// question, a reviewer's finding, or a driver error that names host paths.
// The typed causes say why the item exists.
type taskRecordItem struct {
	ID               domain.ItemID                 `json:"id"`
	Type             domain.AttentionType          `json:"type"`
	Status           domain.ItemStatus             `json:"status"`
	RunID            *domain.RunID                 `json:"run_id"`
	CreatedAt        *time.Time                    `json:"created_at"`
	DecidedAt        *time.Time                    `json:"decided_at"`
	HealthDiagnostic *domain.HealthDiagnostic      `json:"health_diagnostic"`
	ExecutionFailure *domain.ExecutionFailureFacts `json:"execution_failure"`
	PublishBlock     *domain.PublishBlockFacts     `json:"publish_block"`
	BlockedOn        *domain.BlockedWait           `json:"blocked_on"`
	Commands         []taskRecordCommand           `json:"commands"`
	Deliveries       []taskRecordDelivery          `json:"deliveries"`
}

// taskRecordCommand leaves out the command's message and attachments, which
// are conversation content.
type taskRecordCommand struct {
	CommandID string          `json:"command_id"`
	Action    domain.Action   `json:"action"`
	DeviceID  domain.DeviceID `json:"device_id"`
}

type taskRecordDelivery struct {
	DeviceID          domain.DeviceID       `json:"device_id"`
	Channel           string                `json:"channel"`
	Attempt           int                   `json:"attempt"`
	Status            domain.DeliveryStatus `json:"status"`
	SubmittedAt       time.Time             `json:"submitted_at"`
	ChannelAcceptedAt *time.Time            `json:"channel_accepted_at"`
	OpenedAt          *time.Time            `json:"opened_at"`
}

// taskRecordCounts counts what the record holds. The two item counts are
// null when the items were not read, and FailedStops when the task was not.
// RunsHeld counts the holds that were read.
type taskRecordCounts struct {
	ItemsByType         []taskRecordTypeCount `json:"items_by_type"`
	HealthNoticesByCode []taskRecordCodeCount `json:"health_notices_by_code"`
	RunsHeld            int                   `json:"runs_held"`
	FailedStops         *int                  `json:"failed_stops"`
}

type taskRecordTypeCount struct {
	Type  domain.AttentionType `json:"type"`
	Count int                  `json:"count"`
}

type taskRecordCodeCount struct {
	Code  string `json:"code"`
	Count int    `json:"count"`
}

// taskRecordErrorKind says which class of check left a section unread.
type taskRecordErrorKind string

const (
	// taskRecordErrorIntegrity is a stored row that contradicts its own
	// history or a row it is bound to.
	taskRecordErrorIntegrity taskRecordErrorKind = "integrity"
	// taskRecordErrorUnapprovedRecipe is evidence the reading store does not
	// approve: the usual cause is a read with no daemon and no
	// -approved-recipe.
	taskRecordErrorUnapprovedRecipe taskRecordErrorKind = "unapproved_recipe"
	// taskRecordErrorOther is a failure the store does not classify, a
	// database error included. The record reports it instead of failing: the
	// message is in the output either way, and failing would drop the whole
	// record for a check the store leaves untyped.
	taskRecordErrorOther taskRecordErrorKind = "other"
)

var allTaskRecordErrorKinds = []taskRecordErrorKind{
	taskRecordErrorIntegrity, taskRecordErrorUnapprovedRecipe, taskRecordErrorOther,
}

func (k taskRecordErrorKind) valid() bool {
	switch k {
	case taskRecordErrorIntegrity, taskRecordErrorUnapprovedRecipe, taskRecordErrorOther:
		return true
	default:
		return false
	}
}

type taskRecordError struct {
	Kind    taskRecordErrorKind `json:"kind"`
	Message string              `json:"message"`
}

func newTaskRecordError(err error) *taskRecordError {
	kind := taskRecordErrorOther
	switch {
	case errors.Is(err, domain.ErrUnapprovedRecipe):
		kind = taskRecordErrorUnapprovedRecipe
	case errors.Is(err, signet.ErrRunObservationIntegrity), store.IsRowVerdict(err):
		kind = taskRecordErrorIntegrity
	}
	return &taskRecordError{Kind: kind, Message: err.Error()}
}

// readTaskRecord builds the record in the caller's transaction, so every
// section is read at one store revision. Only an unknown task and a failed
// transaction fail it: a section that fails a store check is reported in the
// record instead, so a broken task still reads.
func readTaskRecord(ctx context.Context, tx *store.ReadTx, id domain.TaskID, asOf time.Time) (taskRecord, error) {
	state, err := tx.ServerState(ctx)
	if err != nil {
		return taskRecord{}, err
	}
	record := taskRecord{Version: taskRecordVersion, AsOfRevision: state.Revision, AsOf: asOf, TaskID: id}
	switch task, err := tx.GetTask(ctx, id); {
	case err == nil:
		projected := projectTaskRecordTask(task)
		record.Task = &projected
	case errors.Is(err, store.ErrNotFound):
		// The store also answers not-found for a task whose own rows are
		// incomplete; no read tells the two apart.
		return taskRecord{}, fmt.Errorf("task %q: %w", id, err)
	default:
		record.TaskError = newTaskRecordError(err)
	}
	if timeline, err := signet.ReadTaskTimeline(ctx, tx, id, asOf); err != nil {
		record.TimelineError = newTaskRecordError(err)
	} else {
		record.Timeline = &timeline
	}
	runIDs, err := tx.TaskRunIDs(ctx, id)
	if err != nil {
		return taskRecord{}, err
	}
	record.Runs = make([]taskRecordRun, 0, len(runIDs))
	for _, runID := range runIDs {
		run := readTaskRecordRun(ctx, tx, runID)
		if run.Hold != nil {
			record.Counts.RunsHeld++
		}
		record.Runs = append(record.Runs, run)
	}
	if record.Items, err = readTaskRecordItems(ctx, tx, id, record.Runs); err != nil {
		record.Items, record.ItemsError = nil, newTaskRecordError(err)
	} else {
		record.Counts.ItemsByType, record.Counts.HealthNoticesByCode = countTaskRecordItems(record.Items)
	}
	if record.Task != nil {
		// The store keeps the latest cancellation only, so this is 0 or 1.
		failedStops := 0
		if c := record.Task.Cancellation; c != nil && c.Acknowledgement != nil &&
			c.Acknowledgement.State == domain.TaskCancellationFailed {
			failedStops = 1
		}
		record.Counts.FailedStops = &failedStops
	}
	// A cancelled or expired read fails every later section the same way;
	// that is a failed transaction, not a finding about the task.
	if err := ctx.Err(); err != nil {
		return taskRecord{}, err
	}
	return record, nil
}

func projectTaskRecordTask(task domain.Task) taskRecordTask {
	out := taskRecordTask{
		ProjectID: task.ProjectID, CreatedAt: task.CreatedAt,
		LifecycleFacts: append([]domain.TaskLifecycleFact{}, task.LifecycleFacts...),
	}
	if c := task.Cancellation; c != nil {
		out.Cancellation = &taskRecordCancellation{RequestID: c.RequestID, State: c.State, RequestedAt: c.RequestedAt}
		if a := c.Acknowledgement; a != nil {
			out.Cancellation.Acknowledgement = &taskRecordAcknowledgement{
				State: a.State, EvidenceDigest: a.EvidenceDigest, RecordedAt: a.RecordedAt,
			}
		}
	}
	return out
}

func readTaskRecordRun(ctx context.Context, tx *store.ReadTx, runID domain.RunID) taskRecordRun {
	out := taskRecordRun{RunID: runID, Errors: []taskRecordRunError{}}
	fail := func(section taskRecordRunSection, err error) {
		classified := newTaskRecordError(err)
		out.Errors = append(out.Errors, taskRecordRunError{
			Section: section, Kind: classified.Kind, Message: classified.Message,
		})
	}
	// The record read, not the re-gated one: a later policy change must not
	// hide what a run was admitted under. It scans every run's admissions,
	// so one unreadable admission row fails this section for every run.
	if admissions, err := tx.ListRunExecutionAdmissionRecords(ctx, runID); err != nil {
		fail(taskRecordRunAdmissions, err)
	} else {
		out.Admissions = make([]taskRecordAdmission, 0, len(admissions))
		for _, admission := range admissions {
			out.Admissions = append(out.Admissions, projectTaskRecordAdmission(admission))
		}
	}
	switch policy, err := tx.GetResolvedPolicy(ctx, runID); {
	case err == nil:
		out.ResolvedPolicyDigest = &policy.Digest
	case !errors.Is(err, store.ErrNotFound):
		fail(taskRecordRunPolicy, err)
	}
	if cost, err := tx.BillableCostSoFar(ctx, runID); err != nil {
		fail(taskRecordRunCost, err)
	} else {
		out.BillableCost = cost
	}
	observation, err := tx.ObserveRun(ctx, runID)
	if err != nil {
		// The conclusion is classified from the observation.
		fail(taskRecordRunObservation, err)
		fail(taskRecordRunConclusion, err)
		return out
	}
	out.Hold = observation.Hold
	out.Invocations = append([]domain.InvocationObservation{}, observation.Invocations...)
	run, err := tx.GetRun(ctx, runID)
	if err != nil {
		fail(taskRecordRunConclusion, err)
		return out
	}
	conclusion, err := authenticatedTaskRecordConclusion(ctx, tx, run, observation)
	if err != nil {
		fail(taskRecordRunConclusion, err)
		return out
	}
	out.Conclusion = &taskRecordConclusion{
		Outcome: conclusion.Outcome, HoldReason: conclusion.Reason,
		TerminalStatus: conclusion.Terminal, Final: conclusion.Final,
	}
	return out
}

// authenticatedTaskRecordConclusion classifies a run the way `freesided
// follow` does (observedb's authenticateRunConclusion), so the two reads
// cannot disagree about one run. A milestone-only domain.ConcludeRun would
// report a block that a later accepted reevaluation resolved, and a
// completion nothing authenticates.
func authenticatedTaskRecordConclusion(
	ctx context.Context, tx *store.ReadTx, run domain.Run, observation domain.RunObservation,
) (domain.RunConclusion, error) {
	identity, completed, err := engine.ProductionPublicationCompletion(ctx, tx, run)
	if err != nil {
		return domain.RunConclusion{}, err
	}
	readyAuthenticated := false
	if completed {
		readyAuthenticated, err = engine.AuthenticateProductionPublicationReady(ctx, tx, run, observation, identity)
		if err != nil {
			return domain.RunConclusion{}, err
		}
	}
	return signet.AuthenticatedRunConclusion(ctx, tx, run, observation, readyAuthenticated)
}

func projectTaskRecordAdmission(admission domain.ExecutionAdmission) taskRecordAdmission {
	out := taskRecordAdmission{
		ID: admission.ID, InvocationID: admission.InvocationID,
		StageID: admission.StageID, AttemptID: admission.AttemptID, AdmittedAt: admission.AdmittedAt,
		OperatingMode: admission.OperatingMode, CredentialMode: admission.CredentialMode,
		EgressProfile: admission.EgressProfile, ImageRef: admission.ImageRef, Base: admission.Base,
		SpecDigest: admission.SpecDigest, PolicyDigest: admission.PolicyDigest, InputDigest: admission.InputDigest,
		TrustProfileDigest: admission.TrustProfileDigest,
	}
	if binding := admission.AgentBinding; binding != nil {
		out.Agent = &taskRecordAgent{
			AgentDigest: binding.AgentDigest, LaunchDigest: binding.LaunchDigest,
			TreatmentDigest: binding.TreatmentDigest, LineupRevision: binding.LineupRevision,
		}
		if binding.RouteModelID != "" {
			out.Agent.RouteModelID = &binding.RouteModelID
		}
	}
	return out
}

// readTaskRecordItems returns every attention item that belongs to the task,
// open or closed, in item-id order. The store has no read of items by task,
// so this filters the whole list. ListAttentionItems re-runs the evidence
// gate on every row and fails as a whole when one row fails.
func readTaskRecordItems(
	ctx context.Context, tx *store.ReadTx, id domain.TaskID, runs []taskRecordRun,
) ([]taskRecordItem, error) {
	stored, err := tx.ListAttentionItems(ctx)
	if err != nil {
		return nil, err
	}
	deliveries, err := tx.ListAttentionDeliveries(ctx)
	if err != nil {
		return nil, err
	}
	deliveriesByItem := make(map[domain.ItemID][]taskRecordDelivery)
	for _, delivery := range deliveries {
		d := delivery.Value
		deliveriesByItem[d.ItemID] = append(deliveriesByItem[d.ItemID], taskRecordDelivery{
			DeviceID: d.DeviceID, Channel: d.Channel, Attempt: d.Attempt, Status: d.Status,
			SubmittedAt: d.SubmittedAt, ChannelAcceptedAt: d.ChannelAcceptedAt, OpenedAt: d.OpenedAt,
		})
	}
	items := []taskRecordItem{}
	for _, snapshot := range stored {
		item := snapshot.Value
		if !taskRecordOwnsItem(item, id, runs) {
			continue
		}
		commands, err := tx.ListCommandsForItem(ctx, item.ID)
		if err != nil {
			return nil, err
		}
		out := taskRecordItem{
			ID: item.ID, Type: item.Type, Status: item.Status, RunID: item.Subject.RunID,
			CreatedAt: item.CreatedAt, DecidedAt: item.DecidedAt,
			HealthDiagnostic: item.HealthDiagnostic, ExecutionFailure: item.ExecutionFailure,
			PublishBlock: item.PublishBlock, BlockedOn: item.BlockedOn,
			Commands:   make([]taskRecordCommand, 0, len(commands)),
			Deliveries: append([]taskRecordDelivery{}, deliveriesByItem[item.ID]...),
		}
		for _, command := range commands {
			out.Commands = append(out.Commands, taskRecordCommand{
				CommandID: command.CommandID, Action: command.Action, DeviceID: command.DeviceID,
			})
		}
		items = append(items, out)
	}
	return items, nil
}

// taskRecordOwnsItem reports whether an item belongs to the task: its
// subject names the task or one of its runs, or it is the stall notice of
// one of the task's invocations. A stall notice has the system as its
// subject, so only its id ties it to an invocation; the invocation ids come
// from the run's admissions and observations, whichever were read.
func taskRecordOwnsItem(item domain.AttentionItem, id domain.TaskID, runs []taskRecordRun) bool {
	if item.Subject.TaskID != nil && *item.Subject.TaskID == id {
		return true
	}
	for _, run := range runs {
		if item.Subject.RunID != nil && *item.Subject.RunID == run.RunID {
			return true
		}
		for _, admission := range run.Admissions {
			if isInvocationStallNotice(string(item.ID), admission.InvocationID) {
				return true
			}
		}
		for _, invocation := range run.Invocations {
			if isInvocationStallNotice(string(item.ID), invocation.InvocationID) {
				return true
			}
		}
	}
	return false
}

// countTaskRecordItems counts items by type, listing every type so a zero is
// explicit, and health notices by code, listing only the codes that occur.
func countTaskRecordItems(items []taskRecordItem) ([]taskRecordTypeCount, []taskRecordCodeCount) {
	byType := make([]taskRecordTypeCount, len(domain.AllAttentionTypes))
	for index, attentionType := range domain.AllAttentionTypes {
		byType[index].Type = attentionType
	}
	byCode := []taskRecordCodeCount{}
	for _, item := range items {
		if index := slices.Index(domain.AllAttentionTypes, item.Type); index >= 0 {
			byType[index].Count++
		}
		if item.HealthDiagnostic == nil {
			continue
		}
		code := item.HealthDiagnostic.Code
		index := slices.IndexFunc(byCode, func(c taskRecordCodeCount) bool { return c.Code == code })
		if index < 0 {
			byCode = append(byCode, taskRecordCodeCount{Code: code})
			index = len(byCode) - 1
		}
		byCode[index].Count++
	}
	slices.SortFunc(byCode, func(a, b taskRecordCodeCount) int { return cmp.Compare(a.Code, b.Code) })
	return byType, byCode
}
