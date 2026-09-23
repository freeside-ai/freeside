package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/operations"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestControlDoctorPreservesRequestedCheckpointRecipes(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.db")
	dbPath := filepath.Join(root, "alias.db")
	if err := os.Symlink(target, dbPath); err != nil {
		t.Fatal(err)
	}
	recipeA := domain.Digest("sha256:" + strings.Repeat("a", 64))
	recipeB := domain.Digest("sha256:" + strings.Repeat("b", 64))
	approved := map[domain.Digest]bool{recipeA: true, recipeB: true}
	blobs, err := signet.NewBlobStore(dbPath + ".blobs")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("checkpoint recipe B evidence")
	digest := domain.Digest(contentaddr.Sum(body))
	if _, err := blobs.Put(digest, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	artifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID: "artifact-doctor-b", Type: domain.ArtifactKindVerifyLog, Digest: digest,
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerVerifier, ProducerInvocationID: "inv-doctor-b",
			HeadBinding: domain.HeadIndependent, VerificationRecipeDigest: &recipeB,
			SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: testRunEvidenceMetadata(int64(len(body))),
	}, approved)
	if err != nil {
		t.Fatal(err)
	}
	seed, _, err := openStoreWithTopicKey(t.Context(), dbPath, store.Options{ApprovedRecipes: approved})
	if err != nil {
		t.Fatal(err)
	}
	writeErr := seed.Write(t.Context(), func(tx *store.WriteTx) error { return tx.PutArtifact(t.Context(), artifact) })
	if err := errors.Join(writeErr, seed.Close()); err != nil {
		t.Fatal(err)
	}
	h, err := run(t.Context(), nil, config{DBPath: dbPath, ListenAddr: "127.0.0.1:0", ApprovedRecipes: approved})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	check := func(recipe domain.Digest, wantClosure bool) {
		t.Helper()
		var output bytes.Buffer
		err := runDoctorCommand(t.Context(), []string{
			"-db", dbPath, "-backend-configuration-digest", string(recipeA),
			"-approved-recipe", string(recipe),
		}, &output, io.Discard)
		if !wantClosure {
			if err == nil || !strings.Contains(err.Error(), domain.ErrPublishEligibleInconsistent.Error()) || output.Len() != 0 {
				t.Fatalf("Doctor should reject recipe B without a report: %s, %v", output.String(), err)
			}
			return
		}
		if err == nil || !strings.Contains(err.Error(), "unhealthy") {
			t.Fatalf("Doctor without conformance: %v", err)
		}
		var report operations.DoctorReport
		if err := json.Unmarshal(output.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		for _, finding := range report.Findings {
			if finding.Code == "artifact_closure" {
				if finding.Healthy != wantClosure {
					t.Fatalf("recipe %s artifact closure = %+v", recipe, finding)
				}
				return
			}
		}
		t.Fatal("Doctor omitted artifact closure")
	}
	seedDoctor := operations.Doctor{
		Store: h.store, Attention: signet.NewService(h.store), ProjectID: "project-system",
		Backend: domain.BackendFreshVMReadOnlyVolumeHandoff, Mode: domain.ModeAttendedDev,
		ConfigurationDigest: recipeA,
		BackupHealthSource: store.BackupHealthSourceFunc(func(context.Context, store.BackupHealthContext) (domain.BackupHealth, error) {
			return domain.BackupHealth{
				Encryption: domain.BackupHealthHealthy, CheckpointCurrency: domain.BackupHealthHealthy,
				ArtifactClosure: domain.BackupHealthUnhealthy, RestoreTestAge: domain.BackupHealthHealthy,
			}, nil
		}),
	}
	if _, err := seedDoctor.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	check(recipeA, false)
	if err := h.store.Read(t.Context(), func(tx *store.ReadTx) error {
		items, err := tx.ListAttentionItems(t.Context())
		if err != nil {
			return err
		}
		for _, item := range items {
			if strings.HasPrefix(string(item.Value.ID), "system-health-doctor-artifact_closure-") && item.Value.Status == domain.StatusOpen {
				return nil
			}
		}
		return errors.New("narrow Doctor request cleared the artifact-closure blocker")
	}); err != nil {
		t.Fatal(err)
	}
	check(recipeB, true)
	ordinary, err := h.store.BackupHealth(t.Context())
	if err != nil || ordinary.ArtifactClosure != domain.BackupHealthHealthy {
		t.Fatalf("request mutated daemon health: %+v, %v", ordinary, err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	check(recipeA, false)
	check(recipeB, true)
	for _, suffix := range []string{".blobs", ".checkpoints", ".backup-encryption.key"} {
		if _, err := os.Stat(target + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Doctor created a canonical-path sidecar %s: %v", suffix, err)
		}
	}
}
