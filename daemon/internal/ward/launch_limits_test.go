package ward

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

// bootLogKilling is the real memory-limit boot log fixture, retargeted at the
// named container: the kernel names the killed cgroup by container id.
func bootLogKilling(t *testing.T, id string) []byte {
	t.Helper()
	return bytes.ReplaceAll(readBootLogFixture(t, "boot-log-memory-limit-kill.txt"),
		[]byte("memory-limit-fixture"), []byte(id))
}

func markerWriterSpec(fx *handoffFixture, status string) HandoffSpec {
	hs := testHandoffSpec()
	hs.Agent.OutcomeMarkerPath = fx.cfg.WorkspaceTarget + "/.freeside/evidence/writer-outcome"
	hs.Agent.Command = []string{
		"sh", "-c", "printf '%s " + status + "\\n' " + WriterNoncePlaceholder + " > " + hs.Agent.OutcomeMarkerPath,
	}
	return hs
}

func TestHandoffCreatesEveryContainerAtTheDeclaredSize(t *testing.T) {
	fx := newHandoffFixture(t)
	hs := testHandoffSpec()
	hs.Size = ContainerSize{CPUs: 6, MemoryMiB: 3072}
	var created []ContainerSpec
	fx.rt.onCreateContainer = func(spec ContainerSpec) error {
		created = append(created, spec)
		return nil
	}
	if _, err := fx.runSpec(t, hs); err != nil {
		t.Fatal(err)
	}
	if len(created) == 0 {
		t.Fatal("no container was created")
	}
	for _, spec := range created {
		if spec.Size != hs.Size {
			t.Errorf("%s created at %s, want %s", spec.Name, spec.Size, hs.Size)
		}
	}
}

func TestHandoffNamesAWriterKilledAtItsMemoryLimit(t *testing.T) {
	fx := newHandoffFixture(t)
	fx.journalled()
	var logs bytes.Buffer
	fx.cfg.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	hs := markerWriterSpec(fx, "137")
	fx.rt.writerStatus = 137
	names := namesFor(hs.RunID)
	fx.rt.bootLogs = map[string][]byte{names.Agent: bootLogKilling(t, names.Agent)}

	_, err := fx.runSpec(t, hs)
	var limit *MemoryLimitError
	if !errors.As(err, &limit) || !errors.Is(err, ErrWriterFailed) {
		t.Fatalf("Handoff error = %v, want a MemoryLimitError over ErrWriterFailed", err)
	}
	if limit.Class != LaunchWriter || limit.Size != hs.Size {
		t.Fatalf("named %s at %s, want writer at %s", limit.Class, limit.Size, hs.Size)
	}
	if got := strings.Count(logs.String(), `msg="ward launch"`); got != 1 {
		t.Fatalf("launch records = %d, want 1:\n%s", got, logs.String())
	}
	for _, want := range []string{"class=writer", "ended=memory_limit", "memory_limit_kill=true", "memory_mib=2048"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("launch record lacks %q:\n%s", want, logs.String())
		}
	}
	fx.assertReaped(t)
}

func TestHandoffDoesNotNameAnOrdinaryWriterFailure(t *testing.T) {
	fx := newHandoffFixture(t)
	fx.journalled()
	hs := markerWriterSpec(fx, "137")
	fx.rt.writerStatus = 137
	names := namesFor(hs.RunID)
	// A host SIGKILL also exits 137 and leaves no memcg report.
	fx.rt.bootLogs = map[string][]byte{names.Agent: readBootLogFixture(t, "boot-log-sigkill.txt")}

	_, err := fx.runSpec(t, hs)
	if !errors.Is(err, ErrWriterFailed) || errors.Is(err, ErrMemoryLimit) {
		t.Fatalf("Handoff error = %v, want only ErrWriterFailed", err)
	}
}

func TestHandoffRecordsAKillTheWriterSurvived(t *testing.T) {
	fx := newHandoffFixture(t)
	var logs bytes.Buffer
	fx.cfg.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	hs := testHandoffSpec()
	names := namesFor(hs.RunID)
	// A build child killed at the limit while the agent carried on to a
	// clean finish: the launch succeeds and its record keeps the kill.
	fx.rt.bootLogs = map[string][]byte{names.Agent: bootLogKilling(t, names.Agent)}
	if _, err := fx.runSpec(t, hs); err != nil {
		t.Fatalf("Handoff = %v, want success", err)
	}
	for _, want := range []string{"ended=succeeded", "memory_limit_kill=true"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("launch record lacks %q:\n%s", want, logs.String())
		}
	}
}

