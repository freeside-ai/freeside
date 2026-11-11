package ward

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
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
