package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// The queue holds distinct tasks: a task already queued or being named is not
// queued again. A full advisory queue leaves the identifier fallback; dispatch
// never waits for naming. The queue is process-local and deliberately not
// recovery state.
const taskNameQueueCapacity = 32

// maxTaskNameRunes bounds every stored task display name by Unicode code point
// (rune), the count the agent namer and the heading fallback already apply. The
// OpenAPI maxLength and the mock validator mirror this exact value.
const maxTaskNameRunes = 60

// errInvalidOperatorName marks an operator-supplied name that fails the canonical
// rule. Its messages never include the name text, because the text may be the
// secret the credential check refused; callers at the request boundary surface
// the message verbatim.
var errInvalidOperatorName = errors.New("operator task name")

// operatorTaskName canonicalizes an operator-supplied task name into the single
// bounded form the daemon stores, or rejects it. It trims surrounding whitespace
// (the one repair, since trailing whitespace is never intended), then requires
// one line of 1 to maxTaskNameRunes code points, valid UTF-8, with no
// credential-shaped token. An operator name is permanent (store.SetTaskName
// treats source operator as final), so a name that fails is refused rather than
// silently truncated or first-lined into a name the operator never chose.
func operatorTaskName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	switch {
	case name == "":
		return "", fmt.Errorf("%w is blank", errInvalidOperatorName)
	case !utf8.ValidString(name):
		return "", fmt.Errorf("%w is not valid UTF-8", errInvalidOperatorName)
	case strings.ContainsAny(name, "\r\n"):
		return "", fmt.Errorf("%w is multiline", errInvalidOperatorName)
	case utf8.RuneCountInString(name) > maxTaskNameRunes:
		return "", fmt.Errorf("%w is longer than %d characters", errInvalidOperatorName, maxTaskNameRunes)
	case importer.ContainsSecret([]byte(name)):
		return "", fmt.Errorf("%w contains a credential", errInvalidOperatorName)
	}
	return name, nil
}

// enqueueTaskName queues run's task for naming unless it is already queued or
// in the worker. Every dispatch pass re-enqueues the runs it touched, including
// ones it started earlier; that re-enqueue is what names a task after a
// fallback or a restart, and the dedupe keeps it from filling the queue with
// copies of one task. The send never blocks, so holding the mutex across it
// cannot stall dispatch. A full queue warns once until an enqueue succeeds.
func (e *Engine) enqueueTaskName(run domain.Run) {
	if !e.inference.SupportsSite(inference.TaskNamerSiteID) {
		return
	}
	e.taskNamesMu.Lock()
	defer e.taskNamesMu.Unlock()
	if _, active := e.taskNamesActive[run.TaskID]; active {
		return
	}
	select {
	case e.taskNames <- run:
		e.taskNamesActive[run.TaskID] = struct{}{}
		e.taskNamesFullWarned = false
	default:
		if !e.taskNamesFullWarned {
			e.logger.Warn("task naming queue full", "task_id", run.TaskID)
			e.taskNamesFullWarned = true
		}
	}
}

// runTaskNames is joined by Run before the daemon can close the store. One
// worker also respects the inference client's single in-flight call per site.
func (e *Engine) runTaskNames(ctx context.Context) {
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case run := <-e.taskNames:
			if err := e.nameTaskIfUnnamed(ctx, run); err != nil {
				e.logger.Warn("task naming unavailable", "task_id", run.TaskID, "error", err)
			}
			// Release after naming, not at dequeue, so a pass during the
			// inference call does not queue a second copy. This loop is the
			// queue's only reader, so every dequeued id is released.
			e.taskNamesMu.Lock()
			delete(e.taskNamesActive, run.TaskID)
			e.taskNamesMu.Unlock()
		}
	}
}

// taskHeadingName bounds display text without changing the separate commit-title contract.
func taskHeadingName(body []byte) (string, string) {
	title, failure := fallbackSpecificationTitle(body)
	count := 0
	for offset := range title {
		if count == maxTaskNameRunes {
			title = title[:offset]
			break
		}
		count++
	}
	return strings.TrimSpace(title), failure
}

// nameTaskIfUnnamed runs only after a durable dispatch, outside transactions.
// Its caller logs failures without changing the workflow's dispatch result.
func (e *Engine) nameTaskIfUnnamed(ctx context.Context, run domain.Run) error {
	if !e.inference.SupportsSite(inference.TaskNamerSiteID) || e.specification == nil {
		return nil
	}
	ctx, finish, err := e.beginTaskWork(ctx, run)
	if workflowAdvancementFenced(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer finish()
	var task domain.Task
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		task, err = tx.GetTask(ctx, run.TaskID)
		return err
	}); err != nil {
		return err
	}
	if task.Name.Source != domain.DisplayNameSourceIdentifier || task.Source == nil {
		return nil
	}
	source, err := e.readTaskSource(ctx, run, task, domain.InvocationID("task-namer-"+string(task.ID)),
		"Name the task from the issue's requested outcome")
	if err != nil {
		return err
	}
	input := inference.TaskNamerInput{
		Project: string(run.ProjectID), RootLineage: string(run.ID), SourceKind: string(task.Source.Kind),
		Repository: source.Repository, IssueNumber: source.IssueNumber,
		SourceText: source.Text, IssueTitle: source.IssueTitle, IssueBody: source.IssueBody,
	}
	truncateForSite(inference.TaskNamerSite(inference.Budget{}).MaxInputBytes,
		&input.IssueTitle, &input.SourceText, &input.IssueBody)
	if err := e.claimRunDispatch(ctx, run.ID); err != nil {
		if workflowAdvancementFenced(err) {
			return nil
		}
		return err
	}
	name, fallback, err := e.inference.NameTask(ctx, input)
	if err != nil || fallback {
		return err
	}
	err = e.store.Write(ctx, func(tx *store.WriteTx) error {
		current, err := tx.GetTask(ctx, task.ID)
		if err != nil || current.Name.Source != domain.DisplayNameSourceIdentifier {
			return err
		}
		return tx.SetTaskName(ctx, task.ID, domain.DisplayName{Text: name, Source: domain.DisplayNameSourceAgent})
	})
	if errors.Is(err, domain.ErrImmutableTransition) {
		return nil
	}
	return err
}

