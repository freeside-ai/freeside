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

var ledgerAt = filingAt.Add(24 * time.Hour)

// ledgerBotUserID is the App bot account every test history lists under.
const ledgerBotUserID int64 = 4100

// ledgerOp is one ledger write against an instance's intent, so a test names
// the history it needs.
type ledgerOp func(ctx context.Context, tx *store.InternalTx, id domain.ProposalInstanceID) error

func ledgerOpen(ctx context.Context, tx *store.InternalTx, id domain.ProposalInstanceID) error {
	_, err := tx.OpenFollowUpFilingIntent(ctx, id, ledgerAt)
	return err
}

func ledgerSet(numbers ...int) ledgerOp {
	return func(ctx context.Context, tx *store.InternalTx, id domain.ProposalInstanceID) error {
		_, err := tx.RecordFollowUpFilingPreDispatch(ctx, id, numbers, ledgerBotUserID, ledgerAt.Add(time.Minute))
		return err
	}
}

func ledgerAttempt(ctx context.Context, tx *store.InternalTx, id domain.ProposalInstanceID) error {
	_, err := tx.StartFollowUpFilingAttempt(ctx, id, ledgerAt.Add(2*time.Minute))
	return err
}

func ledgerResponse(class domain.FollowUpFilingResponseClass) ledgerOp {
	return func(ctx context.Context, tx *store.InternalTx, id domain.ProposalInstanceID) error {
		_, err := tx.RecordFollowUpFilingResponse(ctx, id, class, ledgerAt.Add(3*time.Minute))
		return err
	}
}

func ledgerSuccess(issue int) ledgerOp {
	return func(ctx context.Context, tx *store.InternalTx, id domain.ProposalInstanceID) error {
		_, err := tx.LedgerFollowUpFilingSuccess(ctx, id, issue, ledgerAt.Add(4*time.Minute))
		return err
	}
}

func ledgerAdopt(issue int) ledgerOp {
	return func(ctx context.Context, tx *store.InternalTx, id domain.ProposalInstanceID) error {
		_, err := tx.AdoptFollowUpFilingCandidate(ctx, id, issue, ledgerAt.Add(4*time.Minute))
		return err
	}
}

func ledgerRefuse(reason domain.FollowUpFilingRefusalReason) ledgerOp {
	return func(ctx context.Context, tx *store.InternalTx, id domain.ProposalInstanceID) error {
		_, err := tx.RefuseFollowUpFiling(ctx, id, reason, ledgerAt.Add(4*time.Minute))
		return err
	}
}

func ledgerAmbiguous(ctx context.Context, tx *store.InternalTx, id domain.ProposalInstanceID) error {
	_, err := tx.RecordFollowUpFilingAmbiguous(ctx, id, ledgerAt.Add(4*time.Minute))
	return err
}

var (
	ledgerTransient = ledgerResponse(domain.FollowUpFilingResponseTransientRejection)
	ledgerDefinite  = ledgerResponse(domain.FollowUpFilingResponseDefiniteRejection)
	ledgerUnproven  = ledgerResponse(domain.FollowUpFilingResponseUnproven)
)

// tryLedger applies ops in order, each in its own transaction, and returns
// the first error.
func tryLedger(ctx context.Context, st *store.Store, id domain.ProposalInstanceID, ops ...ledgerOp) error {
	for _, op := range ops {
		if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error { return op(ctx, tx, id) }); err != nil {
			return err
		}
	}
	return nil
}

func mustLedger(
	t *testing.T, ctx context.Context, st *store.Store, id domain.ProposalInstanceID, ops ...ledgerOp,
) {
	t.Helper()
	if err := tryLedger(ctx, st, id, ops...); err != nil {
		t.Fatalf("ledger history for %q: %v", id, err)
	}
}

func readLedger[T any](ctx context.Context, st *store.Store, read func(*store.ReadTx) (T, error)) (T, error) {
	var value T
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		value, err = read(tx)
		return err
	})
	return value, err
}

func ledgerIntent(
	ctx context.Context, st *store.Store, id domain.ProposalInstanceID,
) (domain.FollowUpFilingIntent, error) {
	return readLedger(ctx, st, func(tx *store.ReadTx) (domain.FollowUpFilingIntent, error) {
		return tx.GetFollowUpFilingIntent(ctx, id)
	})
}

func ledgerMayHaveCreated(ctx context.Context, st *store.Store, repositoryID int64) (bool, error) {
	return readLedger(ctx, st, func(tx *store.ReadTx) (bool, error) {
		return tx.FollowUpFilingMayHaveCreated(ctx, repositoryID)
	})
}

func ledgerFiledIssue(
	ctx context.Context, st *store.Store, repositoryID int64, issue int,
) (*domain.FiledFollowUpIssue, error) {
	return readLedger(ctx, st, func(tx *store.ReadTx) (*domain.FiledFollowUpIssue, error) {
		return tx.FiledFollowUpIssue(ctx, repositoryID, issue)
	})
}

func ledgerFiledIssueFor(
	ctx context.Context, st *store.Store, id domain.ProposalInstanceID,
) (*domain.FiledFollowUpIssue, error) {
	return readLedger(ctx, st, func(tx *store.ReadTx) (*domain.FiledFollowUpIssue, error) {
		return tx.FiledFollowUpIssueForInstance(ctx, id)
	})
}

// approvedFiling admits a filing from source and records its approve
// decision, the state an intent may be opened from.
func approvedFiling(
	t *testing.T, ctx context.Context, st *store.Store, fx filingFixture,
	source domain.FollowUpFilingSource, event string,
) domain.ProposalInstance {
	t.Helper()
	instance, command := openFilingCard(t, ctx, st, fx, fx.proposal(t, fx.input(source)), event, domain.ActionApprove)
	if err := decideFiling(ctx, st, instance, command); err != nil {
		t.Fatalf("approve filing %q: %v", event, err)
	}
	return instance
}

// ledgerFixture is one store holding two approved filings in repository 123,
// neither with an intent yet.
type ledgerFixture struct {
	path          string
	st            *store.Store
	fx            filingFixture
	first, second domain.ProposalInstance
}

