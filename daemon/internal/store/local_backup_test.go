package store_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

type backupArtifactSet map[domain.Digest]bool

func (s backupArtifactSet) Verify(digest domain.Digest) (bool, error) {
	return s[digest], nil
}

func TestLocalCheckpointHealthEvaluatesEveryDimension(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "freeside.db")
	checkpointPath := filepath.Join(root, "checkpoint.db")
	restoreTestPath := filepath.Join(root, "restore-test.db")
	f := newFixtures(t)
	now := time.Date(2026, 7, 27, 4, 0, 0, 0, time.UTC)
	artifacts := backupArtifactSet{f.artifact.Digest: true}
	source, err := store.NewLocalCheckpointHealthSource(store.LocalCheckpointHealthOptions{
		CheckpointPath: checkpointPath, RestoreTestPath: restoreTestPath,
		Artifacts: artifacts, ApprovedRecipes: approvedFixtureRecipes(),
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewLocalCheckpointHealthSource: %v", err)
	}
	s := openStoreAt(t, dbPath, store.Options{
		ApprovedRecipes:    approvedFixtureRecipes(),
		BackupHealthSource: source,
	})
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutArtifact(ctx, f.artifact)
	}); err != nil {
		t.Fatalf("seed artifact: %v", err)
	}
	if err := s.Checkpoint(ctx, checkpointPath); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	writeCheckpointGeneratedAt(t, checkpointPath, now)

	restoreTest, err := store.Open(ctx, restoreTestPath, store.Options{})
	if err != nil {
		t.Fatalf("open restore test: %v", err)
	}
	if _, err := restoreTest.Restore(ctx, checkpointPath); err != nil {
		_ = restoreTest.Close()
		t.Fatalf("restore test: %v", err)
	}
	if err := restoreTest.Close(); err != nil {
		t.Fatalf("close restore test: %v", err)
	}
	checkpointBytes, err := os.ReadFile(checkpointPath) //nolint:gosec // test-owned checkpoint path
	if err != nil {
		t.Fatalf("read checkpoint for restore marker: %v", err)
	}
	checkpointDigest := (domain.ClaimText{Content: string(checkpointBytes)}).ComputeDigest()
	restoreDB, err := sql.Open("sqlite", restoreTestPath)
	if err != nil {
		t.Fatalf("open restore marker database: %v", err)
	}
	if _, err := restoreDB.Exec(
		`INSERT INTO local_backup_restore_marker
		    (id, checkpoint_digest, restored_at) VALUES (1, ?, ?)`,
		checkpointDigest, now.Format(time.RFC3339Nano)); err != nil {
		_ = restoreDB.Close()
		t.Fatalf("write restore marker: %v", err)
	}
	if err := restoreDB.Close(); err != nil {
		t.Fatalf("close restore marker database: %v", err)
	}
	if err := os.Chmod(restoreTestPath, 0o600); err != nil {
		t.Fatalf("restrict restore test: %v", err)
	}
	for _, path := range []string{checkpointPath, restoreTestPath} {
		if err := os.Chtimes(path, now, now); err != nil {
			t.Fatalf("set %s observation time: %v", filepath.Base(path), err)
		}
	}

	health, err := s.BackupHealth(ctx)
	if err != nil {
		t.Fatalf("BackupHealth: %v", err)
	}
	wantLegacy := healthyBackupHealth()
	wantLegacy.Encryption = domain.BackupHealthUnhealthy
	if health != wantLegacy {
		t.Fatalf("complete legacy checkpoint = %+v, want %+v", health, wantLegacy)
	}
	restoreDB, err = sql.Open("sqlite", restoreTestPath)
	if err != nil {
		t.Fatalf("reopen restore marker database: %v", err)
	}
	if _, err := restoreDB.Exec(`DELETE FROM local_backup_restore_marker`); err != nil {
		_ = restoreDB.Close()
		t.Fatalf("delete restore marker: %v", err)
	}
	if err := restoreDB.Close(); err != nil {
		t.Fatalf("close markerless restore database: %v", err)
	}
	health, err = s.BackupHealth(ctx)
	if err != nil {
		t.Fatalf("BackupHealth without restore marker: %v", err)
	}
	if health.RestoreTestAge != domain.BackupHealthUnhealthy {
		t.Fatalf("markerless restore-test age = %q, want unhealthy", health.RestoreTestAge)
	}
	restoreDB, err = sql.Open("sqlite", restoreTestPath)
	if err != nil {
		t.Fatalf("reopen restore marker database for repair: %v", err)
	}
	if _, err := restoreDB.Exec(
		`INSERT INTO local_backup_restore_marker
		    (id, checkpoint_digest, restored_at) VALUES (1, ?, ?)`,
		checkpointDigest, now.Format(time.RFC3339Nano)); err != nil {
		_ = restoreDB.Close()
		t.Fatalf("repair restore marker: %v", err)
	}
	if err := restoreDB.Close(); err != nil {
		t.Fatalf("close repaired restore marker database: %v", err)
	}
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, newItem(t, "post-checkpoint", nil, 1))
	}); err != nil {
		t.Fatalf("advance live store: %v", err)
	}
	health, err = s.BackupHealth(ctx)
	if err != nil {
		t.Fatalf("BackupHealth after live advance: %v", err)
	}
	if health.CheckpointCurrency != domain.BackupHealthHealthy {
		t.Fatalf("checkpoint currency after live advance = %q, want healthy", health.CheckpointCurrency)
	}

	if err := os.Chmod(checkpointPath, 0o644); err != nil { //nolint:gosec // deliberately widened adversarial fixture
		t.Fatalf("widen checkpoint permissions: %v", err)
	}
	health, err = s.BackupHealth(ctx)
	if err != nil {
		t.Fatalf("BackupHealth with widened checkpoint permissions: %v", err)
	}
	if health.CheckpointCurrency != domain.BackupHealthUnhealthy ||
		health.ArtifactClosure != domain.BackupHealthUnhealthy {
		t.Fatalf("widened checkpoint health = %+v, want currency and closure unhealthy", health)
	}
	if err := os.Chmod(checkpointPath, 0o600); err != nil {
		t.Fatalf("restore checkpoint permissions: %v", err)
	}

	artifacts[f.artifact.Digest] = false
	health, err = s.BackupHealth(ctx)
	if err != nil {
		t.Fatalf("BackupHealth with missing artifact: %v", err)
	}
	if health.ArtifactClosure != domain.BackupHealthUnhealthy {
		t.Fatalf("artifact closure = %q, want unhealthy", health.ArtifactClosure)
	}
	artifacts[f.artifact.Digest] = true

	now = now.Add(store.DefaultLocalCheckpointMaxAge + time.Second)
	health, err = s.BackupHealth(ctx)
	if err != nil {
		t.Fatalf("BackupHealth after checkpoint ages: %v", err)
	}
	if health.CheckpointCurrency != domain.BackupHealthUnhealthy {
		t.Fatalf("checkpoint currency = %q, want unhealthy", health.CheckpointCurrency)
	}

	now = time.Date(2026, 8, 27, 4, 0, 0, 0, time.UTC)
	health, err = s.BackupHealth(ctx)
	if err != nil {
		t.Fatalf("BackupHealth after restore test ages: %v", err)
	}
	if health.RestoreTestAge != domain.BackupHealthUnhealthy {
		t.Fatalf("restore-test age = %q, want unhealthy", health.RestoreTestAge)
	}
}

