package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/observe"
	"github.com/freeside-ai/freeside/daemon/internal/observe/observedb"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
	"github.com/freeside-ai/freeside/daemon/internal/usagecollect"
)

type conclusionResponse struct {
	Observation domain.RunObservation `json:"observation"`
	Conclusion  domain.RunConclusion  `json:"conclusion"`
}
type snapshotWire struct {
	Snapshot                      observedb.Snapshot   `json:"snapshot"`
	AuthenticatedConclusion       domain.RunConclusion `json:"authenticated_conclusion"`
	LastStage                     string               `json:"last_stage"`
	ProducingInvocationID         domain.InvocationID  `json:"producing_invocation_id"`
	PublicationInvocationID       domain.InvocationID  `json:"publication_invocation_id"`
	PublicationReadyAuthenticated bool                 `json:"publication_ready_authenticated"`
}

func wireSnapshot(s observedb.Snapshot) snapshotWire {
	return snapshotWire{
		Snapshot: s, AuthenticatedConclusion: s.AuthenticatedConclusion,
		LastStage: s.LastStage, ProducingInvocationID: s.ProducingInvocationID,
		PublicationInvocationID:       s.PublicationInvocationID,
		PublicationReadyAuthenticated: s.PublicationReadyAuthenticated,
	}
}

func (w snapshotWire) observation() observedb.Snapshot {
	s := w.Snapshot
	s.AuthenticatedConclusion = w.AuthenticatedConclusion
	s.LastStage = w.LastStage
	s.ProducingInvocationID = w.ProducingInvocationID
	s.PublicationInvocationID = w.PublicationInvocationID
	s.PublicationReadyAuthenticated = w.PublicationReadyAuthenticated
	return s
}

type lockedObservation struct {
	observe.Source
	access storeAccess
}

type (
	directObservationOpener    func(context.Context, string, ...domain.Digest) (*observedb.Store, error)
	directObservationOpenerKey struct{}
)

func (s *lockedObservation) Close() error { return errors.Join(s.Source.Close(), s.access.Close()) }

type remoteObservation struct {
	client          *controlClient
	approvedRecipes []domain.Digest
}

func (s *remoteObservation) Close() error { s.client.Close(); return nil }
func (s *remoteObservation) ObserveRun(ctx context.Context, runID domain.RunID) (domain.RunObservation, error) {
	var result domain.RunObservation
	err := s.client.get(ctx, "/observe/runs/"+url.PathEscape(string(runID)), nil, &result)
	return result, err
}

func (s *remoteObservation) ObserveConclusion(ctx context.Context, runID domain.RunID) (domain.RunObservation, domain.RunConclusion, error) {
	var result conclusionResponse
	err := s.client.get(ctx, "/observe/runs/"+url.PathEscape(string(runID))+"/conclusion", nil, &result)
	return result.Observation, result.Conclusion, err
}

func (s *remoteObservation) ObserveSnapshot(ctx context.Context, runID domain.RunID) (observedb.Snapshot, error) {
	var result snapshotWire
	query := url.Values{}
	for _, recipe := range s.approvedRecipes {
		query.Add("approved_recipe", string(recipe))
	}
	err := s.client.get(ctx, "/observe/runs/"+url.PathEscape(string(runID))+"/snapshot", query, &result)
	return result.observation(), err
}

func openObservation(ctx context.Context, dbPath string, approvedRecipes ...domain.Digest) (observe.Source, error) {
	access, err := openStoreOrControl(dbPath)
	if err != nil {
		return nil, err
	}
	if access.client != nil {
		return &remoteObservation{client: access.client, approvedRecipes: approvedRecipes}, nil
	}
	open, ok := ctx.Value(directObservationOpenerKey{}).(directObservationOpener)
	if !ok {
		open = observedb.Open
	}
	source, err := open(ctx, dbPath, approvedRecipes...)
	if err != nil {
		_ = access.Close()
		return nil, err
	}
	return &lockedObservation{Source: source, access: access}, nil
}

// executionReportVersion names the GET /execution answer's shape; a reader
// refuses any other.
const executionReportVersion = "freeside-execution-v1"

// executionReport is what a daemon may run. The control socket serves it, not
// /health, because /health is unauthenticated and its schema is paired with
// generated clients; a real-work run reads it to prove prod runs without a
// driver.
type executionReport struct {
	Version     string        `json:"version"`
	Environment environment   `json:"environment"`
	Execution   executionMode `json:"execution"`
}

func decodeControlPayload[T any](body json.RawMessage) (T, error) {
	var value T
	err := strictjson.Decode(body, &value, strictjson.RejectInvalidUTF8, maxControlRequestBytes)
	return value, err
}

