package publish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/keyedrun"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const (
	// ExternalReplyIntentKind, ExternalReplyOutcomeKind, and
	// ExternalReviewThreadResolutionKind are package-owned outbox records.
	// They contain no artifact references.
	ExternalReplyIntentKind            = "publish.external_review_reply_intent"
	ExternalReplyOutcomeKind           = "publish.external_review_reply_outcome"
	ExternalReviewThreadResolutionKind = "publish.external_review_thread_resolution"
	externalReplySettle                = 10 * time.Minute
	externalReplyAttemptBound          = 3
)

type externalReplyIntent struct {
	ItemID      domain.ItemID            `json:"item_id"`
	FindingID   domain.FindingID         `json:"finding_id"`
	RunID       domain.RunID             `json:"run_id"`
	Round       int                      `json:"round"`
	Repo        string                   `json:"repo"`
	PRNumber    int                      `json:"pr_number"`
	Thread      string                   `json:"thread"`
	Disposition domain.ReviewDisposition `json:"disposition"`
	HeadSHA     string                   `json:"head_sha"`
	TextDigest  string                   `json:"text_digest"`
	Ruleset     domain.IssueTextRuleset  `json:"ruleset"`
	BotID       int64                    `json:"bot_id"`
	Before      []int64                  `json:"before"`
	CreatedAt   time.Time                `json:"created_at"`
}

func externalReplyKey(finding domain.FindingID, round int) string {
	return fmt.Sprintf("external-review-reply/%s/%d", finding, round)
}

func (i externalReplyIntent) key() string { return externalReplyKey(i.FindingID, i.Round) }

type externalReplyOutcome struct {
	FindingID domain.FindingID `json:"finding_id"`
	Round     int              `json:"round"`
	CommentID int64            `json:"comment_id"`
	// Code is empty for a posted reply, or one of the health diagnostic
	// codes below. Revocation is terminal without an attention item.
	Code string `json:"code"`
}

const (
	externalReplyRefused          = "external_review_reply_refused"
	externalReplyAmbiguous        = "external_review_reply_ambiguous"
	externalReplyUnreadable       = "external_review_reply_unreadable"
	externalReplyRevoked          = "external_review_reply_not_admitted"
	externalReplyThreadUnresolved = "external_review_thread_unresolved"
)

type externalReviewThreadResolution struct {
	FindingID domain.FindingID `json:"finding_id"`
	Round     int              `json:"round"`
	// Code is empty when the thread is resolved, or
	// externalReplyThreadUnresolved when resolution failed permanently.
	Code string `json:"code"`
}

func decodeExternalReplyIntent(entry store.QueueEntry) (externalReplyIntent, error) {
	var i externalReplyIntent
	if entry.Kind != ExternalReplyIntentKind || strictjson.Decode(entry.Payload, &i, strictjson.TolerateInvalidUTF8, strictjson.NoLimit) != nil ||
		i.ItemID == "" || i.FindingID == "" || i.RunID == "" || i.Round < 1 || i.PRNumber < 1 || i.BotID < 1 ||
		i.CreatedAt.IsZero() || i.TextDigest == "" || i.Ruleset != domain.IssueTextRulesetGitHubIssue1 ||
		entry.IdempotencyKey != i.key() {
		return i, errors.New("review reply: invalid intent")
	}
	if _, err := parseRepo(i.Repo); err != nil {
		return i, errors.New("review reply: invalid repository")
	}
	if _, err := parseExternalReplyThread(i.Thread); err != nil {
		return i, err
	}
	return i, nil
}

// ExternalReplyBackupPayloadDigests validates either self-contained reply
// record before backup accepts the outbox as complete.
func ExternalReplyBackupPayloadDigests(entry store.QueueEntry) ([]domain.Digest, error) {
	if entry.Kind == ExternalReplyIntentKind {
		_, err := decodeExternalReplyIntent(entry)
		return nil, err
	}
	_, err := decodeExternalReplyOutcome(entry)
	return nil, err
}

func decodeExternalReplyOutcome(entry store.QueueEntry) (externalReplyOutcome, error) {
	var o externalReplyOutcome
	if entry.Kind != ExternalReplyOutcomeKind || strictjson.Decode(entry.Payload, &o, strictjson.TolerateInvalidUTF8, strictjson.NoLimit) != nil ||
		o.FindingID == "" || o.Round < 1 || entry.IdempotencyKey != externalReplyKey(o.FindingID, o.Round)+"/outcome" ||
		(o.Code == "" && o.CommentID <= 0) || (o.Code != "" && o.Code != externalReplyRefused && o.Code != externalReplyAmbiguous && o.Code != externalReplyRevoked) {
		return o, errors.New("review reply: invalid outcome")
	}
	return o, nil
}