func seedLedger(t *testing.T, ctx context.Context) ledgerFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.db")
	st := storetest.Open(t, path, store.Options{})
	fx := seedFiling(t, ctx, st, "project-filing")
	return ledgerFixture{
		path: path, st: st, fx: fx,
		first:  approvedFiling(t, ctx, st, fx, fx.deferredSource(), "event-first"),
		second: approvedFiling(t, ctx, st, fx, fx.parkedSource(), "event-second"),
	}
}

// raw closes the store, runs fn on a direct connection to its file, and
// reopens it. A direct connection is how a test reaches the schema without
// the store's own checks in the way.
func (l *ledgerFixture) raw(t *testing.T, fn func(raw *sql.DB)) {
	t.Helper()
	if err := l.st.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", l.path)
	if err != nil {
		t.Fatal(err)
	}
	// One connection, so a PRAGMA a test sets holds for its statements.
	raw.SetMaxOpenConns(1)
	fn(raw)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	l.st = storetest.Open(t, l.path, store.Options{})
}

// tamper applies statements the append-only triggers would refuse, as an
// editor of the database file could: it drops the triggers first. Each
// statement must change exactly one row, so a tamper that silently missed
// cannot pass for a refused read.
func (l *ledgerFixture) tamper(t *testing.T, statements ...string) {
	t.Helper()
	l.raw(t, func(raw *sql.DB) {
		triggers, err := raw.Query(`SELECT name FROM sqlite_master
			WHERE type = 'trigger' AND tbl_name LIKE 'follow_up_fil%'`)
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
		if len(names) != 8 {
			t.Fatalf("ledger triggers = %v, want eight", names)
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

// TestFollowUpFilingIntentOneOutstandingPerRepository pins §5.17's
// serialization: a repository holds one outstanding intent, the schema
// refuses a second whatever writes it, and each terminal outcome frees the
// repository for the next.
func TestFollowUpFilingIntentOneOutstandingPerRepository(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	resolutions := map[string][]ledgerOp{
		"ledgered":  {ledgerSet(), ledgerAttempt, ledgerSuccess(40)},
		"refused":   {ledgerRefuse(domain.FollowUpFilingRefusalPreconditionFailed)},
		"ambiguous": {ledgerSet(), ledgerAttempt, ledgerAmbiguous},
	}
	for name, resolve := range resolutions {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			l := seedLedger(t, ctx)
			elsewhere := seedFilingIn(t, ctx, l.st, "project-elsewhere", "owner/elsewhere", 456)
			other := approvedFiling(t, ctx, l.st, elsewhere, elsewhere.deferredSource(), "event-elsewhere")

			mustLedger(t, ctx, l.st, l.first.ID, ledgerOpen)
			if err := tryLedger(ctx, l.st, l.second.ID, ledgerOpen); !errors.Is(err, domain.ErrFollowUpFilingRepositoryBusy) {
				t.Fatalf("second intent in the repository: error = %v, want %v", err, domain.ErrFollowUpFilingRepositoryBusy)
			}
			mustLedger(t, ctx, l.st, other.ID, ledgerOpen)
			l.raw(t, func(raw *sql.DB) {
				if _, err := raw.Exec(`INSERT INTO follow_up_filing_intents (instance_id, repository_id, opened_at)
					VALUES (?, 123, '2026-09-02T12:00:00Z')`, l.second.ID); err == nil {
					t.Fatal("the schema admitted a second outstanding intent in one repository")
				}
			})

			mustLedger(t, ctx, l.st, l.first.ID, resolve...)
			mustLedger(t, ctx, l.st, l.second.ID, ledgerOpen)
			outstanding, err := readLedger(ctx, l.st, func(tx *store.ReadTx) (*domain.FollowUpFilingIntent, error) {
				return tx.OutstandingFollowUpFilingIntent(ctx, 123)
			})
			if err != nil || outstanding == nil || outstanding.InstanceID != l.second.ID {
				t.Fatalf("outstanding intent = %+v, %v; want the second instance's", outstanding, err)
			}
		})
	}
}

func TestFollowUpFilingIntentOpenRequiresApprovedFiling(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{})
	fx := seedFiling(t, ctx, st, "project-filing")
	undecided, approve := openFilingCard(t, ctx, st, fx,
		fx.proposal(t, fx.input(fx.deferredSource())), "event-undecided", domain.ActionApprove)
	declined, decline := openFilingCard(t, ctx, st, fx,
		fx.proposal(t, fx.input(fx.parkedSource())), "event-declined", domain.ActionDecline)
	if err := decideFiling(ctx, st, declined, decline); err != nil {
		t.Fatal(err)
	}
	policy, taskProposal, _ := proposalFixture(t)
	var task domain.ProposalInstance
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := putProposalPolicy(t, ctx, tx, policy); err != nil {
			return err
		}
		var err error
		task, _, err = tx.AllocateProposalInstance(ctx,
			domain.ProposalAdmissionKey{Source: domain.ProposalSourceClientCommand, SubmissionCommandID: "command-task"},
			"batch-1", taskProposal, filingAt)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		id   domain.ProposalInstanceID
		want error
	}{
		"an undecided filing":  {undecided.ID, domain.ErrFollowUpFilingNotApproved},
		"a declined filing":    {declined.ID, domain.ErrFollowUpFilingNotApproved},
		"a task proposal":      {task.ID, domain.ErrFollowUpFilingNotApproved},
		"no proposal instance": {"proposal-missing", store.ErrNotFound},
	} {
		if err := tryLedger(ctx, st, tc.id, ledgerOpen); !errors.Is(err, tc.want) {
			t.Errorf("open for %s: error = %v, want %v", name, err, tc.want)
		}
		if _, err := ledgerIntent(ctx, st, tc.id); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("intent for %s: error = %v, want %v", name, err, store.ErrNotFound)
		}
	}

	if err := decideFiling(ctx, st, undecided, approve); err != nil {
		t.Fatal(err)
	}
	mustLedger(t, ctx, st, undecided.ID, ledgerOpen, ledgerSet(7))
	opened, err := ledgerIntent(ctx, st, undecided.ID)
	if err != nil {
		t.Fatal(err)
	}
	if opened.RepositoryID != fx.target.RepositoryID || !opened.OpenedAt.Equal(ledgerAt) {
		t.Fatalf("intent = %+v, want repository %d opened at %s", opened, fx.target.RepositoryID, ledgerAt)
	}
	// A repeat is a read: it returns the stored intent, at the state it has
	// reached, and records nothing from the later call.
	var reopened domain.FollowUpFilingIntent
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		reopened, err = tx.OpenFollowUpFilingIntent(ctx, undecided.ID, ledgerAt.Add(time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reopened, opened) {
		t.Fatalf("re-opened intent = %+v, want the existing %+v", reopened, opened)
	}
}

