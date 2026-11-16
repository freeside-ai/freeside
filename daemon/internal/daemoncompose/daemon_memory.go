package daemoncompose

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// A proof pass runs the agent and a helper beside it, each at the
// conformance size, so it reserves two VMs of that size.
var proofPassSize = ward.DefaultLaunchSize(ward.LaunchConformance)

const proofPassVMs = 2

// recoverDaemonReservations clears the daemon waits an earlier process left
// and releases each daemon reservation it left open once that work's
// containers are proved gone (plan §5.4). It runs at start, before any site
// reserves. An image build is recovered by stopping the builder and deleting
// what its label marks. The rest are swept as a later pass sweeps them
// (sweepLeftReservations). With a rebuild composed, every verification
// reservation left open is marked as one a lane build may have run under, so
// its release proves that build's containers gone too.
func recoverDaemonReservations(
	ctx context.Context, memory *engine.DaemonMemory, rebuild *engine.ProjectImageRebuild,
	containerPath string,
) error {
	if err := memory.ClearWaits(ctx); err != nil {
		return fmt.Errorf("clear the daemon's memory waits: %w", err)
	}
	if err := sweepLeftReservations(ctx, memory, containerPath, domain.ReservationImageRegistry,
		domain.ReservationProofPass, domain.ReservationProbe); err != nil {
		return err
	}
	if rebuild != nil {
		if err := rebuild.MarkLeftLaneBuilds(ctx, memory.Store); err != nil {
			return err
		}
	} else {
		// No rebuild is composed, but an earlier process's may have built.
		rebuild = &engine.ProjectImageRebuild{
			Memory: memory,
			RecoverBuild: func(ctx context.Context, id domain.InvocationID) error {
				return projectimage.RecoverBuild(ctx, containerPath, id)
			},
		}
	}
	return rebuild.RecoverLeftBuilds(ctx)
}

// sweepLeftReservations releases each reservation of kinds left open
// (engine.DaemonMemory.ListLeft) whose containers it proves hold no memory.
// A registry's reservation holds while the registry runs, because a later
// build reuses it. A proof pass or probe holds while a container with its
// label still runs; ward's own recovery and teardown own those containers,
// so one still running keeps its reservation, logged, until a later sweep.
// Only work no process holds is swept, so a sweep is safe beside live
// passes.
func sweepLeftReservations(
	ctx context.Context, memory *engine.DaemonMemory, containerPath string, kinds ...domain.LaunchReservationKind,
) error {
	left, err := memory.ListLeft(ctx)
	if err != nil {
		return fmt.Errorf("list the daemon's memory reservations: %w", err)
	}
	for _, reservation := range left {
		if !slices.Contains(kinds, reservation.Kind) {
			continue
		}
		var proof error
		switch reservation.Kind {
		case domain.ReservationImageRegistry:
			var free bool
			if free, proof = registryHoldsNoMemory(ctx, containerPath, reservation.InvocationID); proof == nil && !free {
				continue
			}
		case domain.ReservationProofPass, domain.ReservationProbe:
			proof = proveHoldsNoMemory(ctx, containerPath, reservation.InvocationID)
		case domain.ReservationImageBuild:
			// engine.ProjectImageRebuild.RecoverLeftBuilds proves and
			// releases these.
			continue
		case domain.ReservationExecution, domain.ReservationReview, domain.ReservationVerification:
			// A run's reservation is never listed as the daemon's own.
			continue
		}
		if err := memory.Recovered(reservation.InvocationID).Release(ctx, proof); err != nil {
			return err
		}
	}
	return nil
}

// proveHoldsNoMemory proves no container carrying reservation's label still
// runs.
func proveHoldsNoMemory(ctx context.Context, containerPath string, reservation domain.InvocationID) error {
	holding, err := projectimage.ContainersHoldingMemory(ctx, containerPath, reservation)
	if err == nil && len(holding) > 0 {
		err = fmt.Errorf("containers %s still run under the reservation", strings.Join(holding, ", "))
	}
	return err
}

