package daemoncompose

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
)

// The audit records are written out, not derived, so this is what notices a
// call adapter changing under its record: a changed adapter is a launch
// nobody audited, and the record must be re-earned, not regenerated. Each
// record is held to its own adapter, so the v2 audit never stands in for v1.
func TestJudgmentCallAuditsNameTheCallAdapters(t *testing.T) {
	t.Parallel()
	v1, err := agentbaseline.ClaudeCallAdapterV1()
	if err != nil {
		t.Fatal(err)
	}
	v2, err := agentbaseline.ClaudeCallAdapter()
	if err != nil {
		t.Fatal(err)
	}
	audited := []struct {
		adapter domain.AdapterFragment
		on      string
	}{{v1, "2026-09-09"}, {v2, "2026-10-11"}}
	if len(judgmentCallAudits) != len(audited) {
		t.Fatalf("%d audit records for %d call adapters", len(judgmentCallAudits), len(audited))
	}
	for i, want := range audited {
		audit, adapter := judgmentCallAudits[i], want.adapter
		if err := audit.Validate(); err != nil {
			t.Fatal(err)
		}
		if audit.AdapterDigest != adapter.Digest || audit.HarnessBuild != adapter.HarnessBuild || audit.AuditedOn != want.on {
			t.Fatalf("audit covers %s %q on %s, the call adapter %s is %s %q, audited %s",
				audit.AdapterDigest, audit.HarnessBuild, audit.AuditedOn,
				adapter.AdapterBuild, adapter.Digest, adapter.HarnessBuild, want.on)
		}
		if got := judgmentCallAuditFor(adapter.Digest); got == nil || *got != audit {
			t.Fatalf("audit for %s = %+v, want %+v", adapter.AdapterBuild, got, audit)
		}
	}
	if got := judgmentCallAuditFor(domain.Digest("sha256:" + strings.Repeat("0", 64))); got != nil {
		t.Fatalf("audit for an adapter nobody audited = %+v", got)
	}
}

func judgmentTestSites() []inference.Site {
	limits := inference.Limits{Calls: 100, ComputeUnits: 10_000_000, AttentionItems: 100, Starvation: 10 * time.Hour}
	budget := inference.Budget{
		Window: 24 * time.Hour, Site: limits, Project: limits, Global: limits,
		MaxCallsPerRoot: 100, MaxStarvationPerRoot: 10 * time.Hour,
	}
	return JudgmentSites(budget)
}

// A judgment line's adapter is the tree's audited call adapter that sends
// its effort. An adapter no audit names is no candidate, so a tree whose
// only call adapter is unaudited admits nothing; the earlier adapter sends
// only harness_default; and two audited adapters that both send the effort
// refuse as ambiguous rather than leave the choice to tree order.
func TestJudgmentCallAdapterIsTheAuditedOneThatSendsTheEffort(t *testing.T) {
	t.Parallel()
	v1, err := agentbaseline.ClaudeCallAdapterV1()
	if err != nil {
		t.Fatal(err)
	}
	v2, err := agentbaseline.ClaudeCallAdapter()
	if err != nil {
		t.Fatal(err)
	}
	// The v2 launch under a build id no audit covers.
	unaudited := v2
	unaudited.AdapterBuild = "claude_code_call_v3"
	if unaudited.Digest, err = unaudited.ComputeDigest(); err != nil {
		t.Fatal(err)
	}
	ward, err := agentbaseline.ClaudeWardAdapter()
	if err != nil {
		t.Fatal(err)
	}
	treeOf := func(adapters ...domain.AdapterFragment) agenttree.Tree {
		var tree agenttree.Tree
		for _, adapter := range adapters {
			tree.Adapters = append(tree.Adapters, agenttree.Adapter{Name: adapter.AdapterBuild, Fragment: adapter})
		}
		return tree
	}
	harnessDefault := string(domain.EffortHarnessDefault)
	for name, tc := range map[string]struct {
		tree   agenttree.Tree
		effort string
		want   domain.Digest
		err    error
	}{
		"v2 at harness_default":               {treeOf(ward, v2), harnessDefault, v2.Digest, nil},
		"v2 at high":                          {treeOf(ward, v2), "high", v2.Digest, nil},
		"v1 at harness_default":               {treeOf(ward, v1), harnessDefault, v1.Digest, nil},
		"v1 at high":                          {treeOf(ward, v1), "high", "", domain.ErrNoAdapterForLaunch},
		"v1 and v2 at high":                   {treeOf(v1, v2), "high", v2.Digest, nil},
		"v1 and v2 at harness_default":        {treeOf(v1, v2), harnessDefault, "", domain.ErrAmbiguousAdapterForLaunch},
		"an unaudited adapter only":           {treeOf(ward, unaudited), harnessDefault, "", domain.ErrNoAdapterForLaunch},
		"an unaudited adapter beside v2":      {treeOf(unaudited, v2), harnessDefault, v2.Digest, nil},
		"the ward adapter is no call adapter": {treeOf(ward), harnessDefault, "", domain.ErrNoAdapterForLaunch},
	} {
		adapter, err := judgmentCallAdapter(tc.tree, domain.HarnessClientClaudeCode, tc.effort)
		if tc.err != nil {
			if !errors.Is(err, tc.err) {
				t.Fatalf("%s = %+v, %v; want %v", name, adapter, err, tc.err)
			}
			continue
		}
		if err != nil || adapter.Digest != tc.want {
			t.Fatalf("%s = %s, %v; want %s", name, adapter.Digest, err, tc.want)
		}
	}
}
