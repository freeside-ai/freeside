package engine

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	inferencefake "github.com/freeside-ai/freeside/daemon/internal/inference/fake"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// triageSourceFixture is a submitted task whose work item holds a case's
// text, on project freeasinbird/freeside (repository ID 42) unless the case
// leaves it unregistered, with a research fetcher that answers every request
// with issue 7 and records the URLs it was asked for.
type triageSourceFixture struct {
	spec    specificationFixture
	run     domain.Run
	fetcher *specify.Fetcher
	fetches *[]string
}

func newTriageSourceFixture(t *testing.T, text string, registered bool, status int) triageSourceFixture {
	t.Helper()
	f := newSpecificationFixtureAllowing(t, true, 3, "https://api.github.com")
	source := testSpecificationArtifact(t, "submitted-source", domain.ArtifactKindSpecification,
		domain.Digest(contentaddr.Sum([]byte(text))), domain.ProducerAgent, "submit")
	if _, err := f.blobs.Put(source.Digest, strings.NewReader(text)); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		if err := tx.PutArtifact(t.Context(), source); err != nil {
			return err
		}
		if !registered {
			return nil
		}
		return tx.RegisterProject(t.Context(), domain.Project{ID: "project-1", Repo: "freeasinbird/freeside", RepositoryID: 42})
	}); err != nil {
		t.Fatal(err)
	}
	f.source = source
	submitted, err := SubmitSpecificationRun(t.Context(), f.store, f.specWithSource(domain.SpecificationSource{
		Kind: domain.SpecificationSourceWorkItemArtifact, WorkItemArtifactID: source.ID,
	}))
	if err != nil {
		t.Fatal(err)
	}
	fetches := &[]string{}
	fetcher, err := specify.NewFetcher(f.store, f.blobs, specificationRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		*fetches = append(*fetches, request.URL.String())
		return &http.Response{
			StatusCode: status, Request: request,
			Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"title":"Bound requests","body":"Enforce the request body limit.","labels":[{"name":"bug"},{"name":"daemon"}]}`)),
		}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return triageSourceFixture{spec: f, run: submitted.Run, fetcher: fetcher, fetches: fetches}
}

// engine is an engine with the fixture's fetcher and the given inference
// client, enough to call a judgment site directly.
func (f triageSourceFixture) engine(client *inference.Client) *Engine {
	return &Engine{
		store: f.spec.store, inference: client, logger: slog.New(slog.DiscardHandler),
		specification: &specificationWorkflow{fetcher: f.fetcher, blobs: f.spec.blobs},
	}
}

// triagerLineup is a lineup with a triager line, as callTriager requires
// before it reads the source.
func triagerLineup(t *testing.T) domain.Lineup {
	t.Helper()
	admission := newAgentAdmissionFixture(t)
	admission.addTriagerLine(t)
	lineup, err := admission.selection.Tree.ResolveLineup()
	if err != nil {
		t.Fatal(err)
	}
	return lineup
}

// A submitted task that is exactly one canonical issue URL in the managed
// repository triages from that issue, fetched by repository ID; any other
// submitted text triages as written and fetches nothing.
func TestTriageReadsBareIssueURLSubmission(t *testing.T) {
	t.Parallel()
	lineup := triagerLineup(t)
	const issueEndpoint = "https://api.github.com/repositories/42/issues/7"
	for _, tc := range []struct {
		name       string
		text       string
		registered bool
		readsIssue bool
	}{
		{"bare URL", "https://github.com/freeasinbird/freeside/issues/7\n", true, true},
		{"owner case differs", "  https://github.com/FreeAsInBird/freeside/issues/7", true, true},
		{"another repository", "https://github.com/freeasinbird/other/issues/7", true, false},
		{"URL with more text", "https://github.com/freeasinbird/freeside/issues/7\nAlso fix the docs.", true, false},
		{"leading zero", "https://github.com/freeasinbird/freeside/issues/07", true, false},
		{"pull request path", "https://github.com/freeasinbird/freeside/pull/7", true, false},
		{"unregistered project", "https://github.com/freeasinbird/freeside/issues/7", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newTriageSourceFixture(t, tc.text, tc.registered, http.StatusOK)
			driver := inferencefake.New()
			driver.Script(inference.TriagerSiteID, inferencefake.Script{Response: inference.Response{
				Output: []byte(`{"tier":"standard","reason":"A small fix."}`),
			}})
			record, err := f.engine(triageClient(t, driver, 10)).callTriager(t.Context(), f.run, lineup)
			if err != nil {
				t.Fatal(err)
			}
			if record.Outcome != domain.TriageOutcomeAnswered || record.Escalated {
				t.Fatalf("triage record = %+v, want answered standard", record)
			}
			requests := driver.Requests()
			if len(requests) != 1 {
				t.Fatalf("triager calls = %d, want 1", len(requests))
			}
			fields := requests[0].Fields
			if !tc.readsIssue {
				if len(*f.fetches) != 0 || fields["source_text"] != tc.text || fields["issue_title"] != "" ||
					fields["issue_body"] != "" || fields["issue_labels"] != "" {
					t.Fatalf("fetches = %v, fields = %+v; want the submitted text and no fetch", *f.fetches, fields)
				}
				return
			}
			if len(*f.fetches) != 1 || (*f.fetches)[0] != issueEndpoint {
				t.Fatalf("fetches = %v, want one to %s", *f.fetches, issueEndpoint)
			}
			if fields["issue_title"] != "Bound requests" || fields["issue_body"] != "Enforce the request body limit." ||
				fields["issue_labels"] != "bug\ndaemon" || fields["source_text"] != "" {
				t.Fatalf("triager fields = %+v, want the issue's", fields)
			}
			if err := f.spec.store.Read(t.Context(), func(tx *store.ReadTx) error {
				_, err := tx.GetArtifact(t.Context(), domain.ArtifactID("research-task-triager-"+string(f.run.TaskID)+"-1"))
				return err
			}); err != nil {
				t.Fatalf("triage research artifact: %v", err)
			}
		})
	}
}

// An issue that cannot be fetched escalates as an unavailable source, with
// no triager call, as for an issue-subject task.
func TestTriageBareIssueURLFetchFailureEscalates(t *testing.T) {
	t.Parallel()
	f := newTriageSourceFixture(t, "https://github.com/freeasinbird/freeside/issues/7", true, http.StatusNotFound)
	driver := inferencefake.New()
	record, err := f.engine(triageClient(t, driver, 10)).callTriager(t.Context(), f.run, triagerLineup(t))
	if err != nil {
		t.Fatal(err)
	}
	if record.Outcome != domain.TriageOutcomeCallFailed || record.Reason == nil ||
		*record.Reason != triageSourceUnavailable || !record.Escalated {
		t.Fatalf("triage record = %+v, want escalated %s", record, triageSourceUnavailable)
	}
	if len(*f.fetches) != 1 || len(driver.Requests()) != 0 {
		t.Fatalf("fetches = %v, triager calls = %d; want one fetch and no call", *f.fetches, len(driver.Requests()))
	}
}

// The task namer keeps naming a bare-URL submission from its text: only
// triage reads the issue it names.
func TestTaskNamerKeepsBareIssueURLSubmissionText(t *testing.T) {
	t.Parallel()
	const text = "https://github.com/freeasinbird/freeside/issues/7"
	f := newTriageSourceFixture(t, text, true, http.StatusOK)
	namer := inferencefake.New()
	namer.Script(inference.TaskNamerSiteID, inferencefake.Script{Response: inference.Response{Output: []byte(`{"name":"Bound requests"}`)}})
	if err := f.engine(taskNamingClient(t, namer)).nameTaskIfUnnamed(t.Context(), f.run); err != nil {
		t.Fatal(err)
	}
	requests := namer.Requests()
	if len(*f.fetches) != 0 || len(requests) != 1 || requests[0].Fields["source_text"] != text ||
		requests[0].Fields["issue_title"] != "" {
		t.Fatalf("fetches = %v, namer requests = %+v; want the submitted text and no fetch", *f.fetches, requests)
	}
}
