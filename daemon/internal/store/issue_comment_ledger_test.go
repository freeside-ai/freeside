package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// The comment ledger reuses the filing ledger's test op shape; these ops
// write the comment ledger.

func commentOpen(ctx context.Context, tx *store.InternalTx, id domain.ProposalInstanceID) error {
	_, err := tx.OpenIssueCommentIntent(ctx, id, ledgerAt)
	return err
}

func commentSet(ids ...int64) ledgerOp {
	return func(ctx context.Context, tx *store.InternalTx, id domain.ProposalInstanceID) error {
		_, err := tx.RecordIssueCommentPreDispatch(ctx, id, ids, ledgerBotUserID, ledgerAt.Add(time.Minute))
		return err
	}
}

func commentAttempt(ctx context.Context, tx *store.InternalTx, id domain.ProposalInstanceID) error {
	_, err := tx.StartIssueCommentAttempt(ctx, id, ledgerAt.Add(2*time.Minute))
	return err
}

func commentResponse(class domain.FollowUpFilingResponseClass) ledgerOp {
	return func(ctx context.Context, tx *store.InternalTx, id domain.ProposalInstanceID) error {
		_, err := tx.RecordIssueCommentResponse(ctx, id, class, ledgerAt.Add(3*time.Minute))
		return err
	}
}

func commentSuccess(commentID int64) ledgerOp {
	return func(ctx context.Context, tx *store.InternalTx, id domain.ProposalInstanceID) error {
		_, err := tx.LedgerIssueCommentSuccess(ctx, id, commentID, ledgerAt.Add(4*time.Minute))
		return err
	}
}

func commentAdopt(commentID int64) ledgerOp {
	return func(ctx context.Context, tx *store.InternalTx, id domain.ProposalInstanceID) error {
		_, err := tx.AdoptIssueCommentCandidate(ctx, id, commentID, ledgerAt.Add(4*time.Minute))
		return err
	}
}

func commentRefuse(reason domain.FollowUpFilingRefusalReason) ledgerOp {
	return func(ctx context.Context, tx *store.InternalTx, id domain.ProposalInstanceID) error {
		_, err := tx.RefuseIssueComment(ctx, id, reason, ledgerAt.Add(4*time.Minute))
		return err
	}
}

func commentAmbiguous(ctx context.Context, tx *store.InternalTx, id domain.ProposalInstanceID) error {
	_, err := tx.RecordIssueCommentAmbiguous(ctx, id, ledgerAt.Add(4*time.Minute))
	return err
}

var (
	commentTransient = commentResponse(domain.FollowUpFilingResponseTransientRejection)
	commentDefinite  = commentResponse(domain.FollowUpFilingResponseDefiniteRejection)
	commentUnproven  = commentResponse(domain.FollowUpFilingResponseUnproven)
)

func commentIntent(
	ctx context.Context, st *store.Store, id domain.ProposalInstanceID,
) (domain.IssueCommentIntent, error) {
	return readLedger(ctx, st, func(tx *store.ReadTx) (domain.IssueCommentIntent, error) {
		return tx.GetIssueCommentIntent(ctx, id)
	})
}

func postedComment(
	ctx context.Context, st *store.Store, commentID int64,
) (*domain.PostedIssueComment, error) {
	return readLedger(ctx, st, func(tx *store.ReadTx) (*domain.PostedIssueComment, error) {
		return tx.PostedIssueComment(ctx, 123, commentID)
	})
}

// commentLedgerFixture is one store holding two approved comments on issue
// 41 of repository 123, from two admissions of one proposal, neither with an
// intent yet, and the comment proposal itself for further cards.
type commentLedgerFixture struct {
	ledgerFixture
	proposal domain.EffectProposal
}

func seedCommentLedger(t *testing.T, ctx context.Context) commentLedgerFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.db")
	st := storetest.Open(t, path, store.Options{})
	fx := seedFiling(t, ctx, st, "project-comment")
	proposal := fx.commentProposal(t, seedComment(t, ctx, st, fx))
	approve := func(event string) domain.ProposalInstance {
		instance, command := openFilingCard(t, ctx, st, fx, proposal, event, domain.ActionApprove)
		if err := decideFiling(ctx, st, instance, command); err != nil {
			t.Fatalf("approve comment %q: %v", event, err)
		}
		return instance
	}
	return commentLedgerFixture{
		ledgerFixture: ledgerFixture{
			path: path, st: st, fx: fx, first: approve("event-first"), second: approve("event-second"),
		},
		proposal: proposal,
	}
}

