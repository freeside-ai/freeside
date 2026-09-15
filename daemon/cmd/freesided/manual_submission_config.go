package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

type manualSubmissionConfig struct {
	Version  int                       `json:"version"`
	Projects []manualSubmissionProject `json:"projects"`
}

type manualSubmissionProject struct {
	ProjectID    domain.ProjectID              `json:"project_id"`
	PolicyKeys   []domain.PolicyKey            `json:"policy_keys"`
	CommitAuthor engine.ProductionCommitAuthor `json:"commit_author"`
}

// loadManualSubmissionConfig snapshots operator-owned configuration before the
// daemon serves commands. It supplies no label initiators or publication
// authority; production still authenticates the claimed author and admits the
// policy before execution. A restart changes only future task creation.
func loadManualSubmissionConfig(path string) (func(domain.ProjectID) (engine.ManualInitiator, bool), error) {
	projects := make(map[domain.ProjectID]engine.ManualInitiator)
	if path != "" {
		file, err := readSubmissionFile(path)
		if err != nil {
			return nil, fmt.Errorf("manual submission config: %w", err)
		}
		if err := ward.RejectDuplicateJSONKeys(file.body); err != nil {
			return nil, fmt.Errorf("manual submission config: %w", err)
		}
		var cfg manualSubmissionConfig
		if err := strictjson.Decode(file.body, &cfg, strictjson.RejectInvalidUTF8, strictjson.Limit(maxSubmissionFileBytes)); err != nil {
			return nil, fmt.Errorf("manual submission config: %w", err)
		}
		if cfg.Version != 1 || cfg.Projects == nil {
			return nil, fmt.Errorf("manual submission config requires version 1 and a projects array")
		}
		for index, project := range cfg.Projects {
			if strings.TrimSpace(string(project.ProjectID)) == "" {
				return nil, fmt.Errorf("manual submission config project %d requires project_id", index)
			}
			if _, exists := projects[project.ProjectID]; exists {
				return nil, fmt.Errorf("manual submission config repeats project_id %q", project.ProjectID)
			}
			// This temporary identity validates policy without persisting a run.
			policy, err := domain.NewResolvedPolicy("manual-submission-config", project.PolicyKeys)
			if err != nil {
				return nil, fmt.Errorf("manual submission config project %d policy: %w", index, err)
			}
			if err := engine.SubmittedPathBoundary(policy); err != nil {
				return nil, fmt.Errorf("manual submission config project %d: %w", index, err)
			}
			if _, err := specify.ParsePolicy(policy); err != nil {
				return nil, fmt.Errorf("manual submission config project %d: %w", index, err)
			}
			if err := project.CommitAuthor.Validate(); err != nil {
				return nil, fmt.Errorf("manual submission config project %d author: %w", index, err)
			}
			projects[project.ProjectID] = engine.ManualInitiator{PolicyKeys: policy.Keys, CommitAuthor: project.CommitAuthor}
		}
	}
	return func(projectID domain.ProjectID) (engine.ManualInitiator, bool) {
		project, ok := projects[projectID]
		// Callers receive their own slice, never the startup snapshot's storage.
		project.PolicyKeys = slices.Clone(project.PolicyKeys)
		return project, ok
	}, nil
}
