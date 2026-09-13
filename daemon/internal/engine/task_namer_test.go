package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/advisory"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	inferencefake "github.com/freeside-ai/freeside/daemon/internal/inference/fake"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	specifyfake "github.com/freeside-ai/freeside/daemon/internal/specify/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func taskNamingClient(t *testing.T, driver inference.Driver) *inference.Client {
	t.Helper()
	root := t.TempDir()
	claims, err := advisory.Open(filepath.Join(root, "claims.json"), 100, 16<<10)
	if err != nil {
		t.Fatal(err)
	}
	limits := inference.Limits{Calls: 10, ComputeUnits: 100_000, AttentionItems: 10, Starvation: time.Hour}
	site := inference.TaskNamerSite(inference.Budget{
		Window: time.Hour, Site: limits, Project: limits, Global: limits,
		MaxCallsPerRoot: 10, MaxStarvationPerRoot: time.Hour,
	})
	client, err := inference.New(inference.Config{
		StatePath: filepath.Join(root, "ledger.json"), Advisory: claims,
		Binding: inference.Binding{Provider: "fake", Model: "namer", Driver: driver}, Sites: []inference.Site{site},
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func taskNameForRun(t *testing.T, st *store.Store, id domain.RunID) domain.DisplayName {
	t.Helper()
	var task domain.Task
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		run, err := tx.GetRun(t.Context(), id)
		if err != nil {
			return err
		}
		task, err = tx.GetTask(t.Context(), run.TaskID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return task.Name
}

func TestTaskNamingAfterSpecificationDispatch(t *testing.T) {
	for _, tc := range []struct {
		label, source, mode string
		wantSource          domain.DisplayNameSource
		wantCalls           int
	}{
		{"operator heading", "# Choose this name\n\nRequested work.", "bound", domain.DisplayNameSourceOperator, 0},
		{"headingless", "Bound request bodies.", "bound", domain.DisplayNameSourceAgent, 1},
		{"unbound", "Bound request bodies.", "unbound", domain.DisplayNameSourceIdentifier, 0},
		{"no client", "Bound request bodies.", "none", domain.DisplayNameSourceIdentifier, 0},
		{"failed", "Bound request bodies.", "failed", domain.DisplayNameSourceIdentifier, 1},
		{"bounded input", strings.Repeat("界\x01", 20_000), "bound", domain.DisplayNameSourceAgent, 1},
	} {
		t.Run(tc.label, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newSpecificationFixture(t, true, 3)
				source := testSpecificationArtifact(t, "naming-source", domain.ArtifactKindSpecification,
					domain.Digest(contentaddr.Sum([]byte(tc.source))), domain.ProducerAgent, "submit")
				if _, err := f.blobs.Put(source.Digest, strings.NewReader(tc.source)); err != nil {
					t.Fatal(err)
				}
				if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error { return tx.PutArtifact(t.Context(), source) }); err != nil {
					t.Fatal(err)
				}
				f.source = source
				spec := f.specWithSource(domain.SpecificationSource{})
				spec.SourceBytes = []byte(tc.source)
				for range 2 {
					if _, err := SubmitSpecificationRun(t.Context(), f.store, spec); err != nil {
						t.Fatal(err)
					}
				}
				before := taskNameForRun(t, f.store, spec.SpecificationRunID)
				if tc.wantSource == domain.DisplayNameSourceOperator {
					if before != (domain.DisplayName{Text: "Choose this name", Source: domain.DisplayNameSourceOperator}) {
						t.Fatalf("submitted name = %+v", before)
					}
				} else if before.Source != domain.DisplayNameSourceIdentifier {
					t.Fatalf("name before dispatch = %+v", before)
				}
				stageDriver := f.newDriver(t)
				if err := specifyfake.Script(stageDriver, specificationInvocationID(spec.SpecificationRunID, 1), 0, 0, specify.Output{Specification: &specify.Specification{
					Summary: "Ready for approval.", Body: "# Approved specification", Addressals: []specify.Addressal{},
				}}); err != nil {
					t.Fatal(err)
				}
				e := f.newEngine(t, stageDriver)
				namer := inferencefake.New()
				script := inferencefake.Script{Response: inference.Response{Output: []byte(`{"name":"Bound request bodies"}`)}}
				if tc.mode == "failed" {
					script.Err = errors.New("provider unavailable")
				}
				namer.Script(inference.TaskNamerSiteID, script)
				if tc.mode != "none" {
					var driver inference.Driver = namer
					if tc.mode == "unbound" {
						driver = nil
					}
					if err := WithInference(taskNamingClient(t, driver))(e); err != nil {
						t.Fatal(err)
					}
				}
				stop := startTaskNameWorker(t, e)
				defer stop()
				if started, err := e.dispatchPendingInvocations(t.Context()); err != nil || started != 1 {
					t.Fatalf("dispatch = %d, %v", started, err)
				}
				synctest.Wait()
				after := taskNameForRun(t, f.store, spec.SpecificationRunID)
				if after.Source != tc.wantSource || (tc.wantSource == domain.DisplayNameSourceAgent && after.Text != "Bound request bodies") {
					t.Fatalf("name after dispatch = %+v", after)
				}
				requests := namer.Requests()
				if len(requests) != tc.wantCalls {
					t.Fatalf("namer calls = %d, want %d", len(requests), tc.wantCalls)
				}
				if len(requests) > 0 {
					fields := requests[0].Fields
					encoded, err := json.Marshal(fields)
					if err != nil || len(encoded) > 64<<10 || !utf8.ValidString(fields["source_text"]) ||
						!strings.HasPrefix(tc.source, fields["source_text"]) || fields["source_text"] == "" ||
						fields["source_kind"] != "work_item_artifact" || fields["issue_body"] != "" {
						t.Fatalf("invalid outbound source, encoded bytes=%d: %v", len(encoded), err)
					}
				}
				if started, err := e.dispatchPendingInvocations(t.Context()); err != nil || started != 0 || len(namer.Requests()) != tc.wantCalls {
					t.Fatalf("dispatch replay = %d, %v", started, err)
				}
			})
		})
	}
}

