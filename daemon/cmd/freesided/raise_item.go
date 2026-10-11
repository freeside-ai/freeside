package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// raiseAlert names an alert whose attention item `freesided raise-item` can
// raise. It names the alert, not the attention type, because the stall and
// held-work alerts share system_health.
type raiseAlert string

const (
	raiseAlertStall        raiseAlert = "stall"
	raiseAlertHeldWork     raiseAlert = "held-work"
	raiseAlertReviewGrowth raiseAlert = "review-growth"
)

// AllRaiseAlerts is the single registration point for raisable alerts;
// `raise-item -list` prints it.
var AllRaiseAlerts = []raiseAlert{
	raiseAlertStall,
	raiseAlertHeldWork,
	raiseAlertReviewGrowth,
}

func (a raiseAlert) valid() bool {
	switch a {
	case raiseAlertStall, raiseAlertHeldWork, raiseAlertReviewGrowth:
		return true
	default:
		return false
	}
}

// needsRun reports whether the alert's item is about one run. The stall
// notice's subject is the daemon, so it takes none.
func (a raiseAlert) needsRun() bool {
	switch a {
	case raiseAlertHeldWork, raiseAlertReviewGrowth:
		return true
	case raiseAlertStall:
		return false
	}
	return false
}

const (
	raiseItemRoute = "/attention/raise"

	// raisedMarker labels every value the command invents, so a raised item
	// names itself as synthetic wherever that value is shown.
	raisedMarker = "synthetic-raise-item"

	// raisedHeldWorkItemPrefix must not parse as the engine's own held-work
	// notice ID (work-held-<run>-<unix>): the engine resolves every open
	// notice of that form whose run has no matching hold, and a raised item
	// has none. The daemon resolves these itself at its next start.
	raisedHeldWorkItemPrefix = "raised-work-held-"
)

var errRaiseItemNoDaemon = errors.New(
	"raise-item needs a running daemon started with -environment " + string(environmentEphemeral) +
		"; no daemon holds this database")

// runRaiseItemMain implements `freesided raise-item` (issue #1941): it raises
// the attention item behind one alert on a running ephemeral daemon, so an
// alert whose condition cannot be caused on cue (a stalled writer, held work,
// a review that keeps growing) can still be proven to reach the operator.
//
// The item is synthetic. Nothing stalled, nothing is held, and no review
// round stands behind it: the Go tests prove each condition raises its item,
// and this command proves the item is then delivered. It only adds items; it
// changes no run, hold, or existing item.
func runRaiseItemMain(args []string) {
	cfg, list, err := parseRaiseItemCommand(args, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "freesided raise-item:", err)
		os.Exit(2)
	}
	if list {
		if err := writeRaiseAlerts(os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "freesided:", err)
			os.Exit(1)
		}
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	result, err := runRaiseItemCommand(ctx, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "freesided:", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, "freesided:", err)
		os.Exit(1)
	}
}

func writeRaiseAlerts(w io.Writer) error {
	for _, alert := range AllRaiseAlerts {
		if _, err := fmt.Fprintln(w, alert); err != nil {
			return err
		}
	}
	return nil
}

