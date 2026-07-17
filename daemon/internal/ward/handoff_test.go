package ward

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// handoffFixture wires a fake runtime to a backend with deterministic
// waiting: Sleep is a no-op counter, and timeouts are budgeted in whole poll
// intervals, so no test depends on wall-clock time.
type handoffFixture struct {
	rt     *fakeRuntime
	cfg    Config
	sleeps *int
}

func newHandoffFixture(t *testing.T) *handoffFixture {
	t.Helper()
	sleeps := 0
	cfg := testConfig()
	cfg.PollInterval = 100 * time.Millisecond
	cfg.WriterStopTimeout = time.Second // 10 poll attempts, with a non-flaky real deadline
	cfg.ExporterTimeout = time.Second
	cfg.Sleep = func(context.Context, time.Duration) error {
		sleeps++
		return nil
	}
	return &handoffFixture{rt: newFakeRuntime(t), cfg: cfg, sleeps: &sleeps}
}

func (fx *handoffFixture) backend(t *testing.T) *Backend {
	t.Helper()
	b, err := New(fx.rt, fx.cfg)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (fx *handoffFixture) run(t *testing.T) (*HandoffResult, error) {
	t.Helper()
	res, err := fx.backend(t).Handoff(context.Background(), testHandoffSpec())
	if res != nil {
		t.Cleanup(func() { _ = os.RemoveAll(res.ExportDir) })
	}
	return res, err
}

// assertReaped proves teardown left nothing: no containers, no volumes
// (acceptance 5, asserted after success and after every induced failure).
func (fx *handoffFixture) assertReaped(t *testing.T) {
	t.Helper()
	fx.rt.mu.Lock()
	defer fx.rt.mu.Unlock()
	for name := range fx.rt.ctrs {
		t.Errorf("container %q survived teardown", name)
	}
	for name := range fx.rt.vols {
		t.Errorf("volume %q survived teardown", name)
	}
}

func wantCheckFailure(t *testing.T, err error, want Check) {
	t.Helper()
	var cf *ConformanceFailure
	if !errors.As(err, &cf) {
		t.Fatalf("error = %v, want ConformanceFailure", err)
	}
	if cf.Check != want {
		t.Fatalf("Check = %q, want %q (reason: %s)", cf.Check, want, cf.Reason)
	}
	if !errors.Is(err, ErrConformance) {
		t.Error("failure does not unwrap to ErrConformance")
	}
}

func TestHandoffSuccess(t *testing.T) {
	fx := newHandoffFixture(t)
	res, err := fx.run(t)
	if err != nil {
		t.Fatalf("Handoff = %v, want success", err)
	}

	if res.Admission.Backend != BackendName {
		t.Errorf("Admission.Backend = %q, want %q", res.Admission.Backend, BackendName)
	}
	for _, c := range declaredCapabilities {
		if !res.Admission.Declared.Has(c) {
			t.Errorf("Admission.Declared missing %q", c)
		}
	}
	if len(res.Manifest.Entries) != 1 {
		t.Errorf("Manifest entries = %d, want 1", len(res.Manifest.Entries))
	}
	if _, err := os.Stat(filepath.Join(res.ExportDir, "manifest.json")); err != nil {
		t.Errorf("released output dir: %v", err)
	}
	// Only the returned output dir survives: the archive scratch dir is
	// removed, and no other handoff temp dir for this run lingers.
	for _, d := range scratchDirs(t, testHandoffSpec().RunID) {
		if d != res.ExportDir {
			t.Errorf("unexpected leftover handoff temp dir: %s", d)
		}
	}
	fx.assertReaped(t)
}

func TestArchiveCapWriterBoundary(t *testing.T) {
	t.Run("exact fit", func(t *testing.T) {
		var dest bytes.Buffer
		w := &archiveCapWriter{dest: &dest, remaining: 8}
		if n, err := w.Write([]byte("12345678")); err != nil || n != 8 || w.overflow {
			t.Fatalf("Write = (%d, %v), overflow=%v; want exact fit", n, err, w.overflow)
		}
	})

	t.Run("one byte over", func(t *testing.T) {
		var dest bytes.Buffer
		w := &archiveCapWriter{dest: &dest, remaining: 8}
		n, err := w.Write([]byte("123456789"))
		if n != 8 || !errors.Is(err, errArchiveByteCap) || !w.overflow {
			t.Fatalf("Write = (%d, %v), overflow=%v; want capped overflow", n, err, w.overflow)
		}
		if dest.Len() != 8 {
			t.Fatalf("materialized bytes = %d, want 8", dest.Len())
		}
	})
}

func TestHandoffRootFSArchiveCap(t *testing.T) {
	fx := newHandoffFixture(t)
	fx.cfg.MaxArchiveBytes = 1024
	_, err := fx.run(t)
	wantCheckFailure(t, err, CheckExportVerification)
	fx.assertReaped(t)
}

// TestHandoffOrderObservedState is acceptance 3: the gate acts on observed
// stopped state, never on scheduling intent. The agent reports running for
// three polls; nothing writer-terminating or exporter-related may happen
// before the stopped observation, and the exporter is inspected before it
// is started (check 4 is pre-execution).
func TestHandoffOrderObservedState(t *testing.T) {
	fx := newHandoffFixture(t)
	names := namesFor(testHandoffSpec().RunID)
	fx.rt.runningInspects[names.Agent] = 3

	if _, err := fx.run(t); err != nil {
		t.Fatalf("Handoff = %v, want success", err)
	}

	idx := func(call string) int {
		i := fx.rt.callIndex(call)
		if i < 0 {
			t.Fatalf("call %q never happened", call)
		}
		return i
	}
	createAgent := idx("create-container " + names.Agent)
	inspectAgent := idx("inspect " + names.Agent)
	startAgent := idx("start-container " + names.Agent)
	deleteAgent := idx("delete-container " + names.Agent)
	createExporter := idx("create-container " + names.Exporter)
	inspectExporter := idx("inspect " + names.Exporter)
	startExporter := idx("start-container " + names.Exporter)

	// Five agent inspects: one stopped pre-start allowlist check, three
	// running polls, and the final stopped observation.
	agentInspects := 0
	lastAgentInspect := -1
	fx.rt.mu.Lock()
	for i, c := range fx.rt.calls {
		if c == "inspect "+names.Agent && i < deleteAgent {
			agentInspects++
			lastAgentInspect = i
		}
	}
	fx.rt.mu.Unlock()
	if agentInspects != 5 {
		t.Errorf("agent inspected %d times before delete, want 5 (pre-start + 3 running + 1 stopped)", agentInspects)
	}
	if createAgent >= inspectAgent || inspectAgent >= startAgent {
		t.Errorf("agent allowlist not pre-execution: create %d, inspect %d, start %d",
			createAgent, inspectAgent, startAgent)
	}
	if lastAgentInspect >= deleteAgent || deleteAgent >= createExporter {
		t.Errorf("writer termination out of order: last inspect %d, delete %d, exporter create %d",
			lastAgentInspect, deleteAgent, createExporter)
	}
	if createExporter >= inspectExporter || inspectExporter >= startExporter {
		t.Errorf("check 4 not pre-execution: create %d, inspect %d, start %d",
			createExporter, inspectExporter, startExporter)
	}
}

// TestHandoffWriterNeverStops is acceptance 2/3 for check 3: a writer that
// stays running exhausts the observation budget and fails the gate; no
// exporter is ever created, and teardown still reaps everything.
func TestHandoffWriterNeverStops(t *testing.T) {
	fx := newHandoffFixture(t)
	names := namesFor(testHandoffSpec().RunID)
	fx.rt.runningInspects[names.Agent] = math.MaxInt - 1

	_, err := fx.run(t)
	wantCheckFailure(t, err, CheckWriterTermination)
	if i := fx.rt.callIndex("create-container " + names.Exporter); i >= 0 {
		t.Error("exporter was created despite an unterminated writer")
	}
	fx.assertReaped(t)
}

// TestHandoffObservationTimeoutBoundsInspect proves the named writer and
// exporter timeouts are hard ceilings around runtime observation itself, not
// merely poll-count budgets that a wedged Inspect call can defeat.
func TestHandoffObservationTimeoutBoundsInspect(t *testing.T) {
	names := namesFor(testHandoffSpec().RunID)
	cases := []struct {
		name      string
		container string
		check     Check
		setLimit  func(*Config, time.Duration)
	}{
		{
			name:      "writer",
			container: names.Agent,
			check:     CheckWriterTermination,
			setLimit:  func(c *Config, d time.Duration) { c.WriterStopTimeout = d },
		},
		{
			name:      "exporter",
			container: names.Exporter,
			check:     CheckExportVerification,
			setLimit:  func(c *Config, d time.Duration) { c.ExporterTimeout = d },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newHandoffFixture(t)
			const limit = 20 * time.Millisecond
			tc.setLimit(&fx.cfg, limit)
			fx.rt.blockInspect = tc.container

			started := time.Now()
			_, err := fx.run(t)
			wantCheckFailure(t, err, tc.check)
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Errorf("Handoff returned after %s, want bounded near %s", elapsed, limit)
			}
			fx.assertReaped(t)
		})
	}
}