func startTaskNameWorker(t *testing.T, e *Engine) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.runTaskNames(ctx)
	}()
	return func() {
		cancel()
		<-done
	}
}

type taskNamingDriverFunc func(context.Context, inference.Request, inference.Secret) (inference.Response, error)

func (f taskNamingDriverFunc) Complete(ctx context.Context, request inference.Request, secret inference.Secret) (inference.Response, error) {
	return f(ctx, request, secret)
}

func TestSlowTaskNameDoesNotBlockReconcileOrReplacePermanentName(t *testing.T) {
	for _, source := range []domain.DisplayNameSource{domain.DisplayNameSourceOperator, domain.DisplayNameSourceSpecification} {
		t.Run(string(source), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newSpecificationFixture(t, true, 3)
				f.submit(t)
				driver := f.newDriver(t)
				if err := specifyfake.Script(driver, specificationInvocationID("specification-run", 1), 0, 0, specify.Output{Specification: &specify.Specification{
					Summary: "Ready for approval.", Body: "# Permanent name", Addressals: []specify.Addressal{},
				}}); err != nil {
					t.Fatal(err)
				}
				e := f.newEngine(t, driver)
				started, release := make(chan struct{}), make(chan struct{})
				namer := taskNamingDriverFunc(func(ctx context.Context, _ inference.Request, _ inference.Secret) (inference.Response, error) {
					close(started)
					select {
					case <-ctx.Done():
						return inference.Response{}, ctx.Err()
					case <-release:
						return inference.Response{Output: []byte(`{"name":"Late advisory name"}`)}, nil
					}
				})
				if err := WithInference(taskNamingClient(t, namer))(e); err != nil {
					t.Fatal(err)
				}
				stop := startTaskNameWorker(t, e)
				defer stop()
				before := time.Now()
				result, err := e.Reconcile(t.Context())
				if err != nil || result.ResultsAccepted != 1 || time.Since(before) != 0 {
					t.Fatalf("reconcile waited for naming or failed to accept completion: %+v, %v", result, err)
				}
				<-started
				item, snapshot := f.item(t, "spec-approval-implementation-run-1")
				if item.DisplayNames.Task.Source != domain.DisplayNameSourceIdentifier {
					t.Fatalf("pending namer changed approval name: %+v", item.DisplayNames.Task)
				}
				if source == domain.DisplayNameSourceOperator {
					run, err := f.run("specification-run")
					if err != nil {
						t.Fatal(err)
					}
					if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
						return tx.SetTaskName(t.Context(), run.TaskID, domain.DisplayName{Text: "Permanent name", Source: source})
					}); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := f.signet.Submit(t.Context(), signet.ClientCommand{
						CommandID: "approve-while-naming", DeviceID: "device-1", ExpectedEntityVersion: snapshot.EntityVersion,
						Payload: signet.DecisionPayload{ItemID: item.ID, Action: domain.ActionApprove, ItemVersion: item.ItemVersion, ArtifactDigests: item.ArtifactDigests},
					}); err != nil {
						t.Fatal(err)
					}
					if _, err := e.Reconcile(t.Context()); err != nil || time.Since(before) != 0 {
						t.Fatalf("approval waited for naming: %v", err)
					}
				}
				close(release)
				synctest.Wait()
				want := domain.DisplayName{Text: "Permanent name", Source: source}
				if got := taskNameForRun(t, f.store, "specification-run"); got != want {
					t.Fatalf("late name replaced permanent name: %+v, want %+v", got, want)
				}
			})
		})
	}
}