func TestHandoffRecordsAnUnreadableBootLog(t *testing.T) {
	fx := newHandoffFixture(t)
	var logs bytes.Buffer
	fx.cfg.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	fx.rt.bootLogErr = errors.New("logs unavailable")
	if _, err := fx.runSpec(t, testHandoffSpec()); err != nil {
		t.Fatalf("Handoff = %v; an unreadable boot log must not fail the launch", err)
	}
	if !strings.Contains(logs.String(), "boot_log_error=") {
		t.Fatalf("launch record does not report the unread boot log:\n%s", logs.String())
	}
}

func TestHandoffRecordsAnUnreadBootLogWhenTheWriterNeverStops(t *testing.T) {
	fx := newHandoffFixture(t)
	var logs bytes.Buffer
	fx.cfg.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	hs := testHandoffSpec()
	agent := namesFor(hs.RunID).Agent
	fx.rt.onStart = func(id string) error {
		if id == agent {
			return errors.New("start refused")
		}
		return nil
	}
	if _, err := fx.runSpec(t, hs); err == nil {
		t.Fatal("Handoff succeeded with a writer that never started")
	}
	// The record must say the kill question went unanswered, not "no".
	if !strings.Contains(logs.String(), "boot_log_error=") || !strings.Contains(logs.String(), "ended=failed") {
		t.Fatalf("launch record does not report the unread boot log:\n%s", logs.String())
	}
}

func TestHandoffRecordsALaunchThatFailsBeforeTheAgent(t *testing.T) {
	fx := newHandoffFixture(t)
	var logs bytes.Buffer
	fx.cfg.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	hs := testHandoffSpec()
	seeder := namesFor(hs.RunID).InstructionSeeder
	fx.rt.onStart = func(id string) error {
		if id == seeder {
			return errors.New("start refused")
		}
		return nil
	}
	if _, err := fx.runSpec(t, hs); err == nil {
		t.Fatal("Handoff succeeded with a seeder that never started")
	}
	if got := strings.Count(logs.String(), `msg="ward launch"`); got != 1 ||
		!strings.Contains(logs.String(), "ended=failed") {
		t.Fatalf("want one failed launch record, got:\n%s", logs.String())
	}
}

func TestHandoffRefusesAnUnsizedSpec(t *testing.T) {
	fx := newHandoffFixture(t)
	hs := testHandoffSpec()
	hs.Size = ContainerSize{}
	if _, err := fx.runSpec(t, hs); !errors.Is(err, ErrInvalidHandoffSpec) {
		t.Fatalf("Handoff = %v, want ErrInvalidHandoffSpec", err)
	}
	if len(fx.rt.calls) != 0 {
		t.Fatalf("an unsized handoff reached the runtime: %q", fx.rt.calls)
	}
}

// TestUnsizedSpecDigestsInThePreSizeFormat pins why pre-size journal records
// still recover: with Class and Size zero, the digested bytes carry neither.
func TestUnsizedSpecDigestsInThePreSizeFormat(t *testing.T) {
	t.Parallel()
	hs := testHandoffSpec()
	hs.Class, hs.Size = "", ContainerSize{}
	body, err := json.Marshal(hs)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte(`"Class"`)) || bytes.Contains(body, []byte(`"Size"`)) {
		t.Fatalf("unsized spec digests with size fields: %s", body)
	}
}

func TestRecoverAdoptsARecordJournaledBeforeSizes(t *testing.T) {
	fx := newRecoveryFixture(t)
	hs := testHandoffSpec()
	presize := hs
	presize.Class, presize.Size = "", ContainerSize{}
	fx.openRecord(t, presize)

	res, err := fx.recover(t, hs.RunID, hs)
	if err != nil {
		t.Fatalf("Recover = %v, want the pre-size record recovered", err)
	}
	if res.Outcome != RecoveryLoss {
		t.Fatalf("Outcome = %q, want loss", res.Outcome)
	}
	fx.wantClosed(t, hs.RunID, HandoffLoss)
}

func TestRecoverRefusesARecordJournaledAtAnotherSize(t *testing.T) {
	fx := newRecoveryFixture(t)
	hs := testHandoffSpec()
	other := hs
	other.Size = ContainerSize{CPUs: 8, MemoryMiB: 8192}
	fx.openRecord(t, other)

	if _, err := fx.recover(t, hs.RunID, hs); !errors.Is(err, ErrInvalidJournalRecord) {
		t.Fatalf("Recover = %v, want ErrInvalidJournalRecord", err)
	}
	fx.wantOpen(t, hs.RunID)
}

