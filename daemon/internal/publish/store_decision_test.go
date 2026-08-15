package publish_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestPublisherRejectsNonAtomicStoreAdapterWiring(t *testing.T) {
	t.Parallel()
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

func executionWorkflowAudit(t *testing.T) domain.WorkflowAudit {
	t.Helper()
	audit := testWorkflowAudit(t)
	audit.AuditedCommitSHA = testBaseSHA
	return audit
}

const testProducingInvocationID = domain.InvocationID("inv-producing-0001")

type executionChainOptions struct {
	reservationRunID domain.RunID
	repo             string
	repositoryID     int64
	baseRef          string
	baseSHA          string
	exportHead       *string
	omitReservation  bool
	attended         bool
}

const testBackendConfigurationDigest = domain.Digest(
	"sha256:1111111111111111111111111111111111111111111111111111111111111111",
)

func executionStoreOptions() store.Options {
	return store.Options{
		ApprovedRecipes: testApprovedRecipes(),
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
			domain.ModeUnattended:  domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		},
		ApprovedCredentialModes: []domain.CredentialMode{
			domain.CredentialSubscriptionContained,
		},
		BackupHealthSource: store.BackupHealthSourceFunc(func(
			context.Context,
			store.BackupHealthContext,
		) (domain.BackupHealth, error) {
			return domain.BackupHealth{
				Encryption:         domain.BackupHealthHealthy,
				CheckpointCurrency: domain.BackupHealthHealthy,
				ArtifactClosure:    domain.BackupHealthHealthy,
				RestoreTestAge:     domain.BackupHealthHealthy,
			}, nil
		}),
	}
}

func testExecutionCapabilities(t *testing.T) domain.CapabilitySnapshot {
	t.Helper()
	capabilities, ok := domain.ProvableCapabilities(
		domain.BackendFreshVMReadOnlyVolumeHandoff,
	)
	if !ok {
		t.Fatal("fresh-vm backend has no provable capability ceiling")
	}
	return capabilities
}

func newExecutionBoundStore(t *testing.T, opts executionChainOptions) *store.Store {
	t.Helper()
	return newExecutionBoundStoreAt(t, filepath.Join(t.TempDir(), "store.db"), opts)
}

func newExecutionBoundStoreAt(
	t *testing.T, path string, opts executionChainOptions,
) *store.Store {
	t.Helper()
	s, err := store.Open(
		t.Context(),
		path,
		executionStoreOptions(),
	)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	return s
}

