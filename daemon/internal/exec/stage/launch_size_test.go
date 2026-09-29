package stage

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

func handoffSpecWithPolicy(t *testing.T, policyBody string) (ward.HandoffSpec, error) {
	t.Helper()
	d := newTestDriver(t, &stubGate{}, newStubExports())
	spec := testStartSpec()
	inputs := stageInputsWithBodies(t, &spec,
		[]byte("# Work item\nDo the thing.\n"),
		[]byte("You are the Phase 1A implementer.\n"),
		[]byte(policyBody),
	)
	instructions, err := ward.VendorInstructionsFromStageInputs(inputs)
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := renderPrompt(inputs)
	if err != nil {
		t.Fatal(err)
	}
	return d.handoffSpec(context.Background(), intent{
		InvocationID: testInvoke, RunID: testRunIDFor(testInvoke), Phase: phaseRunning,
		Spec: spec, Seed: filepath.Join(d.seedRoot, testRunIDFor(testInvoke)),
		Prompt: prompt, Inputs: durableInputsFrom(inputs),
		Instructions: instructions, RecordedAt: fixedNow, CommitDate: fixedNow,
	})
}

func TestHandoffSpecSizesTheWriterFromDurablePolicy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, policy string
		want         ward.ContainerSize
	}{
		{"default", `[{"key":"paths","value":"daemon/**"}]`, ward.DefaultLaunchSize(ward.LaunchWriter)},
		{
			"raised writer", `[{"key":"execution.writer_cpus","value":"8"},` +
				`{"key":"execution.writer_memory_mib","value":"6144"},{"key":"paths","value":"daemon/**"}]`,
			ward.ContainerSize{CPUs: 8, MemoryMiB: 6144},
		},
		{"raised verification only", `[{"key":"execution.verification_memory_mib","value":"8192"},` +
			`{"key":"paths","value":"daemon/**"}]`, ward.DefaultLaunchSize(ward.LaunchWriter)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hs, err := handoffSpecWithPolicy(t, tc.policy)
			if err != nil {
				t.Fatal(err)
			}
			if hs.Class != ward.LaunchWriter || hs.Size != tc.want {
				t.Fatalf("handoff = %s at %s, want writer at %s", hs.Class, hs.Size, tc.want)
			}
		})
	}
}

func TestHandoffSpecRefusesAnUnresolvableSize(t *testing.T) {
	t.Parallel()
	for name, policy := range map[string]string{
		"below default": `[{"key":"execution.writer_memory_mib","value":"512"},{"key":"paths","value":"daemon/**"}]`,
		"malformed":     `[{"key":"execution.writer_cpus","value":"eight"},{"key":"paths","value":"daemon/**"}]`,
		"not keys":      `{"paths":"daemon/**"}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := handoffSpecWithPolicy(t, policy); !errors.Is(err, ErrUnsupportedStart) {
				t.Fatalf("handoffSpec = %v, want ErrUnsupportedStart", err)
			}
		})
	}
}

func TestHandoffSpecIgnoresAProviderChosenSize(t *testing.T) {
	t.Parallel()
	gate := &stubGate{}
	d := newTestDriver(t, gate, newStubExports())
	d.provider = testProvider{volumes: stubVolumes{volume: testAuthVol}, handoffMutate: func(hs *ward.HandoffSpec) {
		hs.Class, hs.Size = ward.LaunchConformance, ward.ContainerSize{CPUs: 64, MemoryMiB: 1 << 20}
	}}
	hs, err := d.handoffSpec(context.Background(), testHandoffIntent(t, d))
	if err != nil {
		t.Fatal(err)
	}
	if hs.Class != ward.LaunchWriter || hs.Size != ward.DefaultLaunchSize(ward.LaunchWriter) {
		t.Fatalf("handoff = %s at %s, want the policy's writer size", hs.Class, hs.Size)
	}
}

func TestWriterMemoryLimitKillNamesTheFailedResult(t *testing.T) {
	size := ward.DefaultLaunchSize(ward.LaunchWriter)
	gate := &stubGate{
		handoffFn: func(ward.HandoffSpec) (*ward.HandoffResult, error) {
			return nil, &ward.MemoryLimitError{Class: ward.LaunchWriter, Size: size, Cause: ward.ErrWriterFailed}
		},
		recoverFn: func(string, ward.HandoffSpec) (*ward.RecoveryResult, error) {
			return &ward.RecoveryResult{Outcome: ward.RecoveryFailed, FailureStatus: 137}, nil
		},
	}
	d := newTestDriver(t, gate, newStubExports())
	d.seeder = &recordingSeeder{}
	d.runPipeline(context.Background(), orphan(t, d, phaseSeeding, nil))
	got, err := d.Collect(context.Background(), testInvoke)
	if err != nil || got.Status != exec.StatusFailed {
		t.Fatalf("result = %+v, %v; want failed", got, err)
	}
	if !strings.Contains(got.Summary, "memory limit") || !strings.Contains(got.Summary, size.String()) ||
		!strings.Contains(got.Summary, "status 137") {
		t.Fatalf("summary %q does not name the memory limit, size, and status", got.Summary)
	}
}
