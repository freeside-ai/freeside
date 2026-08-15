package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
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
	domain.ErrIdentityParallelismExhausted,
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
