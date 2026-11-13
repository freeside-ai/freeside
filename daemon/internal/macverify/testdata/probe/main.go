//go:build darwin

// Command probe is the macverify live suite's workload: the suite runs it
// through the room as a real slot and reads its report. Each check prints
// "ok <name>" or "FAIL <name>: <detail>", and the probe exits 1 if any
// failed. The network, write, and home-reset confinements are the daemon's
// own probe's (freesided macverify probe), which the room runs before every
// command; confine checks only what that probe does not.
//
//	probe confine
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
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: probe confine|kill-self|linger")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "confine":
		os.Exit(confine())
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

func confine() int {
	var r report
	marker := fmt.Sprintf("freeside-probe-%d", os.Getpid())
	r.check("write-home", writeFile(filepath.Join(os.Getenv("HOME"), marker)))
	r.check("write-tmpdir", writeFile(filepath.Join(os.Getenv("TMPDIR"), marker)))
	wd, err := os.Getwd()
	if err == nil {
		err = jobDirOwned(wd)
	}
	r.check("job-dir-owner", err)
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

func writeFile(path string) error {
	return os.WriteFile(path, []byte("probe\n"), 0o600)
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
	keys := make([]string, 0, 5)
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		keys = append(keys, key)
	}
	slices.Sort(keys)
	if want := []string{"FREESIDE_VERIFY_PORTS", "HOME", "LC_ALL", "PATH", "TMPDIR"}; !slices.Equal(keys, want) {
		return fmt.Errorf("environment keys %v, want %v", keys, want)
	}
	return nil
}