func decodeExternalReviewThreadResolution(entry store.QueueEntry) (externalReviewThreadResolution, error) {
	var r externalReviewThreadResolution
	if entry.Kind != ExternalReviewThreadResolutionKind || strictjson.Decode(entry.Payload, &r, strictjson.TolerateInvalidUTF8, strictjson.NoLimit) != nil ||
		r.FindingID == "" || r.Round < 1 || entry.IdempotencyKey != externalReplyKey(r.FindingID, r.Round)+"/resolve" ||
		(r.Code != "" && r.Code != externalReplyThreadUnresolved) {
		return r, errors.New("review reply: invalid thread resolution")
	}
	return r, nil
}

// ExternalReviewThreadResolutionBackupPayloadDigests validates a thread
// resolution record before backup accepts the outbox as complete.
func ExternalReviewThreadResolutionBackupPayloadDigests(entry store.QueueEntry) ([]domain.Digest, error) {
	_, err := decodeExternalReviewThreadResolution(entry)
	return nil, err
}

type externalReplyCollection struct {
	repositoryID int64
	prNumber     int
	root         int64
}

// replyCollection authenticates each intent against its own historical
// publication. Repository names can change; numeric identity cannot.
func replyCollection(ctx context.Context, tx *store.ReadTx, i externalReplyIntent) (externalReplyCollection, error) {
	binding, err := tx.GetReadyItemPRBinding(ctx, i.ItemID)
	if err != nil {
		return externalReplyCollection{}, err
	}
	if binding.ItemID != i.ItemID || binding.RunID != i.RunID || binding.Repo != i.Repo ||
		binding.PRNumber != i.PRNumber || binding.HeadSHA != i.HeadSHA {
		return externalReplyCollection{}, errors.New("review reply: pinned publication differs")
	}
	thread, err := parseExternalReplyThread(i.Thread)
	if err != nil {
		return externalReplyCollection{}, err
	}
	return externalReplyCollection{binding.RepositoryID, binding.PRNumber, thread.root}, nil
}

func (r *ExternalReviewReplier) priorAmbiguousReply(ctx context.Context, i externalReplyIntent) (bool, error) {
	var ambiguous bool
	err := r.store.Read(ctx, func(tx *store.ReadTx) error {
		collection, err := replyCollection(ctx, tx, i)
		if err != nil {
			return err
		}
		rows, err := tx.ListDispatchedOutbox(ctx, ExternalReplyOutcomeKind)
		if err != nil {
			return err
		}
		for _, row := range rows {
			outcome, err := decodeExternalReplyOutcome(row)
			if err != nil {
				return err
			}
			if outcome.Code != externalReplyAmbiguous {
				continue
			}
			entry, err := tx.GetOutbox(ctx, externalReplyKey(outcome.FindingID, outcome.Round))
			if err != nil {
				return err
			}
			other, err := decodeExternalReplyIntent(entry)
			if err != nil {
				return err
			}
			if !entry.Dispatched() || other.FindingID != outcome.FindingID || other.Round != outcome.Round {
				return errors.New("review reply: ambiguous outcome differs from intent")
			}
			previous, err := replyCollection(ctx, tx, other)
			if err != nil {
				return err
			}
			ambiguous = ambiguous || previous == collection
		}
		return nil
	})
	return ambiguous, err
}

// ExternalReviewReplier publishes recorded dispositions and reconciles
// uncertain sends. One daemon owns the store.
type ExternalReviewReplier struct {
	store    *store.Store
	forge    *forge
	identity FollowUpFilingIdentitySource
	now      func() time.Time
	// projects runs each project's dispositions apart, so one repository's
	// stalled GitHub call holds up only its own replies (#3215). An intent
	// belongs to one project, so only that project's run touches its keys.
	projects keyedrun.Group[domain.ProjectID]
	// mu guards only the three maps below and is never held across a call.
	mu          sync.Mutex
	settleAfter map[string]time.Time
	attempts    map[string]int
	retryAfter  map[string]time.Time
}

// NewExternalReviewReplier uses the publish token's pull_requests permission.
func NewExternalReviewReplier(s *store.Store, tokens TokenSource, client *http.Client, baseURL string, identity FollowUpFilingIdentitySource, now func() time.Time) (*ExternalReviewReplier, error) {
	if s == nil || tokens == nil || client == nil || strings.TrimSpace(baseURL) == "" || identity == nil || now == nil {
		return nil, errors.New("review replier: nil or empty dependency")
	}
	return &ExternalReviewReplier{
		store: s, forge: newForge(tokens, client, strings.TrimRight(baseURL, "/")), identity: identity, now: now,
		settleAfter: map[string]time.Time{}, attempts: map[string]int{}, retryAfter: map[string]time.Time{},
	}, nil
}

