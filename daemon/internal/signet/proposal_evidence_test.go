package signet_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func requireEvidenceBody(t *testing.T, blobs *signet.BlobStore, digest domain.Digest) {
	t.Helper()
	if ok, err := blobs.Verify(digest); err != nil || !ok {
		t.Fatalf("evidence body %s verifies = %v, %v; want true", digest, ok, err)
	}
}

// reviseClosure approves a non-resolving closure with changes and returns the
// revised proposal digest the approval binds.
func reviseClosure(t *testing.T, f closureFixture, service *signet.Service) (domain.Digest, error) {
	t.Helper()
	item := f.open(t, f.merge)
	cmd := f.decision(item, "approve-changes-evidence", domain.ActionApproveWithChanges)
	cmd.Payload.EffectProposalRevision = &signet.EffectProposalRevisionInput{Resolves: true}
	if _, err := service.Submit(context.Background(), cmd); err != nil {
		return "", err
	}
	return f.approval(t).ProposalDigest, nil
}

// Every production path that writes a proposal evidence carrier also writes
// the body its digest names, so a checkpoint's artifact closure verifies.
func TestProposalEvidenceBodyPersistsOnEveryPath(t *testing.T) {
	t.Run("closure card", func(t *testing.T) {
		f := newClosureFixture(t, true, domain.ClosureFlagOriginProposeSite)
		f.open(t, f.merge)
		requireEvidenceBody(t, f.blobs, f.instance.Proposal.Digest)
	})
	t.Run("closure fallback notice", func(t *testing.T) {
		f := newClosureFixture(t, false, domain.ClosureFlagOriginDaemonFallback)
		f.openNotice(t, f.merge)
		requireEvidenceBody(t, f.blobs, f.instance.Proposal.Digest)
	})
	t.Run("follow-up filing card", func(t *testing.T) {
		f := newFilingFixture(t)
		requireEvidenceBody(t, f.blobs, f.instance.Proposal.Digest)
	})
	t.Run("approve_with_changes revision", func(t *testing.T) {
		f := newClosureFixture(t, false, domain.ClosureFlagOriginProposeSite)
		revised, err := reviseClosure(t, f, f.service)
		if err != nil {
			t.Fatal(err)
		}
		if revised == f.instance.Proposal.Digest {
			t.Fatal("approval bound the prior digest, not the revision")
		}
		requireEvidenceBody(t, f.blobs, revised)
	})
	t.Run("start_with_changes revision", func(t *testing.T) {
		f := newProposalDecisionFixture(t)
		command := f.proposalCommand("command-revise-evidence", domain.ActionStartWithChanges)
		command.Payload.TaskProposalRevision = &signet.TaskProposalRevisionInput{
			Intent:            domain.TaskProposalIntentImplement,
			ExpectedCostUnits: 5, Scope: domain.TaskProposalScope{ComponentCount: 1, DeclaredPathCount: 1},
		}
		if _, err := f.service.Submit(context.Background(), command); err != nil {
			t.Fatal(err)
		}
		replacementID := domain.ItemID(string(f.instance.ID) + "/revision/" + command.CommandID)
		replacement, _ := f.itemSnapshotFor(t, replacementID)
		if len(replacement.ArtifactDigests) != 1 || replacement.ArtifactDigests[0] == f.instance.Proposal.Digest {
			t.Fatalf("replacement digests = %v, want the revised digest", replacement.ArtifactDigests)
		}
		requireEvidenceBody(t, f.blobs, replacement.ArtifactDigests[0])
	})
}

// No proposal path silently skips the body: without a blob store the write
// fails and its transaction leaves no carrier row behind.
func TestProposalEvidenceWithoutBlobStoreFailsClosed(t *testing.T) {
	ctx := context.Background()
	t.Run("closure card", func(t *testing.T) {
		f := newClosureFixture(t, true, domain.ClosureFlagOriginProposeSite)
		service := signet.NewService(f.store)
		if _, err := service.OpenEffectProposalItem(ctx, f.instance.ID, f.merge); !errors.Is(err, signet.ErrAttachmentsUnavailable) {
			t.Fatalf("open without a blob store error = %v, want ErrAttachmentsUnavailable", err)
		}
		artifact, err := f.instance.EvidenceArtifact()
		if err != nil {
			t.Fatal(err)
		}
		if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
			_, err := tx.GetArtifact(ctx, artifact.ID)
			return err
		}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("carrier row after refused open error = %v, want ErrNotFound", err)
		}
	})
	t.Run("approve_with_changes revision", func(t *testing.T) {
		f := newClosureFixture(t, false, domain.ClosureFlagOriginProposeSite)
		if _, err := reviseClosure(t, f, signet.NewService(f.store)); !errors.Is(err, signet.ErrAttachmentsUnavailable) {
			t.Fatalf("revision without a blob store error = %v, want ErrAttachmentsUnavailable", err)
		}
		if approval := f.approval(t); approval != nil {
			t.Fatalf("refused revision recorded approval %+v", approval)
		}
	})
	t.Run("helper", func(t *testing.T) {
		f := newFilingFixture(t)
		err := f.store.Write(ctx, func(tx *store.WriteTx) error {
			_, err := signet.PutProposalEvidence(ctx, tx, nil, f.instance)
			return err
		})
		if !errors.Is(err, signet.ErrAttachmentsUnavailable) {
			t.Fatalf("helper with a nil writer error = %v, want ErrAttachmentsUnavailable", err)
		}
	})
}