func TestTaskNameQueueDoesNotWaitWithoutWorker(t *testing.T) {
	f := newSpecificationFixture(t, true, 3)
	f.submit(t)
	namer := inferencefake.New()
	e := f.newEngine(t, f.newDriver(t))
	if err := WithInference(taskNamingClient(t, namer))(e); err != nil {
		t.Fatal(err)
	}
	run, err := f.run("specification-run")
	if err != nil {
		t.Fatal(err)
	}
	for range taskNameQueueCapacity + 1 {
		e.enqueueTaskName(run)
	}
	if len(e.taskNames) != taskNameQueueCapacity || len(namer.Requests()) != 0 ||
		taskNameForRun(t, f.store, run.ID).Source != domain.DisplayNameSourceIdentifier {
		t.Fatal("saturated queue must keep identifier fallback without calling inference")
	}
}

func TestEngineRunCancelsTaskNameWorker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newSpecificationFixture(t, true, 3)
		f.submit(t)
		driver := f.newDriver(t)
		if err := specifyfake.Script(driver, specificationInvocationID("specification-run", 1), 0, 0, specify.Output{Specification: &specify.Specification{
			Summary: "Ready for approval.", Body: "# Waiting for approval", Addressals: []specify.Addressal{},
		}}); err != nil {
			t.Fatal(err)
		}
		e := f.newEngine(t, driver)
		started, canceled := make(chan struct{}), make(chan struct{})
		namer := taskNamingDriverFunc(func(ctx context.Context, _ inference.Request, _ inference.Secret) (inference.Response, error) {
			close(started)
			<-ctx.Done()
			close(canceled)
			return inference.Response{}, ctx.Err()
		})
		if err := WithInference(taskNamingClient(t, namer))(e); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- e.Run(ctx, time.Hour) }()
		<-started
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		<-canceled
		synctest.Wait()
		if got := taskNameForRun(t, f.store, "specification-run"); got.Source != domain.DisplayNameSourceIdentifier {
			t.Fatalf("canceled namer changed name: %+v", got)
		}
	})
}

