package publish_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

const (
	checkHead = "1111111111111111111111111111111111111111"
	checkBase = "main"
)

// checkJob builds a completed job as the classifier sees it.
func checkJob(id, run int64, name, conclusion string, runner int64, steps ...string) publish.CheckJob {
	return publish.CheckJob{
		ID: id, RunID: run, RunAttempt: 1, Name: name, Status: "completed",
		Conclusion: conclusion, RunnerID: runner, Steps: steps,
		CompletedAt: time.Date(2026, 11, 24, 17, 0, 0, 0, time.UTC).Add(time.Duration(id) * time.Second),
	}
}

// issue3591Jobs is #3591's first attempt: the macOS test job was cancelled
// with no runner and no steps, and the jobs that waited on it failed with
// steps that ran.
func issue3591Jobs() []publish.CheckJob {
	return []publish.CheckJob{
		checkJob(10, 7, "daemon / linux (build, test)", "success", 4, "success", "success"),
		checkJob(11, 7, "daemon / macos (test, darwin 1/1)", "cancelled", 0),
		checkJob(12, 7, "daemon / macos (build, test)", "failure", 5, "success", "failure"),
		checkJob(13, 7, "required", "failure", 6, "success", "failure"),
	}
}

func TestClassifyRequiredCheck(t *testing.T) {
	t.Parallel()
	infrastructure := domain.CheckFailureInfrastructure
	code := domain.CheckFailureCode
	other := domain.CheckFailureOther
	inProgress := checkJob(13, 7, "required", "", 6)
	inProgress.Status = "in_progress"
	rerun := checkJob(20, 7, "required", "success", 8, "success")
	rerun.RunAttempt = 2

	cases := []struct {
		name     string
		required []string
		jobs     []publish.CheckJob
		want     publish.RequiredCheckObservation
	}{
		{
			name:     "no required check",
			required: nil,
			jobs:     issue3591Jobs(),
			want:     publish.RequiredCheckObservation{None: true},
		},
		{
			name:     "issue 3591 is infrastructure despite failed steps",
			required: []string{"required"},
			jobs:     issue3591Jobs(),
			want: publish.RequiredCheckObservation{
				State: domain.RequiredCheckFailed, FailureClass: &infrastructure,
				FailedJobs: []publish.FailedCheckJob{
					{ID: 11, RunID: 7, RunAttempt: 1, Name: "daemon / macos (test, darwin 1/1)"},
					{ID: 12, RunID: 7, RunAttempt: 1, Name: "daemon / macos (build, test)"},
				},
				FailedRuns: []publish.CheckRun{{ID: 7, Attempt: 1}},
			},
		},
		{
			name:     "a failed step is code",
			required: []string{"required"},
			jobs: []publish.CheckJob{
				checkJob(12, 7, "daemon / linux (build, test)", "failure", 5, "success", "failure"),
				checkJob(13, 7, "required", "failure", 6, "success", "failure"),
			},
			want: publish.RequiredCheckObservation{
				State: domain.RequiredCheckFailed, FailureClass: &code,
				FailedJobs: []publish.FailedCheckJob{{ID: 12, RunID: 7, RunAttempt: 1, Name: "daemon / linux (build, test)"}},
				FailedRuns: []publish.CheckRun{{ID: 7, Attempt: 1}},
			},
		},
		{
			name:     "a job cancelled part-way is other, and the required job is named when nothing else failed",
			required: []string{"required"},
			jobs:     []publish.CheckJob{checkJob(13, 7, "required", "timed_out", 6, "success", "cancelled")},
			want: publish.RequiredCheckObservation{
				State: domain.RequiredCheckFailed, FailureClass: &other,
				FailedJobs: []publish.FailedCheckJob{{ID: 13, RunID: 7, RunAttempt: 1, Name: "required"}},
				FailedRuns: []publish.CheckRun{{ID: 7, Attempt: 1}},
			},
		},
		{
			name:     "a conclusion that waits on a person fails and is named",
			required: []string{"required"},
			jobs:     []publish.CheckJob{checkJob(13, 7, "required", "action_required", 6)},
			want: publish.RequiredCheckObservation{
				State: domain.RequiredCheckFailed, FailureClass: &other,
				FailedJobs: []publish.FailedCheckJob{{ID: 13, RunID: 7, RunAttempt: 1, Name: "required"}},
				FailedRuns: []publish.CheckRun{{ID: 7, Attempt: 1}},
			},
		},
		{
			name:     "a required check with no job is pending",
			required: []string{"required", "third-party/ci"},
			jobs:     []publish.CheckJob{checkJob(13, 7, "required", "success", 6, "success")},
			want:     publish.RequiredCheckObservation{State: domain.RequiredCheckPending},
		},
		{
			name:     "a running required job is pending",
			required: []string{"required"},
			jobs:     []publish.CheckJob{inProgress},
			want:     publish.RequiredCheckObservation{State: domain.RequiredCheckPending},
		},
		{
			name:     "a failure outranks a pending check",
			required: []string{"required", "third-party/ci"},
			jobs:     issue3591Jobs()[1:],
			want: publish.RequiredCheckObservation{
				State: domain.RequiredCheckFailed, FailureClass: &infrastructure,
				FailedJobs: []publish.FailedCheckJob{
					{ID: 11, RunID: 7, RunAttempt: 1, Name: "daemon / macos (test, darwin 1/1)"},
					{ID: 12, RunID: 7, RunAttempt: 1, Name: "daemon / macos (build, test)"},
				},
				FailedRuns: []publish.CheckRun{{ID: 7, Attempt: 1}},
			},
		},
		{
			name:     "a rerun's later job speaks for its name",
			required: []string{"required"},
			jobs:     append(issue3591Jobs(), rerun),
			want:     publish.RequiredCheckObservation{State: domain.RequiredCheckPassed},
		},
		{
			name:     "skipped and neutral pass",
			required: []string{"a", "b"},
			jobs:     []publish.CheckJob{checkJob(1, 7, "a", "skipped", 0), checkJob(2, 7, "b", "neutral", 3)},
			want:     publish.RequiredCheckObservation{State: domain.RequiredCheckPassed},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := publish.ClassifyRequiredCheck(tc.required, tc.jobs)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ClassifyRequiredCheck() =\n%+v\nwant\n%+v", got, tc.want)
			}
		})
	}
}

