package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	specifyfake "github.com/freeside-ai/freeside/daemon/internal/specify/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// holdClassifiedSentinels is every error the dispatch path classifies as a
// quiet hold or refusal (the unattendedDispatchRefusal,
// invocationDispatchHold, and operating-gate sets). The observation contract
// requires each to carry a typed reason: a sentinel held quietly but mapped
// to no code would leave a run held with nothing for an operator to see.
var holdClassifiedSentinels = []error{
	domain.ErrUnattendedOperationStopped,
	domain.ErrBlockingSystemHealth,
	domain.ErrUsagePoolExhausted,
	exec.ErrInputUnavailable,
	exec.ErrCapabilityRefused,
	exec.ErrPreJobRefused,
	store.ErrBackendNotConformant,
	domain.ErrConformanceConfigurationUnbound,
	domain.ErrAdmissionConfigurationMismatch,
	domain.ErrAdmissionExceedsConformance,
	domain.ErrUnknownAdmissionFloor,
	domain.ErrCapabilityBelowFloor,
	domain.ErrCredentialModeNotApproved,
	domain.ErrWaiverNotConfigured,
	domain.ErrBackupHealthUnavailable,
	domain.ErrCheckpointNotEncrypted,
	domain.ErrCheckpointNotCurrent,
	domain.ErrArtifactClosureIncomplete,
	domain.ErrRestoreTestStale,
	domain.ErrInvalidBackupHealthStatus,
	store.ErrRepositoryUntrusted,
	publish.ErrJanitorInactive,
	domain.ErrRepositoryIdentityMismatch,
	domain.ErrPathBoundaryMismatch,
	domain.ErrTrustProfileSuperseded,
	domain.ErrReviewConfigurationUnapproved,
	ErrEgressProfileNotEnforceable,
	ErrEgressPolicyRefused,
	ErrImageUpkeepPending,
	ErrProjectImageUnserved,
}

// TestDispatchHoldReasonCoversEveryHoldClass: every sentinel the dispatch
// predicates hold quietly classifies onto the closed reason vocabulary, and
// every produced code is a registered member. An unclassified error maps to
// nothing, which is the loud-failure path, not a hold.
func TestDispatchHoldReasonCoversEveryHoldClass(t *testing.T) {
	for _, sentinel := range holdClassifiedSentinels {
		wrapped := fmt.Errorf("intent %q: dispatch: %w", "inv-1", sentinel)
		reason, ok := dispatchHoldReason(wrapped)
		if !ok {
			t.Errorf("sentinel %v classifies as a hold but maps to no reason code", sentinel)
			continue
		}
		registered := false
		for _, member := range domain.AllRunHoldReasons {
			if member == reason {
				registered = true
				break
			}
		}
		if !registered {
			t.Errorf("sentinel %v maps to unregistered reason %q", sentinel, reason)
		}
		// The mapped classes must agree with the dispatch predicates: a
		// hold-classified error is exactly one the loop would keep quiet.
		if !unattendedDispatchRefusal(wrapped) && !invocationDispatchHold(wrapped) &&
			reason != domain.HoldOperationStopped && reason != domain.HoldBlockingSystemHealth {
			t.Errorf("sentinel %v maps to %q but no dispatch predicate holds it", sentinel, reason)
		}
	}
	if _, ok := dispatchHoldReason(fmt.Errorf("an unclassified failure")); ok {
		t.Error("an unclassified error produced a hold reason")
	}
	if _, ok := dispatchHoldReason(nil); ok {
		t.Error("nil produced a hold reason")
	}
}

type admissionCheckCase struct {
	sentinel error
	check    domain.AdmissionCheck
}

