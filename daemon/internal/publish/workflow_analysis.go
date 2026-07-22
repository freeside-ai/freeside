package publish

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"gopkg.in/yaml.v3"
)

// workflowFacts is the policy-neutral result of parsing the live GitHub
// Actions documents. The auditor combines it with repository settings before
// constructing the trusted WorkflowAudit observation.
type workflowFacts struct {
	EffectiveTokenPerms domain.TokenPermissionsMode
	OIDCAvailable       bool
	EnvironmentSecrets  bool
	SecretBearingPRJobs bool
	SelfHostedRunners   bool
	PullRequestTarget   bool
	ReusableWorkflows   bool
	PackagePublishing   bool
	ArtifactConsumers   bool
}

type workflowSource struct {
	Path    string
	Content []byte
	Active  bool
}

type workflowDocument struct {
	On          yaml.Node              `yaml:"on"`
	Permissions yaml.Node              `yaml:"permissions"`
	Env         map[string]any         `yaml:"env"`
	Jobs        map[string]workflowJob `yaml:"jobs"`
}

type workflowJob struct {
	Permissions yaml.Node      `yaml:"permissions"`
	Env         map[string]any `yaml:"env"`
	Environment yaml.Node      `yaml:"environment"`
	RunsOn      yaml.Node      `yaml:"runs-on"`
	Uses        string         `yaml:"uses"`
	Secrets     yaml.Node      `yaml:"secrets"`
	Container   yaml.Node      `yaml:"container"`
	Services    yaml.Node      `yaml:"services"`
	Steps       []workflowStep `yaml:"steps"`
}

type localActionSource struct {
	Path    string
	Content []byte
}

type actionDocument struct {
	Runs struct {
		Using string         `yaml:"using"`
		Steps []workflowStep `yaml:"steps"`
	} `yaml:"runs"`
}

type workflowStep struct {
	Uses string         `yaml:"uses"`
	Run  string         `yaml:"run"`
	With map[string]any `yaml:"with"`
	Env  map[string]any `yaml:"env"`
}