// TestClassifyRequiredCheckBoundsAndScreensNames holds the card's failed job
// list to its bound and screens each name.
func TestClassifyRequiredCheckBoundsAndScreensNames(t *testing.T) {
	t.Parallel()
	jobs := []publish.CheckJob{checkJob(100, 7, "required", "failure", 6, "failure")}
	for i := range 12 {
		jobs = append(jobs, checkJob(int64(i+1), 7, fmt.Sprintf("job %02d", i), "failure", 5, "failure"))
	}
	jobs[1].Name = "deploy ghp_" + strings.Repeat("a", 36)
	got := publish.ClassifyRequiredCheck([]string{"required"}, jobs)
	if len(got.FailedJobs) != domain.MaxRequiredCheckFailedJobs {
		t.Fatalf("failed jobs = %d, want %d", len(got.FailedJobs), domain.MaxRequiredCheckFailedJobs)
	}
	if got.FailedJobs[0].Name != domain.ScreenRequiredCheckJobName(jobs[1].Name) || strings.Contains(got.FailedJobs[0].Name, "ghp_") {
		t.Errorf("first failed job name = %q, want the screened name", got.FailedJobs[0].Name)
	}
}

// checkResponse is one canned response; an etag makes a matching
// If-None-Match answer 304.
type checkResponse struct {
	status int
	body   string
	etag   string
}

// checkForge serves canned responses by request URI and counts 304s.
type checkForge struct {
	t         *testing.T
	mu        sync.Mutex
	responses map[string]checkResponse
	notMod    int
}

func (f *checkForge) set(uri string, resp checkResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responses[uri] = resp
}

