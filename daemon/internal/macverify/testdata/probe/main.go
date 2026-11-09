//go:build darwin

// Command probe is the macverify live suite's workload: the suite runs it
// through the room as a real slot and reads its report. Each check prints
// "ok <name>" or "FAIL <name>: <detail>", and the probe exits 1 if any
// failed.
//
//	probe confine <loopback-port> <operator-home>
//	probe home-clean
//	probe kill-self
//	probe linger
package main

/*
#cgo LDFLAGS: -framework CoreGraphics -framework CoreFoundation
#include <CoreGraphics/CoreGraphics.h>

static int has_window_server_session(void) {
	CFDictionaryRef session = CGSessionCopyCurrentDictionary();
	if (session == NULL) {
		return 0;
	}
	CFRelease(session);
	return 1;
}
*/
import "C"

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

const homeMarker = "freeside-probe-marker"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: probe confine|home-clean|kill-self|linger")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "confine":
		if len(os.Args) != 4 {
			fmt.Fprintln(os.Stderr, "usage: probe confine <loopback-port> <operator-home>")
			os.Exit(2)
		}
		os.Exit(confine(os.Args[2], os.Args[3]))
	case "home-clean":
		os.Exit(homeClean())
	case "kill-self":
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		time.Sleep(time.Minute)
	case "linger":
		// A descendant that leaves the process group and the session, which
		// a process-group reap would miss.
		child := exec.Command("/bin/sleep", "600")
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := child.Start(); err != nil {
			fmt.Println("FAIL linger:", err)
			os.Exit(1)
		}
		fmt.Println("ok linger: started", child.Process.Pid)
		time.Sleep(10 * time.Minute)
	default:
		fmt.Fprintln(os.Stderr, "unknown mode", os.Args[1])
		os.Exit(2)
	}
}

type report struct{ failed bool }

func (r *report) check(name string, err error) {
	if err != nil {
		r.failed = true
		fmt.Printf("FAIL %s: %v\n", name, err)
		return
	}
	fmt.Printf("ok %s\n", name)
}

func confine(port, operatorHome string) int {
	var r report
	r.check("loopback-refused", expectFailure(func() error {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", port), 3*time.Second)
		if err == nil {
			_ = conn.Close()
		}
		return err
	}))
	r.check("resolver-empty", expectFailure(func() error {
		_, err := net.LookupHost("github.com")
		return err
	}))
	marker := fmt.Sprintf("freeside-probe-%d", os.Getpid())
	for _, dir := range []string{"/private/tmp", "/Users/Shared", operatorHome} {
		r.check("write-denied "+dir, expectFailure(func() error { return writeFile(filepath.Join(dir, marker)) }))
	}
	wd, err := os.Getwd()
	r.check("write-job-dir", errors(err, writeFile(filepath.Join(wd, marker))))
	r.check("write-home", writeFile(filepath.Join(os.Getenv("HOME"), homeMarker)))
	r.check("write-tmpdir", writeFile(filepath.Join(os.Getenv("TMPDIR"), marker)))
	r.check("job-dir-owner", jobDirOwned(wd))
	r.check("environment", environmentAllowlist())
	r.check("no-window-server-session", func() error {
		if C.has_window_server_session() != 0 {
			return fmt.Errorf("CGSessionCopyCurrentDictionary returned a session")
		}
		return nil
	}())
	if r.failed {
		return 1
	}
	return 0
}

func homeClean() int {
	var r report
	_, err := os.Lstat(filepath.Join(os.Getenv("HOME"), homeMarker))
	if err == nil {
		err = fmt.Errorf("%s survived from an earlier job", homeMarker)
	} else if os.IsNotExist(err) {
		err = nil
	}
	r.check("home-reset", err)
	if r.failed {
		return 1
	}
	return 0
}

func expectFailure(attempt func() error) error {
	if err := attempt(); err != nil {
		fmt.Printf("  (refused as expected: %v)\n", err)
		return nil
	}
	return fmt.Errorf("succeeded")
}

func writeFile(path string) error {
	return os.WriteFile(path, []byte("probe\n"), 0o600)
}

func errors(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// jobDirOwned checks the job directory, the workspace's parent, is this
// uid's own at 0700.
func jobDirOwned(workspace string) error {
	info, err := os.Lstat(filepath.Dir(workspace))
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("no stat for %s", filepath.Dir(workspace))
	}
	if int(st.Uid) != os.Getuid() || info.Mode().Perm() != 0o700 || !info.IsDir() {
		return fmt.Errorf("job dir owner %d mode %v, want %d 0700", st.Uid, info.Mode(), os.Getuid())
	}
	return nil
}

func environmentAllowlist() error {
	keys := make([]string, 0, 4)
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		keys = append(keys, key)
	}
	slices.Sort(keys)
	if want := []string{"HOME", "LC_ALL", "PATH", "TMPDIR"}; !slices.Equal(keys, want) {
		return fmt.Errorf("environment keys %v, want %v", keys, want)
	}
	return nil
}