// TestHandoffRuntimeCannotRewriteExpectedSpec treats the Runtime argument as
// an untrusted call boundary: mutating a received spec may change realized
// state, but must not rewrite the gate's expected command or mount allowlist.
func TestHandoffRuntimeCannotRewriteExpectedSpec(t *testing.T) {
	names := namesFor(testHandoffSpec().RunID)
	cases := []struct {
		name      string
		container string
		check     Check
	}{
		{name: "writer", container: names.Agent, check: CheckControlPlaneIsolation},
		{name: "exporter", container: names.Exporter, check: CheckExporterAllowlist},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newHandoffFixture(t)
			fx.rt.onCreateContainer = func(spec ContainerSpec) error {
				if spec.Name == tc.container {
					spec.Command[0] = "runtime-rewrite"
					spec.Mounts[0].Source = "runtime-rewrite"
				}
				return nil
			}

			_, err := fx.run(t)
			wantCheckFailure(t, err, tc.check)
			fx.assertReaped(t)
		})
	}
}

func TestHandoffAgentDeleteFails(t *testing.T) {
	fx := newHandoffFixture(t)
	names := namesFor(testHandoffSpec().RunID)
	fx.rt.onDeleteContainer = func(id string) (bool, error) {
		if id == names.Agent {
			return false, errors.New("runtime refused")
		}
		return false, nil
	}
	_, err := fx.run(t)
	wantCheckFailure(t, err, CheckWriterTermination)
}

// TestHandoffAgentStillListed: a successful delete call is not enough; the
// ID must be absent from the full listing.
func TestHandoffAgentStillListed(t *testing.T) {
	fx := newHandoffFixture(t)
	names := namesFor(testHandoffSpec().RunID)
	fx.rt.onListContainers = func(list []ContainerSummary) ([]ContainerSummary, error) {
		return append(list, ContainerSummary{ID: names.Agent, State: StateStopped}), nil
	}
	_, err := fx.run(t)
	wantCheckFailure(t, err, CheckWriterTermination)
}

// TestHandoffAmbiguousContainerCreateReaped proves an ambiguous agent or
// exporter create is recovered by inspecting the per-invocation ownership
// label, then reaped despite the create call reporting failure.
func TestHandoffAmbiguousContainerCreateReaped(t *testing.T) {
	names := namesFor(testHandoffSpec().RunID)
	for _, id := range []string{names.Agent, names.Exporter} {
		t.Run(id, func(t *testing.T) {
			fx := newHandoffFixture(t)
			fx.rt.createThenFail = id

			if _, err := fx.run(t); err == nil {
				t.Fatal("ambiguous create returned success")
			}
			fx.assertReaped(t)
		})
	}
}

// TestHandoffAmbiguousContainerCreateReapedAfterCancellation proves
// ownership recovery happens inside teardown's detached context. The caller
// is canceled after the runtime inserts the object but before create returns;
// both agent and exporter must still be discovered by their ownership label.
func TestHandoffAmbiguousContainerCreateReapedAfterCancellation(t *testing.T) {
	names := namesFor(testHandoffSpec().RunID)
	for _, id := range []string{names.Agent, names.Exporter} {
		t.Run(id, func(t *testing.T) {
			fx := newHandoffFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			fx.rt.createThenFail = id
			fx.rt.afterAmbiguousContainerCreate = cancel

			if _, err := fx.backend(t).Handoff(ctx, testHandoffSpec()); err == nil {
				t.Fatal("ambiguous canceled create returned success")
			}
			fx.assertReaped(t)
		})
	}
}

// TestHandoffAmbiguousVolumeCreateReaped proves a CreateVolume call that
// makes the workspace and then errors is cleaned up by its unpredictable
// ownership label, even though no other runtime object was owned.
func TestHandoffAmbiguousVolumeCreateReaped(t *testing.T) {
	fx := newHandoffFixture(t)
	names := namesFor(testHandoffSpec().RunID)
	fx.rt.createVolumeThenFail = true

	if _, err := fx.run(t); err == nil {
		t.Fatal("ambiguous volume create returned success")
	}
	fx.rt.mu.Lock()
	defer fx.rt.mu.Unlock()
	if _, ok := fx.rt.vols[names.Workspace]; ok {
		t.Error("workspace from ambiguous create survived teardown")
	}
	if len(fx.rt.ctrs) != 0 {
		t.Errorf("ambiguous workspace create unexpectedly touched containers: %v", fx.rt.ctrs)
	}
}

