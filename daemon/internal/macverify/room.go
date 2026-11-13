package macverify

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/procbound"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

// ErrAbsenceUnproven marks a Run whose slot was not proved free of every
// process once the command was over, including a Run that stopped waiting
// for the root helper. The slot is quarantined. Every other return, an error
// included, proves nothing of the job is still running.
var ErrAbsenceUnproven = errors.New("macverify: slot process absence is unproven")

// defaultTeardownTimeout bounds the reap, the absence proof, and the cleanup
// after a command, which run detached from the step's context, as the ward
// room's cleanup does.
const defaultTeardownTimeout = 2 * time.Minute

// defaultPrepareTimeout bounds prepare, which copies the workspace as root
// and cannot be canceled, so it runs detached from the step's context.
const defaultPrepareTimeout = 15 * time.Minute

// cancelExitGrace is how long, beyond the wait delay, a canceled command may
// take to exit once its slot is reaped.
const cancelExitGrace = 5 * time.Second

const absencePollInterval = 20 * time.Millisecond

// probeTimeout bounds one probe command. Under the sandbox every check fails
// at once; the packet-filter mode waits out two dropped attempts.
const probeTimeout = 2 * time.Minute

// maxProbeReport bounds a probe's report; a longer one is truncated and
// fails closed.
const maxProbeReport = 64 << 10

// The pre-job probe's off-host targets are TEST-NET-1 literals (RFC 5737).
// The sandbox refuses a connect or send with EPERM before routing, so the
// check needs no network and no name, and an unconfined probe's attempt goes
// nowhere and fails the check. The resolver host is a name that resolves
// for the daemon.
const (
	preJobResolverHost = "github.com"
	preJobTCP          = "192.0.2.1:443"
	preJobUDP          = "192.0.2.1:53"
)

// RoomOptions tunes a Room. The zero value selects the defaults.
type RoomOptions struct {
	// MaxOutputBytes bounds the combined output retained per command; zero
	// selects verify.DefaultMaxRoomOutputBytes.
	MaxOutputBytes int64
	// WaitDelay bounds the wait for the command's output pipes after it
	// exits; zero selects verify.DefaultRoomWaitDelay.
	WaitDelay time.Duration
}

// Room is the executor's verify.Room: it runs one trusted recipe command as a
// leased slot account, in the slot's own launchd context, under the
// configured sandbox profile, through the slot-run helper. It is safe for
// concurrent use; each Run leases its own slot.
type Room struct {
	root        string
	profile     []byte
	pool        *slotPool
	launcher    launcher
	helperPath  string
	helperOwner uint32
	maxOutput   int64
	// cancelGrace bounds the wait for a canceled command to exit after its
	// slot is reaped; prepareTimeout and teardownTimeout bound the helper
	// operations around it.
	cancelGrace     time.Duration
	prepareTimeout  time.Duration
	teardownTimeout time.Duration
	hostSupported   bool
	// toolchain is the configuration's pin, and hostToolchain reads the
	// host's (HostToolchain outside tests).
	toolchain     Toolchain
	hostToolchain func(context.Context) (Toolchain, error)
	// swiftCacheMasked makes the room serve no Swift dependency cache, so
	// the live suite can show a job without it attempts the network.
	swiftCacheMasked bool
	// backend's declaration is withdrawn when a pre-job probe finds a
	// confinement broken.
	backend *Backend
	// outsideDirs are where the sandbox probe must be refused writing.
	outsideDirs []string
	// probeWorkspace returns the directory holding the probe executable
	// (the backend's stageProbe outside tests).
	probeWorkspace func() (string, error)
}

var _ verify.Room = (*Room)(nil)

