package engine

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// triageSourceUnavailable is a failed call's reason when the task's source
// could not be read or fetched. Like the client's fallback reasons, it is a
// daemon-authored code, never provider or fetch text.
const triageSourceUnavailable = "source_unavailable"

// triageSpecifier runs the triager once for the task of the specifier's run,
// before the specifier's first admission and before the router (plan §5.13,
// Specifier Triage). It runs only when the specifier's lineup line names an
// escalated counterpart for its own agent, the specifier has no line version
// (an operator line, even a cleared one, skips it), the task has no triage
// record, and no run of the task has admitted the specifier. It records every
// outcome; an escalation writes the specifier's triage line naming that
// counterpart, which admission then anchors. Later admissions read the record
// and never call again.
//
// A failed call, a refused answer, a spent budget, and a lineup with no
// triager line each escalate with their outcome, so an unavailable triager
// never puts a complex task on the smaller model. Only a failed store read or
// write returns an error, which fails the admission for a later retry.
func (e *Engine) triageSpecifier(
	ctx context.Context, selection AgentSelection, runID domain.RunID, now func() time.Time,
) error {
	// The run is read back, as admission reads it, for the task it is bound
	// to; admission refuses a run with no task.
	var run domain.Run
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		run, err = tx.GetRun(ctx, runID)
		return err
	}); err != nil {
		return err
	}
	if run.TaskID == "" {
		return nil
	}
	lineup, err := selection.Tree.ResolveLineup()
	if err != nil {
		// Admission refuses a lineup that does not resolve; triage has
		// nothing to add to that refusal.
		return nil
	}
	line, ok := lineup.Line(domain.RoleSpecifier)
	if !ok {
		return nil
	}
	// Escalation keeps the specifier line's enrollment and changes its
	// model and effort, so the counterpart is the one the lineup names for
	// that enrollment.
	counterpart, ok := lineup.EscalatedCounterpart(domain.RoleSpecifier, line.Names.Enrollment)
	if !ok {
		return nil
	}
	if needed, err := e.specifierTriageNeeded(ctx, run.TaskID); err != nil || !needed {
		return err
	}
	// The triage line selects what the counterpart names. Whether it
	// resolves against the enrollment's catalog is admission's to decide,
	// which refuses it rather than leave a complex task on the smaller model.
	counterpartSelection := counterpart.TaskLineSelection()
	// One triage call at a time: the inference client refuses a second call
	// at a site while one is in flight, which would escalate a task for no
	// reason of its own. The check repeats under the lock, so a task another
	// admission triaged meanwhile is not triaged twice.
	e.triageMu.Lock()
	defer e.triageMu.Unlock()
	if needed, err := e.specifierTriageNeeded(ctx, run.TaskID); err != nil || !needed {
		return err
	}
	record, err := e.callTriager(ctx, run, lineup)
	if err != nil {
		return err
	}
	record.RecordedAt = now()
	var escalateTo *domain.TaskLineSelection
	if record.Escalated {
		escalateTo = &counterpartSelection
	}
	err = e.store.Write(ctx, func(tx *store.WriteTx) error {
		_, err := tx.RecordSpecifierTriage(ctx, record, escalateTo)
		return err
	})
	// Another path recorded the task's triage, or set the specifier's line,
	// after the check: the admission reads what it wrote.
	if errors.Is(err, domain.ErrDuplicate) || errors.Is(err, domain.ErrTaskLineInconsistent) {
		return nil
	}
	if err != nil {
		return err
	}
	e.logger.Info("specifier triaged", "task_id", record.TaskID, "run_id", run.ID,
		"outcome", record.Outcome, "escalated", record.Escalated)
	return nil
}

// specifierTriageNeeded reports whether the task still needs its triage: no
// record, no specifier line version, and no admitted specifier attempt.
func (e *Engine) specifierTriageNeeded(ctx context.Context, taskID domain.TaskID) (bool, error) {
	needed := false
	err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		if _, found, err := tx.GetTaskTriage(ctx, taskID); err != nil || found {
			return err
		}
		lines, err := tx.TaskLines(ctx, taskID)
		if err != nil {
			return err
		}
		for _, line := range lines {
			if line.Role == domain.RoleSpecifier {
				return nil
			}
		}
		admitted, err := specifierAdmitted(ctx, tx, taskID)
		if err != nil || admitted {
			return err
		}
		needed = true
		return nil
	})
	return needed, err
}

// specifierAdmitted reports whether any run of the task holds an execution
// admission of its specification stage. A task whose specifier ran before
// triage applied to it keeps the agent it ran on.
func specifierAdmitted(ctx context.Context, tx *store.ReadTx, taskID domain.TaskID) (bool, error) {
	runIDs, err := tx.TaskRunIDs(ctx, taskID)
	if err != nil {
		return false, err
	}
	for _, runID := range runIDs {
		admissions, err := tx.ListRunExecutionAdmissionRecords(ctx, runID)
		if err != nil {
			return false, err
		}
		for _, admission := range admissions {
			if admission.StageID == specificationStageID(runID) {
				return true, nil
			}
		}
	}
	return false, nil
}

