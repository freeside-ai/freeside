package publish_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestPublisherRejectsNonAtomicStoreAdapterWiring(t *testing.T) {
	first, second := newTestStore(t), newTestStore(t)
	ledger, err := publish.NewStoreLedger(first)
	if err != nil {
		t.Fatal(err)
	}
	firstTrust, err := publish.NewStoreTrustSource(first)
	if err != nil {
		t.Fatal(err)
	}
	secondTrust, err := publish.NewStoreTrustSource(second)
	if err != nil {
		t.Fatal(err)
	}
	authz, err := publish.NewStoreAuthorizationSource(first)
	if err != nil {
		t.Fatal(err)
	}
	gh := newFakeGitHub(t)
	server := gh.server()
	tests := []struct {
		name  string
		trust publish.TrustSource
		authz publish.AuthorizationSource
		want  string
	}{
		{"different stores", secondTrust, authz, "must share one store"},
		{"mixed adapters", firstTrust, conformantAuthz(t), "cannot compose atomically"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := publish.NewPublisher(testTokenSource(), server.Client(), server.URL, fixedWorkflowAuditor{}, ledger, tt.trust, tt.authz)
			_, err := p.Publish(t.Context(), publish.Candidate{}, nil)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
	if requests := gh.requestLog(); len(requests) != 0 {
		t.Fatalf("invalid wiring reached forge: %v", requests)
	}
}

type fixedWorkflowAuditor struct {
	audit domain.WorkflowAudit
	err   error
}

func (a fixedWorkflowAuditor) Audit(context.Context, string, string) (domain.WorkflowAudit, error) {
	return a.audit, a.err
}

func storeBackedPublisher(t *testing.T, s *store.Store, gh *fakeGitHub, auditor publish.WorkflowAuditor) *publish.Publisher {
	t.Helper()
	ledger, err := publish.NewStoreLedger(s)
	if err != nil {
		t.Fatal(err)
	}
	trust, err := publish.NewStoreTrustSource(s)
	if err != nil {
		t.Fatal(err)
	}
	authz, err := publish.NewStoreAuthorizationSource(s)
	if err != nil {
		t.Fatal(err)
	}
	server := gh.server()
	return publish.NewPublisher(testTokenSource(), server.Client(), server.URL, auditor, ledger, trust, authz)
}

func seedDecisionRecords(t *testing.T, s *store.Store) {
	t.Helper()
	profile := testTrustProfile(t)
	auth := testCandidateAuthorization(t)
	if err := s.WriteInternal(t.Context(), func(tx *store.InternalTx) error {
		if err := tx.RecordTrustProfile(t.Context(), profile, auth.CreatedAt); err != nil {
			return err
		}
		return tx.RecordCandidateAuthorization(t.Context(), auth)
	}); err != nil {
		t.Fatalf("seed decision records: %v", err)
	}
}

func assertDecisionRows(t *testing.T, s *store.Store, wantAudits, wantIntents int) {
	t.Helper()
	if err := s.Read(t.Context(), func(tx *store.ReadTx) error {
		audits, err := tx.ListWorkflowAudits(t.Context(), testTrustRepo)
		if err != nil {
			return err
		}
		if len(audits) != wantAudits {
			t.Errorf("workflow audits = %d, want %d", len(audits), wantAudits)
		}
		intents, err := tx.ListPendingOutbox(t.Context(), publish.IntentKindPublication)
		if err != nil {
			return err
		}
		if len(intents) != wantIntents {
			t.Errorf("pending intents = %d, want %d", len(intents), wantIntents)
		}
		return nil
	}); err != nil {
		t.Fatalf("read decision rows: %v", err)
	}
}

func TestStorePublicationDecisionRecordsDriftedAuditWithoutIntent(t *testing.T) {
	s := newTestStore(t)
	seedDecisionRecords(t, s)
	audit := testWorkflowAudit(t)
	audit.OIDCAvailable = true
	gh := newFakeGitHub(t)
	p := storeBackedPublisher(t, s, gh, fixedWorkflowAuditor{audit: audit})

	_, err := p.Publish(t.Context(), testCandidate(t), testApprovedRecipes())
	if !errors.Is(err, publish.ErrTrustProfileDrift) {
		t.Fatalf("Publish error = %v, want ErrTrustProfileDrift", err)
	}
	assertDecisionRows(t, s, 1, 0)
	if requests := gh.requestLog(); len(requests) != 0 {
		t.Fatalf("drifted decision reached forge: %v", requests)
	}
}

func TestStorePublicationDecisionCommitsFreshAuditWithIntent(t *testing.T) {
	s := newTestStore(t)
	seedDecisionRecords(t, s)
	gh := newFakeGitHub(t)
	p := storeBackedPublisher(t, s, gh, fixedWorkflowAuditor{audit: testWorkflowAudit(t)})

	if _, err := p.Publish(t.Context(), testCandidate(t), testApprovedRecipes()); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	assertDecisionRows(t, s, 1, 1)
}