// admissionRefusalChecks is the check each error class dispatchHoldReason
// maps onto admission_policy_refused names when it carries no typed agent
// refusal (issue #2211).
var admissionRefusalChecks = []admissionCheckCase{
	{ErrAgentNotAdmissible, domain.AdmissionCheckAgentAdmission},
	{domain.ErrGenerationIntegrityMarked, domain.AdmissionCheckGenerationIntegrityMarked},
	{ErrEgressProfileNotEnforceable, domain.AdmissionCheckEgressProfileNotEnforceable},
	{ErrEgressPolicyRefused, domain.AdmissionCheckEgressPolicyRefused},
	{exec.ErrCapabilityRefused, domain.AdmissionCheckBackendCapabilityRefused},
	{exec.ErrPreJobRefused, domain.AdmissionCheckBackendPreJobRefused},
	{domain.ErrUnknownAdmissionFloor, domain.AdmissionCheckAdmissionFloorUnknown},
	{domain.ErrCapabilityBelowFloor, domain.AdmissionCheckCapabilityBelowFloor},
	{domain.ErrCredentialModeNotApproved, domain.AdmissionCheckCredentialModeNotApproved},
	{domain.ErrWaiverNotConfigured, domain.AdmissionCheckWaiverNotConfigured},
	{domain.ErrRepositoryIdentityMismatch, domain.AdmissionCheckRepositoryIdentityMismatch},
	{domain.ErrPathBoundaryMismatch, domain.AdmissionCheckPathBoundaryMismatch},
	{domain.ErrTrustProfileSuperseded, domain.AdmissionCheckTrustProfileSuperseded},
	{domain.ErrReviewConfigurationUnapproved, domain.AdmissionCheckReviewConfigurationUnapproved},
}

// TestAdmissionRefusalDetailCoversEveryAdmissionClass: every error class the
// hold records as admission_policy_refused names its own check and no agent
// or launch, every hold-classified sentinel of that reason is in the table,
// and an error of any other reason has no detail.
func TestAdmissionRefusalDetailCoversEveryAdmissionClass(t *testing.T) {
	seen := map[domain.AdmissionCheck]bool{}
	for _, tc := range admissionRefusalChecks {
		wrapped := fmt.Errorf("intent %q: dispatch: %w", "inv-1", tc.sentinel)
		if reason, ok := dispatchHoldReason(wrapped); !ok || reason != domain.HoldAdmissionPolicyRefused {
			t.Errorf("%v: hold reason = %q, %t; want %q", tc.sentinel, reason, ok, domain.HoldAdmissionPolicyRefused)
			continue
		}
		got := admissionRefusalDetail(wrapped)
		want := domain.AdmissionRefusal{Check: tc.check}
		if got == nil || *got != want {
			t.Errorf("%v: detail = %+v, want %+v", tc.sentinel, got, want)
		}
		if seen[tc.check] {
			t.Errorf("%v: check %q names two error classes", tc.sentinel, tc.check)
		}
		seen[tc.check] = true
	}
	for _, sentinel := range holdClassifiedSentinels {
		reason, _ := dispatchHoldReason(sentinel)
		detail := admissionRefusalDetail(sentinel)
		// An unserved base has no check in the vocabulary, so its hold
		// carries none rather than a check that would name another cause.
		if reason != domain.HoldAdmissionPolicyRefused || errors.Is(sentinel, ErrProjectImageUnserved) {
			if detail != nil {
				t.Errorf("%v: reason %q carries detail %+v", sentinel, reason, detail)
			}
			continue
		}
		if !slices.ContainsFunc(admissionRefusalChecks, func(tc admissionCheckCase) bool {
			return errors.Is(sentinel, tc.sentinel)
		}) {
			t.Errorf("%v maps to %q but has no expected check", sentinel, reason)
		}
	}
	// A typed agent refusal names its own check, agent, and launch through
	// any wrapping; one joined with a cause of another reason has no detail,
	// because the hold records that other reason.
	agent, stage, launch := "implementer-agent", domain.StageNameSpecification, domain.Digest("sha256:launch")
	typed := &AgentAdmissionRefusal{
		Refusal: domain.AdmissionRefusal{
			Check: domain.AdmissionCheckAgentPrompt, Agent: &agent,
			LaunchStage: &stage, LaunchDigest: &launch,
		},
		err: fmt.Errorf("role specifier: the lineup selects prompt: %w", ErrAgentNotAdmissible),
	}
	if got := admissionRefusalDetail(fmt.Errorf("admit invocation %q: %w", "inv-1", typed)); got == nil ||
		!reflect.DeepEqual(*got, typed.Refusal) {
		t.Errorf("typed refusal detail = %+v, want %+v", got, typed.Refusal)
	}
	gated := &AgentAdmissionRefusal{
		Refusal: domain.AdmissionRefusal{Check: domain.AdmissionCheckAgentGate},
		err:     errors.Join(ErrAgentNotAdmissible, domain.ErrBlockingSystemHealth),
	}
	if reason, _ := dispatchHoldReason(gated); reason != domain.HoldBlockingSystemHealth {
		t.Fatalf("gate refusal on system health: reason = %q", reason)
	}
	if got := admissionRefusalDetail(gated); got != nil {
		t.Errorf("a blocking_system_health hold carries detail %+v", got)
	}
	if got := admissionRefusalDetail(errors.New("an unclassified failure")); got != nil {
		t.Errorf("an unclassified error carries detail %+v", got)
	}
}

