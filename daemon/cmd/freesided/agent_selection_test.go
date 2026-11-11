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

// adapterProofEnv is an adopted lineup whose implementer the adapter proof
// resolves, with the store it was adopted into.
type adapterProofEnv struct {
	st          *store.Store
	tree        agenttree.Tree
	selection   engine.AgentSelection
	implementer engine.RoleAgent
	prompts     map[domain.RoleName]domain.Digest
}

// withAdapterProof adopts the fixture lineup and runs fn against it. The
// store holds no adapter conformance record.
func withAdapterProof(t *testing.T, fn func(adapterProofEnv)) {
	t.Helper()
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
	f.withStore(t, func(st *store.Store) {
		implementer, err := engine.ResolveRole(t.Context(), st, tree, domain.RoleImplementer)
		if err != nil {
			t.Fatal(err)
		}
		fn(adapterProofEnv{st: st, tree: tree, selection: adapterProofSelection(t, tree), implementer: implementer, prompts: prompts})
	})
}

// adapterProofSelection is the selection the daemon would load from tree.
func adapterProofSelection(t *testing.T, tree agenttree.Tree) engine.AgentSelection {
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

// latestAdapterRecord is the Claude ward adapter's latest conformance
// record, and false when it has none.
func latestAdapterRecord(t *testing.T, st *store.Store) (domain.AdapterConformance, bool) {
	t.Helper()
	adapter, err := agentbaseline.ClaudeWardAdapter()
	if err != nil {
		t.Fatal(err)
	}
	var record domain.AdapterConformance
	var found bool
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		record, found, err = tx.LatestAdapterConformance(t.Context(), adapter.Digest)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return record, found
}

// proofResult is what a scripted proof observes on one subscription.
type proofResult struct {
	observed domain.LaunchCapabilitySet
	err      error
}

// scriptedProof returns an observe that answers each enrollment's proof from
// results, and the enrollments it launched on, in order. A launch on an
// enrollment results does not name fails the test.
func scriptedProof(t *testing.T, results map[domain.ClientEnrollmentID]proofResult) (func(engine.RoleAgent) (domain.LaunchCapabilitySet, error), *[]domain.ClientEnrollmentID) {
	t.Helper()
	var launched []domain.ClientEnrollmentID
	return func(agent engine.RoleAgent) (domain.LaunchCapabilitySet, error) {
		launched = append(launched, agent.Enrollment.ID)
		result, ok := results[agent.Enrollment.ID]
		if !ok {
			t.Errorf("the proof launched on %s", agent.Enrollment.ID)
			return nil, errors.New("unexpected launch")
		}
		return result.observed, result.err
	}, &launched
}

// reducedClaudeCapabilities is a proof that saw every capability but one.
func reducedClaudeCapabilities() domain.LaunchCapabilitySet {
	return slices.DeleteFunc(testObservedClaudeCapabilities(), func(c domain.LaunchCapability) bool { return c == domain.LaunchCapMutationTools })
}

// TestMarkedImplementerLeavesTheAdapterRecord: when the lineup implementer's
// generation is marked and no other subscription could run its line, the
// conformance pass cannot run, so it records nothing about the adapter and
// the earlier pass stands. With a healthy second subscription the proof runs
// there instead (#2971), and a writer on it stays admitted. Re-enrollment
// readmits the marked roles and puts the proof back on the implementer
// without a restart. A resolution failure other than the mark still records
// failed and tries no other subscription.
func TestMarkedImplementerLeavesTheAdapterRecord(t *testing.T) {
	t.Parallel()
	withAdapterProof(t, func(env adapterProofEnv) {
		ctx, st, tree, source := t.Context(), env.st, env.tree, env.implementer
		at := time.Now().UTC()
		if err := recordTestClaudeAdapterConformance(ctx, st, at); err != nil {
			t.Fatal(err)
		}
		passed, _ := latestAdapterRecord(t, st)
		markStartupGeneration(t, st, source.Enrollment.ID)

		launchNothing, launched := scriptedProof(t, nil)
		if err := daemoncompose.ProveSelectedClaudeAdapter(ctx, st, env.selection, launchNothing); err != nil {
			t.Fatalf("marked implementer pass = %v, want nil so the follower counts no failure", err)
		}
		if got, _ := latestAdapterRecord(t, st); len(*launched) != 0 || !reflect.DeepEqual(got, passed) {
			t.Fatalf("marked implementer with no substitute launched %v and recorded %+v, want the earlier %+v", *launched, got, passed)
		}

		// The specifier moves to a healthy second subscription, which the
		// proof now runs on. The implementer and remediator stay on the
		// marked generation.
		second := enrollSecondSubscription(t, st, source, at)
		for i := range tree.Lineup {
			if domain.RoleName(tree.Lineup[i].Key) == domain.RoleSpecifier {
				tree.Lineup[i].Selection.Names.Enrollment = string(second.ID)
			}
		}
		selection := adapterProofSelection(t, tree)
		observe, launched := scriptedProof(t, map[domain.ClientEnrollmentID]proofResult{
			second.ID: {observed: testObservedClaudeCapabilities()},
		})
		if err := daemoncompose.ProveSelectedClaudeAdapter(ctx, st, selection, observe); err != nil {
			t.Fatal(err)
		}
		reproved, _ := latestAdapterRecord(t, st)
		if !slices.Equal(*launched, []domain.ClientEnrollmentID{second.ID}) ||
			reproved.Outcome != domain.ConformancePassed || reproved.Generation <= passed.Generation {
			t.Fatalf("marked implementer with a substitute launched %v and recorded %+v, want a pass on %s", *launched, reproved, second.ID)
		}
		if _, err := selection.CheckRole(ctx, st, domain.RoleSpecifier, env.prompts[domain.RoleSpecifier], at); err != nil {
			t.Fatalf("writer on the healthy second subscription: %v", err)
		}
		if _, err := selection.CheckRole(ctx, st, domain.RoleImplementer, env.prompts[domain.RoleImplementer], at); !errors.Is(err, domain.ErrGenerationIntegrityMarked) {
			t.Fatalf("marked implementer = %v", err)
		}

		reenrollStartupRole(t, st, source.Enrollment)
		if _, err := selection.CheckRole(ctx, st, domain.RoleImplementer, env.prompts[domain.RoleImplementer], at); err != nil {
			t.Fatalf("same selection after re-enrollment: %v", err)
		}
		observe, launched = scriptedProof(t, map[domain.ClientEnrollmentID]proofResult{
			source.Enrollment.ID: {observed: testObservedClaudeCapabilities()},
		})
		if err := daemoncompose.ProveSelectedClaudeAdapter(ctx, st, selection, observe); err != nil {
			t.Fatal(err)
		}
		if got, _ := latestAdapterRecord(t, st); !slices.Equal(*launched, []domain.ClientEnrollmentID{source.Enrollment.ID}) ||
			got.Outcome != domain.ConformancePassed || got.Generation <= reproved.Generation {
			t.Fatalf("proof after re-enrollment launched %v and recorded %+v", *launched, got)
		}

		// An implementer line naming no enrollment is a refusal other than
		// the mark, so the pass still records failed and launches nothing,
		// though the second subscription could run the line.
		moved := lineupSelection(t, tree, domain.RoleImplementer)
		for i := range tree.Lineup {
			if tree.Lineup[i].Selection.Names == moved.Names() {
				tree.Lineup[i].Selection.Names.Enrollment = "claude-absent/claude_code"
			}
		}
		launchNothing, launched = scriptedProof(t, nil)
		if err := daemoncompose.ProveSelectedClaudeAdapter(ctx, st, adapterProofSelection(t, tree), launchNothing); err != nil {
			t.Fatal(err)
		}
		if got, _ := latestAdapterRecord(t, st); len(*launched) != 0 || got.Outcome != domain.ConformanceFailed || got.ProvedCapabilities != nil {
			t.Fatalf("unresolvable implementer launched %v and recorded %+v, want failed", *launched, got)
		}
	})
}

// TestAdapterProofTriesAnotherSubscription: a proof that fails on the
// implementer's subscription, by an error or a reduced observation, runs
// again on another subscription that could run the implementer's line, and a
// full pass there records passed (#2971, case 1). The record then admits a
// writer on that subscription.
func TestAdapterProofTriesAnotherSubscription(t *testing.T) {
	t.Parallel()
	for name, implementerResult := range map[string]proofResult{
		"enrollment failure":    {err: errors.New("authentication refused")},
		"reduced observation":   {observed: reducedClaudeCapabilities()},
		"error after observing": {observed: testObservedClaudeCapabilities(), err: context.DeadlineExceeded},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			withAdapterProof(t, func(env adapterProofEnv) {
				ctx, st, tree, at := t.Context(), env.st, env.tree, time.Now().UTC()
				second := enrollSecondSubscription(t, st, env.implementer, at)
				for i := range tree.Lineup {
					if domain.RoleName(tree.Lineup[i].Key) == domain.RoleSpecifier {
						tree.Lineup[i].Selection.Names.Enrollment = string(second.ID)
					}
				}
				selection := adapterProofSelection(t, tree)
				observe, launched := scriptedProof(t, map[domain.ClientEnrollmentID]proofResult{
					env.implementer.Enrollment.ID: implementerResult,
					second.ID:                     {observed: testObservedClaudeCapabilities()},
				})
				if err := daemoncompose.ProveSelectedClaudeAdapter(ctx, st, selection, observe); err != nil {
					t.Fatal(err)
				}
				got, _ := latestAdapterRecord(t, st)
				if want := []domain.ClientEnrollmentID{env.implementer.Enrollment.ID, second.ID}; !slices.Equal(*launched, want) {
					t.Fatalf("launched %v, want %v", *launched, want)
				}
				if got.Outcome != domain.ConformancePassed || !slices.Equal(got.ProvedCapabilities, testObservedClaudeCapabilities()) {
					t.Fatalf("record = %+v, want the second subscription's full pass", got)
				}
				if _, err := selection.CheckRole(ctx, st, domain.RoleSpecifier, env.prompts[domain.RoleSpecifier], at); err != nil {
					t.Fatalf("writer on the second subscription: %v", err)
				}
			})
		})
	}
}