// analyzeWorkflows derives every privilege axis from active workflow YAML.
// Unknown YAML fields are harmless, but malformed documents and dynamic
// permission expressions fail closed: an auditor must never turn syntax it
// cannot prove safe into a read-only observation.
func analyzeWorkflows(sources []workflowSource, actionSources []localActionSource, defaultPerms domain.TokenPermissionsMode, secretEnvironments map[string]bool, selfHostedRunners []auditRunner) (workflowFacts, error) {
	facts := workflowFacts{EffectiveTokenPerms: domain.TokenPermissionsReadOnly}
	normalizedSecretEnvironments := make(map[string]bool, len(secretEnvironments))
	for name, hasSecrets := range secretEnvironments {
		normalizedName := strings.ToLower(name)
		normalizedSecretEnvironments[normalizedName] = normalizedSecretEnvironments[normalizedName] || hasSecrets
	}
	actions, err := indexLocalActions(actionSources)
	if err != nil {
		return workflowFacts{}, err
	}
	localWorkflows := make(map[string]bool, len(sources))
	for _, source := range sources {
		localWorkflows["./"+source.Path] = source.Active
	}
	for _, source := range sources {
		if !source.Active {
			continue
		}
		var doc workflowDocument
		if err := decodeOneYAML(source.Content, &doc); err != nil {
			return workflowFacts{}, fmt.Errorf("analyze workflow %s: %w", source.Path, err)
		}
		if doc.On.Kind == 0 || len(doc.Jobs) == 0 {
			return workflowFacts{}, fmt.Errorf("analyze workflow %s: missing on or jobs", source.Path)
		}
		events, err := workflowEvents(doc.On)
		if err != nil {
			return workflowFacts{}, fmt.Errorf("analyze workflow %s: %w", source.Path, err)
		}
		// A workflow_call document can be invoked by a pull-request workflow.
		// Treat it as PR-reachable because this local analysis does not build a
		// complete caller graph, and under-reporting its authority would fail open.
		prTriggered := events["pull_request"] || events["pull_request_target"] || events["workflow_call"]
		workflowHasSecretEnv := anyContainsSecret(doc.Env)
		if events["pull_request_target"] {
			facts.PullRequestTarget = true
			// Keep this axis fail-high even when YAML narrows permissions:
			// pull_request_target executes in the base repository's privileged
			// context, so approval requires the explicit read-write profile mode.
			facts.EffectiveTokenPerms = domain.TokenPermissionsReadWrite
		}
		inheritedPerms := defaultPerms
		inheritedPackages := defaultPerms == domain.TokenPermissionsReadWrite
		if events["pull_request_target"] {
			inheritedPerms = domain.TokenPermissionsReadWrite
			inheritedPackages = true
		}
		workflowPerms, workflowOIDC, workflowPackages, err := permissionFacts(doc.Permissions, inheritedPerms)
		if err != nil {
			return workflowFacts{}, fmt.Errorf("analyze workflow %s permissions: %w", source.Path, err)
		}
		if doc.Permissions.Kind == 0 {
			workflowPackages = inheritedPackages
		}
		for jobName, job := range doc.Jobs {
			jobPerms, oidc, packages, err := permissionFacts(job.Permissions, workflowPerms)
			if err != nil {
				return workflowFacts{}, fmt.Errorf("analyze workflow %s job %s permissions: %w", source.Path, jobName, err)
			}
			if prTriggered && jobPerms == domain.TokenPermissionsReadWrite {
				facts.EffectiveTokenPerms = domain.TokenPermissionsReadWrite
			}
			if job.Permissions.Kind == 0 {
				oidc, packages = workflowOIDC, workflowPackages
			}
			if prTriggered && oidc {
				facts.OIDCAvailable = true
			}
			if prTriggered && packages {
				facts.PackagePublishing = true
			}
			if prTriggered && runsOnSelfHosted(job.RunsOn, selfHostedRunners) {
				facts.SelfHostedRunners = true
			}
			if prTriggered && job.Uses != "" {
				facts.ReusableWorkflows = true
				if !strings.HasPrefix(job.Uses, "./.github/workflows/") {
					return workflowFacts{}, fmt.Errorf("analyze workflow %s job %s: remote reusable workflow %q is not locally attestable", source.Path, jobName, job.Uses)
				}
				if active, ok := localWorkflows[job.Uses]; !ok || !active {
					return workflowFacts{}, fmt.Errorf("analyze workflow %s job %s: local reusable workflow %q is absent or inactive", source.Path, jobName, job.Uses)
				}
			}
			if prTriggered && (nodeContains(job.Secrets, "inherit") || nodeHasSecret(job.Secrets)) {
				facts.SecretBearingPRJobs = true
			}
			if prTriggered && (workflowHasSecretEnv || anyContainsSecret(job.Env) ||
				nodeHasSecret(job.Container) || nodeHasSecret(job.Services)) {
				facts.SecretBearingPRJobs = true
			}
			if prTriggered {
				envName, dynamic := jobEnvironment(job.Environment)
				switch {
				case dynamic && len(normalizedSecretEnvironments) > 0:
					facts.EnvironmentSecrets = true
				case normalizedSecretEnvironments[strings.ToLower(envName)]:
					facts.EnvironmentSecrets = true
				}
			}
			for _, step := range job.Steps {
				if isArtifactConsumer(step) {
					facts.ArtifactConsumers = true
				}
				if prTriggered && (containsSecret(step.Run) || anyContainsSecret(step.With) || anyContainsSecret(step.Env)) {
					facts.SecretBearingPRJobs = true
				}
				if strings.HasPrefix(step.Uses, "./") {
					if err := analyzeLocalAction(step.Uses, actions, map[string]bool{}, prTriggered, &facts); err != nil {
						return workflowFacts{}, fmt.Errorf("analyze workflow %s job %s: %w", source.Path, jobName, err)
					}
				}
			}
		}
		if events["workflow_call"] {
			facts.ReusableWorkflows = true
		}
	}
	return facts, nil
}

func decodeOneYAML(content []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(content))
	dec.KnownFields(false)
	if err := dec.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple YAML documents")
		}
		return err
	}
	return nil
}

func indexLocalActions(sources []localActionSource) (map[string]localActionSource, error) {
	actions := make(map[string]localActionSource)
	for _, source := range sources {
		base := path.Base(source.Path)
		if base != "action.yml" && base != "action.yaml" {
			continue
		}
		ref := path.Clean("./" + path.Dir(source.Path))
		if _, exists := actions[ref]; exists {
			return nil, fmt.Errorf("analyze local actions: multiple metadata files for %q", ref)
		}
		actions[ref] = source
	}
	return actions, nil
}