// Run recovers at startup, then sweeps on the supplied interval. Pass errors
// are reported without stopping future recovery; cancellation ends the loop.
func (r *ExternalReviewReplier) Run(ctx context.Context, interval time.Duration, report func(error)) error {
	if interval <= 0 {
		return errors.New("review replier: interval must be positive")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	// Each pass runs on its own goroutine, so a pass waiting on a stalled
	// project never delays the next tick; that pass skips the busy project.
	var passes sync.WaitGroup
	defer passes.Wait()
	for {
		passes.Go(func() {
			if err := r.Pass(ctx); err != nil && ctx.Err() == nil && report != nil {
				report(err)
			}
		})
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Pass reconstructs source authority every time. It drives each project's
// dispositions on its own goroutine and returns once the projects it started
// finish; a project whose previous run has not returned is skipped. Errors
// crossing this public boundary are fixed text: transports and token sources
// may echo credentials.
func (r *ExternalReviewReplier) Pass(ctx context.Context) error {
	if err := r.pass(ctx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("external review replies: pass failed; retry on next sweep")
	}
	return nil
}

func (r *ExternalReviewReplier) pass(ctx context.Context) error {
	var runs []store.Snapshotted[domain.Run]
	if err := r.store.Read(ctx, func(tx *store.ReadTx) error { var err error; runs, err = tx.ListRuns(ctx); return err }); err != nil {
		return err
	}
	parts := keyedrun.Partition(runs, func(s store.Snapshotted[domain.Run]) domain.ProjectID { return s.Value.ProjectID })
	err := keyedrun.Run(&r.projects, parts, func(runs []store.Snapshotted[domain.Run]) error {
		return r.driveRuns(ctx, runs)
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// driveRuns drives one project's runs in order.
func (r *ExternalReviewReplier) driveRuns(ctx context.Context, runs []store.Snapshotted[domain.Run]) error {
	var errs []error
	for _, snapshot := range runs {
		run := snapshot.Value
		var dispositions []domain.ExternalFindingDisposition
		err := r.store.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			dispositions, err = tx.ListExternalFindingDispositions(ctx, run.ID)
			return err
		})
		if err != nil {
			errs = append(errs, err, r.notice(ctx, run, "unreadable", externalReplyUnreadable, "External review dispositions could not be read. No replies were sent for this run."))
			continue
		}
		for _, d := range dispositions {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err := r.drive(ctx, run, d); err != nil && !errors.Is(err, store.ErrTaskWorkflowClosed) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

type externalReplySource struct {
	finding domain.Finding
	binding domain.ReadyItemPRBinding
	// adjudication is the entry that decided a declined or deferred outcome,
	// and nil for a fixed one.
	adjudication *domain.FindingAdjudicationEntry
}

// source is the current publication and active-profile gate. A fixed reply
// cannot precede publication of a head reviewed in the remediation round.
func replySource(ctx context.Context, tx *store.ReadTx, d domain.ExternalFindingDisposition) (externalReplySource, error) {
	var source externalReplySource
	item, err := tx.PublishedProductionReadyItemID(ctx, d.RunID)
	if err != nil {
		return source, err
	}
	source.binding, err = tx.GetReadyItemPRBinding(ctx, item)
	if err != nil {
		return source, err
	}
	source.finding, err = tx.GetAdmittedExternalFinding(ctx, d.FindingID)
	if err != nil {
		return source, err
	}
	source.adjudication, err = replyAdjudication(ctx, tx, d)
	return source, err
}

// replyAdjudication loads the entry a declined or deferred outcome names. The
// store lists an outcome only when its adjudication authorizes it, so a
// missing entry is a broken record and fails closed.
func replyAdjudication(ctx context.Context, tx *store.ReadTx, d domain.ExternalFindingDisposition) (*domain.FindingAdjudicationEntry, error) {
	if d.Disposition == domain.ReviewDispositionFixed {
		return nil, nil
	}
	artifact, err := tx.GetFindingAdjudication(ctx, d.Adjudication())
	if err != nil {
		return nil, err
	}
	for _, entry := range artifact.Entries {
		if entry.FindingID == d.FindingID {
			return &entry, nil
		}
	}
	return nil, errors.New("review reply: adjudication names no entry for the finding")
}

func replyHeadReviewed(ctx context.Context, tx *store.ReadTx, d domain.ExternalFindingDisposition, binding domain.ReadyItemPRBinding) error {
	if d.Disposition == domain.ReviewDispositionFixed {
		fix, err := tx.GetReviewRecord(ctx, d.Remediation())
		if err != nil {
			return err
		}
		records, err := tx.ListReviewRecords(ctx, d.RunID)
		if err != nil {
			return err
		}
		for _, record := range records {
			if record.Round >= fix.Round && record.HeadSHA == binding.HeadSHA {
				return nil
			}
		}
		return store.ErrNotFound
	}
	return nil
}

type externalReviewThreadTarget struct {
	repo     repoRef
	prNumber int
	root     int64
}

func reviewDispositionAllowsThreadResolution(disposition domain.ReviewDisposition) bool {
	switch disposition {
	case domain.ReviewDispositionDeclined, domain.ReviewDispositionDeferred:
		return true
	case domain.ReviewDispositionFixed:
		return false
	}
	return false
}

// reviewThreadResolutionTarget is the Tier A gate deciding whether a posted
// external review reply should now resolve its inline GitHub review thread. It
// re-checks, in cheapest-first order:
//   - that outcomeRow decodes to a posted reply (empty Code, CommentID > 0) for
//     d's finding and round, and that d.Disposition is declined or deferred;
//   - that no resolution row already exists at key+"/resolve" (decoding and
//     verifying any existing row so corruption fails closed);
//   - that the run's workflow is still open (RequireRunWorkflowOpen) and the
//     finding's reviewer is still admitted by the active trust profile
//     (GetAdmittedExternalFinding);
//   - that the finding names an inline review thread (root > 0);
//   - that the reply's intent at key decodes, is dispatched, matches the
//     finding's run, round, disposition, and thread, and still authenticates
//     against its pinned ReadyItemPRBinding via replyCollection;
//   - that every other external finding in the same inline thread (excluding
//     Freeside's own replies authored by intent.BotID) has a highest-round
//     declined or deferred disposition with a decoded posted-reply outcome
//     (empty Code, CommentID > 0).
func reviewThreadResolutionTarget(ctx context.Context, tx *store.ReadTx, run domain.Run, d domain.ExternalFindingDisposition, outcomeRow store.QueueEntry) (externalReviewThreadTarget, bool, error) {
	outcome, err := decodeExternalReplyOutcome(outcomeRow)
	if err != nil {
		return externalReviewThreadTarget{}, false, err
	}
	if outcome.FindingID != d.FindingID || outcome.Round != d.Round {
		return externalReviewThreadTarget{}, false, errors.New("review reply: outcome identity changed")
	}
	if outcome.Code != "" || outcome.CommentID <= 0 {
		return externalReviewThreadTarget{}, false, nil
	}
	if !reviewDispositionAllowsThreadResolution(d.Disposition) {
		return externalReviewThreadTarget{}, false, nil
	}
	key := externalReplyKey(d.FindingID, d.Round)
	resolveRow, err := tx.GetOutbox(ctx, key+"/resolve")
	if err == nil {
		res, err := decodeExternalReviewThreadResolution(resolveRow)
		if err != nil {
			return externalReviewThreadTarget{}, false, err
		}
		if res.FindingID != d.FindingID || res.Round != d.Round || !resolveRow.Dispatched() {
			return externalReviewThreadTarget{}, false, errors.New("review reply: thread resolution identity changed")
		}
		return externalReviewThreadTarget{}, false, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return externalReviewThreadTarget{}, false, err
	}
	if err := tx.RequireRunWorkflowOpen(ctx, run.ID); err != nil {
		if errors.Is(err, store.ErrTaskWorkflowClosed) {
			return externalReviewThreadTarget{}, false, nil
		}
		return externalReviewThreadTarget{}, false, err
	}
	finding, err := tx.GetAdmittedExternalFinding(ctx, d.FindingID)
	if err != nil {
		if errors.Is(err, domain.ErrExternalReviewNotAdmitted) {
			return externalReviewThreadTarget{}, false, nil
		}
		return externalReviewThreadTarget{}, false, err
	}
	thread, err := parseExternalReplyThread(finding.External.ThreadID)
	if err != nil {
		return externalReviewThreadTarget{}, false, err
	}
	if thread.root <= 0 {
		return externalReviewThreadTarget{}, false, nil
	}
	intentRow, err := tx.GetOutbox(ctx, key)
	if err != nil {
		return externalReviewThreadTarget{}, false, err
	}
	intent, err := decodeExternalReplyIntent(intentRow)
	if err != nil {
		return externalReviewThreadTarget{}, false, err
	}
	if !intentRow.Dispatched() || intent.FindingID != d.FindingID || intent.RunID != d.RunID || intent.RunID != run.ID ||
		intent.Round != d.Round || intent.Disposition != d.Disposition || intent.Thread != finding.External.ThreadID {
		return externalReviewThreadTarget{}, false, errors.New("review reply: source identity changed")
	}
	collection, err := replyCollection(ctx, tx, intent)
	if err != nil {
		return externalReviewThreadTarget{}, false, err
	}
	if collection.root != thread.root {
		return externalReviewThreadTarget{}, false, errors.New("review reply: source identity changed")
	}
	repo, err := parseRepo(intent.Repo)
	if err != nil {
		return externalReviewThreadTarget{}, false, err
	}
	// Multiple inline comments in one thread each become a finding sharing
	// ThreadID. Do not collapse the thread while any other finding (aside from
	// Freeside's own replies) still needs it open or has not yet settled.
	findings, err := tx.ListExternalFindings(ctx, run.ID)
	if err != nil {
		return externalReviewThreadTarget{}, false, err
	}
	var siblings []domain.Finding
	for _, g := range findings {
		if g.External == nil || g.External.ThreadID != finding.External.ThreadID || g.ID == d.FindingID {
			continue
		}
		if g.External.ReviewerAccountID == intent.BotID {
			continue
		}
		siblings = append(siblings, g)
	}
	if len(siblings) > 0 {
		dispositions, err := tx.ListExternalFindingDispositions(ctx, run.ID)
		if err != nil {
			return externalReviewThreadTarget{}, false, err
		}
		latest := make(map[domain.FindingID]domain.ExternalFindingDisposition, len(dispositions))
		for _, other := range dispositions {
			if prev, ok := latest[other.FindingID]; !ok || other.Round > prev.Round {
				latest[other.FindingID] = other
			}
		}
		for _, g := range siblings {
			other, ok := latest[g.ID]
			if !ok || !reviewDispositionAllowsThreadResolution(other.Disposition) {
				return externalReviewThreadTarget{}, false, nil
			}
			otherOutcomeRow, err := tx.GetOutbox(ctx, externalReplyKey(g.ID, other.Round)+"/outcome")
			if errors.Is(err, store.ErrNotFound) {
				return externalReviewThreadTarget{}, false, nil
			}
			if err != nil {
				return externalReviewThreadTarget{}, false, err
			}
			otherOutcome, err := decodeExternalReplyOutcome(otherOutcomeRow)
			if err != nil {
				return externalReviewThreadTarget{}, false, err
			}
			if otherOutcome.FindingID != g.ID || otherOutcome.Round != other.Round {
				return externalReviewThreadTarget{}, false, errors.New("review reply: outcome identity changed")
			}
			if otherOutcome.Code != "" || otherOutcome.CommentID <= 0 {
				return externalReviewThreadTarget{}, false, nil
			}
		}
	}
	return externalReviewThreadTarget{repo: repo, prNumber: collection.prNumber, root: collection.root}, true, nil
}

func (r *ExternalReviewReplier) drive(ctx context.Context, run domain.Run, d domain.ExternalFindingDisposition) error {
	key := externalReplyKey(d.FindingID, d.Round)
	var entry store.QueueEntry
	var source externalReplySource
	var resolveTarget externalReviewThreadTarget
	var hasOutcome, resolveDue bool
	err := r.store.Read(ctx, func(tx *store.ReadTx) error {
		outcome, err := tx.GetOutbox(ctx, key+"/outcome")
		if err == nil {
			hasOutcome = true
			resolveTarget, resolveDue, err = reviewThreadResolutionTarget(ctx, tx, run, d, outcome)
			return err
		}
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		entry, err = tx.GetOutbox(ctx, key)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		// Only a claimed request has an external outcome to observe after
		// closure. A pending intent is never authority to send a new reply.
		if entry.Status != "dispatching" {
			if err := tx.RequireRunWorkflowOpen(ctx, run.ID); err != nil {
				return err
			}
		}
		source, err = replySource(ctx, tx, d)
		if errors.Is(err, domain.ErrExternalReviewNotAdmitted) && entry.ID != 0 && entry.Status == "dispatching" {
			source.finding, err = tx.GetFinding(ctx, d.FindingID)
		}
		if err == nil && entry.ID == 0 {
			err = replyHeadReviewed(ctx, tx, d, source.binding)
		}
		return err
	})
	if hasOutcome {
		if err != nil {
			return err
		}
		if resolveDue {
			return r.resolve(ctx, run, d, resolveTarget)
		}
		return nil
	}
	if errors.Is(err, domain.ErrExternalReviewNotAdmitted) {
		if entry.ID != 0 {
			return r.finish(ctx, run, d, externalReplyRevoked, 0, "")
		}
		return nil
	}
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if errors.Is(err, store.ErrTaskWorkflowClosed) {
		if entry.ID == 0 {
			return nil
		}
		return r.retireClosed(ctx, run, d)
	}
	if err != nil {
		return err
	}
	if entry.ID == 0 {
		return r.open(ctx, run, d, source)
	}
	intent, err := decodeExternalReplyIntent(entry)
	if err != nil {
		return err
	}
	if entry.Dispatched() {
		return errors.New("review reply: dispatched without outcome")
	}
	if intent.RunID != d.RunID || intent.Disposition != d.Disposition || intent.Thread != source.finding.External.ThreadID ||
		intent.Repo != source.binding.Repo || intent.PRNumber != source.binding.PRNumber {
		return errors.New("review reply: source identity changed")
	}
	if entry.Status == "dispatching" {
		return r.settle(ctx, run, d, intent)
	}
	return r.send(ctx, run, d, source, intent)
}

func (r *ExternalReviewReplier) resolve(ctx context.Context, run domain.Run, d domain.ExternalFindingDisposition, target externalReviewThreadTarget) error {
	key := externalReplyKey(d.FindingID, d.Round)
	resolveKey := key + "/resolve"
	r.mu.Lock()
	retryAfter := r.retryAfter[resolveKey]
	r.mu.Unlock()
	if r.now().Before(retryAfter) {
		return nil
	}
	result, err := r.forge.resolveReviewThread(ctx, target.repo, target.prNumber, target.root)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var code string
	switch result {
	case reviewThreadResolved:
		code = ""
	case reviewThreadRetry:
		r.mu.Lock()
		r.attempts[resolveKey]++
		attempts := r.attempts[resolveKey]
		if attempts < externalReplyAttemptBound {
			r.retryAfter[resolveKey] = r.now().Add(time.Minute)
			r.mu.Unlock()
			return nil
		}
		r.mu.Unlock()
		code = externalReplyThreadUnresolved
	case reviewThreadFailed:
		code = externalReplyThreadUnresolved
	}
	if !result.valid() {
		return unhandledReviewThreadResolutionResult(result)
	}
	payload, err := json.Marshal(externalReviewThreadResolution{
		FindingID: d.FindingID,
		Round:     d.Round,
		Code:      code,
	})
	if err != nil {
		return err
	}
	return r.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.RequireRunWorkflowOpen(ctx, run.ID); err != nil {
			if errors.Is(err, store.ErrTaskWorkflowClosed) {
				return nil
			}
			return err
		}
		row, _, err := tx.RecordDispatchedOutbox(ctx, resolveKey, ExternalReviewThreadResolutionKind, payload)
		if err != nil {
			return err
		}
		res, err := decodeExternalReviewThreadResolution(row)
		if err != nil {
			return err
		}
		if res.Code != "" {
			return r.putNotice(ctx, tx, run, key, res.Code,
				"Freeside replied to this finding but could not resolve its review thread. Resolve it on the pull request by hand; Freeside will not try again.")
		}
		return nil
	})
}

// A pending reply was never sent or was released after a proven rejection.
// Recheck its state with closure in one writer transaction: a dispatching
// claim still belongs to its sender and may have an external outcome.
func (r *ExternalReviewReplier) retireClosed(ctx context.Context, run domain.Run, d domain.ExternalFindingDisposition) error {
	return r.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.RequireRunWorkflowOpen(ctx, run.ID); !errors.Is(err, store.ErrTaskWorkflowClosed) {
			return err
		}
		entry, err := tx.GetOutbox(ctx, externalReplyKey(d.FindingID, d.Round))
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.Status != "pending" {
			return nil
		}
		intent, err := decodeExternalReplyIntent(entry)
		if err != nil {
			return err
		}
		finding, err := tx.GetFinding(ctx, d.FindingID)
		if err != nil {
			return err
		}
		if intent.RunID != run.ID || intent.RunID != d.RunID ||
			intent.Disposition != d.Disposition || intent.Thread != finding.External.ThreadID {
			return errors.New("review reply: source identity changed")
		}
		if _, err := replyCollection(ctx, &tx.ReadTx, intent); err != nil {
			return err
		}
		return r.finishInTx(ctx, tx, run, d, externalReplyRefused, 0, "")
	})
}

// open serializes reservation by repository and PR inside the store write,
// including intents from other runs. Network reads happen before that lock.
func (r *ExternalReviewReplier) open(ctx context.Context, run domain.Run, d domain.ExternalFindingDisposition, source externalReplySource) error {
	thread, err := parseExternalReplyThread(source.finding.External.ThreadID)
	if err != nil {
		return r.finish(ctx, run, d, externalReplyRefused, 0, "The source thread cannot receive a reply.")
	}
	body, err := externalReviewReplyText(d, source.adjudication, thread, source.binding, domain.IssueTextRulesetGitHubIssue1)
	if err != nil {
		return r.finish(ctx, run, d, externalReplyRefused, 0, "The reply reason failed publication screening.")
	}
	identity, err := r.identity.Resolve(ctx, source.binding.Repo)
	if err != nil || identity.BotUserID <= 0 {
		return errors.New("review reply: App identity unavailable")
	}
	i := externalReplyIntent{
		ItemID: source.binding.ItemID, FindingID: d.FindingID, RunID: d.RunID, Round: d.Round, Repo: source.binding.Repo, PRNumber: source.binding.PRNumber,
		Thread: source.finding.External.ThreadID, Disposition: d.Disposition, HeadSHA: source.binding.HeadSHA,
		TextDigest: contentaddr.Sum([]byte(body)), Ruleset: domain.IssueTextRulesetGitHubIssue1, BotID: identity.BotUserID, CreatedAt: r.now().UTC(),
	}
	comments, err := r.comments(ctx, i)
	if err != nil {
		return err
	}
	for _, c := range comments {
		if c.AuthorID == i.BotID {
			i.Before = append(i.Before, c.ID)
		}
	}
	payload, err := json.Marshal(i)
	if err != nil {
		return err
	}
	return r.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RequireRunWorkflowOpen(ctx, run.ID); err != nil {
			return err
		}
		collection, err := replyCollection(ctx, &tx.ReadTx, i)
		if err != nil {
			return err
		}
		pending, err := tx.ListPendingOutbox(ctx, ExternalReplyIntentKind)
		if err != nil {
			return err
		}
		for _, row := range pending {
			other, err := decodeExternalReplyIntent(row)
			if err != nil {
				return err
			}
			previous, err := replyCollection(ctx, &tx.ReadTx, other)
			if err != nil {
				return err
			}
			if previous.repositoryID == collection.repositoryID && previous.prNumber == collection.prNumber {
				return nil
			}
		}
		row, _, err := tx.EnqueueOutbox(ctx, i.key(), ExternalReplyIntentKind, payload)
		if err != nil {
			return err
		}
		_, err = decodeExternalReplyIntent(row)
		return err
	})
}

func (r *ExternalReviewReplier) comments(ctx context.Context, i externalReplyIntent) ([]replyComment, error) {
	repo, err := parseRepo(i.Repo)
	if err != nil {
		return nil, err
	}
	thread, err := parseExternalReplyThread(i.Thread)
	if err != nil {
		return nil, err
	}
	if thread.root == 0 {
		return r.forge.listConversationComments(ctx, repo, i.PRNumber)
	}
	listed, err := r.forge.getPullReviewComments(ctx, repo, i.PRNumber, "")
	if err != nil {
		return nil, errors.New("review reply: inline listing failed")
	}
	var comments []replyComment
	for _, c := range listed.Items {
		if c.ID <= 0 || c.AuthorID <= 0 || c.CreatedAt.IsZero() || c.InReplyToID < 0 {
			return nil, errors.New("review reply: incomplete inline identity")
		}
		if c.InReplyToID == thread.root {
			comments = append(comments, replyComment{c.ID, c.AuthorID, c.InReplyToID, c.CreatedAt})
		}
	}
	return comments, nil
}

func (r *ExternalReviewReplier) send(ctx context.Context, run domain.Run, d domain.ExternalFindingDisposition, source externalReplySource, i externalReplyIntent) error {
	r.mu.Lock()
	retryAfter := r.retryAfter[i.key()]
	r.mu.Unlock()
	if r.now().Before(retryAfter) {
		return nil
	}
	thread, err := parseExternalReplyThread(i.Thread)
	if err != nil {
		return err
	}
	// Reload the pinned publication instead of trusting a decoded head and
	// digest. This also permits a prior published head after a later cycle.
	var binding domain.ReadyItemPRBinding
	err = r.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		binding, err = tx.GetReadyItemPRBinding(ctx, i.ItemID)
		if err != nil {
			return err
		}
		if binding.RunID != d.RunID || binding.Repo != source.binding.Repo || binding.PRNumber != source.binding.PRNumber || binding.HeadSHA != i.HeadSHA {
			return errors.New("review reply: pinned publication differs")
		}
		return replyHeadReviewed(ctx, tx, d, binding)
	})
	if err != nil {
		return err
	}
	body, err := externalReviewReplyText(d, source.adjudication, thread, binding, i.Ruleset)
	if err != nil || contentaddr.Sum([]byte(body)) != i.TextDigest {
		return r.finish(ctx, run, d, externalReplyRefused, 0, "The recorded reply failed publication screening or changed.")
	}
	identity, err := r.identity.Resolve(ctx, i.Repo)
	if err != nil {
		return err
	}
	if identity.BotUserID != i.BotID {
		return r.finish(ctx, run, d, externalReplyRefused, 0, "The App identity changed before the reply was sent.")
	}
	repo, err := parseRepo(i.Repo)
	if err != nil {
		return err
	}
	req, err := r.forge.newReviewReplyRequest(ctx, repo, i.PRNumber, thread.root, body)
	if err != nil {
		return err
	}
	var claimed bool
	err = r.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		// Recheck after token and identity resolution, immediately before
		// making the irreversible dispatch claim.
		if err := tx.RequireRunWorkflowOpen(ctx, run.ID); err != nil {
			return err
		}
		if _, err := tx.GetAdmittedExternalFinding(ctx, d.FindingID); err != nil {
			return err
		}
		var err error
		claimed, err = tx.TryMarkOutboxDispatching(ctx, i.key())
		return err
	})
	if errors.Is(err, domain.ErrExternalReviewNotAdmitted) {
		return r.finish(ctx, run, d, externalReplyRevoked, 0, "")
	}
	if err != nil || !claimed {
		return err
	}
	r.mu.Lock()
	r.settleAfter[i.key()] = r.now().Add(externalReplySettle)
	r.attempts[i.key()]++
	attempts := r.attempts[i.key()]
	r.mu.Unlock()
	result, sendErr := r.forge.sendReviewReply(req, repo, i.PRNumber, thread.root)
	switch classifyForgeCreate(result.Status, result.Header, result.Comment != nil, sendErr) {
	case domain.FollowUpFilingResponseSuccess:
		c := result.Comment
		if c.AuthorID == i.BotID && c.InReplyToID == thread.root && !slices.Contains(i.Before, c.ID) {
			return r.finish(ctx, run, d, "", c.ID, "")
		}
	case domain.FollowUpFilingResponseDefiniteRejection:
		return r.finish(ctx, run, d, externalReplyRefused, 0, "GitHub rejected the reply. It will not be retried.")
	case domain.FollowUpFilingResponseTransientRejection:
		if attempts >= externalReplyAttemptBound {
			return r.finish(ctx, run, d, externalReplyRefused, 0, "GitHub repeatedly rate-limited the reply. It will not be retried.")
		}
		r.mu.Lock()
		r.retryAfter[i.key()] = r.now().Add(time.Minute)
		r.mu.Unlock()
		return r.store.WriteInternal(ctx, func(tx *store.InternalTx) error { return tx.ReleaseOutboxDispatch(ctx, i.key()) })
	case domain.FollowUpFilingResponseUnproven:
	}
	return ctx.Err()
}

