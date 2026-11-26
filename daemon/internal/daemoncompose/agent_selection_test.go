package daemoncompose

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
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

// TestAdapterProofPastItsBudgetSaysSo pins #3337: a proof the 3-minute
// budget ends says so, names the budget to the runtime calls it kills, and
// keeps the gate's error in the chain; a failure before the budget ends reads
// as it did.
func TestAdapterProofPastItsBudgetSaysSo(t *testing.T) {
	t.Parallel()
	input := claude.ConformanceInput{
		Task: claude.ConformanceTask{
			ReadNonce: "read-nonce", InstructionNonce: "instruction-nonce", Seed: "/unused",
			Base: domain.BaseRevision{Repo: "freeside-ai/conformance", RepositoryID: 1, BaseRef: "refs/heads/main", BaseSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		},
		InvocationID: "adapter-fixture", Identity: "claude-fixture", Volumes: conformanceVolumes("fixture-volume"),
	}
	errKilled := errors.New("container start: signal: killed")
	t.Run("budget ended", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			var cause error
			gate := conformanceGateFunc(func(ctx context.Context, _ ward.HandoffSpec) (*ward.HandoffResult, error) {
				<-ctx.Done()
				cause = context.Cause(ctx)
				return nil, errKilled
			})
			_, err := runAdapterConformance(t.Context(), gate, input, []string{"api.anthropic.com:443"})
			if !errors.Is(cause, errAdapterProofBudget) {
				t.Fatalf("runtime calls see cause %v, want the adapter proof budget", cause)
			}
			if !errors.Is(err, errKilled) {
				t.Fatalf("error %v lost the gate's error", err)
			}
			if !strings.HasPrefix(fmt.Sprint(err), "adapter proof ran 3m0s and passed its 3m0s budget; ") ||
				!strings.Contains(fmt.Sprint(err), "the daemon retries the proof after a backoff") {
				t.Fatalf("error %q does not say the proof ran past its budget", err)
			}
			if !adapterProofOverran(err) || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error %v is not an overrun with the deadline kept out of its chain", err)
			}
			// A release that did not land fails the pass as itself.
			if joined := errors.Join(err, errors.New("release the reservation")); adapterProofOverran(joined) {
				t.Fatalf("error %v reads as an overrun alone", joined)
			}
		})
	})
	t.Run("failed in time", func(t *testing.T) {
		t.Parallel()
		gate := conformanceGateFunc(func(context.Context, ward.HandoffSpec) (*ward.HandoffResult, error) {
			return nil, errKilled
		})
		_, err := runAdapterConformance(t.Context(), gate, input, nil)
		if !errors.Is(err, errKilled) || err.Error() != errKilled.Error() || adapterProofOverran(err) {
			t.Fatalf("error %v, want the gate's error unchanged", err)
		}
	})
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
			// Only the timeout is an overrun the daemon retries.
			err := proveAdapterOnCandidates(t.Context(), st, []engine.RoleAgent{{Adapter: adapter}}, noRecheck, func(engine.RoleAgent) (domain.LaunchCapabilitySet, error) { return observed, runErr })
			if overran := kind == "timeout"; adapterProofOverran(err) != overran || (!overran && err != nil) {
				t.Fatalf("proof = %v, want an overrun only for a timeout", err)
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
	if err := proveAdapterOnCandidates(t.Context(), st, []engine.RoleAgent{{Adapter: codex}}, noRecheck, func(engine.RoleAgent) (domain.LaunchCapabilitySet, error) {
		t.Fatal("launched another adapter")
		return nil, nil
	}); err != nil {
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

// TestAdapterProofRetriesOnlyAnOverrunEverywhere pins #3574: a proof that
// ran out its budget on every subscription records its failure and returns
// an overrun for the daemon to retry; one that also failed on its merits on
// any subscription records its failure and returns nil, since no retry would
// change it.
func TestAdapterProofRetriesOnlyAnOverrunEverywhere(t *testing.T) {
	t.Parallel()
	adapter, err := agentbaseline.ClaudeWardAdapter()
	if err != nil {
		t.Fatal(err)
	}
	overran := &adapterProofOverranError{elapsed: adapterProofBudget, err: errors.New("signal: killed")}
	type result struct {
		seen domain.LaunchCapabilitySet
		err  error
	}
	for name, tc := range map[string]struct {
		results     []result
		wantOverran bool
	}{
		"every subscription overran": {results: []result{{err: overran}, {err: overran}}, wantOverran: true},
		"one overran, one saw too little": {
			results: []result{{err: overran}, {seen: domain.NewLaunchCapabilitySet(domain.LaunchCapReadTools)}},
		},
		"one failed, one overran": {results: []result{{err: errors.New("handoff failed")}, {err: overran}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
			if err := recordTestClaudeAdapterConformance(t.Context(), st, time.Now()); err != nil {
				t.Fatal(err)
			}
			candidates := make([]engine.RoleAgent, len(tc.results))
			for i := range candidates {
				candidates[i] = engine.RoleAgent{Adapter: adapter}
			}
			next := 0
			err := proveAdapterOnCandidates(t.Context(), st, candidates, noRecheck, func(engine.RoleAgent) (domain.LaunchCapabilitySet, error) {
				r := tc.results[next]
				next++
				return r.seen, r.err
			})
			if adapterProofOverran(err) != tc.wantOverran || (!tc.wantOverran && err != nil) {
				t.Fatalf("proof = %v, want overran %t", err, tc.wantOverran)
			}
			if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
				record, _, err := tx.LatestAdapterConformance(t.Context(), adapter.Digest)
				if err == nil && record.Outcome != domain.ConformanceFailed {
					t.Fatalf("record = %+v, want the failure to revoke the earlier pass", record)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// noRecheck passes every candidate's launch-time recheck: these candidates
// are not in the store.
func noRecheck(engine.RoleAgent) error { return nil }

// TestAdapterProofSkipsAnExpiringCredential: a subscription whose observable
// expiry falls inside the proof's budget is not launched. The proof goes to
// the next subscription, or records nothing when there is none.
func TestAdapterProofSkipsAnExpiringCredential(t *testing.T) {
	t.Parallel()
	adapter, err := agentbaseline.ClaudeWardAdapter()
	if err != nil {
		t.Fatal(err)
	}
	expiring := engine.RoleAgent{Adapter: adapter}
	expiring.Enrollment.ID, expiring.Enrollment.AuthMethod = "claude-oauth/claude_code", domain.AuthMethodOAuth
	expiring.Generation.EnrollmentID = expiring.Enrollment.ID
	expiring.Generation.TokenExpiry = new(time.Now().Add(adapterProofBudget / 2))
	healthy := engine.RoleAgent{Adapter: adapter}
	healthy.Enrollment.ID, healthy.Generation.EnrollmentID = "claude-healthy/claude_code", "claude-healthy/claude_code"
	for name, tc := range map[string]struct {
		candidates []engine.RoleAgent
		launched   []domain.ClientEnrollmentID
	}{
		"next subscription": {candidates: []engine.RoleAgent{expiring, healthy}, launched: []domain.ClientEnrollmentID{healthy.Enrollment.ID}},
		"no other":          {candidates: []engine.RoleAgent{expiring}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
			var launched []domain.ClientEnrollmentID
			if err := proveAdapterOnCandidates(t.Context(), st, tc.candidates, noRecheck, func(agent engine.RoleAgent) (domain.LaunchCapabilitySet, error) {
				launched = append(launched, agent.Enrollment.ID)
				return testObservedClaudeCapabilities(), nil
			}); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(launched, tc.launched) {
				t.Fatalf("launched %v, want %v", launched, tc.launched)
			}
			if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
				_, found, err := tx.LatestAdapterConformance(t.Context(), adapter.Digest)
				if found != (tc.launched != nil) {
					t.Fatalf("record found = %t after launching %v", found, launched)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestAdapterRecoveryPendingStopsTheProof: a proof whose cleanup is still
// pending records its failure and returns at once, so recovery runs before
// any other subscription launches a proof.
func TestAdapterRecoveryPendingStopsTheProof(t *testing.T) {
	t.Parallel()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	adapter, err := agentbaseline.ClaudeWardAdapter()
	if err != nil {
		t.Fatal(err)
	}
	if err := recordTestClaudeAdapterConformance(t.Context(), st, time.Now()); err != nil {
		t.Fatal(err)
	}
	first, second := engine.RoleAgent{Adapter: adapter}, engine.RoleAgent{Adapter: adapter}
	first.Enrollment.ID, second.Enrollment.ID = "claude-first/claude_code", "claude-second/claude_code"
	first.Generation.EnrollmentID, second.Generation.EnrollmentID = first.Enrollment.ID, second.Enrollment.ID
	var launched []domain.ClientEnrollmentID
	pending := &adapterRecoveryPending{errors.New("runtime still present")}
	err = proveAdapterOnCandidates(t.Context(), st, []engine.RoleAgent{first, second}, noRecheck, func(agent engine.RoleAgent) (domain.LaunchCapabilitySet, error) {
		launched = append(launched, agent.Enrollment.ID)
		return nil, pending
	})
	if !errors.Is(err, pending) {
		t.Fatalf("recovery pending = %v, want it returned", err)
	}
	if !slices.Equal(launched, []domain.ClientEnrollmentID{first.Enrollment.ID}) {
		t.Fatalf("launched %v, want only the first candidate", launched)
	}
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		record, _, err := tx.LatestAdapterConformance(t.Context(), adapter.Digest)
		if err == nil && record.Outcome != domain.ConformanceFailed {
			t.Fatalf("recovery pending recorded %+v, want failed", record)
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
