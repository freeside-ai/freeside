package publish

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Required-check reads (issue #3598): which checks a pull request's base
// branch requires, and the GitHub Actions jobs on the published head that
// produce them. The reconciler reads them on the publication token, which
// already holds actions:read and administration:read. Every response is
// untrusted: it decodes into closed structs, must name the head and run the
// request asked about, and is bounded before it is classified.
const (
	maxRequiredCheckNames = 20
	maxCheckRuns          = 10
	maxCheckJobs          = 300
	checkJobsPageSize     = 100
	checkRulesPageSize    = 100
)

// CheckRun names one workflow run attempt on the head.
type CheckRun struct {
	ID      int64
	Attempt int
}

// CheckJob is one GitHub Actions job on the head, as the jobs endpoint
// reported it: the latest attempt of each job in its run.
type CheckJob struct {
	ID          int64
	RunID       int64
	RunAttempt  int
	Name        string
	Status      string
	Conclusion  string
	RunnerID    int64
	Steps       []string // each step's conclusion, "" while it runs
	CompletedAt time.Time
}

// FailedCheckJob is one failed job a RequiredCheckObservation names. Name is
// screened (domain.ScreenRequiredCheckJobName); the IDs let a later read
// fetch the job's log.
type FailedCheckJob struct {
	ID         int64
	RunID      int64
	RunAttempt int
	Name       string
}

// RequiredCheckObservation is the classified reading of a head's required
// check. None reports that the base branch requires no check, so there is
// nothing to show; the other fields are then zero.
type RequiredCheckObservation struct {
	None         bool
	State        domain.RequiredCheckState
	FailureClass *domain.CheckFailureClass
	// FailedJobs are every failed job in the runs that hold a failed
	// required check, earliest completion first, capped to
	// domain.MaxRequiredCheckFailedJobs.
	FailedJobs []FailedCheckJob
	// FailedRuns are those runs' attempts, by run ID: what a rerun reruns
	// and what keys one failure.
	FailedRuns []CheckRun
}

// ReconcileRequiredCheck reads which checks baseRef requires in repo and the
// state of each on headSHA, and classifies them. Each GET is conditional on
// its own cached validator, as the other reconciler reads are.
func (r *Reconciler) ReconcileRequiredCheck(
	ctx context.Context, repo, baseRef, headSHA string,
) (RequiredCheckObservation, error) {
	ref, err := parseRepo(repo)
	if err != nil {
		return RequiredCheckObservation{}, fmt.Errorf("reconcile required check: %w", err)
	}
	if baseRef == "" || headSHA == "" {
		return RequiredCheckObservation{}, errors.New("reconcile required check: empty base or head")
	}
	names, err := r.requiredCheckNames(ctx, ref, baseRef)
	if err != nil {
		return RequiredCheckObservation{}, fmt.Errorf("reconcile required check: %w", err)
	}
	if len(names) == 0 {
		return RequiredCheckObservation{None: true}, nil
	}
	jobs, err := r.headJobs(ctx, ref, headSHA)
	if err != nil {
		return RequiredCheckObservation{}, fmt.Errorf("reconcile required check: %w", err)
	}
	return ClassifyRequiredCheck(names, jobs), nil
}

// Closed decode shapes for the required-check reads. decodeResponse admits
// unknown fields; these name only what the reads use.
type (
	requiredStatusChecksResponse struct {
		Contexts []string `json:"contexts"`
		Checks   []struct {
			Context string `json:"context"`
		} `json:"checks"`
	}
	branchRule struct {
		Type       string `json:"type"`
		Parameters struct {
			RequiredStatusChecks []struct {
				Context string `json:"context"`
			} `json:"required_status_checks"`
		} `json:"parameters"`
	}
	workflowRunsResponse struct {
		TotalCount   *int `json:"total_count"`
		WorkflowRuns []struct {
			ID         int64  `json:"id"`
			HeadSHA    string `json:"head_sha"`
			RunAttempt int    `json:"run_attempt"`
		} `json:"workflow_runs"`
	}
	runJobsResponse struct {
		TotalCount *int `json:"total_count"`
		Jobs       []struct {
			ID          int64      `json:"id"`
			RunID       int64      `json:"run_id"`
			RunAttempt  int        `json:"run_attempt"`
			HeadSHA     string     `json:"head_sha"`
			Name        string     `json:"name"`
			Status      string     `json:"status"`
			Conclusion  *string    `json:"conclusion"`
			RunnerID    *int64     `json:"runner_id"`
			CompletedAt *time.Time `json:"completed_at"`
			Steps       []struct {
				Conclusion *string `json:"conclusion"`
			} `json:"steps"`
		} `json:"jobs"`
	}
)

// requiredCheckNames is the union of the contexts classic branch protection
// and rulesets require on baseRef, sorted. A 404 from either source means it
// requires nothing; so does a private repository's plan that lacks the
// feature.
func (r *Reconciler) requiredCheckNames(ctx context.Context, repo repoRef, baseRef string) ([]string, error) {
	branch := url.PathEscape(baseRef)
	names := map[string]bool{}

	protectionPath := "/repos/" + repo.path() + "/branches/" + branch + "/protection/required_status_checks"
	protection, status, err := conditionalGet[requiredStatusChecksResponse](ctx, r, repo, protectionPath)
	if err != nil {
		return nil, err
	}
	if status == http.StatusOK {
		for _, name := range protection.Contexts {
			names[name] = true
		}
		for _, check := range protection.Checks {
			names[check.Context] = true
		}
	}

	rulesPath := fmt.Sprintf("/repos/%s/rules/branches/%s?per_page=%d", repo.path(), branch, checkRulesPageSize)
	rules, status, err := conditionalGet[[]branchRule](ctx, r, repo, rulesPath)
	if err != nil {
		return nil, err
	}
	if status == http.StatusOK {
		if rules == nil {
			return nil, errors.New("branch rules response is not a list")
		}
		// A full page may hide more rules on the next one.
		if len(rules) >= checkRulesPageSize {
			return nil, fmt.Errorf("branch rules reach the %d-rule read bound", checkRulesPageSize)
		}
		for _, rule := range rules {
			if rule.Type != "required_status_checks" {
				continue
			}
			for _, check := range rule.Parameters.RequiredStatusChecks {
				names[check.Context] = true
			}
		}
	}

	delete(names, "")
	if len(names) > maxRequiredCheckNames {
		return nil, fmt.Errorf("base branch requires %d checks, over the %d-check read bound",
			len(names), maxRequiredCheckNames)
	}
	return slices.Sorted(maps.Keys(names)), nil
}

// headJobs reads every pull_request workflow run on headSHA and the latest
// attempt of each of their jobs.
func (r *Reconciler) headJobs(ctx context.Context, repo repoRef, headSHA string) ([]CheckJob, error) {
	runsPath := fmt.Sprintf("/repos/%s/actions/runs?head_sha=%s&event=pull_request&per_page=%d",
		repo.path(), url.QueryEscape(headSHA), maxCheckRuns)
	runs, status, err := conditionalGet[workflowRunsResponse](ctx, r, repo, runsPath)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, &APIError{Status: status, RequestPath: runsPath}
	}
	if runs.TotalCount == nil || runs.WorkflowRuns == nil {
		return nil, errors.New("workflow runs response carries no total_count or workflow_runs")
	}
	if *runs.TotalCount > maxCheckRuns || len(runs.WorkflowRuns) != *runs.TotalCount {
		return nil, fmt.Errorf("head lists %d of %d workflow runs; the read bound is %d",
			len(runs.WorkflowRuns), *runs.TotalCount, maxCheckRuns)
	}
	var jobs []CheckJob
	seenRuns := map[int64]bool{}
	for _, run := range runs.WorkflowRuns {
		// Returned-object boundary: a run on another head, or one listed
		// twice, must not speak for this one.
		if run.HeadSHA != headSHA {
			return nil, fmt.Errorf("workflow run %d is on head %q, not %q", run.ID, run.HeadSHA, headSHA)
		}
		if run.ID <= 0 || run.RunAttempt <= 0 || seenRuns[run.ID] {
			return nil, fmt.Errorf("workflow run %d attempt %d is malformed or repeated", run.ID, run.RunAttempt)
		}
		seenRuns[run.ID] = true
		runJobs, err := r.runJobs(ctx, repo, run.ID, headSHA)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, runJobs...)
		if len(jobs) > maxCheckJobs {
			return nil, fmt.Errorf("head has over %d jobs, the read bound", maxCheckJobs)
		}
	}
	return jobs, nil
}