// TestHandoffAmbiguousCreateOwnershipEvidence proves teardown uses
// inspect when the runtime's list shape omits container labels, while still
// failing closed if neither runtime view exposes the invocation token.
func TestHandoffAmbiguousCreateOwnershipEvidence(t *testing.T) {
	names := namesFor(testHandoffSpec().RunID)
	t.Run("container list falls back to inspect", func(t *testing.T) {
		fx := newHandoffFixture(t)
		fx.rt.createThenFail = names.Agent
		fx.rt.onListContainers = func(list []ContainerSummary) ([]ContainerSummary, error) {
			for i := range list {
				if list[i].ID == names.Agent {
					list[i].LabelsObserved = false
					list[i].Labels = nil
				}
			}
			return list, nil
		}

		_, err := fx.run(t)
		if err == nil {
			t.Fatal("ambiguous create returned success")
		}
		fx.assertReaped(t)
	})

	t.Run("container list and inspect both omit labels", func(t *testing.T) {
		fx := newHandoffFixture(t)
		fx.rt.createThenFail = names.Agent
		fx.rt.onListContainers = func(list []ContainerSummary) ([]ContainerSummary, error) {
			for i := range list {
				if list[i].ID == names.Agent {
					list[i].LabelsObserved = false
					list[i].Labels = nil
				}
			}
			return list, nil
		}
		fx.rt.onInspect = func(id string, rep InspectReport) (InspectReport, error) {
			if id == names.Agent {
				rep.LabelsObserved = false
				rep.Labels = nil
			}
			return rep, nil
		}

		_, err := fx.run(t)
		wantCheckFailure(t, err, CheckTeardown)
		fx.rt.mu.Lock()
		defer fx.rt.mu.Unlock()
		if _, ok := fx.rt.ctrs[names.Agent]; !ok {
			t.Error("teardown deleted an ambiguously owned container without observing its labels")
		}
	})

	t.Run("container inspect identifies another object", func(t *testing.T) {
		fx := newHandoffFixture(t)
		fx.rt.createThenFail = names.Agent
		fx.rt.onListContainers = func(list []ContainerSummary) ([]ContainerSummary, error) {
			for i := range list {
				if list[i].ID == names.Agent {
					list[i].LabelsObserved = false
					list[i].Labels = nil
				}
			}
			return list, nil
		}
		fx.rt.onInspect = func(id string, rep InspectReport) (InspectReport, error) {
			if id == names.Agent {
				rep.ID = "another-object"
			}
			return rep, nil
		}

		_, err := fx.run(t)
		wantCheckFailure(t, err, CheckTeardown)
		fx.rt.mu.Lock()
		defer fx.rt.mu.Unlock()
		if _, ok := fx.rt.ctrs[names.Agent]; !ok {
			t.Error("teardown deleted a container using another object's inspect report")
		}
	})

	t.Run("container inspect omits unrelated exporter field", func(t *testing.T) {
		fx := newHandoffFixture(t)
		fx.rt.createThenFail = names.Agent
		fx.rt.onListContainers = func(list []ContainerSummary) ([]ContainerSummary, error) {
			for i := range list {
				if list[i].ID == names.Agent {
					list[i].LabelsObserved = false
					list[i].Labels = nil
				}
			}
			return list, nil
		}
		fx.rt.onInspect = func(id string, rep InspectReport) (InspectReport, error) {
			if id == names.Agent {
				rep.AllowlistFieldsObserved = false
				rep.ImageReference = ""
			}
			return rep, nil
		}

		_, err := fx.run(t)
		if err == nil {
			t.Fatal("ambiguous create returned success")
		}
		fx.assertReaped(t)
	})

	t.Run("volume", func(t *testing.T) {
		fx := newHandoffFixture(t)
		fx.rt.createVolumeThenFail = true
		fx.rt.onListVolumes = func(list []VolumeSummary) ([]VolumeSummary, error) {
			for i := range list {
				if list[i].Name == names.Workspace {
					list[i].LabelsObserved = false
					list[i].Labels = nil
				}
			}
			return list, nil
		}

		_, err := fx.run(t)
		wantCheckFailure(t, err, CheckTeardown)
		fx.rt.mu.Lock()
		defer fx.rt.mu.Unlock()
		if _, ok := fx.rt.vols[names.Workspace]; !ok {
			t.Error("teardown deleted an ambiguously owned volume without observing its labels")
		}
	})
}

// TestHandoffAmbiguousCreateDuplicateContainerIDFailsTeardown proves a
// contradictory duplicate identity cannot make list ordering decide whether
// the invocation token was observed. No candidate is deleted, and the
// incomplete cleanup is returned as a teardown failure.
func TestHandoffAmbiguousCreateDuplicateContainerIDFailsTeardown(t *testing.T) {
	fx := newHandoffFixture(t)
	names := namesFor(testHandoffSpec().RunID)
	fx.rt.createThenFail = names.Agent
	fx.rt.onListContainers = func(list []ContainerSummary) ([]ContainerSummary, error) {
		for _, cs := range list {
			if cs.ID == names.Agent {
				foreignView := ContainerSummary{
					ID: names.Agent, State: cs.State,
					Labels: runLabels(testHandoffSpec().RunID), LabelsObserved: true,
				}
				return append([]ContainerSummary{foreignView}, list...), nil
			}
		}
		return list, nil
	}

	_, err := fx.run(t)
	wantCheckFailure(t, err, CheckTeardown)
	fx.rt.mu.Lock()
	defer fx.rt.mu.Unlock()
	if _, ok := fx.rt.ctrs[names.Agent]; !ok {
		t.Error("teardown deleted a container after a contradictory duplicate-id listing")
	}
}

// TestHandoffAmbiguousCreateDuplicateVolumeNameFailsTeardown is the volume
// sibling: deletion is by name, so a labeled row cannot authorize deletion
// while a contradictory row presents the same identity.
func TestHandoffAmbiguousCreateDuplicateVolumeNameFailsTeardown(t *testing.T) {
	fx := newHandoffFixture(t)
	names := namesFor(testHandoffSpec().RunID)
	fx.rt.createVolumeThenFail = true
	fx.rt.onListVolumes = func(list []VolumeSummary) ([]VolumeSummary, error) {
		for _, v := range list {
			if v.Name == names.Workspace {
				foreignView := VolumeSummary{
					Name: names.Workspace, Labels: runLabels(testHandoffSpec().RunID), LabelsObserved: true,
				}
				return append([]VolumeSummary{foreignView}, list...), nil
			}
		}
		return list, nil
	}

	_, err := fx.run(t)
	wantCheckFailure(t, err, CheckTeardown)
	fx.rt.mu.Lock()
	defer fx.rt.mu.Unlock()
	if _, ok := fx.rt.vols[names.Workspace]; !ok {
		t.Error("teardown deleted a volume after a contradictory duplicate-name listing")
	}
}

