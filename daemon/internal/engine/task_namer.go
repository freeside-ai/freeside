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

// A full advisory queue leaves the identifier fallback; dispatch never waits
// for naming. The queue is process-local and deliberately not recovery state.
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

func (e *Engine) enqueueTaskName(run domain.Run) {
	if !e.inference.SupportsSite(inference.TaskNamerSiteID) {
		return
	}
	select {
	case e.taskNames <- run:
	default:
		e.logger.Warn("task naming queue full", "task_id", run.TaskID)
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
	if errors.Is(err, store.ErrTaskCancellationFenced) {
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
	input := inference.TaskNamerInput{
		Project: string(run.ProjectID), RootLineage: string(run.ID), SourceKind: string(task.Source.Kind),
	}
	switch task.Source.Kind {
	case domain.SpecificationSourceWorkItemArtifact:
		var artifact domain.Artifact
		if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			artifact, err = tx.GetArtifact(ctx, task.Source.WorkItemArtifactID)
			return err
		}); err != nil {
			return err
		}
		body, err := readBoundedArtifactBlob(e.specification.blobs, artifact.Digest, exec.ProductionMaxInputBytes)
		if err != nil {
			return err
		}
		input.SourceText = string(body)
	case domain.SpecificationSourceIssueSubject:
		var resolved domain.ResolvedPolicy
		if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			resolved, err = tx.GetResolvedPolicy(ctx, run.ID)
			return err
		}); err != nil {
			return err
		}
		settings, err := specify.ParsePolicy(resolved)
		if err != nil {
			return err
		}
		issue := task.Source.IssueSubject
		input.Repository, input.IssueNumber = issue.Repo, strconv.Itoa(issue.IssueNumber)
		artifact, err := e.specification.fetcher.Fetch(ctx, domain.InvocationID("task-namer-"+string(task.ID)), 1,
			specify.FetchRequest{
				URL:     fmt.Sprintf("https://api.github.com/repositories/%d/issues/%d", issue.RepositoryID, issue.IssueNumber),
				Purpose: "Name the task from the issue's requested outcome",
			}, settings.ResearchAllowlist, settings.ResearchMaxBytes)
		if err != nil {
			return err
		}
		body, err := readBoundedArtifactBlob(e.specification.blobs, artifact.Artifact.Digest, exec.ProductionMaxInputBytes)
		if err != nil {
			return err
		}
		evidence, err := specify.DecodeResearchEvidence(body)
		if err != nil {
			return err
		}
		var content struct {
			Title string `json:"title"`
			Body  string `json:"body"`
		}
		if err := json.Unmarshal([]byte(evidence.Body), &content); err != nil {
			return fmt.Errorf("decode task naming research: %w", err)
		}
		input.IssueTitle, input.IssueBody = content.Title, content.Body
	}
	// JSON can expand each input byte to six bytes. Reserve room for field
	// names and coordinates, then preserve a UTF-8 prefix of the source text.
	remaining := inference.TaskNamerSite(inference.Budget{}).MaxInputBytes/6 - 1024
	for _, field := range []*string{&input.IssueTitle, &input.SourceText, &input.IssueBody} {
		if len(*field) > remaining {
			end := remaining
			for end > 0 && !utf8.RuneStart((*field)[end]) {
				end--
			}
			*field = (*field)[:end]
		}
		remaining -= len(*field)
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
