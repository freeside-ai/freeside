package integration_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/macverify"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// mixedRecipe runs one command in the project image, then one on the macOS
// host executor.
var mixedRecipe = []byte(`{"commands":[["/usr/bin/true"],{"argv":["/usr/bin/true"],"room":"macos_host"}],"capture":"none"}`)

// hostExecutor stands in for the macOS verification executor: a backend of
// the executor's class and the room its admitted jobs run in.
type hostExecutor struct {
	backend *countingHostBackend
	// room counts the commands run on the executor; its runErr is the
	// executor's own failure.
	room *productionRoom
	// down, when set, is the room constructor's failure: the executor
	// refuses every job until it is cleared.
	down error
	// onRun runs inside each executor command, before it returns.
	onRun func()
	// rooms counts the room constructions, one per admitted job.
	rooms int
}

// countingHostBackend counts the gate's reads of the executor's
// configuration, so a test can show the gate was never asked.
type countingHostBackend struct {
	fake.RunnerBackend
	digestReads int
}

func (b *countingHostBackend) ConfigurationDigest() domain.Digest {
	b.digestReads++
	return b.RunnerBackend.ConfigurationDigest()
}

type hostRoom struct{ executor *hostExecutor }

func (r hostRoom) Run(ctx context.Context, workdir string, argv []string) (verify.StepResult, error) {
	if r.executor.onRun != nil {
		r.executor.onRun()
	}
	return r.executor.room.Run(ctx, workdir, argv)
}