// TestAdapterFailureOnEverySubscriptionRecordsFailed: when the proof falls
// short on every subscription, the pass records the first proof's
// observation, so a failed adapter still revokes an older pass, and it
// launches each subscription once.
func TestAdapterFailureOnEverySubscriptionRecordsFailed(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		first, second proofResult
		outcome       domain.ConformanceOutcome
		proved        domain.LaunchCapabilitySet
	}{
		"both error": {
			first: proofResult{err: errors.New("handoff failed")}, second: proofResult{err: errors.New("handoff failed")},
			outcome: domain.ConformanceFailed,
		},
		"the first observation is kept": {
			first: proofResult{observed: reducedClaudeCapabilities()}, second: proofResult{err: errors.New("handoff failed")},
			outcome: domain.ConformancePassed, proved: reducedClaudeCapabilities(),
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			withAdapterProof(t, func(env adapterProofEnv) {
				ctx, st, at := t.Context(), env.st, time.Now().UTC()
				if err := recordTestClaudeAdapterConformance(ctx, st, at); err != nil {
					t.Fatal(err)
				}
				second := enrollSecondSubscription(t, st, env.implementer, at)
				observe, launched := scriptedProof(t, map[domain.ClientEnrollmentID]proofResult{
					env.implementer.Enrollment.ID: tc.first, second.ID: tc.second,
				})
				if err := daemoncompose.ProveSelectedClaudeAdapter(ctx, st, env.selection, observe); err != nil {
					t.Fatal(err)
				}
				if want := []domain.ClientEnrollmentID{env.implementer.Enrollment.ID, second.ID}; !slices.Equal(*launched, want) {
					t.Fatalf("launched %v, want each subscription once: %v", *launched, want)
				}
				got, _ := latestAdapterRecord(t, st)
				if got.Outcome != tc.outcome || !slices.Equal(got.ProvedCapabilities, tc.proved) {
					t.Fatalf("record = %+v, want %s with %v", got, tc.outcome, tc.proved)
				}
				if _, err := env.selection.CheckRole(ctx, st, domain.RoleImplementer, env.prompts[domain.RoleImplementer], at); !errors.Is(err, engine.ErrAgentNotAdmissible) {
					t.Fatalf("writer after a failed adapter = %v, want refused", err)
				}
			})
		})
	}
}