func analyzeLocalAction(ref string, actions map[string]localActionSource, visiting map[string]bool, prTriggered bool, facts *workflowFacts) error {
	clean := path.Clean(ref)
	if !strings.HasPrefix(ref, "./") || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("local action reference %q is not canonical", ref)
	}
	source, ok := actions[clean]
	if !ok {
		return fmt.Errorf("local action %q is absent or outside the audited action surface", ref)
	}
	if visiting[clean] {
		return fmt.Errorf("local action cycle at %q", ref)
	}
	visiting[clean] = true
	defer delete(visiting, clean)

	var doc actionDocument
	if err := decodeOneYAML(source.Content, &doc); err != nil {
		return fmt.Errorf("analyze local action %s: %w", source.Path, err)
	}
	if doc.Runs.Using != "composite" || len(doc.Runs.Steps) == 0 {
		return fmt.Errorf("analyze local action %s: unsupported or empty runs definition", source.Path)
	}
	for _, step := range doc.Runs.Steps {
		if isArtifactConsumer(step) {
			facts.ArtifactConsumers = true
		}
		if prTriggered && (containsSecret(step.Run) || anyContainsSecret(step.With) || anyContainsSecret(step.Env)) {
			facts.SecretBearingPRJobs = true
		}
		if strings.HasPrefix(step.Uses, "./") {
			if err := analyzeLocalAction(step.Uses, actions, visiting, prTriggered, facts); err != nil {
				return err
			}
		}
	}
	return nil
}

func isArtifactConsumer(step workflowStep) bool {
	uses, run := strings.ToLower(step.Uses), strings.ToLower(step.Run)
	return strings.Contains(uses, "download-artifact") || strings.Contains(run, "/actions/artifacts/") ||
		strings.Contains(run, "gh run download") || strings.Contains(run, "download-artifact")
}

func runsOnSelfHosted(n yaml.Node, runners []auditRunner) bool {
	if nodeContains(n, "self-hosted") {
		return true
	}
	if n.Kind == 0 {
		return false
	}
	if nodeContains(n, "${{") || n.Kind == yaml.MappingNode {
		// Expressions and groups can resolve to organization or enterprise
		// runners that the repository-scoped runner endpoint does not expose.
		return true
	}

	var requiredLabels []string
	switch n.Kind {
	case yaml.ScalarNode:
		requiredLabels = []string{strings.ToLower(n.Value)}
	case yaml.SequenceNode:
		for _, child := range n.Content {
			if child.Kind != yaml.ScalarNode {
				return true
			}
			requiredLabels = append(requiredLabels, strings.ToLower(child.Value))
		}
	default:
		return true
	}
	for _, runner := range runners {
		available := make(map[string]bool, len(runner.Labels))
		for _, label := range runner.Labels {
			available[strings.ToLower(label)] = true
		}
		matches := true
		for _, required := range requiredLabels {
			matches = matches && available[required]
		}
		if matches {
			return true
		}
	}
	return len(requiredLabels) != 1 || !standardGitHubHostedRunnerLabel(requiredLabels[0])
}

func standardGitHubHostedRunnerLabel(label string) bool {
	switch strings.ToLower(label) {
	case "ubuntu-slim", "ubuntu-latest", "ubuntu-22.04", "ubuntu-24.04", "ubuntu-26.04",
		"ubuntu-22.04-arm", "ubuntu-24.04-arm", "ubuntu-26.04-arm",
		"windows-latest", "windows-2022", "windows-2025", "windows-2025-vs2026",
		"windows-11-arm", "windows-11-vs2026-arm",
		"macos-latest", "macos-14", "macos-15", "macos-26", "macos-15-intel", "macos-26-intel":
		return true
	}
	return false
}

func workflowEvents(n yaml.Node) (map[string]bool, error) {
	events := map[string]bool{}
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Value == "" || strings.Contains(n.Value, "${{") {
			return nil, errors.New("dynamic or empty workflow trigger")
		}
		events[n.Value] = true
	case yaml.SequenceNode:
		for _, child := range n.Content {
			if child.Kind != yaml.ScalarNode || child.Value == "" || strings.Contains(child.Value, "${{") {
				return nil, errors.New("invalid workflow trigger list")
			}
			events[child.Value] = true
		}
	case yaml.MappingNode:
		for i := 0; i < len(n.Content); i += 2 {
			key := n.Content[i]
			if key.Kind != yaml.ScalarNode || key.Value == "" {
				return nil, errors.New("invalid workflow trigger map")
			}
			events[key.Value] = true
		}
	default:
		return nil, errors.New("unsupported workflow trigger")
	}
	return events, nil
}

