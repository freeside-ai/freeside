package main

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
type adapterConformanceLeaser struct {
	*wardstore.Leaser
	store    *store.Store
	mu       sync.RWMutex
	bindings map[domain.InvocationID]engine.RoleAgent
}

func (l *adapterConformanceLeaser) AuthStoreVolume(ctx context.Context, identity domain.AuthIdentityID, holder domain.InvocationID) (string, error) {
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
	err := l.store.Read(ctx, func(tx *store.ReadTx) error {
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

func (l *adapterConformanceLeaser) bind(id domain.InvocationID, agent engine.RoleAgent) func() {
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

// recordObservedAdapterConformance uses the declaration only as a ceiling.
// Failed observations supersede older passes, including declaration-only ones.
func recordObservedAdapterConformance(ctx context.Context, st *store.Store, observed domain.LaunchCapabilitySet, runErr error, provedAt time.Time) error {
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

func proveClaudeAdapter(ctx context.Context, st *store.Store, backend *ward.Backend, cfg claudeDriverConfig, leaser *adapterConformanceLeaser) error {
	selection, err := loadAgentSelection(ctx, cfg)
	if err != nil {
		return recordObservedAdapterConformance(ctx, st, nil, err, time.Now().UTC())
	}
	agent, err := engine.ResolveRole(ctx, st, selection.Tree, domain.RoleImplementer)
	if err != nil {
		slog.Warn("cannot resolve implementer for Claude adapter conformance", "error", err)
		return recordObservedAdapterConformance(ctx, st, nil, err, time.Now().UTC())
	}
	return proveResolvedClaudeAdapter(ctx, st, agent.Resolved.Adapter.Digest, func() (domain.LaunchCapabilitySet, error) {
		return observeClaudeAdapter(ctx, backend, cfg, leaser, agent)
	})
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
	if recordErr := recordObservedAdapterConformance(ctx, st, observed, err, time.Now().UTC()); recordErr != nil {
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

func observeClaudeAdapter(ctx context.Context, backend *ward.Backend, cfg claudeDriverConfig, leaser *adapterConformanceLeaser, agent engine.RoleAgent) (domain.LaunchCapabilitySet, error) {
	launch, err := domain.DeriveAgentLaunchSelection(agent.Resolved.Definition, agent.Resolved.Adapter, agent.Resolved.Offer)
	if err != nil {
		return nil, err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	id := domain.InvocationID("adapter-conformance-" + hex.EncodeToString(nonce[:]))
	release := leaser.bind(id, agent)
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
		RouteModelID: launch.RouteModelID, NativeEffort: launch.NativeEffort, Volumes: leaser,
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
	observed, runErr := runAdapterConformance(ctx, backend, input, agent.Resolved.Route.InferenceAuthorities)
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

func recoverAdapterConformance(ctx context.Context, st *store.Store, backend adapterRecoveryGate, cfg claudeDriverConfig, leaser *adapterConformanceLeaser) error {
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
		if err := recordObservedAdapterConformance(ctx, st, nil, errors.New("interrupted adapter proof"), time.Now().UTC()); err != nil {
			return err
		}
		release := leaser.bind(intent.InvocationID, intent.Agent)
		err = retireAdapterConformance(ctx, backend, intent, path)
		release()
		if err != nil {
			return fmt.Errorf("recover adapter conformance: %w", err)
		}
	}
	return nil
}
