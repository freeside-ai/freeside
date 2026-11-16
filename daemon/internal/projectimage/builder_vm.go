package projectimage

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// builderContainerID is the container Apple container runs its image builder
// in: one per host user, which `container build` uses and the `container
// builder` verbs manage.
const builderContainerID = "buildkit"

// BuilderSize is the size a build starts the builder VM at, the size the
// image's proofs run at (proofSize). Without it `container build` starts the
// builder at the runtime's own default, which no reservation names. A
// reservation for a build covers it.
var BuilderSize = proofSize

// RegistrySize is the size a build starts a loopback registry at: Apple
// container's default memory, declared so the registry's standing
// reservation names what it holds.
var RegistrySize = ward.ContainerSize{CPUs: 4, MemoryMiB: 1024}

// ErrContainerAbsenceUnproven marks a build whose cleanup could not prove a
// container it started gone: the builder VM still running after its stop, or
// a probe whose deletion failed. The memory such a container holds may still
// be in use, so the build's reservation stays held until a later proof.
var ErrContainerAbsenceUnproven = errors.New("project-image container absence is unproven")

// RegistryMemory reserves memory for a loopback registry a build starts,
// which outlives the build that started it (plan §5.4). Reserve runs before
// the build boots the registry on port, a new one or a stopped one it
// restarts, and a refusal fails the build before the registry starts.
// Release runs once a build proves no registry for port exists, after it
// found none or deleted the one it started. A builder with none reserves
// nothing, as for an operator's build.
type RegistryMemory interface {
	Reserve(ctx context.Context, port int) error
	Release(ctx context.Context, port int) error
}

// startBuilder starts the builder VM at BuilderSize unless it runs already.
// A running builder is used as it stands: another instance or a hand-run
// build may be using it.
func (a appleBackend) startBuilder(ctx context.Context) error {
	report, exists, err := a.inspectContainer(ctx, builderContainerID)
	if err != nil {
		return fmt.Errorf("inspect image builder: %w", err)
	}
	if exists && report.Status.State == "running" {
		return nil
	}
	args := append([]string{"builder", "start"}, ward.SizeArgs(BuilderSize)...)
	output, err := a.runner.Run(ctx, commandSpec{Path: a.containerPath, Args: args})
	if err != nil {
		// A start that failed partway may have booted the VM.
		return errors.Join(runError("start image builder", output, err), a.stopBuilder(context.WithoutCancel(ctx)))
	}
	return nil
}

// stopBuilder stops the builder VM and proves it stopped, so a build's VM is
// gone before its probes boot theirs and the build runs one VM at a time.
// A builder already stopped or never created is proved stopped as it is.
func (a appleBackend) stopBuilder(ctx context.Context) error {
	report, exists, err := a.inspectContainer(ctx, builderContainerID)
	if err == nil && exists && report.Status.State != "stopped" {
		output, stopErr := a.runner.Run(ctx, commandSpec{
			Path: a.containerPath, Args: []string{"builder", "stop"},
		})
		if stopErr != nil {
			err = runError("stop image builder", output, stopErr)
		} else {
			report, exists, err = a.inspectContainer(ctx, builderContainerID)
		}
	}
	if err == nil && exists && report.Status.State != "stopped" {
		err = fmt.Errorf("image builder is %q after its stop", report.Status.State)
	}
	if err != nil {
		return errors.Join(ErrContainerAbsenceUnproven, fmt.Errorf("prove the image builder stopped: %w", err))
	}
	return nil
}

// RecoverBuild proves gone what a build under reservation left when its
// process ended: it stops the builder VM and proves it stopped, then deletes
// every container carrying the reservation's label (ward.ReservationLabel)
// and proves none remains. The label holds the reservation's random id, so
// only that build's containers match. An error wraps
// ErrContainerAbsenceUnproven.
func RecoverBuild(ctx context.Context, containerPath string, reservation domain.InvocationID) error {
	path, err := resolveExecutable(containerPath, "container")
	if err != nil {
		return errors.Join(ErrContainerAbsenceUnproven, err)
	}
	return (appleBackend{containerPath: path, runner: execRunner{}}).recoverBuild(ctx, reservation)
}

func (a appleBackend) recoverBuild(ctx context.Context, reservation domain.InvocationID) error {
	if err := a.stopBuilder(ctx); err != nil {
		return err
	}
	labeled, err := a.labeledContainers(ctx, ward.ReservationLabel, string(reservation))
	if err == nil {
		for _, id := range labeled {
			output, deleteErr := a.runner.Run(ctx, commandSpec{
				Path: a.containerPath, Args: []string{"delete", "--force", id},
			})
			if deleteErr != nil && !containerDeletionAbsent(output, id) {
				err = errors.Join(err, runError("delete container "+id+" left by build "+string(reservation), output, deleteErr))
			}
		}
	}
	if err == nil && len(labeled) > 0 {
		var left []string
		if left, err = a.labeledContainers(ctx, ward.ReservationLabel, string(reservation)); err == nil && len(left) > 0 {
			err = fmt.Errorf("containers %s left by build %s remain after their deletion",
				strings.Join(left, ", "), reservation)
		}
	}
	if err != nil {
		return errors.Join(ErrContainerAbsenceUnproven, err)
	}
	return nil
}