func TestCodexReviewKilledAtItsMemoryLimitFailsAsConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, status, events string
		wantEvidence         bool
		wantClass            domain.ReviewFailureClass
		wantMessage          string
	}{
		{"nonzero exit", "137", "partial transcript\n", true, domain.ReviewFailureConfiguration, ""},
		{"missing status", "", "partial transcript\n", false, domain.ReviewFailureConfiguration, ""},
		// A failure the provider reported keeps its class and leads the
		// message; the kill is still named.
		{
			"provider quota", "1", `{"type":"turn.failed","error":{"message":"rate limit reached"}}` + "\n", true,
			domain.ReviewFailureQuota, "rate limit reached",
		},
		{
			"credential refresh attempt", "1", `{"type":"turn.failed","error":{"message":"failed to refresh token"}}` + "\n", true,
			domain.ReviewFailureConfiguration, "failed to refresh token",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newHandoffFixture(t)
			var logs bytes.Buffer
			fx.cfg.Logger = slog.New(slog.NewTextHandler(&logs, nil))
			seed := fx.seed(t)
			lc := fx.codexReviewLifecycle(t)
			cfg, spec := testCodexReview(t)
			journal := &fakeCodexReviewJournal{}
			source, err := NewCodexReviewSource(codexReviewSourceConfigForTest(t, lc, cfg, spec, journal))
			if err != nil {
				t.Fatal(err)
			}
			id := domain.InvocationID("review-memory-limit")
			req := exec.ReviewRequest{
				RunID: "run-1", Round: 1, Repo: seed.Seed.Base.Repo, RepositoryID: seed.Seed.Base.RepositoryID,
				BaseRef: seed.Seed.Base.BaseRef, BaseSHA: strings.Repeat("a", 40), HeadSHA: seed.Seed.Base.BaseSHA,
				Workspace: seed.Seed.SourceDir, Verification: testReviewVerificationEvidence(),
				Instructions: testReviewInstructionBinding(), RequestedAt: codexReviewEpoch,
			}
			if err := source.RequestReview(t.Context(), id, req); err != nil {
				t.Fatal(err)
			}
			var entries []tarEntry
			for path, body := range map[string]string{codexReviewStatusPath: tc.status, codexReviewEventsPath: tc.events} {
				if body != "" {
					entries = append(entries, tarEntry{name: strings.TrimPrefix(path, "/"), body: []byte(body)})
				}
			}
			fx.rt.exportTarPath = buildTar(t, entries)
			container := codexReviewContainerName(string(id))
			fx.rt.bootLogs = map[string][]byte{container: bootLogKilling(t, container)}
			status, err := source.Inspect(t.Context(), id)
			for err == nil && status == exec.StatusRunning {
				status, err = source.Inspect(t.Context(), id)
			}
			if err != nil || status != exec.StatusFailed {
				t.Fatalf("status=%s err=%v, want failed", status, err)
			}
			outcome, ready, err := journal.GetCodexReviewOutcome(t.Context(), string(id))
			if err != nil || !ready {
				t.Fatalf("outcome ready=%t err=%v", ready, err)
			}
			if outcome.FailureClass != tc.wantClass ||
				!strings.HasPrefix(outcome.Failure, tc.wantMessage) ||
				!strings.Contains(outcome.Failure, "memory limit") ||
				!strings.Contains(outcome.Failure, DefaultLaunchSize(LaunchReview).String()) {
				t.Fatalf("outcome = %s %q, want %s led by %q, naming the limit and size",
					outcome.FailureClass, outcome.Failure, tc.wantClass, tc.wantMessage)
			}
			if (outcome.Collection != nil) != tc.wantEvidence {
				t.Fatalf("retained collection = %t, want %t", outcome.Collection != nil, tc.wantEvidence)
			}
			for _, want := range []string{"class=review", "ended=memory_limit", "memory_mib=1024"} {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("launch record lacks %q:\n%s", want, logs.String())
				}
			}
		})
	}
}

