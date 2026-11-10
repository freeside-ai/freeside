package daemoncompose

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/atomicfile"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec/claude"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
	"github.com/freeside-ai/freeside/daemon/internal/wardstore"
)

// Adapter proofs have no task admission. Bind only their unpredictable
// invocation IDs to the resolved enrollment generation; ordinary invocations
// still use wardstore's admission-bound resolver. Embedding preserves both the
// mutation-lease and shared-read interfaces and their store-backed checks.
type AdapterConformanceLeaser struct {
	*wardstore.Leaser
	Store    *store.Store
	mu       sync.RWMutex
	bindings map[domain.InvocationID]engine.RoleAgent
}

func (l *AdapterConformanceLeaser) AuthStoreVolume(ctx context.Context, identity domain.AuthIdentityID, holder domain.InvocationID) (string, error) {
	l.mu.RLock()
	agent, bound := l.bindings[holder]
	l.mu.RUnlock()
	if !bound {
		return l.Leaser.AuthStoreVolume(ctx, identity, holder)
	}
	if agent.Identity.ID != identity {
		return "", fmt.Errorf("adapter conformance holder belongs to another identity")
	}
	var volume string
	err := l.Store.Read(ctx, func(tx *store.ReadTx) error {
		enrollment, err := tx.GetClientEnrollment(ctx, agent.Enrollment.ID)
		if err != nil {
			return err
		}
		if enrollment.AuthIdentityID != identity {
			return fmt.Errorf("adapter conformance enrollment belongs to another identity")
		}
		generation, err := tx.GetEnrollmentGeneration(ctx, agent.Enrollment.ID, agent.Generation.Ordinal)
		if err != nil {
			return err
		}
		volume = generation.AuthStoreVolume
		return nil
	})
	return volume, err
}

func (l *AdapterConformanceLeaser) Bind(id domain.InvocationID, agent engine.RoleAgent) func() {
	l.mu.Lock()
	if l.bindings == nil {
		l.bindings = make(map[domain.InvocationID]engine.RoleAgent)
	}
	l.bindings[id] = agent
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		delete(l.bindings, id)
		l.mu.Unlock()
	}
}

// RecordObservedAdapterConformance uses the declaration only as a ceiling.
// Failed observations supersede older passes, including declaration-only ones.
// The caller does not record a proof that could not run because the
// implementer's generation is marked (ProveSelectedClaudeAdapter).
func RecordObservedAdapterConformance(ctx context.Context, st *store.Store, observed domain.LaunchCapabilitySet, runErr error, provedAt time.Time) error {
	adapter, err := agentbaseline.ClaudeWardAdapter()
	if err != nil {
		return err
	}
	input := domain.AdapterConformanceInput{AdapterDigest: adapter.Digest, Outcome: domain.ConformanceFailed, ProvedAt: provedAt}
	if runErr == nil {
		input.Outcome = domain.ConformancePassed
		for _, capability := range observed {
			if adapter.LaunchCapabilities.Has(capability) {
				input.ProvedCapabilities = append(input.ProvedCapabilities, capability)
			}
		}
	}
	record, err := domain.NewAdapterConformance(input)
	if err != nil {
		return err
	}
	// A timeout must still revoke the prior pass.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return st.WriteInternal(writeCtx, func(tx *store.InternalTx) error {
		_, err := tx.RecordAdapterConformance(writeCtx, record)
		return err
	})
}

type adapterConformanceGate interface {
	Handoff(context.Context, ward.HandoffSpec) (*ward.HandoffResult, error)
}

func runAdapterConformance(ctx context.Context, gate adapterConformanceGate, input claude.ConformanceInput, authorities []string) (domain.LaunchCapabilitySet, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	spec, err := claude.ConformanceHandoff(ctx, input)
	if err != nil {
		return nil, err
	}
	result, err := gate.Handoff(ctx, spec)
	if result != nil && result.ExportDir != "" {
		defer func() { _ = os.RemoveAll(result.ExportDir) }()
	}
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return claude.JudgeConformance(result, input.Task, input.InvocationID, authorities)
}

// proveClaudeAdapter proves the Claude ward adapter on the lineup
// implementer's subscription. A selection that cannot load records failed;
// ProveSelectedClaudeAdapter decides the rest.
func proveClaudeAdapter(ctx context.Context, st *store.Store, backend *ward.Backend, cfg ClaudeDriverConfig, leaser *AdapterConformanceLeaser) error {
	selection, err := LoadAgentSelection(ctx, cfg)
	if err != nil {
		return RecordObservedAdapterConformance(ctx, st, nil, err, time.Now().UTC())
	}
	return ProveSelectedClaudeAdapter(ctx, st, *selection, func(agent engine.RoleAgent) (domain.LaunchCapabilitySet, error) {
		return observeClaudeAdapter(ctx, backend, cfg, leaser, agent)
	})
}