func TestLocalCheckpointHealthIncludesEveryStageInputRole(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "freeside.db")
	checkpointPath := filepath.Join(root, "checkpoint.db")
	now := time.Date(2026, 7, 27, 4, 0, 0, 0, time.UTC)
	digest := func(char string) domain.Digest {
		return domain.Digest("sha256:" + strings.Repeat(char, 64))
	}
	specDigest := digest("1")
	promptDigest := digest("2")
	policyDigest := digest("3")
	inputDigest := digest("4")
	conversationDigest := digest("5")
	priorDigest := digest("6")
	imageDigest := digest("7")
	vendorDigest := digest("8")
	stageInputs, err := domain.NewStageInputSnapshot(domain.StageInputSnapshotInput{
		InputDigest:         inputDigest,
		SpecificationDigest: specDigest,
		PromptPackageDigest: promptDigest,
		PolicyDigest:        policyDigest,
		VendorInstructions: &domain.VendorInstructionSnapshot{
			Vendor:   domain.AgentVendorClaude,
			Delivery: domain.VendorInstructionDeliveryAppendFile,
			Digest:   &vendorDigest,
		},
		ConversationDigest:   &conversationDigest,
		PriorArtifactDigests: []domain.Digest{priorDigest},
		ImageInputDigests:    []domain.Digest{imageDigest},
	})
	if err != nil {
		t.Fatalf("NewStageInputSnapshot: %v", err)
	}
	f := newAdmissionFixture(t, nil)
	f.run.SpecDigest = specDigest
	f.run.PolicyDigest = policyDigest
	f.admission, err = domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: f.admission.InvocationID, RunID: f.admission.RunID,
		StageID: f.admission.StageID, AttemptID: f.admission.AttemptID,
		Backend: f.admission.Backend, Capabilities: f.admission.Capabilities,
		OperatingMode: f.admission.OperatingMode, CredentialMode: f.admission.CredentialMode,
		EgressProfile: f.admission.EgressProfile, ImageRef: f.admission.ImageRef,
		SpecDigest: specDigest, PolicyDigest: policyDigest, InputDigest: inputDigest,
		StageInputs: &stageInputs, Base: f.admission.Base, Workspace: f.admission.Workspace,
		AuthIdentityID: f.admission.AuthIdentityID, AdmittedAt: f.admission.AdmittedAt,
	})
	if err != nil {
		t.Fatalf("NewExecutionAdmission: %v", err)
	}

	stageInputDigests := []domain.Digest{
		specDigest, promptDigest, policyDigest, vendorDigest,
		conversationDigest, priorDigest, imageDigest,
	}
	artifacts := backupArtifactSet{}
	for _, stageInputDigest := range stageInputDigests {
		artifacts[stageInputDigest] = true
	}
	source, err := store.NewLocalCheckpointHealthSource(store.LocalCheckpointHealthOptions{
		CheckpointPath:  checkpointPath,
		RestoreTestPath: filepath.Join(root, "restore-test.db"),
		Artifacts:       artifacts, ApprovedRecipes: approvedFixtureRecipes(),
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewLocalCheckpointHealthSource: %v", err)
	}
	s := openStoreAt(t, dbPath, store.Options{
		AdmissionFloors: attendedFloors(), ApprovedRecipes: approvedFixtureRecipes(),
		BackupHealthSource: source,
	})
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, f.run); err != nil {
			return err
		}
		return tx.RecordAuthIdentity(ctx, f.identity, admissionEpoch)
	}); err != nil {
		t.Fatalf("seed admission parents: %v", err)
	}
	if err := recordAdmission(t, s, f.admission); err != nil {
		t.Fatalf("record admission: %v", err)
	}
	if err := s.Checkpoint(ctx, checkpointPath); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	writeCheckpointGeneratedAt(t, checkpointPath, now)

	for _, missing := range stageInputDigests {
		health, err := s.BackupHealth(ctx)
		if err != nil {
			t.Fatalf("BackupHealth with complete stage inputs: %v", err)
		}
		if health.ArtifactClosure != domain.BackupHealthHealthy {
			t.Fatalf("complete stage input closure = %q, want healthy", health.ArtifactClosure)
		}
		artifacts[missing] = false
		health, err = s.BackupHealth(ctx)
		if err != nil {
			t.Fatalf("BackupHealth missing %s: %v", missing, err)
		}
		if health.ArtifactClosure != domain.BackupHealthUnhealthy {
			t.Fatalf("closure missing %s = %q, want unhealthy",
				missing, health.ArtifactClosure)
		}
		artifacts[missing] = true
	}
}

