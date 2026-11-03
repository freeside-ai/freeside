package main

import "golang.org/x/sys/unix"

// physicalMemoryBytes is the host's installed memory.
func physicalMemoryBytes() (uint64, error) {
	return unix.SysctlUint64("hw.memsize")
}