// TestSubstituteMarkedDuringAnEarlierProofIsNotLaunched: the substitutes are
// read before the first proof, which may run for minutes. A substitute whose
// generation is marked while an earlier proof runs is re-read and not
// launched, so the pass records the earlier proof's failure.
func TestSubstituteMarkedDuringAnEarlierProofIsNotLaunched(t *testing.T) {
	t.Parallel()
	withAdapterProof(t, func(env adapterProofEnv) {
		ctx, st, at := t.Context(), env.st, time.Now().UTC()
		second := enrollSecondSubscription(t, st, env.implementer, at)
		var launched []domain.ClientEnrollmentID
		observe := func(agent engine.RoleAgent) (domain.LaunchCapabilitySet, error) {
			launched = append(launched, agent.Enrollment.ID)
			if agent.Enrollment.ID == env.implementer.Enrollment.ID {
				markStartupGeneration(t, st, second.ID)
				return nil, errors.New("authentication refused")
			}
			return testObservedClaudeCapabilities(), nil
		}
		if err := daemoncompose.ProveSelectedClaudeAdapter(ctx, st, env.selection, observe); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(launched, []domain.ClientEnrollmentID{env.implementer.Enrollment.ID}) {
			t.Fatalf("launched %v, want only the implementer", launched)
		}
		if got, _ := latestAdapterRecord(t, st); got.Outcome != domain.ConformanceFailed {
			t.Fatalf("record = %+v, want the implementer's failure", got)
		}
	})
}