func TestLocalCheckpointHealthDoesNotTrustCheckpointModTime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "freeside.db")
	checkpointPath := filepath.Join(root, "checkpoint.db")
	restoreTestPath := filepath.Join(root, "restore-test.db")
	now := time.Date(2026, 7, 27, 4, 0, 0, 0, time.UTC)
	source, err := store.NewLocalCheckpointHealthSource(store.LocalCheckpointHealthOptions{
		CheckpointPath: checkpointPath, RestoreTestPath: restoreTestPath,
		Artifacts: backupArtifactSet{}, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewLocalCheckpointHealthSource: %v", err)
	}
	s := openStoreAt(t, dbPath, store.Options{BackupHealthSource: source})
	if err := s.Checkpoint(ctx, checkpointPath); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	writeCheckpointGeneratedAt(
		t, checkpointPath, now.Add(-store.DefaultLocalCheckpointMaxAge-time.Second))
	if err := os.Chtimes(checkpointPath, now, now); err != nil {
		t.Fatalf("touch stale checkpoint: %v", err)
	}

	health, err := s.BackupHealth(ctx)
	if err != nil {
		t.Fatalf("BackupHealth: %v", err)
	}
	if health.CheckpointCurrency != domain.BackupHealthUnhealthy {
		t.Fatalf("touched stale checkpoint currency = %q, want unhealthy", health.CheckpointCurrency)
	}
}

