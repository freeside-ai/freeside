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
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// A full advisory queue leaves the identifier fallback; dispatch never waits
// for naming. The queue is process-local and deliberately not recovery state.
const taskNameQueueCapacity = 32

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
		if count == 60 {
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
