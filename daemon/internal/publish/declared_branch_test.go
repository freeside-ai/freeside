package publish_test

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func TestDeclaredBranchRejectsBeforeIntentOrEffect(t *testing.T) {
	t.Parallel()
	for _, branch := range []string{"main", "refs/heads/work", "freeside/work", "bad branch", "a/.hidden"} {
		t.Run(branch, func(t *testing.T) {
			h := newDrainHarness(t)
			c := testCandidate(t)
			c.Branch = branch
			if _, err := h.pub.Publish(t.Context(), c, testApprovedRecipes()); err == nil || !strings.Contains(err.Error(), branch) {
				t.Fatalf("publish branch %q: %v", branch, err)
			}
			if len(pendingPublications(t, h.store)) != 0 || len(h.gh.requestLog()) != 0 {
				t.Fatal("invalid branch reached intent or forge")
			}
		})
	}
}

func TestDeclaredBranchURLCharactersConverge(t *testing.T) {
	t.Parallel()
	for _, branch := range []string{"feat/a#b", "feat/a&b", "feat/a+b", "feat/a%b", "feat/a%2Fb", "feat/a;b", "feat/café"} {
		t.Run(branch, func(t *testing.T) {
			h := newDrainHarness(t)
			c := testCandidate(t)
			c.Branch = branch
			for _, invocation := range []domain.InvocationID{"first", "retry"} {
				c.InvocationID = invocation
				result, err := h.pub.Publish(t.Context(), c, testApprovedRecipes())
				if err != nil {
					t.Fatalf("%s: %v", invocation, err)
				}
				if result.Branch != branch {
					t.Fatalf("branch = %q, want %q", result.Branch, branch)
				}
			}
			if len(h.gh.refs) != 1 || h.gh.refs[branch] != c.HeadSHA || len(h.gh.prs) != 1 {
				t.Fatal("retry did not converge on the exact branch and one PR")
			}
		})
	}
}

func TestDeclaredBranchRestartAndBinding(t *testing.T) {
	t.Parallel()
	for _, afterEffect := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-effect", true: "after-effect"}[afterEffect], func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "store.db")
			s := storetest.Open(t, path, store.Options{})
			gh := newFakeGitHub(t)
			ledger, err := publish.NewStoreLedger(s)
			if err != nil {
				t.Fatal(err)
			}
			p := newTestPublisherWithTrust(t, gh, ledger, conformantTrust(t))
			c := testCandidate(t)
			c.Branch = "feat/meaningful-task"
			interrupted := errors.New("crash after intent")
			if afterEffect {
				if _, err := p.Publish(ctx, c, testApprovedRecipes()); err != nil {
					t.Fatal(err)
				}
			} else {
				_, err := p.PublishAfterGate(ctx, c, testApprovedRecipes(), func(_ context.Context, g publish.GatedHead) error {
					if g.Branch() != c.Branch {
						t.Fatalf("capability branch %q", g.Branch())
					}
					return interrupted
				})
				if !errors.Is(err, interrupted) {
					t.Fatal(err)
				}
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s = storetest.Open(t, path, store.Options{})
			ledger, err = publish.NewStoreLedger(s)
			if err != nil {
				t.Fatal(err)
			}
			p = newTestPublisherWithTrust(t, gh, ledger, conformantTrust(t))
			// A new invocation cannot escape the old intent's branch binding,
			// including before an outcome exists.
			other := c
			other.InvocationID = "other-invocation"
			other.Branch = "fix/renamed-task"
			if _, err := p.Publish(ctx, other, testApprovedRecipes()); !errors.Is(err, publish.ErrPublicationConflict) {
				t.Fatalf("rename = %v", err)
			}
			if _, err := publish.DrainPendingPublications(ctx, s, p, resolverFor(t, other)); err == nil {
				t.Fatal("foreign resolver accepted")
			}
			other.InvocationID = c.InvocationID
			if _, err := publish.DrainPendingPublications(ctx, s, p, resolverFor(t, other)); !errors.Is(err, publish.ErrPublicationConflict) {
				t.Fatalf("renaming resolver = %v", err)
			}
			if n, err := publish.DrainPendingPublications(ctx, s, p, resolverFor(t, c)); err != nil || n != 1 {
				t.Fatalf("drain = %d, %v", n, err)
			}
			if n, err := publish.DrainPendingPublications(ctx, s, p, resolverFor(t, c)); err != nil || n != 0 {
				t.Fatalf("second drain = %d, %v", n, err)
			}
			outcome, found, err := publish.LoadOutcome(ctx, s, c, testApprovedRecipes(), p.VerifyOutcome)
			if err != nil || !found || outcome.Branch != c.Branch {
				t.Fatalf("outcome = %+v, %t, %v", outcome, found, err)
			}
			if err := p.VerifyOutcome(ctx, other, testCandidateIdentity(t), outcome); !errors.Is(err, publish.ErrPublicationConflict) {
				t.Fatalf("verify renamed outcome = %v", err)
			}
			if _, _, err := publish.LoadOutcome(ctx, s, other, testApprovedRecipes(), p.VerifyOutcome); !errors.Is(err, publish.ErrPublicationConflict) {
				t.Fatalf("load renamed outcome = %v", err)
			}
			if _, err := p.Publish(ctx, other, testApprovedRecipes()); !errors.Is(err, publish.ErrPublicationConflict) {
				t.Fatalf("rename after outcome = %v", err)
			}
			c.InvocationID = "rerun-invocation"
			if _, err := p.Publish(ctx, c, testApprovedRecipes()); err != nil {
				t.Fatal(err)
			}
			if len(gh.refs) != 1 || len(gh.prs) != 1 || gh.refs[c.Branch] != c.HeadSHA {
				t.Fatal("retry did not converge to one declared branch and PR")
			}
			if countRequests(gh.requestLog(), http.MethodPost+" "+testRepoPath+"/pulls") != 1 {
				t.Fatal("retry created a second PR")
			}
			if marker, ok := publish.ParseMarker(gh.prs[0].Body); !ok || marker != testCandidateIdentity(t).Digest() {
				t.Fatal("identity marker changed")
			}
		})
	}
}

func TestDeclaredBranchCollisionsLeaveFirstPublicationUntouched(t *testing.T) {
	t.Parallel()
	for _, foreignPR := range []bool{false, true} {
		t.Run(map[bool]string{false: "foreign-commit", true: "foreign-identity"}[foreignPR], func(t *testing.T) {
			h := newDrainHarness(t)
			c := testCandidate(t)
			c.Branch = "feat/shared-name"
			sha := strings.Repeat("f", 40)
			want := publish.ErrPublicationConflict
			if foreignPR {
				sha = c.HeadSHA
				want = publish.ErrForeignResource
				h.gh.prs = append(h.gh.prs, fakePR{Number: 7, State: "open", HeadRef: c.Branch, HeadSHA: sha, Title: "First publication", Body: "<!-- freeside:publication-identity=sha256:" + strings.Repeat("f", 64) + " -->"})
			}
			h.gh.refs[c.Branch] = sha
			if _, err := h.pub.Publish(t.Context(), c, testApprovedRecipes()); !errors.Is(err, want) {
				t.Fatalf("collision = %v, want %v", err, want)
			}
			if h.gh.refs[c.Branch] != sha {
				t.Fatal("collision moved existing branch")
			}
			for _, req := range h.gh.requestLog() {
				if strings.HasPrefix(req, "POST ") || strings.HasPrefix(req, "PATCH ") || strings.HasPrefix(req, "DELETE ") {
					t.Fatalf("collision mutated forge: %s", req)
				}
			}
		})
	}
}