// TestHandoffUnrelatedDuplicateSummariesDoNotBlockTeardown proves identity
// contradictions are scoped to the object being classified. A malformed
// unrelated pair cannot suppress cleanup of the run's owned objects.
func TestHandoffUnrelatedDuplicateSummariesDoNotBlockTeardown(t *testing.T) {
	fx := newHandoffFixture(t)
	fx.rt.onListContainers = func(list []ContainerSummary) ([]ContainerSummary, error) {
		duplicate := ContainerSummary{ID: "unrelated", State: StateStopped}
		return append(list, duplicate, duplicate), nil
	}
	fx.rt.onListVolumes = func(list []VolumeSummary) ([]VolumeSummary, error) {
		duplicate := VolumeSummary{Name: "unrelated"}
		return append(list, duplicate, duplicate), nil
	}

	if _, err := fx.run(t); err != nil {
		t.Fatalf("Handoff = %v, want success", err)
	}
	fx.assertReaped(t)
}

// TestHandoffListContainersError: check 3's absence proof fails closed when
// the runtime cannot be listed, rather than trusting the delete call.
func TestHandoffListContainersError(t *testing.T) {
	fx := newHandoffFixture(t)
	fx.rt.onListContainers = func([]ContainerSummary) ([]ContainerSummary, error) {
		return nil, errors.New("apiserver down")
	}
	_, err := fx.run(t)
	wantCheckFailure(t, err, CheckWriterTermination)
}

// TestHandoffAgentLingersTeardownReaps: when the agent delete reports
// success but the container is still listed (a lying runtime, the exact
// case check 3 catches), the credential-bearing agent must not leak: the
// liveness flag stays set until absence is proven, so teardown re-attempts
// to reap it rather than trusting the delete.
func TestHandoffAgentLingersTeardownReaps(t *testing.T) {
	fx := newHandoffFixture(t)
	names := namesFor(testHandoffSpec().RunID)
	fx.rt.onDeleteContainer = func(id string) (bool, error) {
		if id == names.Agent {
			return true, nil // report success, leave the container
		}
		return false, nil
	}
	_, err := fx.run(t)
	wantCheckFailure(t, err, CheckWriterTermination)

	deletes := 0
	fx.rt.mu.Lock()
	for _, c := range fx.rt.calls {
		if c == "delete-container "+names.Agent {
			deletes++
		}
	}
	fx.rt.mu.Unlock()
	if deletes < 2 {
		t.Errorf("agent delete attempted %d times; teardown did not reap the lingering credential-bearing agent", deletes)
	}
}

// TestHandoffExporterAllowlistViolation is acceptance 2 for check 4 through
// the full lifecycle: the runtime reports an extra mount on the exporter,
// and the gate fails before the exporter ever executes.
func TestHandoffExporterAllowlistViolation(t *testing.T) {
	fx := newHandoffFixture(t)
	names := namesFor(testHandoffSpec().RunID)
	fx.rt.onInspect = func(id string, rep InspectReport) (InspectReport, error) {
		if id == names.Exporter {
			rep.Mounts = append(rep.Mounts, Mount{
				Type: MountVolume, Source: "provider-cred", Target: "/credentials", ReadOnly: true,
			})
		}
		return rep, nil
	}
	_, err := fx.run(t)
	wantCheckFailure(t, err, CheckExporterAllowlist)
	if i := fx.rt.callIndex("start-container " + names.Exporter); i >= 0 {
		t.Error("exporter was started despite a failed pre-execution inspection")
	}
	fx.assertReaped(t)
}

// TestHandoffExporterPayloadMismatch proves check 4 binds approval to the
// runtime-observed image and argv, not only the mounts around the helper the
// gate requested. A substituted helper never starts.
func TestHandoffExporterPayloadMismatch(t *testing.T) {
	names := namesFor(testHandoffSpec().RunID)
	cases := []struct {
		name   string
		mutate func(*InspectReport)
	}{
		{"image digest", func(rep *InspectReport) { rep.ImageDigest = "sha256:" + strings.Repeat("1", 64) }},
		{"command", func(rep *InspectReport) { rep.Command = []string{"/bin/other"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newHandoffFixture(t)
			fx.rt.onInspect = func(id string, rep InspectReport) (InspectReport, error) {
				if id == names.Exporter {
					tc.mutate(&rep)
				}
				return rep, nil
			}
			_, err := fx.run(t)
			wantCheckFailure(t, err, CheckExporterAllowlist)
			if i := fx.rt.callIndex("start-container " + names.Exporter); i >= 0 {
				t.Error("exporter was started despite a mismatched inspected payload")
			}
			fx.assertReaped(t)
		})
	}
}

// TestHandoffExporterNeverStops: an exporter that hangs exhausts its budget
// and fails the export check.
func TestHandoffExporterNeverStops(t *testing.T) {
	fx := newHandoffFixture(t)
	names := namesFor(testHandoffSpec().RunID)
	fx.rt.runningInspects[names.Exporter] = math.MaxInt - 1
	_, err := fx.run(t)
	wantCheckFailure(t, err, CheckExportVerification)
	fx.assertReaped(t)
}

// TestHandoffProofMissing is acceptance 2 for check 5 through the full
// lifecycle: an exported rootfs without the proof file fails.
func TestHandoffProofMissing(t *testing.T) {
	fx := newHandoffFixture(t)
	entries := fixtureArchive(t)
	fx.rt.exportTarPath = buildTar(t, append(entries[:3:3], entries[4:]...))
	_, err := fx.run(t)
	wantCheckFailure(t, err, CheckInExporterVerification)
	fx.assertReaped(t)
}

// TestHandoffScannerRefusal is acceptance 2 for check 7 through the full
// lifecycle.
func TestHandoffScannerRefusal(t *testing.T) {
	fx := newHandoffFixture(t)
	fx.cfg.Scanner = scannerFunc(func(context.Context, string) error {
		return errors.New("marker found")
	})
	res, err := fx.run(t)
	wantCheckFailure(t, err, CheckExportVerification)
	if res != nil {
		t.Error("refused export still released a result")
	}
	fx.assertReaped(t)
}

// TestHandoffPanicRemovesOutput proves a panic after the output dir is created
// (for example a typed-nil scanner that panics when called) still removes the
// unscanned output on unwind: the deferred cleanup keys off an explicit success
// flag, not the named err, which is still nil during a panic.
func TestHandoffPanicRemovesOutput(t *testing.T) {
	fx := newHandoffFixture(t)
	runID := testHandoffSpec().RunID
	fx.cfg.Scanner = scannerFunc(func(context.Context, string) error {
		panic("scanner boom")
	})
	before := scratchDirs(t, runID)
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("panicking scanner did not propagate a panic")
			}
		}()
		_, _ = fx.backend(t).Handoff(context.Background(), testHandoffSpec())
	}()
	if after := scratchDirs(t, runID); len(after) > len(before) {
		t.Errorf("panic unwind leaked unscanned output dir(s): %v", after)
	}
	fx.assertReaped(t)
}