func permissionFacts(n yaml.Node, inherited domain.TokenPermissionsMode) (domain.TokenPermissionsMode, bool, bool, error) {
	if n.Kind == 0 {
		return inherited, false, false, nil
	}
	switch n.Kind {
	case yaml.ScalarNode:
		switch n.Value {
		case "read-all", "{}":
			return domain.TokenPermissionsReadOnly, false, false, nil
		case "write-all":
			return domain.TokenPermissionsReadWrite, true, true, nil
		default:
			return "", false, false, fmt.Errorf("unsupported permission scalar %q", n.Value)
		}
	case yaml.MappingNode:
		mode := domain.TokenPermissionsReadOnly
		var oidc, packages bool
		for i := 0; i < len(n.Content); i += 2 {
			key, value := n.Content[i].Value, n.Content[i+1]
			if value.Kind != yaml.ScalarNode || strings.Contains(value.Value, "${{") {
				return "", false, false, fmt.Errorf("dynamic permission %q", key)
			}
			switch value.Value {
			case "none", "read":
			case "write":
				mode = domain.TokenPermissionsReadWrite
				oidc = oidc || key == "id-token"
				packages = packages || key == "packages"
			default:
				return "", false, false, fmt.Errorf("invalid permission %q value %q", key, value.Value)
			}
		}
		return mode, oidc, packages, nil
	default:
		return "", false, false, errors.New("unsupported permissions shape")
	}
}

func nodeContains(n yaml.Node, needle string) bool {
	needle = strings.ToLower(needle)
	if n.Kind == yaml.ScalarNode && strings.Contains(strings.ToLower(n.Value), needle) {
		return true
	}
	for _, child := range n.Content {
		if nodeContains(*child, needle) {
			return true
		}
	}
	return false
}

func nodeHasSecret(n yaml.Node) bool {
	if n.Kind == yaml.ScalarNode && containsSecret(n.Value) {
		return true
	}
	for _, child := range n.Content {
		if nodeHasSecret(*child) {
			return true
		}
	}
	return false
}

func jobEnvironment(n yaml.Node) (name string, dynamic bool) {
	if n.Kind == 0 {
		return "", false
	}
	if n.Kind == yaml.ScalarNode {
		return n.Value, strings.Contains(n.Value, "${{")
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i < len(n.Content); i += 2 {
			if n.Content[i].Value == "name" {
				v := n.Content[i+1].Value
				return v, strings.Contains(v, "${{")
			}
		}
	}
	return "", true
}

func containsSecret(s string) bool {
	lower := strings.ToLower(s)
	for {
		start := strings.Index(lower, "${{")
		if start < 0 {
			return false
		}
		lower = lower[start+3:]
		end := strings.Index(lower, "}}")
		if end < 0 {
			return false
		}
		expr := lower[:end]
		for i := 0; i < len(expr); {
			at := strings.Index(expr[i:], "secrets")
			if at < 0 {
				break
			}
			at += i
			beforeOK := at == 0 || !isExpressionIdentifierByte(expr[at-1]) && expr[at-1] != '.' && expr[at-1] != '\'' && expr[at-1] != '"'
			after := at + len("secrets")
			for after < len(expr) && (expr[after] == ' ' || expr[after] == '\t' || expr[after] == '\r' || expr[after] == '\n') {
				after++
			}
			afterOK := after == len(expr) || !isExpressionIdentifierByte(expr[after]) && expr[after] != '\'' && expr[after] != '"'
			if beforeOK && afterOK {
				return true
			}
			i = at + len("secrets")
		}
		lower = lower[end+2:]
	}
}

func isExpressionIdentifierByte(b byte) bool {
	return b == '_' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9'
}

func anyContainsSecret(values map[string]any) bool {
	for _, value := range values {
		if containsSecret(fmt.Sprint(value)) {
			return true
		}
	}
	return false
}
