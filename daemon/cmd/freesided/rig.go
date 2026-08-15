package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	osexec "os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec/claude"
	"github.com/freeside-ai/freeside/daemon/internal/procbound"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

const defaultRigLaunchAgentLabel = "ai.freeside.daemon"

const launchctlServiceNotFoundExitCode = 113

var errRigUsage = errors.New("invalid rig command")

var errRigCleanRelease = errors.New("clean rig release requested")

type rigHoldOutput struct {
	Token    string                 `json:"token"`
	Manifest daemonlock.RigManifest `json:"manifest"`
}

type rigHost interface {
	ProbeDaemon(context.Context, string) (description string, live bool, err error)
	SupervisedDaemon(context.Context, string) (bool, error)
	PresentContainers(context.Context, string, []string) ([]string, error)
	DeleteContainers(context.Context, string, []string) error
	PresentPersistentResources(context.Context, string, []string, []string) ([]string, []string, error)
	RuntimeCLIActive(context.Context, string) (bool, error)
}

type productionRigHost struct {
	client *http.Client
}

func newProductionRigHost() productionRigHost {
	return productionRigHost{client: &http.Client{Timeout: 2 * time.Second}}
}

func (h productionRigHost) ProbeDaemon(
	ctx context.Context, address string,
) (string, bool, error) {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", false, fmt.Errorf("parse rig listen address %q: %w", address, err)
	}
	if port == "0" {
		return "", false, nil
	}
	connection, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("probe rig listen address %q: %w", address, err)
	}
	_ = connection.Close()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/health", nil)
	if err != nil {
		return "", false, err
	}
	response, err := h.client.Do(request)
	if err != nil {
		return "process accepting connections", true, nil
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Sprintf("process answering HTTP %d", response.StatusCode), true, nil
	}
	var health struct {
		Status    string    `json:"status"`
		Version   string    `json:"version"`
		StartedAt time.Time `json:"started_at"`
	}
	if err := strictjson.DecodeReader(
		response.Body, &health, strictjson.RejectInvalidUTF8, strictjson.Limit(16<<10),
	); err != nil || health.Status != "ok" || health.Version == "" || health.StartedAt.IsZero() {
		return "process answering on the Freeside health path", true, nil
	}
	return fmt.Sprintf("freesided version=%s started_at=%s",
		health.Version, health.StartedAt.UTC().Format(time.RFC3339)), true, nil
}

func (productionRigHost) SupervisedDaemon(ctx context.Context, label string) (bool, error) {
	uid := os.Getuid()
	// #nosec G204 -- launchctl receives fixed arguments directly, without a shell;
	// label selects the operator-supplied read-only service lookup.
	command := osexec.CommandContext(ctx, "launchctl", "print", fmt.Sprintf("gui/%d/%s", uid, label))
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	err := procbound.Run(command, procbound.DefaultWaitDelay)
	if err == nil {
		return true, nil
	}
	var exitErr *osexec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == launchctlServiceNotFoundExitCode {
		return false, nil
	}
	return false, fmt.Errorf("inspect launchd service %q: %w", label, err)
}

func (productionRigHost) PresentContainers(
	ctx context.Context, executable string, names []string,
) ([]string, error) {
	containers, err := ward.NewCLIRuntime(executable).ListContainers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list containers for rig cleanup: %w", err)
	}
	listed := make(map[string]bool, len(containers))
	for _, container := range containers {
		if listed[container.ID] {
			return nil, fmt.Errorf("container listing duplicated identity %q", container.ID)
		}
		listed[container.ID] = true
	}
	present := make([]string, 0, len(names))
	for _, name := range names {
		if listed[name] {
			present = append(present, name)
		}
	}
	return present, nil
}

func (productionRigHost) DeleteContainers(
	ctx context.Context, executable string, names []string,
) error {
	runtime := ward.NewCLIRuntime(executable)
	containers, err := runtime.ListContainers(ctx)
	if err != nil {
		return fmt.Errorf("list containers before rig cleanup: %w", err)
	}
	wanted := make(map[string]bool, len(names))
	for _, name := range names {
		wanted[name] = true
	}
	matched := make(map[string]ward.ContainerState, len(names))
	for _, container := range containers {
		if !wanted[container.ID] {
			continue
		}
		if _, exists := matched[container.ID]; exists {
			return fmt.Errorf("container listing duplicated recorded identity %q", container.ID)
		}
		switch container.State {
		case ward.StateRunning, ward.StateStopped:
			matched[container.ID] = container.State
		default:
			return fmt.Errorf("recorded container %q has unknown state %q", container.ID, container.State)
		}
	}
	for _, name := range names {
		state, exists := matched[name]
		if !exists {
			continue
		}
		if state == ward.StateRunning {
			if err := runtime.StopContainer(ctx, name); err != nil {
				return fmt.Errorf("stop recorded container %q: %w", name, err)
			}
		}
		if err := runtime.DeleteContainer(ctx, name); err != nil {
			return fmt.Errorf("delete recorded container %q: %w", name, err)
		}
	}
	return nil
}

