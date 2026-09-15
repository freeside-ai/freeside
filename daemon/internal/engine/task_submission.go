package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// ManualInitiator is a project's configured client-submission policy: the
// resolved policy keys a submitted task runs under and the commit author its
// publication is attributed to. The daemon loads this operator-owned snapshot
// separately from label intake; a paired client cannot choose these bindings.
type ManualInitiator struct {
	PolicyKeys   []domain.PolicyKey
	CommitAuthor ProductionCommitAuthor
}

// TaskSubmitter is the engine-backed implementation of signet.TaskSubmitter: it
// creates or fetches the task for a submitted source inside the accepting
// transaction. It holds the shared blob store (for the policy bytes it
// registers) and a per-project initiator lookup; it needs no *Engine, so the
// daemon composition can construct it before the engine is wired.
type TaskSubmitter struct {
	blobs     *signet.BlobStore
	initiator func(domain.ProjectID) (ManualInitiator, bool)
}

// NewTaskSubmitter builds the injected submitter. A nil initiator refuses every
// project (client submission unavailable); the daemon composition supplies one
// over the configured initiators.
func NewTaskSubmitter(blobs *signet.BlobStore, initiator func(domain.ProjectID) (ManualInitiator, bool)) *TaskSubmitter {
	return &TaskSubmitter{blobs: blobs, initiator: initiator}
}

// SubmitTask creates or fetches the task for (project, source) inside the
// caller's transaction and returns its identity. The project-scoped intake key
// is the only concurrency control: an existing task is reused (its recorded
// specification run and name), so a configuration change after creation moves
// no existing identity; only the first submission composes the policy,
// publication, and work-unit declaration and starts the specification run. A
// project with no configured initiator is reported as store.ErrNotFound so the
// boundary answers 404 without enumerating projects. An operator name that
// fails the canonical bound is refused as ErrInvalidSubmitTaskPayload before
// any write, whether the source would create or fetch a task.
func (t *TaskSubmitter) SubmitTask(ctx context.Context, tx *store.WriteTx, in signet.TaskSubmissionInput) (signet.TaskSubmissionResult, error) {
	if t.blobs == nil {
		return signet.TaskSubmissionResult{}, errors.New("task submitter has no blob store")
	}
	// Canonicalize the operator name before any write, so an invalid name is
	// refused whether the source creates a new task or fetches an existing one,
	// and never leaves a durable row. The error carries no name text (it may be
	// the refused secret) and wraps ErrInvalidSubmitTaskPayload so the boundary
	// answers 400.
	operatorName := in.OperatorName
	if operatorName != "" {
		var nameErr error
		if operatorName, nameErr = operatorTaskName(operatorName); nameErr != nil {
			return signet.TaskSubmissionResult{}, fmt.Errorf("submit task: %w: %w", signet.ErrInvalidSubmitTaskPayload, nameErr)
		}
	}
	sourceArtifact, err := SubmissionArtifact(
		domain.ArtifactKindSpecification, in.SourceDigest, domain.EvidenceMediaTextMarkdown, int64(len(in.Source)))
	if err != nil {
		return signet.TaskSubmissionResult{}, err
	}
	if err := RegisterSubmissionArtifact(ctx, tx, sourceArtifact); err != nil {
		return signet.TaskSubmissionResult{}, err
	}
	// Fetch an existing task by the project-scoped intake key; the same source
	// in one project always fetches the same task, so no second run starts.
	intakeKey := "source:" + string(in.SourceDigest)
	if task, err := tx.GetTaskByIntakeKey(ctx, in.ProjectID, intakeKey); err == nil {
		runID, err := existingTaskSpecificationRunID(ctx, tx, task)
		if err != nil {
			return signet.TaskSubmissionResult{}, err
		}
		return signet.TaskSubmissionResult{TaskID: task.ID, SpecificationRunID: runID, Name: task.Name}, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return signet.TaskSubmissionResult{}, err
	}

	// Create path. Resolve the project's submission policy; an unconfigured
	// project is refused before any write beyond the idempotent source artifact.
	if t.initiator == nil {
		return signet.TaskSubmissionResult{}, fmt.Errorf("client submission has no configured policy: %w", store.ErrNotFound)
	}
	init, ok := t.initiator(in.ProjectID)
	if !ok {
		return signet.TaskSubmissionResult{}, fmt.Errorf("project %q has no configured submission policy: %w", in.ProjectID, store.ErrNotFound)
	}
	publication := ProductionPublication{
		Title:        submissionTitle(operatorName, in.Source),
		Body:         "Implements the operator-submitted task specification.",
		CommitAuthor: init.CommitAuthor,
	}
	if err := publication.Validate(); err != nil {
		return signet.TaskSubmissionResult{}, fmt.Errorf("compose submission publication: %w", err)
	}
	publicationBytes, err := json.Marshal(publication)
	if err != nil {
		return signet.TaskSubmissionResult{}, err
	}
	publicationDigest := domain.Digest(contentaddr.Sum(publicationBytes))
	// The run-id derivation uses the keys-only policy digest so an exact
	// resubmission converges; the resolved policy bound to the run carries its
	// own digest.
	policyKeysDigest, err := (domain.ResolvedPolicy{Keys: init.PolicyKeys}).ComputeDigest()
	if err != nil {
		return signet.TaskSubmissionResult{}, err
	}
	implementationRunID := SubmissionRunID(in.ProjectID, in.SourceDigest, policyKeysDigest, publicationDigest, "")
	specificationRunID, err := SpecificationRunIDForImplementation(implementationRunID)
	if err != nil {
		return signet.TaskSubmissionResult{}, err
	}
	campaignID, err := ProductionCampaignIDForImplementation(implementationRunID)
	if err != nil {
		return signet.TaskSubmissionResult{}, err
	}
	resolvedPolicy, err := domain.NewResolvedPolicy(specificationRunID, init.PolicyKeys)
	if err != nil {
		return signet.TaskSubmissionResult{}, fmt.Errorf("resolve submission policy: %w", err)
	}
	// The declared-path boundary the runner enforces is refused at submission
	// too: a run durable without one is a task the daemon holds forever.
	if err := SubmittedPathBoundary(resolvedPolicy); err != nil {
		return signet.TaskSubmissionResult{}, err
	}
	policyBytes, err := json.Marshal(resolvedPolicy.Keys)
	if err != nil {
		return signet.TaskSubmissionResult{}, err
	}
	if _, err := t.blobs.Put(resolvedPolicy.Digest, bytes.NewReader(policyBytes)); err != nil {
		return signet.TaskSubmissionResult{}, fmt.Errorf("store submission policy: %w", err)
	}
	policyArtifact, err := SubmissionArtifact(
		domain.ArtifactKindPolicy, resolvedPolicy.Digest, domain.EvidenceMediaApplicationJSON, int64(len(policyBytes)))
	if err != nil {
		return signet.TaskSubmissionResult{}, err
	}
	if err := RegisterSubmissionArtifact(ctx, tx, policyArtifact); err != nil {
		return signet.TaskSubmissionResult{}, err
	}
	workUnit := &domain.WorkUnitDeclarationInput{
		CompletionCriterion: domain.CompletionBoundPRMerged,
		DeclaredPaths:       domain.CanonicalDeclaredPaths(resolvedPolicy),
	}
	submitted, err := SubmitSpecificationRunTx(ctx, tx, SpecificationRunSpec{
		SpecificationRunID:  specificationRunID,
		ImplementationRunID: implementationRunID,
		ProjectID:           in.ProjectID,
		SourceArtifactID:    sourceArtifact.ID,
		SourceBytes:         in.Source,
		PolicyArtifactID:    policyArtifact.ID,
		ResolvedPolicy:      resolvedPolicy,
		Publication:         publication,
		PublicationDigest:   publicationDigest,
		WorkUnit:            workUnit,
		CampaignID:          campaignID,
		AttemptNumber:       1,
		OperatorName:        operatorName,
		Source:              domain.SpecificationSource{Kind: domain.SpecificationSourceWorkItemArtifact, WorkItemArtifactID: sourceArtifact.ID},
	})
	if err != nil {
		return signet.TaskSubmissionResult{}, err
	}
	// Read the task back for its stored name (operator name, else the source
	// heading, else the identifier fallback).
	task, err := tx.GetTask(ctx, submitted.Run.TaskID)
	if err != nil {
		return signet.TaskSubmissionResult{}, err
	}
	return signet.TaskSubmissionResult{TaskID: task.ID, SpecificationRunID: submitted.Run.ID, Name: task.Name}, nil
}