// pairingControlSocket is the control socket a start opens: the pairing
// control, and the facts its routes read from main's configuration.
type pairingControlSocket struct {
	pairing      *pairingControl
	repositoryID int64
	environment  environment
	execution    executionMode
	launch       *deputyLaunch
}

func (s pairingControlSocket) Serve() error { return s.pairing.Serve() }
func (s pairingControlSocket) Close() error { return s.pairing.Close() }

func (s pairingControlSocket) Configure(
	apiURL string, mint func(context.Context) (string, domain.PairingCode, error),
) {
	s.pairing.configure(apiURL, mint)
}

func (s pairingControlSocket) RegisterRoutes(in daemoncompose.ControlRouteInputs) {
	s.pairing.registerControlRoutes(
		s.pairing.mux, in.Store, in.Blobs, in.LocalBackupFiles, in.ApprovedRecipes, s.repositoryID,
		in.IntegrityProbe, in.Usage, s.environment, s.execution, in.Attention, s.launch, in.CommitWorkflowClosure, in.CommitTaskStop,
		in.ReleaseVerificationMemory, in.UpdateDrain, in.CancelRootImplementations, in.Running)
}

func (s pairingControlSocket) Publish() error { return s.pairing.publish() }

func (p *pairingControl) registerControlRoutes(
	mux *http.ServeMux, st *store.Store, blobs *signet.BlobStore, backupFiles *store.LocalBackupFiles,
	approved map[domain.Digest]bool, repositoryID int64, integrityProbe daemoncompose.CredentialIntegrityProbe,
	usage *usagecollect.Coordinator, env environment,
	execution executionMode, attention *signet.Service, launch *deputyLaunch,
	commitWorkflowClosure, commitTaskStop func(context.Context, domain.TaskID, func() error) error,
	releaseVerificationMemory func(context.Context, domain.TaskID, bool) (engine.VerificationMemoryRelease, error),
	updateDrain engine.UpdateDrain, cancelRootImplementations func(context.Context) error,
	running *engine.RunningWork,
) {
	p.registerNotifyTest(mux, st, attention)
	p.handleGet(mux, "/execution", func(context.Context, *http.Request) (any, error) {
		return executionReport{Version: executionReportVersion, Environment: env, Execution: execution}, nil
	})
	daemon := daemonStoreContext{
		store: st, blobs: blobs, backupFiles: backupFiles, dbPath: p.dbPath, approvedRecipes: approved,
		repositoryID: repositoryID, credentialIntegrityProbe: integrityProbe,
		commitWorkflowClosure: commitWorkflowClosure, commitTaskStop: commitTaskStop, usageCollection: usage,
		releaseVerificationMemory: releaseVerificationMemory, running: running,
	}
	observation := observedb.Borrow(st)
	p.handleGet(mux, "/observe/runs/{run_id}", func(ctx context.Context, r *http.Request) (any, error) {
		return observation.ObserveRun(ctx, domain.RunID(r.PathValue("run_id")))
	})
	p.handleGet(mux, "/observe/runs/{run_id}/conclusion", func(ctx context.Context, r *http.Request) (any, error) {
		view, conclusion, err := observation.ObserveConclusion(ctx, domain.RunID(r.PathValue("run_id")))
		return conclusionResponse{Observation: view, Conclusion: conclusion}, err
	})
	p.handleGet(mux, "/observe/runs/{run_id}/snapshot", func(ctx context.Context, r *http.Request) (any, error) {
		var recipes []domain.Digest
		requested := map[domain.Digest]bool{}
		for _, recipe := range r.URL.Query()["approved_recipe"] {
			recipes = append(recipes, domain.Digest(recipe))
			requested[domain.Digest(recipe)] = true
		}
		if err := daemon.requireApproved(ctx, requested); err != nil {
			return nil, err
		}
		view, err := observedb.Borrow(st, recipes...).ObserveSnapshot(ctx, domain.RunID(r.PathValue("run_id")))
		return wireSnapshot(view), err
	})
	withStore := func(ctx context.Context) context.Context {
		return context.WithValue(ctx, daemonStoreContextKey{}, daemon)
	}
	command := func(route string, run func(context.Context, []string, io.Writer, io.Writer) error) {
		p.handle(mux, route, func(ctx context.Context, body json.RawMessage) (any, error) {
			args, err := decodeControlPayload[[]string](body)
			if err != nil {
				return nil, err
			}
			var out bytes.Buffer
			runErr := run(withStore(ctx), args, &out, io.Discard)
			if out.Len() > 8<<20 {
				return nil, errors.New("command output exceeds control limit")
			}
			result := commandOutput{Output: out.String()}
			if runErr != nil {
				result.Error = runErr.Error()
				result.Kind = controlErrorKind(runErr)
			}
			return result, nil
		})
	}
	p.handleGet(mux, "/comprehension/measures", func(ctx context.Context, _ *http.Request) (any, error) {
		var out bytes.Buffer
		runErr := runComprehensionMeasures(withStore(ctx), []string{"-db", p.dbPath}, &out, io.Discard)
		result := commandOutput{Output: out.String()}
		if runErr != nil {
			result.Error = runErr.Error()
			result.Kind = controlErrorKind(runErr)
		}
		return result, nil
	})
	command("/comprehension/defects", runComprehensionCommand)
	command("/shadow-review/configuration-approvals", runApproveShadowReviewCommand)
	command("/usage-pools/limit", runSetPoolLimitCommand)
	command("/memory-budget", runSetMemoryBudgetCommand)
	command(releaseVerificationMemoryRoute, runReleaseVerificationMemoryCommand)
	command("/tasks/lines", runTaskLineCommand)
	command(routingPolicyRoute, runRoutingCommand)
	command("/auth/list", runAuthListCommand)
	command("/auth/usage/refresh", runAuthRefreshUsageCommand)
	command("/auth/usage/seed-exhausted", runAuthSeedExhaustedCommand)
	command("/auth/collector", runAuthSetCollectorCommand)
	command("/doctor", runDoctorCommand)
	p.handle(mux, "/snapshot", func(ctx context.Context, body json.RawMessage) (any, error) {
		return runSnapshotControl(ctx, st, body)
	})
	p.handle(mux, "/tasks/abandon", func(ctx context.Context, body json.RawMessage) (any, error) {
		cfg, err := decodeControlPayload[abandonCommandConfig](body)
		if err != nil {
			return nil, err
		}
		result, err := runAbandonCommand(withStore(ctx), cfg)
		if err != nil {
			result.Error = err.Error()
		}
		return result, nil
	})
	p.handle(mux, "/tasks/stop", func(ctx context.Context, body json.RawMessage) (any, error) {
		// The envelope's database path is already bound to this daemon.
		cfg, err := decodeControlPayload[stopCommandConfig](body)
		if err != nil {
			return nil, err
		}
		return stopTaskInDaemon(ctx, daemon, cfg.TaskID)
	})
	p.handle(mux, updateDrainRoute, func(ctx context.Context, body json.RawMessage) (any, error) {
		cfg, err := decodeControlPayload[drainCommandConfig](body)
		if err != nil {
			return nil, err
		}
		return updateDrainInDaemon(ctx, updateDrain, cancelRootImplementations, attention, cfg.Action)
	})
	p.handle(mux, "/tasks/reattempt", func(ctx context.Context, body json.RawMessage) (any, error) {
		cfg, err := decodeControlPayload[reattemptCommandConfig](body)
		if err != nil {
			return nil, err
		}
		return runReattemptCommand(withStore(ctx), cfg)
	})
	p.handle(mux, raiseItemRoute, func(ctx context.Context, body json.RawMessage) (any, error) {
		req, err := decodeControlPayload[raiseItemCommandConfig](body)
		if err != nil {
			return nil, err
		}
		return raiseItem(ctx, st, env, time.Now().UTC(), req)
	})
	p.handleGet(mux, "/tasks/{task_id}/latest-run", func(ctx context.Context, r *http.Request) (any, error) {
		return latestTaskRun(ctx, st, domain.TaskID(r.PathValue("task_id")))
	})
	p.handleGet(mux, "/tasks/{task_id}/record", func(ctx context.Context, r *http.Request) (any, error) {
		return readTaskRecordFrom(ctx, st, domain.TaskID(r.PathValue("task_id")))
	})
	p.handleGet(mux, "/tasks/{task_id}/conversations", func(ctx context.Context, r *http.Request) (any, error) {
		return readTaskConversationsFrom(ctx, st, domain.TaskID(r.PathValue("task_id")))
	})
	p.handleGet(mux, "/inspect", func(ctx context.Context, _ *http.Request) (any, error) {
		return readInspectIndexFrom(ctx, st)
	})
	p.handleGet(mux, "/system-health", func(ctx context.Context, _ *http.Request) (any, error) {
		return readSystemHealthFrom(ctx, st)
	})
	p.handleGet(mux, "/proposal-cards", func(ctx context.Context, _ *http.Request) (any, error) {
		return readProposalCardsFrom(ctx, st)
	})
	p.handle(mux, "/submissions", func(ctx context.Context, body json.RawMessage) (any, error) {
		req, err := decodeControlPayload[submitApplyRequest](body)
		if err != nil {
			return nil, err
		}
		return applyLaunchSubmission(ctx, st, blobs, req, launch)
	})
}