func seedExecutionPublicationChain(
	t *testing.T,
	s *store.Store,
	opts executionChainOptions,
) publish.Reservation {
	t.Helper()
	ctx := t.Context()
	run := domain.Run{
		ID: "run-0001", ProjectID: "project-0001",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
		Stages: []domain.Stage{{
			ID: "implement-run-0001", RunID: "run-0001", Name: "implement",
			Attempts: []domain.Attempt{{
				ID: "attempt-producing-0001", StageID: "implement-run-0001",
				Number: 1, InvocationID: testProducingInvocationID,
			}},
		}},
	}
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutRun(ctx, run)
	}); err != nil {
		t.Fatalf("seed producing run: %v", err)
	}

	identity := domain.AuthIdentity{
		ID: "auth-producing-0001", Provider: "claude",
		AuthStoreMutationLease: true, AuthStoreVolume: "provider-cred",
		MaxParallelExecutions: 1, RefreshStrategy: domain.RefreshOnDemand,
	}
	identityID := identity.ID
	if opts.repo == "" {
		opts.repo = "freeside-ai/evidence-repo"
	}
	if opts.baseRef == "" {
		opts.baseRef = "main"
	}
	if opts.repositoryID == 0 {
		opts.repositoryID = fixtureRepositoryID
	}
	if opts.baseSHA == "" {
		opts.baseSHA = testBaseSHA
	}
	operatingMode := domain.ModeUnattended
	capabilities := testExecutionCapabilities(t)
	backendConfigurationDigest := testBackendConfigurationDigest
	sourceProfile := trustProfileForRepoID(t, opts.repo, opts.repositoryID)
	trustProfileDigest := sourceProfile.ProfileDigest
	trustProfileBinding := &trustProfileDigest
	if opts.attended {
		operatingMode = domain.ModeAttendedDev
		capabilities = domain.NewCapabilitySnapshot(
			domain.CapDetachableWorkspace,
			domain.CapPostExitExport,
		)
		backendConfigurationDigest = ""
		trustProfileBinding = nil
	}
	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID:               testProducingInvocationID,
		RunID:                      run.ID,
		StageID:                    run.Stages[0].ID,
		AttemptID:                  run.Stages[0].Attempts[0].ID,
		Backend:                    string(domain.BackendFreshVMReadOnlyVolumeHandoff),
		Capabilities:               capabilities,
		BackendConfigurationDigest: backendConfigurationDigest,
		OperatingMode:              operatingMode,
		CredentialMode:             domain.CredentialSubscriptionContained,
		EgressProfile:              domain.EgressProviderOnly,
		ImageRef: domain.ImageRef(
			"ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("ab", 32),
		),
		SpecDigest: run.SpecDigest, PolicyDigest: run.PolicyDigest,
		InputDigest: "sha256:input",
		Base: domain.BaseRevision{
			Repo: opts.repo, RepositoryID: opts.repositoryID,
			BaseRef: opts.baseRef, BaseSHA: opts.baseSHA,
		},
		Workspace:          "workspace-producing-0001",
		AuthIdentityID:     &identityID,
		TrustProfileDigest: trustProfileBinding,
		AdmittedAt:         fixtureTime,
	})
	if err != nil {
		t.Fatalf("NewExecutionAdmission: %v", err)
	}
	if opts.reservationRunID == "" {
		opts.reservationRunID = run.ID
	}
	reservation, err := publish.NewReservation("inv-0001", opts.reservationRunID)
	if err != nil {
		t.Fatalf("NewReservation: %v", err)
	}
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		conformance, err := domain.NewBackendConformance(
			domain.BackendConformanceInput{
				Backend:             domain.BackendFreshVMReadOnlyVolumeHandoff,
				Outcome:             domain.ConformancePassed,
				ConfigurationDigest: testBackendConfigurationDigest,
				Capabilities:        testExecutionCapabilities(t),
				ProvedAt:            fixtureTime.Add(-time.Minute),
			},
		)
		if err != nil {
			return err
		}
		if _, err := tx.RecordBackendConformance(ctx, conformance); err != nil {
			return err
		}
		if !opts.attended {
			if err := tx.RecordTrustProfile(
				ctx, sourceProfile, fixtureTime.Add(-2*time.Minute),
			); err != nil {
				return err
			}
		}
		if err := tx.RecordAuthIdentity(ctx, identity, fixtureTime); err != nil {
			return err
		}
		if err := tx.RecordExecutionAdmission(ctx, admission); err != nil {
			return err
		}
		if opts.exportHead != nil {
			export, err := domain.NewExecutionExport(domain.ExecutionExportInput{
				InvocationID:    testProducingInvocationID,
				AdmissionID:     admission.ID,
				ObservedBaseSHA: admission.Base.BaseSHA,
				HeadSHA:         *opts.exportHead,
				ManifestDigest:  "sha256:manifest",
				RecordedAt:      fixtureTime.Add(time.Minute),
			})
			if err != nil {
				return err
			}
			if err := tx.RecordExecutionExport(ctx, export); err != nil {
				return err
			}
		}
		currentProfile := testTrustProfile(t)
		if !opts.attended &&
			sourceProfile.Repo == currentProfile.Repo &&
			sourceProfile.ProfileDigest != currentProfile.ProfileDigest {
			if err := tx.ActivateTrustProfile(
				ctx, currentProfile.Repo, currentProfile.ProfileDigest,
				fixtureTime.Add(2*time.Minute),
			); err != nil {
				return err
			}
		}
		if opts.omitReservation {
			return nil
		}
		return publish.ClaimInvocation(ctx, tx, reservation)
	}); err != nil {
		t.Fatalf("seed execution publication chain: %v", err)
	}
	return reservation
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
	t.Parallel()
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
	t.Parallel()
	s := newTestStore(t)
	seedDecisionRecords(t, s)
	gh := newFakeGitHub(t)
	p := storeBackedPublisher(t, s, gh, fixedWorkflowAuditor{audit: testWorkflowAudit(t)})

	candidate := testCandidate(t)
	if _, err := p.Publish(t.Context(), candidate, testApprovedRecipes()); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	assertDecisionRows(t, s, 1, 1)
	writesBeforeRetry := len(gh.writeRequests())
	if _, err := p.Publish(t.Context(), candidate, testApprovedRecipes()); err != nil {
		t.Fatalf("Publish committed history-free retry: %v", err)
	}
	if writes := gh.writeRequests(); len(writes) != writesBeforeRetry {
		t.Fatalf("committed history-free retry duplicated a forge effect: %v", writes)
	}
}

