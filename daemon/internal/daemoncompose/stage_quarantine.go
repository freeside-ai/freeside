package daemoncompose

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec/claude"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const stageIntentItemPrefix = "system-health-stage-intent-"

var stageIntentItemKind = healthItemKind{Prefix: stageIntentItemPrefix, Code: "stage_intent_quarantined"}

// heldClaudeRecovery reports whether a restart-recovery failure holds its
// intent for the engine's later Inspect passes rather than quarantining it:
// an operational retry, or a refusal under mutable admission policy, which
// clears when the policy is restored. The stage driver cannot name the
// engine's policy refusals itself, so startup injects this classifier.
func heldClaudeRecovery(err error) bool {
	return errors.Is(err, claude.ErrRecoveryRetryable) || engine.MutableAdmissionPolicyRefusal(err)
}

// stageIntentCauseKey keys a quarantined intent's item by its file name and
// class, so each intent has its own item, a start that still quarantines it
// the same way keeps that item, and a change of class replaces the item
// rather than leaving the old class's repair text open.
func stageIntentCauseKey(failure claude.IntentFailure) string {
	return contentaddr.Hex(contentaddr.Sum([]byte(failure.Entry + "\x00" + string(failure.Cause))))[:12]
}

// stageIntentCause is the item for one intent restart recovery quarantined
// (#386). It is advisory: the quarantine holds only that invocation, and
// every other intent was still adopted. The reason names the file, the
// invocation when the file decoded that far, and the failure class, never
// the error: the error can carry decoder text or record content, so it goes
// to the daemon log alone. The daemon never repairs the files; the item
// says what the operator can do.
func stageIntentCause(failure claude.IntentFailure) healthCause {
	invocation := "an invocation the file does not name readably"
	if failure.Invocation != "" {
		invocation = "invocation " + string(failure.Invocation)
	}
	return healthCause{
		Reason: fmt.Sprintf(
			"Restart recovery quarantined the stage intent %q in %s (%s): %s The daemon reports the invocation "+
				"as running and never adopts or collects it, so its run stays open and keeps its usage-pool slot; "+
				"recovery does not rewrite or delete the intent, its export, or their records. The daemon log has "+
				"the full error. A task stop is refused until a start adopts the intent. To give the task up, freesided abandon "+
				"closes its workflow without collecting the invocation. %s Then restart; the item resolves at the "+
				"first start that no longer quarantines the intent.",
			failure.Entry, failure.Path, invocation, stageIntentFailureText(failure.Cause),
			stageIntentRepairText(failure.Cause)),
		Posture: domain.HealthPostureAdvisory, Impairs: domain.ImpairedCapabilityNone,
	}
}

func stageIntentFailureText(cause claude.QuarantineCause) string {
	switch cause {
	case claude.QuarantineUnreadable:
		return "the file could not be read or decoded, or is not named for the run it records."
	case claude.QuarantineUnauthenticated:
		return "the record failed the reconstruction check against the database and the ward journal."
	case claude.QuarantineExportConflict:
		return "its released export no longer matches the export record in the database."
	}
	return "recovery could not use it."
}

func stageIntentRepairText(cause claude.QuarantineCause) string {
	switch cause {
	case claude.QuarantineUnreadable:
		return "To resume the invocation, restore the intent file from a copy taken before it changed."
	case claude.QuarantineUnauthenticated:
		return "To resume the invocation, restore the intent file or the record the logged error names " +
			"from a copy taken before it changed."
	case claude.QuarantineExportConflict:
		return "To resume the invocation, restore the export directory the intent names, under ward-exports in " +
			"the state directory, from a copy taken before it changed."
	}
	return ""
}

// convergeStageIntentItems keeps one open item per quarantined intent and
// resolves the item of every intent this start no longer quarantines.
func convergeStageIntentItems(
	ctx context.Context, st *store.Store, quarantined []claude.IntentFailure, now time.Time,
) error {
	causes := make(map[string]healthCause, len(quarantined))
	for _, failure := range quarantined {
		causes[stageIntentCauseKey(failure)] = stageIntentCause(failure)
	}
	return convergeHealthItems(ctx, st, stageIntentItemKind, causes, nil, now)
}
