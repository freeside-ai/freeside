package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/observe"
	"github.com/freeside-ai/freeside/daemon/internal/observe/observedb"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
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

func decodeControlPayload[T any](body json.RawMessage) (T, error) {
	var value T
	err := strictjson.Decode(body, &value, strictjson.RejectInvalidUTF8, maxControlRequestBytes)
	return value, err
}

func (p *pairingControl) registerControlRoutes(mux *http.ServeMux, st *store.Store, blobs *signet.BlobStore, backupFiles *store.LocalBackupFiles, approved map[domain.Digest]bool) {
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
		for _, recipe := range r.URL.Query()["approved_recipe"] {
			if domain.Digest(recipe) != domain.EffectProposalRecipeDigest && !approved[domain.Digest(recipe)] {
				return nil, fmt.Errorf("%w: recipe %s is not approved by the running daemon", domain.ErrUnapprovedRecipe, recipe)
			}
			recipes = append(recipes, domain.Digest(recipe))
		}
		view, err := observedb.Borrow(st, recipes...).ObserveSnapshot(ctx, domain.RunID(r.PathValue("run_id")))
		return wireSnapshot(view), err
	})
	withStore := func(ctx context.Context) context.Context {
		return context.WithValue(ctx, daemonStoreContextKey{}, daemonStoreContext{
			store: st, blobs: blobs, backupFiles: backupFiles, dbPath: p.dbPath, approvedRecipes: approved,
		})
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
	command("/doctor", runDoctorCommand)
	p.handle(mux, "/snapshot", func(ctx context.Context, body json.RawMessage) (any, error) {
		return runSnapshotControl(ctx, st, body)
	})
	p.handle(mux, "/tasks/abandon", func(ctx context.Context, body json.RawMessage) (any, error) {
		cfg, err := decodeControlPayload[abandonCommandConfig](body)
		if err != nil {
			return nil, err
		}
		return runAbandonCommand(withStore(ctx), cfg)
	})
	p.handle(mux, "/tasks/reattempt", func(ctx context.Context, body json.RawMessage) (any, error) {
		cfg, err := decodeControlPayload[reattemptCommandConfig](body)
		if err != nil {
			return nil, err
		}
		return runReattemptCommand(withStore(ctx), cfg)
	})
	p.handleGet(mux, "/tasks/{task_id}/latest-run", func(ctx context.Context, r *http.Request) (any, error) {
		return latestTaskRun(ctx, st, domain.TaskID(r.PathValue("task_id")))
	})
	p.handle(mux, "/submissions", func(ctx context.Context, body json.RawMessage) (any, error) {
		req, err := decodeControlPayload[submitApplyRequest](body)
		if err != nil {
			return nil, err
		}
		return applySubmission(ctx, st, blobs, req)
	})
}