func (r *ExternalReviewReplier) settle(ctx context.Context, run domain.Run, d domain.ExternalFindingDisposition, i externalReplyIntent) error {
	r.mu.Lock()
	deadline, ok := r.settleAfter[i.key()]
	if !ok {
		// A restart cannot know when a pending row was last dispatched.
		// Wait a full interval from first observation, not from intent age.
		r.settleAfter[i.key()] = r.now().Add(externalReplySettle)
	}
	r.mu.Unlock()
	if !ok {
		return nil
	}
	if r.now().Before(deadline) {
		return nil
	}
	// An earlier unknown send can commit after its terminal outcome, so
	// even a unique new comment cannot identify this later request.
	ambiguous, err := r.priorAmbiguousReply(ctx, i)
	if err != nil {
		return err
	}
	if ambiguous {
		return r.finish(ctx, run, d, externalReplyAmbiguous, 0, "An earlier reply in this comment collection may still post. No listed comment can prove this later reply. Check the pull request by hand; no second reply will be sent.")
	}
	comments, err := r.comments(ctx, i)
	if err != nil {
		return r.finish(ctx, run, d, externalReplyAmbiguous, 0, "The reply may have posted, but its comments could not be read. Check the pull request by hand.")
	}
	var candidates []int64
	for _, c := range comments {
		if c.AuthorID == i.BotID && !slices.Contains(i.Before, c.ID) && !c.CreatedAt.Before(i.CreatedAt.Truncate(time.Second)) && !slices.Contains(candidates, c.ID) {
			candidates = append(candidates, c.ID)
		}
	}
	if len(candidates) == 1 {
		return r.finish(ctx, run, d, "", candidates[0], "")
	}
	return r.finish(ctx, run, d, externalReplyAmbiguous, 0, "The reply may have posted, but no unique comment proves it. Check the pull request by hand; no second reply will be sent.")
}