// runJobs reads the latest attempt of each job in one run, page by page.
func (r *Reconciler) runJobs(ctx context.Context, repo repoRef, runID int64, headSHA string) ([]CheckJob, error) {
	var jobs []CheckJob
	seen := map[int64]bool{}
	for page := 1; page <= maxCheckJobs/checkJobsPageSize; page++ {
		path := fmt.Sprintf("/repos/%s/actions/runs/%d/jobs?filter=latest&per_page=%d&page=%d",
			repo.path(), runID, checkJobsPageSize, page)
		decoded, status, err := conditionalGet[runJobsResponse](ctx, r, repo, path)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, &APIError{Status: status, RequestPath: path}
		}
		if decoded.TotalCount == nil || decoded.Jobs == nil {
			return nil, fmt.Errorf("jobs of run %d carry no total_count or jobs", runID)
		}
		if *decoded.TotalCount > maxCheckJobs {
			return nil, fmt.Errorf("run %d has %d jobs, over the %d-job read bound", runID, *decoded.TotalCount, maxCheckJobs)
		}
		for _, job := range decoded.Jobs {
			// Returned-object boundary, as for runs.
			if job.RunID != runID || job.HeadSHA != headSHA {
				return nil, fmt.Errorf("job %d names run %d on head %q, not run %d on %q",
					job.ID, job.RunID, job.HeadSHA, runID, headSHA)
			}
			if job.ID <= 0 || seen[job.ID] || job.Name == "" {
				return nil, fmt.Errorf("job %d of run %d is malformed or repeated", job.ID, runID)
			}
			seen[job.ID] = true
			out := CheckJob{
				ID: job.ID, RunID: job.RunID, RunAttempt: job.RunAttempt, Name: job.Name,
				Status: job.Status, Steps: make([]string, len(job.Steps)),
			}
			if job.Conclusion != nil {
				out.Conclusion = *job.Conclusion
			}
			if job.RunnerID != nil {
				out.RunnerID = *job.RunnerID
			}
			if job.CompletedAt != nil {
				out.CompletedAt = job.CompletedAt.UTC()
			}
			for i, step := range job.Steps {
				if step.Conclusion != nil {
					out.Steps[i] = *step.Conclusion
				}
			}
			jobs = append(jobs, out)
		}
		if len(jobs) >= *decoded.TotalCount || len(decoded.Jobs) < checkJobsPageSize {
			if len(jobs) != *decoded.TotalCount {
				return nil, fmt.Errorf("run %d listed %d of %d jobs", runID, len(jobs), *decoded.TotalCount)
			}
			return jobs, nil
		}
	}
	return nil, fmt.Errorf("run %d jobs exceed the %d-job read bound", runID, maxCheckJobs)
}