func (productionRigHost) PresentPersistentResources(
	ctx context.Context, executable string, volumeNames, networkNames []string,
) ([]string, []string, error) {
	runtime := ward.NewCLIRuntime(executable)
	volumes, err := runtime.ListVolumes(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list volumes for rig cleanup: %w", err)
	}
	networks, err := runtime.ListNetworks(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list networks for rig cleanup: %w", err)
	}
	presentVolumes, err := selectRigResourceNames(volumeNames, func(yield func(string) bool) {
		for _, volume := range volumes {
			if !yield(volume.Name) {
				return
			}
		}
	})
	if err != nil {
		return nil, nil, err
	}
	presentNetworks, err := selectRigResourceNames(networkNames, func(yield func(string) bool) {
		for _, network := range networks {
			if !yield(network.Name) {
				return
			}
		}
	})
	return presentVolumes, presentNetworks, err
}

func (productionRigHost) RuntimeCLIActive(ctx context.Context, executable string) (bool, error) {
	pattern := `^(.*/)?` + regexp.QuoteMeta(filepath.Base(executable)) + `([[:space:]]|$)`
	// #nosec G204 -- pgrep receives fixed flags and an escaped executable basename, never a shell string.
	command := osexec.CommandContext(ctx, "pgrep", "-f", pattern)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	err := procbound.Run(command, procbound.DefaultWaitDelay)
	if err == nil {
		return true, nil
	}
	var exitErr *osexec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("inspect runtime CLI processes: %w", err)
}

func selectRigResourceNames(
	wanted []string, observed func(func(string) bool),
) ([]string, error) {
	allowed := make(map[string]bool, len(wanted))
	for _, name := range wanted {
		allowed[name] = true
	}
	seen := make(map[string]bool, len(wanted))
	var present []string
	var duplicate string
	observed(func(name string) bool {
		if seen[name] {
			duplicate = name
			return false
		}
		seen[name] = true
		if allowed[name] {
			present = append(present, name)
		}
		return true
	})
	if duplicate != "" {
		return nil, fmt.Errorf("runtime listing duplicated identity %q", duplicate)
	}
	sort.Strings(present)
	return present, nil
}

func runRigMain(args []string) {
	ctx, stop := rigSignalContext(args)
	defer stop()
	err := runRigCommand(ctx, args, os.Stdout, os.Stderr, newProductionRigHost())
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "freesided rig:", err)
	if errors.Is(err, errRigUsage) {
		os.Exit(2)
	}
	os.Exit(1)
}

func rigSignalContext(args []string) (context.Context, context.CancelFunc) {
	if len(args) == 0 || args[0] != "hold" {
		return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	}
	ctx, cancelCause := context.WithCancelCause(context.Background())
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1)
	go func() {
		sig := <-signals
		if sig == syscall.SIGUSR1 {
			cancelCause(errRigCleanRelease)
			return
		}
		cancelCause(context.Canceled)
	}()
	return ctx, func() {
		signal.Stop(signals)
		cancelCause(context.Canceled)
	}
}

func runRigCommand(
	ctx context.Context, args []string, stdout, stderr io.Writer, host rigHost,
) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: expected hold, check, resource, bind, cleanup, or recover", errRigUsage)
	}
	switch args[0] {
	case "hold":
		return runRigHold(ctx, args[1:], stdout, stderr, host)
	case "check":
		return runRigCheck(args[1:], stderr)
	case "resource":
		return runRigResource(args[1:], stdout, stderr)
	case "bind":
		return runRigBind(args[1:], stdout, stderr)
	case "cleanup":
		return runRigCleanup(ctx, args[1:], stdout, stderr, host)
	case "recover":
		return runRigRecover(ctx, args[1:], stdout, stderr, host)
	default:
		return fmt.Errorf("%w: unknown subcommand %q", errRigUsage, args[0])
	}
}

func runRigHold(
	ctx context.Context, args []string, stdout, stderr io.Writer, host rigHost,
) (err error) {
	return runRigHoldWithLeaseRoot(ctx, args, stdout, stderr, host, "")
}

