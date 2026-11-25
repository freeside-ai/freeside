package publish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// CheckRerunKind is the package-owned outbox intent to rerun the failed jobs
// of one workflow run attempt on a ready item's head, because its required
// check failed on infrastructure (#3598). The reconciler enqueues it in the
// transaction that raises the card's rerun count; CheckRerunner sends it.
const CheckRerunKind = "publish.check_rerun"

type checkRerun struct {
	ItemID        domain.ItemID `json:"item_id"`
	Repo          string        `json:"repo"`
	PRNumber      int           `json:"pr_number"`
	HeadSHA       string        `json:"head_sha"`
	WorkflowRunID int64         `json:"workflow_run_id"`
	Attempt       int           `json:"attempt"`
}

// CheckRerunKey is the outbox key of the intent to rerun one attempt of a
// workflow run for item. It names the attempt, so each attempt is rerun at
// most once.
func CheckRerunKey(item domain.ItemID, workflowRunID int64, attempt int) string {
	return fmt.Sprintf("check-rerun/%s/%d/%d", item, workflowRunID, attempt)
}

// CheckRerunIntent is the key and payload of the intent to rerun run's
// failed jobs on the pull request of a ready item bound to head.
func CheckRerunIntent(item domain.ItemID, repo string, prNumber int, head string, run CheckRun) (string, []byte, error) {
	intent := checkRerun{
		ItemID: item, Repo: repo, PRNumber: prNumber, HeadSHA: head,
		WorkflowRunID: run.ID, Attempt: run.Attempt,
	}
	if err := intent.validate(); err != nil {
		return "", nil, err
	}
	payload, err := json.Marshal(intent)
	if err != nil {
		return "", nil, err
	}
	return CheckRerunKey(item, run.ID, run.Attempt), payload, nil
}

func (c checkRerun) validate() error {
	if c.ItemID == "" || c.PRNumber < 1 || c.HeadSHA == "" || c.WorkflowRunID < 1 || c.Attempt < 1 {
		return errors.New("check rerun: incomplete intent")
	}
	if _, err := parseRepo(c.Repo); err != nil {
		return errors.New("check rerun: invalid repository")
	}
	return nil
}

func decodeCheckRerun(entry store.QueueEntry) (checkRerun, error) {
	var c checkRerun
	if entry.Kind != CheckRerunKind ||
		strictjson.Decode(entry.Payload, &c, strictjson.RejectInvalidUTF8, strictjson.NoLimit) != nil {
		return checkRerun{}, errors.New("check rerun: invalid intent record")
	}
	if err := c.validate(); err != nil {
		return checkRerun{}, err
	}
	if entry.IdempotencyKey != CheckRerunKey(c.ItemID, c.WorkflowRunID, c.Attempt) {
		return checkRerun{}, errors.New("check rerun: intent key does not match its payload")
	}
	return c, nil
}

// CheckRerunBackupPayloadDigests validates an intent before backup accepts
// the outbox as complete. The intent references no artifact.
func CheckRerunBackupPayloadDigests(entry store.QueueEntry) ([]domain.Digest, error) {
	if _, err := decodeCheckRerun(entry); err != nil {
		return nil, err
	}
	return nil, nil
}

// CheckRerunner sends pending rerun intents: POST
// /repos/{repo}/actions/runs/{id}/rerun-failed-jobs, on a token that holds
// only CheckRerunPermissions. Before it sends, it re-reads the intent's ready
// item and the run, and completes the intent without sending when the item
// left the head or the run is no longer the failed attempt the intent names.
// That read, not the outbox mark, is what keeps a crash between the POST and
// the mark from rerunning the attempt twice. It writes to no other endpoint.
type CheckRerunner struct {
	store *store.Store
	forge *forge
}

// NewCheckRerunner wires the rerunner. tokens must mint only
// CheckRerunPermissions (CheckRerunTokenSource).
func NewCheckRerunner(s *store.Store, tokens TokenSource, client *http.Client, baseURL string) (*CheckRerunner, error) {
	if s == nil || tokens == nil || client == nil || strings.TrimSpace(baseURL) == "" {
		return nil, errors.New("check rerun: nil or empty dependency")
	}
	return &CheckRerunner{store: s, forge: newForge(tokens, client, strings.TrimRight(baseURL, "/"))}, nil
}

