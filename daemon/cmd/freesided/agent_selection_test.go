package main

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/wardstore"
)

func testObservedClaudeCapabilities() domain.LaunchCapabilitySet {
	return domain.NewLaunchCapabilitySet(domain.LaunchCapReadTools, domain.LaunchCapMutationTools,
		domain.LaunchCapInstructionDelivery, domain.LaunchCapStructuredOutput, domain.LaunchCapContextSeverance, domain.LaunchCapRouteStoreContract)
}

// These fixtures test admission, not a real harness. Their proof is explicit
// so changing an adapter declaration cannot silently grow fixture coverage.
func recordTestClaudeAdapterConformance(ctx context.Context, st *store.Store, at time.Time) error {
	return daemoncompose.RecordObservedAdapterConformance(ctx, st, testObservedClaudeCapabilities(), nil, at)
}

func TestObservedAdapterCoverageHoldsWriterAdmission(t *testing.T) {
	t.Parallel()
	f := newAuthAdoptFixture(t)
	_, patch, err := f.run(t, f.args())
	if err != nil {
		t.Fatal(err)
	}
	checkout, commit := commitAdoptedPatch(t, patch)
	selection, err := daemoncompose.LoadAgentSelection(t.Context(), daemoncompose.ClaudeDriverConfig{AgentTreeCheckout: checkout, AgentTreeCommit: commit, ProviderEndpoints: []string{"api.anthropic.com:443"}})
	if err != nil {
		t.Fatal(err)
	}
	selection.AttemptBudget = time.Hour
	f.withStore(t, func(st *store.Store) {
		agent, err := engine.ResolveRole(t.Context(), st, selection.Tree, domain.RoleImplementer)
		if err != nil {
			t.Fatal(err)
		}
		for _, runErr := range []error{nil, errors.New("handoff failed")} {
			observed := slices.DeleteFunc(testObservedClaudeCapabilities(), func(c domain.LaunchCapability) bool { return c == domain.LaunchCapMutationTools })
			if err := daemoncompose.RecordObservedAdapterConformance(t.Context(), st, observed, runErr, time.Now()); err != nil {
				t.Fatal(err)
			}
			_, err = selection.CheckRole(t.Context(), st, domain.RoleImplementer, agent.Line.PromptDigest, time.Now())
			var refusal *engine.AgentAdmissionRefusal
			if !errors.As(err, &refusal) || refusal.Refusal.Check != domain.AdmissionCheckAgentLaunchCoverage {
				t.Fatalf("admission refusal = %v", err)
			}
		}
		adapters, err := wardstore.New(st)
		if err != nil {
			t.Fatal(err)
		}
		leaser := &daemoncompose.AdapterConformanceLeaser{Leaser: adapters.Leaser, Store: st}
		const holder domain.InvocationID = "conformance-fixture-holder"
		release := leaser.Bind(holder, agent)
		defer release()
		volume, err := leaser.AuthStoreVolume(t.Context(), agent.Identity.ID, holder)
		if err != nil || volume != agent.Generation.AuthStoreVolume {
			t.Fatalf("generation binding = %q, %v", volume, err)
		}
		if _, err := leaser.AuthStoreVolume(t.Context(), "another-identity", holder); err == nil {
			t.Fatal("cross-identity binding accepted")
		}
		other, err := engine.ResolveRole(t.Context(), st, selection.Tree, domain.RoleReviewer)
		if err != nil {
			t.Fatal(err)
		}
		tampered := agent
		tampered.Enrollment.ID = other.Enrollment.ID
		undo := leaser.Bind("tampered-conformance-holder", tampered)
		defer undo()
		if _, err := leaser.AuthStoreVolume(t.Context(), agent.Identity.ID, "tampered-conformance-holder"); err == nil {
			t.Fatal("reconstructed enrollment from another identity accepted")
		}
		if _, ok := any(leaser).(interface {
			AcquireRead(context.Context, domain.AuthIdentityID, domain.InvocationID, time.Time, time.Time) (domain.AuthStoreReadHold, error)
		}); !ok {
			t.Fatal("generation binding lost shared read holds")
		}
	})
}