func TestProjectImageRoomDeclaresItsSizeAndNamesAMemoryLimitKill(t *testing.T) {
	size := ContainerSize{CPUs: 6, MemoryMiB: 4096}
	for _, tc := range []struct {
		name     string
		bootLog  func(t *testing.T, id string) []byte
		wantKill bool
	}{
		{"killed at the limit", bootLogKilling, true},
		{"ordinary failure", func(t *testing.T, _ string) []byte {
			return readBootLogFixture(t, "boot-log-sigkill.txt")
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime := newFakeRuntime(t)
			var logs bytes.Buffer
			var calls [][]string
			room := newProjectImageRoom(
				"container", verificationProjectImage(t, []string{"prepare"}), runtime,
				verificationRunner(t, runtime, []verify.StepResult{{}, {ExitCode: 137}}, &calls),
				nil, verify.DefaultMaxRoomOutputBytes, size,
			)
			room.logger = slog.New(slog.NewTextHandler(&logs, nil))
			// The runner names its containers verification-1, verification-2.
			runtime.bootLogs = map[string][]byte{"verification-2": tc.bootLog(t, "verification-2")}

			result, err := room.Run(t.Context(), t.TempDir(), []string{"verify"})
			for _, call := range calls {
				if !slices.Contains(call, "--cpus") ||
					!strings.Contains(strings.Join(call, " "), strings.Join(sizeArgs(size), " ")) {
					t.Fatalf("room container %q does not declare %s", call, size)
				}
			}
			if tc.wantKill {
				var limit *MemoryLimitError
				if !errors.As(err, &limit) || limit.Class != LaunchVerification || limit.Size != size {
					t.Fatalf("Run = %+v, %v; want a verification MemoryLimitError at %s", result, err, size)
				}
				if !strings.Contains(logs.String(), "ended=memory_limit") {
					t.Errorf("launch record lacks the kill:\n%s", logs.String())
				}
			} else if err != nil || result.ExitCode != 137 {
				t.Fatalf("Run = %+v, %v; want the recipe's own failed step", result, err)
			}
			if got := strings.Count(logs.String(), `msg="ward launch"`); got != 2 {
				t.Errorf("launch records = %d, want one per container:\n%s", got, logs.String())
			}
			for id := range runtime.ctrs {
				t.Errorf("room container %s survived", id)
			}
		})
	}
}

// A production room is always bound to verification ownership, whose finish
// cancels the command context before the boot log is read.
func TestBoundProjectImageRoomNamesAMemoryLimitKill(t *testing.T) {
	size := DefaultLaunchSize(LaunchVerification)
	runtime := newFakeRuntime(t)
	var calls [][]string
	room := newProjectImageRoom(
		"container", verificationProjectImage(t, []string{"prepare"}), runtime,
		verificationRunner(t, runtime, []verify.StepResult{{}, {ExitCode: 137}}, &calls),
		nil, verify.DefaultMaxRoomOutputBytes, size,
	)
	owned, err := NewVerificationOwnership(filepath.Join(t.TempDir(), "owned"), runtime)
	if err != nil {
		t.Fatal(err)
	}
	if err := owned.Bind(room, "task", "run", "verify"); err != nil {
		t.Fatal(err)
	}
	runtime.bootLogs = map[string][]byte{"verification-2": bootLogKilling(t, "verification-2")}
	result, err := room.Run(t.Context(), t.TempDir(), []string{"verify"})
	var limit *MemoryLimitError
	if !errors.As(err, &limit) || limit.Class != LaunchVerification {
		t.Fatalf("Run = %+v, %v; want a verification MemoryLimitError", result, err)
	}
}

// A helper runs no child whose kill its launch could survive, so a kill fails
// the launch as that failure even though the fake's helpers exit cleanly.
func TestHandoffFailsWhenAHelperIsKilledAtItsMemoryLimit(t *testing.T) {
	for _, helper := range []string{"instruction seeder", "exporter"} {
		t.Run(helper, func(t *testing.T) {
			fx := newHandoffFixture(t)
			var logs bytes.Buffer
			fx.cfg.Logger = slog.New(slog.NewTextHandler(&logs, nil))
			hs := testHandoffSpec()
			names := namesFor(hs.RunID)
			id := map[string]string{"instruction seeder": names.InstructionSeeder, "exporter": names.Exporter}[helper]
			fx.rt.bootLogs = map[string][]byte{id: bootLogKilling(t, id)}
			_, err := fx.runSpec(t, hs)
			var limit *MemoryLimitError
			if !errors.As(err, &limit) || limit.Class != LaunchWriter || limit.Size != hs.Size ||
				!strings.Contains(err.Error(), id) {
				t.Fatalf("Handoff = %v, want a writer MemoryLimitError naming %s", err, id)
			}
			for _, want := range []string{"ended=memory_limit", "memory_limit_kill=true"} {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("launch record lacks %q:\n%s", want, logs.String())
				}
			}
		})
	}
}

