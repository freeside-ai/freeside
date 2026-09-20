package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeContainer writes a stand-in container CLI that answers the build
// network inspection and, for `build`, records its arguments, reports whether
// the managed proxy is accepting connections, and exits with buildExit.
func fakeContainer(t *testing.T, network string, buildExit int) (path, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "build-args")
	path = filepath.Join(dir, "container")
	script := `#!/bin/sh
case "$1" in
network)
	cat <<'JSON'
` + network + `
JSON
	;;
build)
	printf '%s\n' "$@" >"` + argsFile + `"
	echo "build stdout"
	echo "build stderr" >&2
	exit ` + strconv.Itoa(buildExit) + `
	;;
*) exit 64 ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // test executable
		t.Fatal(err)
	}
	return path, argsFile
}

const defaultNetwork = `[{"id":"default","configuration":{"name":"default","mode":"nat","labels":{}},
"status":{"ipv4Gateway":"192.168.64.1","ipv4Subnet":"192.168.64.0/24"}}]`

func TestRunPutsTheManagedProxyAheadOfTheCallersBuildArguments(t *testing.T) {
	container, argsFile := fakeContainer(t, defaultNetwork, 0)
	var stdout, stderr bytes.Buffer
	code, err := run(t.Context(),
		[]string{"-container", container, "--", "--tag", "image:local", "/tmp/context"}, &stdout, &stderr)
	if code != 0 || err != nil {
		t.Fatalf("run = %d, %v; stderr %q", code, err, stderr.String())
	}
	recorded, err := os.ReadFile(argsFile) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(recorded)), "\n")
	const prefix = "HTTP_PROXY=http://192.168.64.1:"
	if len(args) != 8 || args[0] != "build" || args[1] != "--build-arg" || !strings.HasPrefix(args[2], prefix) ||
		args[3] != "--build-arg" || args[4] != "HTTPS_PROXY="+strings.TrimPrefix(args[2], "HTTP_PROXY=") ||
		strings.Join(args[5:], " ") != "--tag image:local /tmp/context" {
		t.Fatalf("container build arguments = %q", args)
	}
	if stdout.String() != "build stdout\n" || stderr.String() != "build stderr\n" {
		t.Fatalf("build output was not passed through: stdout %q, stderr %q", stdout.String(), stderr.String())
	}
	port := strings.TrimPrefix(args[2], prefix)
	if conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", port)); err == nil { //nolint:noctx // test dial to a local listener
		_ = conn.Close()
		t.Fatalf("managed proxy on port %s outlived the build", port)
	}
}

func TestRunReportsTheBuildExitStatus(t *testing.T) {
	container, _ := fakeContainer(t, defaultNetwork, 7)
	code, err := run(t.Context(), []string{"-container", container, "--", "/tmp/context"}, &bytes.Buffer{}, &bytes.Buffer{})
	if code != 7 || err != nil {
		t.Fatalf("run = %d, %v; want the build's own status 7", code, err)
	}
}

func TestRunRefusesToBuildWithoutATrustworthyNetworkOrArguments(t *testing.T) {
	everything := strings.Replace(defaultNetwork, "192.168.64.0/24", "0.0.0.0/0", 1)
	for name, tc := range map[string]struct {
		network string
		args    []string
		code    int
	}{
		"implausible subnet": {everything, []string{"--", "/tmp/context"}, 1},
		"no build arguments": {defaultNetwork, nil, 2},
		"unknown flag":       {defaultNetwork, []string{"-nope"}, 2},
	} {
		t.Run(name, func(t *testing.T) {
			container, argsFile := fakeContainer(t, tc.network, 0)
			code, _ := run(t.Context(), append([]string{"-container", container}, tc.args...), &bytes.Buffer{}, &bytes.Buffer{})
			if code != tc.code {
				t.Fatalf("exit code = %d, want %d", code, tc.code)
			}
			if _, err := os.Stat(argsFile); err == nil {
				t.Fatal("container build ran")
			}
		})
	}
}
