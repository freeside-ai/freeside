package macverify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/procbound"
)

// ErrToolchainMismatch refuses a job when the host's Xcode or Swift differs
// from the configuration's pin, so a toolchain change invalidates the
// conformance pass instead of running under it.
var ErrToolchainMismatch = errors.New("macverify: host toolchain differs from the configured pin")

// Toolchain identifies an operator-installed Xcode and Swift by the tokens
// app CI already keys its Swift build cache on (app/README.md).
type Toolchain struct {
	// XcodeBuild is the build number `xcodebuild -version` prints, such as
	// 17F42.
	XcodeBuild string
	// Swift is the swiftlang- token `swift --version` prints, such as
	// swiftlang-6.1.2.1.2.
	Swift string
}

var (
	xcodeBuildPattern = regexp.MustCompile(`^[0-9A-Za-z]+$`)
	swiftPattern      = regexp.MustCompile(`^swiftlang-[0-9A-Za-z.]+$`)
	xcodeBuildLine    = regexp.MustCompile(`(?m)^Build version ([0-9A-Za-z]+)$`)
	swiftToken        = regexp.MustCompile(`\bswiftlang-[0-9A-Za-z.]+`)
)

func (t Toolchain) validate() error {
	if !xcodeBuildPattern.MatchString(t.XcodeBuild) {
		return fmt.Errorf("toolchain Xcode build %q is not an xcodebuild -version build number", t.XcodeBuild)
	}
	if !swiftPattern.MatchString(t.Swift) {
		return fmt.Errorf("toolchain Swift %q is not a swiftlang- token", t.Swift)
	}
	return nil
}

func (t Toolchain) String() string { return "Xcode " + t.XcodeBuild + ", " + t.Swift }

// HostToolchain reads the Xcode and Swift a recipe command on this host
// would run. It runs both tools with the command PATH and without the
// daemon's environment, so a DEVELOPER_DIR the daemon inherited cannot make
// it read a toolchain the slot would not use; the slot and the daemon share
// the host's one xcode-select choice.
func HostToolchain(ctx context.Context) (Toolchain, error) {
	xcode, err := runToolchainProbe(ctx, "xcodebuild", "-version")
	if err != nil {
		return Toolchain{}, err
	}
	swift, err := runToolchainProbe(ctx, "swift", "--version")
	if err != nil {
		return Toolchain{}, err
	}
	build, err := parseXcodeBuild(xcode)
	if err != nil {
		return Toolchain{}, err
	}
	token, err := parseSwiftToken(swift)
	if err != nil {
		return Toolchain{}, err
	}
	return Toolchain{XcodeBuild: build, Swift: token}, nil
}

func runToolchainProbe(ctx context.Context, tool string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "/usr/bin/xcrun", append([]string{tool}, args...)...) //nolint:gosec // G204: fixed xcrun and tool names
	cmd.Env = toolEnv()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := procbound.Run(cmd, hostToolWaitDelay); err != nil {
		return "", fmt.Errorf("read host toolchain: xcrun %s %s: %w: %s",
			tool, strings.Join(args, " "), err, strings.TrimSpace(out.String()))
	}
	return out.String(), nil
}

// hostToolWaitDelay bounds the wait for a host tool's output pipes after it
// exits, since xcrun and xcodebuild may leave a helper holding them.
const hostToolWaitDelay = 10 * time.Second

// toolEnv is the environment for the host tools macverify runs as the
// daemon user: the command PATH and the user's own home and temporary
// directory, nothing else inherited.
func toolEnv() []string {
	env := []string{"PATH=" + commandPath, "LC_ALL=C"}
	for _, key := range []string{"HOME", "TMPDIR"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func parseXcodeBuild(out string) (string, error) {
	m := xcodeBuildLine.FindAllStringSubmatch(out, -1)
	if len(m) != 1 {
		return "", fmt.Errorf("read host toolchain: xcodebuild -version printed %d build versions", len(m))
	}
	return m[0][1], nil
}

func parseSwiftToken(out string) (string, error) {
	m := swiftToken.FindAllString(out, -1)
	if len(m) != 1 {
		return "", fmt.Errorf("read host toolchain: swift --version printed %d swiftlang- tokens", len(m))
	}
	return m[0], nil
}