func (f *checkForge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	resp, ok := f.responses[r.URL.RequestURI()]
	f.mu.Unlock()
	if !ok {
		f.t.Errorf("unexpected request %s", r.URL.RequestURI())
		w.WriteHeader(http.StatusTeapot)
		return
	}
	if resp.etag != "" && r.Header.Get("If-None-Match") == resp.etag {
		f.mu.Lock()
		f.notMod++
		f.mu.Unlock()
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if resp.etag != "" {
		w.Header().Set("ETag", resp.etag)
	}
	w.WriteHeader(resp.status)
	_, _ = w.Write([]byte(resp.body))
}

const (
	protectionURI = "/repos/" + testRepo + "/branches/main/protection/required_status_checks"
	rulesURI      = "/repos/" + testRepo + "/rules/branches/main?per_page=100"
	runsURI       = "/repos/" + testRepo + "/actions/runs?head_sha=" + checkHead + "&event=pull_request&per_page=10"
)

func jobsURI(run int64, page int) string {
	return fmt.Sprintf("/repos/%s/actions/runs/%d/jobs?filter=latest&per_page=100&page=%d", testRepo, run, page)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func runsBody(t *testing.T, head string, runs ...int64) string {
	t.Helper()
	list := []map[string]any{}
	for _, id := range runs {
		list = append(list, map[string]any{"id": id, "head_sha": head, "run_attempt": 1})
	}
	return mustJSON(t, map[string]any{"total_count": len(list), "workflow_runs": list})
}

// jobsBody renders jobs as the jobs endpoint does, with total as its count.
func jobsBody(t *testing.T, head string, total int, jobs ...publish.CheckJob) string {
	t.Helper()
	list := []map[string]any{}
	for _, job := range jobs {
		steps := []map[string]any{}
		for _, step := range job.Steps {
			steps = append(steps, map[string]any{"conclusion": step})
		}
		var conclusion any
		if job.Conclusion != "" {
			conclusion = job.Conclusion
		}
		list = append(list, map[string]any{
			"id": job.ID, "run_id": job.RunID, "run_attempt": job.RunAttempt, "head_sha": head,
			"name": job.Name, "status": job.Status, "conclusion": conclusion,
			"runner_id": job.RunnerID, "completed_at": job.CompletedAt, "steps": steps,
		})
	}
	return mustJSON(t, map[string]any{"total_count": total, "jobs": list})
}

// newCheckForge serves #3591's first attempt: classic protection requires
// "required", and a ruleset adds nothing.
func newCheckForge(t *testing.T) (*checkForge, *publish.Reconciler) {
	t.Helper()
	f := &checkForge{t: t, responses: map[string]checkResponse{
		protectionURI: {status: http.StatusOK, body: `{"strict":true,"contexts":["required"],"checks":[{"context":"required","app_id":15368}]}`, etag: `"p1"`},
		rulesURI:      {status: http.StatusOK, body: `[{"type":"pull_request","parameters":{}}]`, etag: `"r1"`},
		runsURI:       {status: http.StatusOK, body: runsBody(t, checkHead, 7), etag: `"w1"`},
		jobsURI(7, 1): {status: http.StatusOK, body: jobsBody(t, checkHead, 4, issue3591Jobs()...), etag: `"j1"`},
	}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, publish.NewReconciler(testTokenSource(), srv.Client(), srv.URL)
}

func TestReconcileRequiredCheckReadsAndRevalidates(t *testing.T) {
	t.Parallel()
	f, r := newCheckForge(t)
	ctx := context.Background()
	first, err := r.ReconcileRequiredCheck(ctx, testRepo, checkBase, checkHead)
	if err != nil {
		t.Fatal(err)
	}
	want := publish.ClassifyRequiredCheck([]string{"required"}, issue3591Jobs())
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("observation = %+v, want %+v", first, want)
	}
	if first.FailureClass == nil || *first.FailureClass != domain.CheckFailureInfrastructure {
		t.Fatalf("class = %v, want infrastructure", first.FailureClass)
	}

	second, err := r.ReconcileRequiredCheck(ctx, testRepo, checkBase, checkHead)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(second, first) || f.notMod != 4 {
		t.Errorf("revalidated observation = %+v with %d 304s, want the first with 4", second, f.notMod)
	}

	// After eviction every read is unconditional again.
	r.EvictRequiredCheck(testRepo)
	if _, err := r.ReconcileRequiredCheck(ctx, testRepo, checkBase, checkHead); err != nil {
		t.Fatal(err)
	}
	if f.notMod != 4 {
		t.Errorf("304s after eviction = %d, want 4", f.notMod)
	}
}

func TestReconcileRequiredCheckRequirementSources(t *testing.T) {
	t.Parallel()
	upgrade := checkResponse{
		status: http.StatusForbidden,
		body:   `{"message":"Upgrade to GitHub Pro or make this repository public to enable this feature."}`,
	}
	notFound := checkResponse{status: http.StatusNotFound, body: `{"message":"Branch not protected"}`}
	ruleset := checkResponse{
		status: http.StatusOK,
		body:   `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"required"}]}}]`,
	}

	cases := []struct {
		name       string
		protection checkResponse
		rules      checkResponse
		wantNone   bool
	}{
		{name: "neither source requires a check", protection: notFound, rules: checkResponse{status: http.StatusOK, body: `[]`}, wantNone: true},
		{name: "the plan lacks both features", protection: upgrade, rules: upgrade, wantNone: true},
		{name: "a ruleset alone requires the check", protection: notFound, rules: ruleset},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, r := newCheckForge(t)
			f.set(protectionURI, tc.protection)
			f.set(rulesURI, tc.rules)
			got, err := r.ReconcileRequiredCheck(context.Background(), testRepo, checkBase, checkHead)
			if err != nil {
				t.Fatal(err)
			}
			if got.None != tc.wantNone || (!tc.wantNone && got.State != domain.RequiredCheckFailed) {
				t.Errorf("observation = %+v, want None=%t", got, tc.wantNone)
			}
		})
	}
}

