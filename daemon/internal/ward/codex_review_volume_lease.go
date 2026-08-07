package ward

import (
	"context"
	"errors"
	"slices"
	"sync"
)

// RuntimeCodexReviewVolumeLeaser serializes every review-volume attachment
// through the same Runtime instance. Start holds the coordinator lock across
// the runtime call, making the final read-only observations and the container
// attachment one exclusive lifecycle window.
type RuntimeCodexReviewVolumeLeaser struct {
	mu        sync.Mutex
	runtime   Runtime
	holders   map[string]string
	transfers map[string]CodexReviewVolumeLeaseTransfer
}

func NewRuntimeCodexReviewVolumeLeaser(rt Runtime) (*RuntimeCodexReviewVolumeLeaser, error) {
	if rt == nil {
		return nil, errors.New("codex review volume leaser: nil runtime")
	}
	return &RuntimeCodexReviewVolumeLeaser{
		runtime: rt, holders: make(map[string]string),
		transfers: make(map[string]CodexReviewVolumeLeaseTransfer),
	}, nil
}

func (l *RuntimeCodexReviewVolumeLeaser) AcquireCodexReviewVolumeLease(
	ctx context.Context, holder string, volumes []string,
) (CodexReviewVolumeLifecycleLease, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.refreshTransfersLocked(ctx); err != nil {
		return nil, err
	}
	if holder == "" || !distinctVolumes(volumes) {
		return nil, ErrCodexReviewVolumeLeaseForeignOwner
	}
	for _, volume := range volumes {
		if current := l.holders[volume]; current != "" {
			return nil, ErrCodexReviewVolumeLeaseForeignOwner
		}
	}
	if attached, err := l.attachedLocked(ctx, volumes); err != nil {
		return nil, err
	} else if attached {
		return nil, ErrCodexReviewVolumeLeaseForeignOwner
	}
	for _, volume := range volumes {
		l.holders[volume] = holder
	}
	return &runtimeCodexReviewVolumeLease{
		coordinator: l, holder: holder, volumes: slices.Clone(volumes),
	}, nil
}

func (l *RuntimeCodexReviewVolumeLeaser) RecoverCodexReviewVolumeLease(
	ctx context.Context, holder string, volumes []string,
) (CodexReviewVolumeLifecycleLease, CodexReviewVolumeLeaseTransfer, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.refreshTransfersLocked(ctx); err != nil {
		return nil, CodexReviewVolumeLeaseTransfer{}, err
	}
	if holder == "" || !distinctVolumes(volumes) {
		return nil, CodexReviewVolumeLeaseTransfer{}, ErrCodexReviewVolumeLeaseForeignOwner
	}
	if transfer, ok := l.transfers[holder]; ok {
		if !slices.Equal(transfer.Volumes, volumes) {
			return nil, CodexReviewVolumeLeaseTransfer{}, ErrCodexReviewVolumeLeaseForeignOwner
		}
	}
	transfer, found, err := l.reconstructTransferLocked(ctx, holder, volumes)
	if err != nil {
		return nil, CodexReviewVolumeLeaseTransfer{}, err
	}
	if found {
		l.transfers[holder] = transfer
		for _, volume := range volumes {
			l.holders[volume] = holder
		}
		return nil, transfer, ErrCodexReviewVolumeLeaseTransferred
	}
	for _, volume := range volumes {
		if current := l.holders[volume]; current != "" && current != holder {
			return nil, CodexReviewVolumeLeaseTransfer{}, ErrCodexReviewVolumeLeaseForeignOwner
		}
	}
	if attached, err := l.attachedLocked(ctx, volumes); err != nil {
		return nil, CodexReviewVolumeLeaseTransfer{}, err
	} else if attached {
		return nil, CodexReviewVolumeLeaseTransfer{}, ErrCodexReviewVolumeLeaseForeignOwner
	}
	for _, volume := range volumes {
		l.holders[volume] = holder
	}
	return &runtimeCodexReviewVolumeLease{
		coordinator: l, holder: holder, volumes: slices.Clone(volumes),
	}, CodexReviewVolumeLeaseTransfer{}, nil
}