// TestHandoffTeardownFailure: everything passes but the workspace volume
// cannot be deleted; the gate still fails, no result is released, and the
// verified output dir is cleaned (the caller gets nil and cannot own it).
func TestHandoffTeardownFailure(t *testing.T) {
	fx := newHandoffFixture(t)
	runID := testHandoffSpec().RunID
	fx.rt.onDeleteVolume = func(string) (bool, error) {
		return true, errors.New("volume busy")
	}
	before := scratchDirs(t, runID)
	res, err := fx.run(t)
	wantCheckFailure(t, err, CheckTeardown)
	if res != nil {
		t.Error("teardown failure still released a result")
	}
	if after := scratchDirs(t, runID); len(after) > len(before) {
		t.Errorf("teardown failure after a good export leaked output dir(s): %v", after)
	}
}

// TestHandoffVolumeSurvives: a delete call that silently does nothing is
// caught by the labeled-volume sweep.
func TestHandoffVolumeSurvives(t *testing.T) {
	fx := newHandoffFixture(t)
	fx.rt.onDeleteVolume = func(string) (bool, error) {
		return true, nil // pretend success, leave the volume
	}
	res, err := fx.run(t)
	wantCheckFailure(t, err, CheckTeardown)
	if res != nil {
		t.Error("survived volume still released a result")
	}
}

// TestHandoffContainerSurvives: a container delete that reports success but
// leaves the container is caught by teardown's re-listing sweep, mirroring
// the volume case. Teardown proves absence, never trusting the delete call.
func TestHandoffContainerSurvives(t *testing.T) {
	fx := newHandoffFixture(t)
	names := namesFor(testHandoffSpec().RunID)
	// The exporter delete lies: it reports success but leaves the container.
	fx.rt.onDeleteContainer = func(id string) (bool, error) {
		return id == names.Exporter, nil
	}
	res, err := fx.run(t)
	wantCheckFailure(t, err, CheckTeardown)
	if res != nil {
		t.Error("survived container still released a result")
	}
}

// TestHandoffWorkspaceVolumeSurvivesUnlabeled: a workspace volume that
// survives teardown with its label dropped is still flagged by name, so an
// unlabeled survivor holding agent-written data cannot pass as reaped.
func TestHandoffWorkspaceVolumeSurvivesUnlabeled(t *testing.T) {
	fx := newHandoffFixture(t)
	names := namesFor(testHandoffSpec().RunID)
	fx.rt.onDeleteVolume = func(string) (bool, error) {
		return true, nil // report success, leave the volume
	}
	fx.rt.onListVolumes = func(list []VolumeSummary) ([]VolumeSummary, error) {
		// The workspace volume survives but with no labels.
		return []VolumeSummary{{Name: names.Workspace}}, nil
	}
	res, err := fx.run(t)
	wantCheckFailure(t, err, CheckTeardown)
	if res != nil {
		t.Error("unlabeled surviving workspace volume still released a result")
	}
}

// TestHandoffPreservesCredentialVolumeWithRunLabel proves labels are not
// ownership evidence. A provisioner may label every resource for a run,
// including caller-owned credential volumes; teardown must still delete only
// the workspace volume the gate created.
func TestHandoffPreservesCredentialVolumeWithRunLabel(t *testing.T) {
	fx := newHandoffFixture(t)
	hs := testHandoffSpec()
	names := namesFor(hs.RunID)
	credentialVolume := hs.Agent.CredentialMounts[0].Volume
	fx.rt.vols[credentialVolume] = &fakeVol{labels: runLabels(hs.RunID), created: "caller-created"}

	if _, err := fx.run(t); err != nil {
		t.Fatalf("Handoff = %v, want success", err)
	}

	fx.rt.mu.Lock()
	defer fx.rt.mu.Unlock()
	if _, ok := fx.rt.vols[credentialVolume]; !ok {
		t.Error("teardown deleted caller-owned credential volume sharing the run label")
	}
	if _, ok := fx.rt.vols[names.Workspace]; ok {
		t.Error("workspace volume survived teardown")
	}
	for _, call := range fx.rt.calls {
		if call == "delete-volume "+credentialVolume {
			t.Errorf("teardown attempted to delete caller-owned credential volume: %q", call)
		}
	}
}

// TestHandoffTeardownListVolumesError: teardown fails closed when it cannot
// list volumes to prove nothing was left behind.
func TestHandoffTeardownListVolumesError(t *testing.T) {
	fx := newHandoffFixture(t)
	fx.rt.onListVolumes = func([]VolumeSummary) ([]VolumeSummary, error) {
		return nil, errors.New("apiserver down")
	}
	res, err := fx.run(t)
	wantCheckFailure(t, err, CheckTeardown)
	if res != nil {
		t.Error("unverifiable teardown still released a result")
	}
}

// TestHandoffScratchDirCleaned: a failed run leaves no scratch directory in
// the host temp dir. The scratch dir holds the raw exporter rootfs archive
// and extracted output, plausibly the very credential a refused scan
// withheld; it must not persist.
func TestHandoffScratchDirCleaned(t *testing.T) {
	fx := newHandoffFixture(t)
	fx.cfg.Scanner = scannerFunc(func(context.Context, string) error {
		return errors.New("marker found")
	})
	runID := testHandoffSpec().RunID
	before := scratchDirs(t, runID)
	_, err := fx.run(t)
	wantCheckFailure(t, err, CheckExportVerification)
	if after := scratchDirs(t, runID); len(after) > len(before) {
		t.Errorf("failed run leaked scratch dir(s): before %v, after %v", before, after)
	}
}

