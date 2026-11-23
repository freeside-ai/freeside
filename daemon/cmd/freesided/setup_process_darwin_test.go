package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"

	"golang.org/x/sys/unix"
)

// processCommandLine returns pid's arguments from the kernel's
// kern.procargs2 record. It reads the record directly rather than through
// ps, because ps is setuid and the local test sandbox
// (scripts/daemon-test.sb) refuses to exec a setuid binary.
func processCommandLine(pid int) (string, error) {
	// The record is the argument count, the executable path, NUL padding,
	// then each argument NUL-terminated.
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return "", err
	}
	if len(raw) < 4 {
		return "", errors.New("kern.procargs2 record is shorter than its argument count")
	}
	argc := int(binary.LittleEndian.Uint32(raw[:4]))
	rest := raw[4:]
	end := bytes.IndexByte(rest, 0)
	if end < 0 {
		return "", errors.New("kern.procargs2 record has no executable path")
	}
	rest = bytes.TrimLeft(rest[end:], "\x00")
	args := make([]string, 0, argc)
	for range argc {
		end := bytes.IndexByte(rest, 0)
		if end < 0 {
			return "", errors.New("kern.procargs2 record ends inside its arguments")
		}
		args = append(args, string(rest[:end]))
		rest = rest[end+1:]
	}
	return strings.Join(args, " "), nil
}