func runRigHoldWithLeaseRoot(
	ctx context.Context, args []string, stdout, stderr io.Writer, host rigHost, leaseRoot string,
) (err error) {
	flags := flag.NewFlagSet("freesided rig hold", flag.ContinueOnError)
	flags.SetOutput(stderr)
	stateRoot := flags.String("state-root", "", "production daemon state root (required)")
	databasePath := flags.String("db", "", "production SQLite database path (required)")
	listenAddress := flags.String("listen", "", "production daemon listen address (required)")
	seedRoot := flags.String("seed-root", "", "production exact-base seed root (required)")
	ownerNote := flags.String("owner-note", "production acceptance harness", "inspectable owner note")
	launchAgentLabel := flags.String("launch-agent-label", defaultRigLaunchAgentLabel, "supervised daemon label")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errRigUsage, err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%w: unexpected positional arguments: %v", errRigUsage, flags.Args())
	}
	if *stateRoot == "" || *databasePath == "" || *listenAddress == "" || *seedRoot == "" {
		return fmt.Errorf("%w: -state-root, -db, -listen, and -seed-root are required", errRigUsage)
	}
	currentUser, err := user.Current()
	if err != nil {
		return fmt.Errorf("identify rig owner user: %w", err)
	}
	hostname, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("identify rig owner host: %w", err)
	}
	lease, err := daemonlock.AcquireRig(daemonlock.RigAcquireConfig{
		Owner: daemonlock.RigOwner{
			User: currentUser.Username, Host: hostname, PID: os.Getpid(), Note: *ownerNote,
		},
		StateRoot: *stateRoot, DatabasePath: *databasePath,
		ListenAddress: *listenAddress, SeedRoot: *seedRoot, LeaseRoot: leaseRoot,
	})
	if err != nil {
		return err
	}
	published := false
	cleanRelease := false
	defer func() {
		if !published || cleanRelease {
			err = errors.Join(err, lease.Close())
			return
		}
		err = errors.Join(err, lease.Abandon())
	}()
	databaseLock, err := daemonlock.Acquire(lease.Manifest().Resources.DatabasePath)
	if errors.Is(err, daemonlock.ErrAlreadyRunning) {
		return fmt.Errorf("database is already held by a daemon: %w", err)
	}
	if err != nil {
		return fmt.Errorf("prove rig database is idle: %w", err)
	}
	if err := databaseLock.Close(); err != nil {
		return fmt.Errorf("release rig database probe: %w", err)
	}
	if supervised, err := host.SupervisedDaemon(ctx, *launchAgentLabel); err != nil {
		return err
	} else if supervised {
		return fmt.Errorf("supervised daemon %q is loaded; stop it before starting the production rig", *launchAgentLabel)
	}
	canonicalListen := lease.Manifest().Resources.ListenAddress
	if description, live, err := host.ProbeDaemon(ctx, canonicalListen); err != nil {
		return err
	} else if live {
		return fmt.Errorf("listen address %q is already occupied by %s", canonicalListen, description)
	}
	if err := json.NewEncoder(stdout).Encode(rigHoldOutput{
		Token: lease.Token(), Manifest: lease.Manifest(),
	}); err != nil {
		return fmt.Errorf("write rig acquisition: %w", err)
	}
	published = true
	<-ctx.Done()
	cleanRelease = errors.Is(context.Cause(ctx), errRigCleanRelease)
	return nil
}

type repeatedStringFlag []string

func (f *repeatedStringFlag) String() string { return strings.Join(*f, ",") }

func (f *repeatedStringFlag) Set(value string) error {
	if value == "" {
		return errors.New("value is empty")
	}
	*f = append(*f, value)
	return nil
}

func runRigBind(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("freesided rig bind", flag.ContinueOnError)
	flags.SetOutput(stderr)
	stateRoot := flags.String("state-root", "", "production daemon state root (required)")
	tokenFile := flags.String("token-file", "", "rig hold acquisition JSON (required)")
	var invocations repeatedStringFlag
	flags.Var(&invocations, "invocation", "submitted invocation identity to bind (repeatable)")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errRigUsage, err)
	}
	if flags.NArg() != 0 || *stateRoot == "" || *tokenFile == "" || len(invocations) == 0 {
		return fmt.Errorf("%w: -state-root, -token-file, and at least one -invocation are required", errRigUsage)
	}
	token, err := readRigToken(*tokenFile)
	if err != nil {
		return err
	}
	var containers, volumes, networks []string
	for _, invocation := range invocations {
		invocationID := domain.InvocationID(invocation)
		names := ward.RuntimeResourceNamesFor(claude.RunIDFor(invocationID))
		containers = append(containers, names.Containers...)
		containers = append(containers, ward.PreJobContainerNameForInvocation(invocationID))
		volumes = append(volumes, names.Volumes...)
		networks = append(networks, names.Networks...)
	}
	manifest, err := daemonlock.BindRigRuntimeResources(
		*stateRoot, token, containers, volumes, networks,
	)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(manifest)
}

