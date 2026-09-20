// Command freeside-image-build runs `container build` with RUN-step egress
// routed through an ephemeral host-side build proxy, so an image build does
// not depend on vmnet guest NAT (which a host VPN can break; see
// internal/buildproxy). The proxy starts before the build and stops when it
// exits. Everything after `--` is passed to `container build` unchanged:
//
//	freeside-image-build -- --tag name:local ./context
//
// The scripts/build-*-image.sh builders call this when the operator has not
// supplied their own HTTPS_PROXY. All logic lives in internal/buildproxy and
// ward's runtime inspection; this command only binds them to a child process.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/freeside-ai/freeside/daemon/internal/buildproxy"
	"github.com/freeside-ai/freeside/daemon/internal/procbound"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	code, err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "freeside-image-build:", err)
	}
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) (code int, err error) {
	flags := flag.NewFlagSet("freeside-image-build", flag.ContinueOnError)
	flags.SetOutput(stderr)
	containerBin := flags.String("container", "container", "Apple container executable")
	if err := flags.Parse(args); err != nil {
		// The flag set already reported the error and usage on stderr.
		return 2, nil
	}
	if flags.NArg() == 0 {
		return 2, errors.New("usage: freeside-image-build [-container PATH] -- BUILD_ARGUMENTS")
	}
	containerPath, err := exec.LookPath(*containerBin)
	if err != nil {
		return 1, fmt.Errorf("resolve container executable %q: %w", *containerBin, err)
	}
	network, err := ward.NewCLIRuntime(containerPath).InspectNetwork(ctx, buildproxy.Network)
	if err != nil {
		return 1, fmt.Errorf("inspect build network: %w", err)
	}
	proxy, err := buildproxy.Start(buildproxy.BuildNetwork{
		Name: network.Name, Mode: string(network.Mode),
		IPv4Gateway: network.IPv4Gateway, IPv4Subnet: network.IPv4Subnet,
	})
	if err != nil {
		return 1, err
	}
	defer func() {
		if closeErr := proxy.Close(); closeErr != nil && err == nil {
			code, err = 1, fmt.Errorf("build proxy: %w", closeErr)
		}
	}()

	buildArgs := append([]string{
		"build",
		"--build-arg", "HTTP_PROXY=" + proxy.URL(),
		"--build-arg", "HTTPS_PROXY=" + proxy.URL(),
	}, flags.Args()...)
	cmd := exec.CommandContext(ctx, containerPath, buildArgs...) //nolint:gosec // operator-resolved executable; arguments are the caller's own build arguments
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := procbound.Run(cmd, procbound.DefaultWaitDelay); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode(), nil
		}
		return 1, fmt.Errorf("run container build: %w", err)
	}
	return 0, nil
}
