package publish_test

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

const auditedWorkflowYAML = `name: publish
on:
  pull_request:
permissions:
  contents: write
  id-token: write
  packages: write
jobs:
  publish:
    runs-on: [self-hosted, linux]
    environment: production
    steps:
      - uses: actions/download-artifact@v4
      - run: echo "${{ secrets.DEPLOY_KEY }}"
`

type auditFixtureServer struct {
	t                 *testing.T
	mu                sync.Mutex
	refCalls          int
	changeOnce        bool
	allowedActions    string
	environmentWait   int
	requiredApprovals int
	localAction       string
	baseOnlyWorkflow  string
	runnerOverflow    bool
}

func (f *auditFixtureServer) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
		f.t.Errorf("%s has no bearer token", r.URL.Path)
	}
	if got := r.Header.Get("X-GitHub-Api-Version"); got != "2022-11-28" {
		f.t.Errorf("%s API version = %q", r.URL.Path, got)
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/repos/freeside-ai/evidence-repo/git/ref/heads/main":
		f.refCalls++
		sha := "base-sha"
		if f.changeOnce && f.refCalls == 1 {
			sha = "old-sha"
		}
		_, _ = fmt.Fprintf(w, `{"ref":"refs/heads/main","object":{"sha":%q}}`, sha)
	case "/repos/freeside-ai/evidence-repo/actions/permissions/workflow":
		_, _ = w.Write([]byte(`{"default_workflow_permissions":"read","can_approve_pull_request_reviews":false}`))
	case "/repos/freeside-ai/evidence-repo/actions/permissions":
		allowed := f.allowedActions
		if allowed == "" {
			allowed = "all"
		}
		_, _ = fmt.Fprintf(w, `{"enabled":true,"allowed_actions":%q}`, allowed)
	case "/repos/freeside-ai/evidence-repo/actions/permissions/selected-actions":
		_, _ = w.Write([]byte(`{"github_owned_allowed":true,"verified_allowed":false,"patterns_allowed":["freeside-ai/*"]}`))
	case "/repos/freeside-ai/evidence-repo/actions/workflows":
		_, _ = w.Write([]byte(`{"total_count":1,"workflows":[{"path":".github/workflows/publish.yml","state":"active"}]}`))
	case "/repos/freeside-ai/evidence-repo/git/trees/base-sha", "/repos/freeside-ai/evidence-repo/git/trees/old-sha":
		entries := []string{`{"path":".github/workflows/publish.yml","type":"blob","sha":"workflow-blob"}`}
		if f.localAction != "" {
			entries = append(entries, `{"path":".github/actions/release/action.yml","type":"blob","sha":"action-blob"}`)
		}
		if f.baseOnlyWorkflow != "" {
			entries = append(entries, `{"path":".github/workflows/base-only.yml","type":"blob","sha":"base-only-blob"}`)
		}
		_, _ = fmt.Fprintf(w, `{"truncated":false,"tree":[%s]}`, strings.Join(entries, ","))
	case "/repos/freeside-ai/evidence-repo/contents/.github/workflows/publish.yml":
		if r.URL.Query().Get("ref") == "" {
			f.t.Error("workflow content request is not SHA-pinned")
		}
		encoded := base64.StdEncoding.EncodeToString([]byte(auditedWorkflowYAML))
		_, _ = fmt.Fprintf(w, `{"type":"file","encoding":"base64","content":%q,"sha":"workflow-blob","path":".github/workflows/publish.yml"}`, encoded)
	case "/repos/freeside-ai/evidence-repo/contents/.github/actions/release/action.yml":
		encoded := base64.StdEncoding.EncodeToString([]byte(f.localAction))
		_, _ = fmt.Fprintf(w, `{"type":"file","encoding":"base64","content":%q,"sha":"action-blob","path":".github/actions/release/action.yml"}`, encoded)
	case "/repos/freeside-ai/evidence-repo/contents/.github/workflows/base-only.yml":
		encoded := base64.StdEncoding.EncodeToString([]byte(f.baseOnlyWorkflow))
		_, _ = fmt.Fprintf(w, `{"type":"file","encoding":"base64","content":%q,"sha":"base-only-blob","path":".github/workflows/base-only.yml"}`, encoded)
	case "/repos/freeside-ai/evidence-repo/environments":
		wait := f.environmentWait
		if wait == 0 {
			wait = 10
		}
		_, _ = fmt.Fprintf(w, `{"total_count":1,"environments":[{"name":"production","protection_rules":[{"type":"wait_timer","wait_timer":%d}],"deployment_branch_policy":{"protected_branches":true,"custom_branch_policies":false}}]}`, wait)
	case "/repos/freeside-ai/evidence-repo/environments/production/secrets":
		_, _ = w.Write([]byte(`{"total_count":1,"secrets":[{"name":"DEPLOY_KEY"}]}`))
	case "/repos/freeside-ai/evidence-repo/actions/runners":
		if f.runnerOverflow {
			_, _ = w.Write([]byte(`{"total_count":10001,"runners":[`))
			for i := 0; i < 100; i++ {
				if i > 0 {
					_, _ = w.Write([]byte(","))
				}
				_, _ = fmt.Fprintf(w, `{"name":"runner-%d","labels":[{"name":"gpu"}]}`, i)
			}
			_, _ = w.Write([]byte(`]}`))
			break
		}
		_, _ = w.Write([]byte(`{"total_count":1,"runners":[{"name":"runner-1","labels":[{"name":"self-hosted"},{"name":"linux"}]}]}`))
	case "/repos/freeside-ai/evidence-repo/branches/main/protection":
		_, _ = w.Write([]byte(`{"required_status_checks":{"contexts":["ci"]},"enforce_admins":{"enabled":true}}`))
	case "/repos/freeside-ai/evidence-repo/rulesets":
		_, _ = w.Write([]byte(`[{"id":42,"name":"main","enforcement":"active"}]`))
	case "/repos/freeside-ai/evidence-repo/rulesets/42":
		approvals := f.requiredApprovals
		if approvals == 0 {
			approvals = 1
		}
		_, _ = fmt.Fprintf(w, `{"id":42,"name":"main","enforcement":"active","rules":[{"type":"pull_request","parameters":{"required_approving_review_count":%d}}]}`, approvals)
	default:
		f.t.Errorf("unexpected request %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		http.NotFound(w, r)
	}
}

