package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	specifyfake "github.com/freeside-ai/freeside/daemon/internal/specify/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// stall holds every call that reaches it until release closes or the call's
// context ends, and closes reached on the first.
type stall struct {
	reached, release chan struct{}
	once             sync.Once
}

func newStall() *stall {
	return &stall{reached: make(chan struct{}), release: make(chan struct{})}
}

func (s *stall) hold(ctx context.Context) error {
	s.once.Do(func() { close(s.reached) })
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// returnsWhileStalled runs pass until the stalled call is in flight and
// requires the pass that started it to return while it still hangs. The
// engine's one pass dispatches every project's work, so a pass that returns
// is a pass that reaches every other project (#3215).
func returnsWhileStalled(t *testing.T, s *stall, pass func() error) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		returned := make(chan error, 1)
		go func() { returned <- pass() }()
		select {
		case err := <-returned:
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-s.reached:
				return
			default:
			}
			continue
		case <-deadline:
			t.Fatal("the pass waited on the stalled call")
		}
	}
}

// TestTriageCallRunsOutsideThePass: admission holds the specifier while its
// triage call hangs, rather than waiting for the call, and admits once the
// call's record lands.
func TestTriageCallRunsOutsideThePass(t *testing.T) {
	t.Parallel()
	f := newAgentAdmissionFixture(t)
	f.addTriagerLine(t)
	f.addEscalatedSpecifierOn(t, true, agentTestEscalatedModel)
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.RegisterProject(t.Context(), domain.Project{ID: f.run.ProjectID, Repo: "owner/repo", RepositoryID: 1})
	}); err != nil {
		t.Fatal(err)
	}
	s := newStall()
	f.engine.inference = triageClient(t, taskNamingDriverFunc(
		func(ctx context.Context, _ inference.Request, _ inference.Secret) (inference.Response, error) {
			if err := s.hold(ctx); err != nil {
				return inference.Response{}, err
			}
			return inference.Response{Output: []byte(`{"tier":"standard","reason":"A small fix."}`)}, nil
		}), 10)
	f.engine.logger = slog.New(slog.DiscardHandler)
	returnsWhileStalled(t, s, func() error {
		_, _, err := f.admitSpecifierOnce(t, "inv-specification-1")
		if !errors.Is(err, errSpecifierTriagePending) {
			return err
		}
		return nil
	})
	close(s.release)
	if _, err := f.admitSpecifier(t, "inv-specification-1"); err != nil {
		t.Fatal(err)
	}
	if record, _ := f.triageState(t); record == nil || record.Outcome != domain.TriageOutcomeAnswered {
		t.Fatalf("triage record = %+v", record)
	}
}

// TestDiscussionReplyRunsOutsideThePass: a pass returns while a discussion's
// model call hangs, and a later pass reports the reply the call produced.
func TestDiscussionReplyRunsOutsideThePass(t *testing.T) {
	t.Parallel()
	s := newStall()
	f := newAttentionDiscussionFixture(t, domain.AttentionExecutionFailure, taskNamingDriverFunc(
		func(ctx context.Context, _ inference.Request, _ inference.Secret) (inference.Response, error) {
			if err := s.hold(ctx); err != nil {
				return inference.Response{}, err
			}
			return inference.Response{Output: []byte(`{"reply":"The decision set is unchanged."}`), ComputeUnits: 1}, nil
		}))
	f.submit(t)
	e := f.engine(t, true)
	returnsWhileStalled(t, s, func() error {
		_, err := e.Reconcile(t.Context())
		return err
	})
	close(s.release)
	if result, err := reconcileSettled(t, e); err != nil || result.ResultsAccepted != 1 {
		t.Fatalf("Reconcile = %+v, %v", result, err)
	}
	f.assertReply(t, "The decision set is unchanged.", []domain.Action{domain.ActionDiscuss, domain.ActionStop})
}

// TestResearchFetchRunsOutsideThePass: a pass returns while a specification
// attempt's research fetch hangs, and the pass that takes the fetched
// research accepts it without fetching again.
func TestResearchFetchRunsOutsideThePass(t *testing.T) {
	t.Parallel()
	f := newSpecificationFixture(t, true, 2)
	s := newStall()
	f.researchTransport = specificationRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		f.fetchCalls.Add(1)
		if err := s.hold(request.Context()); err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/plain"}},
			Body:       io.NopCloser(strings.NewReader("authoritative research")), Request: request,
		}, nil
	})
	driver := f.newDriver(t)
	if err := specifyfake.Script(driver, specificationInvocationID("specification-run", 1), 0, 0, specify.Output{
		FetchRequests: []specify.FetchRequest{{URL: "https://docs.example/contracts", Purpose: "verify the contract"}},
	}); err != nil {
		t.Fatal(err)
	}
	f.submit(t)
	e := f.newEngine(t, driver)
	returnsWhileStalled(t, s, func() error {
		_, err := e.Reconcile(t.Context())
		return err
	})
	close(s.release)
	if result, err := reconcileSettled(t, e); err != nil || result.ResultsAccepted != 1 {
		t.Fatalf("Reconcile = %+v, %v", result, err)
	}
	if got := f.fetchCalls.Load(); got != 1 {
		t.Fatalf("research fetches = %d, want 1", got)
	}
}

// TestOffPassTriageErrorStopsTheNextPass: a triage call that fails outside
// the pass stops the next pass with its error, as it stopped the pass that
// made the call when the call ran inside it, and is reported once.
func TestOffPassTriageErrorStopsTheNextPass(t *testing.T) {
	t.Parallel()
	f := newAttentionDiscussionFixture(t, domain.AttentionExecutionFailure, nil)
	e := f.engine(t, false)
	failed := errors.New("record specifier triage: store refused")
	e.triageOutcomes.record(false, failed)
	if _, err := e.Reconcile(t.Context()); !errors.Is(err, failed) {
		t.Fatalf("Reconcile after a failed triage = %v, want %v", err, failed)
	}
	if _, err := e.Reconcile(t.Context()); err != nil {
		t.Fatalf("the next Reconcile reported the triage failure again: %v", err)
	}
}

// TestFailureDiagnosisRunsOutsideThePass: recording a failed run returns
// while the run's advisory diagnosis call hangs.
func TestFailureDiagnosisRunsOutsideThePass(t *testing.T) {
	t.Parallel()
	s := newStall()
	workflow, run, terminal, _ := newDiagnosticFixture(t, taskNamingDriverFunc(
		func(ctx context.Context, _ inference.Request, _ inference.Secret) (inference.Response, error) {
			if err := s.hold(ctx); err != nil {
				return inference.Response{}, err
			}
			return inference.Response{Output: []byte(`{"probable_cause":"tool failure","explanation":"failed"}`)}, nil
		}))
	returnsWhileStalled(t, s, func() error {
		_, err := workflow.recordProductionTerminalWithAuthority(t.Context(), run, terminal, false)
		return err
	})
	close(s.release)
	workflow.waitOffPass()
}
