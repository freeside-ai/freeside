package ward

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
)

// ReleaseAbsentRuntimeResources releases the names in resources that the
// runtime shows no object under, and keeps the rest bound. It is the
// absence half of RuntimeResourceReleaser's contract: teardown can succeed
// while a same-name object it judged foreign remains, and such a name must
// stay enumerable. A listing failure releases nothing. A nil release is a
// no-op, so callers without a rig skip the runtime listings too.
func ReleaseAbsentRuntimeResources(
	ctx context.Context, rt Runtime, release RuntimeResourceReleaser, resources RuntimeResourceNames,
) error {
	if release == nil {
		return nil
	}
	absent, err := absentRuntimeResources(ctx, rt, resources)
	if err != nil {
		return err
	}
	if len(absent.Containers)+len(absent.Volumes)+len(absent.Networks) == 0 {
		return nil
	}
	return release(ctx, absent)
}

func absentRuntimeResources(
	ctx context.Context, rt Runtime, resources RuntimeResourceNames,
) (RuntimeResourceNames, error) {
	var absent RuntimeResourceNames
	if len(resources.Containers) > 0 {
		containers, err := rt.ListContainers(ctx)
		if err != nil {
			return RuntimeResourceNames{}, fmt.Errorf("list containers before release: %w", err)
		}
		absent.Containers = namesWithout(resources.Containers, func(name string) bool {
			return slices.ContainsFunc(containers, func(c ContainerSummary) bool { return c.ID == name })
		})
	}
	if len(resources.Volumes) > 0 {
		volumes, err := rt.ListVolumes(ctx)
		if err != nil {
			return RuntimeResourceNames{}, fmt.Errorf("list volumes before release: %w", err)
		}
		absent.Volumes = namesWithout(resources.Volumes, func(name string) bool {
			return slices.ContainsFunc(volumes, func(v VolumeSummary) bool { return v.Name == name })
		})
	}
	if len(resources.Networks) > 0 {
		networks, err := rt.ListNetworks(ctx)
		if err != nil {
			return RuntimeResourceNames{}, fmt.Errorf("list networks before release: %w", err)
		}
		absent.Networks = namesWithout(resources.Networks, func(name string) bool {
			return slices.ContainsFunc(networks, func(n NetworkSummary) bool { return n.Name == name })
		})
	}
	return absent, nil
}

// concatRuntimeResourceNames joins name sets; a repeated name is harmless to
// both the absence check and the unbind.
func concatRuntimeResourceNames(sets ...RuntimeResourceNames) RuntimeResourceNames {
	var joined RuntimeResourceNames
	for _, set := range sets {
		joined.Containers = append(joined.Containers, set.Containers...)
		joined.Volumes = append(joined.Volumes, set.Volumes...)
		joined.Networks = append(joined.Networks, set.Networks...)
	}
	return joined
}

func namesWithout(names []string, present func(string) bool) []string {
	var absent []string
	for _, name := range names {
		if !present(name) {
			absent = append(absent, name)
		}
	}
	return absent
}

// releaseHandoffNamespace releases runID's handoff namespace once its journal
// record is closed. Production binds that namespace before the run starts
// (the pre-job hook). For a run that never bound it, such as a conformance
// suite run, the release changes nothing.
func (b *Backend) releaseHandoffNamespace(ctx context.Context, runID string) {
	releaseAfterClose(ctx, b.rt, b.cfg.ReleaseRuntimeResources, b.cfg.TeardownTimeout,
		b.cfg.Logger, runID, RuntimeResourceNamesFor(runID))
}

// releaseAfterClose runs ReleaseAbsentRuntimeResources on its own bounded,
// uncancelled context, since it follows a teardown that may have outlived
// its caller's context. A failure is logged, never returned: the names stay
// bound, which is the safe direction, and the run's outcome stands.
func releaseAfterClose(
	ctx context.Context, rt Runtime, release RuntimeResourceReleaser, timeout time.Duration,
	logger *slog.Logger, runID string, resources RuntimeResourceNames,
) {
	if release == nil {
		return
	}
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	if err := ReleaseAbsentRuntimeResources(releaseCtx, rt, release, resources); err != nil && logger != nil {
		logger.Warn("rig runtime names stay bound after a failed release", "run", runID, "error", err)
	}
}

// ReleaseFinishedHandoffs releases the handoff namespace of each run in runIDs
// whose journal record is absent or closed, and keeps every other run's names
// bound. It is the release for runs whose own close never released: a stage
// that refused before ward opened a record, a crash between close and
// release, or a release that failed.
//
// The caller's precondition makes absence safe: no later Handoff or Recover
// for a named run creates an object under its names without binding them
// again. The stage driver meets it either by holding a terminal intent for
// the run, which it never resumes, or, when no intent exists, by releasing
// under the lock its next start holds while that start binds the names
// before handing off. An absent record alone proves nothing, since a stage
// may still rerun a refused handoff under the same run ID. The gate this
// re-checks is the journal record: an open record, an invalid one, or a
// failed read keeps the run's names. The runtime absence check then keeps any
// name an object still holds.
//
// Like releaseAfterClose it runs on its own bounded, uncancelled context, and
// a failure is logged, never returned. It reads nothing without a releaser.
func (b *Backend) ReleaseFinishedHandoffs(ctx context.Context, runIDs []string) {
	if b == nil || !b.initialized || b.cfg.ReleaseRuntimeResources == nil || b.cfg.Journal == nil ||
		len(runIDs) == 0 {
		return
	}
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.cfg.TeardownTimeout)
	defer cancel()
	var finished []RuntimeResourceNames
	for _, runID := range runIDs {
		closed, err := b.handoffRecordFinished(releaseCtx, runID)
		if err != nil {
			b.warnNamesStayBound(runID, err)
			continue
		}
		if closed {
			finished = append(finished, RuntimeResourceNamesFor(runID))
		}
	}
	if len(finished) == 0 {
		return
	}
	if err := ReleaseAbsentRuntimeResources(releaseCtx, b.rt, b.cfg.ReleaseRuntimeResources,
		concatRuntimeResourceNames(finished...)); err != nil {
		b.warnNamesStayBound(strings.Join(runIDs, ","), err)
	}
}

// handoffRecordFinished reports whether runID's journal record is absent or
// closed. An open record answers false; a read or validation failure is an
// error, which the caller treats as open.
func (b *Backend) handoffRecordFinished(ctx context.Context, runID string) (bool, error) {
	rec, err := b.cfg.Journal.Get(ctx, runID)
	switch {
	case errors.Is(err, ErrJournalRecordNotFound):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("read handoff journal: %w", err)
	}
	if err := rec.Validate(); err != nil {
		return false, err
	}
	if rec.RunID != runID {
		return false, fmt.Errorf("%w: journal returned record for run %q", ErrInvalidJournalRecord, rec.RunID)
	}
	return rec.Outcome != nil, nil
}

func (b *Backend) warnNamesStayBound(runID string, err error) {
	if b.cfg.Logger != nil {
		b.cfg.Logger.Warn("rig runtime names stay bound after a failed release", "run", runID, "error", err)
	}
}