// parseRaiseItemCommand reports list when -list was given: that mode prints
// the alert names and contacts no daemon, so it takes no other flag.
func parseRaiseItemCommand(args []string, stderr io.Writer) (_ raiseItemCommandConfig, list bool, _ error) {
	flags := flag.NewFlagSet("freesided raise-item", flag.ContinueOnError)
	flags.SetOutput(stderr)
	listAlerts := flags.Bool("list", false, "print the alert names and exit")
	dbPath := flags.String("db", "", "SQLite database path of the running daemon (required)")
	alert := flags.String("alert", "", "alert whose item to raise (required; see -list)")
	run := flags.String("run", "", "run the item is about (required by held-work and review-growth)")
	if err := flags.Parse(args); err != nil {
		return raiseItemCommandConfig{}, false, err
	}
	if flags.NArg() != 0 {
		return raiseItemCommandConfig{}, false, fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	if *listAlerts {
		if flags.NFlag() != 1 {
			return raiseItemCommandConfig{}, false, errors.New("-list takes no other flag")
		}
		return raiseItemCommandConfig{}, true, nil
	}
	cfg := raiseItemCommandConfig{DBPath: *dbPath, Alert: raiseAlert(*alert), RunID: domain.RunID(*run)}
	if err := validateRaiseItemConfig(cfg); err != nil {
		return raiseItemCommandConfig{}, false, err
	}
	return cfg, false, nil
}

type raiseItemCommandConfig struct {
	DBPath string
	Alert  raiseAlert
	RunID  domain.RunID
}

// raiseItemResult names the raised item so an exit record can cite it.
type raiseItemResult struct {
	ItemID domain.ItemID        `json:"item_id"`
	Type   domain.AttentionType `json:"type"`
	Alert  raiseAlert           `json:"alert"`
}

func validateRaiseItemConfig(cfg raiseItemCommandConfig) error {
	switch {
	case cfg.DBPath == "":
		return errors.New("-db is required")
	case cfg.Alert == "":
		return errors.New("-alert is required")
	case !cfg.Alert.valid():
		return fmt.Errorf("-alert %q is not one of %v", cfg.Alert, AllRaiseAlerts)
	case cfg.Alert.needsRun() && cfg.RunID == "":
		return fmt.Errorf("-alert %s requires -run", cfg.Alert)
	case !cfg.Alert.needsRun() && cfg.RunID != "":
		return fmt.Errorf("-alert %s takes no -run", cfg.Alert)
	default:
		return nil
	}
}

// runRaiseItemCommand sends the request to the daemon that holds the
// database. With no daemon it refuses instead of opening the store, because
// only a running daemon knows its tier: the store records none.
func runRaiseItemCommand(ctx context.Context, cfg raiseItemCommandConfig) (raiseItemResult, error) {
	if err := validateRaiseItemConfig(cfg); err != nil {
		return raiseItemResult{}, fmt.Errorf("raise-item: %w", err)
	}
	access, err := openStoreOrControl(cfg.DBPath)
	if err != nil {
		return raiseItemResult{}, fmt.Errorf("raise-item: %w", err)
	}
	if access.client == nil {
		return raiseItemResult{}, errors.Join(errRaiseItemNoDaemon, access.Close())
	}
	defer access.client.Close()
	cfg.DBPath = access.client.dbPath
	var result raiseItemResult
	err = access.client.call(ctx, raiseItemRoute, cfg, &result)
	return result, err
}

// raiseItem is the control route's body. The tier check comes first and is
// the only authority for it: the request names no tier, and a dev or prod
// daemon writes nothing.
//
// The write is the one every item takes, signet's intake policy and then
// tx.PutAttentionItem, so a raised item reaches the clients and the delivery
// pass exactly as the real condition's item does.
func raiseItem(
	ctx context.Context, st *store.Store, env environment, now time.Time, req raiseItemCommandConfig,
) (raiseItemResult, error) {
	if env != environmentEphemeral {
		return raiseItemResult{}, fmt.Errorf(
			"raise-item requires a daemon started with -environment %s, not %q", environmentEphemeral, env)
	}
	if err := validateRaiseItemConfig(req); err != nil {
		return raiseItemResult{}, fmt.Errorf("raise-item: %w", err)
	}
	var item domain.AttentionItem
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		var err error
		item, err = raisedItem(ctx, tx, req, now)
		if err != nil {
			return err
		}
		if err := signet.ValidateItemIntake(item); err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		return raiseItemResult{}, fmt.Errorf("raise-item: %s: %w", req.Alert, err)
	}
	return raiseItemResult{ItemID: item.ID, Type: item.Type, Alert: req.Alert}, nil
}

// requireCompletedRun refuses a review-growth item on any run the engine may
// still review. Such an item is the run's own decision record for its round:
// the engine and the store's convergence reads authenticate every item at a
// round's ID against that round's review record, and a raised item fails
// that proof. Raised past the run's last review, it is read by nothing
// until the run reviews that far.
//
// Only a run whose work unit is recorded complete never does. A published run
// reviews again in an external review cycle, a failed one after a retry, and
// a blocked one after its publication is re-evaluated, so a concluded run is
// not enough. The completion record is the authority the scheduler and the
// resource watch stop on, and the store re-derives it from the merge and
// issue facts on every read.
func requireCompletedRun(ctx context.Context, tx *store.WriteTx, runID domain.RunID) error {
	declaration, err := tx.GetWorkUnitDeclarationByRun(ctx, runID)
	if err == nil {
		_, err = tx.GetWorkUnitCompletion(ctx, declaration.ID)
	}
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("run %q has no recorded completion, and this alert needs a run whose work unit is complete",
			runID)
	}
	return err
}

func raisedItem(
	ctx context.Context, tx *store.WriteTx, req raiseItemCommandConfig, now time.Time,
) (domain.AttentionItem, error) {
	switch req.Alert {
	case raiseAlertStall:
		// The real hook's builder, for an invocation no run has. Like every
		// stall notice, the item resolves at the next daemon start.
		return invocationStalledItem(ctx, tx, raisedMarker, stallInterval(0), now)
	case raiseAlertHeldWork:
		return raisedHeldWorkItem(ctx, tx, req.RunID, now)
	case raiseAlertReviewGrowth:
		return raisedReviewGrowthItem(ctx, tx, req.RunID, now)
	}
	return domain.AttentionItem{}, fmt.Errorf("alert %q is not one of %v", req.Alert, AllRaiseAlerts)
}

// isRaisedHeldWorkNotice reports whether an item is a held-work notice this
// command raised. The daemon resolves these with the stall notices at
// startup: the engine resolves a real one when its hold clears and its sweep
// must not own a raised one, so nothing else ever would. The notice offers
// no action that closes it.
func isRaisedHeldWorkNotice(itemID string) bool {
	return strings.HasPrefix(itemID, raisedHeldWorkItemPrefix)
}

