package macverify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
// rule run without root. A job prepared from probeDir is a probe job: its
// run plays probe, and the recipe-only hooks below leave it alone.
type fakeLauncher struct {
	root     string
	probeDir string

	mu       sync.Mutex
	calls    []string
	prepares []prepareCall
	runs     []runRequest
	// profiles maps a job id to the profile prepare installed.
	profiles map[string][]byte

	// jobDirMode is the mode a recipe job's prepare gives the job directory;
	// zero is 0700.
	jobDirMode os.FileMode
	// uid is what prepare and reap report; zero is testSlotUID.
	uid int
	// reapUID overrides the uid reap reports.
	reapUID int
	// prepareHook runs inside a recipe job's prepare before it acts; a
	// non-nil error is prepare's result.
	prepareHook func(ctx context.Context) error
	// prepareErr fails every prepare, probe jobs included.
	prepareErr error
	// reportSwiftCache overrides the Swift cache key a recipe job's prepare
	// reports.
	reportSwiftCache *string
	reapErr          error
	cleanupErr       error
	// recipeCleanupErr fails only a recipe job's cleanup.
	recipeCleanupErr error
	// command plays a recipe command; reaped closes on the first reap after
	// it starts.
	command  func(req runRequest, out io.Writer, reaped <-chan struct{}) runOutcome
	reaped   chan struct{}
	reapOnce *sync.Once
	// probe plays a probe job and returns its report and exit status; nil
	// reports every check of the job's argv ok.
	probe func(req runRequest, profile []byte) (string, int)
	// live answers ownsProcess; nil means the slot owns nothing.
	live func(uid int) bool
}

type prepareCall struct {
	slot, jobID, workdir, swiftCache string
	profile                          []byte
}

func newFakeLauncher() *fakeLauncher {
	return &fakeLauncher{
		profiles: make(map[string][]byte),
		reaped:   make(chan struct{}),
		reapOnce: &sync.Once{},
		command: func(runRequest, io.Writer, <-chan struct{}) runOutcome {
			return runOutcome{}
		},
	}
}

func isProbeArgv(argv []string) bool {
	return len(argv) > len(probeCommand) && slices.Equal(argv[:len(probeCommand)], probeCommand)
}

// specFromArgv rebuilds the probe spec a probe argv was built from.
func specFromArgv(argv []string) probeSpec {
	mode, args := argv[len(probeCommand)], argv[len(probeCommand)+1:]
	switch mode {
	case probeModeSandbox:
		return sandboxProbe(sandboxTargets{
			loopbackPort: args[0], resolverHost: args[1], tcp: args[2], udp: args[3], outsideDirs: args[4:],
		})
	case probeModePacketFilter:
		return packetFilterProbe(args[0], args[1])
	case probeModeHomeMark:
		return homeMarkProbe()
	case probeModeHomeClean:
		return homeCleanProbe()
	}
	panic("unknown probe mode " + mode)
}

// passingReport is the report a confined slot's probe prints for spec.
func passingReport(spec probeSpec) string {
	var b strings.Builder
	for _, c := range spec.checks {
		b.WriteString("  (refused as expected: operation not permitted)\n")
		b.WriteString("ok " + c.name + "\n")
	}
	return b.String()
}

// failingReport is spec's passing report with check failed instead.
func failingReport(spec probeSpec, check string) string {
	return strings.Replace(passingReport(spec), "ok "+check+"\n", "FAIL "+check+": succeeded\n", 1)
}

func (f *fakeLauncher) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

// testSlotUID is the uid a fake slot runs as: the test's own, or a stand-in
// when the test runs as root, as it does in the project image's recipe proof
// (#3013). A real slot is never root: parseUID refuses uid 0, and teardown
// reads a prepared uid of 0 as none reported and skips its uid check.
func testSlotUID() int {
	if uid := os.Getuid(); uid != 0 {
		return uid
	}
	return 4242
}

func (f *fakeLauncher) slotUID() int {
	if f.uid != 0 {
		return f.uid
	}
	return testSlotUID()
}