// tamperComment is ledgerFixture.tamper for the comment ledger's tables: it
// drops their seven triggers, then requires each statement to change exactly
// one row.
func (l *commentLedgerFixture) tamperComment(t *testing.T, statements ...string) {
	t.Helper()
	l.raw(t, func(raw *sql.DB) {
		triggers, err := raw.Query(`SELECT name FROM sqlite_master
			WHERE type = 'trigger' AND tbl_name IN
				('issue_comment_intents', 'issue_comment_attempts', 'posted_issue_comments')`)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for triggers.Next() {
			var name string
			if err := triggers.Scan(&name); err != nil {
				t.Fatal(err)
			}
			names = append(names, name)
		}
		if err := errors.Join(triggers.Err(), triggers.Close()); err != nil {
			t.Fatal(err)
		}
		if len(names) != 7 {
			t.Fatalf("comment ledger triggers = %v, want seven", names)
		}
		for _, name := range names {
			if _, err := raw.Exec(`DROP TRIGGER "` + name + `"`); err != nil {
				t.Fatal(err)
			}
		}
		for _, statement := range statements {
			result, err := raw.Exec(statement)
			if err != nil {
				t.Fatalf("tamper %q: %v", statement, err)
			}
			if changed, err := result.RowsAffected(); err != nil || changed != 1 {
				t.Fatalf("tamper %q changed %d rows, %v", statement, changed, err)
			}
		}
	})
}

// TestIssueCommentIntentOnePerIssue: an issue holds one outstanding intent,
// the target comes from the proposal, and a terminal outcome frees the issue
// for the next.
func TestIssueCommentIntentOnePerIssue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	l := seedCommentLedger(t, ctx)
	mustLedger(t, ctx, l.st, l.first.ID, commentOpen)
	intent, err := commentIntent(ctx, l.st, l.first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if intent.RepositoryID != 123 || intent.IssueNumber != 41 || intent.PreDispatch != nil || len(intent.Attempts) != 0 {
		t.Fatalf("opened intent = %+v", intent)
	}
	// A repeat open returns the intent unchanged.
	mustLedger(t, ctx, l.st, l.first.ID, commentOpen)
	if err := tryLedger(ctx, l.st, l.second.ID, commentOpen); !errors.Is(err, domain.ErrIssueCommentIssueBusy) {
		t.Fatalf("second outstanding intent on the issue = %v, want ErrIssueCommentIssueBusy", err)
	}
	mustLedger(t, ctx, l.st, l.first.ID, commentSet(), commentAttempt, commentDefinite,
		commentRefuse(domain.FollowUpFilingRefusalDefiniteRejection))
	mustLedger(t, ctx, l.st, l.second.ID, commentOpen)
}

// TestIssueCommentIntentOpenRequiresApprovedComment: an undecided or
// declined comment, and a filing, open no comment intent.
func TestIssueCommentIntentOpenRequiresApprovedComment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	l := seedCommentLedger(t, ctx)
	undecided, _ := openFilingCard(t, ctx, l.st, l.fx, l.proposal, "event-undecided", domain.ActionApprove)
	declined, declineCommand := openFilingCard(t, ctx, l.st, l.fx, l.proposal, "event-declined", domain.ActionDecline)
	if err := decideFiling(ctx, l.st, declined, declineCommand); err != nil {
		t.Fatal(err)
	}
	filing := approvedFiling(t, ctx, l.st, l.fx, l.fx.deferredSource(), "event-filing")
	for name, id := range map[string]domain.ProposalInstanceID{
		"undecided": undecided.ID, "declined": declined.ID, "filing": filing.ID,
	} {
		if err := tryLedger(ctx, l.st, id, commentOpen); !errors.Is(err, domain.ErrIssueCommentNotApproved) {
			t.Errorf("%s: open = %v, want ErrIssueCommentNotApproved", name, err)
		}
	}
}