func readRigToken(path string) (string, error) {
	// #nosec G304 -- the operator supplies the private acquisition file emitted by rig hold.
	body, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read rig token file: %w", err)
	}
	var acquisition rigHoldOutput
	if err := strictjson.Decode(
		body, &acquisition, strictjson.RejectInvalidUTF8, strictjson.Limit(128<<10),
	); err != nil {
		return "", fmt.Errorf("decode rig token file: %w", err)
	}
	if acquisition.Token == "" {
		return "", errors.New("rig token file has no token")
	}
	return acquisition.Token, nil
}

func runRigResource(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("freesided rig resource", flag.ContinueOnError)
	flags.SetOutput(stderr)
	tokenFile := flags.String("token-file", "", "rig hold acquisition JSON (required)")
	name := flags.String("name", "", "state-root, database-path, listen-address, or seed-root")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errRigUsage, err)
	}
	if flags.NArg() != 0 || *tokenFile == "" || *name == "" {
		return fmt.Errorf("%w: -token-file and -name are required", errRigUsage)
	}
	// The acquisition manifest supplies only the canonical lookup root. The
	// live manifest authenticated by the secret is the resource authority.
	body, err := os.ReadFile(*tokenFile) // #nosec G304 -- operator-supplied private acquisition file.
	if err != nil {
		return fmt.Errorf("read rig token file: %w", err)
	}
	var acquisition rigHoldOutput
	if err := strictjson.Decode(
		body, &acquisition, strictjson.RejectInvalidUTF8, strictjson.Limit(128<<10),
	); err != nil {
		return fmt.Errorf("decode rig token file: %w", err)
	}
	manifest, err := daemonlock.AuthenticateRig(
		acquisition.Manifest.Resources.StateRoot, acquisition.Token,
	)
	if err != nil {
		return err
	}
	var value string
	switch *name {
	case "state-root":
		value = manifest.Resources.StateRoot
	case "database-path":
		value = manifest.Resources.DatabasePath
	case "listen-address":
		value = manifest.Resources.ListenAddress
	case "seed-root":
		value = manifest.Resources.SeedRoot
	default:
		return fmt.Errorf("%w: unknown rig resource %q", errRigUsage, *name)
	}
	_, err = fmt.Fprintln(stdout, value)
	return err
}

func runRigCheck(args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("freesided rig check", flag.ContinueOnError)
	flags.SetOutput(stderr)
	stateRoot := flags.String("state-root", "", "production daemon state root (required)")
	tokenFile := flags.String("token-file", "", "rig hold acquisition JSON (required)")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errRigUsage, err)
	}
	if flags.NArg() != 0 || *stateRoot == "" || *tokenFile == "" {
		return fmt.Errorf("%w: -state-root and -token-file are required", errRigUsage)
	}
	token, err := readRigToken(*tokenFile)
	if err != nil {
		return err
	}
	_, err = daemonlock.AuthenticateRig(*stateRoot, token)
	return err
}

func runRigCleanup(
	ctx context.Context, args []string, _ io.Writer, stderr io.Writer, host rigHost,
) (err error) {
	flags := flag.NewFlagSet("freesided rig cleanup", flag.ContinueOnError)
	flags.SetOutput(stderr)
	stateRoot := flags.String("state-root", "", "production daemon state root (required)")
	tokenFile := flags.String("token-file", "", "rig hold acquisition JSON (required)")
	containerBin := flags.String("container-bin", "container", "Apple container executable")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errRigUsage, err)
	}
	if flags.NArg() != 0 || *stateRoot == "" || *tokenFile == "" {
		return fmt.Errorf("%w: -state-root and -token-file are required", errRigUsage)
	}
	token, err := readRigToken(*tokenFile)
	if err != nil {
		return err
	}
	authorization, err := daemonlock.AuthorizeRig(*stateRoot, token)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, authorization.Close())
	}()
	manifest := authorization.Manifest()
	databaseLock, err := daemonlock.Acquire(manifest.Resources.DatabasePath)
	if errors.Is(err, daemonlock.ErrAlreadyRunning) {
		return fmt.Errorf("recorded database %q is still held by a daemon: %w",
			manifest.Resources.DatabasePath, err)
	}
	if err != nil {
		return fmt.Errorf("prove recorded database is idle before cleanup: %w", err)
	}
	defer func() {
		err = errors.Join(err, databaseLock.Close())
	}()
	if err := cleanupRigContainers(ctx, host, *containerBin, manifest.Resources.Containers); err != nil {
		return err
	}
	return requireRigResourcesAbsent(ctx, host, *containerBin, manifest.Resources)
}