// Run sends on the supplied interval until ctx ends. Pass errors are reported
// without stopping later passes.
func (r *CheckRerunner) Run(ctx context.Context, interval time.Duration, report func(error)) error {
	if interval <= 0 {
		return errors.New("check rerun: interval must be positive")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var running sync.Mutex
	for {
		if running.TryLock() {
			go func() {
				defer running.Unlock()
				if err := r.Pass(ctx); err != nil && ctx.Err() == nil && report != nil {
					report(err)
				}
			}()
		}
		select {
		case <-ctx.Done():
			running.Lock() // wait for the pass in flight
			return nil
		case <-ticker.C:
		}
	}
}

// Pass sends every pending intent once. Errors crossing this boundary are
// fixed text: transports and token sources may echo credentials. A refused
// mint, which an installation that has not accepted actions: write gets,
// leaves every intent pending for the next pass.
func (r *CheckRerunner) Pass(ctx context.Context) error {
	var pending []store.QueueEntry
	if err := r.store.Read(ctx, func(tx *store.ReadTx) (err error) {
		pending, err = tx.ListPendingOutbox(ctx, CheckRerunKind)
		return err
	}); err != nil {
		return errors.New("check rerun: list intents failed")
	}
	failed := 0
	for _, entry := range pending {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := r.send(ctx, entry); err != nil {
			failed++
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if failed > 0 {
		return fmt.Errorf("check rerun: %d of %d intents failed; retry on next pass", failed, len(pending))
	}
	return nil
}

func (r *CheckRerunner) send(ctx context.Context, entry store.QueueEntry) error {
	intent, err := decodeCheckRerun(entry)
	if err != nil {
		return err
	}
	current, err := r.itemOnHead(ctx, intent)
	if err != nil {
		return err
	}
	if current {
		repo, err := parseRepo(intent.Repo)
		if err != nil {
			return err
		}
		due, err := r.forge.rerunDue(ctx, repo, intent)
		if err != nil {
			return err
		}
		if due {
			if err := r.forge.rerunFailedJobs(ctx, repo, intent.WorkflowRunID); err != nil {
				return err
			}
		}
	}
	return r.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.MarkOutboxDispatched(ctx, entry.IdempotencyKey)
	})
}

// itemOnHead reports whether the intent's item is still an open ready item
// on the intent's pull request and head. A rerun for anything else is
// stale and completes without sending.
func (r *CheckRerunner) itemOnHead(ctx context.Context, intent checkRerun) (bool, error) {
	var item domain.AttentionItem
	err := r.store.Read(ctx, func(tx *store.ReadTx) (err error) {
		item, err = tx.GetAttentionItem(ctx, intent.ItemID)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return item.Type == domain.AttentionReadyForFinalReview && item.Status == domain.StatusOpen &&
		item.PRHeadSHA == intent.HeadSHA && item.PRReference != nil &&
		item.PRReference.Repo == intent.Repo && item.PRReference.Number == intent.PRNumber, nil
}

// rerunDue reads the workflow run and reports whether it is still the
// completed attempt the intent names. A later attempt or a run in progress
// means the rerun already happened.
func (f *forge) rerunDue(ctx context.Context, repo repoRef, intent checkRerun) (bool, error) {
	path := fmt.Sprintf("/repos/%s/actions/runs/%d", repo.path(), intent.WorkflowRunID)
	resp, err := f.do(ctx, http.MethodGet, repo, path, "", nil)
	if err != nil {
		return false, fmt.Errorf("read workflow run: %w", err)
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("read workflow run: %w", &APIError{Status: resp.StatusCode, RequestPath: path})
	}
	var run struct {
		ID         int64  `json:"id"`
		HeadSHA    string `json:"head_sha"`
		RunAttempt int    `json:"run_attempt"`
		Status     string `json:"status"`
	}
	if err := decodeResponse(resp.Body, &run); err != nil {
		return false, fmt.Errorf("decode workflow run: %w", err)
	}
	if run.ID != intent.WorkflowRunID || run.HeadSHA != intent.HeadSHA || run.RunAttempt < intent.Attempt {
		return false, fmt.Errorf("workflow run %d attempt %d on %q is not the intent's", run.ID, run.RunAttempt, run.HeadSHA)
	}
	return run.RunAttempt == intent.Attempt && run.Status == "completed", nil
}

// rerunFailedJobs asks GitHub to rerun a run's failed jobs. A refusal that
// says the run is already running is the state the call asks for.
func (f *forge) rerunFailedJobs(ctx context.Context, repo repoRef, runID int64) error {
	path := fmt.Sprintf("/repos/%s/actions/runs/%d/rerun-failed-jobs", repo.path(), runID)
	resp, err := f.do(ctx, http.MethodPost, repo, path, "", nil)
	if err != nil {
		return fmt.Errorf("rerun failed jobs: %w", err)
	}
	defer drainAndClose(resp.Body)
	switch resp.StatusCode {
	case http.StatusCreated:
		return nil
	case http.StatusForbidden:
		var refusal struct {
			Message string `json:"message"`
		}
		if decodeResponse(resp.Body, &refusal) == nil && strings.Contains(refusal.Message, "already running") {
			return nil
		}
	}
	return fmt.Errorf("rerun failed jobs: %w", &APIError{Status: resp.StatusCode, RequestPath: path})
}

// CheckRerunTokenSource caches only the rerun grant, separately from
// publication, filing, label, and read credentials. Each call revalidates
// trust and the App binding before it looks in the cache.
type CheckRerunTokenSource struct {
	minter *Minter
	now    func() time.Time
	// cache holds no lock across the authority resolution or the mint
	// (tokenCache).
	cache *tokenCache
}

func NewCheckRerunTokenSource(m *Minter, now func() time.Time) *CheckRerunTokenSource {
	return &CheckRerunTokenSource{minter: m, now: now, cache: newTokenCache(now)}
}

// Format renders the source without its cache.
func (*CheckRerunTokenSource) Format(f fmt.State, _ rune) {
	io.WriteString(f, "publish.CheckRerunTokenSource") //nolint:errcheck,gosec // fmt.State writes cannot be usefully handled
}

// Token mints only CheckRerunPermissions for the repository's current
// trusted installation. An installation that has not accepted actions: write
// refuses the mint.
func (s *CheckRerunTokenSource) Token(ctx context.Context, repo string) (InstallationToken, error) {
	if s == nil || s.minter == nil || s.now == nil {
		return InstallationToken{}, errors.New("check rerun token: nil dependency")
	}
	binding, parsed, repositoryID, _, err := s.minter.resolveTrusted(ctx, repo)
	if err != nil {
		return InstallationToken{}, err
	}
	key := tokenCacheKey{
		registrationID: binding.RegistrationID,
		installationID: binding.InstallationID,
		repositoryID:   repositoryID,
	}
	return s.cache.get(ctx, key, func() (InstallationToken, error) {
		return s.minter.mintResolved(ctx, binding, parsed, repositoryID,
			CheckRerunPermissions, checkRerunPermissionScopes)
	})
}