// TestIssueCommentLedgerSuccessAndAdoption walks the two ledgered outcomes:
// a success names its comment; an adoption after an unproven attempt takes a
// comment outside the pre-dispatch set, never one in it or one another
// instance ledgered, and never on an issue that holds an ambiguous outcome.
func TestIssueCommentLedgerSuccessAndAdoption(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	l := seedCommentLedger(t, ctx)
	mustLedger(t, ctx, l.st, l.first.ID, commentOpen, commentSet(500), commentAttempt, commentSuccess(900))
	posted, err := postedComment(ctx, l.st, 900)
	if err != nil || posted == nil || posted.InstanceID != l.first.ID || posted.IssueNumber != 41 {
		t.Fatalf("posted comment = %+v, %v", posted, err)
	}
	forInstance, err := readLedger(ctx, l.st, func(tx *store.ReadTx) (*domain.PostedIssueComment, error) {
		return tx.PostedIssueCommentForInstance(ctx, l.first.ID)
	})
	if err != nil || forInstance == nil || *forInstance != *posted {
		t.Fatalf("posted comment for instance = %+v, %v", forInstance, err)
	}

	mustLedger(t, ctx, l.st, l.second.ID, commentOpen, commentSet(500, 900), commentAttempt, commentUnproven)
	if err := tryLedger(ctx, l.st, l.second.ID, commentAdopt(500)); !errors.Is(err, domain.ErrIssueCommentAdoptionRefused) {
		t.Fatalf("adopt a pre-dispatch comment = %v, want ErrIssueCommentAdoptionRefused", err)
	}
	if err := tryLedger(ctx, l.st, l.second.ID, commentStartAgain); !errors.Is(err, domain.ErrIssueCommentCreateUnproven) {
		t.Fatalf("retry after an unproven attempt = %v, want ErrIssueCommentCreateUnproven", err)
	}
	mustLedger(t, ctx, l.st, l.second.ID, commentAdopt(901))

	third := l.approve(t, ctx, "event-third")
	mustLedger(t, ctx, l.st, third.ID, commentOpen, commentSet(500, 900, 901), commentAttempt, commentUnproven)
	if err := tryLedger(ctx, l.st, third.ID, commentAdopt(900)); !errors.Is(err, domain.ErrIssueCommentAdoptionRefused) {
		t.Fatalf("adopt a comment in the set = %v, want ErrIssueCommentAdoptionRefused", err)
	}
	mustLedger(t, ctx, l.st, third.ID, commentAmbiguous)
	fourth := l.approve(t, ctx, "event-fourth")
	mustLedger(t, ctx, l.st, fourth.ID, commentOpen, commentSet(500, 900, 901), commentAttempt, commentUnproven)
	if err := tryLedger(ctx, l.st, fourth.ID, commentAdopt(950)); !errors.Is(err, domain.ErrIssueCommentAdoptionRefused) {
		t.Fatalf("adopt on an issue with an ambiguous outcome = %v, want ErrIssueCommentAdoptionRefused", err)
	}
	ambiguous, err := readLedger(ctx, l.st, func(tx *store.ReadTx) (bool, error) {
		return tx.IssueCommentIssueAmbiguous(ctx, 123, 41)
	})
	if err != nil || !ambiguous {
		t.Fatalf("issue ambiguity = %v, %v", ambiguous, err)
	}
}

// commentStartAgain starts another attempt, which the evidence rule refuses
// after an unproven one.
func commentStartAgain(ctx context.Context, tx *store.InternalTx, id domain.ProposalInstanceID) error {
	_, err := tx.StartIssueCommentAttempt(ctx, id, ledgerAt.Add(5*time.Minute))
	return err
}

func (l *commentLedgerFixture) approve(t *testing.T, ctx context.Context, event string) domain.ProposalInstance {
	t.Helper()
	instance, command := openFilingCard(t, ctx, l.st, l.fx, l.proposal, event, domain.ActionApprove)
	if err := decideFiling(ctx, l.st, instance, command); err != nil {
		t.Fatalf("approve comment %q: %v", event, err)
	}
	return instance
}

// TestIssueCommentLedgerIDIsLedgeredOnce: a comment ID another instance
// already ledgered is refused, by success and by adoption.
func TestIssueCommentLedgerIDIsLedgeredOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	l := seedCommentLedger(t, ctx)
	mustLedger(t, ctx, l.st, l.first.ID, commentOpen, commentSet(), commentAttempt, commentSuccess(900))
	mustLedger(t, ctx, l.st, l.second.ID, commentOpen, commentSet(), commentAttempt)
	if err := tryLedger(ctx, l.st, l.second.ID, commentSuccess(900)); !errors.Is(err, domain.ErrIssueCommentLedgered) {
		t.Fatalf("success naming a ledgered comment = %v, want ErrIssueCommentLedgered", err)
	}
	mustLedger(t, ctx, l.st, l.second.ID, commentUnproven)
	if err := tryLedger(ctx, l.st, l.second.ID, commentAdopt(900)); !errors.Is(err, domain.ErrIssueCommentLedgered) {
		t.Fatalf("adoption of a ledgered comment = %v, want ErrIssueCommentLedgered", err)
	}
}

