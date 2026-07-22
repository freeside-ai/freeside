package publish

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const (
	workflowAuditEncodingVersion = "freeside-workflow-audit/v1"
	auditPageSize                = 100
	auditMaxPages                = 100
)

// WorkflowAuditor reads the live automation authority for a repository and
// returns a fresh trusted observation. Reads are external but non-mutating;
// the publication decision recorder persists and gates the returned value.
type WorkflowAuditor interface {
	Audit(ctx context.Context, repo, baseRef string) (domain.WorkflowAudit, error)
}

// GitHubWorkflowAuditor derives a WorkflowAudit from GitHub's live workflow
// and repository settings APIs. It uses the publisher's credential-contained
// forge client and never retains response bodies or tokens in errors.
type GitHubWorkflowAuditor struct {
	forge *forge
	now   func() time.Time
}

func NewGitHubWorkflowAuditor(ts TokenSource, client *http.Client, baseURL string, now func() time.Time) (*GitHubWorkflowAuditor, error) {
	if ts == nil || client == nil || now == nil {
		return nil, errors.New("workflow auditor: nil dependency")
	}
	return &GitHubWorkflowAuditor{forge: newForge(ts, client, baseURL), now: now}, nil
}

type auditedWorkflow struct {
	Path    string `json:"path"`
	SHA     string `json:"sha"`
	State   string `json:"state"`
	Content []byte `json:"content"`
}

type auditedFile struct {
	Path    string `json:"path"`
	SHA     string `json:"sha"`
	Content []byte `json:"content"`
}

type auditEnvironment struct {
	Name                   string   `json:"name"`
	Secrets                []string `json:"secrets"`
	ProtectionRules        any      `json:"protection_rules"`
	DeploymentBranchPolicy any      `json:"deployment_branch_policy"`
}

type auditRunner struct {
	Name   string   `json:"name"`
	Labels []string `json:"labels"`
}

type workflowAuditEvidence struct {
	Version                     string             `json:"version"`
	Repo                        string             `json:"repo"`
	DefaultWorkflowPermissions  string             `json:"default_workflow_permissions"`
	CanApprovePullRequestReview bool               `json:"can_approve_pull_request_reviews"`
	ActionsPermissions          any                `json:"actions_permissions"`
	SelectedActions             any                `json:"selected_actions"`
	Workflows                   []auditedWorkflow  `json:"workflows"`
	LocalActions                []auditedFile      `json:"local_actions"`
	Environments                []auditEnvironment `json:"environments"`
	Runners                     []auditRunner      `json:"runners"`
	BranchProtection            any                `json:"branch_protection"`
	Rulesets                    any                `json:"rulesets"`
}

func (a *GitHubWorkflowAuditor) Audit(ctx context.Context, repoName, baseRef string) (domain.WorkflowAudit, error) {
	repo, err := parseRepo(repoName)
	if err != nil {
		return domain.WorkflowAudit{}, fmt.Errorf("workflow audit: %w", err)
	}
	if baseRef == "" {
		return domain.WorkflowAudit{}, errors.New("workflow audit: empty base ref")
	}
	for attempt := 0; attempt < 2; attempt++ {
		before, err := a.forge.getRef(ctx, repo, baseRef, "")
		if err != nil {
			return domain.WorkflowAudit{}, fmt.Errorf("workflow audit: resolve base: %w", err)
		}
		if !before.Exists {
			return domain.WorkflowAudit{}, errors.New("workflow audit: base ref does not exist")
		}
		evidence, facts, err := a.collect(ctx, repo, before.SHA, baseRef)
		if err != nil {
			return domain.WorkflowAudit{}, fmt.Errorf("workflow audit: %w", err)
		}
		after, err := a.forge.getRef(ctx, repo, baseRef, "")
		if err != nil {
			return domain.WorkflowAudit{}, fmt.Errorf("workflow audit: re-resolve base: %w", err)
		}
		if after.Exists && after.SHA == before.SHA {
			body, err := json.Marshal(evidence)
			if err != nil {
				return domain.WorkflowAudit{}, fmt.Errorf("workflow audit: encode evidence: %w", err)
			}
			audit := domain.WorkflowAudit{
				Repo: repoName, AuditedCommitSHA: before.SHA, AuditedAt: a.now().UTC(),
				WorkflowAuditDigest: domain.Digest(fmt.Sprintf("sha256:%x", sha256.Sum256(body))),
				EffectiveTokenPerms: facts.EffectiveTokenPerms,
				OIDCAvailable:       facts.OIDCAvailable, EnvironmentSecrets: facts.EnvironmentSecrets,
				SecretBearingPRJobs: facts.SecretBearingPRJobs, PullRequestTarget: facts.PullRequestTarget,
				ReusableWorkflows: facts.ReusableWorkflows, SelfHostedRunners: facts.SelfHostedRunners,
				PackagePublishing: facts.PackagePublishing, ArtifactConsumers: facts.ArtifactConsumers,
			}
			if err := audit.Validate(); err != nil {
				return domain.WorkflowAudit{}, fmt.Errorf("workflow audit: construct observation: %w", err)
			}
			return audit, nil
		}
	}
	return domain.WorkflowAudit{}, errors.New("workflow audit: base ref changed during both collection attempts")
}