func TestFollowUpFilingPreDispatchIsWrittenOnceInStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	l := seedLedger(t, ctx)
	mustLedger(t, ctx, l.st, l.first.ID, ledgerOpen, ledgerSet(31, 7))
	for name, op := range map[string]ledgerOp{
		"a different set": ledgerSet(8),
		"the same set":    ledgerSet(7, 31),
		"an empty set":    ledgerSet(),
	} {
		if err := tryLedger(ctx, l.st, l.first.ID, op); !errors.Is(err, domain.ErrFollowUpFilingPreDispatchFixed) {
			t.Errorf("%s over a recorded set: error = %v, want %v", name, err, domain.ErrFollowUpFilingPreDispatchFixed)
		}
	}
	mustLedger(t, ctx, l.st, l.first.ID, ledgerAttempt)
	if err := tryLedger(ctx, l.st, l.first.ID, ledgerSet(8)); !errors.Is(err, domain.ErrFollowUpFilingPreDispatchFixed) {
		t.Errorf("a set after the first attempt: error = %v, want %v", err, domain.ErrFollowUpFilingPreDispatchFixed)
	}
	intent, err := ledgerIntent(ctx, l.st, l.first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := intent.PreDispatch.IssueNumbers; !reflect.DeepEqual(got, []int{7, 31}) {
		t.Fatalf("pre-dispatch set = %v, want [7 31]", got)
	}
	if got := intent.PreDispatch.BotUserID; got == nil || *got != ledgerBotUserID {
		t.Fatalf("pre-dispatch identity = %v, want bot user %d", got, ledgerBotUserID)
	}

	// An empty set is a recorded fact, distinct from no set: it reads back
	// as recorded after a reopen and still fixes the set.
	mustLedger(t, ctx, l.st, l.first.ID, ledgerAmbiguous)
	if err := tryLedger(ctx, l.st, l.second.ID, ledgerOpen, ledgerAttempt); !errors.Is(err, domain.ErrFollowUpFilingPreDispatchMissing) {
		t.Fatalf("attempt before any set: error = %v, want %v", err, domain.ErrFollowUpFilingPreDispatchMissing)
	}
	// A set names the account it was listed under, or it is not recorded.
	for _, botUserID := range []int64{0, -1} {
		err := l.st.WriteInternal(ctx, func(tx *store.InternalTx) error {
			_, err := tx.RecordFollowUpFilingPreDispatch(ctx, l.second.ID, nil, botUserID, ledgerAt.Add(time.Minute))
			return err
		})
		if !errors.Is(err, domain.ErrNonPositive) {
			t.Fatalf("a set under bot user %d: error = %v, want %v", botUserID, err, domain.ErrNonPositive)
		}
	}
	mustLedger(t, ctx, l.st, l.second.ID, ledgerSet())
	l.raw(t, func(*sql.DB) {})
	empty, err := ledgerIntent(ctx, l.st, l.second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if empty.PreDispatch == nil || empty.PreDispatch.IssueNumbers == nil || len(empty.PreDispatch.IssueNumbers) != 0 {
		t.Fatalf("empty pre-dispatch set = %+v, want a recorded empty set", empty.PreDispatch)
	}
	if err := tryLedger(ctx, l.st, l.second.ID, ledgerSet(8)); !errors.Is(err, domain.ErrFollowUpFilingPreDispatchFixed) {
		t.Errorf("a set over a recorded empty set: error = %v, want %v", err, domain.ErrFollowUpFilingPreDispatchFixed)
	}
}

// TestFollowUpFilingAttemptRuleInStore pins that the store, not its caller,
// enforces the §5.17 evidence rule: a dispatch marker is written only when
// no earlier create for the instance can have committed.
func TestFollowUpFilingAttemptRuleInStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cases := []struct {
		name    string
		history []ledgerOp
		want    error
	}{
		{"first attempt", []ledgerOp{ledgerSet()}, nil},
		{"after a transient rejection", []ledgerOp{ledgerSet(), ledgerAttempt, ledgerTransient}, nil},
		{"after a definite rejection", []ledgerOp{ledgerSet(), ledgerAttempt, ledgerDefinite}, domain.ErrFollowUpFilingCreateUnproven},
		{"after an unproven response", []ledgerOp{ledgerSet(), ledgerAttempt, ledgerUnproven}, domain.ErrFollowUpFilingCreateUnproven},
		{"after a marker with no response", []ledgerOp{ledgerSet(), ledgerAttempt}, domain.ErrFollowUpFilingCreateUnproven},
		{"after a success", []ledgerOp{ledgerSet(), ledgerAttempt, ledgerSuccess(40)}, domain.ErrFollowUpFilingIntentResolved},
		{"before the pre-dispatch set", nil, domain.ErrFollowUpFilingPreDispatchMissing},
		{"after a refusal", []ledgerOp{ledgerRefuse(domain.FollowUpFilingRefusalPreconditionFailed)}, domain.ErrFollowUpFilingIntentResolved},
		{"after an ambiguous outcome", []ledgerOp{ledgerSet(), ledgerAttempt, ledgerAmbiguous}, domain.ErrFollowUpFilingIntentResolved},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l := seedLedger(t, ctx)
			mustLedger(t, ctx, l.st, l.first.ID, append([]ledgerOp{ledgerOpen}, tc.history...)...)
			before, err := ledgerIntent(ctx, l.st, l.first.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := tryLedger(ctx, l.st, l.first.ID, ledgerAttempt); !errors.Is(err, tc.want) {
				t.Fatalf("attempt error = %v, want %v", err, tc.want)
			}
			after, err := ledgerIntent(ctx, l.st, l.first.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantAttempts := len(before.Attempts)
			if tc.want == nil {
				wantAttempts++
			}
			if len(after.Attempts) != wantAttempts {
				t.Fatalf("attempts = %d, want %d", len(after.Attempts), wantAttempts)
			}
		})
	}
	t.Run("no intent", func(t *testing.T) {
		t.Parallel()
		l := seedLedger(t, ctx)
		if err := tryLedger(ctx, l.st, l.first.ID, ledgerAttempt); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("attempt with no intent: error = %v, want %v", err, store.ErrNotFound)
		}
	})
}