// callTriager asks the triager for the task's tier and returns the record of
// what the call came to, without its instant. A lineup with no triager line
// is checked before any call.
func (e *Engine) callTriager(ctx context.Context, run domain.Run, lineup domain.Lineup) (domain.TaskTriage, error) {
	failed := func(outcome domain.TriageOutcome, reason string) domain.TaskTriage {
		return domain.TaskTriage{
			TaskID: run.TaskID, Outcome: outcome, Reason: &reason,
			Escalated: domain.TriageEscalates(outcome, nil),
		}
	}
	if _, ok := lineup.Line(domain.RoleTriager); !ok {
		return domain.TaskTriage{
			TaskID: run.TaskID, Outcome: domain.TriageOutcomeNoTriagerLine,
			Escalated: domain.TriageEscalates(domain.TriageOutcomeNoTriagerLine, nil),
		}, nil
	}
	if !e.inference.SupportsSite(inference.TriagerSiteID) {
		return failed(domain.TriageOutcomeNoTriagerLine, string(inference.FallbackRoleSourceAbsent)), nil
	}
	var (
		task    domain.Task
		project domain.Project
	)
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if task, err = tx.GetTask(ctx, run.TaskID); err != nil {
			return err
		}
		// A task submitted for a repository label intake never saw has no
		// project row; the call then names no project, as research
		// scoping treats the same run (researchScope).
		project, err = tx.GetProject(ctx, run.ProjectID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}); err != nil {
		return domain.TaskTriage{}, err
	}
	var source taskSource
	if e.specification != nil {
		var err error
		source, err = e.readTriageSource(ctx, run, task, domain.InvocationID("task-triager-"+string(task.ID)),
			"Choose the specifier's tier from the issue's requested outcome")
		if errors.Is(err, errTaskSourceUnavailable) {
			e.logger.Warn("specifier triage source unavailable", "task_id", task.ID, "error", err)
			return failed(domain.TriageOutcomeCallFailed, triageSourceUnavailable), nil
		}
		if err != nil {
			return domain.TaskTriage{}, err
		}
	}
	input := inference.TriagerInput{
		Project: string(run.ProjectID), RootLineage: string(run.ID), ProjectName: project.Repo,
		IssueTitle: source.IssueTitle, IssueBody: source.IssueBody,
		IssueLabels: strings.Join(source.IssueLabels, "\n"), SourceText: source.Text,
	}
	truncateForSite(inference.TriagerSite(inference.Budget{}).MaxInputBytes,
		&input.IssueTitle, &input.IssueLabels, &input.SourceText, &input.IssueBody)
	answer, reason, err := e.inference.TriageSpecifier(ctx, input)
	if err != nil {
		return domain.TaskTriage{}, err
	}
	if reason != "" {
		return failed(inference.TriageFallbackOutcome(reason), string(reason)), nil
	}
	return domain.TaskTriage{
		TaskID: run.TaskID, Outcome: domain.TriageOutcomeAnswered, Tier: &answer.Tier, Reason: &answer.Reason,
		Escalated: domain.TriageEscalates(domain.TriageOutcomeAnswered, &answer.Tier),
	}, nil
}

// readTriageSource reads the task's source as readTaskSource does, except
// that a submitted task whose text is exactly one canonical issue URL in the
// project's managed repository reads as that issue: its title, body, and
// label names, with no source text, as an issue-subject task reads. Without
// this, triage sees only the URL, which it treats as a thin source and
// escalates (#3177). The issue is fetched by the project's repository ID,
// never by the submitted URL, and the owner/name comparison ignores case as
// the research fetcher's scope check does. Any other text, a URL for another
// repository, or a project with no registered repository reads as submitted.
func (e *Engine) readTriageSource(
	ctx context.Context, run domain.Run, task domain.Task, invocation domain.InvocationID, purpose string,
) (taskSource, error) {
	source, err := e.readTaskSource(ctx, run, task, invocation, purpose)
	if err != nil || task.Source == nil || task.Source.Kind != domain.SpecificationSourceWorkItemArtifact {
		return source, err
	}
	ownerRepo, number, ok := parseSourceIssueURL(TaskSourceIssue([]byte(source.Text)))
	if !ok {
		return source, nil
	}
	var scope specify.ManagedRepository
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		scope, err = researchScope(ctx, tx, run.ProjectID)
		return err
	}); err != nil {
		return taskSource{}, err
	}
	if scope.RepositoryID <= 0 || !strings.EqualFold(ownerRepo, scope.Repo) {
		return source, nil
	}
	return e.readIssueSource(ctx, run, domain.IssueSubjectRef{
		Repo: scope.Repo, RepositoryID: scope.RepositoryID, IssueNumber: number,
	}, invocation, purpose)
}
