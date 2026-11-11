package integration_test

import (
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestDefaultPolicyClosesWithoutInference is the #3023 end-to-end proof: with
// gates.source_issue_closure unset and no inference client at all, a
// same-repository source URL (recommended provenance) publishes a ready pull
// request that says Closes #82. The default policy sets the flag itself, so no
// model is asked, no closure card or notice opens, and no closure wait is
// armed. The verified provenance is covered at the engine with a daemon-bound
// issue subject (TestReconcileClosureDefaultPolicyRecordsBindingApproval); this
// harness binds no issue subject.
func TestDefaultPolicyClosesWithoutInference(t *testing.T) {
	t.Parallel()
	metadata := productionPublicationMetadata()
	metadata.Title, metadata.Body = "", ""
	metadata.Recipe = "freeside.client-publication/v2"
	metadata.SourceIssue = "https://github.com/" + fakePublicationRepo + "/issues/82"
	p := newAuthoredMetadataHarness(t, metadata, nil)
	p.replay = withPublicAccount(t, p, p.replay, publicAccount)
	if p.judgments != nil {
		t.Fatal("harness composed an inference client; want none")
	}
	p.startAndRecordExport(t)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}

	prs := p.forge.PullRequests()
	if len(prs) != 1 || prs[0].Draft || !strings.Contains(prs[0].Body, "Closes #82") ||
		strings.Contains(prs[0].Body, "Refs #82") {
		t.Fatalf("default-policy PR = %+v, want one ready PR with Closes #82", prs)
	}
	assertClosureOrigin(t, p, domain.ClosureFlagOriginPolicyDefault)
	if open, closed := p.closureCards(t); len(open) != 0 || len(closed) != 0 {
		t.Fatalf("closure cards open=%d closed=%d, want none", len(open), len(closed))
	}
	if waits := pendingClosureWaits(t, p); len(waits) != 0 {
		t.Fatalf("closure waits = %d, want none", len(waits))
	}
}