// TestIssueCommentLedgerRowsAreAppendOnly: the schema's triggers refuse a
// posted row's update or delete and a recorded intent fact's change, whatever
// writes them.
func TestIssueCommentLedgerRowsAreAppendOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	l := seedCommentLedger(t, ctx)
	mustLedger(t, ctx, l.st, l.first.ID, commentOpen, commentSet(500), commentAttempt, commentSuccess(900))
	mustLedger(t, ctx, l.st, l.second.ID, commentOpen)
	l.raw(t, func(raw *sql.DB) {
		for _, statement := range []string{
			`UPDATE posted_issue_comments SET issue_number = 7`,
			`DELETE FROM posted_issue_comments`,
			`DELETE FROM issue_comment_attempts`,
			`DELETE FROM issue_comment_intents`,
			`UPDATE issue_comment_intents SET outcome = NULL, resolved_at = NULL WHERE outcome IS NOT NULL`,
			`UPDATE issue_comment_intents SET issue_number = 7 WHERE outcome IS NULL`,
			`UPDATE issue_comment_attempts SET response_comment_id = 901`,
		} {
			if _, err := raw.Exec(statement); err == nil {
				t.Errorf("%q succeeded", statement)
			}
		}
	})
}

// TestIssueCommentReconstructionFailsClosed: a row changed past the triggers
// is an error on read, never an absent or different answer.
func TestIssueCommentReconstructionFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cases := []struct {
		name      string
		statement string
		read      func(context.Context, commentLedgerFixture) error
	}{
		{
			"intent names another issue", `UPDATE issue_comment_intents SET issue_number = 7`,
			func(ctx context.Context, l commentLedgerFixture) error {
				_, err := commentIntent(ctx, l.st, l.first.ID)
				return err
			},
		},
		{
			"posted row lost", `DELETE FROM posted_issue_comments`,
			func(ctx context.Context, l commentLedgerFixture) error {
				_, err := readLedger(ctx, l.st, func(tx *store.ReadTx) (*domain.PostedIssueComment, error) {
					return tx.PostedIssueCommentForInstance(ctx, l.first.ID)
				})
				return err
			},
		},
		{
			"posted row names another comment", `UPDATE posted_issue_comments
			SET comment_id = 901, body = json_set(body, '$.comment_id', 901)`,
			func(ctx context.Context, l commentLedgerFixture) error {
				_, err := postedComment(ctx, l.st, 901)
				return err
			},
		},
		{
			"posted row key disagrees with its body", `UPDATE posted_issue_comments SET issue_number = 7`,
			func(ctx context.Context, l commentLedgerFixture) error {
				_, err := postedComment(ctx, l.st, 900)
				return err
			},
		},
		{
			"success response names a pre-dispatch comment", `UPDATE issue_comment_attempts SET response_comment_id = 500`,
			func(ctx context.Context, l commentLedgerFixture) error {
				_, err := commentIntent(ctx, l.st, l.first.ID)
				return err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l := seedCommentLedger(t, ctx)
			mustLedger(t, ctx, l.st, l.first.ID, commentOpen, commentSet(500), commentAttempt, commentSuccess(900))
			l.tamperComment(t, tc.statement)
			if err := tc.read(ctx, l); err == nil {
				t.Fatal("tampered ledger read succeeded")
			}
		})
	}
}