func (a *GitHubWorkflowAuditor) collect(ctx context.Context, repo repoRef, sha, baseRef string) (workflowAuditEvidence, workflowFacts, error) {
	actionsPermissions, selectedActions, err := a.actionsPermissions(ctx, repo)
	if err != nil {
		return workflowAuditEvidence{}, workflowFacts{}, err
	}
	defaultPerms, canApprove, err := a.defaultWorkflowPermissions(ctx, repo)
	if err != nil {
		return workflowAuditEvidence{}, workflowFacts{}, err
	}
	workflows, err := a.workflows(ctx, repo, sha)
	if err != nil {
		return workflowAuditEvidence{}, workflowFacts{}, err
	}
	localActions, err := a.localActionFiles(ctx, repo, sha)
	if err != nil {
		return workflowAuditEvidence{}, workflowFacts{}, err
	}
	environments, secretEnvironments, err := a.environments(ctx, repo)
	if err != nil {
		return workflowAuditEvidence{}, workflowFacts{}, err
	}
	runners, err := a.runners(ctx, repo)
	if err != nil {
		return workflowAuditEvidence{}, workflowFacts{}, err
	}
	branchProtection, err := a.objectOrNotFound(ctx, repo, "/repos/"+repo.path()+"/branches/"+url.PathEscape(baseRef)+"/protection")
	if err != nil {
		return workflowAuditEvidence{}, workflowFacts{}, fmt.Errorf("read branch protection: %w", err)
	}
	rulesets, err := a.rulesets(ctx, repo)
	if err != nil {
		return workflowAuditEvidence{}, workflowFacts{}, fmt.Errorf("read rulesets: %w", err)
	}
	sources := make([]workflowSource, len(workflows))
	for i, workflow := range workflows {
		sources[i] = workflowSource{Path: workflow.Path, Content: workflow.Content, Active: workflow.State == "active"}
	}
	actionSources := make([]localActionSource, 0, len(localActions))
	for _, action := range localActions {
		actionSources = append(actionSources, localActionSource{Path: action.Path, Content: action.Content})
	}
	facts, err := analyzeWorkflows(sources, actionSources, defaultPerms, secretEnvironments, runners)
	if err != nil {
		return workflowAuditEvidence{}, workflowFacts{}, err
	}
	evidence := workflowAuditEvidence{
		Version: workflowAuditEncodingVersion, Repo: repo.path(),
		DefaultWorkflowPermissions: string(defaultPerms), CanApprovePullRequestReview: canApprove,
		ActionsPermissions: actionsPermissions, SelectedActions: selectedActions,
		Workflows: workflows, LocalActions: localActions, Environments: environments, Runners: runners,
		BranchProtection: branchProtection, Rulesets: rulesets,
	}
	return evidence, facts, nil
}