func TestPublishExecutionHoldsSourceHeadAgainstRecordedExport(t *testing.T) {
	t.Parallel()
	matchingHead := testHeadSHA
	mismatchingHead := testOtherSHA
	tests := []struct {
		name    string
		source  executionChainOptions
		wantErr error
	}{
		{name: "matching", source: executionChainOptions{exportHead: &matchingHead}},
		{
			name:    "mismatching",
			source:  executionChainOptions{exportHead: &mismatchingHead},
			wantErr: publish.ErrExecutionExportHeadMismatch,
		},
		{
			name:    "missing export",
			wantErr: publish.ErrExecutionExportMissing,
		},
		{
			name: "missing publication reservation",
			source: executionChainOptions{
				exportHead: &matchingHead, omitReservation: true,
			},
			wantErr: publish.ErrInvocationReserved,
		},
		{
			name: "attended producing execution",
			source: executionChainOptions{
				exportHead: &matchingHead, attended: true,
			},
			wantErr: publish.ErrUnauthorizedPublication,
		},
		{
			name: "producing invocation from another run",
			source: executionChainOptions{
				reservationRunID: "run-other", exportHead: &matchingHead,
			},
			wantErr: domain.ErrParentKeyMismatch,
		},
		{
			name: "producing invocation from another repository",
			source: executionChainOptions{
				repo: "freeside-ai/other", exportHead: &matchingHead,
			},
			wantErr: domain.ErrParentKeyMismatch,
		},
		{
			name: "producing invocation from transferred repository name",
			source: executionChainOptions{
				repositoryID: fixtureRepositoryID + 1, exportHead: &matchingHead,
			},
			wantErr: domain.ErrParentKeyMismatch,
		},
		{
			name: "producing invocation from another base ref",
			source: executionChainOptions{
				baseRef: "release", exportHead: &matchingHead,
			},
			wantErr: domain.ErrParentKeyMismatch,
		},
		{
			name: "producing invocation from another base commit",
			source: executionChainOptions{
				baseSHA: testOtherSHA, exportHead: &matchingHead,
			},
			wantErr: domain.ErrParentKeyMismatch,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newExecutionBoundStore(t, tt.source)
			seedDecisionRecords(t, s)
			reservation := seedExecutionPublicationChain(t, s, tt.source)
			gh := newFakeGitHub(t)
			p := storeBackedPublisher(
				t, s, gh, fixedWorkflowAuditor{audit: executionWorkflowAudit(t)},
			)
			candidate := testCandidate(t)
			candidate.RunID = reservation.RunID
			candidate.DispositionHistory = testDispositionHistory(t, s, candidate)

			result, err := p.PublishExecution(
				t.Context(),
				publish.ExecutionCandidate{
					Candidate:             candidate,
					ProducingInvocationID: testProducingInvocationID,
				},
				testApprovedRecipes(),
			)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("PublishExecution: %v", err)
				}
				if !result.PRCreated || result.PRNumber == 0 {
					t.Fatalf("PublishExecution result = %+v, want created PR", result)
				}
				assertDecisionRows(t, s, 1, 1)
				key, err := reservation.Key()
				if err != nil {
					t.Fatal(err)
				}
				if err := s.Read(t.Context(), func(tx *store.ReadTx) error {
					entry, err := tx.GetOutbox(t.Context(), key)
					if err != nil {
						return err
					}
					intent, err := publish.DecodeIntent(entry.Payload)
					if err != nil {
						return err
					}
					if entry.Kind != publish.IntentKindPublication ||
						intent.InvocationID != candidate.InvocationID ||
						intent.SourceHeadSHA != candidate.HeadSHA ||
						intent.ProducingInvocationID != testProducingInvocationID ||
						intent.ReservationRunID != candidate.RunID {
						t.Errorf("settled execution intent = %+v under kind %q",
							intent, entry.Kind)
					}
					return nil
				}); err != nil {
					t.Fatalf("read settled execution intent: %v", err)
				}
				writesBeforeRetry := len(gh.writeRequests())
				if _, err := p.PublishExecution(
					t.Context(),
					publish.ExecutionCandidate{
						Candidate:             candidate,
						ProducingInvocationID: testProducingInvocationID,
					},
					testApprovedRecipes(),
				); err != nil {
					t.Fatalf("PublishExecution committed retry: %v", err)
				}
				if writes := gh.writeRequests(); len(writes) != writesBeforeRetry {
					t.Fatalf("committed retry duplicated a forge effect: %v", writes)
				}
				gh.mu.Lock()
				prs := append([]fakePR(nil), gh.prs...)
				gh.mu.Unlock()
				if len(prs) != 1 ||
					strings.Count(prs[0].Body, "<!-- freeside:disposition-history ") != 1 ||
					strings.Count(prs[0].Body, "<!-- /freeside:disposition-history -->") != 1 {
					t.Fatalf("committed retry duplicated disposition history: %#v", prs)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("PublishExecution error = %v, want %v", err, tt.wantErr)
			}
			assertDecisionRows(t, s, 1, 0)
			if requests := gh.requestLog(); len(requests) != 0 {
				t.Fatalf("refused execution publication reached forge: %v", requests)
			}
			key, err := reservation.Key()
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Read(t.Context(), func(tx *store.ReadTx) error {
				entry, err := tx.GetOutbox(t.Context(), key)
				if tt.source.omitReservation && errors.Is(err, store.ErrNotFound) {
					return nil
				}
				if err != nil {
					return err
				}
				if tt.source.omitReservation {
					t.Errorf("unreserved invocation unexpectedly holds kind %q", entry.Kind)
					return nil
				}
				if entry.Kind != publish.IntentKindReservation {
					t.Errorf("reservation kind = %q, want %q",
						entry.Kind, publish.IntentKindReservation)
				}
				return nil
			}); err != nil {
				t.Fatalf("read retained reservation: %v", err)
			}
		})
	}
}

