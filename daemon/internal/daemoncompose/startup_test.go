package daemoncompose

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/claude"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// testEvidenceTime is the fixed UTC creation time for the package's
// evidence-metadata fixtures.
var testEvidenceTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// testRunEvidenceMetadata is valid run-sourced evidence metadata.
func testRunEvidenceMetadata(sizeBytes int64) domain.EvidenceMetadata {
	return domain.EvidenceMetadata{
		MediaType:    domain.EvidenceMediaApplicationJSON,
		SizeBytes:    sizeBytes,
		CreatedAt:    testEvidenceTime,
		Source:       domain.EvidenceSourceRun,
		Availability: domain.EvidenceAvailable,
	}
}

// testClaimEvidenceMetadata is valid claim-sourced evidence metadata for
// the given media type.
func testClaimEvidenceMetadata(mediaType domain.EvidenceMediaType) domain.EvidenceMetadata {
	return domain.EvidenceMetadata{
		MediaType:    mediaType,
		SizeBytes:    1,
		CreatedAt:    testEvidenceTime,
		Source:       domain.EvidenceSourceClaim,
		Availability: domain.EvidenceAvailable,
	}
}

// testClaimTextEvidenceMetadata derives the media type and size from an inline
// text claim's content, so the fixture satisfies AgentClaim.Validate's
// text/metadata bindings by construction.
func testClaimTextEvidenceMetadata(text domain.ClaimText) domain.EvidenceMetadata {
	m := testClaimEvidenceMetadata(domain.EvidenceMediaType(text.MediaType))
	m.SizeBytes = int64(len(text.Content))
	return m
}

func TestEnabledShadowReviewRate(t *testing.T) {
	t.Parallel()
	if got := enabledShadowReviewRate(nil, 0.2); got != 0 {
		t.Fatalf("disabled shadow rate = %v, want 0", got)
	}
	if got := enabledShadowReviewRate(fake.NewReviewSource(), 0.2); got != 0.2 {
		t.Fatalf("enabled shadow rate = %v, want 0.2", got)
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestHeldClaudeRecovery(t *testing.T) {
	t.Parallel()
	for _, held := range []error{
		fmt.Errorf("journal unavailable: %w", claude.ErrRecoveryRetryable),
		fmt.Errorf("encrypted checkpoint pending: %w", domain.ErrCheckpointNotEncrypted),
		fmt.Errorf("checkpoint drift: %w", domain.ErrCheckpointNotCurrent),
		fmt.Errorf("conformance drift: %w", domain.ErrAdmissionConfigurationMismatch),
	} {
		if !heldClaudeRecovery(held) {
			t.Fatalf("held recovery would be quarantined: %v", held)
		}
	}
	permanent := errors.New("intent authentication failed")
	if heldClaudeRecovery(permanent) {
		t.Fatalf("permanent recovery error %v would be held, want quarantined", permanent)
	}
}

func TestScheduledDoctorPassRefreshesConformanceBeforeReporting(t *testing.T) {
	t.Parallel()
	t.Run("reports failures", func(t *testing.T) {
		var calls []string
		conformanceErr := errors.New("conformance failed")
		doctorErr := errors.New("doctor failed")
		err := runScheduledDoctorPass(
			context.Background(),
			func(context.Context) error {
				calls = append(calls, "conformance")
				return conformanceErr
			},
			func(context.Context) error {
				calls = append(calls, "doctor")
				return doctorErr
			},
		)
		if !slices.Equal(calls, []string{"conformance", "doctor"}) {
			t.Fatalf("calls = %v, want conformance then doctor", calls)
		}
		if !errors.Is(err, conformanceErr) || !errors.Is(err, doctorErr) {
			t.Fatalf("error = %v, want both conformance and doctor failures", err)
		}
	})
	t.Run("cancellation is shutdown", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		err := runScheduledDoctorPass(
			ctx,
			func(context.Context) error {
				cancel()
				return context.Canceled
			},
			func(context.Context) error {
				return context.Canceled
			},
		)
		if err != nil {
			t.Fatalf("canceled pass = %v, want graceful shutdown", err)
		}
	})
}

func TestStoreConformanceRecorderPersistsConfigurationBoundPass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	record, err := domain.NewBackendConformance(domain.BackendConformanceInput{
		Backend: domain.BackendFreshVMReadOnlyVolumeHandoff,
		Outcome: domain.ConformancePassed,
		Capabilities: domain.NewCapabilitySnapshot(
			exec.CapDetachableWorkspace,
			exec.CapPostExitExport,
			exec.CapReadOnlyRemount,
			exec.CapNetworklessExport,
			exec.CapEnforcedProviderEgress,
		),
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("ab", 32)),
		ProvedAt:            time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("NewBackendConformance: %v", err)
	}
	if err := (StoreConformanceRecorder{Store: st}).RecordBackendConformance(ctx, record); err != nil {
		t.Fatalf("RecordBackendConformance: %v", err)
	}

	var (
		got   domain.BackendConformance
		found bool
	)
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		got, found, err = tx.LatestBackendConformance(
			ctx, domain.BackendFreshVMReadOnlyVolumeHandoff,
		)
		return err
	}); err != nil {
		t.Fatalf("LatestBackendConformance: %v", err)
	}
	if !found || got.Generation == 0 || got.ConfigurationDigest != record.ConfigurationDigest ||
		got.Outcome != domain.ConformancePassed {
		t.Fatalf("stored conformance = (%+v, found=%v), want configuration-bound pass", got, found)
	}
}