func (a *GitHubWorkflowAuditor) localActionFiles(ctx context.Context, repo repoRef, sha string) ([]auditedFile, error) {
	var decoded struct {
		Truncated bool `json:"truncated"`
		Tree      []struct {
			Path string `json:"path"`
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"tree"`
	}
	requestPath := "/repos/" + repo.path() + "/git/trees/" + url.PathEscape(sha) + "?recursive=1"
	if err := a.getJSON(ctx, repo, requestPath, &decoded); err != nil {
		return nil, fmt.Errorf("list local action files: %w", err)
	}
	if decoded.Truncated {
		return nil, errors.New("list local action files: recursive tree is truncated")
	}
	if decoded.Tree == nil {
		return nil, errors.New("list local action files: response is not a tree")
	}
	var files []auditedFile
	for _, entry := range decoded.Tree {
		if !strings.HasPrefix(entry.Path, ".github/actions/") {
			continue
		}
		if entry.Type == "tree" {
			continue
		}
		if entry.Type != "blob" || entry.SHA == "" {
			return nil, fmt.Errorf("list local action files: unsupported entry %q of type %q", entry.Path, entry.Type)
		}
		content, blobSHA, err := a.repositoryContent(ctx, repo, entry.Path, sha)
		if err != nil {
			return nil, fmt.Errorf("read local action file %s: %w", entry.Path, err)
		}
		if blobSHA != entry.SHA {
			return nil, fmt.Errorf("read local action file %s: tree/content SHA mismatch", entry.Path)
		}
		files = append(files, auditedFile{Path: entry.Path, SHA: blobSHA, Content: content})
	}
	slices.SortFunc(files, func(x, y auditedFile) int { return strings.Compare(x.Path, y.Path) })
	return files, nil
}

func (a *GitHubWorkflowAuditor) actionsPermissions(ctx context.Context, repo repoRef) (any, any, error) {
	var permissions map[string]any
	requestPath := "/repos/" + repo.path() + "/actions/permissions"
	if err := a.getJSON(ctx, repo, requestPath, &permissions); err != nil {
		return nil, nil, fmt.Errorf("read Actions permissions: %w", err)
	}
	allowedActions, ok := permissions["allowed_actions"].(string)
	if !ok || allowedActions == "" {
		return nil, nil, errors.New("read Actions permissions: missing allowed_actions")
	}
	if allowedActions != "selected" {
		return permissions, nil, nil
	}
	var selected map[string]any
	requestPath += "/selected-actions"
	if err := a.getJSON(ctx, repo, requestPath, &selected); err != nil {
		return nil, nil, fmt.Errorf("read selected Actions permissions: %w", err)
	}
	return permissions, selected, nil
}

func (a *GitHubWorkflowAuditor) defaultWorkflowPermissions(ctx context.Context, repo repoRef) (domain.TokenPermissionsMode, bool, error) {
	var decoded struct {
		Default string `json:"default_workflow_permissions"`
		Approve bool   `json:"can_approve_pull_request_reviews"`
	}
	path := "/repos/" + repo.path() + "/actions/permissions/workflow"
	if err := a.getJSON(ctx, repo, path, &decoded); err != nil {
		return "", false, fmt.Errorf("read default workflow permissions: %w", err)
	}
	switch decoded.Default {
	case "read":
		return domain.TokenPermissionsReadOnly, decoded.Approve, nil
	case "write":
		return domain.TokenPermissionsReadWrite, decoded.Approve, nil
	default:
		return "", false, fmt.Errorf("read default workflow permissions: unsupported value %q", decoded.Default)
	}
}

func (a *GitHubWorkflowAuditor) workflows(ctx context.Context, repo repoRef, sha string) ([]auditedWorkflow, error) {
	states := map[string]string{}
	for page := 1; page <= auditMaxPages; page++ {
		var decoded struct {
			Total     int `json:"total_count"`
			Workflows []struct {
				Path  string `json:"path"`
				State string `json:"state"`
			} `json:"workflows"`
		}
		requestPath := fmt.Sprintf("/repos/%s/actions/workflows?per_page=%d&page=%d", repo.path(), auditPageSize, page)
		if err := a.getJSON(ctx, repo, requestPath, &decoded); err != nil {
			return nil, fmt.Errorf("list workflows: %w", err)
		}
		if decoded.Workflows == nil {
			return nil, errors.New("list workflows: response is not a list")
		}
		for _, item := range decoded.Workflows {
			clean := path.Clean(item.Path)
			if clean != item.Path || !strings.HasPrefix(clean, ".github/workflows/") || item.State == "" {
				return nil, errors.New("list workflows: malformed workflow row")
			}
			if _, exists := states[clean]; exists {
				return nil, fmt.Errorf("list workflows: duplicate path %q", clean)
			}
			states[clean] = item.State
		}
		if len(states) >= decoded.Total {
			break
		}
		if len(decoded.Workflows) < auditPageSize {
			return nil, errors.New("list workflows: pagination ended before total_count")
		}
		if page == auditMaxPages {
			return nil, errors.New("list workflows: pagination exceeded 100 pages")
		}
	}

	var tree struct {
		Truncated bool `json:"truncated"`
		Entries   []struct {
			Path string `json:"path"`
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"tree"`
	}
	requestPath := "/repos/" + repo.path() + "/git/trees/" + url.PathEscape(sha) + "?recursive=1"
	if err := a.getJSON(ctx, repo, requestPath, &tree); err != nil {
		return nil, fmt.Errorf("list workflow files: %w", err)
	}
	if tree.Truncated {
		return nil, errors.New("list workflow files: recursive tree is truncated")
	}
	if tree.Entries == nil {
		return nil, errors.New("list workflow files: response is not a tree")
	}
	var all []auditedWorkflow
	seen := map[string]bool{}
	for _, entry := range tree.Entries {
		if path.Clean(entry.Path) != entry.Path {
			return nil, fmt.Errorf("list workflow files: non-canonical path %q", entry.Path)
		}
		if !strings.HasPrefix(entry.Path, ".github/workflows/") ||
			(path.Ext(entry.Path) != ".yml" && path.Ext(entry.Path) != ".yaml") {
			continue
		}
		if seen[entry.Path] {
			return nil, fmt.Errorf("list workflow files: duplicate path %q", entry.Path)
		}
		seen[entry.Path] = true
		if entry.Type != "blob" || entry.SHA == "" {
			return nil, fmt.Errorf("list workflow files: unsupported entry %q of type %q", entry.Path, entry.Type)
		}
		content, blobSHA, err := a.repositoryContent(ctx, repo, entry.Path, sha)
		if err != nil {
			return nil, fmt.Errorf("read workflow %s: %w", entry.Path, err)
		}
		if blobSHA != entry.SHA {
			return nil, fmt.Errorf("read workflow %s: tree/content SHA mismatch", entry.Path)
		}
		state := states[entry.Path]
		if state == "" {
			// The workflow registry follows the default branch and can omit a
			// file that exists on a non-default audited base. Treat it as active
			// so the authority derivation fails high rather than skipping it.
			state = "active"
		}
		all = append(all, auditedWorkflow{Path: entry.Path, SHA: blobSHA, State: state, Content: content})
	}
	slices.SortFunc(all, func(x, y auditedWorkflow) int { return strings.Compare(x.Path, y.Path) })
	return all, nil
}

func (a *GitHubWorkflowAuditor) repositoryContent(ctx context.Context, repo repoRef, filePath, ref string) ([]byte, string, error) {
	segments := strings.Split(filePath, "/")
	for i := range segments {
		segments[i] = url.PathEscape(segments[i])
	}
	var decoded struct {
		Type     string `json:"type"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
		SHA      string `json:"sha"`
		Path     string `json:"path"`
	}
	requestPath := "/repos/" + repo.path() + "/contents/" + strings.Join(segments, "/") + "?ref=" + url.QueryEscape(ref)
	if err := a.getJSON(ctx, repo, requestPath, &decoded); err != nil {
		return nil, "", err
	}
	if decoded.Type != "file" || decoded.Encoding != "base64" || decoded.SHA == "" || decoded.Path != filePath {
		return nil, "", errors.New("content response does not identify the requested file")
	}
	content, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(decoded.Content, "\n", ""))
	if err != nil {
		return nil, "", errors.New("content response is not valid base64")
	}
	return content, decoded.SHA, nil
}

func (a *GitHubWorkflowAuditor) environments(ctx context.Context, repo repoRef) ([]auditEnvironment, map[string]bool, error) {
	var environments []auditEnvironment
	secretEnvironments := map[string]bool{}
	for page := 1; page <= auditMaxPages; page++ {
		var decoded struct {
			Total        int `json:"total_count"`
			Environments []struct {
				Name                   string `json:"name"`
				ProtectionRules        any    `json:"protection_rules"`
				DeploymentBranchPolicy any    `json:"deployment_branch_policy"`
			} `json:"environments"`
		}
		requestPath := fmt.Sprintf("/repos/%s/environments?per_page=%d&page=%d", repo.path(), auditPageSize, page)
		if err := a.getJSON(ctx, repo, requestPath, &decoded); err != nil {
			return nil, nil, fmt.Errorf("list environments: %w", err)
		}
		if decoded.Environments == nil {
			return nil, nil, errors.New("list environments: response is not a list")
		}
		for _, env := range decoded.Environments {
			if env.Name == "" {
				return nil, nil, errors.New("list environments: empty name")
			}
			secrets, err := a.environmentSecrets(ctx, repo, env.Name)
			if err != nil {
				return nil, nil, fmt.Errorf("list environment %q secrets: %w", env.Name, err)
			}
			secretEnvironments[env.Name] = len(secrets) > 0
			environments = append(environments, auditEnvironment{
				Name: env.Name, Secrets: secrets,
				ProtectionRules: env.ProtectionRules, DeploymentBranchPolicy: env.DeploymentBranchPolicy,
			})
		}
		if len(environments) >= decoded.Total {
			break
		}
		if len(decoded.Environments) < auditPageSize {
			return nil, nil, errors.New("list environments: pagination ended before total_count")
		}
		if page == auditMaxPages {
			return nil, nil, errors.New("list environments: pagination exceeded 100 pages")
		}
	}
	slices.SortFunc(environments, func(x, y auditEnvironment) int { return strings.Compare(x.Name, y.Name) })
	return environments, secretEnvironments, nil
}

func (a *GitHubWorkflowAuditor) rulesets(ctx context.Context, repo repoRef) ([]any, error) {
	summaries, err := a.arrayPages(ctx, repo, "/repos/"+repo.path()+"/rulesets?includes_parents=true")
	if err != nil {
		return nil, err
	}
	details := make([]any, 0, len(summaries))
	for _, summary := range summaries {
		row, ok := summary.(map[string]any)
		if !ok {
			return nil, errors.New("ruleset summary is not an object")
		}
		id, ok := row["id"].(float64)
		if !ok || id <= 0 || id != float64(int64(id)) {
			return nil, errors.New("ruleset summary has invalid id")
		}
		var detail map[string]any
		requestPath := fmt.Sprintf("/repos/%s/rulesets/%d?includes_parents=true", repo.path(), int64(id))
		if err := a.getJSON(ctx, repo, requestPath, &detail); err != nil {
			return nil, err
		}
		details = append(details, detail)
	}
	if err := sortJSONValues(details); err != nil {
		return nil, err
	}
	return details, nil
}

func sortJSONValues(values []any) error {
	type encodedValue struct {
		value   any
		encoded string
	}
	encoded := make([]encodedValue, len(values))
	for i, value := range values {
		body, err := json.Marshal(value)
		if err != nil {
			return err
		}
		encoded[i] = encodedValue{value: value, encoded: string(body)}
	}
	slices.SortFunc(encoded, func(x, y encodedValue) int { return strings.Compare(x.encoded, y.encoded) })
	for i := range encoded {
		values[i] = encoded[i].value
	}
	return nil
}

func (a *GitHubWorkflowAuditor) environmentSecrets(ctx context.Context, repo repoRef, environment string) ([]string, error) {
	var names []string
	for page := 1; page <= auditMaxPages; page++ {
		var decoded struct {
			Total   int `json:"total_count"`
			Secrets []struct {
				Name string `json:"name"`
			} `json:"secrets"`
		}
		requestPath := fmt.Sprintf("/repos/%s/environments/%s/secrets?per_page=%d&page=%d", repo.path(), url.PathEscape(environment), auditPageSize, page)
		if err := a.getJSON(ctx, repo, requestPath, &decoded); err != nil {
			return nil, err
		}
		if decoded.Secrets == nil {
			return nil, errors.New("response is not a list")
		}
		for _, secret := range decoded.Secrets {
			if secret.Name == "" {
				return nil, errors.New("response carries empty secret name")
			}
			names = append(names, secret.Name)
		}
		if len(names) >= decoded.Total {
			break
		}
		if len(decoded.Secrets) < auditPageSize {
			return nil, errors.New("pagination ended before total_count")
		}
		if page == auditMaxPages {
			return nil, errors.New("pagination exceeded 100 pages")
		}
	}
	slices.Sort(names)
	return names, nil
}

func (a *GitHubWorkflowAuditor) runners(ctx context.Context, repo repoRef) ([]auditRunner, error) {
	var runners []auditRunner
	for page := 1; page <= auditMaxPages; page++ {
		var decoded struct {
			Total   int `json:"total_count"`
			Runners []struct {
				Name   string `json:"name"`
				Labels []struct {
					Name string `json:"name"`
				} `json:"labels"`
			} `json:"runners"`
		}
		requestPath := fmt.Sprintf("/repos/%s/actions/runners?per_page=%d&page=%d", repo.path(), auditPageSize, page)
		if err := a.getJSON(ctx, repo, requestPath, &decoded); err != nil {
			return nil, fmt.Errorf("list self-hosted runners: %w", err)
		}
		if decoded.Runners == nil {
			return nil, errors.New("list self-hosted runners: response is not a list")
		}
		for _, runner := range decoded.Runners {
			if runner.Name == "" || runner.Labels == nil {
				return nil, errors.New("list self-hosted runners: malformed row")
			}
			labels := make([]string, len(runner.Labels))
			for i, label := range runner.Labels {
				if label.Name == "" {
					return nil, errors.New("list self-hosted runners: empty label")
				}
				labels[i] = label.Name
			}
			slices.Sort(labels)
			runners = append(runners, auditRunner{Name: runner.Name, Labels: labels})
		}
		if len(runners) >= decoded.Total {
			break
		}
		if len(decoded.Runners) < auditPageSize {
			return nil, errors.New("list self-hosted runners: pagination ended before total_count")
		}
		if page == auditMaxPages {
			return nil, errors.New("list self-hosted runners: pagination exceeded 100 pages")
		}
	}
	slices.SortFunc(runners, func(x, y auditRunner) int { return strings.Compare(x.Name, y.Name) })
	return runners, nil
}

func (a *GitHubWorkflowAuditor) objectOrNotFound(ctx context.Context, repo repoRef, requestPath string) (any, error) {
	resp, err := a.forge.do(ctx, http.MethodGet, repo, requestPath, "", nil)
	if err != nil {
		return nil, err
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &APIError{Status: resp.StatusCode, RequestPath: requestPath}
	}
	var decoded any
	if err := decodeResponse(resp.Body, &decoded); err != nil {
		return nil, err
	}
	if _, ok := decoded.(map[string]any); !ok {
		return nil, errors.New("response is not an object")
	}
	return decoded, nil
}

func (a *GitHubWorkflowAuditor) arrayPages(ctx context.Context, repo repoRef, basePath string) ([]any, error) {
	var all []any
	separator := "&"
	if !strings.Contains(basePath, "?") {
		separator = "?"
	}
	for page := 1; page <= auditMaxPages; page++ {
		requestPath := fmt.Sprintf("%s%sper_page=%d&page=%d", basePath, separator, auditPageSize, page)
		var decoded []any
		if err := a.getJSON(ctx, repo, requestPath, &decoded); err != nil {
			return nil, err
		}
		if decoded == nil {
			return nil, errors.New("response is not a list")
		}
		all = append(all, decoded...)
		if len(decoded) < auditPageSize {
			return all, nil
		}
	}
	return nil, errors.New("pagination exceeded 100 pages")
}

func (a *GitHubWorkflowAuditor) getJSON(ctx context.Context, repo repoRef, requestPath string, out any) error {
	resp, err := a.forge.do(ctx, http.MethodGet, repo, requestPath, "", nil)
	if err != nil {
		return err
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return &APIError{Status: resp.StatusCode, RequestPath: requestPath}
	}
	return decodeResponse(resp.Body, out)
}

var _ WorkflowAuditor = (*GitHubWorkflowAuditor)(nil)
