package publish

import (
	"cmp"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// issueCommentClosureVersion tags the canonical body a closure digest
// addresses. Changing that body is a new version, and every profile that
// accepted a set under the old one then drifts until the owner re-reviews it.
const issueCommentClosureVersion = "freeside-issue-comment-closure/v1"

// issueCommentWorkflows lists every active workflow triggered by issue_comment
// (plan §5.5, §5.11) with the digest of its local closure: the workflow file,
// each local reusable workflow its jobs call, and each local composite action
// its steps use (every audited file in the action's directory, so a script the
// action runs from its own path is covered), followed transitively. Remote uses references are not
// followed; their ref strings are part of the file that names them. A listed
// workflow that cannot be parsed, or names a local dependency the audited
// files do not hold, fails the audit closed. The result is sorted by path and
// nil when no workflow is listed.
func issueCommentWorkflows(sources []workflowSource, actionSources []localActionSource) ([]domain.IssueCommentWorkflow, error) {
	actions, err := indexLocalActions(actionSources)
	if err != nil {
		return nil, err
	}
	workflows := make(map[string]workflowSource, len(sources))
	for _, source := range sources {
		workflows["./"+source.Path] = source
	}
	var listed []domain.IssueCommentWorkflow
	for _, source := range sources {
		if !source.Active {
			continue
		}
		var doc workflowDocument
		if err := decodeOneYAML(source.Content, &doc); err != nil {
			return nil, fmt.Errorf("issue_comment workflows: %s: %w", source.Path, err)
		}
		events, err := workflowEvents(doc.On)
		if err != nil {
			return nil, fmt.Errorf("issue_comment workflows: %s: %w", source.Path, err)
		}
		if !events["issue_comment"] {
			continue
		}
		files := map[string][]byte{}
		if err := addWorkflowClosure(source, workflows, actions, actionSources, files, map[string]bool{}); err != nil {
			return nil, fmt.Errorf("issue_comment workflows: %s: %w", source.Path, err)
		}
		digest, err := issueCommentClosureDigest(source.Path, files)
		if err != nil {
			return nil, fmt.Errorf("issue_comment workflows: %s: %w", source.Path, err)
		}
		listed = append(listed, domain.IssueCommentWorkflow{Path: source.Path, Digest: digest})
	}
	slices.SortFunc(listed, func(a, b domain.IssueCommentWorkflow) int { return cmp.Compare(a.Path, b.Path) })
	return listed, nil
}

// addWorkflowClosure adds the workflow and its local dependencies to files,
// keyed by repository path. followed records each workflow and action
// metadata file whose references have been read: the cycle guard, which also
// keeps a shared dependency to one pass. It is kept apart from files because
// an action's directory sweep can add a nested action's metadata before that
// action is followed, so a file being present says nothing about whether its
// references were read.
func addWorkflowClosure(source workflowSource, workflows map[string]workflowSource, actions map[string]localActionSource, actionSources []localActionSource, files map[string][]byte, followed map[string]bool) error {
	if followed[source.Path] {
		return nil
	}
	followed[source.Path] = true
	files[source.Path] = source.Content
	var doc workflowDocument
	if err := decodeOneYAML(source.Content, &doc); err != nil {
		return fmt.Errorf("decode %s: %w", source.Path, err)
	}
	for jobName, job := range doc.Jobs {
		if strings.HasPrefix(job.Uses, "./") {
			callee, ok := workflows[job.Uses]
			if !ok {
				return fmt.Errorf("%s job %s: local reusable workflow %q is absent", source.Path, jobName, job.Uses)
			}
			if err := addWorkflowClosure(callee, workflows, actions, actionSources, files, followed); err != nil {
				return err
			}
		}
		for _, step := range job.Steps {
			if strings.HasPrefix(step.Uses, "./") {
				if err := addActionClosure(step.Uses, actions, actionSources, files, followed); err != nil {
					return fmt.Errorf("%s job %s: %w", source.Path, jobName, err)
				}
			}
		}
	}
	return nil
}

// addActionClosure adds every audited file in a local composite action's
// directory, its metadata file included, and the local actions its steps use,
// one nested in that directory included. The reference rules are analyzeLocalAction's, so a
// reference the privilege analysis would refuse is refused here too, including
// one to an action that is not composite and so runs code this closure cannot
// see.
func addActionClosure(ref string, actions map[string]localActionSource, actionSources []localActionSource, files map[string][]byte, followed map[string]bool) error {
	clean := path.Clean(ref)
	if !strings.HasPrefix(ref, "./") || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("local action reference %q is not canonical", ref)
	}
	source, ok := actions[clean]
	if !ok {
		return fmt.Errorf("local action %q is absent or outside the audited action surface", ref)
	}
	if followed[source.Path] {
		return nil
	}
	followed[source.Path] = true
	dir := path.Dir(source.Path) + "/"
	for _, file := range actionSources {
		if strings.HasPrefix(file.Path, dir) {
			files[file.Path] = file.Content
		}
	}
	var doc actionDocument
	if err := decodeOneYAML(source.Content, &doc); err != nil {
		return fmt.Errorf("decode local action %s: %w", source.Path, err)
	}
	if doc.Runs.Using != "composite" || len(doc.Runs.Steps) == 0 {
		return fmt.Errorf("local action %s: unsupported or empty runs definition", source.Path)
	}
	for _, step := range doc.Runs.Steps {
		if strings.HasPrefix(step.Uses, "./") {
			if err := addActionClosure(step.Uses, actions, actionSources, files, followed); err != nil {
				return err
			}
		}
	}
	return nil
}

type issueCommentClosureFile struct {
	Path    string `json:"path"`
	Content []byte `json:"content"`
}

type issueCommentClosureBody struct {
	Version  string                    `json:"version"`
	Workflow string                    `json:"workflow"`
	Files    []issueCommentClosureFile `json:"files"`
}

// issueCommentClosureDigest addresses the closure as a canonical JSON body: the
// version, the listed workflow's path, and every file sorted by path.
func issueCommentClosureDigest(workflow string, files map[string][]byte) (domain.Digest, error) {
	body := issueCommentClosureBody{Version: issueCommentClosureVersion, Workflow: workflow}
	for filePath, content := range files {
		body.Files = append(body.Files, issueCommentClosureFile{Path: filePath, Content: content})
	}
	slices.SortFunc(body.Files, func(a, b issueCommentClosureFile) int { return cmp.Compare(a.Path, b.Path) })
	encoded, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("encode closure: %w", err)
	}
	return domain.Digest(contentaddr.Sum(encoded)), nil
}