func (r *ExternalReviewReplier) finish(ctx context.Context, run domain.Run, d domain.ExternalFindingDisposition, code string, commentID int64, reason string) error {
	return r.store.Write(ctx, func(tx *store.WriteTx) error {
		return r.finishInTx(ctx, tx, run, d, code, commentID, reason)
	})
}

func (r *ExternalReviewReplier) finishInTx(ctx context.Context, tx *store.WriteTx, run domain.Run, d domain.ExternalFindingDisposition, code string, commentID int64, reason string) error {
	key := externalReplyKey(d.FindingID, d.Round)
	payload, err := json.Marshal(externalReplyOutcome{d.FindingID, d.Round, commentID, code})
	if err != nil {
		return err
	}
	row, _, err := tx.RecordDispatchedOutbox(ctx, key+"/outcome", ExternalReplyOutcomeKind, payload)
	if err != nil {
		return err
	}
	if _, err := ExternalReplyBackupPayloadDigests(row); err != nil {
		return err
	}
	if code != "" && code != externalReplyRevoked {
		if err := r.putNotice(ctx, tx, run, key, code, reason); err != nil {
			return err
		}
	}
	return tx.MarkOutboxDispatched(ctx, key)
}

func (r *ExternalReviewReplier) notice(ctx context.Context, run domain.Run, key, code, reason string) error {
	return r.store.Write(ctx, func(tx *store.WriteTx) error { return r.putNotice(ctx, tx, run, key, code, reason) })
}

