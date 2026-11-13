package publish_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

// newEchoMintServer grants exactly the permissions each mint request names,
// as GitHub does for an App registration that holds them, and records each
// request's permissions.
func newEchoMintServer(t *testing.T, requests *[]map[string]string) *httptest.Server {
	t.Helper()
	srv := newMintServer(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Permissions map[string]string `json:"permissions"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("decode mint request: %v", err)
		}
		*requests = append(*requests, request.Permissions)
		fixture, err := os.ReadFile(filepath.Join("testdata", "token-response.json"))
		if err != nil {
			t.Errorf("read fixture: %v", err)
		}
		var response map[string]any
		if err := json.Unmarshal(fixture, &response); err != nil {
			t.Errorf("decode fixture: %v", err)
		}
		response["permissions"] = request.Permissions
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(response)
	})
	t.Cleanup(srv.Close)
	return srv
}

func stanceTrust(t *testing.T, stance domain.AutomationChangePolicy) memoryTrustSource {
	t.Helper()
	p := trustProfileWithStance(t, testTrustRepo, fixtureRepositoryID, stance)
	a := testWorkflowAudit(t)
	return memoryTrustSource{profile: &p, audit: &a}
}

// TestMintPublicationPermissionsFollowProfileStance: the publication mint
// requests workflows:write only under an advisory candidate_automation_changes
// profile (plan §5.5), and the audit records it as requested and granted;
// under block it never requests it.
func TestMintPublicationPermissionsFollowProfileStance(t *testing.T) {
	t.Parallel()
	for stance, want := range map[domain.AutomationChangePolicy]publish.Permissions{
		domain.AutomationChangesBlocked:  publish.PublishPermissions,
		domain.AutomationChangesAdvisory: publish.PublishWorkflowPermissions,
	} {
		t.Run(string(stance), func(t *testing.T) {
			t.Parallel()
			var requests []map[string]string
			srv := newEchoMintServer(t, &requests)
			rec := &captureRecorder{}
			m := newCoveredMinter(newRegisteredKeystore(t), srv.Client(), srv.URL, rec, stanceTrust(t, stance), fixedNow)
			tok, err := m.MintInstallationToken(context.Background(), testTrustRepo)
			if err != nil {
				t.Fatalf("MintInstallationToken: %v", err)
			}
			if len(requests) != 1 {
				t.Fatalf("requests = %d, want 1", len(requests))
			}
			_, requested := requests[0]["workflows"]
			if wantWorkflows := want.Workflows != ""; requested != wantWorkflows || requests[0]["workflows"] != want.Workflows {
				t.Fatalf("request permissions = %v, want workflows %q", requests[0], want.Workflows)
			}
			if tok.Permissions != want {
				t.Errorf("token permissions = %+v, want %+v", tok.Permissions, want)
			}
			if len(rec.records) != 1 || rec.records[0].Requested != want || rec.records[0].Granted != want {
				t.Fatalf("audit = %+v, want requested and granted %+v", rec.records, want)
			}
		})
	}
	if publish.PublishPermissions.Workflows != "" || publish.PublishWorkflowPermissions.Workflows != "write" ||
		publish.RegistrationPermissions.Workflows != "write" {
		t.Fatal("the registration and the advisory publication set carry workflows:write, the block set does not")
	}
	for name, set := range map[string]publish.Permissions{
		"workflow audit": publish.WorkflowAuditPermissions, "follow-up filing": publish.FollowUpFilingPermissions,
		"issue read": publish.IssueReadPermissions, "issue inspection": publish.IssueInspectionPermissions,
		"hand-back label": publish.HandBackLabelPermissions,
	} {
		if set.Workflows != "" {
			t.Errorf("%s permissions carry workflows %q", name, set.Workflows)
		}
	}
}

// TestMintRefusesAdvisoryGrantWithoutWorkflows: a registration whose owner
// has not accepted workflows:write grants the narrower set, which the mint
// refuses under an advisory profile before anything is audited or returned.
func TestMintRefusesAdvisoryGrantWithoutWorkflows(t *testing.T) {
	t.Parallel()
	srv := newMintServer(t, func(w http.ResponseWriter, _ *http.Request) {
		fixture, err := os.ReadFile(filepath.Join("testdata", "token-response.json"))
		if err != nil {
			t.Errorf("read fixture: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(fixture)
	})
	t.Cleanup(srv.Close)
	rec := &captureRecorder{}
	m := newCoveredMinter(newRegisteredKeystore(t), srv.Client(), srv.URL, rec,
		stanceTrust(t, domain.AutomationChangesAdvisory), fixedNow)
	if _, err := m.MintInstallationToken(context.Background(), testTrustRepo); !errors.Is(err, publish.ErrGrantMismatch) {
		t.Fatalf("err = %v, want ErrGrantMismatch", err)
	}
	if len(rec.records) != 0 {
		t.Fatalf("refused mint recorded %d audit rows", len(rec.records))
	}
}

// TestCachedTokenSourceRemintsWhenStanceChanges: the publication token cache
// keys on the current profile's stance, so a token minted under block is not
// reused once the profile turns advisory.
func TestCachedTokenSourceRemintsWhenStanceChanges(t *testing.T) {
	t.Parallel()
	var requests []map[string]string
	srv := newEchoMintServer(t, &requests)
	trust := stanceTrust(t, domain.AutomationChangesBlocked)
	m := newCoveredMinter(newRegisteredKeystore(t), srv.Client(), srv.URL, &captureRecorder{}, trust, fixedNow)
	src := publish.NewCachedTokenSource(m, fixedNow)

	blocked, err := src.Token(context.Background(), testTrustRepo)
	if err != nil {
		t.Fatal(err)
	}
	*trust.profile = trustProfileWithStance(t, testTrustRepo, fixtureRepositoryID, domain.AutomationChangesAdvisory)
	advisory, err := src.Token(context.Background(), testTrustRepo)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("mints = %d, want a fresh mint after the stance changed", len(requests))
	}
	if blocked.Permissions != publish.PublishPermissions || advisory.Permissions != publish.PublishWorkflowPermissions {
		t.Fatalf("tokens = %+v then %+v", blocked.Permissions, advisory.Permissions)
	}
	if !maps.Equal(requests[1], map[string]string{
		"actions": "read", "administration": "read", "contents": "write", "environments": "read",
		"pull_requests": "write", "metadata": "read", "workflows": "write",
	}) {
		t.Fatalf("advisory request = %v", requests[1])
	}
}
