package macverify

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

// fakeLauncher stands in for slot-run: it creates the job directory prepare
// would and records every operation, so the room's sequencing and absence
// rule run without root.
type fakeLauncher struct {
	root string

	mu       sync.Mutex
	calls    []string
	prepares []prepareCall
	runs     []runRequest

	// jobDirMode is the mode prepare gives the job directory; zero is 0700.
	jobDirMode os.FileMode
	// uid is what prepare and reap report; zero is the test's own uid.
	uid int
	// reapUID overrides the uid reap reports.
	reapUID int
	// prepareHook runs inside prepare before it acts; a non-nil error is
	// prepare's result.
	prepareHook func(ctx context.Context) error
	prepareErr  error
	reapErr     error
	cleanupErr  error
	// command plays the command; reaped closes on the first reap.
	command  func(req runRequest, out io.Writer, reaped <-chan struct{}) runOutcome
	reaped   chan struct{}
	reapOnce sync.Once
	// live answers ownsProcess; nil means the slot owns nothing.
	live func(uid int) bool
}

type prepareCall struct {
	slot, jobID, workdir string
	profile              []byte
}

func newFakeLauncher() *fakeLauncher {
	return &fakeLauncher{
		reaped: make(chan struct{}),
		command: func(runRequest, io.Writer, <-chan struct{}) runOutcome {
			return runOutcome{}
		},
	}
}

func (f *fakeLauncher) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeLauncher) slotUID() int {
	if f.uid != 0 {
		return f.uid
	}
	return os.Getuid()
}

func (f *fakeLauncher) prepare(ctx context.Context, slot, jobID, workdir string, profile []byte) (preparedJob, error) {
	f.record("prepare " + slot)
	f.mu.Lock()
	f.prepares = append(f.prepares, prepareCall{slot: slot, jobID: jobID, workdir: workdir, profile: slices.Clone(profile)})
	f.mu.Unlock()
	if f.prepareHook != nil {
		if err := f.prepareHook(ctx); err != nil {
			return preparedJob{}, err
		}
	}
	if f.prepareErr != nil {
		return preparedJob{}, f.prepareErr
	}
	jobDir := filepath.Join(f.root, "jobs", slot, jobID)
	if err := os.MkdirAll(filepath.Dir(jobDir), 0o750); err != nil {
		return preparedJob{}, err
	}
	mode := f.jobDirMode
	if mode == 0 {
		mode = 0o700
	}
	if err := os.Mkdir(jobDir, mode); err != nil {
		return preparedJob{}, err
	}
	if err := os.Chmod(jobDir, mode); err != nil {
		return preparedJob{}, err
	}
	return preparedJob{
		Root:  f.root,
		UID:   f.slotUID(),
		Home:  filepath.Join(f.root, "homes", slot),
		Tmp:   "/private/var/folders/ab/" + slot + "/T",
		Cache: "/private/var/folders/ab/" + slot + "/C",
	}, nil
}

func (f *fakeLauncher) run(req runRequest, out io.Writer) (<-chan runOutcome, error) {
	f.record("run " + req.Slot)
	f.mu.Lock()
	f.runs = append(f.runs, req)
	f.mu.Unlock()
	done := make(chan runOutcome, 1)
	go func() { done <- f.command(req, out, f.reaped) }()
	return done, nil
}

func (f *fakeLauncher) reap(_ context.Context, slot string) (int, error) {
	f.record("reap " + slot)
	f.reapOnce.Do(func() { close(f.reaped) })
	if f.reapErr != nil {
		return 0, f.reapErr
	}
	if f.reapUID != 0 {
		return f.reapUID, nil
	}
	return f.slotUID(), nil
}

func (f *fakeLauncher) cleanup(_ context.Context, slot, _ string) error {
	f.record("cleanup " + slot)
	return f.cleanupErr
}

func (f *fakeLauncher) ownsProcess(uid int) (bool, error) {
	if f.live == nil {
		return false, nil
	}
	return f.live(uid), nil
}

func (f *fakeLauncher) snapshot() ([]string, []prepareCall, []runRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls), slices.Clone(f.prepares), slices.Clone(f.runs)
}

