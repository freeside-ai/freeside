package publish_test

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

// composedGate is the whole publication path standing up at once: a real
// bare repository over the file scheme, the real hardened transport bound
// to it, and a real Publisher over the forge fake, joined exactly the way
// the engine joins them (#288). Nothing here is a double except GitHub
// itself, which the transport never touches.
type composedGate struct {
	remote    publish.LocalRemoteFixture
	transport *publish.Transport
	checkout  publish.Checkout
	head      string
	candidate publish.Candidate
	forge     *fakeGitHub
	pushes    int
}

func newComposedGate(t *testing.T) *composedGate {
	t.Helper()
	// The repository is testTrustRepo so that one name runs through all
	// of it: the git remote, the forge fake's routes, the trust profile,
	// and the authorization record.
	remote := publish.NewLocalRemoteFixture(t, testTrustRepo)
	co, err := remote.Transport.FetchBase(
		t.Context(), remote.Repo, "main", remote.BaseSHA, filepath.Join(t.TempDir(), "checkout"),
	)
	if err != nil {
		t.Fatalf("FetchBase: %v", err)
	}
	head := publish.CandidateHeadForTest(t, co)
	return &composedGate{
		remote:    remote,
		transport: remote.Transport,
		checkout:  co,
		head:      head,
		candidate: testCandidateAtHead(t, head),
		forge:     newFakeGitHub(t),
	}
}

// authorize claims the transport's single publication authority for p,
// standing in for the daemon's wiring.
func (c *composedGate) authorize(t *testing.T, p *publish.Publisher) *publish.Publisher {
	t.Helper()
	if err := c.transport.AuthorizePublisher(p); err != nil {
		t.Fatalf("AuthorizePublisher: %v", err)
	}
	return p
}

// publishThrough runs the composition: Publisher gates, and only its
// post-gate callback can reach the transport, because only it holds the
// GatedHead PushHead requires.
func (c *composedGate) publishThrough(t *testing.T, p *publish.Publisher, approved map[domain.Digest]bool) error {
	t.Helper()
	_, err := p.PublishAfterGate(
		t.Context(), c.candidate, approved,
		func(ctx context.Context, gated publish.GatedHead) error {
			c.pushes++
			_, pushErr := c.transport.PushHead(ctx, c.checkout, gated)
			return pushErr
		},
	)
	return err
}

// remoteRefs reads the bare repository directly: the assertion is about
// what is actually on the remote, not what the transport reported.
func (c *composedGate) remoteRefs(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("git", "-C", c.remote.Bare, "for-each-ref", "--format=%(refname) %(objectname)") //nolint:gosec // G204: test fixture path
	cmd.Env = scrubbedLiveGitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("for-each-ref: %v\n%s", err, out)
	}
	return string(out)
}

// candidateBranch is the branch this candidate's identity derives to,
// computed independently of the code under test.
func (c *composedGate) candidateBranch(t *testing.T) string {
	t.Helper()
	recipe := testRecipe
	id, err := publish.DeriveIdentity(publish.IdentityInput{
		Repo:            c.candidate.Repo,
		BaseRef:         c.candidate.BaseRef,
		SourceHeadSHA:   c.candidate.HeadSHA,
		ArtifactDigests: []domain.Digest{testArtifactD},
		RecipeDigest:    &recipe,
	})
	if err != nil {
		t.Fatalf("DeriveIdentity: %v", err)
	}
	return id.BranchName()
}

// TestGateRefusalReachesNoRemoteRef is #288 acceptance 1: a candidate that
// fails Publisher's authorization or artifact gate cannot put a ref on the
// managed repository. The refusal is not a cleanup — the push is never
// attempted, because the capability PushHead requires is minted only on
// the far side of those gates.
func TestGateRefusalReachesNoRemoteRef(t *testing.T) {
	cases := []struct {
		name string
		// publisher wires the gate under test to refuse.
		publisher func(t *testing.T, c *composedGate) *publish.Publisher
		approved  func() map[domain.Digest]bool
		wantErr   error
	}{
		{
			// No record under the candidate's authorization id: the
			// publication is not authorized at all.
			name: "unauthorized candidate",
			publisher: func(t *testing.T, c *composedGate) *publish.Publisher {
				t.Helper()
				return c.authorize(t,
					newTestPublisherFull(t, c.forge, newMemoryLedger(), conformantTrust(t), authzWith()))
			},
			approved: testApprovedRecipes,
			wantErr:  publish.ErrUnauthorizedPublication,
		},
		{
			// The recipe the evidence was produced under is no longer
			// approved, so no artifact is eligible for the snapshot.
			name: "unapproved recipe",
			publisher: func(t *testing.T, c *composedGate) *publish.Publisher {
				t.Helper()
				return c.authorize(t, newTestPublisherFull(
					t, c.forge, newMemoryLedger(), conformantTrust(t),
					authzWith(testCandidateAuthorizationAtHead(t, c.head)),
				))
			},
			approved: func() map[domain.Digest]bool { return map[domain.Digest]bool{} },
			wantErr:  domain.ErrUnapprovedRecipe,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newComposedGate(t)
			err := c.publishThrough(t, tc.publisher(t, c), tc.approved())
			if err == nil {
				t.Fatal("gated publication succeeded for a candidate the gates must refuse")
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("error = %v, want %v", err, tc.wantErr)
			}
			if c.pushes != 0 {
				t.Errorf("transport reached %d times after a gate refusal, want 0", c.pushes)
			}
			refs := c.remoteRefs(t)
			if branch := c.candidateBranch(t); strings.Contains(refs, branch) {
				t.Errorf("refused candidate created %s; remote refs:\n%s", branch, refs)
			}
			if strings.Count(strings.TrimSpace(refs), "\n") != 0 {
				t.Errorf("refused publication changed the remote ref set:\n%s", refs)
			}
		})
	}
}