// newHostExecutor installs a conformant executor on every engine p builds
// from now on. The harness runs unattended, so the executor needs its
// declaration and a passed record for its configuration.
func newHostExecutor(t *testing.T, p *productionPublicationHarness) *hostExecutor {
	t.Helper()
	h := &hostExecutor{
		backend: &countingHostBackend{RunnerBackend: fake.RunnerBackend{
			BackendName: string(domain.BackendCredentialFreeHostVerification),
			Caps:        exec.NewCapabilitySet(exec.CapCredentialFreeHostVerification),
		}},
		room: &productionRoom{},
	}
	record, err := domain.NewBackendConformance(domain.BackendConformanceInput{
		Backend:             domain.BackendCredentialFreeHostVerification,
		Outcome:             domain.ConformancePassed,
		ConfigurationDigest: h.backend.RunnerBackend.ConfigurationDigest(),
		Capabilities:        domain.NewCapabilitySnapshot(domain.CapCredentialFreeHostVerification),
		ProvedAt:            fakePublicationTime,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.store.WriteInternal(p.ctx, func(tx *store.InternalTx) error {
		_, err := tx.RecordBackendConformance(p.ctx, record)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	p.engineOptions = append(p.engineOptions, engine.WithHostVerificationAdmission(h.backend,
		func() (verify.Room, error) {
			h.rooms++
			if h.down != nil {
				return nil, h.down
			}
			return hostRoom{executor: h}, nil
		}))
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	return h
}

func newMixedRecipeHarness(t *testing.T) *productionPublicationHarness {
	t.Helper()
	h := newPublicationHarnessWithBaseFiles(t, mixedRecipe, productionBaseManifests())
	return newProductionPublicationHarnessFromBase(t, h, "", nil, nil, nil)
}

// readyReportRooms returns the room each step of the ready item's
// verification report names.
func readyReportRooms(t *testing.T, p *productionPublicationHarness) []verify.ExecutionClass {
	t.Helper()
	for _, evidence := range p.currentReadyItem(t).EvidenceSnapshot {
		reader, err := p.blobs.Open(evidence.Digest)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(reader)
		_ = reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		report, err := verify.ParseReport(raw)
		if err != nil {
			continue
		}
		rooms := make([]verify.ExecutionClass, len(report.Steps))
		for i, step := range report.Steps {
			rooms[i] = step.Room
		}
		return rooms
	}
	t.Fatal("ready item carries no verification report")
	return nil
}

// TestMixedRecipeRunsEachCommandInItsRoom: a recipe naming both classes runs
// its project_image command in the project image and its macos_host command
// on the admitted executor, and the report names each step's room.
func TestMixedRecipeRunsEachCommandInItsRoom(t *testing.T) {
	t.Parallel()
	p := newMixedRecipeHarness(t)
	host := newHostExecutor(t, p)
	p.publishReady(t)
	if p.room.runs != 1 || host.room.runs != 1 {
		t.Fatalf("image room ran %d commands, executor %d; want one each", p.room.runs, host.room.runs)
	}
	if rooms := readyReportRooms(t, p); !slices.Equal(rooms,
		[]verify.ExecutionClass{verify.ClassProjectImage, verify.ClassMacOSHost}) {
		t.Fatalf("report rooms = %v, want project_image then macos_host", rooms)
	}
}

// TestProjectImageRecipeNeverAsksTheHostExecutor: a recipe naming only
// project_image publishes as before, even beside a configured executor, and
// the host gate is never consulted.
func TestProjectImageRecipeNeverAsksTheHostExecutor(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	host := newHostExecutor(t, p)
	p.publishReady(t)
	if host.backend.digestReads != 0 || host.rooms != 0 || host.room.runs != 0 || p.room.runs != 1 {
		t.Fatalf("gate reads = %d, executor rooms = %d, executor runs = %d, image runs = %d; want 0, 0, 0, 1",
			host.backend.digestReads, host.rooms, host.room.runs, p.room.runs)
	}
}

// TestUnadmittedHostExecutorFailsVerification: with no executor admitted, a
// recipe naming macos_host fails its first cycle's verification by name
// before any command runs. The block is the ordinary failed-verification
// block, with its rerun actions, and its evidence names the class and the
// refusal.
func TestUnadmittedHostExecutorFailsVerification(t *testing.T) {
	t.Parallel()
	p := newMixedRecipeHarness(t)
	p.startAndRecordExport(t)
	result, err := p.reconcileLanes()
	if err != nil || result.BlockedItemsCreated != 1 || result.ReadyItemsCreated != 0 ||
		result.PublicationTasksCompleted != 1 {
		t.Fatalf("refused cycle = %#v, %v", result, err)
	}
	blocked, text := p.blockedEvidenceText(t, domain.ProductionBlockedItemID(p.runID))
	if blocked.Type != domain.AttentionPublishBlocked ||
		blocked.Reason != "Verification or current policy findings blocked production publication." ||
		!slices.Equal(blocked.RequestedDecision, []domain.Action{
			domain.ActionRerunTrustEvaluation, domain.ActionInspectTrustFailure, domain.ActionStop,
		}) ||
		blocked.PublishBlock == nil || blocked.PublishBlock.TrustRule == nil ||
		*blocked.PublishBlock.TrustRule != domain.TrustRuleVerificationFailed {
		t.Fatalf("blocked item = %#v", blocked)
	}
	if !strings.HasPrefix(text, "Host verification refused: the recipe names execution class macos_host") ||
		!strings.Contains(text, "no host verification backend is configured") ||
		!strings.Contains(text, p.replay.HeadSHA) {
		t.Fatalf("evidence does not name the class and the refusal:\n%s", text)
	}
	if p.room.runs != 0 || p.room.reads != 1 {
		t.Fatalf("refused executor ran %d commands and read %d recipes, want 0 and 1", p.room.runs, p.room.reads)
	}
	if refs, prs := p.forge.Counts(); refs != 0 || prs != 0 {
		t.Fatalf("refused executor caused effects: %d refs, %d PRs", refs, prs)
	}
}

// TestHostExecutorReservation: a mixed recipe's verification job takes its
// one verification reservation before the recipe read and holds it across
// both rooms, so the executor's command runs inside it with nothing more
// reserved. The job releases it once every room proved its work absent, and
// keeps it when the executor could not prove its slot empty.
func TestHostExecutorReservation(t *testing.T) {
	t.Parallel()
	for name, absenceUnproven := range map[string]bool{"released": false, "kept": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := newMixedRecipeHarness(t)
			host := newHostExecutor(t, p)
			// The writer starts before any budget, so only verification
			// reserves.
			p.startAndRecordExport(t)
			bound := ward.LaunchBound{RuntimeVersion: ward.RuntimeNone, BudgetMiB: 1}
			job, _, err := bound.LaunchReservation("verification-probe", p.runID,
				domain.ReservationVerification, ward.DefaultLaunchSize(ward.LaunchVerification))
			if err != nil {
				t.Fatal(err)
			}
			// One job's reservation fits; a second beside it does not.
			bound.BudgetMiB = 2*job.TotalMiB() - 1
			probe := func() error {
				return p.store.Write(p.ctx, func(tx *store.WriteTx) error {
					if err := tx.RequireMachineCapacity(p.ctx, bound.BudgetMiB, job, p.now); err != nil {
						return err
					}
					return tx.ReleaseVerificationReservation(p.ctx, job.InvocationID, p.now)
				})
			}
			var duringRun, afterVerification error
			host.onRun = func() { duringRun = probe() }
			if absenceUnproven {
				host.room.runErr = fmt.Errorf("slot teardown: %w", macverify.ErrAbsenceUnproven)
			}
			p.workflow = p.newEngineForMode(t, productionCrashSeams{
				afterVerification: func() error {
					afterVerification = probe()
					return nil
				},
			}, true, map[domain.Digest]bool{p.recipeD: true}, domain.ModeUnattended, false,
				engine.WithMachineCapacity(bound))
			if _, err := p.reconcileLanes(); err != nil {
				t.Fatalf("cycle: %v", err)
			}
			if host.room.runs != 1 || p.room.runs != 1 {
				t.Fatalf("image room ran %d commands, executor %d; want one each", p.room.runs, host.room.runs)
			}
			if !errors.Is(duringRun, domain.ErrMachineCapacity) {
				t.Fatalf("reservation during the executor's command = %v, want the job's held", duringRun)
			}
			if !absenceUnproven {
				if afterVerification != nil {
					t.Fatalf("reservation after verification = %v, want the job's released", afterVerification)
				}
				return
			}
			// The unproven slot keeps the job's reservation and the task
			// pending for a later pass.
			if err := probe(); !errors.Is(err, domain.ErrMachineCapacity) {
				t.Fatalf("reservation after an unproven slot = %v, want the job's kept", err)
			}
			if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
				pending, err := tx.ListPendingOutbox(p.ctx, engine.KindProductionPublicationRequested)
				if err == nil && len(pending) != 1 {
					t.Errorf("pending publication tasks = %d, want the one waiting", len(pending))
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestRemediationRoundSelectsRoomsThroughTheSameHelper: a remediation round's
// verification runs its macos_host command on the executor as the first
// cycle's does.
func TestRemediationRoundSelectsRoomsThroughTheSameHelper(t *testing.T) {
	t.Parallel()
	p := newMixedRecipeHarness(t)
	host := newHostExecutor(t, p)
	testProductionAdjudicatedRemediation(t, p, false, false)
	if host.room.runs != 2 {
		t.Fatalf("executor ran %d commands, want one per verified head", host.room.runs)
	}
}

// TestReadinessReentrySelectsRoomsThroughTheSameHelper: a re-entry verifies
// its mixed recipe in both rooms; with the executor refusing, the cycle stops
// on a reason naming the class, and no command runs.
func TestReadinessReentrySelectsRoomsThroughTheSameHelper(t *testing.T) {
	t.Parallel()
	for name, refuse := range map[string]bool{"admitted": false, "refused": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := newMixedRecipeHarness(t)
			host := newHostExecutor(t, p)
			p.publishReady(t)
			head := p.pushToPullRequest(t, "FOLLOWUP.md", "pushed after readiness\n")
			superseded, started := p.invalidateReady(t, domain.ReadinessInvalidation{
				Reason: domain.ReadinessInvalidationHeadChanged, Bound: p.replay.HeadSHA, Observed: head,
			})
			if !started {
				t.Fatal("a head change started no re-entry")
			}
			if refuse {
				host.down = errors.New("executor root is gone")
			}
			p.scriptCleanReview(2, p.baseSHA, head)
			result, err := p.reconcileLanes()
			if !refuse {
				if err != nil || result.ReadyItemsCreated != 1 {
					t.Fatalf("re-entry result = %#v, %v", result, err)
				}
				p.assertReentered(t, superseded, 2, p.baseSHA, head)
				if p.room.runs != 2 || host.room.runs != 2 {
					t.Fatalf("image room ran %d commands, executor %d; want two each", p.room.runs, host.room.runs)
				}
				return
			}
			if err != nil || result.BlockedItemsCreated != 1 || result.ReadyItemsCreated != 0 {
				t.Fatalf("refused re-entry result = %#v, %v", result, err)
			}
			stop := p.reentryStopItem(t)
			if !strings.Contains(stop.Reason, "execution class macos_host") ||
				!strings.Contains(stop.Reason, "executor root is gone") ||
				!strings.Contains(stop.Reason, head) {
				t.Fatalf("stop reason = %q", stop.Reason)
			}
			if p.room.runs != 1 || host.room.runs != 1 {
				t.Fatalf("a refused re-entry ran commands: image %d, executor %d", p.room.runs, host.room.runs)
			}
		})
	}
}

// TestHandoffStopsWhenTheHostExecutorRefuses: the handoff merge check selects
// rooms through the same helper. An executor that refuses the merge's job
// stops the handoff on evidence naming the class, before any command runs.
func TestHandoffStopsWhenTheHostExecutorRefuses(t *testing.T) {
	t.Parallel()
	p := newMixedRecipeHarness(t)
	host := newHostExecutor(t, p)
	p.startAndRecordExport(t)
	tip, _ := p.advanceBaseBeforePublication(t, "UPSTREAM.md", "upstream change\n")
	// The candidate's own verification runs; the executor goes down before
	// the merge's.
	p.workflow = p.newEngine(t, productionCrashSeams{
		afterVerification: func() error {
			host.down = errors.New("executor root is gone")
			return nil
		},
	}, true)
	for range 3 {
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatal(err)
		}
		p.now = p.now.Add(time.Minute)
	}
	text := p.assertHandoffStopEvidence(t, tip, "Host verification refused: the recipe names execution class macos_host")
	if !strings.Contains(text, "executor root is gone") {
		t.Fatalf("handoff stop evidence does not carry the refusal:\n%s", text)
	}
	if host.room.runs != 1 {
		t.Fatalf("executor ran %d commands, want only the candidate's", host.room.runs)
	}
}

// hostSlotsNotices returns every attention item on p's run that reports its
// executor's slots quarantined, open or concluded.
func hostSlotsNotices(t *testing.T, p *productionPublicationHarness) []domain.AttentionItem {
	t.Helper()
	var notices []domain.AttentionItem
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		items, err := tx.ListAttentionItems(p.ctx)
		for _, item := range items {
			if item.Value.Subject.ID == domain.SubjectID(p.runID) &&
				strings.HasPrefix(string(item.Value.ID), "host-slots-quarantined-") {
				notices = append(notices, item.Value)
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return notices
}

// assertHostSlotsRetrying checks that a pass refused a slot left the task
// pending under the generic environmental hold, ready for a later pass.
func assertHostSlotsRetrying(t *testing.T, p *productionPublicationHarness) {
	t.Helper()
	assertRunHoldReason(t, p, domain.HoldPublicationEnvironment)
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		pending, err := tx.ListPendingOutbox(p.ctx, engine.KindProductionPublicationRequested)
		if err == nil && len(pending) != 1 {
			t.Errorf("pending publication tasks = %d, want the one retrying", len(pending))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// TestQuarantinedHostExecutorRaisesANotice: a job whose executor has every
// slot quarantined keeps retrying under the publication_environment hold and
// raises one execution_failure notice naming the executor and the count; a
// second pass converges on it. Once the executor serves the job again (after
// a restart, in practice), the job verifies and the notice is superseded.
func TestQuarantinedHostExecutorRaisesANotice(t *testing.T) {
	t.Parallel()
	p := newMixedRecipeHarness(t)
	host := newHostExecutor(t, p)
	host.room.runErr = &macverify.NoFreeSlotError{
		ExecutorRoot: "/var/freeside/executor", Slots: 2, Quarantined: []string{"slot1", "slot2"},
	}
	p.startAndRecordExport(t)
	for pass := range 2 {
		if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 {
			t.Fatalf("pass %d = %#v, %v; want the job retrying", pass, result, err)
		}
		assertHostSlotsRetrying(t, p)
		notices := hostSlotsNotices(t, p)
		if len(notices) != 1 {
			t.Fatalf("pass %d left %d notices, want one", pass, len(notices))
		}
		notice := notices[0]
		if notice.Status != domain.StatusOpen || notice.Type != domain.AttentionExecutionFailure ||
			!slices.Equal(notice.RequestedDecision, []domain.Action{domain.ActionStop}) ||
			!strings.Contains(notice.Reason, "/var/freeside/executor") ||
			!strings.Contains(notice.Reason, "all 2 of its 2 slots are quarantined") {
			t.Fatalf("pass %d notice = %#v", pass, notice)
		}
		p.now = p.now.Add(time.Minute)
	}
	if host.room.runs != 2 {
		t.Fatalf("executor ran %d commands, want one per pass", host.room.runs)
	}

	host.room.runErr = nil
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 1 {
		t.Fatalf("pass after the executor recovered = %#v, %v", result, err)
	}
	if rooms := readyReportRooms(t, p); !slices.Contains(rooms, verify.ClassMacOSHost) {
		t.Fatalf("report rooms = %v, want macos_host", rooms)
	}
	notices := hostSlotsNotices(t, p)
	if len(notices) != 1 || notices[0].Status == domain.StatusOpen || notices[0].Conclusion == nil ||
		notices[0].Conclusion.Cause != domain.ConcludedBySupersession {
		t.Fatalf("notices after the job verified = %#v, want the one superseded", notices)
	}
}

// TestBusyHostExecutorRetriesQuietly: a job refused because a slot is leased,
// with the rest quarantined, retries with no notice and verifies once the
// slot frees.
func TestBusyHostExecutorRetriesQuietly(t *testing.T) {
	t.Parallel()
	p := newMixedRecipeHarness(t)
	host := newHostExecutor(t, p)
	host.room.runErr = &macverify.NoFreeSlotError{
		ExecutorRoot: "/var/freeside/executor", Slots: 2, Quarantined: []string{"slot1"},
	}
	p.startAndRecordExport(t)
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 {
		t.Fatalf("busy pass = %#v, %v; want the job retrying", result, err)
	}
	assertHostSlotsRetrying(t, p)
	if notices := hostSlotsNotices(t, p); len(notices) != 0 {
		t.Fatalf("a busy executor raised %#v", notices)
	}

	host.room.runErr = nil
	p.now = p.now.Add(time.Minute)
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 1 {
		t.Fatalf("pass after the slot freed = %#v, %v", result, err)
	}
	if notices := hostSlotsNotices(t, p); len(notices) != 0 {
		t.Fatalf("a busy executor raised %#v", notices)
	}
}

// TestReentryRaisesAndRetiresTheHostSlotsNotice: a readiness re-entry, which
// verifies through the path the handoff merge check shares, raises the
// notice while every slot is quarantined and retires it once the executor
// serves the job again.
func TestReentryRaisesAndRetiresTheHostSlotsNotice(t *testing.T) {
	t.Parallel()
	p := newMixedRecipeHarness(t)
	host := newHostExecutor(t, p)
	p.publishReady(t)
	head := p.pushToPullRequest(t, "FOLLOWUP.md", "pushed after readiness\n")
	superseded, started := p.invalidateReady(t, domain.ReadinessInvalidation{
		Reason: domain.ReadinessInvalidationHeadChanged, Bound: p.replay.HeadSHA, Observed: head,
	})
	if !started {
		t.Fatal("a head change started no re-entry")
	}
	host.room.runErr = &macverify.NoFreeSlotError{
		ExecutorRoot: "/var/freeside/executor", Slots: 1, Quarantined: []string{"slot1"},
	}
	p.scriptCleanReview(2, p.baseSHA, head)
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 {
		t.Fatalf("quarantined re-entry = %#v, %v; want it retrying", result, err)
	}
	assertRunHoldReason(t, p, domain.HoldPublicationEnvironment)
	if notices := hostSlotsNotices(t, p); len(notices) != 1 || notices[0].Status != domain.StatusOpen ||
		!strings.Contains(notices[0].Reason, "all 1 of its 1 slots are quarantined") {
		t.Fatalf("notices after the quarantined re-entry = %#v, want one open", notices)
	}

	host.room.runErr = nil
	p.now = p.now.Add(time.Minute)
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 1 {
		t.Fatalf("re-entry after the executor recovered = %#v, %v", result, err)
	}
	p.assertReentered(t, superseded, 2, p.baseSHA, head)
	if notices := hostSlotsNotices(t, p); len(notices) != 1 || notices[0].Status == domain.StatusOpen {
		t.Fatalf("notices after the re-entry verified = %#v, want the one superseded", notices)
	}
}