// NewRoom builds the room over b's configuration, so every job runs exactly
// the executor root, profile, and slots the configuration digest names.
// Rooms over one backend share its slot pool. NewRoom builds on every
// platform; off macOS, Run refuses.
func NewRoom(b *Backend, opts RoomOptions) (*Room, error) {
	if b == nil || !b.initialized {
		return nil, fmt.Errorf("%w: backend is not initialized", ErrInvalidConfig)
	}
	maxOutput := opts.MaxOutputBytes
	if maxOutput == 0 {
		maxOutput = verify.DefaultMaxRoomOutputBytes
	}
	if maxOutput < 0 {
		return nil, fmt.Errorf("%w: negative room output cap", ErrInvalidConfig)
	}
	waitDelay := opts.WaitDelay
	if waitDelay == 0 {
		waitDelay = verify.DefaultRoomWaitDelay
	}
	if waitDelay < 0 {
		return nil, fmt.Errorf("%w: negative room wait delay", ErrInvalidConfig)
	}
	helper := helperPath(b.cfg.ExecutorRoot)
	r := &Room{
		root:            b.cfg.ExecutorRoot,
		profile:         b.cfg.SandboxProfile,
		pool:            b.slots,
		launcher:        sudoLauncher{helper: helper, waitDelay: waitDelay},
		helperPath:      helper,
		helperOwner:     0,
		maxOutput:       maxOutput,
		cancelGrace:     waitDelay + cancelExitGrace,
		prepareTimeout:  defaultPrepareTimeout,
		teardownTimeout: defaultTeardownTimeout,
		hostSupported:   hostSupported,
		toolchain:       b.cfg.Toolchain,
		hostToolchain:   HostToolchain,
		backend:         b,
		// Only the sandbox can refuse a slot's write to a world-writable
		// directory, so EPERM there proves the confinement. The daemon
		// user's home proves nothing more: file permissions already refuse
		// the slot there, as EACCES or EPERM depending on the home's mode,
		// and a 0750 home failed the EPERM check outright (#3009).
		outsideDirs: []string{"/private/tmp", "/Users/Shared"},
	}
	r.probeWorkspace = b.stageProbe
	return r, nil
}

// Run executes argv as a leased slot in a copy of workdir. A nonzero exit or
// a signal death (a timeout kill included) is a StepResult; an error means
// the room failed. Cancellation and the timeout kill by the slot's uid,
// because the daemon cannot signal the root helper.
//
// Tier A gates, each checked on every call: the installed helper is the
// embedded one (checkInstalledHelper); the host's Xcode and Swift are the
// configuration's pin (checkToolchain); a workspace holding a
// Package.resolved has a complete Swift dependency cache entry for its key,
// and one holding a go.mod or go.sum has a complete Go module cache entry
// and the pinned Go toolchain, and prepare set the job up from exactly those
// entries (swiftCacheFor, goCacheFor, checkPrepared); the
// leased slot passes the pre-job probe, its sandbox network and write
// confinements, before the command is launched (preJobProbe); the prepared
// job directory is the slot's own at 0700 (checkPrepared); and, before the
// slot is released, no process owned by the slot's uid survives (teardown).
// A slot that fails the last, or whose helper the room stopped waiting for,
// is quarantined and Run returns ErrAbsenceUnproven. A slot that fails the
// pre-job probe is quarantined, the backend's declaration is withdrawn, and
// Run returns the *ConformanceFailure; a probe that could not run (prepare
// failed, the probe could not be staged) fails the command and withdraws
// nothing.
func (r *Room) Run(ctx context.Context, workdir string, argv []string) (verify.StepResult, error) {
	if r == nil || r.launcher == nil || r.pool == nil {
		return verify.StepResult{}, errors.New("macverify room is not initialized")
	}
	if !r.hostSupported {
		return verify.StepResult{}, fmt.Errorf("%w: the room cannot run a recipe command here", ErrHostUnsupported)
	}
	if err := validateArgv(argv); err != nil {
		return verify.StepResult{}, err
	}
	if err := validateWorkdir(workdir); err != nil {
		return verify.StepResult{}, err
	}
	if err := checkInstalledHelper(r.helperPath, r.helperOwner); err != nil {
		return verify.StepResult{}, err
	}
	if err := r.checkToolchain(ctx); err != nil {
		return verify.StepResult{}, err
	}
	swiftCache, err := r.swiftCacheFor(workdir)
	if err != nil {
		return verify.StepResult{}, err
	}
	goCache, err := r.goCacheFor(workdir)
	if err != nil {
		return verify.StepResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return verify.StepResult{}, err
	}
	slot, err := r.pool.lease()
	if err != nil {
		return verify.StepResult{}, err
	}
	if err := r.preJobProbe(ctx, slot); err != nil {
		var failure *ConformanceFailure
		switch {
		case errors.Is(err, ErrAbsenceUnproven):
			// runOnSlot quarantined the slot.
		case errors.As(err, &failure):
			r.pool.quarantine(slot)
			r.backend.withdrawConformance()
		default:
			r.pool.release(slot)
		}
		return verify.StepResult{}, fmt.Errorf("pre-job probe: %w", err)
	}
	result, err := r.runOnSlot(ctx, slot, r.profile, workdir, jobCaches{Swift: swiftCache, Go: goCache}, argv, r.maxOutput)
	if !errors.Is(err, ErrAbsenceUnproven) {
		r.pool.release(slot)
	}
	return result, err
}

