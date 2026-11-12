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
// The caller records nothing when no subscription could run the proof
// (proveAdapterOnCandidates).
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

// adapterProofBudget is how long one adapter proof may run. A credential
// that expires inside it is not launched (proveAdapterOnCandidates).
const adapterProofBudget = 3 * time.Minute

func runAdapterConformance(ctx context.Context, gate adapterConformanceGate, input claude.ConformanceInput, authorities []string) (domain.LaunchCapabilitySet, error) {
	ctx, cancel := context.WithTimeout(ctx, adapterProofBudget)
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
// implementer's subscription or a substitute for it. A selection that cannot
// load records failed; ProveSelectedClaudeAdapter decides the rest.
func proveClaudeAdapter(ctx context.Context, st *store.Store, backend *ward.Backend, cfg ClaudeDriverConfig, leaser *AdapterConformanceLeaser) error {
	selection, err := LoadAgentSelection(ctx, cfg)
	if err != nil {
		return RecordObservedAdapterConformance(ctx, st, nil, err, time.Now().UTC())
	}
	return ProveSelectedClaudeAdapter(ctx, st, *selection, func(agent engine.RoleAgent) (domain.LaunchCapabilitySet, error) {
		return observeClaudeAdapter(ctx, backend, cfg, leaser, agent)
	})
}

// ProveSelectedClaudeAdapter proves the Claude ward adapter through observe,
// on the lineup implementer's subscription first and then on each other
// subscription that could run the implementer's line
// (engine.AgentSelection.AdapterProofSubstitutes), until one proof passes
// (proveAdapterOnCandidates). The adapter record is keyed by adapter digest,
// not enrollment, so a proof that fails for its subscription's sake, such as
// an expired, refused, or marked credential, would otherwise refuse every
// Claude writer on every subscription. A pass on any subscription observes
// the same adapter digest on the same image (#2971).
//
// A marked implementer is skipped, not refused: a mark holds only the
// admissions that would run on its own generation (the owner decision on
// #1740), as CheckWriterRoles holds at startup. Any other failure to resolve
// the implementer or select its launch records failed and tries no
// substitute: startup already holds the whole lineup for those, so the
// record holds nothing wider.
func ProveSelectedClaudeAdapter(ctx context.Context, st *store.Store, selection engine.AgentSelection, observe func(engine.RoleAgent) (domain.LaunchCapabilitySet, error)) error {
	var candidates []engine.RoleAgent
	implementer, err := engine.ResolveRole(ctx, st, selection.Tree, domain.RoleImplementer)
	if err == nil {
		implementer, err = selectForLaunch(selection, domain.RoleImplementer, implementer)
	}
	switch {
	case errors.Is(err, domain.ErrGenerationIntegrityMarked):
		slog.Warn("Claude adapter conformance skips the implementer's subscription: its generation is marked",
			"role", domain.RoleImplementer, "error", err)
	case err != nil:
		slog.Warn("cannot resolve implementer for Claude adapter conformance", "error", err)
		return RecordObservedAdapterConformance(ctx, st, nil, err, time.Now().UTC())
	default:
		candidates = append(candidates, implementer)
	}
	substitutes, err := selection.AdapterProofSubstitutes(ctx, st, domain.RoleImplementer)
	if err != nil {
		return fmt.Errorf("adapter proof substitutes: %w", err)
	}
	for _, substitute := range substitutes {
		// A substitute's launch must resolve as the implementer's would on
		// its enrollment; one that does not cannot run the proof.
		selected, err := selectForLaunch(selection, domain.RoleImplementer, substitute)
		if err != nil {
			slog.Info("Claude adapter conformance skips a subscription whose launch does not resolve",
				"enrollment", substitute.Enrollment.ID, "error", err)
			continue
		}
		candidates = append(candidates, selected)
	}
	recheck := func(agent engine.RoleAgent) error { return recheckProofCandidate(ctx, st, agent) }
	return proveAdapterOnCandidates(ctx, st, candidates, recheck, observe)
}

// errProofCandidateStale is a candidate whose credential changed after the
// candidate list was read. It no longer qualifies, so its proof does not run.
var errProofCandidateStale = errors.New("the subscription changed after the adapter proof's candidates were read")

// recheckProofCandidate re-reads, just before a candidate's proof launches,
// what its selection rested on that can change while earlier proofs run: its
// identity is still enabled and bound to its enrollment, the generation it
// would mount is still the enrollment's current one, and that generation is
// unmarked. Each earlier proof may run for adapterProofBudget, long enough
// for the credential-integrity probe to mark a later candidate. A change
// returns errProofCandidateStale; anything else is a store failure.
func recheckProofCandidate(ctx context.Context, st *store.Store, agent engine.RoleAgent) error {
	return st.Read(ctx, func(tx *store.ReadTx) error {
		enrollment, err := tx.GetClientEnrollment(ctx, agent.Enrollment.ID)
		if errors.Is(err, store.ErrNotFound) {
			return errProofCandidateStale
		}
		if err != nil {
			return err
		}
		identity, err := tx.GetAuthIdentity(ctx, enrollment.AuthIdentityID)
		if err != nil {
			return err
		}
		if identity.ID != agent.Identity.ID || !identity.Enabled ||
			domain.ValidateEnrollmentIdentityBinding(identity, enrollment) != nil {
			return errProofCandidateStale
		}
		generation, err := tx.CurrentEnrollmentGeneration(ctx, enrollment.ID)
		if err != nil {
			return err
		}
		if generation.Ordinal != agent.Generation.Ordinal {
			return errProofCandidateStale
		}
		err = tx.RequireGenerationUnmarked(ctx, enrollment.ID, generation.Ordinal)
		if errors.Is(err, domain.ErrGenerationIntegrityMarked) {
			return fmt.Errorf("%w: %w", errProofCandidateStale, err)
		}
		return err
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

// proveAdapterOnCandidates runs the Claude ward adapter proof on each
// candidate in order and stops at the first full pass: observe returned no
// error and saw every capability the adapter declares. It records that pass.
// It does not launch a candidate whose launch picks another adapter, whose
// credential, where its auth method observes expiry, would not outlive
// adapterProofBudget, or that recheck finds stale (errProofCandidateStale);
// any other recheck error is returned. A canceled ctx launches no further
// candidate.
//
// When no proof passes, it records the first proof that ran, so an adapter
// that fails everywhere still revokes an older pass and a lone subscription
// records what it records today. When none ran, it records nothing, returns
// nil, and the adapter's last record stands. An adapterRecoveryPending error
// records its proof and returns at once, launching no further candidate.
//
// Substitutes copy the implementer's line, so they bind its client unless a
// recorded catalog serves another client's model; an implementer on another
// adapter therefore leaves nothing here to launch, as before #2971.
func proveAdapterOnCandidates(
	ctx context.Context, st *store.Store, candidates []engine.RoleAgent,
	recheck func(engine.RoleAgent) error, observe func(engine.RoleAgent) (domain.LaunchCapabilitySet, error),
) error {
	adapter, err := agentbaseline.ClaudeWardAdapter()
	if err != nil {
		return err
	}
	var (
		ran        bool
		firstSeen  domain.LaunchCapabilitySet
		firstErr   error
		firstAgent domain.ClientEnrollmentID
	)
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			break
		}
		if candidate.Adapter.Digest != adapter.Digest {
			slog.Info("Claude adapter conformance skips a subscription whose launch uses another adapter",
				"enrollment", candidate.Enrollment.ID)
			continue
		}
		if err := domain.ValidateGenerationExpiryMargin(candidate.Enrollment, candidate.Generation,
			time.Now().Add(adapterProofBudget), 0); err != nil {
			slog.Warn("Claude adapter conformance skips a subscription whose credential does not cover the proof",
				"enrollment", candidate.Enrollment.ID, "error", err)
			continue
		}
		if err := recheck(candidate); errors.Is(err, errProofCandidateStale) {
			slog.Warn("Claude adapter conformance skips a subscription that changed while earlier proofs ran",
				"enrollment", candidate.Enrollment.ID, "error", err)
			continue
		} else if err != nil {
			return fmt.Errorf("recheck adapter proof candidate %s: %w", candidate.Enrollment.ID, err)
		}
		observed, runErr := observe(candidate)
		var pending *adapterRecoveryPending
		if errors.As(runErr, &pending) {
			if err := RecordObservedAdapterConformance(ctx, st, observed, runErr, time.Now().UTC()); err != nil {
				return err
			}
			slog.Warn("Claude adapter conformance failed; writer admission held",
				"enrollment", candidate.Enrollment.ID, "error", runErr)
			return runErr
		}
		missing := domain.MissingLaunchCapabilities(domain.NewLaunchCapabilitySet(observed...), adapter.LaunchCapabilities)
		if runErr == nil && len(missing) == 0 {
			return RecordObservedAdapterConformance(ctx, st, observed, nil, time.Now().UTC())
		}
		slog.Warn("Claude adapter conformance fell short on one subscription",
			"enrollment", candidate.Enrollment.ID, "missing", missing, "error", runErr)
		if !ran {
			ran, firstSeen, firstErr, firstAgent = true, observed, runErr, candidate.Enrollment.ID
		}
	}
	if !ran {
		slog.Warn("skipping Claude adapter conformance: no subscription could run the proof; the adapter's last record stands")
		return nil
	}
	if err := RecordObservedAdapterConformance(ctx, st, firstSeen, firstErr, time.Now().UTC()); err != nil {
		return err
	}
	slog.Warn("Claude adapter conformance failed on every subscription; writer admission held",
		"enrollment", firstAgent, "error", firstErr)
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