type fakeConformanceRestorer struct {
	digest   domain.Digest
	restored int
}

func (f *fakeConformanceRestorer) ConfigurationDigest() domain.Digest { return f.digest }

func (f *fakeConformanceRestorer) RestoreConformance(domain.BackendConformance) error {
	f.restored++
	return nil
}

func TestUnattendedStartupRequiresExactPassingConformance(t *testing.T) {
	t.Parallel()
	backend := &fakeConformanceRestorer{digest: "sha256:active"}
	passing := domain.BackendConformance{
		Outcome: domain.ConformancePassed, ConfigurationDigest: backend.digest,
	}
	for name, tc := range map[string]struct {
		record domain.BackendConformance
		found  bool
	}{
		"missing":    {},
		"failed":     {record: domain.BackendConformance{Outcome: domain.ConformanceFailed, ConfigurationDigest: backend.digest}, found: true},
		"mismatched": {record: domain.BackendConformance{Outcome: domain.ConformancePassed, ConfigurationDigest: "sha256:other"}, found: true},
	} {
		t.Run(name, func(t *testing.T) {
			if err := restoreClaudeBackendConformance(backend, tc.record, tc.found, true); !errors.Is(err, errBackendConformanceUnavailable) {
				t.Fatalf("restore error = %v, want %v", err, errBackendConformanceUnavailable)
			}
		})
	}
	if err := restoreClaudeBackendConformance(backend, domain.BackendConformance{}, false, false); err != nil {
		t.Fatalf("attended startup without proof: %v", err)
	}
	if err := restoreClaudeBackendConformance(backend, passing, true, true); err != nil {
		t.Fatalf("restore exact passing proof: %v", err)
	}
	if backend.restored != 1 {
		t.Fatalf("restore calls = %d, want 1", backend.restored)
	}
}

func TestProductionInputSourceClassifiesOperationalErrorsOnly(t *testing.T) {
	t.Parallel()
	blobs, err := signet.NewBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := productionInputSource{blobs: blobs}
	digest := domain.Digest("sha256:" + strings.Repeat("a", 64))

	if _, err := source.OpenContext(t.Context(), digest); !errors.Is(err, signet.ErrBlobNotFound) ||
		errors.Is(err, exec.ErrInputUnavailable) {
		t.Fatalf("missing input = %v, want permanent blob-not-found only", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := source.OpenContext(ctx, digest); !errors.Is(err, exec.ErrInputUnavailable) {
		t.Fatalf("canceled input open = %v, want retryable input class", err)
	}

	readFailure := retryableInputReadCloser{ReadCloser: failingInputReadCloser{}}
	if _, err := readFailure.Read(make([]byte, 1)); !errors.Is(err, exec.ErrInputUnavailable) {
		t.Fatalf("input read = %v, want retryable input class", err)
	}
	if err := readFailure.Close(); !errors.Is(err, exec.ErrInputUnavailable) {
		t.Fatalf("input close = %v, want retryable input class", err)
	}
}

func TestClassifyTransportSeedError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		err       error
		retryable bool
		refused   bool
	}{
		{"missing base", publish.ErrRemoteMissingBase, false, true},
		{"materialization refusal", publish.ErrMaterializationRefused, false, true},
		{"installation token expiry refusal", publish.ErrTokenExpiry, false, true},
		{"git auth", &publish.TransportGitError{Refusal: publish.RefusalAuth}, false, true},
		{"API unauthorized", &publish.APIError{Status: http.StatusUnauthorized}, false, true},
		{"API forbidden", &publish.APIError{Status: http.StatusForbidden}, false, true},
		{"API missing", &publish.APIError{Status: http.StatusNotFound}, false, true},
		{"API rate limit", &publish.APIError{Status: http.StatusTooManyRequests}, true, false},
		{"API server failure", &publish.APIError{Status: http.StatusInternalServerError}, true, false},
		{"ambiguous transport failure", errors.New("temporary network failure"), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyTransportSeedError(tc.err)
			if got := errors.Is(err, claude.ErrSeedRetryable); got != tc.retryable {
				t.Fatalf("classifyTransportSeedError(%v) retryable = %v, want %v",
					tc.err, got, tc.retryable)
			}
			if got := errors.Is(err, claude.ErrSeedRefused); got != tc.refused {
				t.Fatalf("classifyTransportSeedError(%v) refused = %v, want %v",
					tc.err, got, tc.refused)
			}
			if !errors.Is(err, tc.err) {
				t.Fatalf("classified error %v does not retain %v", err, tc.err)
			}
		})
	}
	for _, permanent := range []error{
		publish.ErrNoAppCredentials,
		publish.ErrCredentialPermissions,
		publish.ErrNoInstallation,
		publish.ErrInstallationGrantUntrusted,
		publish.ErrGrantMismatch,
	} {
		if err := classifyTransportSeedError(permanent); !errors.Is(err, claude.ErrSeedRefused) {
			t.Errorf("credential error %v classified as %v, want definitive refusal",
				permanent, err)
		}
	}
}