// preJobProbe runs the sandbox mode of the probe on slot, under the
// configured profile, against a loopback listener the daemon holds and the
// fixed pre-job targets. A *ConformanceFailure means a confinement failed;
// any other error means the probe could not run.
func (r *Room) preJobProbe(ctx context.Context, slot string) error {
	workspace, err := r.probeWorkspace()
	if err != nil {
		return err
	}
	// Nothing accepts: the kernel completes a handshake to a listening
	// socket on its own, so an unconfined probe's connect would succeed.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("probe listener: %w", err)
	}
	defer func() { _ = listener.Close() }()
	spec := sandboxProbe(sandboxTargets{
		loopbackPort: strconv.Itoa(listener.Addr().(*net.TCPAddr).Port),
		resolverHost: preJobResolverHost,
		tcp:          preJobTCP,
		udp:          preJobUDP,
		outsideDirs:  r.outsideDirs,
	})
	return r.runProbe(ctx, slot, workspace, r.profile, spec)
}

// runProbe runs one probe job on a slot the caller has leased and checks its
// report. It returns runOnSlot's error as it is, a probe the caller canceled
// or probeTimeout cut short as an operational error, and a report that does
// not prove every check as a *ConformanceFailure. The timeout is operational
// because it covers prepare, which a loaded host can slow, while every probe
// check gives up on its own within seconds, confined or not.
func (r *Room) runProbe(ctx context.Context, slot, workspace string, profile []byte, spec probeSpec) error {
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	res, err := r.runOnSlot(probeCtx, slot, profile, workspace, jobCaches{}, spec.argv(), maxProbeReport)
	if err != nil {
		return fmt.Errorf("%s probe: %w", spec.mode, err)
	}
	if err := probeCtx.Err(); err != nil {
		return fmt.Errorf("%s probe did not finish: %w", spec.mode, err)
	}
	return checkProbeReport(slot, spec, res)
}

// runOnSlot runs one job on a slot the caller has leased: prepare, run, and
// tear down. Recipe commands and probes take this one path. On an error
// wrapping ErrAbsenceUnproven it has quarantined the slot; otherwise the
// slot is free of the job and the caller releases it.
func (r *Room) runOnSlot(ctx context.Context, slot string, profile []byte, workdir string, caches jobCaches, argv []string, maxOutput int64) (verify.StepResult, error) {
	jobID, err := newJobID()
	if err != nil {
		return verify.StepResult{}, err
	}
	result, preparedUID, runErr := r.runJob(ctx, slot, jobID, profile, workdir, caches, argv, maxOutput)
	if errors.Is(runErr, errHelperAbandoned) {
		// Tearing down under a live helper would race it, and the teardown
		// could not prove the helper would not touch the slot afterwards.
		r.pool.quarantine(slot)
		return verify.StepResult{}, fmt.Errorf("%w: slot %s: %w", ErrAbsenceUnproven, slot, runErr)
	}
	if err := r.teardown(ctx, slot, jobID, preparedUID); err != nil {
		r.pool.quarantine(slot)
		if runErr == nil {
			// The result is dropped with the slot, so the error keeps the
			// one fact an operator needs to tell a failed command from a
			// failed teardown.
			return verify.StepResult{}, fmt.Errorf("%w: slot %s: after the command exited %d: %w",
				ErrAbsenceUnproven, slot, result.ExitCode, err)
		}
		return verify.StepResult{}, fmt.Errorf("%w: slot %s: %w", ErrAbsenceUnproven, slot, errors.Join(err, runErr))
	}
	if runErr != nil {
		return verify.StepResult{}, runErr
	}
	return result, nil
}