func cleanupRigContainers(
	ctx context.Context, host rigHost, containerBin string, names []string,
) error {
	return host.DeleteContainers(ctx, containerBin, names)
}

func requireRigResourcesAbsent(
	ctx context.Context, host rigHost, containerBin string, resources daemonlock.RigResources,
) error {
	active, err := host.RuntimeCLIActive(ctx, containerBin)
	if err != nil {
		return err
	}
	if active {
		return errors.New("runtime CLI process is still active; wait for it before clearing the rig gate")
	}
	containers, err := host.PresentContainers(ctx, containerBin, resources.Containers)
	if err != nil {
		return err
	}
	volumes, networks, err := host.PresentPersistentResources(
		ctx, containerBin, resources.Volumes, resources.Networks,
	)
	if err != nil {
		return err
	}
	if len(containers) != 0 || len(volumes) != 0 || len(networks) != 0 {
		return fmt.Errorf(
			"recorded ward resources remain; recover their owning journal first: containers=%q volumes=%q networks=%q",
			containers, volumes, networks,
		)
	}
	return nil
}

func runRigRecover(
	ctx context.Context, args []string, stdout, stderr io.Writer, host rigHost,
) (err error) {
	flags := flag.NewFlagSet("freesided rig recover", flag.ContinueOnError)
	flags.SetOutput(stderr)
	stateRoot := flags.String("state-root", "", "production daemon state root (required)")
	confirm := flags.Bool("confirm", false, "confirm cleanup of exactly the recorded stale resources")
	containerBin := flags.String("container-bin", "container", "Apple container executable")
	launchAgentLabel := flags.String("launch-agent-label", defaultRigLaunchAgentLabel, "supervised daemon label")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errRigUsage, err)
	}
	if flags.NArg() != 0 || *stateRoot == "" {
		return fmt.Errorf("%w: -state-root is required", errRigUsage)
	}
	lease, err := daemonlock.AcquireStaleRig(*stateRoot)
	if err != nil {
		return err
	}
	completed := false
	defer func() {
		if !completed {
			err = errors.Join(err, lease.Abandon())
		}
	}()
	manifest := lease.Manifest()
	if supervised, err := host.SupervisedDaemon(ctx, *launchAgentLabel); err != nil {
		return err
	} else if supervised {
		return fmt.Errorf("supervised daemon %q is loaded; stop it before recovery", *launchAgentLabel)
	}
	databaseLock, err := daemonlock.Acquire(manifest.Resources.DatabasePath)
	if errors.Is(err, daemonlock.ErrAlreadyRunning) {
		return fmt.Errorf("recorded database %q is still held by a daemon: %w",
			manifest.Resources.DatabasePath, err)
	}
	if err != nil {
		return fmt.Errorf("prove recorded database is idle: %w", err)
	}
	defer func() {
		err = errors.Join(err, databaseLock.Close())
	}()
	if description, live, err := host.ProbeDaemon(ctx, manifest.Resources.ListenAddress); err != nil {
		return err
	} else if live {
		return fmt.Errorf("recorded listen address %q is still live (%s); stop that daemon before recovery",
			manifest.Resources.ListenAddress, description)
	}
	present, err := host.PresentContainers(ctx, *containerBin, manifest.Resources.Containers)
	if err != nil {
		return err
	}
	if !*confirm {
		presentVolumes, presentNetworks, err := host.PresentPersistentResources(
			ctx, *containerBin, manifest.Resources.Volumes, manifest.Resources.Networks,
		)
		if err != nil {
			return err
		}
		if err := json.NewEncoder(stdout).Encode(struct {
			Manifest          daemonlock.RigManifest `json:"manifest"`
			PresentContainers []string               `json:"present_containers"`
			PresentVolumes    []string               `json:"present_volumes"`
			PresentNetworks   []string               `json:"present_networks"`
		}{manifest, present, presentVolumes, presentNetworks}); err != nil {
			return err
		}
		return daemonlock.ErrRigRecoveryConfirmation
	}
	if err := cleanupRigContainers(ctx, host, *containerBin, present); err != nil {
		return err
	}
	if err := requireRigResourcesAbsent(ctx, host, *containerBin, manifest.Resources); err != nil {
		return err
	}
	if err := lease.Close(); err != nil {
		return err
	}
	completed = true
	return nil
}
