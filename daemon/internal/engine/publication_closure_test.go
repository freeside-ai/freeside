package engine

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func closureTestPolicy(t *testing.T, gateValue string) domain.ResolvedPolicy {
	t.Helper()
	keys := []domain.PolicyKey{{
		Key: "gates.spec_approval", Value: "true",
		Provenance: domain.KeyProvenance{Source: domain.ProvenancePreset, Digest: domain.Digest("sha256:" + strings.Repeat("a", 64))},
	}}
	if gateValue != "" {
		keys = append(keys, domain.PolicyKey{
			Key: policySourceIssueClosureGate, Value: gateValue,
			Provenance: domain.KeyProvenance{Source: domain.ProvenancePreset, Digest: domain.Digest("sha256:" + strings.Repeat("b", 64))},
		})
	}
	policy, err := domain.NewResolvedPolicy("run-closure", keys)
	if err != nil {
		t.Fatalf("resolved policy: %v", err)
	}
	return policy
}

func TestParseSourceIssueClosureHumanGateEngine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		value string
		want  bool
	}{
		{"absent defaults to policy approval", "", false},
		{"true is the human gate", "true", true},
		{"false is policy approval", "false", false},
		{"malformed fails closed to the human gate", "sometimes", true},
	}
	for _, tc := range cases {
		if got := parseSourceIssueClosureHumanGate(closureTestPolicy(t, tc.value)); got != tc.want {
			t.Errorf("%s: got %t, want %t", tc.name, got, tc.want)
		}
	}
}

func TestParseSourceIssueURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		source   string
		wantRepo string
		wantNum  int
		wantOK   bool
	}{
		{"same-repo issue url", "https://github.com/owner/name/issues/42", "owner/name", 42, true},
		{"non-github host", "https://gitlab.com/owner/name/issues/1", "", 0, false},
		{"pull request path", "https://github.com/owner/name/pull/42", "", 0, false},
		{"empty", "", "", 0, false},
		{"traversal", "https://github.com/owner/name/../evil/issues/1", "", 0, false},
	}
	for _, tc := range cases {
		repo, num, ok := parseSourceIssueURL(tc.source)
		if repo != tc.wantRepo || num != tc.wantNum || ok != tc.wantOK {
			t.Errorf("%s: got (%q,%d,%t), want (%q,%d,%t)", tc.name, repo, num, ok, tc.wantRepo, tc.wantNum, tc.wantOK)
		}
	}
}

func TestValidateClosureCheckpoint(t *testing.T) {
	t.Parallel()
	want := productionClosureCheckpoint{
		Version: productionClosureCheckpointVersion, RunID: "run-x", PublicationID: "pub-x",
	}
	valid := func(mut func(*productionClosureCheckpoint)) productionClosureCheckpoint {
		got := want
		mut(&got)
		return got
	}
	proposal := valid(func(c *productionClosureCheckpoint) {
		c.HasProposal = true
		c.InstanceID = "proposal-1"
		c.Origin = domain.ClosureFlagOriginProposeSite
	})
	if err := validateClosureCheckpoint(proposal, want); err != nil {
		t.Fatalf("valid proposal checkpoint rejected: %v", err)
	}
	var legacy productionClosureCheckpoint
	if err := json.Unmarshal([]byte(`{"version":"1","run_id":"run-x","publication_id":"pub-x","has_proposal":true,"instance_id":"proposal-1","origin":"propose_site"}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Reason != "" || validateClosureCheckpoint(legacy, want) != nil {
		t.Fatalf("legacy checkpoint rejected: %+v", legacy)
	}
	noProposal := valid(func(c *productionClosureCheckpoint) {})
	if err := validateClosureCheckpoint(noProposal, want); err != nil {
		t.Fatalf("valid no-proposal checkpoint rejected: %v", err)
	}
	bad := []productionClosureCheckpoint{
		valid(func(c *productionClosureCheckpoint) { c.Version = "9" }),
		valid(func(c *productionClosureCheckpoint) { c.RunID = "run-other" }),
		valid(func(c *productionClosureCheckpoint) { c.PublicationID = "pub-other" }),
		// HasProposal without an instance id.
		valid(func(c *productionClosureCheckpoint) { c.HasProposal = true }),
		// An instance id without HasProposal.
		valid(func(c *productionClosureCheckpoint) { c.InstanceID = "proposal-1" }),
		// A proposal with an invalid origin.
		valid(func(c *productionClosureCheckpoint) {
			c.HasProposal = true
			c.InstanceID = "proposal-1"
			c.Origin = "sideways"
		}),
		// An origin without a proposal.
		valid(func(c *productionClosureCheckpoint) { c.Origin = domain.ClosureFlagOriginProposeSite }),
	}
	for i, got := range bad {
		if err := validateClosureCheckpoint(got, want); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Errorf("case %d: got %v, want ErrParentKeyMismatch", i, err)
		}
	}
}

func TestClosureProposalBatchIDIsStableAndDistinct(t *testing.T) {
	t.Parallel()
	if first, second := closureProposalBatchID("run-a"), closureProposalBatchID("run-a"); first != second {
		t.Fatal("batch id is not stable for one run")
	}
	if closureProposalBatchID("run-a") == closureProposalBatchID("run-b") {
		t.Fatal("batch id collides across runs")
	}
	if got := string(closureProposalBatchID("run-a")); !strings.HasPrefix(got, "batch-source-issue-closure-") {
		t.Fatalf("batch id %q lacks the expected prefix", got)
	}
}

func TestProductionClosureCheckpointKeyBindsRunAndPublication(t *testing.T) {
	t.Parallel()
	base := productionClosureCheckpointKey("run-a", "pub-1")
	if base == productionClosureCheckpointKey("run-b", "pub-1") {
		t.Error("closure checkpoint key ignores the run")
	}
	if base == productionClosureCheckpointKey("run-a", "pub-2") {
		t.Error("closure checkpoint key ignores the publication invocation")
	}
}