// TestMarkedImplementerLeavesTheAdapterRecord: when the lineup implementer's
// generation is marked, the conformance pass cannot run, so it records
// nothing about the adapter. The earlier pass stands, a writer on a healthy
// second subscription stays admitted, and re-enrollment readmits the marked
// roles and the proof without a restart. A resolution failure other than the
// mark still records failed.
func TestMarkedImplementerLeavesTheAdapterRecord(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newAuthAdoptFixture(t)
	_, patch, err := f.run(t, f.args())
	if err != nil {
		t.Fatal(err)
	}
	tree := loadAdoptedPatch(t, patch)
	prompts := map[domain.RoleName]domain.Digest{}
	for _, line := range tree.Lineup {
		prompts[domain.RoleName(line.Key)] = line.Selection.PromptDigest
	}
	adapter, err := agentbaseline.ClaudeWardAdapter()
	if err != nil {
		t.Fatal(err)
	}
	latest := func(st *store.Store) domain.AdapterConformance {
		t.Helper()
		var record domain.AdapterConformance
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			var found bool
			var err error
			record, found, err = tx.LatestAdapterConformance(ctx, adapter.Digest)
			if err == nil && !found {
				t.Fatal("the adapter has no record")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return record
	}
	selectionOf := func(tree agenttree.Tree) engine.AgentSelection {
		t.Helper()
		revision, err := treeRevision(tree)
		if err != nil {
			t.Fatal(err)
		}
		return engine.AgentSelection{
			Tree: tree, LineupRevision: revision, Launch: agentbaseline.RoleLaunch,
			EffectiveEgress: []string{"api.anthropic.com:443"}, AttemptBudget: time.Hour,
		}
	}
	f.withStore(t, func(st *store.Store) {
		at := time.Now().UTC()
		if err := recordTestClaudeAdapterConformance(ctx, st, at); err != nil {
			t.Fatal(err)
		}
		passed := latest(st)
		source, err := engine.ResolveRole(ctx, st, tree, domain.RoleImplementer)
		if err != nil {
			t.Fatal(err)
		}
		// The specifier moves to a healthy second subscription. The
		// implementer and remediator stay on the generation that will be
		// marked.
		second := enrollSecondSubscription(t, st, source, at)
		for i := range tree.Lineup {
			if domain.RoleName(tree.Lineup[i].Key) == domain.RoleSpecifier {
				tree.Lineup[i].Selection.Names.Enrollment = string(second.ID)
			}
		}
		selection := selectionOf(tree)
		markStartupGeneration(t, st, source.Enrollment.ID)

		launchMarked := func(engine.RoleAgent) (domain.LaunchCapabilitySet, error) {
			t.Fatal("the proof launched on a marked implementer")
			return nil, nil
		}
		if err := daemoncompose.ProveSelectedClaudeAdapter(ctx, st, selection, launchMarked); err != nil {
			t.Fatalf("marked implementer pass = %v, want nil so the follower counts no failure", err)
		}
		if got := latest(st); !reflect.DeepEqual(got, passed) {
			t.Fatalf("marked implementer pass wrote a record: %+v, want the earlier %+v", got, passed)
		}
		if _, err := selection.CheckRole(ctx, st, domain.RoleSpecifier, prompts[domain.RoleSpecifier], at); err != nil {
			t.Fatalf("writer on the healthy second subscription: %v", err)
		}
		if _, err := selection.CheckRole(ctx, st, domain.RoleImplementer, prompts[domain.RoleImplementer], at); !errors.Is(err, domain.ErrGenerationIntegrityMarked) {
			t.Fatalf("marked implementer = %v", err)
		}

		reenrollStartupRole(t, st, source.Enrollment)
		if _, err := selection.CheckRole(ctx, st, domain.RoleImplementer, prompts[domain.RoleImplementer], at); err != nil {
			t.Fatalf("same selection after re-enrollment: %v", err)
		}
		var proved domain.ClientEnrollmentID
		if err := daemoncompose.ProveSelectedClaudeAdapter(ctx, st, selection, func(agent engine.RoleAgent) (domain.LaunchCapabilitySet, error) {
			proved = agent.Enrollment.ID
			return testObservedClaudeCapabilities(), nil
		}); err != nil {
			t.Fatal(err)
		}
		if got := latest(st); proved != source.Enrollment.ID || got.Outcome != domain.ConformancePassed || got.Generation <= passed.Generation {
			t.Fatalf("proof after re-enrollment ran on %q and recorded %+v", proved, got)
		}

		// An implementer line naming no enrollment is a refusal other than
		// the mark, so the pass still records failed.
		moved := lineupSelection(t, tree, domain.RoleImplementer)
		for i := range tree.Lineup {
			if tree.Lineup[i].Selection.Names == moved.Names() {
				tree.Lineup[i].Selection.Names.Enrollment = "claude-absent/claude_code"
			}
		}
		unresolved := selectionOf(tree)
		if err := daemoncompose.ProveSelectedClaudeAdapter(ctx, st, unresolved, launchMarked); err != nil {
			t.Fatal(err)
		}
		if got := latest(st); got.Outcome != domain.ConformanceFailed || got.ProvedCapabilities != nil {
			t.Fatalf("unresolvable implementer recorded %+v, want failed", got)
		}
	})
}
