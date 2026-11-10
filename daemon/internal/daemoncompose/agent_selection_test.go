package daemoncompose

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
	"github.com/freeside-ai/freeside/daemon/internal/exec/claude"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
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
