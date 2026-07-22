package publish

import (
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func analyzeOne(t *testing.T, body string, defaultPerms domain.TokenPermissionsMode, secretEnvironments map[string]bool) workflowFacts {
	t.Helper()
	facts, err := analyzeWorkflows([]workflowSource{{Path: ".github/workflows/ci.yml", Content: []byte(body), Active: true}}, nil, defaultPerms, secretEnvironments, nil)
	if err != nil {
		t.Fatalf("analyzeWorkflows: %v", err)
	}
	return facts
}

func TestAnalyzeWorkflowsPrivilegeAxes(t *testing.T) {
	body := `name: CI
on:
  pull_request:
permissions:
  contents: read
jobs:
  privileged:
    permissions:
      contents: write
      id-token: write
      packages: write
    runs-on: [self-hosted, linux]
    environment: production
    steps:
      - uses: actions/download-artifact@v4
      - run: echo "${{ secrets.DEPLOY_KEY }}"
`
	facts := analyzeOne(t, body, domain.TokenPermissionsReadOnly, map[string]bool{"production": true})
	if facts.EffectiveTokenPerms != domain.TokenPermissionsReadWrite || !facts.OIDCAvailable ||
		!facts.EnvironmentSecrets || !facts.SecretBearingPRJobs || !facts.SelfHostedRunners ||
		!facts.PackagePublishing || !facts.ArtifactConsumers {
		t.Fatalf("privilege facts = %+v", facts)
	}
	if facts.PullRequestTarget || facts.ReusableWorkflows {
		t.Fatalf("unexpected privilege facts = %+v", facts)
	}
}

func TestAnalyzeWorkflowsMatchesSecretEnvironmentsCaseInsensitively(t *testing.T) {
	tests := []struct {
		name        string
		environment string
	}{
		{"scalar", "production"},
		{"object", "\n      name: production\n      url: https://example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := `on: pull_request
jobs:
  deploy:
    runs-on: ubuntu-latest
    environment: ` + tt.environment + `
    steps:
      - run: true
`
			facts := analyzeOne(t, body, domain.TokenPermissionsReadOnly, map[string]bool{"Production": true})
			if !facts.EnvironmentSecrets {
				t.Fatalf("environment secrets not detected: %+v", facts)
			}
		})
	}
}

func TestAnalyzeWorkflowsPermissionPrecedence(t *testing.T) {
	body := `on: pull_request
permissions:
  contents: write
  id-token: write
  packages: write
jobs:
  reduced:
    permissions:
      contents: read
    runs-on: ubuntu-latest
    steps:
      - run: true
`
	facts := analyzeOne(t, body, domain.TokenPermissionsReadWrite, nil)
	if facts.EffectiveTokenPerms != domain.TokenPermissionsReadOnly || facts.OIDCAvailable || facts.PackagePublishing {
		t.Fatalf("job-level reduction not honored: %+v", facts)
	}
}

func TestAnalyzeWorkflowsInheritedWriteIncludesPackagePublishing(t *testing.T) {
	body := `on: pull_request
jobs:
  publish:
    runs-on: ubuntu-latest
    steps:
      - run: true
`
	facts := analyzeOne(t, body, domain.TokenPermissionsReadWrite, nil)
	if facts.EffectiveTokenPerms != domain.TokenPermissionsReadWrite || !facts.PackagePublishing {
		t.Fatalf("inherited write facts = %+v", facts)
	}
	if facts.OIDCAvailable {
		t.Fatalf("inherited repository write unexpectedly granted OIDC: %+v", facts)
	}
}

func TestAnalyzeWorkflowsPullRequestTargetExplicitReductionClearsPackagePublishing(t *testing.T) {
	body := `on: pull_request_target
permissions:
  contents: read
jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - run: true
`
	facts := analyzeOne(t, body, domain.TokenPermissionsReadOnly, nil)
	if facts.PackagePublishing || facts.EffectiveTokenPerms != domain.TokenPermissionsReadWrite {
		t.Fatalf("pull_request_target reduction facts = %+v", facts)
	}
}

func TestAnalyzeWorkflowsPullRequestTargetFailsHigh(t *testing.T) {
	facts := analyzeOne(t, `on: pull_request_target
jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - run: true
`, domain.TokenPermissionsReadOnly, nil)
	if !facts.PullRequestTarget || facts.EffectiveTokenPerms != domain.TokenPermissionsReadWrite {
		t.Fatalf("pull_request_target facts = %+v", facts)
	}
}

func TestAnalyzeWorkflowsSelfHostedRunnerSelection(t *testing.T) {
	for _, tt := range []struct {
		name    string
		runsOn  string
		runners []auditRunner
		want    bool
	}{
		{"dynamic available", "${{ matrix.runner }}", []auditRunner{{Labels: []string{"self-hosted", "linux"}}}, true},
		{"dynamic inventory absent", "${{ matrix.runner }}", nil, true},
		{"custom label", "gpu", []auditRunner{{Labels: []string{"gpu"}}}, true},
		{"custom label inventory absent", "gpu", nil, true},
		{"label set", "[linux, gpu]", []auditRunner{{Labels: []string{"self-hosted", "linux", "gpu"}}}, true},
		{"unaudited label set", "[linux, gpu]", []auditRunner{{Labels: []string{"self-hosted", "linux"}}}, true},
		{"github hosted", "ubuntu-latest", []auditRunner{{Labels: []string{"self-hosted", "linux"}}}, false},
		{"versioned github hosted", "ubuntu-24.04-arm", nil, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			workflow := []workflowSource{{Path: ".github/workflows/ci.yml", Active: true, Content: []byte(`on: pull_request
jobs:
  check:
    runs-on: ` + tt.runsOn + `
    steps:
      - run: true
`)}}
			facts, err := analyzeWorkflows(workflow, nil, domain.TokenPermissionsReadOnly, nil, tt.runners)
			if err != nil {
				t.Fatalf("analyzeWorkflows: %v", err)
			}
			if facts.SelfHostedRunners != tt.want {
				t.Fatalf("SelfHostedRunners = %v, want %v", facts.SelfHostedRunners, tt.want)
			}
		})
	}
}

func TestAnalyzeWorkflowsIgnoresNonPRReusableWorkflow(t *testing.T) {
	facts := analyzeOne(t, `on: push
jobs:
  release:
    uses: owner/repo/.github/workflows/release.yml@main
`, domain.TokenPermissionsReadOnly, nil)
	if facts.ReusableWorkflows {
		t.Fatalf("push-only reusable workflow treated as PR-reachable: %+v", facts)
	}
}

func TestAnalyzeWorkflowsNonPRLocalActionArtifactConsumer(t *testing.T) {
	workflows := []workflowSource{{Path: ".github/workflows/release.yml", Active: true, Content: []byte(`on: push
jobs:
  release:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/fetch
`)}}
	actions := []localActionSource{{Path: ".github/actions/fetch/action.yml", Content: []byte(`name: fetch
runs:
  using: composite
  steps:
    - uses: actions/download-artifact@v4
    - run: echo "${{ secrets.PUSH_ONLY }}"
`)}}
	facts, err := analyzeWorkflows(workflows, actions, domain.TokenPermissionsReadOnly, nil, nil)
	if err != nil {
		t.Fatalf("analyze non-PR local action: %v", err)
	}
	if !facts.ArtifactConsumers || facts.SecretBearingPRJobs {
		t.Fatalf("non-PR local action facts = %+v", facts)
	}
}

func TestAnalyzeWorkflowsWorkflowRunArtifactConsumer(t *testing.T) {
	facts := analyzeOne(t, `on: workflow_run
jobs:
  consume:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/download-artifact@v4
`, domain.TokenPermissionsReadOnly, nil)
	if !facts.ArtifactConsumers {
		t.Fatalf("workflow_run artifact consumer not detected: %+v", facts)
	}
}

func TestAnalyzeWorkflowsLocalReusableWorkflow(t *testing.T) {
	sources := []workflowSource{
		{Path: ".github/workflows/ci.yml", Active: true, Content: []byte(`on: pull_request
jobs:
  delegated:
    uses: ./.github/workflows/reusable.yml
`)},
		{Path: ".github/workflows/reusable.yml", Active: true, Content: []byte(`on: workflow_call
permissions:
  contents: write
jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - run: true
`)},
	}
	facts, err := analyzeWorkflows(sources, nil, domain.TokenPermissionsReadOnly, nil, nil)
	if err != nil {
		t.Fatalf("analyze local reusable: %v", err)
	}
	if !facts.ReusableWorkflows {
		t.Fatal("local reusable workflow not attested")
	}
	if facts.EffectiveTokenPerms != domain.TokenPermissionsReadWrite {
		t.Fatalf("reusable workflow authority not treated as PR-reachable: %+v", facts)
	}
}

func TestAnalyzeWorkflowsSecretExpressionFormsAndScopes(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"workflow env compact", "env:\n  TOKEN: '${{secrets.TOP}}'\n", true},
		{"job env bracket", "", true},
		{"step with nested expression", "", true},
		{"service credentials", "", true},
		{"whole secret context", "", true},
		{"non-secret contexts", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jobEnv, step := "", "      - run: true\n"
			switch tt.name {
			case "job env bracket":
				jobEnv = "    env:\n      TOKEN: \"${{ secrets['JOB'] }}\"\n"
			case "step with nested expression":
				step = "      - uses: owner/action@v1\n        with:\n          token: \"${{ format('{0}', secrets [ 'STEP' ]) }}\"\n"
			case "service credentials":
				jobEnv = "    services:\n      database:\n        image: postgres\n        credentials:\n          password: '${{ SECRETS[\"DATABASE\"] }}'\n"
			case "whole secret context":
				step = "      - run: echo \"${{ toJSON(secrets) }}\"\n"
			case "non-secret contexts":
				step = "      - run: echo secrets.NOT_AN_EXPRESSION\n        env:\n          TOKEN: '${{ vars.secrets }}'\n"
			}
			body := "on: pull_request\n" + tt.body + "jobs:\n  check:\n" + jobEnv + "    runs-on: ubuntu-latest\n    steps:\n" + step
			facts := analyzeOne(t, body, domain.TokenPermissionsReadOnly, nil)
			if facts.SecretBearingPRJobs != tt.want {
				t.Fatalf("SecretBearingPRJobs = %v, want %v", facts.SecretBearingPRJobs, tt.want)
			}
		})
	}
}

func TestAnalyzeWorkflowsLocalCompositeActionPrivileges(t *testing.T) {
	workflows := []workflowSource{{Path: ".github/workflows/ci.yml", Active: true, Content: []byte(`on: pull_request
jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/deploy
`)}}
	actions := []localActionSource{{Path: ".github/actions/deploy/action.yml", Content: []byte(`name: deploy
runs:
  using: composite
  steps:
    - uses: actions/download-artifact@v4
`)}}
	facts, err := analyzeWorkflows(workflows, actions, domain.TokenPermissionsReadOnly, nil, nil)
	if err != nil {
		t.Fatalf("analyze local composite action: %v", err)
	}
	if !facts.ArtifactConsumers {
		t.Fatalf("local composite action privilege facts = %+v", facts)
	}
}

func TestAnalyzeWorkflowsFailsClosed(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"malformed", "on: ["},
		{"multiple documents", "on: pull_request\njobs:\n  x:\n    runs-on: ubuntu-latest\n---\non: push\njobs:\n  y:\n    runs-on: ubuntu-latest\n"},
		{"dynamic permissions", "on: pull_request\njobs:\n  x:\n    permissions: '${{ matrix.permissions }}'\n    runs-on: ubuntu-latest\n"},
		{"remote reusable", "on: pull_request\njobs:\n  x:\n    uses: owner/repo/.github/workflows/x.yml@main\n"},
		{"missing local reusable", "on: pull_request\njobs:\n  x:\n    uses: ./.github/workflows/missing.yml\n"},
		{"missing local action", "on: pull_request\njobs:\n  x:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./tools/action\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := analyzeWorkflows([]workflowSource{{Path: ".github/workflows/ci.yml", Content: []byte(tt.body), Active: true}}, nil, domain.TokenPermissionsReadOnly, nil, nil)
			if err == nil || errors.Is(err, domain.ErrTrustProfileDrift) {
				t.Fatalf("error = %v, want parse/attestation failure", err)
			}
		})
	}
}
