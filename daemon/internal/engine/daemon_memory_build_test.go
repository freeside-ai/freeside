package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// reservationBuilder records the reservation each build's context carries,
// which the launch guard and the containers' labels read.
type reservationBuilder struct {
	ProjectImageBuilder
	under *[]domain.InvocationID
}

func (b reservationBuilder) Build(ctx context.Context, request projectimage.Request) (domain.ProjectImage, error) {
	id, _ := ward.LaunchReservationFrom(ctx)
	*b.under = append(*b.under, id)
	return b.ProjectImageBuilder.Build(ctx, request)
}

// buildSize is what an upkeep build under the default policy reserves.
func buildSize() int {
	return max(ward.DefaultLaunchSize(ward.LaunchVerification).MemoryMiB, projectimage.BuilderSize.MemoryMiB)
}

// withBuildMemory gives the harness's rebuild a budget that fits one build
// and no second piece of work its size.
func withBuildMemory(h *upkeepHarness) (*DaemonMemory, *[]domain.InvocationID) {
	memory := &DaemonMemory{
		Store: h.st, Bound: ward.LaunchBound{BudgetMiB: buildSize() * 3 / 2, RuntimeVersion: ward.RuntimeNone},
		Now: time.Now,
	}
	var under []domain.InvocationID
	h.upkeep.Rebuild.Memory = memory
	h.upkeep.Rebuild.Builder = reservationBuilder{ProjectImageBuilder: h.builder, under: &under}
	return memory, &under
}

func openBuilds(t *testing.T, m *DaemonMemory) []domain.LaunchReservation {
	t.Helper()
	return slices.DeleteFunc(storedOpen(t, m), func(r domain.LaunchReservation) bool {
		return r.Kind != domain.ReservationImageBuild
	})
}

// TestImageUpkeepWaitsForMemoryThenBuilds: with the budget full, an upkeep
// build waits instead of building or recording a verdict, holding no build
// gate, so the proof that frees a lane build's kept reservation can still
// take it; once the holder releases, it builds under its own reservation and
// releases that once the build proves its containers gone.
func TestImageUpkeepWaitsForMemoryThenBuilds(t *testing.T) {
	t.Parallel()
	repo := newUpkeepRepo(t)
	built := repo.npmBase()
	st := openUpkeepStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newUpkeepHarness(t, st, repo, "")
		h.seed(upkeepImage(t, built, nil))
		memory, under := withBuildMemory(h)
		holder, err := memory.TryReserve(t.Context(), DaemonWork{
			Kind: domain.ReservationProofPass, ID: "holder",
			Size: ward.ContainerSize{CPUs: 1, MemoryMiB: buildSize()}, VMs: 1,
		})
		if err != nil || !holder.Held() {
			t.Fatalf("holder = %+v, %v", holder, err)
		}
		done := make(chan error, 1)
		go func() { done <- h.upkeep.RunAll(t.Context()) }()
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("upkeep returned while the budget is full: %v", err)
		default:
		}
		if len(h.builder.requests) != 0 || len(h.refusals()) != 0 || len(h.faults) != 0 {
			t.Fatalf("builds = %d, refusals = %v, health = %q; want nothing while waiting",
				len(h.builder.requests), h.refusals(), h.faults)
		}
		if !h.upkeep.Rebuild.gate.tryAcquire() {
			t.Fatal("upkeep waits for memory holding the build gate")
		}
		h.upkeep.Rebuild.gate.release()
		if got := h.upkeep.Running.Counts()[domain.RestartBlockerImageBuild]; got != 0 {
			t.Fatalf("image builds counted while waiting = %d, want none, so a drain is not held", got)
		}
		if err := holder.Release(t.Context(), nil); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if len(*under) != 1 || !strings.HasPrefix(string((*under)[0]), "image-build-") {
			t.Fatalf("builds ran under %q, want one under an image-build reservation", *under)
		}
		if open := openBuilds(t, memory); len(open) != 0 {
			t.Fatalf("open build reservations = %+v, want the proved build's released", open)
		}
	})
}

