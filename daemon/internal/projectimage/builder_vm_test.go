package projectimage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// TestAppleBuildRunsTheBuilderAtADeclaredSizeAndStopsIt: a build starts the
// absent builder VM at BuilderSize, and once the build returns, stops it and
// inspects it again to prove it stopped, so the probes that follow never run
// beside it.
func TestAppleBuildRunsTheBuilderAtADeclaredSizeAndStopsIt(t *testing.T) {
	t.Parallel()
	runner := &recordingRunner{}
	spec := testBuildSpec()
	spec.BuildProxy = "http://192.168.64.1:53536"
	if err := (appleBackend{containerPath: "container", runner: runner, manageBuilder: true}).Build(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	inspect := []string{"inspect", builderContainerID}
	want := [][]string{
		inspect,
		append([]string{"builder", "start"}, ward.SizeArgs(BuilderSize)...),
		inspect,
		{"builder", "stop"},
		inspect,
	}
	if !slices.EqualFunc(runner.builder.calls, want, slices.Equal) {
		t.Fatalf("builder calls = %q, want %q", runner.builder.calls, want)
	}
	if runner.builder.state != "stopped" || len(runner.specs) != 1 || runner.specs[0].Args[0] != "build" {
		t.Fatalf("builder %q after the build, commands %q", runner.builder.state, runner.specs)
	}
}

// TestAppleBuildUsesARunningBuilderAndStillStopsIt: a builder another build
// left running is used as it stands, not restarted at a new size, and is
// stopped after this build like one it started.
func TestAppleBuildUsesARunningBuilderAndStillStopsIt(t *testing.T) {
	t.Parallel()
	runner := &recordingRunner{builder: builderVM{state: "running"}}
	spec := testBuildSpec()
	spec.BuildProxy = "http://192.168.64.1:53536"
	if err := (appleBackend{containerPath: "container", runner: runner, manageBuilder: true}).Build(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(runner.builder.calls, func(args []string) bool { return slices.Contains(args, "start") }) ||
		runner.builder.state != "stopped" {
		t.Fatalf("builder calls = %q, state %q", runner.builder.calls, runner.builder.state)
	}
}

// TestAnOperatorsBuildLeavesTheBuilderAlone: a build the daemon does not
// manage, an operator's, neither starts nor stops the builder VM, so it never
// ends a daemon build running in it.
func TestAnOperatorsBuildLeavesTheBuilderAlone(t *testing.T) {
	t.Parallel()
	runner := &recordingRunner{builder: builderVM{state: "running"}}
	spec := testBuildSpec()
	spec.BuildProxy = "http://192.168.64.1:53536"
	if err := (appleBackend{containerPath: "container", runner: runner}).Build(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if len(runner.builder.calls) != 0 || runner.builder.state != "running" {
		t.Fatalf("builder calls = %q, state %q; want the builder untouched", runner.builder.calls, runner.builder.state)
	}
}

// TestAppleBuildFailsWhenTheBuilderIsNotProvedStopped: a builder still
// running after its stop fails the build as unproven absence, which keeps the
// build's reservation held, even when the build itself succeeded.
func TestAppleBuildFailsWhenTheBuilderIsNotProvedStopped(t *testing.T) {
	t.Parallel()
	runner := &recordingRunner{}
	stuck := &stuckBuilderRunner{recordingRunner: runner}
	spec := testBuildSpec()
	spec.BuildProxy = "http://192.168.64.1:53536"
	err := (appleBackend{containerPath: "container", runner: stuck, manageBuilder: true}).Build(t.Context(), spec)
	if !errors.Is(err, ErrContainerAbsenceUnproven) {
		t.Fatalf("Build = %v, want %v", err, ErrContainerAbsenceUnproven)
	}
}

// stuckBuilderRunner is a builder VM whose stop succeeds and leaves it
// running.
type stuckBuilderRunner struct{ *recordingRunner }

func (r *stuckBuilderRunner) Run(ctx context.Context, spec commandSpec) (commandOutput, error) {
	output, err := r.recordingRunner.Run(ctx, spec)
	if slices.Equal(spec.Args, []string{"builder", "stop"}) {
		r.builder.state = "running"
	}
	return output, err
}

// TestAppleProbesCarryTheirReservation: a probe and the allowlist container
// started under a reservation carry its label, so a start after a crash can
// find them; without one they carry none.
func TestAppleProbesCarryTheirReservation(t *testing.T) {
	t.Parallel()
	label := ward.ReservationLabel + "=image-build-1"
	for name, ctx := range map[string]context.Context{
		"reserved":   ward.WithLaunchReservation(t.Context(), "image-build-1"),
		"unreserved": t.Context(),
	} {
		runner := &managedRunner{next: func(context.Context, commandSpec) (commandOutput, error) {
			return commandOutput{exited: true}, nil
		}}
		backend := appleBackend{containerPath: "container", runner: runner}
		if _, err := backend.Run(ctx, runSpec{ImageRef: "image", Argv: []string{"true"}}); err != nil {
			t.Fatalf("%s: Run = %v", name, err)
		}
		labeled := slices.ContainsFunc(runner.specs, func(spec commandSpec) bool {
			return spec.Args[0] == "run" && slices.Contains(spec.Args, label)
		})
		if labeled != (name == "reserved") {
			t.Fatalf("%s: run labeled %t: %q", name, labeled, runner.specs)
		}
	}
}

// TestAppleProbeCleanupFailureIsUnprovenAbsence: a probe whose container
// could not be deleted fails as unproven absence.
func TestAppleProbeCleanupFailureIsUnprovenAbsence(t *testing.T) {
	t.Parallel()
	failing := &failingDeleteRunner{managedRunner: &managedRunner{}}
	_, err := appleBackend{containerPath: "container", runner: failing}.Run(t.Context(),
		runSpec{ImageRef: "image", Argv: []string{"true"}})
	if !errors.Is(err, ErrContainerAbsenceUnproven) {
		t.Fatalf("Run = %v, want %v", err, ErrContainerAbsenceUnproven)
	}
}

// failingDeleteRunner refuses every delete, before managedRunner forgets the
// container.
type failingDeleteRunner struct{ *managedRunner }

func (r *failingDeleteRunner) Run(ctx context.Context, spec commandSpec) (commandOutput, error) {
	if len(spec.Args) > 0 && spec.Args[0] == "delete" {
		return commandOutput{bytes: []byte("runtime busy")}, errors.New("exit status 1")
	}
	return r.managedRunner.Run(ctx, spec)
}

// TestAppleRecoverBuildDeletesOnlyItsReservationsContainers: recovery stops
// the builder and deletes every container carrying the build's reservation,
// leaving another reservation's, and proves its own gone.
func TestAppleRecoverBuildDeletesOnlyItsReservationsContainers(t *testing.T) {
	t.Parallel()
	labels := map[string]string{"probe-a": "image-build-1", "probe-b": "image-build-2", "probe-c": "image-build-1"}
	runner := &recordingRunner{builder: builderVM{state: "running"}}
	runner.run = func(spec commandSpec) (commandOutput, error) {
		switch {
		case slices.Equal(spec.Args, []string{"list", "--all", "--format", "json"}):
			var listed []containerInspect
			for _, id := range slices.Sorted(maps.Keys(labels)) {
				listed = append(listed, tidyContainer(id, ""))
			}
			body, err := json.Marshal(listed)
			return commandOutput{bytes: body}, err
		case len(spec.Args) == 2 && spec.Args[0] == "inspect":
			report := tidyContainer(spec.Args[1], "")
			report.Configuration.Labels[ward.ReservationLabel] = labels[spec.Args[1]]
			return containerInspectOutput(report), nil
		case len(spec.Args) == 3 && spec.Args[0] == "delete":
			delete(labels, spec.Args[2])
			return commandOutput{}, nil
		}
		return commandOutput{}, nil
	}
	if err := (appleBackend{containerPath: "container", runner: runner}).recoverBuild(t.Context(), "image-build-1"); err != nil {
		t.Fatal(err)
	}
	if runner.builder.state != "stopped" {
		t.Fatalf("builder %q after recovery", runner.builder.state)
	}
	if _, kept := labels["probe-b"]; !kept || len(labels) != 1 {
		t.Fatalf("containers after recovery = %v, want only another build's", labels)
	}
}

// TestApplePublishReservesTheRegistryItStarts: a build that finds no
// registry for its port releases that port's reservation, reserves it again
// before it starts the registry at RegistrySize, and releases it once its
// failure cleanup deletes the registry. A refused reservation starts nothing.
func TestApplePublishReservesTheRegistryItStarts(t *testing.T) {
	t.Parallel()
	const port = 5103
	newRunner := func() *managedRunner {
		return &managedRunner{next: func(_ context.Context, spec commandSpec) (commandOutput, error) {
			switch {
			case slices.Equal(spec.Args, []string{"list", "--all", "--format", "json"}):
				return commandOutput{bytes: []byte("[]")}, nil
			case slices.Equal(spec.Args, []string{"image", "inspect", registryImage}):
				return inspectOutputFor(registryImage, registryImageDigest), nil
			case len(spec.Args) >= 2 && spec.Args[0] == "image" && spec.Args[1] == "push":
				return commandOutput{bytes: []byte("push failed")}, errors.New("exit status 1")
			}
			return commandOutput{}, nil
		}}
	}
	publish := func(runner *managedRunner, memory *fakeRegistryMemory) error {
		backend := appleBackend{
			containerPath: "container", runner: runner, registryLockDir: t.TempDir(), registryMemory: memory,
			probeRegistry: func(context.Context, string) error { return errors.New("registry is not running") },
			waitRegistry:  func(context.Context, string) error { return nil },
		}
		_, err := backend.Publish(t.Context(), publishSpec{
			LocalRef: "project:local", Digest: testImageDigest,
			ImageName: "project", RefTag: "v1", LocalRegistryPort: port,
		})
		return err
	}

	runner, memory := newRunner(), &fakeRegistryMemory{}
	if err := publish(runner, memory); err == nil {
		t.Fatal("Publish succeeded past a failed push")
	}
	if want := []string{"release 5103", "reserve 5103", "release 5103"}; !slices.Equal(memory.calls, want) {
		t.Fatalf("registry memory calls = %q, want %q", memory.calls, want)
	}
	if !slices.ContainsFunc(runner.specs, func(spec commandSpec) bool {
		return spec.Args[0] == "run" && slices.Contains(spec.Args, "--detach") &&
			containsRun(spec.Args, ward.SizeArgs(RegistrySize))
	}) {
		t.Fatalf("no registry started at its declared size: %q", runner.specs)
	}

	runner, memory = newRunner(), &fakeRegistryMemory{refuse: errors.New("no memory")}
	if err := publish(runner, memory); err == nil || !errors.Is(err, memory.refuse) {
		t.Fatalf("Publish under a refused reservation = %v", err)
	}
	if slices.ContainsFunc(runner.specs, func(spec commandSpec) bool { return spec.Args[0] == "run" }) {
		t.Fatalf("a refused reservation started the registry: %q", runner.specs)
	}
}

type fakeRegistryMemory struct {
	calls  []string
	refuse error
}

func (m *fakeRegistryMemory) Reserve(_ context.Context, port int) error {
	m.calls = append(m.calls, fmt.Sprintf("reserve %d", port))
	return m.refuse
}

func (m *fakeRegistryMemory) Release(_ context.Context, port int) error {
	m.calls = append(m.calls, fmt.Sprintf("release %d", port))
	return nil
}

// containsRun reports whether args holds want as a contiguous run.
func containsRun(args, want []string) bool {
	for i := range args {
		if i+len(want) <= len(args) && slices.Equal(args[i:i+len(want)], want) {
			return true
		}
	}
	return false
}