func runAuditFixture(t *testing.T, fixture *auditFixtureServer) domain.WorkflowAudit {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(fixture.handler))
	t.Cleanup(server.Close)
	auditor, err := publish.NewGitHubWorkflowAuditor(testTokenSource(), server.Client(), server.URL, time.Now)
	if err != nil {
		t.Fatalf("NewGitHubWorkflowAuditor: %v", err)
	}
	audit, err := auditor.Audit(t.Context(), "freeside-ai/evidence-repo", "main")
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	return audit
}

func TestGitHubWorkflowAuditorProducesFreshObservation(t *testing.T) {
	fixture := &auditFixtureServer{t: t, changeOnce: true}
	server := httptest.NewServer(http.HandlerFunc(fixture.handler))
	defer server.Close()
	now := time.Date(2026, 7, 21, 19, 0, 0, 0, time.FixedZone("CDT", -5*60*60))
	auditor, err := publish.NewGitHubWorkflowAuditor(testTokenSource(), server.Client(), server.URL, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewGitHubWorkflowAuditor: %v", err)
	}
	audit, err := auditor.Audit(t.Context(), "freeside-ai/evidence-repo", "main")
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if audit.AuditedCommitSHA != "base-sha" || !audit.AuditedAt.Equal(now) || audit.AuditedAt.Location() != time.UTC {
		t.Fatalf("audit identity/time = %+v", audit)
	}
	if audit.WorkflowAuditDigest == "" || audit.EffectiveTokenPerms != domain.TokenPermissionsReadWrite ||
		!audit.OIDCAvailable || !audit.EnvironmentSecrets || !audit.SecretBearingPRJobs ||
		!audit.SelfHostedRunners || !audit.PackagePublishing || !audit.ArtifactConsumers {
		t.Fatalf("audit privileges = %+v", audit)
	}
	if audit.PullRequestTarget || audit.ReusableWorkflows {
		t.Fatalf("unexpected audit privileges = %+v", audit)
	}
	if fixture.refCalls != 4 {
		t.Fatalf("ref calls = %d, want 4 for one unstable and one stable collection", fixture.refCalls)
	}
}

func TestGitHubWorkflowAuditorFailsClosedOnMissingScope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/git/ref/") {
			_, _ = w.Write([]byte(`{"ref":"refs/heads/main","object":{"sha":"base-sha"}}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"scope missing","token":"must-not-leak"}`))
	}))
	defer server.Close()
	auditor, err := publish.NewGitHubWorkflowAuditor(testTokenSource(), server.Client(), server.URL, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = auditor.Audit(t.Context(), "freeside-ai/evidence-repo", "main")
	if !errors.Is(err, publish.ErrGitHubAPI) {
		t.Fatalf("error = %v, want ErrGitHubAPI", err)
	}
	if strings.Contains(err.Error(), "must-not-leak") {
		t.Fatalf("error leaked response body: %v", err)
	}
}

func TestGitHubWorkflowAuditorFailsClosedAtPaginationLimit(t *testing.T) {
	fixture := &auditFixtureServer{t: t, runnerOverflow: true}
	server := httptest.NewServer(http.HandlerFunc(fixture.handler))
	defer server.Close()
	auditor, err := publish.NewGitHubWorkflowAuditor(testTokenSource(), server.Client(), server.URL, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = auditor.Audit(t.Context(), "freeside-ai/evidence-repo", "main")
	if err == nil || !strings.Contains(err.Error(), "pagination exceeded 100 pages") {
		t.Fatalf("error = %v, want pagination limit failure", err)
	}
}

func TestGitHubWorkflowAuditorEnumeratesPinnedBaseWorkflows(t *testing.T) {
	audit := runAuditFixture(t, &auditFixtureServer{t: t, baseOnlyWorkflow: `on: pull_request_target
jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - run: true
`})
	if !audit.PullRequestTarget {
		t.Fatalf("base-only workflow was not analyzed: %+v", audit)
	}
}

func TestGitHubWorkflowAuditorDigestCoversLiveSettings(t *testing.T) {
	baseline := runAuditFixture(t, &auditFixtureServer{t: t})
	tests := []struct {
		name    string
		fixture *auditFixtureServer
	}{
		{"selected Actions policy", &auditFixtureServer{t: t, allowedActions: "selected"}},
		{"environment protection", &auditFixtureServer{t: t, environmentWait: 20}},
		{"ruleset detail", &auditFixtureServer{t: t, requiredApprovals: 2}},
		{"local composite action", &auditFixtureServer{t: t, localAction: "name: release\nruns:\n  using: composite\n  steps: []\n"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := runAuditFixture(t, tt.fixture)
			if got.WorkflowAuditDigest == baseline.WorkflowAuditDigest {
				t.Fatalf("settings change retained digest %q", got.WorkflowAuditDigest)
			}
		})
	}
}