// runJob prepares the slot and runs the command. It returns the uid prepare
// reported (zero if prepare failed), for the teardown to cross-check. An
// error wrapping errHelperAbandoned means a helper may still be running.
func (r *Room) runJob(ctx context.Context, slot, jobID string, profile []byte, workdir string, caches jobCaches, argv []string, maxOutput int64) (verify.StepResult, int, error) {
	prepareCtx, cancelPrepare := context.WithTimeout(context.WithoutCancel(ctx), r.prepareTimeout)
	prepared, err := r.launcher.prepare(prepareCtx, slot, jobID, workdir, profile, caches)
	cancelPrepare()
	if err != nil {
		return verify.StepResult{}, 0, fmt.Errorf("prepare slot %s: %w", slot, err)
	}
	jobDir := filepath.Join(r.root, "jobs", slot, jobID)
	if err := r.checkPrepared(slot, jobDir, caches, prepared); err != nil {
		return verify.StepResult{}, prepared.UID, err
	}
	if err := ctx.Err(); err != nil {
		return verify.StepResult{}, prepared.UID, fmt.Errorf("canceled while preparing slot %s: %w", slot, err)
	}
	req := runRequest{
		Slot:   slot,
		JobID:  jobID,
		Env:    commandEnv(prepared),
		Params: sandboxParams(jobDir, prepared),
		Argv:   slices.Clone(argv),
	}
	out := procbound.NewTailBuffer(maxOutput)
	done, err := r.launcher.run(req, out)
	if err != nil {
		return verify.StepResult{}, prepared.UID, fmt.Errorf("launch in slot %s: %w", slot, err)
	}
	var outcome runOutcome
	select {
	case outcome = <-done:
	case <-ctx.Done():
		reapCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.teardownTimeout)
		_, reapErr := r.launcher.reap(reapCtx, slot)
		cancel()
		timer := time.NewTimer(r.cancelGrace)
		defer timer.Stop()
		select {
		case outcome = <-done:
		case <-timer.C:
			// The helper chain is still alive and could yet start the
			// command, so the slot cannot be released. The output buffer
			// may still be written; it is not read.
			return verify.StepResult{}, prepared.UID, fmt.Errorf(
				"canceled command in slot %s did not exit: %w", slot,
				errors.Join(errHelperAbandoned, ctx.Err(), reapErr))
		}
	}
	if outcome.Err != nil {
		return verify.StepResult{}, prepared.UID, fmt.Errorf("run in slot %s: %w", slot, outcome.Err)
	}
	return verify.StepResult{ExitCode: outcome.ExitCode, Output: out.Bytes(), Truncated: out.Truncated()},
		prepared.UID, nil
}

// checkPrepared re-checks what prepare reported against this room's
// configuration, the dependency cache entries the room chose, and the job
// directory on disk, which the daemon can inspect without privilege: the
// slot's own at 0700, not a symlink.
func (r *Room) checkPrepared(slot, jobDir string, caches jobCaches, p preparedJob) error {
	if p.Root != r.root {
		return fmt.Errorf("slot-run acted on executor root %s, configured %s", p.Root, r.root)
	}
	if p.Caches.Swift != caches.Swift {
		return fmt.Errorf("slot-run seeded Swift cache %q, want %q", p.Caches.Swift, caches.Swift)
	}
	if p.Caches.Go != caches.Go {
		return fmt.Errorf("slot-run set up Go cache %q, want %q", p.Caches.Go, caches.Go)
	}
	if want := filepath.Join(r.root, "homes", slot); p.Home != want {
		return fmt.Errorf("slot-run prepared home %s for slot %s, want %s", p.Home, slot, want)
	}
	info, err := os.Lstat(jobDir)
	if err != nil {
		return fmt.Errorf("inspect job directory: %w", err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || int(st.Uid) != p.UID || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("job directory %s is %v, not slot uid %d's directory at 0700", jobDir, info.Mode(), p.UID)
	}
	return nil
}

// checkToolchain refuses the job unless the host's Xcode and Swift are the
// configuration's pin, so a toolchain the conformance pass did not prove
// never runs a recipe.
func (r *Room) checkToolchain(ctx context.Context) error {
	host, err := r.hostToolchain(ctx)
	if err != nil {
		return err
	}
	if host != r.toolchain {
		return fmt.Errorf("%w: host has %s, configuration pins %s", ErrToolchainMismatch, host, r.toolchain)
	}
	return nil
}

// swiftCacheFor returns the Swift dependency cache key prepare seeds the job
// from, or "" for a workspace with no Package.resolved. A workspace whose
// key has no complete entry is refused with ErrSwiftCacheMissing before any
// slot is leased.
func (r *Room) swiftCacheFor(workdir string) (string, error) {
	key, err := workspaceSwiftCacheKey(workdir)
	if err != nil {
		return "", err
	}
	if key.Key == "" || r.swiftCacheMasked {
		return "", nil
	}
	if err := lookupSwiftCache(r.root, key); err != nil {
		return "", err
	}
	return key.Key, nil
}

// goCacheFor returns the Go module cache key the job reads its modules
// from, or "" for a workspace with no go.mod or go.sum. A workspace whose
// key has no complete entry, or a host without the pinned toolchain, is
// refused with ErrGoCacheMissing before any slot is leased.
func (r *Room) goCacheFor(workdir string) (string, error) {
	key, err := workspaceGoCacheKey(workdir)
	if err != nil {
		return "", err
	}
	if key.Key == "" {
		return "", nil
	}
	if err := lookupGoCache(r.root, key); err != nil {
		return "", err
	}
	return key.Key, nil
}

// teardown proves the slot is free again: reap, wait until the slot's uid
// owns no process, clean up, and check once more, since cleanup itself runs
// short-lived processes as the slot.
func (r *Room) teardown(ctx context.Context, slot, jobID string, preparedUID int) error {
	tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.teardownTimeout)
	defer cancel()
	uid, err := r.launcher.reap(tctx, slot)
	if err != nil {
		return fmt.Errorf("reap: %w", err)
	}
	if preparedUID != 0 && uid != preparedUID {
		return fmt.Errorf("slot %s reaped as uid %d, prepared as uid %d", slot, uid, preparedUID)
	}
	if err := r.awaitAbsence(tctx, uid); err != nil {
		return err
	}
	if err := r.launcher.cleanup(tctx, slot, jobID); err != nil {
		return fmt.Errorf("clean up: %w", err)
	}
	return r.awaitAbsence(tctx, uid)
}