func TestPublishExecutionRefusesAdvancedTargetBeforeIntent(t *testing.T) {
	t.Parallel()
	head := testHeadSHA
	s := newExecutionBoundStore(t, executionChainOptions{exportHead: &head})
	seedDecisionRecords(t, s)
	reservation := seedExecutionPublicationChain(
		t, s, executionChainOptions{exportHead: &head},
	)
	audit := executionWorkflowAudit(t)
	audit.AuditedCommitSHA = testOtherSHA
	gh := newFakeGitHub(t)
	p := storeBackedPublisher(t, s, gh, fixedWorkflowAuditor{audit: audit})
	candidate := testCandidate(t)
	candidate.RunID = reservation.RunID
	candidate.DispositionHistory = testDispositionHistory(t, s, candidate)

	_, err := p.PublishExecution(t.Context(), publish.ExecutionCandidate{
		Candidate: candidate, ProducingInvocationID: testProducingInvocationID,
	}, testApprovedRecipes())
	if !errors.Is(err, publish.ErrTargetBaseAdvanced) {
		t.Fatalf("PublishExecution error = %v, want ErrTargetBaseAdvanced", err)
	}
	assertDecisionRows(t, s, 1, 0)
	if requests := gh.requestLog(); len(requests) != 0 {
		t.Fatalf("advanced target reached forge: %v", requests)
	}
	key, err := reservation.Key()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Read(t.Context(), func(tx *store.ReadTx) error {
		entry, err := tx.GetOutbox(t.Context(), key)
		if err != nil {
			return err
		}
		if entry.Kind != publish.IntentKindReservation {
			t.Errorf("advanced target changed reservation to %q", entry.Kind)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPublishExecutionRejectsDispositionHistoryFromAnotherStore(t *testing.T) {
	t.Parallel()
	head := testHeadSHA
	s := newExecutionBoundStore(t, executionChainOptions{exportHead: &head})
	seedDecisionRecords(t, s)
	reservation := seedExecutionPublicationChain(
		t, s, executionChainOptions{exportHead: &head},
	)
	gh := newFakeGitHub(t)
	p := storeBackedPublisher(
		t, s, gh, fixedWorkflowAuditor{audit: executionWorkflowAudit(t)},
	)
	candidate := testCandidate(t)
	candidate.RunID = reservation.RunID
	foreign := newExecutionBoundStore(t, executionChainOptions{})
	seedDecisionRecords(t, foreign)
	candidate.DispositionHistory = testDispositionHistory(t, foreign, candidate)

	_, err := p.PublishExecution(t.Context(), publish.ExecutionCandidate{
		Candidate: candidate, ProducingInvocationID: testProducingInvocationID,
	}, testApprovedRecipes())
	if err == nil || !strings.Contains(err.Error(), "does not come from the publisher decision store") {
		t.Fatalf("PublishExecution foreign disposition store = %v", err)
	}
	assertDecisionRows(t, s, 0, 0)
	if requests := gh.requestLog(); len(requests) != 0 {
		t.Fatalf("foreign disposition store reached forge: %v", requests)
	}
}

func TestPublishExecutionReauthenticatesDispositionHistoryInDecisionTransaction(t *testing.T) {
	t.Parallel()
	head := testHeadSHA
	s := newExecutionBoundStore(t, executionChainOptions{exportHead: &head})
	seedDecisionRecords(t, s)
	reservation := seedExecutionPublicationChain(
		t, s, executionChainOptions{exportHead: &head},
	)
	gh := newFakeGitHub(t)
	p := storeBackedPublisher(
		t, s, gh, fixedWorkflowAuditor{audit: executionWorkflowAudit(t)},
	)
	candidate := testCandidate(t)
	candidate.RunID = reservation.RunID
	candidate.DispositionHistory = testDispositionHistory(t, s, candidate)
	failure := domain.ReviewFailure{
		InvocationID: "review-failure-after-snapshot", RunID: candidate.RunID, Round: 2,
		BaseSHA: testOtherSHA, HeadSHA: candidate.HeadSHA,
		Class: domain.ReviewFailureContradiction, Reason: "late authoritative failure",
		ObservedAt: fixtureTime.Add(time.Minute),
	}
	if err := s.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.PutReviewFailure(t.Context(), failure)
	}); err != nil {
		t.Fatal(err)
	}
	_, err := p.PublishExecution(t.Context(), publish.ExecutionCandidate{
		Candidate: candidate, ProducingInvocationID: testProducingInvocationID,
	}, testApprovedRecipes())
	if err == nil || !strings.Contains(err.Error(), "latest review failure supersedes clean review") {
		t.Fatalf("PublishExecution after late review failure = %v", err)
	}
	assertDecisionRows(t, s, 1, 0)
	if requests := gh.requestLog(); len(requests) != 0 {
		t.Fatalf("late review failure reached forge: %v", requests)
	}
}

func TestPublishExecutionRejectsForeignReviewInstructionsAtDecision(t *testing.T) {
	t.Parallel()
	head := testHeadSHA
	s := newExecutionBoundStore(t, executionChainOptions{exportHead: &head})
	seedDecisionRecords(t, s)
	reservation := seedExecutionPublicationChain(
		t, s, executionChainOptions{exportHead: &head},
	)
	gh := newFakeGitHub(t)
	p := storeBackedPublisher(
		t, s, gh, fixedWorkflowAuditor{audit: executionWorkflowAudit(t)},
	)
	candidate := testCandidate(t)
	candidate.RunID = reservation.RunID
	candidate.DispositionHistory = testDispositionHistory(t, s, candidate)
	foreign, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-foreign-instructions", RunID: candidate.RunID, Round: 2,
		Provider: "openai", ModelConfiguration: "codex/high",
		ConfigurationDigest: testRecipe,
		InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("9", 64)),
		CostOwner:           "operator", BaseSHA: testOtherSHA, HeadSHA: candidate.HeadSHA,
		CompletedAt:        fixtureTime.Add(time.Minute),
		CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("8", 64)),
		Outcome:            domain.ReviewClean,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.PutReviewRecord(t.Context(), foreign, nil)
	}); err != nil {
		t.Fatal(err)
	}
	_, err = p.PublishExecution(t.Context(), publish.ExecutionCandidate{
		Candidate: candidate, ProducingInvocationID: testProducingInvocationID,
	}, testApprovedRecipes())
	if err == nil || !strings.Contains(err.Error(), "review authority disagrees") {
		t.Fatalf("PublishExecution under foreign review instructions = %v", err)
	}
	assertDecisionRows(t, s, 1, 0)
	if requests := gh.requestLog(); len(requests) != 0 {
		t.Fatalf("foreign review instructions reached forge: %v", requests)
	}
}

