package publish

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestOnlyGitNetImportsExec pins the process-boundary discipline the
// importer established, for the publish lane: no publish package file
// may execute a subprocess except the single hardened transport
// runner. A new os/exec import anywhere else is the escape this test
// exists to catch.
func TestOnlyGitNetImportsExec(t *testing.T) {
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range sources {
		if strings.HasSuffix(name, "_test.go") {
			continue // test helpers legitimately run git to build fixtures
		}
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range file.Imports {
			if imp.Path.Value == `"os/exec"` && filepath.Base(name) != "gitnet.go" {
				t.Errorf("%s imports os/exec; only gitnet.go may", name)
			}
		}
	}
}

func TestFinalizeRejectsSubstitutedIntentCoordinates(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "store.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})

	authorizationID := domain.Digest("sha256:" + strings.Repeat("a", 64))
	candidate := Candidate{
		Repo:            "freeside-ai/repo",
		BaseRef:         "main",
		HeadSHA:         strings.Repeat("b", 40),
		Artifacts:       []domain.Artifact{{Digest: "sha256:artifact"}},
		InvocationID:    "publication-1",
		AuthorizationID: &authorizationID,
	}
	identity, err := deriveCandidateIdentity(candidate)
	if err != nil {
		t.Fatalf("derive identity: %v", err)
	}
	substituted := Intent{
		Identity:        identity.Digest(),
		InvocationID:    candidate.InvocationID,
		Repo:            "freeside-ai/foreign",
		BaseRef:         "release",
		SourceHeadSHA:   strings.Repeat("c", 40),
		AuthorizationID: "sha256:" + domain.Digest(strings.Repeat("d", 64)),
	}
	payload, err := substituted.Encode()
	if err != nil {
		t.Fatalf("encode substituted intent: %v", err)
	}
	key, err := IntentKey(candidate.InvocationID, IntentKindPublication)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, _, err := tx.EnqueueOutbox(ctx, key, IntentKindPublication, payload)
		return err
	}); err != nil {
		t.Fatalf("enqueue substituted intent: %v", err)
	}

	err = finalizePublicationResult(ctx, s, candidate, Result{
		Identity: identity, Branch: identity.BranchName(), PRNumber: 101,
	})
	if !errors.Is(err, errPublicationIntentDiverged) {
		t.Fatalf("finalize substituted intent error = %v", err)
	}
	var pending []store.QueueEntry
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		pending, err = tx.ListPendingOutbox(ctx, IntentKindPublication)
		return err
	}); err != nil {
		t.Fatalf("list pending intents: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending intents after rejected finalization = %d, want 1", len(pending))
	}
}