// raisedHeldWorkItem builds the engine's work_held notice for a hold that
// exists only here: no hold row is written, so the run is not held.
func raisedHeldWorkItem(
	ctx context.Context, tx *store.WriteTx, runID domain.RunID, now time.Time,
) (domain.AttentionItem, error) {
	run, err := tx.GetRun(ctx, runID)
	if err != nil {
		return domain.AttentionItem{}, fmt.Errorf("run %q: %w", runID, err)
	}
	state, err := tx.ServerState(ctx)
	if err != nil {
		return domain.AttentionItem{}, err
	}
	// The revision this write commits at keeps a second raise for the same
	// run from colliding with the first, as the stall notice's ID does.
	id := domain.ItemID(fmt.Sprintf("%s%s-%d", raisedHeldWorkItemPrefix, runID, state.Revision+1))
	hold := domain.RunHoldObservation{
		RunID: runID, Reason: domain.HoldAdmissionPolicyRefused, FirstObservedAt: now, LastObservedAt: now,
	}
	return engine.HeldWorkNoticeItem(ctx, tx, id, run, hold, now)
}

// raisedReviewGrowthItem builds a review_diminishing_returns item with the
// growth_without_blockers cause, in the shape the store's card gate accepts.
// The real builder needs a live publication task and review record, so this
// one fills the binding the item's Reason carries with the run's own review
// policy and with labelled placeholders where a review would supply the
// finding batch, its adjudication, and the head it reviewed.
//
// Those placeholders mean no stored record proves the binding. The engine
// reads a decision only through the store's authenticated decision record,
// which refuses this card, so a decision made on it closes the card and is
// acted on by nothing. The card is for the notification proof only.
func raisedReviewGrowthItem(
	ctx context.Context, tx *store.WriteTx, runID domain.RunID, now time.Time,
) (domain.AttentionItem, error) {
	run, err := tx.GetRun(ctx, runID)
	if err != nil {
		return domain.AttentionItem{}, fmt.Errorf("run %q: %w", runID, err)
	}
	if err := requireCompletedRun(ctx, tx, runID); err != nil {
		return domain.AttentionItem{}, err
	}
	policy, err := tx.ReviewConvergencePolicy(ctx, runID)
	if err != nil {
		return domain.AttentionItem{}, err
	}
	round, err := raisedReviewRound(ctx, &tx.ReadTx, runID)
	if err != nil {
		return domain.AttentionItem{}, err
	}
	cause := domain.ReviewDiminishingGrowthWithoutBlockers
	itemID := store.ReviewDiminishingItemID(runID, round)
	reason, err := store.ReviewDiminishingReason(store.ReviewDiminishingBinding{
		ItemID: itemID, RunID: runID, Round: round, HeadSHA: raisedMarker,
		FindingIDs:         []domain.FindingID{raisedMarker},
		AdjudicationDigest: raisedDigest("adjudication"),
		FindingBatchDigest: raisedDigest("finding-batch"),
		PolicyDigest:       policy.Digest, ContinueWhile: policy.ContinueWhile,
		LowValueStreakBeforeAttention: policy.LowValueStreakBeforeAttention,
		HardRoundLimit:                policy.HardRoundLimit,
		Cause:                         cause,
	})
	if err != nil {
		return domain.AttentionItem{}, err
	}
	subject := domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID}
	names, err := tx.DisplayNamesFor(ctx, run.ProjectID, subject)
	if err != nil {
		return domain.AttentionItem{}, err
	}
	return domain.NewAttentionItem(domain.AttentionItemInput{
		ID: itemID, ProjectID: run.ProjectID, Subject: subject,
		Type: domain.AttentionReviewDiminishing, Priority: domain.PriorityNormal,
		Reason:            reason,
		RequestedDecision: store.ReviewDiminishingRequestedActions(round, policy.HardRoundLimit),
		PRHeadSHA:         raisedMarker,
		ReviewDiminishing: &domain.ReviewDiminishingFacts{Cause: cause},
		DisplayNames:      names,
		ItemVersion:       1, InterruptionClass: domain.InterruptionPlannedGate,
		CreatedAt: &now, Status: domain.StatusOpen,
	}, nil)
}

// raisedReviewRound returns the first round no review can take and no item
// holds yet. A round with a review record is never used: the item at that
// round's ID is that review's decision record.
//
// The round after the last review is left free too. The engine checks for a
// completion once, when a publication pass starts, so a pass that started
// before the completion was recorded can still write that one round's
// record. Every later pass stops at the check.
func raisedReviewRound(ctx context.Context, tx *store.ReadTx, runID domain.RunID) (int, error) {
	records, err := tx.ListReviewRecords(ctx, runID)
	if err != nil {
		return 0, err
	}
	last := 0
	for _, record := range records {
		last = max(last, record.Round)
	}
	round := last + 2
	for {
		_, err := tx.GetAttentionItem(ctx, store.ReviewDiminishingItemID(runID, round))
		if errors.Is(err, store.ErrNotFound) {
			return round, nil
		}
		if err != nil {
			return 0, err
		}
		round++
	}
}

func raisedDigest(label string) domain.Digest {
	return domain.Digest(contentaddr.Sum([]byte(raisedMarker + "/" + label)))
}
