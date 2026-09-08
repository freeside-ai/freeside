package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/operations"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

type publicationBackupCase struct {
	name    string
	claim   bool
	entry   store.QueueEntry
	extract store.BackupPayloadDigestExtractor
	live    func(store.QueueEntry) error
	digests []domain.Digest
}

func historicalPublicationCases(t *testing.T, body string) []publicationBackupCase {
	t.Helper()
	publication := ProductionPublication{
		Title: "Preserve historical work", Body: body,
		CommitAuthor: ProductionCommitAuthor{AppSlug: "freeside", BotUserID: 42},
	}
	marshal := func(value any) []byte {
		t.Helper()
		payload, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return payload
	}
	request := productionInvocationRequest{
		Version: productionInvocationRequestVersion, RunID: "run-backup",
		InvocationID: productionInvocationID("run-backup"), StageID: productionStageID("run-backup"),
		Publication: publication,
	}
	productionPayload, err := canonicalProductionRequestPayload(request)
	if err != nil {
		t.Fatal(err)
	}
	specification := specificationRequest{
		Version: specificationRequestVersion, SpecificationRunID: "specification-backup",
		ImplementationRunID: "implementation-backup", ProjectID: "project-backup",
		InvocationID: specificationInvocationID("specification-backup", 1), Iteration: 1,
		InputArtifactIDs: []domain.ArtifactID{"source-backup"}, PolicyArtifactID: "policy-backup",
		Publication: publication,
	}
	task := validPublicationTask(t, "run-backup", "project-backup")
	task.Publication = publication
	task.Artifacts = []domain.Digest{digestProductionBytes([]byte("original result"))}
	return []publicationBackupCase{
		{
			name: "production invocation",
			entry: store.QueueEntry{
				IdempotencyKey: string(request.InvocationID),
				Kind:           KindProductionInvocationRequested, Payload: productionPayload,
			},
			extract: ProductionInvocationBackupPayloadDigests,
			live:    func(entry store.QueueEntry) error { _, err := decodeProductionRequest(entry); return err },
		},
		{
			name: "specification invocation",
			entry: store.QueueEntry{
				IdempotencyKey: string(specification.InvocationID),
				Kind:           KindSpecificationInvocationRequested, Payload: marshal(specification),
			},
			extract: SpecificationInvocationBackupPayloadDigests,
			live:    func(entry store.QueueEntry) error { _, err := decodeSpecificationRequest(entry); return err },
		},
		{
			name: "specification implementation claim", claim: true,
			entry: store.QueueEntry{
				IdempotencyKey: specificationImplementationClaimKey(specification.SpecificationRunID, specification.ImplementationRunID),
				Kind:           KindSpecificationImplementationClaim, Payload: marshal(specification), Status: "dispatched",
			},
			extract: SpecificationImplementationClaimBackupPayloadDigests,
			live:    func(entry store.QueueEntry) error { _, err := decodeSpecificationPayload(entry.Payload); return err },
		},
		{
			name: "publication task",
			entry: store.QueueEntry{
				IdempotencyKey: productionPublicationTaskKey(task.RunID),
				Kind:           KindProductionPublicationRequested, Payload: marshal(task),
			},
			extract: ProductionPublicationBackupPayloadDigests,
			live:    func(entry store.QueueEntry) error { _, err := decodeProductionPublicationTask(entry); return err },
			digests: append(productionReplayDigests(task.Replay), task.Artifacts...),
		},
	}
}

func TestHistoricalPublicationBackupDoesNotGrantLiveAuthority(t *testing.T) {
	for _, body := range []string{"## Verification\n\nRun the tests.\n", strings.Repeat("x", 30<<10)} {
		for _, tc := range historicalPublicationCases(t, body) {
			t.Run(tc.name, func(t *testing.T) {
				original := bytes.Clone(tc.entry.Payload)
				for _, status := range []string{"pending", "dispatched"} {
					tc.entry.Status = status
					if tc.claim && status == "pending" {
						if _, err := tc.extract(tc.entry); !errors.Is(err, domain.ErrParentKeyMismatch) {
							t.Fatalf("pending reservation = %v; want identity rejection", err)
						}
						continue
					}
					got, err := tc.extract(tc.entry)
					if err != nil || !slices.Equal(got, tc.digests) {
						t.Fatalf("%s retained digests = %v, %v; want %v", status, got, err, tc.digests)
					}
					if err := tc.live(tc.entry); err == nil {
						t.Fatalf("%s historical prose gained live authority", status)
					}
				}
				if !bytes.Equal(original, tc.entry.Payload) {
					t.Fatal("backup extraction changed historical bytes")
				}
				for _, mutate := range []func(*store.QueueEntry){
					func(entry *store.QueueEntry) { entry.IdempotencyKey = "foreign" },
					func(entry *store.QueueEntry) { entry.Kind = "foreign" },
					func(entry *store.QueueEntry) { entry.Payload = append(bytes.Clone(entry.Payload), []byte(" {}")...) },
					func(entry *store.QueueEntry) {
						entry.Payload = append([]byte(`{"unknown":true,`), entry.Payload[1:]...)
					},
				} {
					invalid := tc.entry
					mutate(&invalid)
					if _, err := tc.extract(invalid); err == nil {
						t.Fatal("backup extraction accepted a malformed or retargeted row")
					}
				}
			})
		}
	}
}