func TestPublishExecutionReportsReviewConfigurationMismatchAtDecision(t *testing.T) {
	t.Parallel()
	head := testHeadSHA
	s := newExecutionBoundStore(t, executionChainOptions{exportHead: &head})
	seedDecisionRecords(t, s)
	reservation := seedExecutionPublicationChain(
		t, s, executionChainOptions{exportHead: &head},
	)
	gh := newFakeGitHub(t)
	p := storeBackedPublisher(
		t, s, gh, fixedWorkflowAuditor{audit: executionWorkflowAudit(t)},
	)
	candidate := testCandidate(t)
	candidate.RunID = reservation.RunID
	candidate.DispositionHistory = testDispositionHistory(t, s, candidate)
	drifted := domain.Digest("sha256:" + strings.Repeat("7", 64))
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-drifted-configuration", RunID: candidate.RunID, Round: 2,
		Provider: "openai", ModelConfiguration: "codex/high",
		ConfigurationDigest: drifted,
		InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)),
		CostOwner:           "operator", BaseSHA: testBaseSHA, HeadSHA: candidate.HeadSHA,
		CompletedAt:        fixtureTime.Add(time.Minute),
		CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("8", 64)),
		Outcome:            domain.ReviewClean,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.PutReviewRecord(t.Context(), record, nil)
	}); err != nil {
		t.Fatal(err)
	}
	_, err = p.PublishExecution(t.Context(), publish.ExecutionCandidate{
		Candidate: candidate, ProducingInvocationID: testProducingInvocationID,
	}, testApprovedRecipes())
	if !errors.Is(err, domain.ErrReviewConfigurationUnapproved) ||
		!strings.Contains(err.Error(), string(testRecipe)) ||
		!strings.Contains(err.Error(), string(drifted)) {
		t.Fatalf("PublishExecution under drifted review configuration = %v", err)
	}
	assertDecisionRows(t, s, 1, 0)
	if requests := gh.requestLog(); len(requests) != 0 {
		t.Fatalf("drifted review configuration reached forge: %v", requests)
	}
}

