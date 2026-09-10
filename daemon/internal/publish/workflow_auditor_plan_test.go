package publish_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

const (
	auditRepoPath = "/repos/freeside-ai/evidence-repo"
	planResponse  = `{"message":"Upgrade to GitHub Pro or make this repository public to enable this feature.","token":"must-not-leak"}`
)

type auditResponse struct {
	status int
	body   string
}

func auditWithResponses(t *testing.T, responses map[string]auditResponse) (domain.WorkflowAudit, error) {
	t.Helper()
	fixture := &auditFixtureServer{t: t}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response, ok := responses[r.URL.RequestURI()]
		if !ok {
			response, ok = responses[r.URL.Path]
		}
		if ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(response.status)
			_, _ = w.Write([]byte(response.body))
			return
		}
		fixture.handler(w, r)
	}))
	t.Cleanup(server.Close)
	auditor, err := publish.NewGitHubWorkflowAuditor(testTokenSource(), server.Client(), server.URL, func() time.Time {
		return time.Date(2026, 9, 10, 4, 0, 0, 0, time.UTC)
	})
	if err != nil {
		t.Fatal(err)
	}
	return auditor.Audit(t.Context(), "freeside-ai/evidence-repo", "main")
}

func TestWorkflowAuditRecordsPlanUnavailableWithoutLosingAuthority(t *testing.T) {
	t.Parallel()
	baseline, err := auditWithResponses(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, fields := range [][]string{{"branch_protection"}, {"rulesets"}, {"branch_protection", "rulesets"}} {
		t.Run(strings.Join(fields, "+"), func(t *testing.T) {
			responses := map[string]auditResponse{}
			for _, field := range fields {
				endpoint := auditRepoPath + "/rulesets"
				if field == "branch_protection" {
					endpoint = auditRepoPath + "/branches/main/protection"
				}
				responses[endpoint] = auditResponse{http.StatusForbidden, planResponse}
			}
			got, err := auditWithResponses(t, responses)
			if err != nil {
				t.Fatal(err)
			}
			again, err := auditWithResponses(t, responses)
			if err != nil || again.WorkflowAuditDigest != got.WorkflowAuditDigest {
				t.Fatalf("unstable unavailable evidence: %v", err)
			}
			if baseline.WorkflowAuditDigest == got.WorkflowAuditDigest {
				t.Fatal("capability transition did not change the approval-bound digest")
			}
			var expected, actual map[string]any
			if err := json.Unmarshal(baseline.Evidence.Bytes(), &expected); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(got.Evidence.Bytes(), &actual); err != nil {
				t.Fatal(err)
			}
			for _, field := range fields {
				expected[field] = map[string]any{"plan_unavailable": true}
			}
			if !reflect.DeepEqual(expected, actual) {
				t.Fatal("audit changed evidence beyond the unavailable features")
			}
			baselineFacts := baseline
			got.Evidence, baselineFacts.Evidence = nil, nil
			got.WorkflowAuditDigest, baselineFacts.WorkflowAuditDigest = "", ""
			if !reflect.DeepEqual(got, baselineFacts) {
				t.Fatal("unavailable governance feature changed effective-authority facts")
			}
		})
	}
}

func TestWorkflowAuditUnavailableIsDistinctFromAbsent(t *testing.T) {
	t.Parallel()
	absent, err := auditWithResponses(t, map[string]auditResponse{
		auditRepoPath + "/branches/main/protection": {http.StatusNotFound, `{}`},
		auditRepoPath + "/rulesets":                 {http.StatusOK, `[]`},
	})
	if err != nil {
		t.Fatal(err)
	}
	unavailable, err := auditWithResponses(t, map[string]auditResponse{
		auditRepoPath + "/branches/main/protection": {http.StatusForbidden, planResponse},
		auditRepoPath + "/rulesets":                 {http.StatusForbidden, planResponse},
	})
	if err != nil {
		t.Fatal(err)
	}
	if absent.WorkflowAuditDigest == unavailable.WorkflowAuditDigest {
		t.Fatal("unavailable features were recorded as absent")
	}
	if !strings.Contains(string(absent.Evidence.Bytes()), `"branch_protection":null,"rulesets":[]`) {
		t.Fatal("legacy absent-feature encoding changed")
	}
}

func TestWorkflowAuditPlanHandlingRefusesOtherErrors(t *testing.T) {
	t.Parallel()
	for _, endpoint := range []string{"/branches/main/protection", "/rulesets"} {
		for _, response := range []auditResponse{
			{http.StatusForbidden, `{"message":"scope missing","token":"must-not-leak"}`},
			{http.StatusForbidden, `{"message":"API rate limit exceeded"}`},
			{http.StatusForbidden, `{"message":"Upgrade to GitHub Pro to enable this feature."}`},
			{http.StatusForbidden, planResponse + `{}`},
			{http.StatusForbidden, `{"message":`},
			{http.StatusForbidden, `null`},
			{http.StatusUnauthorized, planResponse},
			{http.StatusTooManyRequests, planResponse},
			{http.StatusInternalServerError, planResponse},
		} {
			_, err := auditWithResponses(t, map[string]auditResponse{auditRepoPath + endpoint: response})
			var apiErr *publish.APIError
			if !errors.As(err, &apiErr) || apiErr.Status != response.status {
				t.Fatalf("%s status %d: got %v, want retained API refusal", endpoint, response.status, err)
			}
			if strings.Contains(err.Error(), "must-not-leak") {
				t.Fatal("API refusal exposed response content")
			}
		}
	}
}

func TestWorkflowAuditPlanHandlingDoesNotSkipRequiredReads(t *testing.T) {
	t.Parallel()
	for _, endpoint := range []string{
		"/actions/permissions", "/actions/permissions/workflow", "/actions/workflows",
		"/environments", "/environments/production/secrets", "/actions/runners", "/rulesets/42",
	} {
		_, err := auditWithResponses(t, map[string]auditResponse{
			auditRepoPath + endpoint: {http.StatusForbidden, planResponse},
		})
		if !errors.Is(err, publish.ErrGitHubAPI) {
			t.Fatalf("%s: required read was skipped: %v", endpoint, err)
		}
	}
}

func TestWorkflowAuditPlanRestrictionAfterRulesetPageFails(t *testing.T) {
	t.Parallel()
	_, err := auditWithResponses(t, map[string]auditResponse{
		auditRepoPath + "/rulesets?includes_parents=true&per_page=100&page=1": {http.StatusOK, `[` + strings.Repeat(`{"id":42},`, 99) + `{"id":42}]`},
		auditRepoPath + "/rulesets?includes_parents=true&per_page=100&page=2": {http.StatusForbidden, planResponse},
	})
	if !errors.Is(err, publish.ErrGitHubAPI) {
		t.Fatalf("partial ruleset collection accepted: %v", err)
	}
}