func (r *ExternalReviewReplier) putNotice(ctx context.Context, tx *store.WriteTx, run domain.Run, key, code, reason string) error {
	if err := tx.RequireRunWorkflowOpen(ctx, run.ID); err != nil {
		if errors.Is(err, store.ErrTaskWorkflowClosed) {
			return nil
		}
		return err
	}
	id := domain.ItemID("external-review-reply/" + string(run.ID) + "/" + key + "/" + code)
	if _, err := tx.GetAttentionItem(ctx, id); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	subject := domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(run.ID), RunID: &run.ID}
	names, err := tx.DisplayNamesFor(ctx, run.ProjectID, subject)
	if err != nil {
		return err
	}
	at := r.now().UTC()
	posture := domain.HealthPostureAdvisory
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: id, ProjectID: run.ProjectID, Subject: subject,
		Type: domain.AttentionSystemHealth, Priority: domain.PriorityNormal, Reason: reason, RequestedDecision: []domain.Action{domain.ActionAcknowledge},
		HealthDiagnostic: &domain.HealthDiagnostic{Code: code, Impairs: domain.ImpairedCapabilityNone}, ItemVersion: 1,
		InterruptionClass: domain.InterruptionExceptional, CreatedAt: &at, DisplayNames: names, Status: domain.StatusOpen, Posture: &posture,
	}, nil)
	if err != nil {
		return err
	}
	return tx.PutAttentionItem(ctx, item)
}