// A store written before proposal paths persisted their bodies holds the rows
// but not the bytes. The recovery pass rebuilds exactly the missing bodies,
// is idempotent, and never rewrites a present blob, so a corrupt one still
// fails verification.
func TestRestoreProposalEvidenceRebuildsMissingBodies(t *testing.T) {
	ctx := context.Background()
	f := newClosureFixture(t, false, domain.ClosureFlagOriginProposeSite)
	revised, err := reviseClosure(t, f, f.service)
	if err != nil {
		t.Fatal(err)
	}
	digests := []domain.Digest{f.instance.Proposal.Digest, revised}

	dir := t.TempDir()
	legacy, err := signet.NewBlobStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, digest := range digests {
		if ok, err := legacy.Verify(digest); err != nil || ok {
			t.Fatalf("legacy store already holds %s: %v, %v", digest, ok, err)
		}
	}
	written, err := signet.RestoreProposalEvidence(ctx, f.store, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if written != len(digests) {
		t.Fatalf("restored %d bodies, want %d", written, len(digests))
	}
	for _, digest := range digests {
		requireEvidenceBody(t, legacy, digest)
	}
	if written, err := signet.RestoreProposalEvidence(ctx, f.store, legacy); err != nil || written != 0 {
		t.Fatalf("second pass wrote %d, %v; want 0", written, err)
	}

	corrupt := filepath.Join(dir, "sha256-"+contentaddr.Hex(string(revised)))
	if err := os.Chmod(corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corrupt, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if written, err := signet.RestoreProposalEvidence(ctx, f.store, legacy); err != nil || written != 0 {
		t.Fatalf("pass over a corrupt blob wrote %d, %v; want 0", written, err)
	}
	if ok, err := legacy.Verify(revised); err != nil || ok {
		t.Fatalf("corrupt body verifies = %v, %v; want false", ok, err)
	}

	if _, err := signet.RestoreProposalEvidence(ctx, f.store, nil); !errors.Is(err, signet.ErrAttachmentsUnavailable) {
		t.Fatalf("pass without a blob store error = %v, want ErrAttachmentsUnavailable", err)
	}
}

// backupClosure seals a real local checkpoint of the fixture's store and
// reports its artifact-closure verdict against blobs.
func backupClosure(t *testing.T, f fixture, blobs *signet.BlobStore) domain.BackupHealthStatus {
	t.Helper()
	ctx := context.Background()
	files, err := store.NewDefaultLocalBackupFiles(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	source, err := files.NewCheckpointHealthSource(blobs, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	producer, err := files.NewProducer(f.store)
	if err != nil {
		t.Fatal(err)
	}
	if err := producer.Maintain(ctx); err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	health, err := f.store.BackupHealthWithSource(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	return health.ArtifactClosure
}

// The backup's artifact closure is healthy after proposal activity, because
// every carrier row's body is in the blob store; a truncated body or a store
// that predates persisted bodies is unhealthy until the bytes are back.
func TestProposalEvidenceBackupClosure(t *testing.T) {
	ctx := context.Background()
	t.Run("closure card and approve_with_changes revision", func(t *testing.T) {
		f := newClosureFixture(t, false, domain.ClosureFlagOriginProposeSite)
		if _, err := reviseClosure(t, f, f.service); err != nil {
			t.Fatal(err)
		}
		if got := backupClosure(t, f.fixture, f.blobs); got != domain.BackupHealthHealthy {
			t.Fatalf("artifact closure = %q, want healthy", got)
		}
	})
	t.Run("closure fallback notice", func(t *testing.T) {
		f := newClosureFixture(t, false, domain.ClosureFlagOriginDaemonFallback)
		f.openNotice(t, f.merge)
		if got := backupClosure(t, f.fixture, f.blobs); got != domain.BackupHealthHealthy {
			t.Fatalf("artifact closure = %q, want healthy", got)
		}
	})
	// A follow-up filing card is absent here: its source is a finding
	// disposition, and a store holding one fails the checkpoint restore test
	// for an unrelated reason (#2012). TestProposalEvidenceBodyPersistsOnEveryPath
	// covers its body.
	t.Run("truncated body", func(t *testing.T) {
		f := newClosureFixture(t, true, domain.ClosureFlagOriginProposeSite)
		f.open(t, f.merge)
		dir := t.TempDir()
		blobs, err := signet.NewBlobStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		if written, err := signet.RestoreProposalEvidence(ctx, f.store, blobs); err != nil || written != 1 {
			t.Fatalf("restore wrote %d, %v; want 1", written, err)
		}
		if got := backupClosure(t, f.fixture, blobs); got != domain.BackupHealthHealthy {
			t.Fatalf("artifact closure before truncation = %q, want healthy", got)
		}
		body := filepath.Join(dir, "sha256-"+contentaddr.Hex(string(f.instance.Proposal.Digest)))
		if err := os.Chmod(body, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(body, 1); err != nil {
			t.Fatal(err)
		}
		if got := backupClosure(t, f.fixture, blobs); got == domain.BackupHealthHealthy {
			t.Fatal("artifact closure over a truncated proposal body is healthy")
		}
	})
	t.Run("store from before persisted bodies", func(t *testing.T) {
		f := newClosureFixture(t, false, domain.ClosureFlagOriginProposeSite)
		if _, err := reviseClosure(t, f, f.service); err != nil {
			t.Fatal(err)
		}
		legacy, err := signet.NewBlobStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if got := backupClosure(t, f.fixture, legacy); got == domain.BackupHealthHealthy {
			t.Fatal("artifact closure without proposal bodies is healthy")
		}
		if written, err := signet.RestoreProposalEvidence(ctx, f.store, legacy); err != nil || written != 2 {
			t.Fatalf("restore wrote %d, %v; want 2", written, err)
		}
		if got := backupClosure(t, f.fixture, legacy); got != domain.BackupHealthHealthy {
			t.Fatalf("artifact closure after restore = %q, want healthy", got)
		}
	})
}