func TestFollowUpFilingAdoption(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dispatched := []ledgerOp{ledgerOpen, ledgerSet(7), ledgerAttempt, ledgerUnproven}
	cases := []struct {
		name string
		// earlier resolves the repository's first intent before the adopting
		// instance opens its own.
		earlier []ledgerOp
		history []ledgerOp
		issue   int
		want    error
	}{
		{"an issue found after an unproven attempt", nil, dispatched, 41, nil},
		{"no dispatched attempt", nil, []ledgerOp{ledgerOpen, ledgerSet(7)}, 41, domain.ErrFollowUpFilingAdoptionRefused},
		{"an issue in the pre-dispatch set", nil, dispatched, 7, domain.ErrFollowUpFilingAdoptionRefused},
		{
			"an issue another instance ledgered",
			[]ledgerOp{ledgerOpen, ledgerSet(), ledgerAttempt, ledgerSuccess(40)},
			dispatched, 40, domain.ErrFollowUpFilingIssueLedgered,
		},
		{
			"a repository holding an ambiguous outcome",
			[]ledgerOp{ledgerOpen, ledgerSet(), ledgerAttempt, ledgerAmbiguous},
			dispatched, 41, domain.ErrFollowUpFilingAdoptionRefused,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l := seedLedger(t, ctx)
			mustLedger(t, ctx, l.st, l.first.ID, tc.earlier...)
			mustLedger(t, ctx, l.st, l.second.ID, tc.history...)
			var filed domain.FiledFollowUpIssue
			err := l.st.WriteInternal(ctx, func(tx *store.InternalTx) error {
				var err error
				filed, err = tx.AdoptFollowUpFilingCandidate(ctx, l.second.ID, tc.issue, ledgerAt.Add(time.Hour))
				return err
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("adopt error = %v, want %v", err, tc.want)
			}
			intent, readErr := ledgerIntent(ctx, l.st, l.second.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if tc.want != nil {
				if !intent.Outstanding() {
					t.Fatalf("a refused adoption resolved the intent: %+v", intent.Terminal)
				}
				return
			}
			want := domain.FiledFollowUpIssue{
				InstanceID: l.second.ID, RepositoryID: 123, IssueNumber: tc.issue,
				Origin:     domain.FollowUpFilingOrigin{ProjectID: l.fx.projectID, RunID: l.fx.runID},
				LedgeredAt: ledgerAt.Add(time.Hour),
			}
			if filed != want {
				t.Fatalf("filed issue = %+v, want %+v", filed, want)
			}
			if intent.Terminal == nil || intent.Terminal.Outcome != domain.FollowUpFilingLedgered {
				t.Fatalf("terminal = %+v, want ledgered", intent.Terminal)
			}
			stored, err := ledgerFiledIssue(ctx, l.st, 123, tc.issue)
			if err != nil || stored == nil || *stored != want {
				t.Fatalf("ledger row = %+v, %v; want %+v", stored, err, want)
			}
		})
	}
}

func TestFollowUpFilingTerminalOutcomeRulesInStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	refuse := func(reason domain.FollowUpFilingRefusalReason) ledgerOp { return ledgerRefuse(reason) }
	cases := []struct {
		name    string
		history []ledgerOp
		op      ledgerOp
		want    error
	}{
		{
			"refused while the last attempt is unproven",
			[]ledgerOp{ledgerSet(), ledgerAttempt, ledgerUnproven},
			refuse(domain.FollowUpFilingRefusalPreconditionFailed), domain.ErrFollowUpFilingOutcomeRefused,
		},
		{
			"refused on a marker with no response",
			[]ledgerOp{ledgerSet(), ledgerAttempt},
			refuse(domain.FollowUpFilingRefusalDefiniteRejection), domain.ErrFollowUpFilingOutcomeRefused,
		},
		{
			"definite rejection after a transient one",
			[]ledgerOp{ledgerSet(), ledgerAttempt, ledgerTransient},
			refuse(domain.FollowUpFilingRefusalDefiniteRejection), domain.ErrFollowUpFilingOutcomeRefused,
		},
		{
			"retry bound spent after a definite rejection",
			[]ledgerOp{ledgerSet(), ledgerAttempt, ledgerDefinite},
			refuse(domain.FollowUpFilingRefusalRetryBoundSpent), domain.ErrFollowUpFilingOutcomeRefused,
		},
		{
			"retry bound spent with no attempt",
			[]ledgerOp{ledgerSet()},
			refuse(domain.FollowUpFilingRefusalRetryBoundSpent), domain.ErrFollowUpFilingOutcomeRefused,
		},
		{"ambiguous with no attempt", []ledgerOp{ledgerSet()}, ledgerAmbiguous, domain.ErrFollowUpFilingOutcomeRefused},
		{
			"definite rejection after one",
			[]ledgerOp{ledgerSet(), ledgerAttempt, ledgerDefinite},
			refuse(domain.FollowUpFilingRefusalDefiniteRejection), nil,
		},
		{
			"retry bound spent after a transient rejection",
			[]ledgerOp{ledgerSet(), ledgerAttempt, ledgerTransient},
			refuse(domain.FollowUpFilingRefusalRetryBoundSpent), nil,
		},
		{
			"precondition failed before any attempt", nil,
			refuse(domain.FollowUpFilingRefusalPreconditionFailed), nil,
		},
		{"ambiguous after an unproven attempt", []ledgerOp{ledgerSet(), ledgerAttempt, ledgerUnproven}, ledgerAmbiguous, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l := seedLedger(t, ctx)
			mustLedger(t, ctx, l.st, l.first.ID, append([]ledgerOp{ledgerOpen}, tc.history...)...)
			if err := tryLedger(ctx, l.st, l.first.ID, tc.op); !errors.Is(err, tc.want) {
				t.Fatalf("outcome error = %v, want %v", err, tc.want)
			}
			intent, err := ledgerIntent(ctx, l.st, l.first.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want != nil {
				if !intent.Outstanding() {
					t.Fatalf("a refused outcome resolved the intent: %+v", intent.Terminal)
				}
				return
			}
			// A terminal outcome is permanent: a second, different one and
			// every other write are refused and leave it as recorded.
			for name, later := range map[string]ledgerOp{
				"ambiguous": ledgerAmbiguous, "attempt": ledgerAttempt, "adopt": ledgerAdopt(41),
				"refuse": refuse(domain.FollowUpFilingRefusalPreconditionFailed), "success": ledgerSuccess(41),
				"response": ledgerUnproven, "set": ledgerSet(9),
			} {
				if err := tryLedger(ctx, l.st, l.first.ID, later); !errors.Is(err, domain.ErrFollowUpFilingIntentResolved) {
					t.Errorf("%s after a terminal outcome: error = %v, want %v", name, err, domain.ErrFollowUpFilingIntentResolved)
				}
			}
			again, err := ledgerIntent(ctx, l.st, l.first.ID)
			if err != nil || !reflect.DeepEqual(again, intent) {
				t.Fatalf("intent after refused writes = %+v, %v; want %+v", again, err, intent)
			}
		})
	}
}

