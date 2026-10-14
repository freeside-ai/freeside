package main

import (
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// A store written before proposal paths persisted their evidence bodies holds
// a carrier row with no body. Startup rebuilds that body, so the daemon's
// first backup pass can verify it.
func TestStartupRestoresMissingProposalEvidence(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	cfg := config{DBPath: dbPath, ListenAddr: "127.0.0.1:0", Environment: environmentEphemeral}
	// A first start creates the store and the state an existing store needs.
	if err := startDaemon(t, cfg).Close(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.Context(), dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// The storetest seed writes the filing card's artifact row the way the
	// daemon did before #2001: without its body.
	storetest.DispatchFollowUpFiling(t, st, storetest.FollowUpFiling{
		ProjectID: "project-legacy-evidence", Repo: "owner/repo", RepositoryID: 1,
	})
	var evidence []domain.ProposalInstance
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		evidence, err = tx.ListProposalEvidence(t.Context())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if len(evidence) == 0 {
		t.Fatal("seed holds no proposal evidence")
	}

	h := startDaemon(t, cfg)
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	blobs, err := signet.NewBlobStore(dbPath + ".blobs")
	if err != nil {
		t.Fatal(err)
	}
	for _, instance := range evidence {
		artifact, err := instance.EvidenceArtifact()
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := blobs.Verify(artifact.Digest); err != nil || !ok {
			t.Errorf("evidence body %s after startup: verified %v, %v; want rebuilt", artifact.Digest, ok, err)
		}
	}
}
