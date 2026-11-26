package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/cordon"
	"github.com/freeside-ai/freeside/daemon/internal/cordonhost"
	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
)

// runCordonMain answers the cordon's hidden verbs. probe is the
// conformance probe a cordoned probe launch runs, from a copy of this
// binary, so the probe is the build the conformance record names; it prints
// only its report. supervise is the process that owns one cordoned launch:
// the daemon starts it in its own session so the launch outlives a daemon
// restart. serve is the host launcher, run as a LaunchAgent in a run
// account's desktop session: it serves the backend and its suite to
// clients on the same host. None needs a daemon root.
func runCordonMain(args []string) {
	if len(args) > 0 {
		switch args[0] {
		case "probe":
			os.Exit(cordon.RunProbe(args[1:], os.Stdout))
		case "supervise":
			os.Exit(cordon.RunSupervisor(args[1:]))
		case "serve":
			os.Exit(runCordonServe(args[1:]))
		}
	}
	fmt.Fprintln(os.Stderr, "usage: freesided cordon probe <spec-json>")
	fmt.Fprintln(os.Stderr, "       freesided cordon supervise <runs-root> <run-id>")
	fmt.Fprintln(os.Stderr, "       freesided cordon serve -control-root DIR -socket PATH -launch-agent-plist PATH [cordon flags]")
	os.Exit(2)
}

// runCordonServe runs the host launcher until SIGTERM or SIGINT. It
// builds the backend's configuration with the daemon's own builder, the
// control root standing in for the state directory, and refuses to serve
// from an install the run account could change (cordonhost.CheckInstall).
// Its backend has no forge seeder, and its suite also proves a run can't
// reach its socket or write its binary or plist.
//
// Tier A: the flags here choose the configuration a launcher serves and
// the paths its install check and suite hold to.
func runCordonServe(args []string) int {
	flags := flag.NewFlagSet("freesided cordon serve", flag.ContinueOnError)
	controlRoot := flags.String("control-root", "",
		"the launcher's own directory: its runs, staged seeds, and socket live under it, and a launch can't reach it")
	socket := flags.String("socket", "", "the Unix socket to serve on, under -control-root or a -cordon-protected-root")
	plist := flags.String("launch-agent-plist", "", "the LaunchAgent plist that starts this launcher, which must be root-owned")
	backendConfig := cordonBackendFlags(flags)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *controlRoot == "" || *socket == "" || *plist == "" {
		fmt.Fprintln(os.Stderr, "freesided cordon serve: -control-root, -socket, and -launch-agent-plist are required")
		return 2
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := serveCordon(*controlRoot, *socket, *plist, backendConfig(), logger); err != nil {
		logger.Error("cordon launcher", "error", err)
		return 1
	}
	return 0
}

func serveCordon(controlRoot, socket, plist string, backendConfig daemoncompose.CordonBackendConfig, logger *slog.Logger) error {
	account, err := daemoncompose.LookupCordonAccount()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(controlRoot, 0o700); err != nil {
		return fmt.Errorf("control root: %w", err)
	}
	cfg, err := daemoncompose.CordonConfig(backendConfig, account, controlRoot, nil, nil)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		return fmt.Errorf("socket directory: %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find the launcher's binary: %w", err)
	}
	if err := cordonhost.CheckInstall(cordonhost.Install{
		Binary: exe, Plist: plist, Socket: socket, ProtectedRoots: cfg.ProtectedRoots,
	}); err != nil {
		return err
	}
	// The suite probes the resolved paths, which are what the sandbox
	// matches; the install check has already checked both forms.
	binary, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}
	resolvedPlist, err := filepath.EvalSymlinks(plist)
	if err != nil {
		return err
	}
	resolvedSocket, err := filepath.EvalSymlinks(filepath.Dir(socket))
	if err != nil {
		return err
	}
	resolvedSocket = filepath.Join(resolvedSocket, filepath.Base(socket))
	backend, err := cordon.New(cfg, cordonhost.NoForgeSeeder{})
	if err != nil {
		return err
	}
	suite, err := cordon.NewSuite(backend, cordon.WithConformanceRecorder(cordonhost.ConnectionRecorder()),
		cordon.WithLauncher(cordon.Launcher{Socket: resolvedSocket, Binary: binary, Plist: resolvedPlist}))
	if err != nil {
		return err
	}
	me, err := user.Current()
	if err != nil {
		return err
	}
	uid, err := strconv.Atoi(me.Uid)
	if err != nil {
		return fmt.Errorf("the run account's uid %q: %w", me.Uid, err)
	}
	server, err := cordonhost.NewServer(cordonhost.ServerConfig{
		Build: buildVersion(), Backend: backend, Suite: suite, Account: cordonhost.Account{Name: me.Username, UID: uid},
		SeedRoot: filepath.Join(controlRoot, "seeds"), Logger: logger,
	})
	if err != nil {
		return err
	}
	ln, err := listenCordonSocket(socket)
	if err != nil {
		return err
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-signals
		// Detaching leaves every run to its supervisor, for the next
		// launcher to recover.
		server.Close()
	}()
	logger.Info("cordon launcher serving", "socket", socket, "build", buildVersion(), "digest", backend.ConfigurationDigest())
	return server.Serve(ln)
}

// listenCordonSocket serves on path, first removing a socket an earlier
// launcher left there, and only a socket.
func listenCordonSocket(path string) (net.Listener, error) {
	info, err := os.Lstat(path)
	switch {
	case err == nil && info.Mode()&fs.ModeSocket != 0:
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove an earlier launcher's socket: %w", err)
		}
	case err == nil:
		return nil, fmt.Errorf("%s exists and is not a socket", path)
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

// cordonBackendFlags registers the cordoned launch backend's flags, which
// take effect with -driver claude. An empty -cordon-agent-cli configures
// none.
func cordonBackendFlags(flags *flag.FlagSet) func() daemoncompose.CordonBackendConfig {
	agentCLI := flags.String("cordon-agent-cli", "",
		"with -driver claude: absolute path of the pinned agent CLI a cordoned launch runs; empty (default) configures no cordon")
	agentCLISHA256 := flags.String("cordon-agent-cli-sha256", "",
		"SHA-256 of -cordon-agent-cli in lowercase hex; required with it")
	projects := flags.String("cordon-projects", "",
		"directory holding the owner's other projects, whose .env files a launch can't read; required with -cordon-agent-cli")
	var protectedRoots repeatedStringFlag
	flags.Var(&protectedRoots, "cordon-protected-root",
		"directory a cordoned launch can't list, read, write, or reach a socket under, besides -state-dir; repeatable")
	stallLimit := flags.Duration("cordon-stall-limit", 10*time.Minute,
		"end a cordoned launch whose output has been silent this long")
	endGrace := flags.Duration("cordon-end-grace", 10*time.Second,
		"time a cordoned launch being ended has between SIGTERM and SIGKILL")
	endProofTimeout := flags.Duration("cordon-end-proof-timeout", time.Minute,
		"bound on the proof that no process of a cordoned launch is alive; a launch it can't prove ended is blocked")
	return func() daemoncompose.CordonBackendConfig {
		return daemoncompose.CordonBackendConfig{
			AgentCLI: *agentCLI, AgentCLISHA256: *agentCLISHA256, Projects: *projects,
			ProtectedRoots: protectedRoots,
			StallLimit:     *stallLimit, EndGrace: *endGrace, EndProofTimeout: *endProofTimeout,
		}
	}
}