// TestMarkedImplementerProvesOnAnotherSubscription: a marked implementer no
// longer leaves the adapter without a usable record when another
// subscription could run its line: after an interrupted proof's failed
// record (case 2), or with no record yet for a new adapter digest (case 3),
// the pass proves on the healthy subscription and records passed.
func TestMarkedImplementerProvesOnAnotherSubscription(t *testing.T) {
	t.Parallel()
	for name, prior := range map[string]func(context.Context, *store.Store) error{
		"interrupted proof": func(ctx context.Context, st *store.Store) error {
			// What recoverAdapterConformance records for a leftover intent.
			return daemoncompose.RecordObservedAdapterConformance(ctx, st, nil, errors.New("interrupted adapter proof"), time.Now().UTC())
		},
		"new adapter digest": nil,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			withAdapterProof(t, func(env adapterProofEnv) {
				ctx, st, at := t.Context(), env.st, time.Now().UTC()
				if prior != nil {
					if err := prior(ctx, st); err != nil {
						t.Fatal(err)
					}
				}
				second := enrollSecondSubscription(t, st, env.implementer, at)
				markStartupGeneration(t, st, env.implementer.Enrollment.ID)
				scripted, launched := scriptedProof(t, map[domain.ClientEnrollmentID]proofResult{
					second.ID: {observed: testObservedClaudeCapabilities()},
				})
				adapters, err := wardstore.New(st)
				if err != nil {
					t.Fatal(err)
				}
				leaser := &daemoncompose.AdapterConformanceLeaser{Leaser: adapters.Leaser, Store: st}
				// The proof's invocation mounts the substitute's own
				// generation, as observeClaudeAdapter binds it.
				observe := func(agent engine.RoleAgent) (domain.LaunchCapabilitySet, error) {
					const holder domain.InvocationID = "adapter-conformance-substitute"
					release := leaser.Bind(holder, agent)
					defer release()
					if volume, err := leaser.AuthStoreVolume(ctx, agent.Identity.ID, holder); err != nil || volume != "second-auth" {
						t.Errorf("substitute proof mounts %q, %v; want the second subscription's volume", volume, err)
					}
					return scripted(agent)
				}
				if err := daemoncompose.ProveSelectedClaudeAdapter(ctx, st, env.selection, observe); err != nil {
					t.Fatal(err)
				}
				got, found := latestAdapterRecord(t, st)
				if !slices.Equal(*launched, []domain.ClientEnrollmentID{second.ID}) || !found || got.Outcome != domain.ConformancePassed {
					t.Fatalf("launched %v and recorded %+v (found %t), want a pass on %s", *launched, got, found, second.ID)
				}
			})
		})
	}
}