// scratchDirs lists this run's handoff scratch directories in the host temp
// dir (os.MkdirTemp names them "freeside-handoff-<runID>-{tar,out}-*").
func scratchDirs(t *testing.T, runID string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "freeside-handoff-"+runID+"-*"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

// TestHandoffPrimaryErrorRetainedWithTeardownFailure proves incomplete
// cleanup is surfaced without masking the primary check failure.
func TestHandoffPrimaryErrorRetainedWithTeardownFailure(t *testing.T) {
	fx := newHandoffFixture(t)
	names := namesFor(testHandoffSpec().RunID)
	fx.rt.runningInspects[names.Agent] = math.MaxInt - 1
	fx.rt.onDeleteVolume = func(string) (bool, error) {
		return true, errors.New("volume busy")
	}
	_, err := fx.run(t)
	wantCheckFailure(t, err, CheckWriterTermination)
	if !strings.Contains(err.Error(), string(CheckTeardown)) {
		t.Errorf("joined error omitted teardown failure: %v", err)
	}
}

// TestHandoffTeardownBounded proves teardown runs under its own deadline: a
// runtime call that blocks past TeardownTimeout still lets Handoff return
// (as a teardown failure) rather than hanging, even though teardown is
// detached from the caller's cancellation.
func TestHandoffTeardownBounded(t *testing.T) {
	fx := newHandoffFixture(t)
	fx.cfg.TeardownTimeout = 50 * time.Millisecond
	// The exporter delete (reached only in teardown) blocks until its own
	// context is done, modeling a wedged runtime call.
	fx.rt.blockDelete = namesFor(testHandoffSpec().RunID).Exporter

	done := make(chan error, 1)
	go func() {
		_, err := fx.backend(t).Handoff(context.Background(), testHandoffSpec())
		done <- err
	}()
	select {
	case err := <-done:
		wantCheckFailure(t, err, CheckTeardown)
	case <-time.After(10 * time.Second):
		t.Fatal("Handoff did not return; teardown was not bounded")
	}
}

// TestHandoffNoReapBeforeClaim: a failure before the first create (here an
// invalid credential mount, caught by validateAgentSpec) must not let
// teardown reap by name, since this invocation created nothing and the names
// could belong to another live run sharing the RunID.
func TestHandoffNoReapBeforeClaim(t *testing.T) {
	fx := newHandoffFixture(t)
	names := namesFor(testHandoffSpec().RunID)
	// Simulate another live run already owning these names.
	fx.rt.ctrs[names.Agent] = &fakeCtr{started: true, created: "foreign-created-agent"}
	fx.rt.vols[names.Workspace] = &fakeVol{labels: runLabels(testHandoffSpec().RunID), created: "foreign-created-ws"}

	spec := testHandoffSpec()
	// A relative credential target passes HandoffSpec.validate but fails
	// validateAgentSpec, which runs before anything is created.
	spec.Agent.CredentialMounts = []CredentialMount{{Volume: "cred", Target: "relative"}}
	_, err := fx.backend(t).Handoff(context.Background(), spec)
	if !errors.Is(err, ErrConformance) {
		t.Fatalf("Handoff = %v, want a conformance failure before any create", err)
	}

	fx.rt.mu.Lock()
	defer fx.rt.mu.Unlock()
	if _, ok := fx.rt.ctrs[names.Agent]; !ok {
		t.Error("teardown reaped another run's container despite creating nothing")
	}
	if _, ok := fx.rt.vols[names.Workspace]; !ok {
		t.Error("teardown reaped another run's volume despite creating nothing")
	}
	// It must not have even listed/deleted (no reap attempt at all).
	for _, c := range fx.rt.calls {
		if c == "delete-container "+names.Agent || c == "delete-volume "+names.Workspace {
			t.Errorf("teardown attempted a reap before the run claimed its names: %q", c)
		}
	}
}

// TestHandoffVolumeCollisionDoesNotClaimNames proves an ordinary
// already-exists failure does not authorize teardown. The colliding objects
// lack this invocation's unpredictable workspace ownership label and all
// belong to another live run.
func TestHandoffVolumeCollisionDoesNotClaimNames(t *testing.T) {
	fx := newHandoffFixture(t)
	hs := testHandoffSpec()
	names := namesFor(hs.RunID)
	fx.rt.vols[names.Workspace] = &fakeVol{labels: runLabels(hs.RunID), created: "foreign-created-ws"}
	fx.rt.ctrs[names.Agent] = &fakeCtr{started: true, created: "foreign-created-agent"}
	fx.rt.ctrs[names.Exporter] = &fakeCtr{created: "foreign-created-exporter"}

	if _, err := fx.backend(t).Handoff(context.Background(), hs); err == nil {
		t.Fatal("workspace collision returned success")
	}

	fx.rt.mu.Lock()
	defer fx.rt.mu.Unlock()
	if _, ok := fx.rt.vols[names.Workspace]; !ok {
		t.Error("teardown deleted another run's colliding workspace")
	}
	if _, ok := fx.rt.ctrs[names.Agent]; !ok {
		t.Error("teardown deleted another run's colliding agent")
	}
	if _, ok := fx.rt.ctrs[names.Exporter]; !ok {
		t.Error("teardown deleted another run's colliding exporter")
	}
	for _, call := range fx.rt.calls {
		if strings.HasPrefix(call, "delete-") || strings.HasPrefix(call, "stop-") {
			t.Errorf("teardown attempted to reap a colliding run: %q", call)
		}
	}
}

// TestHandoffContainerCollisionsDoNotClaimNames closes the per-object sibling
// class: owning the workspace does not confer ownership of an independently
// colliding agent or exporter name. A fresh listing without this invocation's
// ownership label leaves the foreign container untouched.
func TestHandoffContainerCollisionsDoNotClaimNames(t *testing.T) {
	names := namesFor(testHandoffSpec().RunID)
	for _, id := range []string{names.Agent, names.Exporter} {
		t.Run(id, func(t *testing.T) {
			fx := newHandoffFixture(t)
			foreign := &fakeCtr{spec: ContainerSpec{Labels: runLabels(testHandoffSpec().RunID)}}
			if id == names.Agent {
				foreign.started = true
			}
			fx.rt.ctrs[id] = foreign

			if _, err := fx.run(t); err == nil {
				t.Fatal("container collision returned success")
			}

			fx.rt.mu.Lock()
			defer fx.rt.mu.Unlock()
			if _, ok := fx.rt.ctrs[id]; !ok {
				t.Errorf("teardown deleted foreign colliding container %q", id)
			}
			if len(fx.rt.vols) != 0 {
				t.Errorf("owned workspace survived failed handoff: %v", fx.rt.vols)
			}
			for _, call := range fx.rt.calls {
				if call == "delete-container "+id || call == "stop-container "+id {
					t.Errorf("teardown attempted to reap foreign colliding container: %q", call)
				}
			}
		})
	}
}

// TestHandoffAgentProvenAbsentIsNotReapedAgain pins the proven-absent state.
// Once the owned agent was deleted and absent from the full listing, a
// foreign same-name container appearing later must not inherit teardown
// authority from the completed create.
func TestHandoffAgentProvenAbsentIsNotReapedAgain(t *testing.T) {
	fx := newHandoffFixture(t)
	names := namesFor(testHandoffSpec().RunID)
	fx.rt.onCreateContainer = func(spec ContainerSpec) error {
		if spec.Name == names.Exporter {
			fx.rt.ctrs[names.Agent] = &fakeCtr{
				spec:    ContainerSpec{Labels: runLabels(testHandoffSpec().RunID)},
				started: true,
			}
		}
		return nil
	}

	if _, err := fx.run(t); err != nil {
		t.Fatalf("Handoff = %v, want success", err)
	}

	fx.rt.mu.Lock()
	defer fx.rt.mu.Unlock()
	if _, ok := fx.rt.ctrs[names.Agent]; !ok {
		t.Error("teardown reaped a foreign agent after the owned agent was proven absent")
	}
	deletes := 0
	for _, call := range fx.rt.calls {
		if call == "delete-container "+names.Agent {
			deletes++
		}
	}
	if deletes != 1 {
		t.Errorf("agent delete attempts = %d, want exactly the owned agent's delete", deletes)
	}
	delete(fx.rt.ctrs, names.Agent)
}

// TestHandoffOwnedContainerReapedWhenListFails proves an unrelated malformed
// list row cannot leave a successfully-created, credential-mounted agent
// restartable. The list error still fails teardown, but exact owned cleanup
// falls back to inspect, stop, and delete before the absence re-list.
func TestHandoffOwnedContainerReapedWhenListFails(t *testing.T) {
	names := namesFor(testHandoffSpec().RunID)
	cases := []struct {
		name               string
		containerID        string
		failListCall       int
		unknownState       bool
		stopErrorAfterStop bool
	}{
		{name: "agent unknown state", containerID: names.Agent, failListCall: 1, unknownState: true},
		// The agent's ordinary absence proof is the first list call; the
		// exporter teardown listing is the second.
		{name: "exporter unknown state", containerID: names.Exporter, failListCall: 2, unknownState: true},
		{name: "stop errors after stopping", containerID: names.Agent, failListCall: 1, stopErrorAfterStop: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newHandoffFixture(t)
			fx.rt.runningInspects[tc.containerID] = math.MaxInt - 1
			listCalls := 0
			fx.rt.onListContainers = func(list []ContainerSummary) ([]ContainerSummary, error) {
				listCalls++
				if listCalls == tc.failListCall {
					return nil, errors.New("unrelated malformed list row")
				}
				return list, nil
			}
			fx.rt.onInspect = func(id string, rep InspectReport) (InspectReport, error) {
				if tc.unknownState && id == tc.containerID && listCalls >= tc.failListCall {
					rep.State = ContainerState("unknown")
				}
				return rep, nil
			}
			fx.rt.onStop = func(id string) error {
				if !tc.stopErrorAfterStop || id != tc.containerID {
					return nil
				}
				fx.rt.ctrs[id].stopped = true
				return errors.New("stop response lost after effect")
			}

			_, err := fx.run(t)
			if err == nil || !strings.Contains(err.Error(), string(CheckTeardown)) {
				t.Fatalf("Handoff = %v, want joined teardown failure", err)
			}
			fx.assertReaped(t)
			fx.rt.mu.Lock()
			defer fx.rt.mu.Unlock()
			wantCalls := []string{
				"inspect " + tc.containerID,
				"stop-container " + tc.containerID,
				"delete-container " + tc.containerID,
				"list-containers",
			}
			next := 0
			for _, call := range fx.rt.calls {
				if next < len(wantCalls) && call == wantCalls[next] {
					next++
				}
			}
			if next != len(wantCalls) {
				t.Errorf("runtime calls %v do not contain ordered cleanup sequence %v", fx.rt.calls, wantCalls)
			}
		})
	}
}

