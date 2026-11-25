package daemoncompose

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// jobLogReader reads the tail of a GitHub Actions job's log
// (publish.Reconciler.ReadJobLogTail).
type jobLogReader func(ctx context.Context, repo string, jobID int64) ([]byte, error)

// These stand in for a log the remediator can't be given, so it still
// learns which job failed. Recording them rather than retrying keeps a log
// the candidate made unreadable from holding the cycle back forever.
const (
	unavailableJobLog = "GitHub no longer keeps this job's log."
	tooLongJobLog     = "This job's log was too long to read."
	emptyJobLog       = "This job's log is empty."
)

// startCheckFailure starts the check_failure cycle for an open ready item
// whose required check failed on code (issue #3598). The probe runs first
// and reads only the store, so the failed jobs' logs are read and a write
// transaction is opened only when a cycle is due. The logs are untrusted
// candidate output: each is screened by domain.ScreenCheckFailureExcerpt
// before any of it is stored, and the engine re-proves every condition in
// the write transaction before it seals the authority. A log read that
// fails writes nothing, and the next pass offers the cycle again.
func (r *ActiveResourceReconciler) startCheckFailure(
	ctx context.Context, itemID domain.ItemID, binding domain.ReadyItemPRBinding,
	check publish.RequiredCheckObservation, observedAt time.Time,
) error {
	if r.readJobLog == nil || r.excerpts == nil {
		return nil
	}
	var due bool
	if err := r.Store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		due, err = engine.CheckFailureReentryDue(ctx, tx, itemID, r.readinessGate)
		return err
	}); err != nil || !due {
		return err
	}
	failure, err := r.checkFailureRecord(ctx, binding, check.FailedJobs)
	if err != nil {
		return err
	}
	return r.Store.Write(ctx, func(tx *store.WriteTx) error {
		started, err := engine.StartCheckFailureReentry(ctx, tx, itemID, failure, r.readinessGate)
		if err != nil || !started {
			return err
		}
		// The item is no longer ready, so its watches end with it.
		return concludePublicationSchedules(ctx, tx, itemID, observedAt)
	})
}

// checkFailureRecord reads, screens, and stores the log excerpts of the
// first domain.MaxCheckFailureJobs failed jobs, in the order the card names
// them, and returns the record that names them.
func (r *ActiveResourceReconciler) checkFailureRecord(
	ctx context.Context, binding domain.ReadyItemPRBinding, jobs []publish.FailedCheckJob,
) (domain.RequiredCheckFailure, error) {
	failure := domain.RequiredCheckFailure{
		Version: domain.RequiredCheckFailureVersion, RunID: binding.RunID,
		Repo: binding.Repo, PRNumber: binding.PRNumber, HeadSHA: binding.HeadSHA,
	}
	for _, job := range jobs[:min(len(jobs), domain.MaxCheckFailureJobs)] {
		log, err := r.readJobLog(ctx, binding.Repo, job.ID)
		switch {
		case errors.Is(err, publish.ErrJobLogUnavailable):
			log, err = []byte(unavailableJobLog), nil
		case errors.Is(err, publish.ErrJobLogTooLong):
			log, err = []byte(tooLongJobLog), nil
		case err == nil && len(bytes.TrimSpace(log)) == 0:
			log = []byte(emptyJobLog)
		}
		if err != nil {
			return domain.RequiredCheckFailure{}, fmt.Errorf("read log of job %d: %w", job.ID, err)
		}
		excerpt := []byte(domain.ScreenCheckFailureExcerpt(log))
		digest := domain.Digest(contentaddr.Sum(excerpt))
		if _, err := r.excerpts.Put(digest, bytes.NewReader(excerpt)); err != nil {
			return domain.RequiredCheckFailure{}, fmt.Errorf("store log excerpt of job %d: %w", job.ID, err)
		}
		failure.Jobs = append(failure.Jobs, domain.CheckFailureJob{
			Name: job.Name, JobID: job.ID, WorkflowRunID: job.RunID, Attempt: job.RunAttempt,
			ExcerptDigest: digest,
		})
	}
	return failure, nil
}