func (f *fakeLauncher) prepare(ctx context.Context, slot, jobID, workdir string, profile []byte, swiftCache string) (preparedJob, error) {
	f.record("prepare " + slot)
	f.mu.Lock()
	f.prepares = append(f.prepares, prepareCall{
		slot: slot, jobID: jobID, workdir: workdir, swiftCache: swiftCache, profile: slices.Clone(profile),
	})
	f.profiles[jobID] = slices.Clone(profile)
	f.mu.Unlock()
	recipe := workdir != f.probeDir
	if recipe && f.prepareHook != nil {
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
	mode := os.FileMode(0o700)
	if recipe && f.jobDirMode != 0 {
		mode = f.jobDirMode
	}
	if err := os.Mkdir(jobDir, mode); err != nil {
		return preparedJob{}, err
	}
	if err := os.Chmod(jobDir, mode); err != nil {
		return preparedJob{}, err
	}
	// As root, hand the directory to the stand-in slot uid, as slot-run's
	// prepare does. A test that sets uid still finds it owned by another.
	if os.Getuid() == 0 {
		if err := os.Chown(jobDir, testSlotUID(), -1); err != nil {
			return preparedJob{}, err
		}
	}
	reported := swiftCache
	if recipe && f.reportSwiftCache != nil {
		reported = *f.reportSwiftCache
	}
	return preparedJob{
		Root:       f.root,
		UID:        f.slotUID(),
		Home:       filepath.Join(f.root, "homes", slot),
		Tmp:        "/private/var/folders/ab/" + slot + "/T",
		Cache:      "/private/var/folders/ab/" + slot + "/C",
		SwiftCache: reported,
	}, nil
}

func (f *fakeLauncher) run(req runRequest, out io.Writer) (<-chan runOutcome, error) {
	f.record("run " + req.Slot)
	f.mu.Lock()
	f.runs = append(f.runs, req)
	profile := f.profiles[req.JobID]
	f.mu.Unlock()
	done := make(chan runOutcome, 1)
	if isProbeArgv(req.Argv) {
		report, exit := passingReport(specFromArgv(req.Argv)), 0
		if f.probe != nil {
			report, exit = f.probe(req, profile)
		}
		_, _ = io.WriteString(out, report)
		done <- runOutcome{ExitCode: exit}
		return done, nil
	}
	f.mu.Lock()
	reaped := make(chan struct{})
	f.reaped, f.reapOnce = reaped, &sync.Once{}
	f.mu.Unlock()
	go func() { done <- f.command(req, out, reaped) }()
	return done, nil
}

func (f *fakeLauncher) reap(_ context.Context, slot string) (int, error) {
	f.record("reap " + slot)
	f.mu.Lock()
	reaped, once := f.reaped, f.reapOnce
	f.mu.Unlock()
	once.Do(func() { close(reaped) })
	if f.reapErr != nil {
		return 0, f.reapErr
	}
	if f.reapUID != 0 {
		return f.reapUID, nil
	}
	return f.slotUID(), nil
}

func (f *fakeLauncher) cleanup(_ context.Context, slot, jobID string) error {
	f.record("cleanup " + slot)
	if f.recipeCleanupErr != nil {
		prepares, _ := f.recipeJobs()
		if slices.ContainsFunc(prepares, func(p prepareCall) bool { return p.jobID == jobID }) {
			return f.recipeCleanupErr
		}
	}
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

// recipeJobs returns the prepares and runs of recipe jobs only.
func (f *fakeLauncher) recipeJobs() ([]prepareCall, []runRequest) {
	_, prepares, runs := f.snapshot()
	prepares = slices.DeleteFunc(prepares, func(p prepareCall) bool { return p.workdir == f.probeDir })
	runs = slices.DeleteFunc(runs, func(r runRequest) bool { return isProbeArgv(r.Argv) })
	return prepares, runs
}

// probeJob is the launcher calls of one passing probe job on slot.
func probeJob(slot string) []string {
	return []string{"prepare " + slot, "run " + slot, "reap " + slot, "cleanup " + slot}
}

// withProbe is calls prefixed with the pre-job probe's on slot1.
func withProbe(calls ...string) []string {
	return append(probeJob("slot1"), calls...)
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
	fake.probeDir = t.TempDir()
	room.probeWorkspace = func() (string, error) { return fake.probeDir, nil }
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
	calls, _, _ := fake.snapshot()
	if want := withProbe("prepare slot1", "run slot1", "reap slot1", "cleanup slot1"); !slices.Equal(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	prepares, runs := fake.recipeJobs()
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
	refused := []string{"prepare slot1", "reap slot1", "cleanup slot1"}
	for name, tc := range map[string]struct {
		setup func(*fakeLauncher)
		calls []string
	}{
		// The mode applies to the recipe job, so its probe passes first.
		"group readable": {setup: func(f *fakeLauncher) { f.jobDirMode = 0o750 }, calls: withProbe(refused...)},
		// A slot that is another uid refuses the probe's job already.
		"other uid": {setup: func(f *fakeLauncher) { f.uid = testSlotUID() + 1 }, calls: refused},
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeLauncher()
			tc.setup(fake)
			room := newTestRoom(t, fake)

			_, err := room.Run(t.Context(), t.TempDir(), []string{"true"})
			if err == nil || !strings.Contains(err.Error(), "0700") || errors.Is(err, ErrAbsenceUnproven) {
				t.Fatalf("Run error = %v, want a job directory refusal", err)
			}
			if calls, _, _ := fake.snapshot(); !slices.Equal(calls, tc.calls) {
				t.Fatalf("calls = %v, want %v (no run, then teardown)", calls, tc.calls)
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
	if want := withProbe("prepare slot1", "run slot1", "reap slot1", "reap slot1", "cleanup slot1"); !slices.Equal(calls, want) {
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
	if want := withProbe("prepare slot1", "run slot1", "reap slot1"); !slices.Equal(calls, want) {
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
	if want := withProbe("prepare slot1"); !slices.Equal(calls, want) {
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
	if want := withProbe("prepare slot1", "reap slot1", "cleanup slot1"); !slices.Equal(calls, want) {
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
	if prepares, _ := fake.recipeJobs(); len(prepares) != 1 {
		t.Fatalf("prepared %d recipe jobs, want only the first", len(prepares))
	}
}

func TestRunQuarantinesSlotWhoseAbsenceIsUnproven(t *testing.T) {
	t.Parallel()
	for name, setup := range map[string]func(*fakeLauncher){
		"lingering process": func(f *fakeLauncher) { f.live = func(int) bool { return true } },
		"reap failure":      func(f *fakeLauncher) { f.reapErr = errors.New("sudo: a password is required") },
		"cleanup failure":   func(f *fakeLauncher) { f.cleanupErr = errors.New("slot-run: cannot remove job") },
		"uid changed":       func(f *fakeLauncher) { f.reapUID = testSlotUID() + 1 },
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

// TestRunNamesExitStatusWhenTeardownFails has a recipe command exit and its
// cleanup fail, as a slot left with an entry it cannot remove does, and
// checks the error still says how the command ended.
func TestRunNamesExitStatusWhenTeardownFails(t *testing.T) {
	t.Parallel()
	for _, code := range []int{0, 3} {
		fake := newFakeLauncher()
		fake.command = func(runRequest, io.Writer, <-chan struct{}) runOutcome { return runOutcome{ExitCode: code} }
		fake.recipeCleanupErr = errors.New("slot-run: rm: Operation not permitted")
		room := newTestRoom(t, fake)

		_, err := room.Run(t.Context(), t.TempDir(), []string{"true"})
		want := fmt.Sprintf("after the command exited %d: ", code)
		if !errors.Is(err, ErrAbsenceUnproven) || !strings.Contains(err.Error(), want) ||
			!strings.Contains(err.Error(), "Operation not permitted") {
			t.Fatalf("Run error = %v, want ErrAbsenceUnproven naming %q and the cleanup failure", err, want)
		}
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