// TestFollowUpFilingLedgerRowsAreAppendOnly pins the schema's half of the
// ledger's immutability, on a direct connection the store's checks do not
// guard: recorded history cannot be rewritten or removed.
func TestFollowUpFilingLedgerRowsAreAppendOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	l := seedLedger(t, ctx)
	mustLedger(t, ctx, l.st, l.first.ID, ledgerOpen, ledgerSet(7), ledgerAttempt, ledgerSuccess(40))
	mustLedger(t, ctx, l.st, l.second.ID, ledgerOpen, ledgerSet(7), ledgerAttempt, ledgerTransient, ledgerAttempt)
	first, second := string(l.first.ID), string(l.second.ID)
	body := `(SELECT body FROM follow_up_filed_issues WHERE instance_id = '` + first + `')`
	refused := map[string]string{
		"update a ledger row":                 `UPDATE follow_up_filed_issues SET body = body WHERE instance_id = '` + first + `'`,
		"change a ledger row's origin":        `UPDATE follow_up_filed_issues SET body = json_set(body, '$.origin.run_id', 'other-run') WHERE instance_id = '` + first + `'`,
		"delete a ledger row":                 `DELETE FROM follow_up_filed_issues WHERE instance_id = '` + first + `'`,
		"a second issue for one instance":     `INSERT INTO follow_up_filed_issues (repository_id, issue_number, instance_id, body) VALUES (123, 41, '` + first + `', ` + body + `)`,
		"one issue for a second instance":     `INSERT INTO follow_up_filed_issues (repository_id, issue_number, instance_id, body) VALUES (123, 40, '` + second + `', ` + body + `)`,
		"change a terminal outcome":           `UPDATE follow_up_filing_intents SET outcome = 'ambiguous' WHERE instance_id = '` + first + `'`,
		"reopen a resolved intent":            `UPDATE follow_up_filing_intents SET outcome = NULL, resolved_at = NULL WHERE instance_id = '` + first + `'`,
		"change a recorded pre-dispatch set":  `UPDATE follow_up_filing_intents SET pre_dispatch_issue_numbers = '[]' WHERE instance_id = '` + second + `'`,
		"change a recorded identity":          `UPDATE follow_up_filing_intents SET pre_dispatch_bot_user_id = 4200 WHERE instance_id = '` + second + `'`,
		"clear a recorded identity":           `UPDATE follow_up_filing_intents SET pre_dispatch_bot_user_id = NULL WHERE instance_id = '` + second + `'`,
		"move an intent to another repo":      `UPDATE follow_up_filing_intents SET repository_id = 456 WHERE instance_id = '` + second + `'`,
		"delete an intent":                    `DELETE FROM follow_up_filing_intents WHERE instance_id = '` + second + `'`,
		"change a recorded response":          `UPDATE follow_up_filing_attempts SET response_class = 'unproven' WHERE instance_id = '` + second + `' AND ordinal = 1`,
		"clear a recorded response":           `UPDATE follow_up_filing_attempts SET response_class = NULL, response_recorded_at = NULL WHERE instance_id = '` + second + `' AND ordinal = 1`,
		"move a dispatch marker":              `UPDATE follow_up_filing_attempts SET dispatch_started_at = '2026-01-01T00:00:00Z' WHERE instance_id = '` + second + `' AND ordinal = 2`,
		"delete an attempt":                   `DELETE FROM follow_up_filing_attempts WHERE instance_id = '` + second + `' AND ordinal = 2`,
		"an outcome with half its columns":    `UPDATE follow_up_filing_intents SET outcome = 'ambiguous' WHERE instance_id = '` + second + `'`,
		"a refusal with no reason":            `UPDATE follow_up_filing_intents SET outcome = 'refused', resolved_at = '2026-09-02T12:00:00Z' WHERE instance_id = '` + second + `'`,
		"a success response with no issue":    `UPDATE follow_up_filing_attempts SET response_class = 'success', response_recorded_at = '2026-09-02T12:00:00Z' WHERE instance_id = '` + second + `' AND ordinal = 2`,
		"an attempt for an instance unopened": `INSERT INTO follow_up_filing_attempts (instance_id, ordinal, dispatch_started_at) VALUES ('proposal-missing', 1, '2026-09-02T12:00:00Z')`,
	}
	l.raw(t, func(raw *sql.DB) {
		if _, err := raw.Exec(`PRAGMA foreign_keys = ON`); err != nil {
			t.Fatal(err)
		}
		for name, statement := range refused {
			if _, err := raw.Exec(statement); err == nil {
				t.Errorf("the schema allowed a statement to %s", name)
			}
		}
	})
	// Nothing above changed a row: both intents read back as they were.
	for id, wantAttempts := range map[domain.ProposalInstanceID]int{l.first.ID: 1, l.second.ID: 2} {
		intent, err := ledgerIntent(ctx, l.st, id)
		if err != nil || len(intent.Attempts) != wantAttempts {
			t.Fatalf("intent %q after refused statements = %+v, %v", id, intent, err)
		}
		if got := intent.PreDispatch.BotUserID; got == nil || *got != ledgerBotUserID {
			t.Fatalf("intent %q identity after refused statements = %v, want bot user %d", id, got, ledgerBotUserID)
		}
	}
}

