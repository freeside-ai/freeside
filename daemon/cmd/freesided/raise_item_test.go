package main

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// The representative fixture's runs (daemon/internal/seedfixture/runs.go),
// named by the outcome each stands for here.
const (
	fixtureCompletedRun domain.RunID = "run-freeside-640"
	fixturePublishedRun domain.RunID = "run-freeside-654"
	fixturePendingRun   domain.RunID = "run-freeside-question"
)

func TestRaiseAlertsAreRegistered(t *testing.T) {
	for _, alert := range AllRaiseAlerts {
		if !alert.valid() {
			t.Errorf("registered alert %q is not valid", alert)
		}
	}
	if raiseAlert("").valid() {
		t.Error("the zero alert is valid")
	}
	var out bytes.Buffer
	if err := writeRaiseAlerts(&out); err != nil {
		t.Fatal(err)
	}
	want := "stall\nheld-work\nreview-growth\n"
	if out.String() != want {
		t.Errorf("-list printed %q, want %q", out.String(), want)
	}
	if got := len(strings.Fields(out.String())); got != len(AllRaiseAlerts) {
		t.Errorf("-list printed %d names for %d registered alerts", got, len(AllRaiseAlerts))
	}
}

func TestRaiseItemCommandLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		list bool
		want string
	}{
		{name: "list", args: []string{"-list"}, list: true},
		{name: "list with another flag", args: []string{"-list", "-db", "x"}, want: "-list takes no other flag"},
		{name: "no database", args: []string{"-alert", "stall"}, want: "-db is required"},
		{name: "no alert", args: []string{"-db", "x"}, want: "-alert is required"},
		{name: "unknown alert", args: []string{"-db", "x", "-alert", "nope"}, want: `-alert "nope" is not one of`},
		{name: "held work without run", args: []string{"-db", "x", "-alert", "held-work"}, want: "requires -run"},
		{name: "review growth without run", args: []string{"-db", "x", "-alert", "review-growth"}, want: "requires -run"},
		{name: "stall with run", args: []string{"-db", "x", "-alert", "stall", "-run", "r"}, want: "takes no -run"},
		{name: "stall", args: []string{"-db", "x", "-alert", "stall"}},
		{name: "positional", args: []string{"-db", "x", "-alert", "stall", "extra"}, want: "unexpected positional"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, list, err := parseRaiseItemCommand(tc.args, io.Discard)
			if tc.want == "" {
				if err != nil || list != tc.list {
					t.Fatalf("parse = list %v, %v; want list %v", list, err, tc.list)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parse = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

// The request goes through the control socket of a daemon running in each
// tier, so the tier that decides is the one the daemon was started with.
// Every tier is named here through AllEnvironments, so a new tier fails this
// test until someone decides whether it may raise items.
func TestRaiseItemRefusedOutsideEphemeral(t *testing.T) {
	for _, env := range AllEnvironments {
		t.Run(string(env), func(t *testing.T) {
			// The supervised-path guards of dev and prod run in main, not
			// run, so a temporary database is safe here.
			dbPath := filepath.Join(t.TempDir(), "freeside.db")
			h := startDaemon(t, config{Environment: env, DBPath: dbPath, ListenAddr: "127.0.0.1:0"})
			defer func() { _ = h.Close() }()
			before := len(storedItems(t, h.store))
			_, err := runRaiseItemCommand(t.Context(), raiseItemCommandConfig{DBPath: dbPath, Alert: raiseAlertStall})
			raised := len(storedItems(t, h.store)) - before
			switch env {
			case environmentEphemeral:
				if err != nil || raised != 1 {
					t.Fatalf("raise on %s = %v with %d new items, want one item", env, err, raised)
				}
			case environmentDev, environmentProd:
				if err == nil || !strings.Contains(err.Error(), `not "`+string(env)+`"`) {
					t.Fatalf("raise on %s = %v, want a refusal naming the tier", env, err)
				}
				if raised != 0 {
					t.Fatalf("refused raise on %s wrote %d items", env, raised)
				}
			default:
				t.Fatalf("tier %q has no expectation here", env)
			}
		})
	}
}

func TestRaiseItemRefusedWithoutDaemon(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	cfg := config{DBPath: dbPath, ListenAddr: "127.0.0.1:0", Environment: environmentEphemeral}
	h := startDaemon(t, cfg)
	before := len(storedItems(t, h.store))
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	_, err := runRaiseItemCommand(t.Context(), raiseItemCommandConfig{DBPath: dbPath, Alert: raiseAlertStall})
	if !errors.Is(err, errRaiseItemNoDaemon) {
		t.Fatalf("raise with no daemon = %v, want %v", err, errRaiseItemNoDaemon)
	}

	h = startDaemon(t, cfg)
	defer func() { _ = h.Close() }()
	if after := len(storedItems(t, h.store)); after != before {
		t.Fatalf("refused raise changed the store: %d items, was %d", after, before)
	}
}

// Each alert's item is raised through the control socket of a running daemon
// and read back from the list that daemon serves to clients.
func TestRaiseItemReachesClients(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	h := startDaemon(t, config{
		DBPath: dbPath, ListenAddr: "127.0.0.1:0",
		SeedFixture: "representative", Environment: environmentEphemeral,
	})
	defer func() { _ = h.Close() }()
	ctx := t.Context()
	runsBefore, err := h.attention.ListRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	decisionsBefore := reviewDecisions(t, h.store, fixtureCompletedRun)
	lastReviewRound := 0
	if err := h.store.Read(ctx, func(tx *store.ReadTx) error {
		records, err := tx.ListReviewRecords(ctx, fixtureCompletedRun)
		for _, record := range records {
			lastReviewRound = max(lastReviewRound, record.Round)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	completed, err := h.attention.GetRun(ctx, fixtureCompletedRun)
	if err != nil {
		t.Fatal(err)
	}

	for _, alert := range AllRaiseAlerts {
		t.Run(string(alert), func(t *testing.T) {
			cfg := raiseItemCommandConfig{DBPath: dbPath, Alert: alert}
			if alert.needsRun() {
				cfg.RunID = fixtureCompletedRun
			}
			result, err := runRaiseItemCommand(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			item := servedItem(t, h, result.ItemID)
			if result.Alert != alert || result.Type != item.Type {
				t.Errorf("result = %+v, served type %s", result, item.Type)
			}
			if item.Status != domain.StatusOpen {
				t.Errorf("status = %s, want open", item.Status)
			}
			switch alert {
			case raiseAlertStall:
				assertHealthNotice(t, item, "invocation_stalled")
				if item.Subject.Type != domain.SubjectSystem || item.Subject.ID != "daemon" {
					t.Errorf("subject = %+v, want the daemon", item.Subject)
				}
			case raiseAlertHeldWork:
				assertHealthNotice(t, item, "work_held")
				if item.Subject.Type != domain.SubjectTask || item.Subject.ID != domain.SubjectID(completed.Run.TaskID) {
					t.Errorf("subject = %+v, want task %s", item.Subject, completed.Run.TaskID)
				}
			case raiseAlertReviewGrowth:
				if item.Type != domain.AttentionReviewDiminishing || item.ReviewDiminishing == nil ||
					item.ReviewDiminishing.Cause != domain.ReviewDiminishingGrowthWithoutBlockers {
					t.Errorf("item = %s with facts %+v, want review growth", item.Type, item.ReviewDiminishing)
				}
				if item.Subject.Type != domain.SubjectRun || item.Subject.ID != domain.SubjectID(fixtureCompletedRun) {
					t.Errorf("subject = %+v, want run %s", item.Subject, fixtureCompletedRun)
				}
				// One round past the last review stays free for a review
				// pass that was already running when the unit completed.
				if want := store.ReviewDiminishingItemID(fixtureCompletedRun, lastReviewRound+2); result.ItemID != want {
					t.Errorf("review card is %s, want %s", result.ItemID, want)
				}
				// No review record stands behind the card, so the store never
				// authenticates it as a decision record. The engine reads a
				// decision only through that record, so one made on the card
				// reaches nothing.
				if err := h.store.Read(ctx, func(tx *store.ReadTx) error {
					_, err := tx.ReviewDiminishingDecision(ctx, result.ItemID)
					return err
				}); err == nil {
					t.Error("the raised review card authenticates as a real decision record")
				}
			default:
				t.Fatalf("alert %q has no expectation here", alert)
			}

			// A second raise is a second item, not a rewrite of the first.
			again, err := runRaiseItemCommand(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if again.ItemID == result.ItemID {
				t.Errorf("second raise reused item %s", again.ItemID)
			}
			servedItem(t, h, again.ItemID)
		})
	}

	// Raised items leave every run readable. None is excluded from the served
	// list, the completed run is still served, and the store's review
	// decisions for it, which authenticate the item at every reviewed round,
	// read as they did: the raised card sits one round past the last review.
	runsAfter, err := h.attention.ListRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runsAfter) != len(runsBefore) {
		t.Errorf("served runs = %d, were %d", len(runsAfter), len(runsBefore))
	}
	if _, err := h.attention.GetRun(ctx, fixtureCompletedRun); err != nil {
		t.Errorf("served run %s after raising: %v", fixtureCompletedRun, err)
	}
	if after := reviewDecisions(t, h.store, fixtureCompletedRun); after != decisionsBefore {
		t.Errorf("review decisions on %s = %d, were %d", fixtureCompletedRun, after, decisionsBefore)
	}
}

// The engine's sweep resolves any open notice whose ID has the real held-work
// form and whose run has no matching hold. A raised notice has no hold, so it
// stays open only because its ID has another form.
func TestRaisedHeldWorkItemSurvivesTheSweep(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	h := startDaemon(t, config{
		Environment: environmentEphemeral, DBPath: dbPath, ListenAddr: "127.0.0.1:0",
		FakeDriverEnabled: true, SeedWalkingSkeleton: true,
	})
	defer func() { _ = h.Close() }()
	ctx := t.Context()
	result, err := runRaiseItemCommand(ctx, raiseItemCommandConfig{
		DBPath: dbPath, Alert: raiseAlertHeldWork, RunID: defaultFakeRunID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.workflow.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if item := servedItem(t, h, result.ItemID); item.Status != domain.StatusOpen {
		t.Fatalf("raised held-work item is %s after a reconcile pass, want open", item.Status)
	}
}

// Nothing but a restart closes a raised held-work notice: it has no hold to
// clear and offers no action that resolves it.
func TestRaisedHeldWorkItemResolvesAtRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	cfg := config{DBPath: dbPath, ListenAddr: "127.0.0.1:0", Environment: environmentEphemeral}
	seeded := cfg
	seeded.SeedFixture = "representative"
	h := startDaemon(t, seeded)
	ctx := t.Context()
	result, err := runRaiseItemCommand(ctx, raiseItemCommandConfig{
		DBPath: dbPath, Alert: raiseAlertHeldWork, RunID: fixtureCompletedRun,
	})
	if err != nil {
		t.Fatal(err)
	}
	open := 0
	for _, item := range storedItems(t, h.store) {
		if item.Value.Status == domain.StatusOpen {
			open++
		}
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	h = startDaemon(t, cfg)
	defer func() { _ = h.Close() }()
	if item := servedItem(t, h, result.ItemID); item.Status != domain.StatusResolved {
		t.Fatalf("raised held-work item is %s after a restart, want resolved", item.Status)
	}
	stillOpen := 0
	for _, item := range storedItems(t, h.store) {
		if item.Value.Status == domain.StatusOpen {
			stillOpen++
		}
	}
	if stillOpen != open-1 {
		t.Fatalf("open items after a restart = %d, want %d: only the raised notice resolves", stillOpen, open-1)
	}
}

// Only a run whose work unit is complete never reviews again. A published run
// has concluded too, and is still refused: an external review cycle can give
// it another round.
func TestRaiseItemReviewGrowthNeedsACompletedRun(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	h := startDaemon(t, config{
		DBPath: dbPath, ListenAddr: "127.0.0.1:0",
		SeedFixture: "representative", Environment: environmentEphemeral,
	})
	defer func() { _ = h.Close() }()
	ctx := t.Context()
	before := len(storedItems(t, h.store))
	for _, run := range []domain.RunID{fixturePublishedRun, fixturePendingRun} {
		_, err := runRaiseItemCommand(ctx, raiseItemCommandConfig{
			DBPath: dbPath, Alert: raiseAlertReviewGrowth, RunID: run,
		})
		want := "run \"" + string(run) + "\" has no recorded completion"
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("raise on %s = %v, want an error containing %q", run, err, want)
		}
	}
	if after := len(storedItems(t, h.store)); after != before {
		t.Fatalf("refused raises wrote items: %d, were %d", after, before)
	}
}

func assertHealthNotice(t *testing.T, item domain.AttentionItem, code string) {
	t.Helper()
	if item.Type != domain.AttentionSystemHealth || item.HealthDiagnostic == nil || item.HealthDiagnostic.Code != code {
		t.Errorf("item = %s with diagnostic %+v, want system_health %s", item.Type, item.HealthDiagnostic, code)
	}
	if item.Posture == nil || *item.Posture != domain.HealthPostureAdvisory {
		t.Errorf("posture = %v, want advisory", item.Posture)
	}
}

// reviewDecisions counts the review decisions the store authenticates for a
// run, failing the test when that read is refused.
func reviewDecisions(t *testing.T, st *store.Store, runID domain.RunID) int {
	t.Helper()
	var count int
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		decisions, err := tx.ListReviewDiminishingDecisions(t.Context(), runID)
		count = len(decisions)
		return err
	}); err != nil {
		t.Fatalf("review decisions on %s: %v", runID, err)
	}
	return count
}

// servedItem reads one item from the list the daemon serves to clients.
func servedItem(t *testing.T, h *daemon, id domain.ItemID) domain.AttentionItem {
	t.Helper()
	var served []signet.AttentionItemSnapshot
	served, err := h.attention.ListAttentionItems(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range served {
		if snapshot.Item.ID == id {
			return snapshot.Item
		}
	}
	t.Fatalf("item %s is not in the served list of %d", id, len(served))
	return domain.AttentionItem{}
}

func storedItems(t *testing.T, st *store.Store) []store.Snapshotted[domain.AttentionItem] {
	t.Helper()
	var items []store.Snapshotted[domain.AttentionItem]
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		items, err = tx.ListAttentionItems(t.Context())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return items
}
