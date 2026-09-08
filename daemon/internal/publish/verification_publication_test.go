package publish_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

func TestPublishVerificationRejectsUnboundRecordsBeforeEffects(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*publish.Candidate){
		"missing report":   func(c *publish.Candidate) { c.VerificationReport = nil },
		"foreign snapshot": func(c *publish.Candidate) { c.Artifacts[0].ID = "artifact-foreign" },
		"tampered bytes":   func(c *publish.Candidate) { c.VerificationReport = []byte("Passed: fabricated") },
		"changed import":   func(c *publish.Candidate) { c.ImportResult.CommitSHA = testOtherSHA },
		"added claim":      func(c *publish.Candidate) { c.ImportResult.Claims = []domain.AgentClaim{{Label: "Passed"}} },
	} {
		t.Run(name, func(t *testing.T) {
			gh := newFakeGitHub(t)
			p := newTestPublisher(t, gh, newMemoryLedger())
			c := testCandidate(t)
			mutate(&c)
			if _, err := p.Publish(t.Context(), c, testApprovedRecipes()); !errors.Is(err, publish.ErrUnauthorizedPublication) {
				t.Fatalf("got %v, want unauthorized", err)
			}
			if len(gh.writeRequests()) != 0 {
				t.Fatal("unbound evidence reached an external write")
			}
		})
	}
}

func TestPublishVerificationPreservesProseAndIdentity(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	p := newTestPublisher(t, gh, newMemoryLedger())
	c := testCandidate(t)
	c.Body = "## Why\n\nPreserve these bytes.  \n\n\n"
	first, err := p.Publish(t.Context(), c, testApprovedRecipes())
	if err != nil {
		t.Fatal(err)
	}
	body := gh.prs[0].Body
	if !strings.HasPrefix(body, c.Body+"\n\n<!-- freeside:verification -->") {
		t.Fatal("operator prose was rewritten")
	}
	// Simulate a PR from the old renderer; the immutable publication identity
	// stays the same while recovery installs the publisher-owned results.
	gh.prs[0].Body = c.Body + "\n\n" + first.Identity.Marker()
	second, err := p.Publish(t.Context(), c, testApprovedRecipes())
	if err != nil {
		t.Fatal(err)
	}
	if second.Identity != first.Identity || second.PRNumber != first.PRNumber || second.PRCreated || gh.prs[0].Body != body {
		t.Fatal("rendering repair changed identity or did not restore the exact body")
	}
}