func TestReviewHelperKillClassifiesAsConfiguration(t *testing.T) {
	t.Parallel()
	killed := &MemoryLimitError{Class: LaunchReview, Size: DefaultLaunchSize(LaunchReview), Cause: errors.New("helper x")}
	// A kill never softens a contradiction joined to it.
	joined := errors.Join(killed, failf(CheckTeardown, "observer cleanup"))
	for name, classify := range map[string]func(error) domain.ReviewFailureClass{
		"launch": classifyCodexLaunchFailure, "observation": classifyCodexObservationFailure,
	} {
		if got := classify(killed); got != domain.ReviewFailureConfiguration {
			t.Errorf("%s class = %s, want configuration", name, got)
		}
		if got := classify(joined); got != domain.ReviewFailureContradiction {
			t.Errorf("%s joined class = %s, want contradiction", name, got)
		}
	}
}

func TestReviewLaunchFailsWhenAHelperIsKilledAtItsMemoryLimit(t *testing.T) {
	id := domain.InvocationID("review-memory-limit")
	// The workspace seed's observer, a review observer, and the snapshot
	// seeder stop at three distinct helper sites.
	for _, helper := range []string{
		"freeside-handoff-review-memory-limit-observer",
		"freeside-review-review-memory-limit-ws-obs",
		"freeside-review-review-memory-limit-snap-init",
	} {
		t.Run(helper, func(t *testing.T) {
			fx := newHandoffFixture(t)
			seed := fx.seed(t)
			lc := fx.codexReviewLifecycle(t)
			cfg, spec := testCodexReview(t)
			var logs bytes.Buffer
			lc.cfg.Logger = slog.New(slog.NewTextHandler(&logs, nil))
			source, err := NewCodexReviewSource(codexReviewSourceConfigForTest(t, lc, cfg, spec, &fakeCodexReviewJournal{}))
			if err != nil {
				t.Fatal(err)
			}
			fx.rt.bootLogs = map[string][]byte{helper: bootLogKilling(t, helper)}
			err = source.RequestReview(t.Context(), id, exec.ReviewRequest{
				RunID: "run-1", Round: 1, Repo: seed.Seed.Base.Repo, RepositoryID: seed.Seed.Base.RepositoryID,
				BaseRef: seed.Seed.Base.BaseRef, BaseSHA: strings.Repeat("a", 40), HeadSHA: seed.Seed.Base.BaseSHA,
				Workspace: seed.Seed.SourceDir, Verification: testReviewVerificationEvidence(),
				Instructions: testReviewInstructionBinding(), RequestedAt: codexReviewEpoch,
			})
			var failure *exec.ReviewSourceFailure
			if !errors.As(err, &failure) || failure.Class != domain.ReviewFailureConfiguration ||
				!errors.Is(err, ErrMemoryLimit) || !strings.Contains(err.Error(), helper) {
				t.Fatalf("RequestReview = %v, want a configuration failure naming %s", err, helper)
			}
			// The attempt never reaches collection, so the launch records it.
			for _, want := range []string{"class=review", "ended=memory_limit", "memory_limit_kill=true"} {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("launch record lacks %q:\n%s", want, logs.String())
				}
			}
		})
	}
}

// A runner that cannot prove its process gone still ran a container the
// runtime identified, so that launch is recorded and a kill is still named.
func TestProjectImageRoomRecordsALaunchWhoseRunnerFailed(t *testing.T) {
	for _, killed := range []bool{false, true} {
		t.Run(fmt.Sprintf("killed=%t", killed), func(t *testing.T) {
			runtime := newFakeRuntime(t)
			var logs bytes.Buffer
			var calls [][]string
			runner := verificationRunner(t, runtime, []verify.StepResult{{}, {ExitCode: 137}}, &calls)
			room := newProjectImageRoom(
				"container", verificationProjectImage(t, []string{"prepare"}), runtime,
				func(ctx context.Context, path string, args []string, limit int64) (verify.StepResult, error) {
					result, err := runner(ctx, path, args, limit)
					if err == nil && len(calls) == 2 {
						err = errVerificationProcessUnproven
					}
					return result, err
				},
				nil, verify.DefaultMaxRoomOutputBytes, DefaultLaunchSize(LaunchVerification),
			)
			room.logger = slog.New(slog.NewTextHandler(&logs, nil))
			if killed {
				runtime.bootLogs = map[string][]byte{"verification-2": bootLogKilling(t, "verification-2")}
			}
			_, err := room.Run(t.Context(), t.TempDir(), []string{"verify"})
			if !errors.Is(err, errVerificationProcessUnproven) || errors.Is(err, ErrMemoryLimit) != killed {
				t.Fatalf("Run = %v; want the runner error, named a memory-limit kill only when killed", err)
			}
			if got := strings.Count(logs.String(), `msg="ward launch"`); got != 2 {
				t.Fatalf("launch records = %d, want one per container:\n%s", got, logs.String())
			}
		})
	}
}
