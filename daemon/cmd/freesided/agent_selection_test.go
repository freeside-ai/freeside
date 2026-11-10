package main

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec/claude"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
	"github.com/freeside-ai/freeside/daemon/internal/wardstore"
)

func testObservedClaudeCapabilities() domain.LaunchCapabilitySet {
	return domain.NewLaunchCapabilitySet(domain.LaunchCapReadTools, domain.LaunchCapMutationTools,
		domain.LaunchCapInstructionDelivery, domain.LaunchCapStructuredOutput, domain.LaunchCapContextSeverance, domain.LaunchCapRouteStoreContract)
}

type conformanceGateFunc func(context.Context, ward.HandoffSpec) (*ward.HandoffResult, error)

func (f conformanceGateFunc) Handoff(ctx context.Context, spec ward.HandoffSpec) (*ward.HandoffResult, error) {
	return f(ctx, spec)
}

type conformanceVolumes string

func (v conformanceVolumes) AuthStoreVolume(context.Context, domain.AuthIdentityID, domain.InvocationID) (string, error) {
	return string(v), nil
}

func TestAdapterConformanceRunFailuresAndOtherAdapters(t *testing.T) {
	t.Parallel()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	adapter, err := agentbaseline.ClaudeWardAdapter()
	if err != nil {
		t.Fatal(err)
	}
	input := claude.ConformanceInput{
		Task: claude.ConformanceTask{
			ReadNonce: "read-nonce", InstructionNonce: "instruction-nonce", Seed: "/unused",
			Base: domain.BaseRevision{Repo: "freeside-ai/conformance", RepositoryID: 1, BaseRef: "refs/heads/main", BaseSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		},
		InvocationID: "adapter-fixture", Identity: "claude-fixture", Volumes: conformanceVolumes("fixture-volume"),
	}
	for _, kind := range []string{"handoff error", "timeout", "no transcript"} {
		t.Run(kind, func(t *testing.T) {
			if err := recordTestClaudeAdapterConformance(t.Context(), st, time.Now()); err != nil {
				t.Fatal(err)
			}
			exportDir := t.TempDir()
			gate := conformanceGateFunc(func(ctx context.Context, _ ward.HandoffSpec) (*ward.HandoffResult, error) {
				switch kind {
				case "timeout":
					<-ctx.Done()
					return nil, ctx.Err()
				case "no transcript":
					return &ward.HandoffResult{ExportDir: exportDir}, nil
				default:
					return nil, errors.New("handoff failed")
				}
			})
			var observed domain.LaunchCapabilitySet
			var runErr error
			synctest.Test(t, func(t *testing.T) {
				observed, runErr = runAdapterConformance(t.Context(), gate, input, []string{"api.anthropic.com:443"})
			})
			if runErr == nil {
				t.Fatal("incomplete run passed")
			}
			if err := proveResolvedClaudeAdapter(t.Context(), st, adapter.Digest, func() (domain.LaunchCapabilitySet, error) { return observed, runErr }); err != nil {
				t.Fatal(err)
			}
			if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
				record, _, err := tx.LatestAdapterConformance(t.Context(), adapter.Digest)
				if err == nil && (record.Outcome != domain.ConformanceFailed || record.ProvedCapabilities != nil) {
					t.Fatalf("incomplete run retained proof: %+v", record)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
	codex, err := agentbaseline.CodexReviewAdapter()
	if err != nil {
		t.Fatal(err)
	}
	if err := proveResolvedClaudeAdapter(t.Context(), st, codex.Digest, func() (domain.LaunchCapabilitySet, error) { t.Fatal("launched another adapter"); return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		_, found, err := tx.LatestAdapterConformance(t.Context(), codex.Digest)
		if found {
			t.Fatal("non-Claude adapter got a record")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// These fixtures test admission, not a real harness. Their proof is explicit
// so changing an adapter declaration cannot silently grow fixture coverage.
func recordTestClaudeAdapterConformance(ctx context.Context, st *store.Store, at time.Time) error {
	return RecordObservedAdapterConformance(ctx, st, testObservedClaudeCapabilities(), nil, at)
}

func TestRecordObservedAdapterConformance(t *testing.T) {
	t.Parallel()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	provedAt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	observed := slices.DeleteFunc(testObservedClaudeCapabilities(), func(c domain.LaunchCapability) bool { return c == domain.LaunchCapMutationTools })
	observed = append(observed, domain.LaunchCapAuxiliaryInferenceControl)
	if err := RecordObservedAdapterConformance(t.Context(), st, observed, nil, provedAt); err != nil {
		t.Fatal(err)
	}
	adapters, err := agentbaseline.Adapters()
	if err != nil {
		t.Fatal(err)
	}
	implementation, err := agentbaseline.Launch(domain.StageNameImplementation)
	if err != nil {
		t.Fatal(err)
	}
	for _, adapter := range adapters {
		var record domain.AdapterConformance
		var found bool
		if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
			var err error
			record, found, err = tx.LatestAdapterConformance(t.Context(), adapter.Digest)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if adapter.AdapterBuild != agentbaseline.ClaudeWardAdapterBuild {
			if found {
				t.Fatalf("review adapter %s got a record", adapter.AdapterBuild)
			}
			continue
		}
		if !found || record.Outcome != domain.ConformancePassed || record.ProvedCapabilities.Has(domain.LaunchCapMutationTools) || record.ProvedCapabilities.Has(domain.LaunchCapAuxiliaryInferenceControl) {
			t.Fatalf("unobserved or undeclared capability recorded: %+v", record)
		}
		if err := domain.ValidateAdapterLaunchCoverage(record, adapter.Digest, implementation); !errors.Is(err, domain.ErrLaunchCapabilityUnproved) {
			t.Fatalf("writer coverage = %v", err)
		}
		// Even a canceled pass must append a newer failed generation.
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := RecordObservedAdapterConformance(ctx, st, testObservedClaudeCapabilities(), context.DeadlineExceeded, provedAt); err != nil {
			t.Fatal(err)
		}
		if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
			latest, _, err := tx.LatestAdapterConformance(t.Context(), adapter.Digest)
			if err == nil && (latest.Generation <= record.Generation || latest.Outcome != domain.ConformanceFailed || latest.ProvedCapabilities != nil) {
				t.Fatalf("failed run did not supersede prior proof: %+v", latest)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestObservedAdapterCoverageHoldsWriterAdmission(t *testing.T) {
	t.Parallel()
	f := newAuthAdoptFixture(t)
	_, patch, err := f.run(t, f.args())
	if err != nil {
		t.Fatal(err)
	}
	checkout, commit := commitAdoptedPatch(t, patch)
	selection, err := LoadAgentSelection(t.Context(), ClaudeDriverConfig{AgentTreeCheckout: checkout, AgentTreeCommit: commit, ProviderEndpoints: []string{"api.anthropic.com:443"}})
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
			if err := RecordObservedAdapterConformance(t.Context(), st, observed, runErr, time.Now()); err != nil {
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
		leaser := &AdapterConformanceLeaser{Leaser: adapters.Leaser, Store: st}
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
		if err := ProveSelectedClaudeAdapter(ctx, st, selection, launchMarked); err != nil {
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
		if err := ProveSelectedClaudeAdapter(ctx, st, selection, func(agent engine.RoleAgent) (domain.LaunchCapabilitySet, error) {
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
		if err := ProveSelectedClaudeAdapter(ctx, st, unresolved, launchMarked); err != nil {
			t.Fatal(err)
		}
		if got := latest(st); got.Outcome != domain.ConformanceFailed || got.ProvedCapabilities != nil {
			t.Fatalf("unresolvable implementer recorded %+v, want failed", got)
		}
	})
}