func TestLocalCheckpointHealthReportsMissingEvidenceUnhealthy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	source, err := store.NewLocalCheckpointHealthSource(store.LocalCheckpointHealthOptions{
		CheckpointPath:  filepath.Join(root, "missing-checkpoint.db"),
		RestoreTestPath: filepath.Join(root, "missing-restore.db"),
		Artifacts:       backupArtifactSet{},
	})
	if err != nil {
		t.Fatalf("NewLocalCheckpointHealthSource: %v", err)
	}
	s := openStore(t, store.Options{BackupHealthSource: source})
	health, err := s.BackupHealth(ctx)
	if err != nil {
		t.Fatalf("BackupHealth: %v", err)
	}
	want := domain.BackupHealth{
		Encryption:         domain.BackupHealthUnhealthy,
		CheckpointCurrency: domain.BackupHealthUnhealthy,
		ArtifactClosure:    domain.BackupHealthUnhealthy,
		RestoreTestAge:     domain.BackupHealthUnhealthy,
	}
	if health != want {
		t.Fatalf("missing local evidence = %+v, want %+v", health, want)
	}
}

func writeCheckpointGeneratedAt(t *testing.T, path string, generatedAt time.Time) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open checkpoint marker database: %v", err)
	}
	_, writeErr := db.Exec(
		`INSERT INTO local_backup_checkpoint_marker (id, generated_at)
		 VALUES (1, ?)
		 ON CONFLICT (id) DO UPDATE SET generated_at = excluded.generated_at`,
		generatedAt.UTC().Format(time.RFC3339Nano))
	if err := errors.Join(writeErr, db.Close()); err != nil {
		t.Fatalf("write checkpoint marker: %v", err)
	}
}

func TestLocalCheckpointHealthIncludesDurablePayloadBlobs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	recipeDigest := domain.Digest("sha256:durable-recipe")
	artifacts := backupArtifactSet{recipeDigest: true}
	extractors := map[string]store.BackupPayloadDigestExtractor{
		"backup.test": func(entry store.QueueEntry) ([]domain.Digest, error) {
			if entry.IdempotencyKey != "durable-task-1" {
				return nil, domain.ErrParentKeyMismatch
			}
			return []domain.Digest{domain.Digest(entry.Payload)}, nil
		},
		"backup.other": func(store.QueueEntry) ([]domain.Digest, error) {
			return nil, domain.ErrParentKeyMismatch
		},
	}
	files, err := store.NewDefaultLocalBackupFiles(dbPath)
	if err != nil {
		t.Fatalf("NewDefaultLocalBackupFiles: %v", err)
	}
	source, err := files.NewCheckpointHealthSource(artifacts, nil, extractors)
	if err != nil {
		t.Fatalf("NewCheckpointHealthSource: %v", err)
	}
	s := openStoreAt(t, dbPath, store.Options{BackupHealthSource: source})
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, _, err := tx.EnqueueOutbox(
			ctx, "durable-task-1", "backup.test", []byte(recipeDigest))
		return err
	}); err != nil {
		t.Fatalf("enqueue durable blob task: %v", err)
	}
	producer, err := files.NewProducer(s)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	if err := producer.Maintain(ctx); err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	if health, err := s.BackupHealth(ctx); err != nil || health != healthyBackupHealth() {
		t.Fatalf("durable payload health = %+v, %v; want healthy", health, err)
	}

	artifacts[recipeDigest] = false
	health, err := s.BackupHealth(ctx)
	if err != nil {
		t.Fatalf("BackupHealth with missing durable payload blob: %v", err)
	}
	if health.ArtifactClosure != domain.BackupHealthUnhealthy {
		t.Fatalf("missing durable payload closure = %q, want unhealthy", health.ArtifactClosure)
	}

	// A durable row this binary cannot reconstruct refuses the checkpoint
	// without failing the evaluation: a substituted key, a substituted kind
	// whose extractor rejects the payload, and an unregistered kind are the
	// same unverifiable manifest, and each must report unhealthy rather than
	// wedge the caller (#430).
	artifacts[recipeDigest] = true
	for _, tamper := range []struct {
		name     string
		mutation string
	}{
		{"substituted durable payload key", `
			UPDATE outbox SET idempotency_key = 'durable-task-2'
			 WHERE idempotency_key = 'durable-task-1'`},
		{"substituted durable payload kind", `
			UPDATE outbox
			   SET idempotency_key = 'durable-task-1', kind = 'backup.other'
			 WHERE idempotency_key = 'durable-task-2'`},
		{"unregistered durable payload kind", `
			UPDATE outbox SET kind = 'backup.unknown'
			 WHERE idempotency_key = 'durable-task-1'`},
	} {
		if err := store.MutateEncryptedCheckpointForTest(
			ctx, files, tamper.mutation); err != nil {
			t.Fatalf("%s: %v", tamper.name, err)
		}
		health, err := s.BackupHealth(ctx)
		if err != nil {
			t.Fatalf("BackupHealth after %s: %v", tamper.name, err)
		}
		if health.ArtifactClosure != domain.BackupHealthUnhealthy {
			t.Fatalf("%s closure = %q, want unhealthy", tamper.name, health.ArtifactClosure)
		}
	}
}

