package macverify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

func TestRunProbeRefusesBadUsage(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		nil,
		{"bogus"},
		{probeModeSandbox, "1", "github.com", "192.0.2.1:443", "192.0.2.1:53"},
		{probeModePacketFilter, "192.0.2.1:443"},
		{probeModeHomeMark, "extra"},
		{probeModeHomeClean, "extra"},
	} {
		var out bytes.Buffer
		if code := RunProbe(args, &out); code != 2 || !strings.HasPrefix(out.String(), "usage:") {
			t.Errorf("RunProbe(%q) = %d, %q; want usage and 2", args, code, out.String())
		}
	}
}

// TestRunProbeUnconfinedFailsEverySandboxCheck runs the sandbox mode with no
// sandbox: every network attempt and every outside write must fail its
// check, and the parser must refuse the report. The writes it made succeed,
// so the probe removes them.
// freePorts returns n distinct ports nothing held on 127.0.0.1 a moment ago.
func freePorts(t *testing.T, n int) []int {
	t.Helper()
	ports := make([]int, 0, n)
	for len(ports) < n {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		_ = ln.Close()
		if !slices.Contains(ports, port) {
			ports = append(ports, port)
		}
	}
	return ports
}

func TestRunProbeUnconfinedFailsEverySandboxCheck(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	// A closed port refuses at once, which is not the sandbox's EPERM.
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closed.Addr().String()
	_ = closed.Close()
	answer := udpAnswerer(t)
	outside := t.TempDir()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	free := freePorts(t, 5)
	spec := sandboxProbe(sandboxTargets{
		loopbackPort: port, resolverHost: "localhost", tcp: closedAddr, udp: answer,
		ports: slotPortSet(free[:4]), foreignPort: free[4], outsideDirs: []string{outside},
	})

	ipv6Loopback := true
	if ln, err := net.Listen("tcp6", "[::1]:0"); err != nil {
		ipv6Loopback = false
	} else {
		_ = ln.Close()
	}

	var out bytes.Buffer
	code := RunProbe(append([]string{spec.mode}, spec.args...), &out)
	if code != 1 {
		t.Fatalf("RunProbe = %d, want 1\n%s", code, out.String())
	}
	for _, c := range spec.checks {
		want := "FAIL " + c.name + ": "
		// Unconfined, the job directory and the reserved ports still work.
		if c.name == checkWriteJobDir || strings.HasPrefix(c.name, "reserved-port ") {
			want = "ok " + c.name + "\n"
		}
		// A host without IPv6 loopback, such as a container, cannot pass
		// a reserved-port check, which needs ::1 too.
		if strings.HasPrefix(c.name, "reserved-port ") && !ipv6Loopback {
			continue
		}
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("outside directory holds %v (%v), want the probe's marker removed", entries, err)
	}
	err = checkProbeReport("slot1", spec, verify.StepResult{Output: out.Bytes(), ExitCode: code})
	if !errors.Is(err, ErrConformance) {
		t.Fatalf("checkProbeReport = %v, want ErrConformance", err)
	}
}

// udpAnswerer is a DNS stand-in that echoes every datagram, which carries the
// query's id back.
func udpAnswerer(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = conn.WriteTo(buf[:n], addr)
		}
	}()
	return conn.LocalAddr().String()
}

// TestRunProbeHomeModes sets HOME, which the home modes read, so it runs
// serially.
func TestRunProbeHomeModes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	run := func(mode string) (int, string) {
		var out bytes.Buffer
		code := RunProbe([]string{mode}, &out)
		return code, out.String()
	}
	if code, out := run(probeModeHomeClean); code != 0 || out != "ok home-reset\n" {
		t.Fatalf("home-clean on an empty home = %d, %q", code, out)
	}
	if code, out := run(probeModeHomeMark); code != 0 || out != "ok home-marked\n" {
		t.Fatalf("home-mark = %d, %q", code, out)
	}
	if code, out := run(probeModeHomeClean); code != 1 || !strings.HasPrefix(out, "FAIL home-reset: ") {
		t.Fatalf("home-clean over a surviving marker = %d, %q", code, out)
	}
	if err := os.Remove(filepath.Join(home, probeHomeMarker)); err != nil {
		t.Fatal(err)
	}
}

func TestFilterRefusedAcceptsOnlyADrop(t *testing.T) {
	t.Parallel()
	timeout := &net.OpError{Op: "dial", Err: timeoutError{}}
	for name, tc := range map[string]struct {
		err  error
		pass bool
	}{
		"succeeded":          {err: nil},
		"sandbox EPERM":      {err: fmt.Errorf("connect: %w", syscall.EPERM)},
		"connection refused": {err: fmt.Errorf("connect: %w", syscall.ECONNREFUSED)},
		"network down":       {err: fmt.Errorf("connect: %w", syscall.ENETUNREACH)},
		"timeout":            {err: timeout, pass: true},
		"host unreachable":   {err: fmt.Errorf("connect: %w", syscall.EHOSTUNREACH), pass: true},
		"permission denied":  {err: fmt.Errorf("sendto: %w", syscall.EACCES), pass: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := &probe{out: &bytes.Buffer{}}
			if err := p.filterRefused(tc.err); (err == nil) != tc.pass {
				t.Fatalf("filterRefused(%v) = %v, want pass %v", tc.err, err, tc.pass)
			}
		})
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// TestDialTimeoutReadsAsTimeout pins that a real dial timeout is what
// isTimeout recognizes, since the packet filter's drop surfaces as one.
func TestDialTimeoutReadsAsTimeout(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 0)
	defer cancel()
	var d net.Dialer
	_, err := d.DialContext(ctx, "tcp", "192.0.2.1:443")
	if !isTimeout(err) {
		t.Fatalf("a dial past its deadline = %v, not a timeout", err)
	}
}