// daemonWorkRunner runs a piece of the daemon's own container work, a ward
// proof pass or a probe, as a restart blocker and under a memory reservation
// (plan §5.4), and releases the reservation once no container with its label
// still runs.
type daemonWorkRunner struct {
	store         *store.Store
	running       *engine.RunningWork
	memory        *engine.DaemonMemory
	containerPath string
}

// run begins blocker, refused while an update drain is in force, and runs
// fn under work's reservation (reserved).
func (d daemonWorkRunner) run(
	ctx context.Context, blocker domain.RestartBlockerKind, work engine.DaemonWork, wait bool,
	fn func(context.Context) error,
) error {
	end, err := d.running.BeginUnlessDraining(ctx, d.store, blocker)
	if err != nil {
		return err
	}
	defer end()
	return d.reserved(ctx, work, wait, fn)
}

// reserved sweeps what earlier work of work's kind left, reserves work,
// waiting in the memory order when wait is set and refused at once otherwise
// (engine.DaemonMemory.Wait names who must not wait), and runs fn under the
// reservation's context, which labels the containers it starts. The caller
// has begun the work's restart blocker. A runner with no memory reserves
// nothing and runs fn as it is.
func (d daemonWorkRunner) reserved(
	ctx context.Context, work engine.DaemonWork, wait bool, fn func(context.Context) error,
) (err error) {
	if err := sweepLeftReservations(ctx, d.memory, d.containerPath, work.Kind); err != nil {
		return err
	}
	reserve := d.memory.TryReserve
	if wait {
		reserve = d.memory.Wait
	}
	reservation, err := reserve(ctx, work)
	if err != nil {
		return err
	}
	defer func() {
		var proof error
		if reservation.Held() {
			proof = proveHoldsNoMemory(context.WithoutCancel(ctx), d.containerPath, reservation.ID())
		}
		if releaseErr := reservation.Release(ctx, proof); releaseErr != nil {
			err = errors.Join(err, releaseErr)
		}
	}()
	return fn(reservation.Context(ctx))
}

// reservationRefusedForGood reports whether err is a reservation the budget
// refuses until the operator acts: work larger than the whole budget, or a
// container runtime with no recorded per-VM overhead.
func reservationRefusedForGood(err error) bool {
	return errors.Is(err, domain.ErrLaunchNeverFits) || errors.Is(err, domain.ErrRuntimeOverheadUnknown)
}

// registryHoldsNoMemory reports whether the registry a reservation names no
// longer runs. One that still runs is no failure: its reservation holds for
// the builds that reuse it.
func registryHoldsNoMemory(ctx context.Context, containerPath string, id domain.InvocationID) (bool, error) {
	port, err := strconv.Atoi(strings.TrimPrefix(string(id), "image-registry-"))
	if err != nil || projectimage.RegistryReservationID(port) != id {
		return false, fmt.Errorf("registry reservation %q names no port", id)
	}
	return projectimage.RegistryHoldsNoMemory(ctx, containerPath, port)
}

// registryMemory reserves a loopback registry's memory for the builds the
// daemon runs. A build holds its own reservation when it starts a registry,
// so it never waits for this one: a refusal fails the build, and the next
// pass builds again.
type registryMemory struct{ memory *engine.DaemonMemory }

func (r registryMemory) Reserve(ctx context.Context, port int) error {
	_, err := r.memory.TryReserve(ctx, engine.DaemonWork{
		Kind: domain.ReservationImageRegistry, ID: projectimage.RegistryReservationID(port),
		Size: projectimage.RegistrySize, VMs: 1,
	})
	return err
}

func (r registryMemory) Release(ctx context.Context, port int) error {
	return r.memory.Recovered(projectimage.RegistryReservationID(port)).Release(ctx, nil)
}