func TestLocalCheckpointHealthRetainsExternalCommandBindings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	f := newFixtures(t)
	externalContent := "externally stored UTF-8 bytes"
	externalClaim := (domain.ClaimText{Content: externalContent}).ComputeDigest()
	claims := append([]domain.AgentClaim{}, f.item.AgentClaims...)
	claims[0].Digest = externalClaim
	duplicateInline := claims[1]
	duplicateInline.Label = "duplicate inline summary"
	boundItem, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: f.item.ID, ProjectID: f.item.ProjectID, Subject: f.item.Subject,
		Type: f.item.Type, Priority: f.item.Priority, Reason: f.item.Reason,
		RequestedDecision: f.item.RequestedDecision, EvidenceSnapshot: f.item.EvidenceSnapshot,
		AgentClaims: append(claims, duplicateInline),
		PRHeadSHA:   f.item.PRHeadSHA, PRReference: f.item.PRReference,
		CommitPlanNotice: f.item.CommitPlanNotice,
		ItemVersion:      f.item.ItemVersion, InterruptionClass: f.item.InterruptionClass,
		ConversationID: f.item.ConversationID, ExpiresWhen: f.item.ExpiresWhen,
		Status: f.item.Status,
	}, approvedFixtureRecipes())
	if err != nil {
		t.Fatalf("NewAttentionItem duplicate inline digest: %v", err)
	}
	boundCommand, err := domain.NewCommand(domain.CommandInput{
		CommandID: f.command.CommandID, DeviceID: f.command.DeviceID,
		ItemID: boundItem.ID, ItemVersion: boundItem.ItemVersion,
		PRHeadSHA: boundItem.PRHeadSHA, ArtifactDigests: boundItem.ArtifactDigests,
		Action: f.command.Action,
	})
	if err != nil {
		t.Fatalf("NewCommand duplicate inline digest: %v", err)
	}
	f.item = boundItem
	f.command = boundCommand
	artifacts := backupArtifactSet{
		f.artifact.Digest: true,
		externalClaim:     true,
		// The fixture's inline ClaimText digest is deliberately absent: its
		// bytes live in the command's accepted item, not the artifact store.
	}
	files, err := store.NewDefaultLocalBackupFiles(dbPath)
	if err != nil {
		t.Fatalf("NewDefaultLocalBackupFiles: %v", err)
	}
	source, err := files.NewCheckpointHealthSource(artifacts, approvedFixtureRecipes(), nil)
	if err != nil {
		t.Fatalf("NewCheckpointHealthSource: %v", err)
	}
	s := openStoreAt(t, dbPath, store.Options{
		ApprovedRecipes: approvedFixtureRecipes(), BackupHealthSource: source,
	})
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, f.run); err != nil {
			return err
		}
		if err := tx.PutConversation(ctx, f.conversation); err != nil {
			return err
		}
		if err := tx.PutArtifact(ctx, f.artifact); err != nil {
			return err
		}
		if err := tx.PutDevice(ctx, f.device); err != nil {
			return err
		}
		if err := tx.PutAttentionItem(ctx, f.item); err != nil {
			return err
		}
		return tx.PutCommand(ctx, f.command)
	}); err != nil {
		t.Fatalf("seed command binding: %v", err)
	}

	evolved, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: f.item.ID, ProjectID: f.item.ProjectID, Subject: f.item.Subject,
		Type: f.item.Type, Priority: f.item.Priority, Reason: f.item.Reason,
		RequestedDecision: f.item.RequestedDecision,
		PRHeadSHA:         f.item.PRHeadSHA,
		PRReference:       f.item.PRReference,
		CommitPlanNotice:  f.item.CommitPlanNotice,
		ItemVersion:       f.item.ItemVersion + 1, InterruptionClass: f.item.InterruptionClass,
		ConversationID: f.item.ConversationID, ExpiresWhen: f.item.ExpiresWhen,
		Status: f.item.Status,
	}, approvedFixtureRecipes())
	if err != nil {
		t.Fatalf("NewAttentionItem evolved: %v", err)
	}
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, evolved)
	}); err != nil {
		t.Fatalf("evolve item past command binding: %v", err)
	}

	producer, err := files.NewProducer(s)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	if err := producer.Maintain(ctx); err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	if health, err := s.BackupHealth(ctx); err != nil || health != healthyBackupHealth() {
		t.Fatalf("command-bound closure health = %+v, %v; want healthy", health, err)
	}

	artifacts[externalClaim] = false
	health, err := s.BackupHealth(ctx)
	if err != nil {
		t.Fatalf("BackupHealth with missing command-bound artifact: %v", err)
	}
	if health.ArtifactClosure != domain.BackupHealthUnhealthy {
		t.Fatalf("missing command-bound closure = %q, want unhealthy", health.ArtifactClosure)
	}

	if err := store.MutateEncryptedCheckpointForTest(ctx, files,
		`UPDATE commands
		    SET body = json_set(
		        body, '$.inline_claims',
		        json_array(json_object('digest', ?, 'content', ?)))
		  WHERE command_id = ?`,
		externalClaim, externalContent, f.command.CommandID); err != nil {
		t.Fatalf("forge external digest classification: %v", err)
	}
	if _, err := s.BackupHealth(ctx); err == nil {
		t.Fatal("BackupHealth accepted matching content injected into an immutable command binding")
	}
}