// conditionalGet sends one GET, conditional on the validator a previous 200
// for the same path left, and decodes the 200 into T, or returns the value a
// solicited 304 confirms. A 404, and a 403 a plan restriction explains,
// return their status and a zero T; any other status is an APIError. One
// path always decodes into one T, so the cached value's type is the caller's.
func conditionalGet[T any](ctx context.Context, r *Reconciler, repo repoRef, path string) (T, int, error) {
	var zero T
	key := repo.path() + "\x00" + path
	r.mu.Lock()
	entry, cached := r.checkReads[key]
	epoch := r.cacheEpoch
	r.mu.Unlock()
	resp, err := r.forge.do(ctx, http.MethodGet, repo, path, entry.etag, nil)
	if err != nil {
		return zero, 0, err
	}
	defer drainAndClose(resp.Body)
	switch resp.StatusCode {
	case http.StatusNotModified:
		// See ReconcileRef: an unsolicited 304 is refused, not trusted.
		if !cached {
			return zero, 0, fmt.Errorf("%s: 304 for a request that sent no validator", path)
		}
		value, ok := entry.value.(T)
		if !ok {
			return zero, 0, fmt.Errorf("%s: cached value is a %T", path, entry.value)
		}
		return value, http.StatusOK, nil
	case http.StatusOK:
		var value T
		if err := decodeResponse(resp.Body, &value); err != nil {
			return zero, 0, fmt.Errorf("decode %s: %w", path, err)
		}
		r.mu.Lock()
		if epoch == r.cacheEpoch {
			if tag := resp.Header.Get("ETag"); tag != "" {
				r.checkReads[key] = checkReadCacheEntry{etag: tag, value: value}
			} else {
				delete(r.checkReads, key)
			}
		}
		r.mu.Unlock()
		return value, http.StatusOK, nil
	case http.StatusNotFound:
		return zero, http.StatusNotFound, nil
	}
	if auditFeatureUnavailable(resp) {
		return zero, http.StatusNotFound, nil
	}
	return zero, 0, &APIError{Status: resp.StatusCode, RequestPath: path}
}

// checkReadCacheEntry caches one required-check GET's validator and decoded
// response.
type checkReadCacheEntry struct {
	etag  string
	value any
}