// An image-upkeep hold carries what it waits on and nothing else, through
// any wrapping, and a changed kind or reference is a changed pace state, so
// it is written at once.
func TestImageUpkeepHoldDetail(t *testing.T) {
	refusal := domain.Digest("sha256:" + strings.Repeat("1", 64))
	other := domain.Digest("sha256:" + strings.Repeat("2", 64))
	item, task := domain.ItemID("card-1"), domain.TaskID("task-1")
	holds := []domain.ImageUpkeepHold{
		{Pending: domain.ImageUpkeepPendingRebuild},
		{Pending: domain.ImageUpkeepPendingAdoptionProposal, RefusalRecordID: &refusal, ProposalItemID: &item},
		{Pending: domain.ImageUpkeepPendingAdoptionProposal, RefusalRecordID: &other, ProposalItemID: &item},
		{Pending: domain.ImageUpkeepPendingMaintenanceTask, RefusalRecordID: &refusal, MaintenanceTaskID: &task},
	}
	states := map[string]bool{}
	for _, hold := range holds {
		err := fmt.Errorf("admit invocation %q: select image: %w", "inv-1", &ImageUpkeepPendingError{Hold: hold})
		if reason, ok := dispatchHoldReason(err); !ok || reason != domain.HoldImageUpkeepPending {
			t.Fatalf("%+v: reason = %q, %t", hold, reason, ok)
		}
		if !invocationDispatchHold(err) {
			t.Fatalf("%+v: the hold does not hold its own invocation", hold)
		}
		detail := holdDetailFor(err)
		if detail.refusal != nil || detail.imageUpkeep == nil || !reflect.DeepEqual(*detail.imageUpkeep, hold) {
			t.Fatalf("%+v: detail = %+v", hold, detail)
		}
		state := holdPaceState(domain.HoldImageUpkeepPending, detail)
		if states[state] {
			t.Fatalf("%+v: pace state %q repeats another hold's", hold, state)
		}
		states[state] = true
	}
	notProved := errors.Join(ErrImageNotProved, domain.ErrAdmissionConfigurationMismatch)
	if reason, _ := dispatchHoldReason(notProved); reason != domain.HoldBackendNotConformant {
		t.Fatalf("an unproved image holds under %q, want backend_not_conformant", reason)
	}
}

// TestUsagePoolRefusalHoldsNamingItsPool: the store's refusal holds its own
// invocation under the pool hold, and a different pool is a different hold, so
// the pacer does not hold back the new name.
func TestUsagePoolRefusalHoldsNamingItsPool(t *testing.T) {
	refusal := func(pool domain.UsagePoolID) error {
		return fmt.Errorf("admit invocation %q: %w", "inv-1",
			&domain.UsagePoolExhaustedError{Pool: pool, Active: 2, Limit: 2})
	}
	err := refusal("pool-a")
	if reason, ok := dispatchHoldReason(err); !ok || reason != domain.HoldUsagePoolExhausted {
		t.Fatalf("reason = %q, %t", reason, ok)
	}
	if !invocationDispatchHold(err) {
		t.Fatal("the refusal does not hold its own invocation")
	}
	detail := holdDetailFor(err)
	if detail.usagePool == nil || *detail.usagePool != "pool-a" {
		t.Fatalf("detail = %+v, want pool-a", detail)
	}
	if holdPaceState(domain.HoldUsagePoolExhausted, detail) ==
		holdPaceState(domain.HoldUsagePoolExhausted, holdDetailFor(refusal("pool-b"))) {
		t.Fatal("two pools share one pace state")
	}
}