func TestLocalCheckpointHealthRejectsDivergentClosureRows(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		mutation string
	}{
		{"artifact column", `UPDATE artifacts SET digest = 'sha256:forged'`},
		{"conversation metadata", `UPDATE conversations SET entity_version = 0`},
		{"attention item column", `UPDATE attention_items SET project_id = 'forged'`},
		{"command binding", `UPDATE commands SET item_version = item_version + 1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dbPath := filepath.Join(t.TempDir(), "freeside.db")
			f := newFixtures(t)
			artifacts := backupArtifactSet{
				f.artifact.Digest: true,
				"sha256:img":      true,
			}
			files, err := store.NewDefaultLocalBackupFiles(dbPath)
			if err != nil {
				t.Fatalf("NewDefaultLocalBackupFiles: %v", err)
			}
			source, err := files.NewCheckpointHealthSource(
				artifacts, approvedFixtureRecipes(), nil)
			if err != nil {
				t.Fatalf("NewCheckpointHealthSource: %v", err)
			}
			s := openStoreAt(t, dbPath, store.Options{
				ApprovedRecipes: approvedFixtureRecipes(), BackupHealthSource: source,
			})
			if err := s.Write(ctx, func(tx *store.WriteTx) error {
				if err := tx.PutRun(ctx, f.run); err != nil {
					return err
				}
				if err := tx.PutConversation(ctx, f.conversation); err != nil {
					return err
				}
				if err := tx.PutArtifact(ctx, f.artifact); err != nil {
					return err
				}
				if err := tx.PutDevice(ctx, f.device); err != nil {
					return err
				}
				if err := tx.PutAttentionItem(ctx, f.item); err != nil {
					return err
				}
				return tx.PutCommand(ctx, f.command)
			}); err != nil {
				t.Fatalf("seed closure rows: %v", err)
			}
			producer, err := files.NewProducer(s)
			if err != nil {
				t.Fatalf("NewProducer: %v", err)
			}
			if err := producer.Maintain(ctx); err != nil {
				t.Fatalf("Maintain: %v", err)
			}

			if err := store.MutateEncryptedCheckpointForTest(
				ctx, files, tc.mutation,
			); err != nil {
				t.Fatalf("mutate checkpoint: %v", err)
			}
			if _, err := s.BackupHealth(ctx); err == nil {
				t.Fatal("BackupHealth accepted a divergent closure row")
			}
		})
	}
}