// taskSource is a task's source as a judgment site reads it: the work item's
// text, or the source issue's coordinates, title, body, and label names. A
// task with no source, or of another kind, reads as empty.
type taskSource struct {
	Text        string
	Repository  string
	IssueNumber string
	IssueTitle  string
	IssueBody   string
	IssueLabels []string
}

// errTaskSourceUnavailable marks a source the daemon could not read or
// fetch: the work item's blob, or the issue from GitHub. A failed store read
// is returned unmarked.
var errTaskSourceUnavailable = errors.New("task source unavailable")

// readTaskSource reads the task's source for a judgment site. An issue is
// fetched through the specification's research fetcher under invocation and
// purpose, inside the run's research allowlist and byte bound.
func (e *Engine) readTaskSource(
	ctx context.Context, run domain.Run, task domain.Task, invocation domain.InvocationID, purpose string,
) (taskSource, error) {
	var source taskSource
	if task.Source == nil {
		return source, nil
	}
	switch task.Source.Kind {
	case domain.SpecificationSourceWorkItemArtifact:
		var artifact domain.Artifact
		if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			artifact, err = tx.GetArtifact(ctx, task.Source.WorkItemArtifactID)
			return err
		}); err != nil {
			return taskSource{}, err
		}
		body, err := readBoundedArtifactBlob(e.specification.blobs, artifact.Digest, exec.ProductionMaxInputBytes)
		if err != nil {
			return taskSource{}, fmt.Errorf("%w: %w", errTaskSourceUnavailable, err)
		}
		source.Text = string(body)
	case domain.SpecificationSourceIssueSubject:
		return e.readIssueSource(ctx, run, *task.Source.IssueSubject, invocation, purpose)
	}
	return source, nil
}

// readIssueSource fetches the issue's title, body, and label names through
// the specification's research fetcher under invocation and purpose, inside
// the run's research allowlist and byte bound. It fetches the repository-ID
// endpoint built from issue, so the request names the repository by its
// numeric identity, never by a URL a submitter wrote.
func (e *Engine) readIssueSource(
	ctx context.Context, run domain.Run, issue domain.IssueSubjectRef, invocation domain.InvocationID, purpose string,
) (taskSource, error) {
	var resolved domain.ResolvedPolicy
	var scope specify.ManagedRepository
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if resolved, err = tx.GetResolvedPolicy(ctx, run.ID); err != nil {
			return err
		}
		scope, err = researchScope(ctx, tx, run.ProjectID)
		return err
	}); err != nil {
		return taskSource{}, err
	}
	settings, err := specify.ParsePolicy(resolved)
	if err != nil {
		return taskSource{}, err
	}
	source := taskSource{Repository: issue.Repo, IssueNumber: strconv.Itoa(issue.IssueNumber)}
	artifact, err := e.specification.fetcher.Fetch(ctx, invocation, 1,
		specify.FetchRequest{
			URL:     fmt.Sprintf("https://api.github.com/repositories/%d/issues/%d", issue.RepositoryID, issue.IssueNumber),
			Purpose: purpose,
		}, scope, settings.ResearchAllowlist, settings.ResearchMaxBytes)
	if err != nil {
		return taskSource{}, fmt.Errorf("%w: %w", errTaskSourceUnavailable, err)
	}
	body, err := readBoundedArtifactBlob(e.specification.blobs, artifact.Artifact.Digest, exec.ProductionMaxInputBytes)
	if err != nil {
		return taskSource{}, fmt.Errorf("%w: %w", errTaskSourceUnavailable, err)
	}
	evidence, err := specify.DecodeResearchEvidence(body)
	if err != nil {
		return taskSource{}, fmt.Errorf("%w: %w", errTaskSourceUnavailable, err)
	}
	var content struct {
		Title  string `json:"title"`
		Body   string `json:"body"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal([]byte(evidence.Body), &content); err != nil {
		return taskSource{}, fmt.Errorf("%w: decode issue research: %w", errTaskSourceUnavailable, err)
	}
	source.IssueTitle, source.IssueBody = content.Title, content.Body
	for _, label := range content.Labels {
		source.IssueLabels = append(source.IssueLabels, label.Name)
	}
	return source, nil
}

// truncateForSite cuts the fields, in order, to UTF-8 prefixes that fit a
// site's input bound. JSON can expand each input byte to six bytes, so it
// reserves room for field names and coordinates and shares the rest out in
// the order given.
func truncateForSite(maxInputBytes int, fields ...*string) {
	remaining := maxInputBytes/6 - 1024
	for _, field := range fields {
		if len(*field) > remaining {
			end := remaining
			for end > 0 && !utf8.RuneStart((*field)[end]) {
				end--
			}
			*field = (*field)[:end]
		}
		remaining -= len(*field)
	}
}