// TestDispatchResetsThePaceOfTheHoldItClears is the regression for #727. A
// dispatch clears the run's pool hold; the same hold returning inside the
// refresh interval is a new wait and must be recorded, or the run shows pending
// work with no explanation.
func TestDispatchResetsThePaceOfTheHoldItClears(t *testing.T) {
	ctx := t.Context()
	f := newSpecificationFixture(t, false, 4)
	driver := f.newDriver(t)
	runID := domain.RunID("specification-run")
	id := specificationInvocationID(runID, 1)
	if err := specifyfake.Script(driver, id, 0, 0, specify.Output{Specification: &specify.Specification{
		Summary: "The implementation plan is ready.", Body: "# Specification\n\nImplement.",
		Addressals: []specify.Addressal{},
	}}); err != nil {
		t.Fatal(err)
	}
	f.submit(t)
	engine := f.newEngine(t, driver)
	pool := domain.UsagePoolID("pool-a")
	hold := func() *domain.RunHoldObservation {
		t.Helper()
		if err := engine.observeRunHold(ctx, runID, id, domain.HoldUsagePoolExhausted,
			holdDetail{usagePool: &pool}); err != nil {
			t.Fatal(err)
		}
		return storedRunHold(t, f.store, runID)
	}
	if got := hold(); got == nil || got.UsagePool == nil || *got.UsagePool != pool {
		t.Fatalf("first hold = %+v", got)
	}
	if _, err := engine.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got := storedRunHold(t, f.store, runID); got != nil {
		t.Fatalf("dispatch left the hold in place: %+v", got)
	}
	if got := hold(); got == nil || got.Reason != domain.HoldUsagePoolExhausted {
		t.Fatalf("hold returning inside the refresh interval = %+v, want it recorded", got)
	}
}