// TestHandoffAmbiguousContainerReapedWhenListFails proves that when the full
// container list is unavailable (an unrelated malformed row), an ambiguous
// create is still reaped by exact name once a direct inspect proves this
// invocation's ownership label. Without it, a broken sibling row could leave
// the credential-mounted writer restartable.
func TestHandoffAmbiguousContainerReapedWhenListFails(t *testing.T) {
	names := namesFor(testHandoffSpec().RunID)
	cases := []struct {
		id           string
		failListCall int
	}{
		// The agent's ambiguous create fails before any gate listing, so the
		// teardown reap list is the first. The exporter's create follows the
		// writer-absence listing, so the reap list is the second.
		{id: names.Agent, failListCall: 1},
		{id: names.Exporter, failListCall: 2},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			fx := newHandoffFixture(t)
			fx.rt.createThenFail = tc.id
			listCalls := 0
			fx.rt.onListContainers = func(list []ContainerSummary) ([]ContainerSummary, error) {
				listCalls++
				if listCalls == tc.failListCall {
					return nil, errors.New("unrelated malformed list row")
				}
				return list, nil
			}

			_, err := fx.run(t)
			if err == nil || !strings.Contains(err.Error(), string(CheckTeardown)) {
				t.Fatalf("Handoff = %v, want joined teardown failure", err)
			}
			fx.assertReaped(t)
			fx.rt.mu.Lock()
			defer fx.rt.mu.Unlock()
			wantCalls := []string{"inspect " + tc.id, "delete-container " + tc.id, "list-containers"}
			next := 0
			for _, call := range fx.rt.calls {
				if next < len(wantCalls) && call == wantCalls[next] {
					next++
				}
			}
			if next != len(wantCalls) {
				t.Errorf("runtime calls %v do not contain ordered cleanup sequence %v", fx.rt.calls, wantCalls)
			}
		})
	}
}

// TestHandoffAmbiguousContainerLeftWhenListFailsAndUnowned proves the exact
// counterweight: with the full list unavailable, an ambiguous create whose
// direct inspect does not carry this invocation's ownership label is a possible
// foreign same-name object and is left untouched, never deleted by name alone.
func TestHandoffAmbiguousContainerLeftWhenListFailsAndUnowned(t *testing.T) {
	fx := newHandoffFixture(t)
	names := namesFor(testHandoffSpec().RunID)
	fx.rt.createThenFail = names.Agent
	listCalls := 0
	fx.rt.onListContainers = func(list []ContainerSummary) ([]ContainerSummary, error) {
		listCalls++
		if listCalls == 1 {
			return nil, errors.New("unrelated malformed list row")
		}
		// Strip our label from every later view so the object reads as foreign
		// through both the list and the inspect fallback.
		for i := range list {
			if list[i].ID == names.Agent {
				list[i].Labels = nil
			}
		}
		return list, nil
	}
	fx.rt.onInspect = func(id string, rep InspectReport) (InspectReport, error) {
		if id == names.Agent {
			rep.Labels = nil // labels observed, but the ownership token absent
		}
		return rep, nil
	}

	_, err := fx.run(t)
	if err == nil || !strings.Contains(err.Error(), string(CheckTeardown)) {
		t.Fatalf("Handoff = %v, want teardown failure from the list error", err)
	}
	fx.rt.mu.Lock()
	defer fx.rt.mu.Unlock()
	if _, ok := fx.rt.ctrs[names.Agent]; !ok {
		t.Error("teardown deleted an unowned same-name container after a list failure")
	}
	for _, call := range fx.rt.calls {
		if call == "delete-container "+names.Agent {
			t.Errorf("teardown issued %q for an object without the ownership label", call)
		}
	}
}

// TestHandoffAgentOwnershipDowngradedAfterDeleteUncertainty proves that once
// the gate's own DeleteContainer succeeds but absence cannot be proven (the
// writer-absence list errors on an unrelated row), the agent claim is
// downgraded to label-gated: if a foreign actor recycles the deterministic
// agent name before teardown, teardown must not reap that same-name stranger by
// identity alone. Without the downgrade, teardown reaps it label-free.
func TestHandoffAgentOwnershipDowngradedAfterDeleteUncertainty(t *testing.T) {
	fx := newHandoffFixture(t)
	names := namesFor(testHandoffSpec().RunID)
	// A recycled object under the deterministic name, lacking this invocation's
	// ownership label (labels observed, none match).
	foreign := ContainerSummary{ID: names.Agent, State: StateStopped, LabelsObserved: true, Labels: nil}
	listCalls := 0
	fx.rt.onListContainers = func(list []ContainerSummary) ([]ContainerSummary, error) {
		listCalls++
		if listCalls == 1 {
			// Writer-absence verify: our delete already succeeded, but the full
			// list errors, so absence cannot be proven this call.
			return nil, errors.New("unrelated malformed list row")
		}
		return append(list, foreign), nil
	}

	_, err := fx.run(t)
	wantCheckFailure(t, err, CheckWriterTermination)
	fx.rt.mu.Lock()
	defer fx.rt.mu.Unlock()
	deletes := 0
	for _, call := range fx.rt.calls {
		if call == "delete-container "+names.Agent {
			deletes++
		}
	}
	if deletes != 1 {
		t.Errorf("agent delete-container calls = %d, want 1 (only the gate's own delete; teardown must not reap the recycled foreign name): %v", deletes, fx.rt.calls)
	}
}