// TestIssueCommentLedgerSurvivesPlaintextRestore pins that a restore
// round-trips all three tables through their delete guards and reinstates
// the guards before it commits.
func TestIssueCommentLedgerSurvivesPlaintextRestore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	l := seedCommentLedger(t, ctx)
	mustLedger(t, ctx, l.st, l.first.ID, commentOpen, commentSet(500), commentAttempt, commentSuccess(900))
	mustLedger(t, ctx, l.st, l.second.ID, commentOpen, commentSet(500, 900), commentAttempt, commentTransient,
		commentAttempt)
	read := func() (intents [2]domain.IssueCommentIntent, posted *domain.PostedIssueComment) {
		t.Helper()
		for index, id := range []domain.ProposalInstanceID{l.first.ID, l.second.ID} {
			intent, err := commentIntent(ctx, l.st, id)
			if err != nil {
				t.Fatal(err)
			}
			intents[index] = intent
		}
		posted, err := postedComment(ctx, l.st, 900)
		if err != nil || posted == nil {
			t.Fatalf("posted comment = %+v, %v", posted, err)
		}
		return intents, posted
	}
	wantIntents, wantPosted := read()

	checkpoint := filepath.Join(t.TempDir(), "comment-checkpoint.db")
	if err := l.st.Checkpoint(ctx, checkpoint); err != nil {
		t.Fatal(err)
	}
	mustLedger(t, ctx, l.st, l.second.ID, commentUnproven, commentAdopt(901))
	if _, err := l.st.Restore(ctx, checkpoint); err != nil {
		t.Fatalf("restore over a non-empty ledger: %v", err)
	}

	gotIntents, gotPosted := read()
	if !reflect.DeepEqual(gotIntents, wantIntents) || *gotPosted != *wantPosted {
		t.Fatalf("restored ledger = %+v, %+v; want %+v, %+v", gotIntents, gotPosted, wantIntents, wantPosted)
	}
	if later, err := postedComment(ctx, l.st, 901); err != nil || later != nil {
		t.Fatalf("a row ledgered after the checkpoint survived the restore: %+v, %v", later, err)
	}
	l.raw(t, func(raw *sql.DB) {
		for _, statement := range []string{
			`DELETE FROM posted_issue_comments`, `DELETE FROM issue_comment_attempts`,
			`DELETE FROM issue_comment_intents`,
		} {
			if _, err := raw.Exec(statement); err == nil {
				t.Errorf("%q succeeded: the restore lost a delete guard", statement)
			}
		}
	})
}

// TestPostedIssueCommentRowGolden pins the stored posted-comment body, the
// one comment ledger representation that is canonical JSON.
func TestPostedIssueCommentRowGolden(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	l := seedCommentLedger(t, ctx)
	mustLedger(t, ctx, l.st, l.first.ID, commentOpen, commentSet(500), commentAttempt, commentSuccess(900))
	posted, err := postedComment(ctx, l.st, 900)
	if err != nil || posted == nil {
		t.Fatalf("posted comment = %+v, %v", posted, err)
	}
	// The instance id is random per admission; the golden pins the shape.
	fixed := *posted
	fixed.InstanceID = "proposal-fixed"
	golden.Assert(t, "posted_issue_comment", marshalIndent(t, fixed))
}

// TestRunPostsMayHaveCreatedCountsFilingsAndComments: the per-run cap reads
// one count over the run's filings and comments, each counted once it may
// have created something, and never the intent asking.
func TestRunPostsMayHaveCreatedCountsFilingsAndComments(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	l := seedCommentLedger(t, ctx)
	filing := approvedFiling(t, ctx, l.st, l.fx, l.fx.deferredSource(), "event-filing")
	count := func(exclude domain.ProposalInstanceID) int {
		t.Helper()
		n, err := readLedger(ctx, l.st, func(tx *store.ReadTx) (int, error) {
			return tx.RunPostsMayHaveCreated(ctx, l.fx.handle, exclude)
		})
		if err != nil {
			t.Fatalf("RunPostsMayHaveCreated: %v", err)
		}
		return n
	}
	const nobody = domain.ProposalInstanceID("nobody")
	if got := count(nobody); got != 0 {
		t.Fatalf("count with no intents = %d, want 0", got)
	}
	// A dispatched comment counts for every other intent, not for itself.
	mustLedger(t, ctx, l.st, l.first.ID, commentOpen, commentSet(), commentAttempt)
	if got, self := count(nobody), count(l.first.ID); got != 1 || self != 0 {
		t.Fatalf("after a dispatched comment: count = %d, own = %d, want 1 and 0", got, self)
	}
	// A filing GitHub definitely rejected created nothing.
	mustLedger(t, ctx, l.st, filing.ID, ledgerOpen, ledgerSet(), ledgerAttempt, ledgerDefinite,
		ledgerRefuse(domain.FollowUpFilingRefusalDefiniteRejection))
	if got := count(nobody); got != 1 {
		t.Fatalf("after a rejected filing: count = %d, want 1", got)
	}
	// A ledgered comment and an ambiguous one both count.
	mustLedger(t, ctx, l.st, l.first.ID, commentSuccess(900))
	mustLedger(t, ctx, l.st, l.second.ID, commentOpen, commentSet(), commentAttempt, commentUnproven, commentAmbiguous)
	if got, self := count(nobody), count(l.second.ID); got != 2 || self != 1 {
		t.Fatalf("after a posted and an ambiguous comment: count = %d, own = %d, want 2 and 1", got, self)
	}
}