// RegistryHoldsNoMemory reports whether no loopback registry for port runs:
// none exists, or the one that exists is stopped, as after a host restart.
// A stopped registry's VM is gone, and a build reserves again before it
// restarts one, so either proves a registry reservation free to release.
func RegistryHoldsNoMemory(ctx context.Context, containerPath string, port int) (bool, error) {
	path, err := resolveExecutable(containerPath, "container")
	if err != nil {
		return false, err
	}
	reports, err := (appleBackend{containerPath: path, runner: execRunner{}}).
		labeledReports(ctx, registryPortLabel, strconv.Itoa(port))
	if err != nil {
		return false, err
	}
	for _, report := range reports {
		if report.Status.State != "stopped" {
			return false, nil
		}
	}
	return true, nil
}

// labeledContainers lists every container, running or stopped, whose label
// key holds value. Apple container's list output omits labels, so each is
// read from its inspection, as findRetainedRegistry does.
func (a appleBackend) labeledContainers(ctx context.Context, key, value string) ([]string, error) {
	reports, err := a.labeledReports(ctx, key, value)
	ids := make([]string, 0, len(reports))
	for _, report := range reports {
		ids = append(ids, report.ID)
	}
	return ids, err
}

func (a appleBackend) labeledReports(ctx context.Context, key, value string) ([]containerInspect, error) {
	output, runErr := a.runner.Run(ctx, commandSpec{
		Path: a.containerPath, Args: []string{"list", "--all", "--format", "json"},
	})
	if runErr != nil {
		return nil, runError("list containers", output, runErr)
	}
	var reports []containerInspect
	if err := decodeStrictJSON(output.bytes, &reports); err != nil {
		return nil, fmt.Errorf("decode container list: %w", err)
	}
	var labeled []containerInspect
	for _, listed := range reports {
		if !validContainerID(listed.ID) || listed.Configuration.ID != listed.ID {
			return nil, fmt.Errorf("container list returned invalid container identity %q", listed.ID)
		}
		report, exists, err := a.inspectContainer(ctx, listed.ID)
		if err != nil {
			return nil, err
		}
		if exists && report.Configuration.Labels[key] == value {
			labeled = append(labeled, report)
		}
	}
	return labeled, nil
}

// absenceUnproven marks a probe's failed cleanup, so the build's reservation
// stays held.
func absenceUnproven(err error) error {
	if err == nil {
		return nil
	}
	return errors.Join(ErrContainerAbsenceUnproven, err)
}

// RegistryReservationID is the reservation a loopback registry on port runs
// under. It is fixed by the port, because the registry outlives the build
// that started it and the next build or a start must find its reservation.
func RegistryReservationID(port int) domain.InvocationID {
	return domain.InvocationID("image-registry-" + strconv.Itoa(port))
}

// registryContext is ctx carrying the registry's own reservation in place of
// the build's, for the launch guard at the registry's start.
func registryContext(ctx context.Context, port int) context.Context {
	return ward.WithLaunchReservation(ctx, RegistryReservationID(port))
}

func (a appleBackend) reserveRegistry(ctx context.Context, port int) error {
	if a.registryMemory == nil {
		return nil
	}
	if err := a.registryMemory.Reserve(ctx, port); err != nil {
		return fmt.Errorf("reserve memory for the loopback registry on port %d: %w", port, err)
	}
	return nil
}

func (a appleBackend) releaseRegistry(ctx context.Context, port int) error {
	if a.registryMemory == nil {
		return nil
	}
	if err := a.registryMemory.Release(ctx, port); err != nil {
		return fmt.Errorf("release memory for the loopback registry on port %d: %w", port, err)
	}
	return nil
}

// ContainersHoldingMemory lists every container carrying reservation's
// label (ward.ReservationLabel) that is not stopped. A stopped container's
// VM is gone and holds no memory, so none listed proves the work that
// reserved it holds none, whoever later deletes what it left.
func ContainersHoldingMemory(ctx context.Context, containerPath string, reservation domain.InvocationID) ([]string, error) {
	path, err := resolveExecutable(containerPath, "container")
	if err != nil {
		return nil, err
	}
	reports, err := (appleBackend{containerPath: path, runner: execRunner{}}).
		labeledReports(ctx, ward.ReservationLabel, string(reservation))
	if err != nil {
		return nil, err
	}
	var holding []string
	for _, report := range reports {
		if report.Status.State != "stopped" {
			holding = append(holding, report.ID)
		}
	}
	return holding, nil
}