// existingTaskSpecificationRunID resolves the specification run of a task
// fetched by intake key: the first campaign's initial attempt names it, and a
// legacy campaign-less task uses its earliest run. A task reachable by a source
// intake key with neither is surfaced rather than guessed.
func existingTaskSpecificationRunID(ctx context.Context, tx *store.WriteTx, task domain.Task) (domain.RunID, error) {
	if len(task.CampaignIDs) > 0 {
		attempt, err := tx.GetProductionAttempt(ctx, task.CampaignIDs[0], 1)
		if err != nil {
			return "", err
		}
		return attempt.SpecificationRunID, nil
	}
	runIDs, err := tx.TaskRunIDs(ctx, task.ID)
	if err != nil {
		return "", err
	}
	if len(runIDs) == 0 {
		return "", fmt.Errorf("task %q reached by a source intake key has no runs: %w", task.ID, domain.ErrParentKeyMismatch)
	}
	return runIDs[0], nil
}

// submissionTitle composes the reviewer-facing publication title: the operator
// name when given, otherwise the source's first non-empty line (stripped of a
// leading Markdown heading marker), otherwise a default. The result is a single
// non-empty trimmed line within the publication title byte limit, so it always
// passes ProductionPublication.Validate.
func submissionTitle(operatorName string, source []byte) string {
	candidate := strings.TrimSpace(operatorName)
	if candidate == "" {
		for _, rawLine := range strings.Split(string(source), "\n") {
			line := strings.TrimSpace(strings.TrimSuffix(rawLine, "\r"))
			if line == "" {
				continue
			}
			hashes := 0
			for hashes < len(line) && line[hashes] == '#' {
				hashes++
			}
			if hashes > 0 && hashes < len(line) && (line[hashes] == ' ' || line[hashes] == '\t') {
				line = strings.TrimSpace(line[hashes:])
			}
			candidate = line
			break
		}
	}
	// Collapse to a single line: the title contract rejects CR/LF.
	if i := strings.IndexAny(candidate, "\r\n"); i >= 0 {
		candidate = candidate[:i]
	}
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		candidate = "Submitted task"
	}
	if len(candidate) > maxProductionPublicationTitleBytes {
		bounded := candidate[:maxProductionPublicationTitleBytes]
		for len(bounded) > 0 && !utf8.ValidString(bounded) {
			bounded = bounded[:len(bounded)-1]
		}
		if trimmed := strings.TrimSpace(bounded); trimmed != "" {
			candidate = trimmed
		} else {
			candidate = "Submitted task"
		}
	}
	return candidate
}