// TestFollowUpFilingReconstructionFailsClosed pins the read boundary: a
// ledger fact whose rows no longer support it is an error from every read
// that reaches it, and never reads as "nothing recorded". Reads select by key
// column, so each case names the reads its tamper is visible to.
func TestFollowUpFilingReconstructionFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const filed = ` WHERE repository_id = 123 AND issue_number = 40`
	cases := []struct {
		name string
		// statements build the tamper from the two instance ids.
		statements func(first, second string) []string
		// issue is the ledger key whose read must fail; 0 when the tamper
		// leaves no ledger row to read.
		issue int
		// intent picks the instance whose intent read must fail, if any.
		intent func(ledgerFixture) domain.ProposalInstanceID
		// repository says the repository-wide intent reads must fail too.
		repository bool
	}{
		{
			"the intent names a repository its proposal does not target", func(first, _ string) []string {
				return []string{`UPDATE follow_up_filing_intents SET repository_id = 999 WHERE instance_id = '` + first + `'`}
			}, 40, func(l ledgerFixture) domain.ProposalInstanceID { return l.first.ID }, false,
		},
		{
			"the ledger row's origin is not the proposal's subject", func(string, string) []string {
				return []string{`UPDATE follow_up_filed_issues SET body = json_set(body, '$.origin.run_id', 'other-run')` + filed}
			}, 40, nil, false,
		},
		{
			"the ledger row's intent is not ledgered", func(_, second string) []string {
				return []string{`INSERT INTO follow_up_filed_issues (repository_id, issue_number, instance_id, body)
				SELECT 123, 41, '` + second + `', json_set(body, '$.issue_number', 41, '$.instance_id', '` + second + `')
				FROM follow_up_filed_issues` + filed}
			}, 41, func(l ledgerFixture) domain.ProposalInstanceID { return l.second.ID }, true,
		},
		{
			"no attempt backs the ledger row's issue number", func(string, string) []string {
				return []string{`UPDATE follow_up_filed_issues SET issue_number = 41, body = json_set(body, '$.issue_number', 41)` + filed}
			}, 41, nil, false,
		},
		{
			"the ledger row's body disagrees with its key columns", func(string, string) []string {
				return []string{`UPDATE follow_up_filed_issues SET issue_number = 41` + filed}
			}, 41, nil, false,
		},
		{
			"the ledgered intent has no ledger row", func(string, string) []string {
				return []string{`DELETE FROM follow_up_filed_issues` + filed}
			}, 0, func(l ledgerFixture) domain.ProposalInstanceID { return l.first.ID }, true,
		},
		{
			"the intent's approval is no longer recorded", func(first, _ string) []string {
				return []string{`UPDATE effect_proposal_decisions SET action = 'decline', selected_digest = NULL WHERE instance_id = '` + first + `'`}
			}, 40, func(l ledgerFixture) domain.ProposalInstanceID { return l.first.ID }, true,
		},
		{
			"the attempts were removed", func(first, _ string) []string {
				return []string{`DELETE FROM follow_up_filing_attempts WHERE instance_id = '` + first + `'`}
			}, 40, func(l ledgerFixture) domain.ProposalInstanceID { return l.first.ID }, true,
		},
		{
			"the pre-dispatch set is not canonical", func(first, _ string) []string {
				return []string{`UPDATE follow_up_filing_intents SET pre_dispatch_issue_numbers = '[31, 7]' WHERE instance_id = '` + first + `'`}
			}, 40, func(l ledgerFixture) domain.ProposalInstanceID { return l.first.ID }, true,
		},
		{
			// Without its attempt and set the row would otherwise read as an
			// intent that has listed nothing yet, and take a new set under
			// whatever identity came next.
			"the dispatching identity is recorded without its set", func(_, second string) []string {
				return []string{
					`DELETE FROM follow_up_filing_attempts WHERE instance_id = '` + second + `'`,
					`UPDATE follow_up_filing_intents SET pre_dispatch_issue_numbers = NULL, pre_dispatch_recorded_at = NULL WHERE instance_id = '` + second + `'`,
				}
			}, 0, func(l ledgerFixture) domain.ProposalInstanceID { return l.second.ID }, true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l := seedLedger(t, ctx)
			mustLedger(t, ctx, l.st, l.first.ID, ledgerOpen, ledgerSet(7, 31), ledgerAttempt, ledgerSuccess(40))
			mustLedger(t, ctx, l.st, l.second.ID, ledgerOpen, ledgerSet(7, 31), ledgerAttempt, ledgerUnproven)
			l.tamper(t, tc.statements(string(l.first.ID), string(l.second.ID))...)

			assertUnreadable := func(what string, err error) {
				t.Helper()
				if !errors.Is(err, store.ErrRowInconsistent) {
					t.Errorf("%s: error = %v, want %v", what, err, store.ErrRowInconsistent)
				}
				// The refusal must not be mistakable for an absent fact.
				if errors.Is(err, store.ErrNotFound) || errors.Is(err, domain.ErrFollowUpFilingNotApproved) {
					t.Errorf("%s: error %v reads as nothing recorded", what, err)
				}
			}
			may, err := ledgerMayHaveCreated(ctx, l.st, 123)
			assertUnreadable("may have created", err)
			if may {
				t.Error("may have created answered alongside its error")
			}
			if tc.issue != 0 {
				row, err := ledgerFiledIssue(ctx, l.st, 123, tc.issue)
				assertUnreadable("filed issue", err)
				if row != nil {
					t.Error("filed issue returned a row alongside its error")
				}
			}
			if tc.intent != nil {
				_, err := ledgerIntent(ctx, l.st, tc.intent(l))
				assertUnreadable("intent", err)
				row, err := ledgerFiledIssueFor(ctx, l.st, tc.intent(l))
				assertUnreadable("filed issue for instance", err)
				if row != nil {
					t.Error("filed issue for instance returned a row alongside its error")
				}
				// No write proceeds from an intent the store cannot read.
				assertUnreadable("ambiguous outcome", tryLedger(ctx, l.st, tc.intent(l), ledgerAmbiguous))
			}
			if tc.repository {
				_, err := readLedger(ctx, l.st, func(tx *store.ReadTx) (*domain.FollowUpFilingIntent, error) {
					return tx.OutstandingFollowUpFilingIntent(ctx, 123)
				})
				assertUnreadable("outstanding intent", err)
				_, err = readLedger(ctx, l.st, func(tx *store.ReadTx) (bool, error) {
					return tx.FollowUpFilingRepositoryAmbiguous(ctx, 123)
				})
				assertUnreadable("repository ambiguity", err)
			}
		})
	}
}