// ProveSelectedClaudeAdapter resolves the implementer the proof launches on
// and proves the adapter through observe. A resolution or launch-selection
// failure records failed, with one exception: when the implementer's
// generation carries an integrity mark, the proof cannot run, so it records
// nothing, returns nil, and the adapter's last record stands.
//
// A mark holds only the admissions that would run on its own generation (the
// owner decision on #1740), and each launch's admission refuses it until
// re-enrollment. The adapter record is keyed by adapter digest, not
// enrollment, so a failed record here would refuse every Claude writer on
// every subscription; CheckWriterRoles makes the same exception at startup.
// Every other refusal still records failed: startup already holds the whole
// lineup for those, so the record holds nothing wider.
func ProveSelectedClaudeAdapter(ctx context.Context, st *store.Store, selection engine.AgentSelection, observe func(engine.RoleAgent) (domain.LaunchCapabilitySet, error)) error {
	agent, err := engine.ResolveRole(ctx, st, selection.Tree, domain.RoleImplementer)
	if errors.Is(err, domain.ErrGenerationIntegrityMarked) {
		slog.Warn("skipping Claude adapter conformance: the implementer's generation is marked; the adapter's last record stands",
			"role", domain.RoleImplementer, "error", err)
		return nil
	}
	if err == nil {
		agent, err = selectForLaunch(selection, domain.RoleImplementer, agent)
	}
	if err != nil {
		slog.Warn("cannot resolve implementer for Claude adapter conformance", "error", err)
		return RecordObservedAdapterConformance(ctx, st, nil, err, time.Now().UTC())
	}
	return proveResolvedClaudeAdapter(ctx, st, agent.Adapter.Digest, func() (domain.LaunchCapabilitySet, error) {
		return observe(agent)
	})
}

// selectForLaunch resolves the selection the role's resolved line names
// under the adapter the role's launch shape picks for its enrollment's
// client and effort, as stage admission does.
func selectForLaunch(selection engine.AgentSelection, role domain.RoleName, agent engine.RoleAgent) (engine.RoleAgent, error) {
	launch, err := selection.Launch(role)
	if err != nil {
		return engine.RoleAgent{}, err
	}
	adapter, err := selection.Tree.AdapterForLaunch(agent.Enrollment.HarnessClient, launch, agent.Line.Names.Effort)
	if err != nil {
		return engine.RoleAgent{}, err
	}
	return agent.Select(adapter)
}

func proveResolvedClaudeAdapter(ctx context.Context, st *store.Store, digest domain.Digest, observe func() (domain.LaunchCapabilitySet, error)) error {
	adapter, err := agentbaseline.ClaudeWardAdapter()
	if err != nil {
		return err
	}
	if digest != adapter.Digest {
		slog.Info("skipping Claude adapter conformance: implementer uses another adapter")
		return nil
	}
	observed, err := observe()
	if recordErr := RecordObservedAdapterConformance(ctx, st, observed, err, time.Now().UTC()); recordErr != nil {
		return recordErr
	}
	if err != nil {
		slog.Warn("Claude adapter conformance failed; writer admission held", "error", err)
	}
	var pending *adapterRecoveryPending
	if errors.As(err, &pending) {
		return err
	}
	return nil
}