func (l *RuntimeCodexReviewVolumeLeaser) reconstructTransferLocked(
	ctx context.Context, holder string, volumes []string,
) (CodexReviewVolumeLeaseTransfer, bool, error) {
	containers, err := l.runtime.ListContainers(ctx)
	if err != nil {
		return CodexReviewVolumeLeaseTransfer{}, false, err
	}
	var transfer CodexReviewVolumeLeaseTransfer
	seen := make(map[string]struct{}, len(containers))
	for _, container := range containers {
		if container.ID == "" || !cliSafe(container.ID) {
			return CodexReviewVolumeLeaseTransfer{}, false, ErrCodexReviewVolumeLeaseForeignOwner
		}
		if _, duplicate := seen[container.ID]; duplicate {
			return CodexReviewVolumeLeaseTransfer{}, false, ErrCodexReviewVolumeLeaseForeignOwner
		}
		seen[container.ID] = struct{}{}
		report, err := l.runtime.Inspect(ctx, container.ID)
		if err != nil {
			return CodexReviewVolumeLeaseTransfer{}, false, err
		}
		if report.ID != container.ID || !report.AllowlistFieldsObserved {
			return CodexReviewVolumeLeaseTransfer{}, false, ErrCodexReviewVolumeLeaseForeignOwner
		}
		// The review container multi-mounts one leased volume (the .agents shadow)
		// at many targets plus the snapshot and workspace once each, so the count
		// of volume mounts is not the count of leased volumes. The invariant is
		// coarser: every leased volume is attached at least once, and no non-leased
		// volume is attached. A container touching none of the leased volumes is
		// unrelated and skipped; one touching some-but-not-all, or any foreign
		// volume, is not the atomic transfer and fails closed.
		attached := make(map[string]struct{}, len(volumes))
		attachesForeignVolume := false
		for _, mount := range report.Mounts {
			if mount.Type != MountVolume {
				continue
			}
			if slices.Contains(volumes, mount.Source) {
				attached[mount.Source] = struct{}{}
			} else {
				attachesForeignVolume = true
			}
		}
		if len(attached) == 0 {
			continue
		}
		if attachesForeignVolume || len(attached) != len(volumes) || !report.LabelsObserved ||
			!slices.Contains(report.Labels, Label{Key: ownershipLabelKey, Value: holder}) ||
			transfer.Container != "" {
			return CodexReviewVolumeLeaseTransfer{}, false, ErrCodexReviewVolumeLeaseForeignOwner
		}
		transfer = CodexReviewVolumeLeaseTransfer{
			Holder: holder, Volumes: slices.Clone(volumes), Container: report.ID,
		}
	}
	return transfer, transfer.Container != "", nil
}

// distinctVolumes reports whether the leased set is well-formed: at least two
// volumes, each nonempty and unique. The lease spans the workspace, the .agents
// shadow, and (on the #591 shape) the credential snapshot; legacy recovery of a
// pre-snapshot intent leases just the workspace and shadow.
func distinctVolumes(volumes []string) bool {
	if len(volumes) < 2 {
		return false
	}
	seen := make(map[string]struct{}, len(volumes))
	for _, volume := range volumes {
		if volume == "" {
			return false
		}
		if _, dup := seen[volume]; dup {
			return false
		}
		seen[volume] = struct{}{}
	}
	return true
}

func (l *RuntimeCodexReviewVolumeLeaser) refreshTransfersLocked(ctx context.Context) error {
	containers, err := l.runtime.ListContainers(ctx)
	if err != nil {
		return err
	}
	for holder, transfer := range l.transfers {
		if slices.ContainsFunc(containers, func(item ContainerSummary) bool {
			return item.ID == transfer.Container
		}) {
			continue
		}
		delete(l.transfers, holder)
		for _, volume := range transfer.Volumes {
			if l.holders[volume] == holder {
				delete(l.holders, volume)
			}
		}
	}
	return nil
}

func (l *RuntimeCodexReviewVolumeLeaser) attachedLocked(
	ctx context.Context, volumes []string,
) (bool, error) {
	containers, err := l.runtime.ListContainers(ctx)
	if err != nil {
		return false, err
	}
	seen := make(map[string]struct{}, len(containers))
	for _, container := range containers {
		if container.ID == "" || !cliSafe(container.ID) {
			return false, ErrCodexReviewVolumeLeaseForeignOwner
		}
		if _, duplicate := seen[container.ID]; duplicate {
			return false, ErrCodexReviewVolumeLeaseForeignOwner
		}
		seen[container.ID] = struct{}{}
		report, err := l.runtime.Inspect(ctx, container.ID)
		if err != nil {
			return false, err
		}
		if report.ID != container.ID || !report.AllowlistFieldsObserved {
			return false, ErrCodexReviewVolumeLeaseForeignOwner
		}
		for _, mount := range report.Mounts {
			if mount.Type == MountVolume && slices.Contains(volumes, mount.Source) {
				return true, nil
			}
		}
	}
	return false, nil
}

type runtimeCodexReviewVolumeLease struct {
	coordinator *RuntimeCodexReviewVolumeLeaser
	holder      string
	volumes     []string
	transferred bool
	released    bool
}

func (l *runtimeCodexReviewVolumeLease) StartCodexReviewContainer(
	ctx context.Context, container string,
) error {
	l.coordinator.mu.Lock()
	defer l.coordinator.mu.Unlock()
	if l.released || l.transferred {
		return ErrCodexReviewVolumeLeaseForeignOwner
	}
	for _, volume := range l.volumes {
		if l.coordinator.holders[volume] != l.holder {
			return ErrCodexReviewVolumeLeaseForeignOwner
		}
	}
	if err := l.coordinator.runtime.StartContainer(ctx, container); err != nil {
		return err
	}
	l.transferred = true
	l.coordinator.transfers[l.holder] = CodexReviewVolumeLeaseTransfer{
		Holder: l.holder, Volumes: slices.Clone(l.volumes), Container: container,
	}
	return nil
}

func (l *runtimeCodexReviewVolumeLease) ReleaseCodexReviewVolumeLease(context.Context) error {
	l.coordinator.mu.Lock()
	defer l.coordinator.mu.Unlock()
	if l.transferred {
		return ErrCodexReviewVolumeLeaseTransferred
	}
	if l.released {
		return nil
	}
	for _, volume := range l.volumes {
		if l.coordinator.holders[volume] == l.holder {
			delete(l.coordinator.holders, volume)
		}
	}
	l.released = true
	return nil
}