type failingInputReadCloser struct{}

func (failingInputReadCloser) Read([]byte) (int, error) {
	return 0, errors.New("fixture read failure")
}

func (failingInputReadCloser) Close() error {
	return errors.New("fixture close failure")
}

type stubJanitorRunner struct {
	active         bool
	passes         int
	runErr         error
	stableCoverage int
	faults         []publish.JanitorRegistrationFault
	churn          []publish.JanitorRegistrationChurn
	incomplete     []publish.JanitorRegistrationIncomplete
}

func (j *stubJanitorRunner) RunScheduledPass(context.Context) error {
	if j.runErr != nil {
		return j.runErr
	}
	j.passes++
	j.active = true
	return nil
}

func (j *stubJanitorRunner) ActiveFor(int64) bool { return j.active }

func (j *stubJanitorRunner) WithStableCoverage(fn func() error) error {
	j.stableCoverage++
	return fn()
}

func (j *stubJanitorRunner) RegistrationFaults() []publish.JanitorRegistrationFault {
	return j.faults
}

func (j *stubJanitorRunner) ChurningRegistrations() []publish.JanitorRegistrationChurn {
	return j.churn
}

func (j *stubJanitorRunner) IncompleteRegistrations() []publish.JanitorRegistrationIncomplete {
	return j.incomplete
}

func (j *stubJanitorRunner) PendingReady(publish.PendingInstallationEnvelope) (int64, bool) {
	return 0, false
}

func TestJanitorSessionPrimesCoverageSynchronously(t *testing.T) {
	t.Parallel()
	janitor := &stubJanitorRunner{}
	session, err := startJanitorSession(t.Context(), janitor, []int64{4385298})
	if err != nil {
		t.Fatalf("startJanitorSession: %v", err)
	}
	if janitor.passes != 1 || !janitor.ActiveFor(4385298) {
		t.Fatalf("startup pass = %d, active = %v", janitor.passes, janitor.active)
	}
	if err := session.WithStableCoverage(func() error { return nil }); err != nil {
		t.Fatalf("WithStableCoverage: %v", err)
	}
	if janitor.stableCoverage != 1 {
		t.Fatalf("stable coverage calls = %d, want 1", janitor.stableCoverage)
	}
	// The scheduler-fired pass reaches the same lifecycle.
	if err := session.RunScheduledPass(t.Context()); err != nil {
		t.Fatalf("RunScheduledPass: %v", err)
	}
	if janitor.passes != 2 {
		t.Fatalf("passes = %d, want 2", janitor.passes)
	}
	if err := session.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestJanitorSessionReturnsStartupFailure(t *testing.T) {
	t.Parallel()
	want := errors.New("janitor failed")
	_, err := startJanitorSession(
		t.Context(), &stubJanitorRunner{runErr: want}, []int64{4385298},
	)
	if !errors.Is(err, want) {
		t.Fatalf("startJanitorSession error = %v, want %v", err, want)
	}
}

func TestJanitorSessionReportsMissingCoverageWithFaults(t *testing.T) {
	t.Parallel()
	fault := errors.New("registration denied")
	janitor := &stubJanitorRunner{
		faults: []publish.JanitorRegistrationFault{{RegistrationID: 4385298, Err: fault}},
	}
	// The pass succeeds but publishes no coverage for the registration: the
	// startup gate fails with the pass's fault attached.
	janitor.runErr = nil
	_, err := startJanitorSession(t.Context(), &faultyCoverageRunner{janitor}, []int64{4385298})
	if err == nil || !errors.Is(err, fault) {
		t.Fatalf("startJanitorSession error = %v, want fault %v", err, fault)
	}
}

func TestJanitorSessionReportsEveryMissingCoverageCause(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		janitor *stubJanitorRunner
		want    string
	}{
		{
			name: "churn",
			janitor: &stubJanitorRunner{churn: []publish.JanitorRegistrationChurn{{
				RegistrationID: 4385298, ConsecutivePasses: 2,
			}}},
			want: "removal churn for 2 consecutive passes",
		},
		{
			name: "incomplete",
			janitor: &stubJanitorRunner{incomplete: []publish.JanitorRegistrationIncomplete{{
				RegistrationID: 4385298,
			}}},
			want: "reconciliation incomplete: removal budget",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := startJanitorSession(
				t.Context(), &faultyCoverageRunner{test.janitor}, []int64{4385298},
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("startJanitorSession error = %v, want %q", err, test.want)
			}
		})
	}
}

// faultyCoverageRunner completes its pass without activating coverage,
// modeling a per-registration fault.
type faultyCoverageRunner struct{ *stubJanitorRunner }

func (j *faultyCoverageRunner) RunScheduledPass(context.Context) error { return nil }

func (j *faultyCoverageRunner) ActiveFor(int64) bool { return false }