// TestFollowUpFilingIntentOpenReGatesApproval pins that opening an intent
// re-derives the approval instead of trusting the decision row's action: a
// decline row rewritten to approve still names its decline command.
func TestFollowUpFilingIntentOpenReGatesApproval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	st := storetest.Open(t, path, store.Options{})
	fx := seedFiling(t, ctx, st, "project-filing")
	declined, decline := openFilingCard(t, ctx, st, fx,
		fx.proposal(t, fx.input(fx.deferredSource())), "event-declined", domain.ActionDecline)
	if err := decideFiling(ctx, st, declined, decline); err != nil {
		t.Fatal(err)
	}
	l := ledgerFixture{path: path, st: st, fx: fx}
	l.raw(t, func(raw *sql.DB) {
		result, err := raw.Exec(`UPDATE effect_proposal_decisions SET action = 'approve', selected_digest = ?
			WHERE instance_id = ?`, declined.Proposal.Digest, declined.ID)
		if err != nil {
			t.Fatal(err)
		}
		if changed, err := result.RowsAffected(); err != nil || changed != 1 {
			t.Fatalf("forged approval changed %d rows, %v", changed, err)
		}
	})
	if err := tryLedger(ctx, l.st, declined.ID, ledgerOpen); !errors.Is(err, store.ErrRowInconsistent) {
		t.Fatalf("open over a forged approval: error = %v, want %v", err, store.ErrRowInconsistent)
	}
}

// TestFollowUpFilingLedgerNormalizesCallerTimes pins that every write takes a
// time in any zone. Ledgering is the write that follows a create that cannot
// be undone, so it must not be the one that refuses a local-zone time.
func TestFollowUpFilingLedgerNormalizesCallerTimes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	zone := time.FixedZone("ahead", 2*60*60)
	at := ledgerAt.In(zone)
	for _, tc := range []struct {
		name   string
		ledger func(*store.InternalTx, domain.ProposalInstanceID) (domain.FiledFollowUpIssue, error)
	}{
		{"success", func(tx *store.InternalTx, id domain.ProposalInstanceID) (domain.FiledFollowUpIssue, error) {
			return tx.LedgerFollowUpFilingSuccess(ctx, id, 40, at)
		}},
		{"adoption", func(tx *store.InternalTx, id domain.ProposalInstanceID) (domain.FiledFollowUpIssue, error) {
			return tx.AdoptFollowUpFilingCandidate(ctx, id, 40, at)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l := seedLedger(t, ctx)
			var filed domain.FiledFollowUpIssue
			if err := l.st.WriteInternal(ctx, func(tx *store.InternalTx) error {
				if _, err := tx.OpenFollowUpFilingIntent(ctx, l.first.ID, at); err != nil {
					return err
				}
				if _, err := tx.RecordFollowUpFilingPreDispatch(ctx, l.first.ID, nil, ledgerBotUserID, at); err != nil {
					return err
				}
				if _, err := tx.StartFollowUpFilingAttempt(ctx, l.first.ID, at); err != nil {
					return err
				}
				var err error
				filed, err = tc.ledger(tx, l.first.ID)
				return err
			}); err != nil {
				t.Fatalf("ledger with times in %s: %v", zone, err)
			}
			if !filed.LedgeredAt.Equal(ledgerAt) || filed.LedgeredAt.Location() != time.UTC {
				t.Errorf("ledgered_at = %v, want %v in UTC", filed.LedgeredAt, ledgerAt)
			}
			intent, err := ledgerIntent(ctx, l.st, l.first.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !intent.Terminal.ResolvedAt.Equal(ledgerAt) {
				t.Errorf("resolved_at = %v, want %v", intent.Terminal.ResolvedAt, ledgerAt)
			}
		})
	}
}