func TestReconcileRequiredCheckPaginatesJobs(t *testing.T) {
	t.Parallel()
	f, r := newCheckForge(t)
	var page1, page2 []publish.CheckJob
	for i := range 100 {
		page1 = append(page1, checkJob(int64(i+1), 7, fmt.Sprintf("matrix %03d", i), "success", 4, "success"))
	}
	page2 = []publish.CheckJob{checkJob(500, 7, "required", "success", 4, "success")}
	f.set(jobsURI(7, 1), checkResponse{status: http.StatusOK, body: jobsBody(t, checkHead, 101, page1...)})
	f.set(jobsURI(7, 2), checkResponse{status: http.StatusOK, body: jobsBody(t, checkHead, 101, page2...)})
	got, err := r.ReconcileRequiredCheck(context.Background(), testRepo, checkBase, checkHead)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.RequiredCheckPassed {
		t.Errorf("state = %q, want passed from the second page's job", got.State)
	}
}

// TestReconcileRequiredCheckRefusesUntrustedResponses: a response that names
// another head or run, hides part of a list, or exceeds a bound is an error,
// never a reading.
func TestReconcileRequiredCheckRefusesUntrustedResponses(t *testing.T) {
	t.Parallel()
	otherHead := strings.Repeat("2", 40)
	foreignRunJob := checkJob(11, 8, "required", "success", 4, "success")
	manyRules := `[` + strings.TrimSuffix(strings.Repeat(`{"type":"pull_request"},`, 100), ",") + `]`
	manyRuns := map[string]any{"total_count": 11, "workflow_runs": []any{}}

	cases := []struct {
		name string
		uri  string
		resp checkResponse
	}{
		{name: "a run on another head", uri: runsURI, resp: checkResponse{status: http.StatusOK, body: runsBody(t, otherHead, 7)}},
		{name: "a run listed twice", uri: runsURI, resp: checkResponse{status: http.StatusOK, body: runsBody(t, checkHead, 7, 7)}},
		{name: "more runs than the bound", uri: runsURI, resp: checkResponse{status: http.StatusOK, body: mustJSON(t, manyRuns)}},
		{name: "a job on another head", uri: jobsURI(7, 1), resp: checkResponse{status: http.StatusOK, body: jobsBody(t, otherHead, 4, issue3591Jobs()...)}},
		{name: "a job of another run", uri: jobsURI(7, 1), resp: checkResponse{status: http.StatusOK, body: jobsBody(t, checkHead, 1, foreignRunJob)}},
		{name: "fewer jobs than the count", uri: jobsURI(7, 1), resp: checkResponse{status: http.StatusOK, body: jobsBody(t, checkHead, 5, issue3591Jobs()...)}},
		{name: "more jobs than the bound", uri: jobsURI(7, 1), resp: checkResponse{status: http.StatusOK, body: jobsBody(t, checkHead, 301, issue3591Jobs()...)}},
		{name: "a full page of rules", uri: rulesURI, resp: checkResponse{status: http.StatusOK, body: manyRules}},
		{name: "a rules object, not a list", uri: rulesURI, resp: checkResponse{status: http.StatusOK, body: `null`}},
		{name: "a server error on protection", uri: protectionURI, resp: checkResponse{status: http.StatusInternalServerError, body: `{}`}},
		{name: "a forbidden runs read", uri: runsURI, resp: checkResponse{status: http.StatusForbidden, body: `{"message":"Resource not accessible by integration"}`}},
		{name: "a missing runs list", uri: runsURI, resp: checkResponse{status: http.StatusNotFound, body: `{}`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, r := newCheckForge(t)
			f.set(tc.uri, tc.resp)
			if got, err := r.ReconcileRequiredCheck(context.Background(), testRepo, checkBase, checkHead); err == nil {
				t.Errorf("observation = %+v, want an error", got)
			}
		})
	}
}

func TestReconcileRequiredCheckRefusesUnsolicited304(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	t.Cleanup(srv.Close)
	r := publish.NewReconciler(testTokenSource(), srv.Client(), srv.URL)
	if _, err := r.ReconcileRequiredCheck(context.Background(), testRepo, checkBase, checkHead); err == nil {
		t.Error("unsolicited 304 accepted, want error")
	}
}