// EvictRequiredCheck drops every cached required-check read for repo, so a
// pass after a rerun or a concluded item reads afresh.
func (r *Reconciler) EvictRequiredCheck(repo string) {
	ref, err := parseRepo(repo)
	if err != nil {
		return // no read can have cached anything under a malformed repo
	}
	prefix := ref.path() + "\x00"
	r.mu.Lock()
	r.cacheEpoch++
	maps.DeleteFunc(r.checkReads, func(key string, _ checkReadCacheEntry) bool {
		return strings.HasPrefix(key, prefix)
	})
	r.mu.Unlock()
}

// ClassifyRequiredCheck derives a head's required-check reading from the
// names its base branch requires and the jobs on the head. It is pure.
//
// A required name with no job on the head, or whose job hasn't completed,
// is pending; success, skipped, and neutral pass; any other conclusion
// fails. The reading fails if any required check failed, else is pending if
// any is pending, else passes.
//
// A failure's class reads the failed jobs of the runs that hold a failed
// required check. Infrastructure is checked first: a failed job with no
// runner and no steps never ran. In #3591, two jobs that only waited on such
// a job reported failure with steps that ran, so a failed step alone is no
// proof of a code failure.
func ClassifyRequiredCheck(required []string, jobs []CheckJob) RequiredCheckObservation {
	if len(required) == 0 {
		return RequiredCheckObservation{None: true}
	}
	// The latest job by ID speaks for a name: a rerun's job has a larger ID.
	latest := map[string]CheckJob{}
	for _, job := range jobs {
		if prior, ok := latest[job.Name]; !ok || job.ID > prior.ID {
			latest[job.Name] = job
		}
	}
	anyPending := false
	failedRunIDs := map[int64]bool{}
	for _, name := range required {
		job, ok := latest[name]
		switch {
		case !ok || job.Status != "completed":
			anyPending = true
		case passedConclusion(job.Conclusion):
		default:
			failedRunIDs[job.RunID] = true
		}
	}
	if len(failedRunIDs) == 0 {
		if anyPending {
			return RequiredCheckObservation{State: domain.RequiredCheckPending}
		}
		return RequiredCheckObservation{State: domain.RequiredCheckPassed}
	}

	isRequired := map[string]bool{}
	for _, name := range required {
		isRequired[name] = true
	}
	var failed []CheckJob
	runs := map[int64]int{}
	for _, job := range jobs {
		if !failedRunIDs[job.RunID] || latest[job.Name].ID != job.ID {
			continue
		}
		if prior, ok := runs[job.RunID]; !ok || job.RunAttempt > prior {
			runs[job.RunID] = job.RunAttempt
		}
		if job.Status == "completed" && !passedConclusion(job.Conclusion) {
			failed = append(failed, job)
		}
	}
	// The required check's own job only says that something it waits on
	// failed; name it only when nothing else failed.
	if slices.ContainsFunc(failed, func(job CheckJob) bool { return !isRequired[job.Name] }) {
		failed = slices.DeleteFunc(failed, func(job CheckJob) bool { return isRequired[job.Name] })
	}
	slices.SortFunc(failed, func(a, b CheckJob) int {
		return cmp.Or(a.CompletedAt.Compare(b.CompletedAt), cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})

	class := classifyFailedJobs(failed)
	observation := RequiredCheckObservation{State: domain.RequiredCheckFailed, FailureClass: &class}
	for _, job := range failed[:min(len(failed), domain.MaxRequiredCheckFailedJobs)] {
		observation.FailedJobs = append(observation.FailedJobs, FailedCheckJob{
			ID: job.ID, RunID: job.RunID, RunAttempt: job.RunAttempt,
			Name: domain.ScreenRequiredCheckJobName(job.Name),
		})
	}
	for id, attempt := range runs {
		observation.FailedRuns = append(observation.FailedRuns, CheckRun{ID: id, Attempt: attempt})
	}
	slices.SortFunc(observation.FailedRuns, func(a, b CheckRun) int { return cmp.Compare(a.ID, b.ID) })
	return observation
}

// passedConclusion reports whether a completed job's conclusion lets the
// check pass. Every other conclusion fails it, including one GitHub adds
// later or one that waits on a person (action_required), so a failed
// reading always names the jobs that failed it.
func passedConclusion(conclusion string) bool {
	switch conclusion {
	case "success", "skipped", "neutral":
		return true
	}
	return false
}

func classifyFailedJobs(failed []CheckJob) domain.CheckFailureClass {
	for _, job := range failed {
		if job.RunnerID == 0 && len(job.Steps) == 0 {
			return domain.CheckFailureInfrastructure
		}
	}
	for _, job := range failed {
		if job.Conclusion == "failure" && slices.Contains(job.Steps, "failure") {
			return domain.CheckFailureCode
		}
	}
	return domain.CheckFailureOther
}