// TestFiledFollowUpIssueForInstanceReadsNoneThroughTheIntent pins the two
// states the instance read answers with no row: no intent, and an intent
// that has not ledgered. The ledgered intent whose row is gone is a tamper
// case in TestFollowUpFilingReconstructionFailsClosed.
func TestFiledFollowUpIssueForInstanceReadsNoneThroughTheIntent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	l := seedLedger(t, ctx)
	mustLedger(t, ctx, l.st, l.first.ID, ledgerOpen, ledgerSet(), ledgerAttempt, ledgerUnproven)
	for name, id := range map[string]domain.ProposalInstanceID{
		"no intent": l.second.ID, "an unresolved intent": l.first.ID,
	} {
		row, err := ledgerFiledIssueFor(ctx, l.st, id)
		if err != nil || row != nil {
			t.Errorf("%s: filed issue = %+v, %v, want none", name, row, err)
		}
	}
}

func TestFollowUpFilingMayHaveCreatedInStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cases := []struct {
		name    string
		history []ledgerOp
		want    bool
	}{
		{"no intent", nil, false},
		{"an open intent with no attempt", []ledgerOp{ledgerOpen, ledgerSet()}, false},
		{"only rejected attempts", []ledgerOp{
			ledgerOpen, ledgerSet(), ledgerAttempt, ledgerTransient, ledgerAttempt, ledgerDefinite,
		}, false},
		{"a refusal after rejections", []ledgerOp{
			ledgerOpen, ledgerSet(), ledgerAttempt, ledgerTransient,
			ledgerRefuse(domain.FollowUpFilingRefusalRetryBoundSpent),
		}, false},
		{"a marker with no response", []ledgerOp{ledgerOpen, ledgerSet(), ledgerAttempt}, true},
		{"an unproven attempt", []ledgerOp{ledgerOpen, ledgerSet(), ledgerAttempt, ledgerUnproven}, true},
		{"an ambiguous outcome", []ledgerOp{ledgerOpen, ledgerSet(), ledgerAttempt, ledgerTransient, ledgerAmbiguous}, true},
		{"a ledger row", []ledgerOp{ledgerOpen, ledgerSet(), ledgerAttempt, ledgerSuccess(40)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l := seedLedger(t, ctx)
			mustLedger(t, ctx, l.st, l.first.ID, tc.history...)
			got, err := ledgerMayHaveCreated(ctx, l.st, 123)
			if err != nil || got != tc.want {
				t.Fatalf("may have created = %v, %v; want %v", got, err, tc.want)
			}
			// The answer is per repository: another one is untouched.
			if elsewhere, err := ledgerMayHaveCreated(ctx, l.st, 456); err != nil || elsewhere {
				t.Fatalf("may have created in another repository = %v, %v; want false", elsewhere, err)
			}
		})
	}
}

// TestFollowUpFilingLedgerSurvivesPlaintextRestore pins that a restore
// round-trips all three ledger tables through their append-only delete
// guards and reinstates the guards before it commits.
func TestFollowUpFilingLedgerSurvivesPlaintextRestore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	l := seedLedger(t, ctx)
	mustLedger(t, ctx, l.st, l.first.ID, ledgerOpen, ledgerSet(7), ledgerAttempt, ledgerSuccess(40))
	mustLedger(t, ctx, l.st, l.second.ID, ledgerOpen, ledgerSet(7, 40), ledgerAttempt, ledgerTransient, ledgerAttempt)
	read := func() (intents [2]domain.FollowUpFilingIntent, filed *domain.FiledFollowUpIssue) {
		t.Helper()
		for index, id := range []domain.ProposalInstanceID{l.first.ID, l.second.ID} {
			intent, err := ledgerIntent(ctx, l.st, id)
			if err != nil {
				t.Fatal(err)
			}
			intents[index] = intent
		}
		filed, err := ledgerFiledIssue(ctx, l.st, 123, 40)
		if err != nil || filed == nil {
			t.Fatalf("filed issue = %+v, %v", filed, err)
		}
		return intents, filed
	}
	wantIntents, wantFiled := read()

	checkpoint := filepath.Join(t.TempDir(), "ledger-checkpoint.db")
	if err := l.st.Checkpoint(ctx, checkpoint); err != nil {
		t.Fatal(err)
	}
	mustLedger(t, ctx, l.st, l.second.ID, ledgerUnproven, ledgerAdopt(41))
	if _, err := l.st.Restore(ctx, checkpoint); err != nil {
		t.Fatalf("restore over a non-empty ledger: %v", err)
	}

	gotIntents, gotFiled := read()
	if !reflect.DeepEqual(gotIntents, wantIntents) || *gotFiled != *wantFiled {
		t.Fatalf("restored ledger = %+v, %+v; want %+v, %+v", gotIntents, gotFiled, wantIntents, wantFiled)
	}
	if later, err := ledgerFiledIssue(ctx, l.st, 123, 41); err != nil || later != nil {
		t.Fatalf("a row ledgered after the checkpoint survived the restore: %+v, %v", later, err)
	}
	l.raw(t, func(raw *sql.DB) {
		for _, statement := range []string{
			`DELETE FROM follow_up_filed_issues`, `DELETE FROM follow_up_filing_attempts`,
			`DELETE FROM follow_up_filing_intents`,
		} {
			if _, err := raw.Exec(statement); err == nil {
				t.Errorf("%q succeeded: the restore lost a delete guard", statement)
			}
		}
	})
}

// TestFollowUpFilingLedgerRowGolden pins the stored ledger body, the one
// ledger representation that is canonical JSON.
func TestFollowUpFilingLedgerRowGolden(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	l := seedLedger(t, ctx)
	mustLedger(t, ctx, l.st, l.first.ID, ledgerOpen, ledgerSet(7), ledgerAttempt, ledgerSuccess(40))
	filed, err := readLedger(ctx, l.st, func(tx *store.ReadTx) (*domain.FiledFollowUpIssue, error) {
		return tx.FiledFollowUpIssueForInstance(ctx, l.first.ID)
	})
	if err != nil || filed == nil {
		t.Fatalf("filed issue = %+v, %v", filed, err)
	}
	// The instance id is random per admission; the golden pins the shape.
	fixed := *filed
	fixed.InstanceID = "proposal-fixed"
	golden.Assert(t, "filed_follow_up_issue", marshalIndent(t, fixed))
}