const testProfile = "(version 1)(allow default)(deny network*)"

var testToolchain = Toolchain{XcodeBuild: "17F42", Swift: "swiftlang-6.1.2.1.2"}

func newTestRoom(t *testing.T, fake *fakeLauncher, slots ...string) *Room {
	t.Helper()
	if len(slots) == 0 {
		slots = []string{"slot1"}
	}
	root := t.TempDir()
	b, err := New(Config{
		ExecutorRoot:      root,
		SandboxProfile:    []byte(testProfile),
		PacketFilterRules: []byte("block drop quick user slot1"),
		Slots:             slots,
		Toolchain:         testToolchain,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	room, err := NewRoom(b, RoomOptions{MaxOutputBytes: 1 << 10})
	if err != nil {
		t.Fatalf("NewRoom: %v", err)
	}
	installHelper(t, room.helperPath, helperScript)
	fake.root = root
	room.launcher = fake
	room.helperOwner = uint32(os.Getuid()) //nolint:gosec // G115: a uid fits
	room.hostSupported = true
	room.hostToolchain = func(context.Context) (Toolchain, error) { return testToolchain, nil }
	room.prepareTimeout = 200 * time.Millisecond
	room.teardownTimeout = 200 * time.Millisecond
	room.cancelGrace = 200 * time.Millisecond
	return room
}

func installHelper(t *testing.T, path string, script []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	// The fake launcher never executes the helper, so owner read and write
	// are enough for the trust check.
	if err := os.WriteFile(path, script, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRunPreparesRunsAndReleasesSlot(t *testing.T) {
	t.Parallel()
	fake := newFakeLauncher()
	fake.command = func(_ runRequest, out io.Writer, _ <-chan struct{}) runOutcome {
		_, _ = io.WriteString(out, "hello\n")
		return runOutcome{}
	}
	room := newTestRoom(t, fake)
	workdir := t.TempDir()

	res, err := room.Run(t.Context(), workdir, []string{"swift", "test", "--disable-sandbox"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 || string(res.Output) != "hello\n" || res.Truncated {
		t.Fatalf("result = %+v", res)
	}
	calls, prepares, runs := fake.snapshot()
	if want := []string{"prepare slot1", "run slot1", "reap slot1", "cleanup slot1"}; !slices.Equal(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	p := prepares[0]
	if p.slot != "slot1" || p.workdir != workdir || string(p.profile) != testProfile {
		t.Fatalf("prepare = %+v", p)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(p.jobID) {
		t.Fatalf("job id %q is not 32 lowercase hex", p.jobID)
	}
	req := runs[0]
	home := filepath.Join(fake.root, "homes", "slot1")
	tmp, cache := "/private/var/folders/ab/slot1/T", "/private/var/folders/ab/slot1/C"
	if want := []string{"HOME=" + home, "TMPDIR=" + tmp, "PATH=" + commandPath, "LC_ALL=C"}; !slices.Equal(req.Env, want) {
		t.Fatalf("environment = %v, want exactly %v", req.Env, want)
	}
	jobDir := filepath.Join(fake.root, "jobs", "slot1", p.jobID)
	if want := []string{"JOB_DIR=" + jobDir, "SLOT_HOME=" + home, "SLOT_TMP=" + tmp, "SLOT_CACHE=" + cache}; !slices.Equal(req.Params, want) {
		t.Fatalf("sandbox parameters = %v, want %v", req.Params, want)
	}
	if req.Slot != "slot1" || req.JobID != p.jobID || !slices.Equal(req.Argv, []string{"swift", "test", "--disable-sandbox"}) {
		t.Fatalf("run request = %+v", req)
	}

	// The one slot is free again.
	if _, err := room.Run(t.Context(), workdir, []string{"true"}); err != nil {
		t.Fatalf("second Run: %v", err)
	}
}

func TestRunRefusesJobDirectoryNotPrivateToSlot(t *testing.T) {
	t.Parallel()
	for name, setup := range map[string]func(*fakeLauncher){
		"group readable": func(f *fakeLauncher) { f.jobDirMode = 0o750 },
		"other uid":      func(f *fakeLauncher) { f.uid = os.Getuid() + 1 },
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeLauncher()
			setup(fake)
			room := newTestRoom(t, fake)

			_, err := room.Run(t.Context(), t.TempDir(), []string{"true"})
			if err == nil || !strings.Contains(err.Error(), "0700") || errors.Is(err, ErrAbsenceUnproven) {
				t.Fatalf("Run error = %v, want a job directory refusal", err)
			}
			calls, _, _ := fake.snapshot()
			if want := []string{"prepare slot1", "reap slot1", "cleanup slot1"}; !slices.Equal(calls, want) {
				t.Fatalf("calls = %v, want %v (no run, then teardown)", calls, want)
			}
			if _, err := room.pool.lease(); err != nil {
				t.Fatalf("slot not released after a proven teardown: %v", err)
			}
		})
	}
}

func TestRunTimeoutKillsBySlotUID(t *testing.T) {
	t.Parallel()
	fake := newFakeLauncher()
	fake.command = func(_ runRequest, out io.Writer, reaped <-chan struct{}) runOutcome {
		_, _ = io.WriteString(out, "started\n")
		select {
		case <-reaped:
			return runOutcome{ExitCode: -1}
		case <-time.After(5 * time.Second):
			return runOutcome{ExitCode: 0}
		}
	}
	room := newTestRoom(t, fake)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	res, err := room.Run(ctx, t.TempDir(), []string{"sleep", "600"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != -1 || string(res.Output) != "started\n" {
		t.Fatalf("result = %+v, want the signal death -1", res)
	}
	calls, _, _ := fake.snapshot()
	if want := []string{"prepare slot1", "run slot1", "reap slot1", "reap slot1", "cleanup slot1"}; !slices.Equal(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestRunQuarantinesSlotWhenCanceledCommandNeverExits(t *testing.T) {
	t.Parallel()
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	ctx, cancel := context.WithCancel(t.Context())
	fake := newFakeLauncher()
	fake.command = func(runRequest, io.Writer, <-chan struct{}) runOutcome {
		cancel()
		<-stuck
		return runOutcome{}
	}
	room := newTestRoom(t, fake)

	res, err := room.Run(ctx, t.TempDir(), []string{"true"})
	if !errors.Is(err, ErrAbsenceUnproven) || !strings.Contains(err.Error(), "did not exit") || res.Output != nil {
		t.Fatalf("Run = %+v, %v; want ErrAbsenceUnproven for a helper still running", res, err)
	}
	calls, _, _ := fake.snapshot()
	if want := []string{"prepare slot1", "run slot1", "reap slot1"}; !slices.Equal(calls, want) {
		t.Fatalf("calls = %v, want %v (no teardown under a live helper)", calls, want)
	}
	if _, err := room.pool.lease(); !errors.Is(err, ErrNoFreeSlot) {
		t.Fatalf("lease after quarantine = %v, want ErrNoFreeSlot", err)
	}
}

func TestRunQuarantinesSlotWhenPrepareIsAbandoned(t *testing.T) {
	t.Parallel()
	fake := newFakeLauncher()
	fake.prepareHook = func(ctx context.Context) error {
		<-ctx.Done()
		return errors.Join(errHelperAbandoned, ctx.Err())
	}
	room := newTestRoom(t, fake)

	_, err := room.Run(t.Context(), t.TempDir(), []string{"true"})
	if !errors.Is(err, ErrAbsenceUnproven) {
		t.Fatalf("Run error = %v, want ErrAbsenceUnproven", err)
	}
	calls, _, _ := fake.snapshot()
	if want := []string{"prepare slot1"}; !slices.Equal(calls, want) {
		t.Fatalf("calls = %v, want %v (no teardown racing prepare)", calls, want)
	}
	if _, err := room.pool.lease(); !errors.Is(err, ErrNoFreeSlot) {
		t.Fatalf("lease after quarantine = %v, want ErrNoFreeSlot", err)
	}
}

func TestRunCanceledDuringPrepareFinishesPrepareThenTearsDown(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	fake := newFakeLauncher()
	fake.prepareHook = func(prepareCtx context.Context) error {
		cancel()
		if err := prepareCtx.Err(); err != nil {
			return errors.New("prepare ran under the step's context")
		}
		return nil
	}
	room := newTestRoom(t, fake)

	_, err := room.Run(ctx, t.TempDir(), []string{"true"})
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrAbsenceUnproven) {
		t.Fatalf("Run error = %v, want the cancellation with absence proven", err)
	}
	calls, _, _ := fake.snapshot()
	if want := []string{"prepare slot1", "reap slot1", "cleanup slot1"}; !slices.Equal(calls, want) {
		t.Fatalf("calls = %v, want %v (no command after cancellation)", calls, want)
	}
	if _, err := room.pool.lease(); err != nil {
		t.Fatalf("slot not released after a proven teardown: %v", err)
	}
}

func TestRunRefusesCanceledContextBeforeLeasing(t *testing.T) {
	t.Parallel()
	fake := newFakeLauncher()
	room := newTestRoom(t, fake)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := room.Run(ctx, t.TempDir(), []string{"true"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	if calls, _, _ := fake.snapshot(); len(calls) != 0 {
		t.Fatalf("helper invoked under a canceled context: %v", calls)
	}
	if _, err := room.pool.lease(); err != nil {
		t.Fatalf("slot leased by a canceled Run: %v", err)
	}
}

func TestRoomsOverOneBackendShareSlots(t *testing.T) {
	t.Parallel()
	b, err := New(Config{
		ExecutorRoot: "/var/freeside/macverify", SandboxProfile: DefaultSandboxProfile(),
		PacketFilterRules: []byte("rules"), Slots: []string{"slot1"}, Toolchain: testToolchain,
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := NewRoom(b, RoomOptions{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewRoom(b, RoomOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.pool.lease(); err != nil {
		t.Fatal(err)
	}
	if _, err := second.pool.lease(); !errors.Is(err, ErrNoFreeSlot) {
		t.Fatalf("second room leased a slot the first holds: %v", err)
	}
}

func TestRunCapsOutputKeepingTheTail(t *testing.T) {
	t.Parallel()
	fake := newFakeLauncher()
	fake.command = func(_ runRequest, out io.Writer, _ <-chan struct{}) runOutcome {
		_, _ = out.Write(bytes.Repeat([]byte("a"), 4<<10))
		_, _ = io.WriteString(out, "TAIL")
		return runOutcome{ExitCode: 1}
	}
	room := newTestRoom(t, fake)

	res, err := room.Run(t.Context(), t.TempDir(), []string{"true"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Truncated || len(res.Output) != 1<<10 || !bytes.HasSuffix(res.Output, []byte("TAIL")) || res.ExitCode != 1 {
		t.Fatalf("result truncated=%v len=%d exit=%d", res.Truncated, len(res.Output), res.ExitCode)
	}
}

func TestRunMapsCommandOutcomes(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		outcome  runOutcome
		wantExit int
		wantErr  bool
	}{
		"nonzero exit":   {outcome: runOutcome{ExitCode: 3}, wantExit: 3},
		"signal death":   {outcome: runOutcome{ExitCode: -1}, wantExit: -1},
		"helper failure": {outcome: runOutcome{Err: errors.New("slot-run refused the run")}, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeLauncher()
			fake.command = func(runRequest, io.Writer, <-chan struct{}) runOutcome { return tc.outcome }
			room := newTestRoom(t, fake)

			res, err := room.Run(t.Context(), t.TempDir(), []string{"true"})
			if tc.wantErr {
				if err == nil || errors.Is(err, ErrAbsenceUnproven) || res.ExitCode != 0 || res.Output != nil {
					t.Fatalf("Run = %+v, %v; want a room error and no step result", res, err)
				}
			} else if err != nil || res.ExitCode != tc.wantExit {
				t.Fatalf("Run = %+v, %v; want exit %d as a step result", res, err, tc.wantExit)
			}
			if _, err := room.pool.lease(); err != nil {
				t.Fatalf("slot not released: %v", err)
			}
		})
	}
}

func TestRunPrepareFailureIsARoomError(t *testing.T) {
	t.Parallel()
	fake := newFakeLauncher()
	fake.prepareErr = errors.New("slot-run: slot slot1 owns a live process")
	room := newTestRoom(t, fake)

	_, err := room.Run(t.Context(), t.TempDir(), []string{"true"})
	if err == nil || errors.Is(err, ErrAbsenceUnproven) || !strings.Contains(err.Error(), "live process") {
		t.Fatalf("Run error = %v", err)
	}
	calls, _, _ := fake.snapshot()
	if want := []string{"prepare slot1", "reap slot1", "cleanup slot1"}; !slices.Equal(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestRunRefusesWhenEverySlotIsLeased(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	fake := newFakeLauncher()
	fake.command = func(runRequest, io.Writer, <-chan struct{}) runOutcome {
		started <- struct{}{}
		<-release
		return runOutcome{}
	}
	room := newTestRoom(t, fake)
	first := make(chan error, 1)
	go func() {
		_, err := room.Run(context.Background(), t.TempDir(), []string{"true"})
		first <- err
	}()
	<-started

	_, err := room.Run(t.Context(), t.TempDir(), []string{"true"})
	close(release)
	if !errors.Is(err, ErrNoFreeSlot) {
		t.Fatalf("second Run error = %v, want ErrNoFreeSlot", err)
	}
	if err := <-first; err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if _, prepares, _ := fake.snapshot(); len(prepares) != 1 {
		t.Fatalf("prepared %d jobs, want only the first", len(prepares))
	}
}

func TestRunQuarantinesSlotWhoseAbsenceIsUnproven(t *testing.T) {
	t.Parallel()
	for name, setup := range map[string]func(*fakeLauncher){
		"lingering process": func(f *fakeLauncher) { f.live = func(int) bool { return true } },
		"reap failure":      func(f *fakeLauncher) { f.reapErr = errors.New("sudo: a password is required") },
		"cleanup failure":   func(f *fakeLauncher) { f.cleanupErr = errors.New("slot-run: cannot remove job") },
		"uid changed":       func(f *fakeLauncher) { f.reapUID = os.Getuid() + 1 },
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeLauncher()
			setup(fake)
			room := newTestRoom(t, fake)

			res, err := room.Run(t.Context(), t.TempDir(), []string{"true"})
			if !errors.Is(err, ErrAbsenceUnproven) || res.ExitCode != 0 || res.Output != nil {
				t.Fatalf("Run = %+v, %v; want ErrAbsenceUnproven and an empty result", res, err)
			}
			if _, err := room.Run(t.Context(), t.TempDir(), []string{"true"}); !errors.Is(err, ErrNoFreeSlot) {
				t.Fatalf("Run after quarantine error = %v, want ErrNoFreeSlot", err)
			}
			if _, prepares, _ := fake.snapshot(); len(prepares) != 1 {
				t.Fatalf("prepared %d jobs, want the quarantined slot never leased again", len(prepares))
			}
		})
	}
}

func TestRunRefusesUntrustedHelper(t *testing.T) {
	t.Parallel()
	for name, tamper := range map[string]func(t *testing.T, room *Room){
		"differing bytes": func(t *testing.T, room *Room) {
			installHelper(t, room.helperPath, append(slices.Clone(helperScript), "\n:\n"...))
		},
		"group writable": func(t *testing.T, room *Room) {
			if err := os.Chmod(room.helperPath, 0o620); err != nil { //nolint:gosec // G302: group-writable on purpose; the refusal is the point
				t.Fatal(err)
			}
		},
		"world writable": func(t *testing.T, room *Room) {
			if err := os.Chmod(room.helperPath, 0o602); err != nil { //nolint:gosec // G302: world-writable on purpose; the refusal is the point
				t.Fatal(err)
			}
		},
		"not owned by root": func(_ *testing.T, room *Room) { room.helperOwner = uint32(os.Getuid() + 1) }, //nolint:gosec // G115: a uid fits
		"symlink": func(t *testing.T, room *Room) {
			real := room.helperPath + ".real"
			installHelper(t, real, helperScript)
			if err := os.Remove(room.helperPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, room.helperPath); err != nil {
				t.Fatal(err)
			}
		},
		"missing": func(t *testing.T, room *Room) {
			if err := os.Remove(room.helperPath); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeLauncher()
			room := newTestRoom(t, fake)
			tamper(t, room)

			_, err := room.Run(t.Context(), t.TempDir(), []string{"true"})
			if !errors.Is(err, ErrHelperUntrusted) {
				t.Fatalf("Run error = %v, want ErrHelperUntrusted", err)
			}
			if calls, _, _ := fake.snapshot(); len(calls) != 0 {
				t.Fatalf("helper invoked despite refusal: %v", calls)
			}
		})
	}
}

func TestRunRejectsInvalidArguments(t *testing.T) {
	t.Parallel()
	fake := newFakeLauncher()
	room := newTestRoom(t, fake)
	workdir := t.TempDir()
	file := filepath.Join(workdir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		workdir string
		argv    []string
	}{
		"empty argv":        {workdir: workdir},
		"empty program":     {workdir: workdir, argv: []string{""}},
		"NUL in argument":   {workdir: workdir, argv: []string{"echo", "a\x00b"}},
		"relative workdir":  {workdir: "workspace", argv: []string{"true"}},
		"unclean workdir":   {workdir: workdir + "/.", argv: []string{"true"}},
		"missing workdir":   {workdir: filepath.Join(workdir, "missing"), argv: []string{"true"}},
		"workdir not a dir": {workdir: file, argv: []string{"true"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := room.Run(t.Context(), tc.workdir, tc.argv); err == nil {
				t.Fatal("Run accepted invalid arguments")
			}
		})
	}
	if calls, _, _ := fake.snapshot(); len(calls) != 0 {
		t.Fatalf("helper invoked for invalid arguments: %v", calls)
	}
}

func TestRunRefusesOffMacOS(t *testing.T) {
	t.Parallel()
	fake := newFakeLauncher()
	room := newTestRoom(t, fake)
	room.hostSupported = false

	if _, err := room.Run(t.Context(), t.TempDir(), []string{"true"}); !errors.Is(err, ErrHostUnsupported) {
		t.Fatalf("Run error = %v, want ErrHostUnsupported", err)
	}
}

// TestNewRoomRefusesToRunOffMacOS builds the production room on a host that
// is not macOS, as the engine will on Linux, and checks Run refuses by name.
func TestNewRoomRefusesToRunOffMacOS(t *testing.T) {
	t.Parallel()
	if hostSupported {
		t.Skip("the host is macOS")
	}
	b, err := New(Config{
		ExecutorRoot: "/var/freeside/macverify", SandboxProfile: DefaultSandboxProfile(),
		PacketFilterRules: []byte("rules"), Slots: []string{"slot1"}, Toolchain: testToolchain,
	})
	if err != nil {
		t.Fatal(err)
	}
	room, err := NewRoom(b, RoomOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := room.Run(t.Context(), t.TempDir(), []string{"true"}); !errors.Is(err, ErrHostUnsupported) {
		t.Fatalf("Run error = %v, want ErrHostUnsupported", err)
	}
}

func TestNewRoomRejectsInvalidOptions(t *testing.T) {
	t.Parallel()
	b, err := New(Config{
		ExecutorRoot: "/var/freeside/macverify", SandboxProfile: DefaultSandboxProfile(),
		PacketFilterRules: []byte("rules"), Slots: []string{"slot1"}, Toolchain: testToolchain,
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		backend *Backend
		opts    RoomOptions
	}{
		"nil backend":         {opts: RoomOptions{}},
		"zero backend":        {backend: &Backend{}},
		"negative cap":        {backend: b, opts: RoomOptions{MaxOutputBytes: -1}},
		"negative wait delay": {backend: b, opts: RoomOptions{WaitDelay: -time.Second}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewRoom(tc.backend, tc.opts); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("NewRoom error = %v, want ErrInvalidConfig", err)
			}
		})
	}
	room, err := NewRoom(b, RoomOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if room.maxOutput != verify.DefaultMaxRoomOutputBytes || room.helperPath != "/var/freeside/macverify/bin/slot-run" {
		t.Fatalf("defaults: cap %d, helper %s", room.maxOutput, room.helperPath)
	}
}
