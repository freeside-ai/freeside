package main

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec/claude"
	"github.com/freeside-ai/freeside/daemon/internal/store"
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
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
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
	return recordObservedAdapterConformance(ctx, st, testObservedClaudeCapabilities(), nil, at)
}

func TestRecordObservedAdapterConformance(t *testing.T) {
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	provedAt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	observed := slices.DeleteFunc(testObservedClaudeCapabilities(), func(c domain.LaunchCapability) bool { return c == domain.LaunchCapMutationTools })
	observed = append(observed, domain.LaunchCapAuxiliaryInferenceControl)
	if err := recordObservedAdapterConformance(t.Context(), st, observed, nil, provedAt); err != nil {
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
		if err := recordObservedAdapterConformance(ctx, st, testObservedClaudeCapabilities(), context.DeadlineExceeded, provedAt); err != nil {
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
	f := newAuthAdoptFixture(t)
	_, patch, err := f.run(t, f.args())
	if err != nil {
		t.Fatal(err)
	}
	checkout, commit := commitAdoptedPatch(t, patch)
	selection, err := loadAgentSelection(t.Context(), claudeDriverConfig{AgentTreeCheckout: checkout, AgentTreeCommit: commit, ProviderEndpoints: []string{"api.anthropic.com:443"}})
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
			if err := recordObservedAdapterConformance(t.Context(), st, observed, runErr, time.Now()); err != nil {
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
		leaser := &adapterConformanceLeaser{Leaser: adapters.Leaser, store: st}
		const holder domain.InvocationID = "conformance-fixture-holder"
		release := leaser.bind(holder, agent)
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
		undo := leaser.bind("tampered-conformance-holder", tampered)
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