// TestImageUpkeepKeepsAnUnprovenBuildsReservation: a build whose cleanup
// could not prove a container gone keeps its reservation, and the next pass's
// recovery releases it only once RecoverBuild proves that build's containers
// gone. A reservation of another kind is never recovered as a build.
func TestImageUpkeepKeepsAnUnprovenBuildsReservation(t *testing.T) {
	t.Parallel()
	repo := newUpkeepRepo(t)
	built := repo.npmBase()
	h := newUpkeepHarness(t, openUpkeepStore(t), repo, "")
	h.seed(upkeepImage(t, built, nil))
	memory, under := withBuildMemory(h)
	memory.Bound.BudgetMiB = buildSize() * 4
	h.builder.fail = fmt.Errorf("stop builder: %w", projectimage.ErrContainerAbsenceUnproven)
	if err := h.upkeep.RunAll(t.Context()); err != nil {
		t.Fatal(err)
	}
	open := openBuilds(t, memory)
	if len(open) != 1 || len(*under) != 1 || open[0].InvocationID != (*under)[0] {
		t.Fatalf("open build reservations = %+v after an unproven build under %q, want it kept", open, *under)
	}
	other, err := memory.TryReserve(t.Context(), daemonWork("pass", 512))
	if err != nil {
		t.Fatal(err)
	}

	proof := errors.New("builder still running")
	var recovered []domain.InvocationID
	h.upkeep.Rebuild.RecoverBuild = func(_ context.Context, id domain.InvocationID) error {
		recovered = append(recovered, id)
		return proof
	}
	if err := h.upkeep.Rebuild.RecoverLeftBuilds(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(openBuilds(t, memory)) != 1 {
		t.Fatal("a failed recovery released the build's reservation")
	}
	proof = nil
	if err := h.upkeep.Rebuild.RecoverLeftBuilds(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(openBuilds(t, memory)) != 0 || !slices.Equal(recovered, []domain.InvocationID{open[0].InvocationID, open[0].InvocationID}) {
		t.Fatalf("recovered %q, open %+v; want the build's reservation recovered and released", recovered, openBuilds(t, memory))
	}
	if !openReservation(t, memory, other.ID()) {
		t.Fatal("build recovery released another kind's reservation")
	}
}

// TestImageUpkeepSkipsARebuildMemoryRefuses: a registry the build would
// start, refused for memory, is no verdict on the tip: upkeep records no
// refusal, raises no health item, and releases the build's reservation.
func TestImageUpkeepSkipsARebuildMemoryRefuses(t *testing.T) {
	t.Parallel()
	repo := newUpkeepRepo(t)
	built := repo.npmBase()
	h := newUpkeepHarness(t, openUpkeepStore(t), repo, "")
	h.seed(upkeepImage(t, built, nil))
	memory, _ := withBuildMemory(h)
	h.builder.fail = fmt.Errorf("reserve memory for the loopback registry: %w", domain.ErrMachineCapacity)
	if err := h.upkeep.RunAll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(h.refusals()) != 0 || len(h.faults) != 0 || len(openBuilds(t, memory)) != 0 {
		t.Fatalf("refusals = %v, health = %q, open = %+v; want no verdict and no reservation",
			h.refusals(), h.faults, openBuilds(t, memory))
	}
}

// funcBuilder is a builder whose build is a closure.
type funcBuilder func(context.Context, projectimage.Request) (domain.ProjectImage, error)

func (f funcBuilder) Build(ctx context.Context, request projectimage.Request) (domain.ProjectImage, error) {
	return f(ctx, request)
}

// TestPublicationRebuildHoldsTheVerificationReservationThroughTheBuild: the
// lane's recipe read, build, and probes run under the task's verification
// reservation, which stays held while the build runs and after a build whose
// containers are not proved gone, and is released after one that proved them
// gone.
func TestPublicationRebuildHoldsTheVerificationReservationThroughTheBuild(t *testing.T) {
	t.Parallel()
	for name, failure := range map[string]error{
		"unproven": fmt.Errorf("stop builder: %w", projectimage.ErrContainerAbsenceUnproven),
		"proved":   errors.New("registry unreachable"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rf := newReleaseFixture(t, false)
			recipe := []byte(upkeepRecipe)
			image := rebuildTestImage(t, func(input *domain.ProjectImageInput) {
				input.RecipeDigest = verify.RecipeDigest(recipe)
			})
			rf.w.recipeReadTimeout = time.Minute
			rf.w.newRoom = func(domain.ProjectImage, ward.ContainerSize) (ProductionVerificationRoom, error) {
				return upkeepRoom{recipe: recipe}, nil
			}
			var (
				under      domain.InvocationID
				heldDuring bool
			)
			rf.w.rebuild = &ProjectImageRebuild{Inputs: projectimage.BuildInputs{BaseBuildRef: "freeside-agent-claude:local"}, Builder: funcBuilder(func(ctx context.Context, _ projectimage.Request) (domain.ProjectImage, error) {
				under, _ = ward.LaunchReservationFrom(ctx)
				heldDuring = rf.held(t)
				return domain.ProjectImage{}, failure
			})}
			_, refusal, err := rf.w.rebuildProjectImage(t.Context(), rf.task,
				productionBinding{run: rf.run, image: image}, t.TempDir(), strings.Repeat("b", 40),
				domain.ProjectImageBaseInputs{})
			if refusal != nil || !errors.Is(err, errProductionRetryable) {
				t.Fatalf("rebuild = %+v, %v; want a retryable failure", refusal, err)
			}
			if under != rf.task.verificationInvocationID() || !heldDuring {
				t.Fatalf("build ran under %q, reservation held %t; want the task's verification reservation held",
					under, heldDuring)
			}
			if held := rf.held(t); held != (name == "unproven") {
				t.Fatalf("held after the build = %t", held)
			}
		})
	}
}

// TestALaneBuildsKeptReservationIsReleasedOnlyOnTheBuildsProof: ward's
// verification proof covers neither the builder VM nor a lane build's probes,
// so a reservation a lane build kept is released by a later pass, and by the
// end-of-task proof, only once RecoverBuild proves that build's containers
// gone, and a builder busy with another build is no proof either way.
func TestALaneBuildsKeptReservationIsReleasedOnlyOnTheBuildsProof(t *testing.T) {
	t.Parallel()
	rf := newReleaseFixture(t, true)
	invocation := rf.task.verificationInvocationID()
	proof := errors.New("builder still running")
	var recovered []domain.InvocationID
	rf.w.rebuild = &ProjectImageRebuild{RecoverBuild: func(_ context.Context, id domain.InvocationID) error {
		recovered = append(recovered, id)
		return proof
	}}
	unproven := fmt.Errorf("stop builder: %w", projectimage.ErrContainerAbsenceUnproven)
	if err := rf.w.releaseVerification(t.Context(), rf.task, rf.run, unproven); err != nil || !rf.held(t) {
		t.Fatalf("release after an unproven build = %v, held %t; want it kept", err, rf.held(t))
	}
	if err := rf.w.releaseVerification(t.Context(), rf.task, rf.run, nil); err != nil || !rf.held(t) {
		t.Fatalf("a later clean pass = %v, held %t; want it kept while the build is unproven", err, rf.held(t))
	}
	if err := rf.w.proveHeldVerification(t.Context(), rf.run, invocation, false); !errors.Is(err, ward.ErrVerificationAbsenceUnproven) {
		t.Fatalf("end-of-task proof = %v, want unproven", err)
	}
	if !rf.w.rebuild.gate.tryAcquire() {
		t.Fatal("gate busy")
	}
	if err := rf.w.proveHeldVerification(t.Context(), rf.run, invocation, false); !errors.Is(err, errProjectImageBuilderBusy) {
		t.Fatalf("end-of-task proof beside another build = %v, want %v", err, errProjectImageBuilderBusy)
	}
	rf.w.rebuild.gate.release()
	if !slices.Equal(recovered, []domain.InvocationID{invocation, invocation}) {
		t.Fatalf("recovered %q, want the verification reservation's build twice", recovered)
	}
	proof = nil
	if err := rf.w.releaseVerification(t.Context(), rf.task, rf.run, nil); err != nil || rf.held(t) {
		t.Fatalf("a pass after the build's proof = %v, held %t; want it released", err, rf.held(t))
	}
}

// TestAStartMarksLeftVerificationReservationsAsLaneBuilds: a lane build an
// earlier process ran left no mark, so a start marks every verification
// reservation left open, and its end-of-task proof proves that build's
// containers gone; a reservation no lane build could have run under is
// proved without stopping the builder.
func TestAStartMarksLeftVerificationReservationsAsLaneBuilds(t *testing.T) {
	t.Parallel()
	for name, left := range map[string]bool{"left by an earlier process": true, "this process's own": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rf := newReleaseFixture(t, true)
			invocation := rf.task.verificationInvocationID()
			var recovered []domain.InvocationID
			rf.w.rebuild = &ProjectImageRebuild{RecoverBuild: func(_ context.Context, id domain.InvocationID) error {
				recovered = append(recovered, id)
				return nil
			}}
			if left {
				if err := rf.w.rebuild.MarkLeftLaneBuilds(t.Context(), rf.w.store); err != nil {
					t.Fatal(err)
				}
			}
			if err := rf.w.proveHeldVerification(t.Context(), rf.run, invocation, false); err != nil {
				t.Fatal(err)
			}
			if want := map[bool]int{true: 1, false: 0}[left]; len(recovered) != want {
				t.Fatalf("recovered %q, want %d build proofs", recovered, want)
			}
		})
	}
}