func (r *Room) awaitAbsence(ctx context.Context, uid int) error {
	ticker := time.NewTicker(absencePollInterval)
	defer ticker.Stop()
	for {
		live, err := r.launcher.ownsProcess(uid)
		if err != nil {
			return err
		}
		if !live {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("uid %d still owns a process: %w", uid, ctx.Err())
		case <-ticker.C:
		}
	}
}

// commandEnv is the command's whole environment: nothing is inherited, and
// none of it is a credential. A job with a Go cache entry gets the pinned
// toolchain first on its PATH and Go held to that toolchain and the entry,
// offline (slot-run's run derives the same values).
func commandEnv(p preparedJob) []string {
	if p.Caches.Go == "" {
		return []string{"HOME=" + p.Home, "TMPDIR=" + p.Tmp, "PATH=" + commandPath, "LC_ALL=C"}
	}
	return []string{
		"HOME=" + p.Home, "TMPDIR=" + p.Tmp,
		"PATH=" + filepath.Join(goToolchainRoot(p.Root), "bin") + ":" + commandPath, "LC_ALL=C",
		"GOFLAGS=-mod=readonly", "GOMODCACHE=" + filepath.Join(goCacheEntry(p.Root, p.Caches.Go), goCacheModcache),
		"GOPROXY=off", "GOTOOLCHAIN=local", "GOWORK=off",
	}
}

// sandboxParams are the sandbox-exec -D parameters the profile confines
// writes to.
func sandboxParams(jobDir string, p preparedJob) []string {
	return []string{"JOB_DIR=" + jobDir, "SLOT_HOME=" + p.Home, "SLOT_TMP=" + p.Tmp, "SLOT_CACHE=" + p.Cache}
}

func validateArgv(argv []string) error {
	if len(argv) == 0 || argv[0] == "" {
		return fmt.Errorf("%w: empty command", verify.ErrInvalidOptions)
	}
	for i, token := range argv {
		if strings.ContainsRune(token, 0) {
			return fmt.Errorf("%w: argv[%d] contains NUL", verify.ErrInvalidOptions, i)
		}
	}
	return nil
}

func validateWorkdir(workdir string) error {
	if !filepath.IsAbs(workdir) || filepath.Clean(workdir) != workdir {
		return fmt.Errorf("%w: workspace %q is not a clean absolute path", verify.ErrInvalidOptions, workdir)
	}
	info, err := os.Lstat(workdir)
	if err != nil {
		return fmt.Errorf("inspect workspace: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: workspace %s is not a directory", verify.ErrInvalidOptions, workdir)
	}
	return nil
}

// newJobID is 32 lowercase hex characters, the only job id slot-run accepts.
func newJobID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("job id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