// TestForeignPublisherCannotReachTheTransport closes the hole a sealed
// type alone leaves open (raised twice on PR #322). Publisher's
// collaborators are exported interfaces and its approved-recipe set is a
// caller argument, so anyone holding the real transport can stand up a
// second publisher whose gates pass by construction. Its verdict must not
// travel — and it must not be able to make itself the authority either,
// which is what the second round of that finding was about.
func TestForeignPublisherCannotReachTheTransport(t *testing.T) {
	// Every gate these publishers run passes: their own authorization
	// source holds the record, their own trust source is conformant, and
	// they approve the candidate's recipe. They are simply not the
	// publisher the transport claimed.
	foreign := func(t *testing.T, c *composedGate) *publish.Publisher {
		t.Helper()
		return newTestPublisherFull(
			t, c.forge, newMemoryLedger(), conformantTrust(t),
			authzWith(testCandidateAuthorizationAtHead(t, c.head)),
		)
	}

	t.Run("never claimed the authority", func(t *testing.T) {
		c := newComposedGate(t)
		c.authorize(t, foreign(t, c)) // the daemon's own wiring
		assertForeignPublisherRefused(t, c, foreign(t, c))
	})

	t.Run("tries to claim a held authority", func(t *testing.T) {
		c := newComposedGate(t)
		c.authorize(t, foreign(t, c)) // the daemon's own wiring, first
		rogue := foreign(t, c)
		// The claim is one-shot, so the rogue cannot nominate itself: it
		// holds the real transport, and that is not enough.
		if err := c.transport.AuthorizePublisher(rogue); !errors.Is(err, publish.ErrTransportAuthorityClaimed) {
			t.Fatalf("second AuthorizePublisher = %v, want ErrTransportAuthorityClaimed", err)
		}
		assertForeignPublisherRefused(t, c, rogue)
	})

	t.Run("claims a transport of its own", func(t *testing.T) {
		c := newComposedGate(t)
		c.authorize(t, foreign(t, c))
		rogue := foreign(t, c)
		// Claiming some *other* transport does not make the rogue's
		// verdict travel on this one.
		other := publish.NewLocalRemoteFixture(t, testTrustRepo)
		if err := other.Transport.AuthorizePublisher(rogue); err != nil {
			t.Fatalf("AuthorizePublisher on another transport: %v", err)
		}
		assertForeignPublisherRefused(t, c, rogue)
	})
}

// assertForeignPublisherRefused drives the composed path with a publisher
// the transport never claimed and pins the whole outcome: the gates
// "passed" so the callback really did reach the transport, the transport
// refused, and the remote is untouched.
func assertForeignPublisherRefused(t *testing.T, c *composedGate, foreign *publish.Publisher) {
	t.Helper()
	err := c.publishThrough(t, foreign, testApprovedRecipes())
	if !errors.Is(err, publish.ErrUngatedPublication) {
		t.Fatalf("error = %v, want ErrUngatedPublication", err)
	}
	if c.pushes != 1 {
		t.Errorf("transport reached %d times, want 1", c.pushes)
	}
	refs := c.remoteRefs(t)
	if branch := c.candidateBranch(t); strings.Contains(refs, branch) {
		t.Errorf("foreign publisher created %s; remote refs:\n%s", branch, refs)
	}
	if strings.Count(strings.TrimSpace(refs), "\n") != 0 {
		t.Errorf("foreign publisher changed the remote ref set:\n%s", refs)
	}
}

// TestGatedPublicationCreatesTheRemoteRef is the positive control the
// refusal cases need: the same composition, with the gates passing, does
// put the candidate head on its derived branch. Without it, the
// no-remote-effect assertions above would also hold for a path that never
// works at all.

func TestGatedPublicationCreatesTheRemoteRef(t *testing.T) {
	c := newComposedGate(t)
	p := c.authorize(t, newTestPublisherFull(
		t, c.forge, newMemoryLedger(), conformantTrust(t),
		authzWith(testCandidateAuthorizationAtHead(t, c.head)),
	))

	if err := c.publishThrough(t, p, testApprovedRecipes()); err != nil {
		t.Fatalf("gated publication: %v", err)
	}
	if c.pushes != 1 {
		t.Errorf("transport reached %d times, want 1", c.pushes)
	}
	branch := c.candidateBranch(t)
	want := "refs/heads/" + branch + " " + c.head
	if refs := c.remoteRefs(t); !strings.Contains(refs, want) {
		t.Errorf("remote does not hold %q; refs:\n%s", want, refs)
	}
}