func TestPublishExecutionRecoversCommittedIntentAfterTargetAdvances(t *testing.T) {
	t.Parallel()
	head := testHeadSHA
	s := newExecutionBoundStore(t, executionChainOptions{exportHead: &head})
	seedDecisionRecords(t, s)
	reservation := seedExecutionPublicationChain(
		t, s, executionChainOptions{exportHead: &head},
	)
	audit := executionWorkflowAudit(t)
	auditor := &fixedWorkflowAuditor{audit: audit}
	gh := newFakeGitHub(t)
	p := storeBackedPublisher(t, s, gh, auditor)
	candidate := testCandidate(t)
	candidate.RunID = reservation.RunID
	candidate.DispositionHistory = testDispositionHistory(t, s, candidate)
	execution := publish.ExecutionCandidate{
		Candidate: candidate, ProducingInvocationID: testProducingInvocationID,
	}
	interrupt := errors.New("interrupt after intent")
	if _, err := p.PublishExecutionAfterGateAndFinalize(
		t.Context(), execution, testApprovedRecipes(),
		func(context.Context, publish.GatedHead) error { return interrupt },
	); !errors.Is(err, interrupt) {
		t.Fatalf("first publication = %v, want interruption", err)
	}
	assertDecisionRows(t, s, 1, 1)
	key, err := publish.IntentKey(candidate.InvocationID, publish.IntentKindPublication)
	if err != nil {
		t.Fatal(err)
	}
	var entry store.QueueEntry
	if err := s.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(t.Context(), key)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	intent, err := publish.DecodeIntent(entry.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if intent.DispositionHistoryDigest == "" {
		t.Fatal("committed execution intent did not freeze the disposition history digest")
	}

	auditor.audit.AuditedCommitSHA = testOtherSHA
	result, err := p.PublishExecutionAfterGateAndFinalize(
		t.Context(), execution, testApprovedRecipes(),
		func(_ context.Context, gated publish.GatedHead) error {
			gh.mu.Lock()
			gh.refs[gated.Identity().BranchName()] = gated.SourceHeadSHA()
			gh.mu.Unlock()
			return nil
		},
	)
	if err != nil {
		t.Fatalf("committed-intent recovery after target advance: %v", err)
	}
	if !result.PRCreated || result.PRNumber == 0 {
		t.Fatalf("recovery result = %+v, want created PR", result)
	}
}

func TestPublishExecutionRevalidatesReadinessProofsOnCommittedRetry(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "store.db")
	head := testHeadSHA
	s := newExecutionBoundStoreAt(t, path, executionChainOptions{exportHead: &head})
	seedDecisionRecords(t, s)
	reservation := seedExecutionPublicationChain(
		t, s, executionChainOptions{exportHead: &head},
	)
	gh := newFakeGitHub(t)
	p := storeBackedPublisher(
		t, s, gh, fixedWorkflowAuditor{audit: executionWorkflowAudit(t)},
	)
	candidate := testCandidate(t)
	candidate.RunID = reservation.RunID
	candidate.DispositionHistory = testDispositionHistory(t, s, candidate)
	execution := publish.ExecutionCandidate{
		Candidate: candidate, ProducingInvocationID: testProducingInvocationID,
	}
	interrupt := errors.New("interrupt after intent")
	if _, err := p.PublishExecutionAfterGateAndFinalize(
		t.Context(), execution, testApprovedRecipes(),
		func(context.Context, publish.GatedHead) error { return interrupt },
	); !errors.Is(err, interrupt) {
		t.Fatalf("first publication = %v, want interruption", err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	result, deleteErr := raw.ExecContext(
		t.Context(),
		"DELETE FROM check_proofs WHERE rowid = (SELECT MIN(rowid) FROM check_proofs)",
	)
	closeErr := raw.Close()
	if deleteErr != nil || closeErr != nil {
		t.Fatal(errors.Join(deleteErr, closeErr))
	}
	if deleted, err := result.RowsAffected(); err != nil || deleted != 1 {
		t.Fatalf("readiness proof rows deleted = %d, %v, want 1", deleted, err)
	}
	callbackCalled := false
	if _, err := p.PublishExecutionAfterGateAndFinalize(
		t.Context(), execution, testApprovedRecipes(),
		func(context.Context, publish.GatedHead) error {
			callbackCalled = true
			return nil
		},
	); err == nil || !strings.Contains(err.Error(), "validate persisted readiness derivation") {
		t.Fatalf("committed retry after proof loss = %v", err)
	}
	if callbackCalled {
		t.Fatal("committed retry reached external effect after proof loss")
	}
	if requests := gh.requestLog(); len(requests) != 0 {
		t.Fatalf("committed retry after proof loss reached forge: %v", requests)
	}
}

func TestPublishExecutionRejectsModernIntentDowngrade(t *testing.T) {
	t.Parallel()
	testPublishExecutionRejectsModernIntentMutation(
		t,
		`UPDATE outbox
		 SET payload = CAST(json_set(
			json_remove(payload, '$.disposition_history_digest'),
			'$.format_version', 1
		 ) AS BLOB)
		 WHERE idempotency_key = ?`,
		"disagrees with outbox format",
	)
}

func TestPublishExecutionRejectsModernIntentHistoryRemoval(t *testing.T) {
	t.Parallel()
	testPublishExecutionRejectsModernIntentMutation(
		t,
		`UPDATE outbox
		 SET payload = CAST(json_remove(payload, '$.disposition_history_digest') AS BLOB)
		 WHERE idempotency_key = ?`,
		"already committed a different intent",
	)
}

func TestValidateIntentDispositionHistoryRejectsCurrentFormatWithoutDigest(t *testing.T) {
	t.Parallel()
	head := testHeadSHA
	s := newExecutionBoundStore(t, executionChainOptions{exportHead: &head})
	seedDecisionRecords(t, s)
	reservation := seedExecutionPublicationChain(
		t, s, executionChainOptions{exportHead: &head},
	)
	candidate := testCandidate(t)
	candidate.RunID = reservation.RunID
	candidate.DispositionHistory = testDispositionHistory(t, s, candidate)
	intent := publish.Intent{FormatVersion: publish.IntentFormatCurrent}
	if err := publish.ValidateIntentDispositionHistory(intent, candidate); !errors.Is(err, publish.ErrPublicationConflict) {
		t.Fatalf("current intent without disposition digest = %v, want ErrPublicationConflict", err)
	}
	intent.FormatVersion = publish.IntentFormatLegacy
	if err := publish.ValidateIntentDispositionHistory(intent, candidate); err != nil {
		t.Fatalf("legacy intent without disposition digest = %v, want compatibility", err)
	}
}

func testPublishExecutionRejectsModernIntentMutation(t *testing.T, mutation, want string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.db")
	head := testHeadSHA
	s := newExecutionBoundStoreAt(t, path, executionChainOptions{exportHead: &head})
	seedDecisionRecords(t, s)
	reservation := seedExecutionPublicationChain(
		t, s, executionChainOptions{exportHead: &head},
	)
	gh := newFakeGitHub(t)
	p := storeBackedPublisher(
		t, s, gh, fixedWorkflowAuditor{audit: executionWorkflowAudit(t)},
	)
	candidate := testCandidate(t)
	candidate.RunID = reservation.RunID
	candidate.DispositionHistory = testDispositionHistory(t, s, candidate)
	execution := publish.ExecutionCandidate{
		Candidate: candidate, ProducingInvocationID: testProducingInvocationID,
	}
	interrupt := errors.New("interrupt after intent")
	if _, err := p.PublishExecutionAfterGateAndFinalize(
		t.Context(), execution, testApprovedRecipes(),
		func(context.Context, publish.GatedHead) error { return interrupt },
	); !errors.Is(err, interrupt) {
		t.Fatalf("first publication = %v, want interruption", err)
	}
	key, err := publish.IntentKey(candidate.InvocationID, publish.IntentKindPublication)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	downgradeResult, downgradeErr := raw.ExecContext(
		t.Context(),
		mutation,
		key,
	)
	if downgradeErr != nil {
		_ = raw.Close()
		t.Fatal(downgradeErr)
	}
	if updated, err := downgradeResult.RowsAffected(); err != nil || updated != 1 {
		_ = raw.Close()
		t.Fatalf("publication intents downgraded = %d, %v, want 1", updated, err)
	}
	var downgradedPayload []byte
	if err := raw.QueryRowContext(
		t.Context(), "SELECT payload FROM outbox WHERE idempotency_key = ?", key,
	).Scan(&downgradedPayload); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	_, digestErr := raw.ExecContext(
		t.Context(), "UPDATE outbox SET payload_digest = ? WHERE idempotency_key = ?",
		contentaddr.Sum(downgradedPayload), key,
	)
	closeErr := raw.Close()
	if digestErr != nil || closeErr != nil {
		t.Fatal(errors.Join(digestErr, closeErr))
	}
	callbackCalled := false
	if _, err := p.PublishExecutionAfterGateAndFinalize(
		t.Context(), execution, testApprovedRecipes(),
		func(context.Context, publish.GatedHead) error {
			callbackCalled = true
			return nil
		},
	); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("mutated modern retry error = %v, want %q", err, want)
	}
	if callbackCalled {
		t.Fatal("mutated modern retry reached external callback")
	}
	if requests := gh.requestLog(); len(requests) != 0 {
		t.Fatalf("mutated modern retry reached forge: %v", requests)
	}
}

func TestPublishExecutionAuthenticatesFrozenAdmissionAndExport(t *testing.T) {
	t.Parallel()
	storePath := filepath.Join(t.TempDir(), "store.db")
	initial, err := store.Open(
		t.Context(),
		storePath,
		executionStoreOptions(),
	)
	if err != nil {
		t.Fatalf("open initial store: %v", err)
	}
	seedDecisionRecords(t, initial)
	head := testHeadSHA
	reservation := seedExecutionPublicationChain(
		t, initial, executionChainOptions{exportHead: &head},
	)
	if err := initial.Close(); err != nil {
		t.Fatalf("close initial store: %v", err)
	}

	currentOptions := executionStoreOptions()
	currentOptions.AdmissionFloors[domain.ModeUnattended] = domain.NewCapabilitySnapshot(domain.CapWorkspaceSnapshot)
	current, err := store.Open(
		t.Context(),
		storePath,
		currentOptions,
	)
	if err != nil {
		t.Fatalf("reopen store under raised floor: %v", err)
	}
	t.Cleanup(func() {
		if err := current.Close(); err != nil {
			t.Errorf("close current store: %v", err)
		}
	})
	if err := current.Read(t.Context(), func(tx *store.ReadTx) error {
		_, err := tx.GetExecutionAdmission(t.Context(), testProducingInvocationID)
		return err
	}); !errors.Is(err, domain.ErrCapabilityBelowFloor) {
		t.Fatalf("current admission read = %v, want raised-floor refusal", err)
	}

	gh := newFakeGitHub(t)
	p := storeBackedPublisher(
		t, current, gh, fixedWorkflowAuditor{audit: executionWorkflowAudit(t)},
	)
	candidate := testCandidate(t)
	candidate.RunID = reservation.RunID
	candidate.DispositionHistory = testDispositionHistory(t, current, candidate)
	if _, err := p.PublishExecution(
		t.Context(),
		publish.ExecutionCandidate{
			Candidate:             candidate,
			ProducingInvocationID: testProducingInvocationID,
		},
		testApprovedRecipes(),
	); err != nil {
		t.Fatalf("PublishExecution under raised current floor: %v", err)
	}
	assertDecisionRows(t, current, 1, 1)
}