// TestHandoffBoundsWedgedWriterStart proves the overall handoff budget bounds a
// runtime that wedges launching the credential VM: StartContainer never
// returns, but the deadline cuts it, Handoff fails, and deferred teardown
// (detached from the budget) still reaps the agent, so the VM cannot stay live
// indefinitely.
func TestHandoffBoundsWedgedWriterStart(t *testing.T) {
	fx := newHandoffFixture(t)
	names := namesFor(testHandoffSpec().RunID)
	fx.cfg.HandoffTimeout = 100 * time.Millisecond
	fx.rt.blockStart = names.Agent

	_, err := fx.run(t)
	if err == nil {
		t.Fatal("wedged writer start returned success")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want context.DeadlineExceeded from the handoff budget", err)
	}
	fx.assertReaped(t)
}

func TestHandoffOwnedWorkspaceReapedWhenListFails(t *testing.T) {
	fx := newHandoffFixture(t)
	names := namesFor(testHandoffSpec().RunID)
	fx.rt.runningInspects[names.Agent] = math.MaxInt - 1
	listCalls := 0
	fx.rt.onListVolumes = func(list []VolumeSummary) ([]VolumeSummary, error) {
		listCalls++
		if listCalls == 1 {
			return nil, errors.New("unrelated malformed volume row")
		}
		return list, nil
	}

	_, err := fx.run(t)
	if err == nil || !strings.Contains(err.Error(), string(CheckTeardown)) {
		t.Fatalf("Handoff = %v, want joined teardown failure", err)
	}
	fx.assertReaped(t)
	fx.rt.mu.Lock()
	defer fx.rt.mu.Unlock()
	wantDelete := "delete-volume " + names.Workspace
	for _, call := range fx.rt.calls {
		if call == wantDelete {
			return
		}
	}
	t.Errorf("runtime calls %v do not contain %q", fx.rt.calls, wantDelete)
}

func TestHandoffInvalidSpec(t *testing.T) {
	fx := newHandoffFixture(t)
	spec := testHandoffSpec()
	spec.RunID = "NOT-VALID"
	_, err := fx.backend(t).Handoff(context.Background(), spec)
	if !errors.Is(err, ErrInvalidHandoffSpec) {
		t.Fatalf("Handoff = %v, want ErrInvalidHandoffSpec", err)
	}
	fx.rt.mu.Lock()
	defer fx.rt.mu.Unlock()
	if len(fx.rt.calls) != 0 {
		t.Errorf("invalid spec still touched the runtime: %v", fx.rt.calls)
	}
}

// TestHandoffCancelled: a cancelled context aborts the wait, fails the gate,
// and teardown still reaps everything (it runs detached from the caller's
// cancellation).
func TestHandoffCancelled(t *testing.T) {
	fx := newHandoffFixture(t)
	names := namesFor(testHandoffSpec().RunID)
	fx.rt.runningInspects[names.Agent] = math.MaxInt - 1
	ctx, cancel := context.WithCancel(context.Background())
	fx.cfg.Sleep = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}
	_, err := fx.backend(t).Handoff(ctx, testHandoffSpec())
	if err == nil {
		t.Fatal("cancelled handoff returned success")
	}
	fx.assertReaped(t)
}

// TestHandoffRuntimeErrorsFailClosed: representative runtime failures at
// each lifecycle step yield an error, never a partial result.
func TestHandoffRuntimeErrorsFailClosed(t *testing.T) {
	names := namesFor(testHandoffSpec().RunID)
	cases := []struct {
		name string
		set  func(fx *handoffFixture)
	}{
		{"create volume fails", func(fx *handoffFixture) {
			fx.rt.onCreateVolume = func(string) error { return errors.New("disk full") }
		}},
		{"create agent fails", func(fx *handoffFixture) {
			fx.rt.onCreateContainer = func(spec ContainerSpec) error {
				if spec.Name == names.Agent {
					return errors.New("image missing")
				}
				return nil
			}
		}},
		{"start agent fails", func(fx *handoffFixture) {
			fx.rt.onStart = func(id string) error {
				if id == names.Agent {
					return errors.New("boot failure")
				}
				return nil
			}
		}},
		{"inspect fails", func(fx *handoffFixture) {
			fx.rt.onInspect = func(string, InspectReport) (InspectReport, error) {
				return InspectReport{}, errors.New("apiserver down")
			}
		}},
		{"export fails", func(fx *handoffFixture) {
			fx.rt.onExport = func(string, io.Writer) error { return errors.New("io error") }
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newHandoffFixture(t)
			tc.set(fx)
			res, err := fx.run(t)
			if err == nil {
				t.Fatal("runtime failure returned success")
			}
			if res != nil {
				t.Error("runtime failure still released a result")
			}
		})
	}
}

// TestHandoffSleepBudget: the wait loop spends its budget in whole poll
// intervals through the injected Sleep — no wall-clock dependence. Agent:
// three running polls, so three sleeps before the stopped observation.
// Exporter: one running poll (the fake default), so one sleep.
func TestHandoffSleepBudget(t *testing.T) {
	fx := newHandoffFixture(t)
	names := namesFor(testHandoffSpec().RunID)
	fx.rt.runningInspects[names.Agent] = 3
	if _, err := fx.run(t); err != nil {
		t.Fatalf("Handoff = %v", err)
	}
	if *fx.sleeps != 4 {
		t.Errorf("sleeps = %d, want 4", *fx.sleeps)
	}
}

// TestHandoffWaitStoppedCeilingBudget: a stop timeout that is not a whole
// multiple of the poll interval must still spend its full budget. Flooring
// timeout/PollInterval drops the final partial interval (950ms/100ms yields 9
// attempts instead of 10), giving up one poll early; the agent stops on the
// tenth observation, so the run succeeds only when the attempt count rounds up.
func TestHandoffWaitStoppedCeilingBudget(t *testing.T) {
	fx := newHandoffFixture(t)
	fx.cfg.WriterStopTimeout = 950 * time.Millisecond
	names := namesFor(testHandoffSpec().RunID)
	fx.rt.runningInspects[names.Agent] = 9
	if _, err := fx.run(t); err != nil {
		t.Fatalf("Handoff with a non-multiple stop timeout = %v, want success", err)
	}
}