func observeClaudeAdapter(ctx context.Context, backend *ward.Backend, cfg ClaudeDriverConfig, leaser *AdapterConformanceLeaser, agent engine.RoleAgent) (domain.LaunchCapabilitySet, error) {
	model, nativeEffort := agent.Selection.LaunchValues(agent.Adapter.ClientKind)
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	id := domain.InvocationID("adapter-conformance-" + hex.EncodeToString(nonce[:]))
	release := leaser.Bind(id, agent)
	defer release()
	if err := bindRigInvocationResources(cfg.StateDir, cfg.RigTokenFile, id); err != nil {
		return nil, err
	}
	seed, err := os.MkdirTemp(cfg.SeedRoot, ".adapter-conformance-")
	if err != nil {
		return nil, err
	}
	retained := false
	defer func() {
		if !retained {
			_ = os.RemoveAll(seed)
		}
	}()
	task, err := claude.NewConformanceTask(ctx, seed, domain.BaseRevision{
		Repo: cfg.Repo, RepositoryID: cfg.RepositoryID, BaseRef: cfg.BaseRef, BaseSHA: cfg.BaseSHA,
	})
	if err != nil {
		return nil, err
	}
	image, _ := backend.ConformanceIdentity()
	input := claude.ConformanceInput{
		Task: task, InvocationID: id, Image: domain.ImageRef(image), Identity: agent.Identity.ID,
		RouteModelID: model, NativeEffort: nativeEffort, Volumes: leaser,
	}
	spec, err := claude.ConformanceHandoff(ctx, input)
	if err != nil {
		return nil, err
	}
	intent := adapterConformanceIntent{Version: 1, InvocationID: id, Agent: agent, Spec: spec}
	path := filepath.Join(cfg.StateDir, "adapter-conformance", string(id)+".json")
	body, err := json.Marshal(intent)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := atomicfile.WriteFile(path, body, 0o600); err != nil {
		return nil, err
	}
	retained = true
	observed, runErr := runAdapterConformance(ctx, backend, input, agent.Route.InferenceAuthorities)
	if err := retireAdapterConformance(ctx, backend, intent, path); err != nil {
		return nil, &adapterRecoveryPending{errors.Join(runErr, err)}
	}
	return observed, runErr
}

type adapterConformanceIntent struct {
	Version      int
	InvocationID domain.InvocationID
	Agent        engine.RoleAgent
	Spec         ward.HandoffSpec
}

type adapterRecoveryPending struct{ error }

type adapterRecoveryGate interface {
	HandoffStarted(context.Context, string) (bool, error)
	HandoffQuiescent(context.Context, string) error
	Recover(context.Context, string, ward.HandoffSpec) (*ward.RecoveryResult, error)
}

// Keep the intent and seed until ward proves runtime absence. Recovery owns
// cleanup failures and process crashes; it never converts recovered output
// into a new proof, since its original observation window may have expired.
func retireAdapterConformance(ctx context.Context, backend adapterRecoveryGate, intent adapterConformanceIntent, path string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
	defer cancel()
	started, err := backend.HandoffStarted(ctx, intent.Spec.RunID)
	if err != nil {
		return err
	}
	if started {
		if err := backend.HandoffQuiescent(ctx, intent.Spec.RunID); err != nil {
			recovered, err := backend.Recover(ctx, intent.Spec.RunID, intent.Spec)
			if err != nil {
				return err
			}
			if recovered != nil && recovered.ExportDir != "" {
				_ = os.RemoveAll(recovered.ExportDir)
			}
			if err := backend.HandoffQuiescent(ctx, intent.Spec.RunID); err != nil {
				return err
			}
		}
	}
	if err := os.RemoveAll(intent.Spec.Seed.SourceDir); err != nil {
		return err
	}
	return os.Remove(path)
}

func recoverAdapterConformance(ctx context.Context, st *store.Store, backend adapterRecoveryGate, cfg ClaudeDriverConfig, leaser *AdapterConformanceLeaser) error {
	root := filepath.Join(cfg.StateDir, "adapter-conformance")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		body, err := os.ReadFile(path) //nolint:gosec // daemon-owned recovery directory
		if err != nil {
			return err
		}
		var intent adapterConformanceIntent
		if err := json.Unmarshal(body, &intent); err != nil {
			return err
		}
		relative, err := filepath.Rel(cfg.SeedRoot, intent.Spec.Seed.SourceDir)
		if err != nil || !filepath.IsAbs(intent.Spec.Seed.SourceDir) || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) ||
			intent.Version != 1 || !strings.HasPrefix(string(intent.InvocationID), "adapter-conformance-") ||
			filepath.Base(string(intent.InvocationID)) != string(intent.InvocationID) || entry.Name() != string(intent.InvocationID)+".json" ||
			intent.Spec.RunID != claude.RunIDFor(intent.InvocationID) {
			return fmt.Errorf("invalid adapter conformance recovery record %s", path)
		}
		if err := RecordObservedAdapterConformance(ctx, st, nil, errors.New("interrupted adapter proof"), time.Now().UTC()); err != nil {
			return err
		}
		release := leaser.Bind(intent.InvocationID, intent.Agent)
		err = retireAdapterConformance(ctx, backend, intent, path)
		release()
		if err != nil {
			return fmt.Errorf("recover adapter conformance: %w", err)
		}
	}
	return nil
}