func TestTaskNamerFetchesIssueEvidence(t *testing.T) {
	for _, tc := range []struct {
		label  string
		status int
		body   string
	}{
		{"small", http.StatusOK, "Enforce the request body limit."},
		{"large", http.StatusOK, strings.Repeat("Enforce the request body limit. ", 2500)},
		{"reused repository name", http.StatusOK, "Enforce the request body limit."},
		{"unavailable", http.StatusNotFound, ""},
	} {
		t.Run(tc.label, func(t *testing.T) {
			r := newIssueSubjectReservation(t)
			if _, err := SubmitSpecificationRun(t.Context(), r.store, r.spec); err != nil {
				t.Fatal(err)
			}
			fetches := 0
			response, err := json.Marshal(map[string]any{"title": "Bound requests", "body": tc.body, "irrelevant": "not sent"})
			if err != nil {
				t.Fatal(err)
			}
			fetcher, err := specify.NewFetcher(r.store, r.blobs, specificationRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				fetches++
				if tc.label == "reused repository name" && request.URL.Path == "/repos/freeasinbird/freeside/issues/7" {
					// The stored display name now belongs to another repository;
					// only numeric repository 42 still names the admitted subject.
					return &http.Response{
						StatusCode: http.StatusOK, Request: request,
						Header: http.Header{"Content-Type": []string{"application/json"}},
						Body:   io.NopCloser(strings.NewReader(`{"title":"Unrelated repository","body":"Unrelated work."}`)),
					}, nil
				}
				if request.URL.String() != "https://api.github.com/repositories/42/issues/7" || request.Header.Get("Authorization") != "" {
					t.Errorf("unexpected research request: %s", request.URL)
				}
				return &http.Response{
					StatusCode: tc.status,
					Request:    request,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(string(response))),
				}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			namer := inferencefake.New()
			namer.Script(inference.TaskNamerSiteID, inferencefake.Script{Response: inference.Response{Output: []byte(`{"name":"Bound requests"}`)}})
			e := &Engine{store: r.store, inference: taskNamingClient(t, namer), specification: &specificationWorkflow{fetcher: fetcher, blobs: r.blobs}}
			var run domain.Run
			if err := r.store.Read(t.Context(), func(tx *store.ReadTx) error {
				var err error
				run, err = tx.GetRun(t.Context(), r.specificationRunID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			err = e.nameTaskIfUnnamed(t.Context(), run)
			name := taskNameForRun(t, r.store, run.ID)
			if fetches != 1 {
				t.Fatalf("fetch calls = %d", fetches)
			}
			if tc.status != http.StatusOK {
				if err == nil || name.Source != domain.DisplayNameSourceIdentifier || len(namer.Requests()) != 0 {
					t.Fatalf("fetch failure = %v, name = %+v", err, name)
				}
				return
			}
			if err != nil || name != (domain.DisplayName{Text: "Bound requests", Source: domain.DisplayNameSourceAgent}) {
				t.Fatalf("issue name = %+v, %v", name, err)
			}
			requests := namer.Requests()
			if len(requests) != 1 || len(requests[0].Fields) != 6 || requests[0].Fields["repository"] != "freeasinbird/freeside" ||
				requests[0].Fields["issue_number"] != "7" || requests[0].Fields["issue_title"] != "Bound requests" ||
				requests[0].Fields["issue_body"] == "" || !strings.HasPrefix(tc.body, requests[0].Fields["issue_body"]) || requests[0].Fields["source_text"] != "" {
				t.Fatalf("issue inputs = %+v", requests)
			}
			var artifact domain.Artifact
			if err := r.store.Read(t.Context(), func(tx *store.ReadTx) error {
				var err error
				artifact, err = tx.GetArtifact(t.Context(), domain.ArtifactID("research-task-namer-"+string(run.TaskID)+"-1"))
				return err
			}); err != nil {
				t.Fatal(err)
			}
			body, err := readBoundedArtifactBlob(r.blobs, artifact.Digest, exec.ProductionMaxInputBytes)
			if err != nil || artifact.Type != domain.ArtifactKindResearch || artifact.PublishEligible || !strings.Contains(string(body), "body_base64") {
				t.Fatalf("research artifact = %+v, %v", artifact, err)
			}
			workItem, err := e.readArtifactBody(t.Context(), r.workItemArtifact)
			if err != nil || strings.Contains(workItem, "Bound requests") || strings.Contains(workItem, "request body limit") {
				t.Fatalf("issue content copied into intake: %q, %v", workItem, err)
			}
			if err := e.nameTaskIfUnnamed(t.Context(), run); err != nil || fetches != 1 || len(namer.Requests()) != 1 {
				t.Fatalf("named issue was fetched or named again: %v", err)
			}
		})
	}
}

func TestFirstSpecificationTitleRefinesTaskName(t *testing.T) {
	for _, tc := range []struct {
		label   string
		initial domain.DisplayNameSource
		title   string
		refines bool
	}{
		{"identifier", domain.DisplayNameSourceIdentifier, "Refine the outcome", true},
		{"agent", domain.DisplayNameSourceAgent, "Refine the outcome", true},
		{"operator", domain.DisplayNameSourceOperator, "Refine the outcome", false},
		{"long", domain.DisplayNameSourceAgent, strings.Repeat("界", 61), false},
		{"secret", domain.DisplayNameSourceAgent, "ghp_" + strings.Repeat("x", 36), false},
	} {
		t.Run(tc.label, func(t *testing.T) {
			f := newSpecificationFixture(t, true, 3)
			f.submit(t)
			run, err := f.run("specification-run")
			if err != nil {
				t.Fatal(err)
			}
			if tc.initial != domain.DisplayNameSourceIdentifier {
				if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
					return tx.SetTaskName(t.Context(), run.TaskID, domain.DisplayName{Text: "Initial name", Source: tc.initial})
				}); err != nil {
					t.Fatal(err)
				}
			}
			before := taskNameForRun(t, f.store, run.ID)
			driver := f.newDriver(t)
			if err := specifyfake.Script(driver, specificationInvocationID(run.ID, 1), 0, 0, specify.Output{Specification: &specify.Specification{
				Title: &tc.title, Summary: "Ready for approval.", Body: "# Approved heading\n\nBound the request.", Addressals: []specify.Addressal{},
			}}); err != nil {
				t.Fatal(err)
			}
			e := f.newEngine(t, driver)
			if _, err := e.Reconcile(t.Context()); err != nil {
				t.Fatal(err)
			}
			want := before
			if tc.refines {
				want = domain.DisplayName{Text: tc.title, Source: domain.DisplayNameSourceAgent}
			}
			if got := taskNameForRun(t, f.store, run.ID); got != want {
				t.Fatalf("accepted title name = %+v, want %+v", got, want)
			}
			item, snapshot := f.item(t, "spec-approval-implementation-run-1")
			if item.DisplayNames.Task != want {
				t.Fatalf("approval card name = %+v, want %+v", item.DisplayNames.Task, want)
			}
			if _, err := f.signet.Submit(t.Context(), signet.ClientCommand{
				CommandID: "approve-name", DeviceID: "device-1", ExpectedEntityVersion: snapshot.EntityVersion,
				Payload: signet.DecisionPayload{ItemID: item.ID, Action: domain.ActionApprove, ItemVersion: item.ItemVersion, ArtifactDigests: item.ArtifactDigests},
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := e.Reconcile(t.Context()); err != nil {
				t.Fatal(err)
			}
			if tc.initial != domain.DisplayNameSourceOperator {
				want = domain.DisplayName{Text: "Approved heading", Source: domain.DisplayNameSourceSpecification}
			}
			if got := taskNameForRun(t, f.store, run.ID); got != want {
				t.Fatalf("approved name = %+v, want %+v", got, want)
			}
		})
	}
}

func TestHeadingTaskNamesAreBounded(t *testing.T) {
	for _, tc := range []struct{ label, heading, want string }{
		{"exact unicode bound", strings.Repeat("界", 60), strings.Repeat("界", 60)},
		{"oversized unicode", strings.Repeat("界", 1000), strings.Repeat("界", 60)},
		{"trim truncated whitespace", strings.Repeat("界", 59) + " next", strings.Repeat("界", 59)},
	} {
		t.Run(tc.label, func(t *testing.T) {
			body := "# " + tc.heading + "\n\nRequested work."
			t.Run("operator", func(t *testing.T) {
				f := newSpecificationFixture(t, true, 3)
				source := testSpecificationArtifact(t, "bounded-heading-source", domain.ArtifactKindSpecification,
					domain.Digest(contentaddr.Sum([]byte(body))), domain.ProducerAgent, "submit")
				if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error { return tx.PutArtifact(t.Context(), source) }); err != nil {
					t.Fatal(err)
				}
				f.source = source
				spec := f.specWithSource(domain.SpecificationSource{})
				spec.SourceBytes = []byte(body)
				if _, err := SubmitSpecificationRun(t.Context(), f.store, spec); err != nil {
					t.Fatal(err)
				}
				want := domain.DisplayName{Text: tc.want, Source: domain.DisplayNameSourceOperator}
				if got := taskNameForRun(t, f.store, spec.SpecificationRunID); got != want {
					t.Fatalf("operator name = %+v, want %+v", got, want)
				}
			})
			t.Run("approval", func(t *testing.T) {
				f := newSpecificationFixture(t, true, 3)
				f.submit(t)
				driver := f.newDriver(t)
				if err := specifyfake.Script(driver, specificationInvocationID("specification-run", 1), 0, 0, specify.Output{Specification: &specify.Specification{
					Summary: "Ready for approval.", Body: body, Addressals: []specify.Addressal{},
				}}); err != nil {
					t.Fatal(err)
				}
				e := f.newEngine(t, driver)
				if _, err := e.Reconcile(t.Context()); err != nil {
					t.Fatal(err)
				}
				item, snapshot := f.item(t, "spec-approval-implementation-run-1")
				if _, err := f.signet.Submit(t.Context(), signet.ClientCommand{
					CommandID: "approve-bounded-name", DeviceID: "device-1", ExpectedEntityVersion: snapshot.EntityVersion,
					Payload: signet.DecisionPayload{ItemID: item.ID, Action: domain.ActionApprove, ItemVersion: item.ItemVersion, ArtifactDigests: item.ArtifactDigests},
				}); err != nil {
					t.Fatal(err)
				}
				if _, err := e.Reconcile(t.Context()); err != nil {
					t.Fatal(err)
				}
				want := domain.DisplayName{Text: tc.want, Source: domain.DisplayNameSourceSpecification}
				if got := taskNameForRun(t, f.store, "specification-run"); got != want {
					t.Fatalf("approved name = %+v, want %+v", got, want)
				}
			})
		})
	}
}

func TestSpecificationRevisionKeepsRefinedTaskName(t *testing.T) {
	f := newSpecificationFixture(t, true, 3)
	f.submit(t)
	driver := f.newDriver(t)
	firstTitle, revisedTitle := "Bound requests", "Revise the request limit"
	if err := specifyfake.Script(driver, specificationInvocationID("specification-run", 1), 0, 0, specify.Output{Specification: &specify.Specification{
		Title: &firstTitle, Summary: "First draft.", Body: "# Bound requests\n\nBound the body.", Addressals: []specify.Addressal{},
	}}); err != nil {
		t.Fatal(err)
	}
	e := f.newEngine(t, driver)
	if _, err := e.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	first, snapshot := f.item(t, "spec-approval-implementation-run-1")
	if _, err := f.signet.Submit(t.Context(), signet.ClientCommand{
		CommandID: "revise-named-spec", DeviceID: "device-1", ExpectedEntityVersion: snapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: first.ID, Action: domain.ActionRequestChanges, ItemVersion: first.ItemVersion,
			ArtifactDigests: first.ArtifactDigests, Message: "Specify the limit.",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := specifyfake.Script(driver, specificationInvocationID("specification-run", 2), 0, 0, specify.Output{Specification: &specify.Specification{
		Title: &revisedTitle, Summary: "Revised draft.", Body: "# Revise the request limit\n\nLimit the body to 1 MiB.",
		Addressals: []specify.Addressal{{CommentID: "revise-named-spec", Response: "Specified 1 MiB."}},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	item, _ := f.item(t, "spec-approval-implementation-run-2")
	want := domain.DisplayName{Text: firstTitle, Source: domain.DisplayNameSourceAgent}
	if got := taskNameForRun(t, f.store, "specification-run"); got != want || item.DisplayNames.Task != want {
		t.Fatalf("revision renamed task: task=%+v item=%+v, want %+v", got, item.DisplayNames.Task, want)
	}
}