func storedRunHold(t *testing.T, st *store.Store, runID domain.RunID) *domain.RunHoldObservation {
	t.Helper()
	var hold *domain.RunHoldObservation
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		stored, present, err := tx.GetRunHold(t.Context(), runID)
		if present {
			hold = &stored
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return hold
}

// TestProductionBlockReasonIsTotalOverDefinitiveReasons: the definitive
// block strings are a closed set and each maps to its code; anything else
// maps to nothing, matching recovery's fail-closed reason gate.
func TestProductionBlockReasonIsTotalOverDefinitiveReasons(t *testing.T) {
	want := map[string]domain.RunHoldReason{
		productionBlockRecipeRevoked: domain.HoldRecipeRevoked,
		productionBlockVerification:  domain.HoldVerificationFindings,
		productionBlockTrust:         domain.HoldTrustBlocked,
		productionBlockBaseAdvanced:  domain.HoldBaseAdvanced,
		productionBlockExternal:      domain.HoldExternalConflict,
	}
	for reason, code := range want {
		got, ok := productionBlockReason(reason)
		if !ok || got != code {
			t.Errorf("productionBlockReason(%q) = %q, %v; want %q", reason, got, ok, code)
		}
	}
	if _, ok := productionBlockReason("An operator-authored explanation."); ok {
		t.Error("an unknown reason string produced a code")
	}
}

// TestObservedStatusCoversDriverVocabulary: the observation mirror is total
// over the driver status vocabulary and rejects the zero value.
func TestObservedStatusCoversDriverVocabulary(t *testing.T) {
	for _, status := range exec.AllStatuses {
		observed, ok := observedStatus(status)
		if !ok {
			t.Errorf("driver status %q has no observed form", status)
			continue
		}
		if string(observed) != string(status) {
			t.Errorf("observed form of %q is %q; the vocabularies are meant to mirror", status, observed)
		}
	}
	if _, ok := observedStatus(""); ok {
		t.Error("zero driver status produced an observed form")
	}
}

// TestObservationPaceClockRollbackIsDue: a clock stepped back past the
// stamp makes the next observation due immediately, so a future-dated
// persisted observation (which readers derive as a gap) is replaced instead
// of standing until wall time catches up.
func TestObservationPaceClockRollbackIsDue(t *testing.T) {
	var p observationPace
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if !p.due("k", "s", ts) {
		t.Fatal("first observation must be due")
	}
	if p.due("k", "s", ts.Add(time.Second)) {
		t.Error("unchanged state inside the interval must be paced")
	}
	if !p.due("k", "s", ts.Add(-time.Minute)) {
		t.Error("a rolled-back clock must be due immediately")
	}
	if p.due("k", "s", ts.Add(-time.Minute+time.Second)) {
		t.Error("the rollback write must restamp pacing at the new instant")
	}
}

// TestObservationPaceReportsChangeSeparatelyFromRefresh pins the change bit the
// hold log keys off (issue #1181): a first observation and a real state change
// report changed, but the periodic freshness refresh of an unchanged state does
// not, so a long-lived hold logs once per reason change rather than once every
// observationRefreshInterval.
func TestObservationPaceReportsChangeSeparatelyFromRefresh(t *testing.T) {
	var p observationPace
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if due, changed := p.dueChanged("k", "a", ts); !due || !changed {
		t.Fatalf("first observation = (due %t, changed %t), want (true, true)", due, changed)
	}
	if due, changed := p.dueChanged("k", "a", ts.Add(time.Second)); due || changed {
		t.Fatalf("unchanged inside the interval = (due %t, changed %t), want (false, false)", due, changed)
	}
	if due, changed := p.dueChanged("k", "a", ts.Add(observationRefreshInterval)); !due || changed {
		t.Fatalf("stale unchanged refresh = (due %t, changed %t), want (true, false)", due, changed)
	}
	if due, changed := p.dueChanged("k", "b", ts.Add(observationRefreshInterval+time.Second)); !due || !changed {
		t.Fatalf("changed state = (due %t, changed %t), want (true, true)", due, changed)
	}
}

// TestHoldObservationsPassTheLeakAxisEnumeration is the adversarial
// enumeration the issue requires, run once as tests: for every leak axis
// (credentials, provider output, specifications, policies, workspace paths,
// environment values) seeded into every hold-classified error, nothing the
// observation surface serializes contains the seeded payload. The reason
// vocabulary is a closed enum with no free-text carrier, so the leak is
// unrepresentable; this test pins that against a future detail field.
func TestHoldObservationsPassTheLeakAxisEnumeration(t *testing.T) {
	axes := map[string]string{ //nolint:gosec // G101: deliberately fake leak payloads, not credentials
		"credential":      "ghs_SECRETINSTALLATIONTOKEN2026",
		"provider_output": "PROVIDER-STDERR: panic at /agent/workspace/main.go",
		"specification":   "SPEC-CONTENT: implement the hidden billing bypass",
		"policy":          "POLICY-CONTENT: allow paths daemon/** except secret/**",
		"workspace_path":  "/Users/operator/.freeside/workspaces/run-42",
		"environment":     "ANTHROPIC_API_KEY=sk-ant-SECRETVALUE",
	}
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	inv := domain.InvocationID("inv-1")

	for axis, payload := range axes {
		for _, sentinel := range holdClassifiedSentinels {
			seeded := fmt.Errorf("dispatch %s: %w", payload, sentinel)
			reason, ok := dispatchHoldReason(seeded)
			if !ok {
				continue // covered by the coverage test above
			}
			observed := []any{
				domain.RunHoldObservation{
					RunID: "run-1", InvocationID: &inv, Reason: reason,
					FirstObservedAt: ts, LastObservedAt: ts,
				},
				domain.RunMilestone{
					RunID: "run-1", Kind: domain.MilestonePublicationBlocked,
					InvocationID: &inv, Reason: &reason, RecordedAt: ts,
				},
			}
			for _, value := range observed {
				encoded, err := json.Marshal(value)
				if err != nil {
					t.Fatalf("marshal %T: %v", value, err)
				}
				if strings.Contains(string(encoded), payload) {
					t.Errorf("axis %s leaked through %T for sentinel %v: %s",
						axis, value, sentinel, encoded)
				}
			}
		}
	}
}
