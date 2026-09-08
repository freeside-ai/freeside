package publish

import (
	"context"
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/publicationrecord"
)

func TestUpdateHeadRequiresExactPredecessor(t *testing.T) {
	for _, state := range []string{"old", "new", "foreign", "missing"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			remote := newLocalRemote(t)
			co, err := remote.transport.FetchBase(t.Context(), remote.repo, "main", remote.baseSHA, checkoutDir(t))
			if err != nil {
				t.Fatal(err)
			}
			old := candidateHead(t, co)
			tree := gitOut(t, co.Dir(), "rev-parse", old+"^{tree}")
			head := gitOut(t, co.Dir(), "commit-tree", tree, "-p", co.BaseSHA(), "-m", "successor")
			gated := testGatedHead(t, remote.transport, testIdentityInput(remote.repo, head))
			gated.branch = "feat/retained-pr"
			initial := map[string]string{"old": old, "new": head, "foreign": remote.baseSHA}[state]
			if initial != "" {
				gitOut(t, co.Dir(), "push", remote.bare, initial+":refs/heads/"+gated.Branch())
			}
			before := gitOut(t, remote.bare, "for-each-ref")
			_, err = remote.transport.UpdateHead(t.Context(), co, GatedUpdate{head: gated, expectedOldHead: old})
			if state == "foreign" || state == "missing" {
				if !errors.Is(err, ErrPublicationConflict) {
					t.Fatalf("update = %v, want conflict", err)
				}
				if after := gitOut(t, remote.bare, "for-each-ref"); after != before {
					t.Fatal("refused update changed refs")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if actual := gitOut(t, remote.bare, "rev-parse", "refs/heads/"+gated.Branch()); actual != head {
				t.Fatalf("head = %s, want %s", actual, head)
			}
			if actual := gitOut(t, remote.bare, "rev-parse", "refs/heads/main"); actual != remote.baseSHA {
				t.Fatal("updated base ref")
			}
			if _, err := remote.transport.UpdateHead(t.Context(), co, GatedUpdate{head: gated, expectedOldHead: old}); err != nil {
				t.Fatalf("replay: %v", err)
			}
		})
	}
}

func TestOrdinaryPublisherRefusesSuccessorBeforeEffects(t *testing.T) {
	candidate := Candidate{Successor: &publicationrecord.SuccessorTarget{}}
	called := false
	_, err := (&Publisher{}).PublishAfterGate(t.Context(), candidate, nil, func(_ context.Context, _ GatedHead) error {
		called = true
		return nil
	})
	if !errors.Is(err, ErrUnauthorizedPublication) || called {
		t.Fatalf("ordinary successor = %v, callback %v", err, called)
	}
}
