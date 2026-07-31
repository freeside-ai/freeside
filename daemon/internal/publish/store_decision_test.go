package publish_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	s, err := store.Open(
		t.Context(),
		filepath.Join(t.TempDir(), "store.db"),
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
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
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

func TestPublishExecutionHoldsSourceHeadAgainstRecordedExport(t *testing.T) {
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

func TestPublishExecutionRecoversCommittedIntentAfterTargetAdvances(t *testing.T) {
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

func TestPublishExecutionAuthenticatesFrozenAdmissionAndExport(t *testing.T) {
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
