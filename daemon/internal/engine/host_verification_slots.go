package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/macverify"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

// hostSlotsQuarantinedPrefix names the notice of a verification job whose
// macOS executor has every slot quarantined (productionQuarantineOccurrenceID).
// A busy executor, whose leased slot frees, retries quietly and raises none.
const hostSlotsQuarantinedPrefix = "host-slots-quarantined-"

// hostSlotsQuarantined reports the executor's refusal when err is a slot
// lease refused because every slot is quarantined: no retry gets a slot
// until the daemon restarts, so the operator has to be told.
func hostSlotsQuarantined(err error) (*macverify.NoFreeSlotError, bool) {
	var refused *macverify.NoFreeSlotError
	if !errors.As(err, &refused) || !refused.AllQuarantined() {
		return nil, false
	}
	return refused, true
}

// hostSlotsQuarantinedReason is the notice's operator-facing reason, naming
// the executor, its quarantined slots, and the restart that returns them.
func hostSlotsQuarantinedReason(refused *macverify.NoFreeSlotError) string {
	return fmt.Sprintf("The macOS executor at %s has no slot to verify this task on: "+
		"all %d of its %d slots are quarantined (%s). It quarantines a slot whose processes "+
		"it could not prove gone after a command, or one that failed its pre-job probe, "+
		"and leases no quarantined slot again until freesided restarts. "+
		"Check those slot accounts for leftover processes, then restart freesided; "+
		"the task keeps retrying until then. Or stop the task.",
		refused.ExecutorRoot, len(refused.Quarantined), refused.Slots,
		strings.Join(refused.Quarantined, ", "))
}

// hostSlotsRoom is the host executor's room as verify runs it, recording
// whether the executor answered any command with something other than the
// all-quarantined refusal. Such a command leased a slot, met a pool that
// could free one (in practice, after a restart), or failed a check the room
// runs before leasing, such as its toolchain probe. The room does not say
// which, so the last counts too: the notice then retires, and returns on the
// next pass that meets every slot quarantined.
type hostSlotsRoom struct {
	verify.Room
	served bool
}

func (r *hostSlotsRoom) Run(ctx context.Context, workdir string, argv []string) (verify.StepResult, error) {
	result, err := r.Room.Run(ctx, workdir, argv)
	if _, quarantined := hostSlotsQuarantined(err); !quarantined {
		r.served = true
	}
	return result, err
}

// retireHostSlotsNotice supersedes the run's quarantined-executor notice once
// verify.Verify, run with rooms, had a host command the executor did not
// refuse for quarantined slots (hostSlotsRoom) and did not end on that
// refusal (a later command can still meet it, and retiring then would raise
// a fresh notice on this same pass). It runs at the verification site, not
// the reconcile loop, and only on the host room's record: a pass that ends
// before the executor is asked, whether before verifying or in an earlier
// image-room command, says nothing about the slots, and retiring on it would
// raise a fresh notice on every pass that does reach the executor.
func (w *productionPublicationWorkflow) retireHostSlotsNotice(
	ctx context.Context, task productionPublicationTask,
	rooms map[verify.ExecutionClass]verify.Room, verifyErr error,
) error {
	host, ok := rooms[verify.ClassMacOSHost].(*hostSlotsRoom)
	if !ok || !host.served {
		return nil
	}
	if _, quarantined := hostSlotsQuarantined(verifyErr); quarantined {
		return nil
	}
	return releaseProductionQuarantine(ctx, w.store, w.attention, hostSlotsQuarantinedPrefix, task.RunID)
}