func TestHistoricalPublicationBackupKeepsMetadataValidation(t *testing.T) {
	for _, body := range []string{"", " \n", strings.Repeat("x", (64<<10)+1)} {
		for _, tc := range historicalPublicationCases(t, body) {
			if _, err := tc.extract(tc.entry); err == nil {
				t.Fatalf("%s accepted invalid retained metadata", tc.name)
			}
		}
	}
}

func TestHistoricalSpecificationCheckpointRoundTrip(t *testing.T) {
	ctx := t.Context()
	cases := historicalPublicationCases(t, "## Verification\n\nOld operator instructions.\n")
	entries := []store.QueueEntry{cases[1].entry, cases[2].entry}
	path := filepath.Join(t.TempDir(), "freeside.db")
	blobs, err := signet.NewBlobStore(path + ".blobs")
	if err != nil {
		t.Fatal(err)
	}
	files, err := store.NewDefaultLocalBackupFiles(path)
	if err != nil {
		t.Fatal(err)
	}
	source, err := files.NewCheckpointHealthSource(blobs, nil, map[string]store.BackupPayloadDigestExtractor{
		KindSpecificationInvocationRequested: SpecificationInvocationBackupPayloadDigests,
		KindSpecificationImplementationClaim: SpecificationImplementationClaimBackupPayloadDigests,
	})
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, path, store.Options{BackupHealthSource: source})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	configuration := digestProductionBytes([]byte("backup regression backend"))
	conformance, err := domain.NewBackendConformance(domain.BackendConformanceInput{
		Backend: domain.BackendFreshVMReadOnlyVolumeHandoff, Outcome: domain.ConformancePassed,
		ConfigurationDigest: configuration,
		Capabilities: domain.NewCapabilitySnapshot(
			domain.CapDetachableWorkspace, domain.CapPostExitExport, domain.CapReadOnlyRemount,
		),
		ProvedAt: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if _, err := tx.RecordBackendConformance(ctx, conformance); err != nil {
			return err
		}
		for _, entry := range entries {
			if _, _, err := tx.EnqueueOutbox(ctx, entry.IdempotencyKey, entry.Kind, entry.Payload); err != nil {
				return err
			}
			if err := tx.MarkOutboxDispatched(ctx, entry.IdempotencyKey); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	doctor := operations.Doctor{
		Store: db, Attention: signet.NewService(db), ProjectID: "project-backup",
		Backend: domain.BackendFreshVMReadOnlyVolumeHandoff, Mode: domain.ModeAttendedDev,
		ConfigurationDigest: configuration,
	}
	if report, err := doctor.Run(ctx); err != nil || report.Healthy {
		t.Fatalf("initial backup health = %+v, %v; want unhealthy", report, err)
	}
	gate := func() error {
		return db.Read(ctx, func(tx *store.ReadTx) error { return tx.RequireUnattendedOperationOpen(ctx) })
	}
	if err := gate(); !errors.Is(err, domain.ErrBlockingSystemHealth) {
		t.Fatalf("missing checkpoint admission = %v; want blocking health", err)
	}
	producer, err := files.NewProducer(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := producer.Maintain(ctx); err != nil {
		t.Fatalf("checkpoint of historical request: %v", err)
	}
	health, err := db.BackupHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if health.Encryption != domain.BackupHealthHealthy || health.CheckpointCurrency != domain.BackupHealthHealthy ||
		health.ArtifactClosure != domain.BackupHealthHealthy || health.RestoreTestAge != domain.BackupHealthHealthy {
		t.Fatalf("historical request blocked backup health: %+v", health)
	}
	if _, _, err := files.RestoreCheckpoint(ctx, db); err != nil {
		t.Fatalf("restore historical request: %v", err)
	}
	// Restore creates a fresh sync epoch, so produce its checkpoint before
	// asking doctor to clear the restored health items.
	if err := producer.Maintain(ctx); err != nil {
		t.Fatalf("checkpoint restored epoch: %v", err)
	}
	if report, err := doctor.Run(ctx); err != nil || !report.Healthy {
		t.Fatalf("restored backup health = %+v, %v; want healthy", report, err)
	}
	if err := gate(); err != nil {
		t.Fatalf("supported doctor refresh did not clear admission hold: %v", err)
	}
	if err := db.Read(ctx, func(tx *store.ReadTx) error {
		for _, entry := range entries {
			restored, err := tx.GetOutbox(ctx, entry.IdempotencyKey)
			if err != nil {
				return err
			}
			if !restored.Dispatched() || !bytes.Equal(restored.Payload, entry.Payload) {
				t.Fatal("restore changed historical payload or dispatch status")
			}
			if _, err := decodeSpecificationPayload(restored.Payload); err == nil {
				t.Fatal("restored historical request gained execution authority")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
