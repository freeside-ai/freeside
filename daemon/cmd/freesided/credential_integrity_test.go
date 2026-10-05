package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/operations"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
	"github.com/freeside-ai/freeside/daemon/internal/wardstore"
)

const (
	// integritySentinel is a planted credential value. It sits in both Codex
	// stores the pass reads, and nothing the pass produces may carry it.
	integritySentinel = "INTEGRITY-SENTINEL-credential-0123456789abcdefghijklmnop"
	// integrityAccountBinding ends every fixture identity's account binding,
	// which a report, an item, or a log line must show only as its masked
	// label.
	integrityAccountBinding = "operator@example.test"
)

var integrityEnrolledAt = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

// enrollIntegrityFixture records one enrollment at generation one.
func enrollIntegrityFixture(
	t *testing.T, adapters *wardstore.Adapters,
	id domain.AuthIdentityID, client domain.HarnessClientKind, locator string, recorded domain.Digest,
) domain.ClientEnrollmentID {
	t.Helper()
	ctx := t.Context()
	enrollmentID := domain.ClientEnrollmentID(string(id) + "/" + string(client))
	// Bindings are unique per identity.
	binding := string(id) + "-" + integrityAccountBinding
	identity := domain.AuthIdentity{
		ID: id, Provider: "claude", AccountBinding: binding, AuthStoreMutationLease: true,
		MaxParallelExecutions: 1, Enabled: true, CostOwner: "operator",
		Interim: domain.InterimClientFacts{
			AuthStoreVolume: locator, RefreshStrategy: domain.RefreshOnDemand, SupportsReadOnlyAuthSnapshot: true,
		},
	}
	enrollment := domain.ClientEnrollment{
		ID: enrollmentID, AuthIdentityID: id, HarnessClient: client,
		Route: "anthropic-subscription", AuthMethod: domain.AuthMethodSetupToken,
		CredentialMode:  domain.CredentialSubscriptionContained,
		RefreshStrategy: domain.RefreshExternal, SupportsReadOnlyAuthSnapshot: true,
		AccountBinding: binding,
	}
	var expiry *time.Time
	if client == domain.HarnessClientCodexCLI {
		identity.Provider = "openai"
		enrollment.Route = "openai-subscription"
		enrollment.AuthMethod = domain.AuthMethodOAuth
		enrollment.RefreshStrategy = domain.RefreshOnDemand
		at := integrityEnrolledAt.Add(24 * time.Hour)
		expiry = &at
	}
	holder := domain.InvocationID("enroll-" + string(id))
	lease, err := adapters.Claude.Begin(ctx, identity, ward.EnrollmentBootstrap{
		Enrollment: enrollment,
		Binding: domain.LeaseGenerationBinding{
			EnrollmentID: enrollmentID, AuthStoreVolume: locator, StoreManifestDigest: recorded,
		},
	}, holder, integrityEnrolledAt, integrityEnrolledAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapters.Claude.AppendGeneration(ctx, domain.EnrollmentGeneration{
		EnrollmentID: enrollmentID, AuthStoreVolume: locator, StoreManifestDigest: recorded,
		LeaseFence: lease.Fence, AccountBinding: binding,
		TokenExpiry: expiry, RecordedAt: integrityEnrolledAt,
	}, integrityEnrolledAt); err != nil {
		t.Fatal(err)
	}
	if err := adapters.Leaser.Release(ctx, id, holder, lease.Fence, integrityEnrolledAt); err != nil {
		t.Fatal(err)
	}
	return enrollmentID
}

// absentVolumeRuntime is a runtime with no volumes. Any call past the volume
// list panics on the nil embedded runtime, so the test also proves an absent
// store starts no observer.
type absentVolumeRuntime struct{ ward.Runtime }

func (absentVolumeRuntime) ListVolumes(context.Context) ([]ward.VolumeSummary, error) {
	return nil, nil
}

// TestDoctorCommandProbesStoredCredentialsWithoutLeakingThem drives a pass
// end to end: the doctor command, borrowing a running daemon's store and
// probe, observes a damaged setup-token store, a cut-off Codex store, a
// Codex store it must refuse to read, and an absent volume. The planted
// credential value reaches none of the report a client receives, the
// attention items, the logs, or the database. A later pass with no daemon
// reports the recorded marks and observes nothing.
func TestDoctorCommandProbesStoredCredentialsWithoutLeakingThem(t *testing.T) {
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	// The store opens the way the daemon opens it, over an initialized
	// backup file set it can lend to a borrowing command.
	blobs, err := signet.NewBlobStore(dbPath + ".blobs")
	if err != nil {
		t.Fatal(err)
	}
	backupFiles, err := store.NewDefaultLocalBackupFiles(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	backupHealth, err := backupFiles.NewCheckpointHealthSource(blobs, nil, backupPayloadExtractors())
	if err != nil {
		t.Fatal(err)
	}
	st, _, err := openStoreWithTopicKey(ctx, dbPath, store.Options{BackupHealthSource: backupHealth})
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = st.Close()
		}
	})
	adapters, err := wardstore.New(st)
	if err != nil {
		t.Fatal(err)
	}

	// The Codex stores are real files under a private root, read by the
	// production observation.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil { //nolint:gosec // G302: owner traversal is required for the private store root.
		t.Fatal(err)
	}
	cutOff := filepath.Join(root, "cut-off.json")
	if err := os.WriteFile(cutOff, []byte(`{"tokens":{"access_token":"`+integritySentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	exposed := filepath.Join(root, "exposed.json")
	if err := os.WriteFile(exposed, []byte(`{"tokens":{"access_token":"`+integritySentinel+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(exposed, 0o640); err != nil { //nolint:gosec // G302: the loosened mode is what the reader must refuse.
		t.Fatal(err)
	}

	recorded := domain.Digest(contentaddr.Sum([]byte("recorded at enrollment")))
	damaged := enrollIntegrityFixture(t, adapters, "claude-damaged", domain.HarnessClientClaudeCode, "claude-damaged-auth", recorded)
	absent := enrollIntegrityFixture(t, adapters, "claude-absent", domain.HarnessClientClaudeCode, "claude-absent-auth", recorded)
	codexCutOff := enrollIntegrityFixture(t, adapters, "codex-cut-off", domain.HarnessClientCodexCLI, cutOff, recorded)
	codexExposed := enrollIntegrityFixture(t, adapters, "codex-exposed", domain.HarnessClientCodexCLI, exposed, recorded)

	observers := productionCredentialIntegrityObservers(claudeDriverConfig{
		ExporterImage:   "registry.test/exporter@sha256:" + strings.Repeat("0", 64),
		ReviewInputRoot: root,
	}, absentVolumeRuntime{})
	production := observers.setupToken
	observers.setupToken = func(ctx context.Context, volume string) (ward.SetupTokenIntegrity, error) {
		if volume != "claude-damaged-auth" {
			return production(ctx, volume)
		}
		// The observation's own tests cover what crosses the container
		// boundary. Here it reports a short token whose digests match
		// neither convention.
		return ward.SetupTokenIntegrity{
			TokenDigest: domain.Digest(contentaddr.Sum([]byte("other bytes"))),
			TreeDigest:  domain.Digest(contentaddr.Sum([]byte("other tree"))),
			Truncated:   true,
		}, nil
	}
	var logs bytes.Buffer
	now := func() time.Time { return integrityEnrolledAt.Add(time.Hour) }
	probe := newCredentialIntegrityProbe(st, observers, now, slog.New(slog.NewTextHandler(&logs, nil)))

	canonical, err := canonicalDatabasePath(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	borrowed := context.WithValue(ctx, daemonStoreContextKey{}, daemonStoreContext{
		store: st, blobs: blobs, backupFiles: backupFiles, dbPath: canonical,
		credentialIntegrityProbe: probe,
	})
	args := []string{"-db", dbPath, "-backend-configuration-digest", "sha256:" + strings.Repeat("a", 64)}

	var response bytes.Buffer
	commandErr := runDoctorCommand(borrowed, args, &response, io.Discard)
	if commandErr == nil || !strings.Contains(commandErr.Error(), "unhealthy") {
		t.Fatalf("doctor = %v, want an unhealthy report", commandErr)
	}
	finding := integrityFinding(t, response.Bytes())
	if finding.Healthy {
		t.Fatalf("finding = %+v, want unhealthy", finding)
	}
	for _, want := range []string{
		"probed 4 enrollments",
		string(damaged) + " generation 1 (corruption, truncation)",
		string(codexCutOff) + " generation 1 (truncation)",
		string(absent) + " (store_absent)",
		string(codexExposed) + " (observation_failed)",
		"corruption check not run on a store the daemon refreshes: " + string(codexCutOff),
	} {
		if !strings.Contains(finding.Detail, want) {
			t.Errorf("detail %q does not carry %q", finding.Detail, want)
		}
	}

	// The unobservable stores carry no mark; the damaged ones carry theirs.
	wantMarks := map[domain.ClientEnrollmentID]int{damaged: 2, codexCutOff: 1, absent: 0, codexExposed: 0}
	var markRows []domain.GenerationIntegrityMark
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		for id, want := range wantMarks {
			marks, err := tx.GenerationIntegrityMarks(ctx, id, 1)
			if err != nil {
				return err
			}
			if len(marks) != want {
				t.Errorf("%s carries %d marks, want %d", id, len(marks), want)
			}
			markRows = append(markRows, marks...)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	snapshots, err := signet.NewService(st).ListAttentionItems(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var integrityItems []domain.AttentionItem
	for _, snapshot := range snapshots {
		item := snapshot.Item
		if item.HealthDiagnostic != nil && item.HealthDiagnostic.Code == "credential_integrity" {
			integrityItems = append(integrityItems, item)
			if item.Posture == nil || *item.Posture != domain.HealthPostureAdvisory {
				t.Errorf("item %s posture = %v, want advisory", item.ID, item.Posture)
			}
			if !strings.Contains(item.Reason, "("+maskAccountBinding(integrityAccountBinding)+")") {
				t.Errorf("item reason %q does not name the identity by its masked label", item.Reason)
			}
		}
	}
	if len(integrityItems) != 2 {
		t.Errorf("credential integrity items = %d, want one per marked generation", len(integrityItems))
	}
	if !strings.Contains(logs.String(), string(codexExposed)) || !strings.Contains(logs.String(), "observation_failed") {
		t.Errorf("logs %q do not report the store that could not be read", logs.String())
	}

	itemJSON, err := json.Marshal(integrityItems)
	if err != nil {
		t.Fatal(err)
	}
	markJSON, err := json.Marshal(markRows)
	if err != nil {
		t.Fatal(err)
	}
	surfaces := map[string][]byte{
		"doctor response": response.Bytes(),
		"command error":   []byte(commandErr.Error()),
		"items":           itemJSON,
		"marks":           markJSON,
		"logs":            logs.Bytes(),
	}
	for name, content := range surfaces {
		if bytes.Contains(content, []byte(integritySentinel)) {
			t.Errorf("the credential value reached the %s", name)
		}
		if bytes.Contains(content, []byte(integrityAccountBinding)) {
			t.Errorf("the account binding reached the %s", name)
		}
	}

	// The probe wrote nothing to a store it read.
	if body, err := os.ReadFile(cutOff); err != nil || !strings.HasSuffix(string(body), integritySentinel) {
		t.Errorf("the cut-off store changed: %v", err)
	}

	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	// Every row the pass wrote is in these files, whatever table holds it.
	for _, name := range []string{dbPath, dbPath + "-wal"} {
		content, err := os.ReadFile(name) //nolint:gosec // G304: test-owned database path
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(content, []byte(integritySentinel)) {
			t.Errorf("the credential value reached %s", filepath.Base(name))
		}
	}

	// With no daemon the command opens the store itself and has no probe.
	logs.Reset()
	var offline bytes.Buffer
	if err := runDoctorCommand(ctx, args, &offline, io.Discard); err == nil || !strings.Contains(err.Error(), "unhealthy") {
		t.Fatalf("offline doctor = %v, want an unhealthy report", err)
	}
	recordedOnly := integrityFinding(t, offline.Bytes())
	if recordedOnly.Healthy || !strings.Contains(recordedOnly.Detail, "recorded marks only") ||
		!strings.Contains(recordedOnly.Detail, string(damaged)+" generation 1 (corruption, truncation)") {
		t.Errorf("offline finding = %+v, want the recorded marks and no live probe", recordedOnly)
	}
	if strings.Contains(recordedOnly.Detail, "not checked") || logs.Len() != 0 {
		t.Errorf("an offline pass observed stores: detail %q, logs %q", recordedOnly.Detail, logs.String())
	}
}

func integrityFinding(t *testing.T, report []byte) operations.DoctorFinding {
	t.Helper()
	var decoded operations.DoctorReport
	if err := json.Unmarshal(report, &decoded); err != nil {
		t.Fatalf("doctor report %q: %v", report, err)
	}
	for _, finding := range decoded.Findings {
		if finding.Code == "credential_integrity" {
			return finding
		}
	}
	t.Fatalf("doctor report has no credential_integrity finding: %s", report)
	return operations.DoctorFinding{}
}

// TestCredentialIntegrityProbeRunOnce: each call observes once and hands back
// a replay of that result, so a later pass never reuses an earlier pass's
// observation, and no probe stays no probe.
func TestCredentialIntegrityProbeRunOnce(t *testing.T) {
	if replay := credentialIntegrityProbe(nil).runOnce(t.Context()); replay != nil {
		t.Error("a nil probe produced a replay, so a pass with no probe would claim it probed")
	}
	calls := 0
	failure := errors.New("probe failed")
	probe := credentialIntegrityProbe(func(context.Context) ([]operations.CredentialIntegrityOutcome, error) {
		calls++
		if calls == 2 {
			return nil, failure
		}
		return []operations.CredentialIntegrityOutcome{{EnrollmentID: "claude-main/claude_code"}}, nil
	})
	first := probe.runOnce(t.Context())
	if calls != 1 {
		t.Fatalf("runOnce observed %d times, want once before it returns", calls)
	}
	for range 2 {
		outcomes, err := first(t.Context())
		if err != nil || len(outcomes) != 1 || calls != 1 {
			t.Fatalf("replay = %+v, %v after %d observations, want the first result and no new observation", outcomes, err, calls)
		}
	}
	second := probe.runOnce(t.Context())
	if _, err := second(t.Context()); !errors.Is(err, failure) || calls != 2 {
		t.Errorf("second pass = %v after %d observations, want a fresh observation's own error", err, calls)
	}
}
