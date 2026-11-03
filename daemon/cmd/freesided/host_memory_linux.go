package main

import "golang.org/x/sys/unix"

// physicalMemoryBytes is the host's installed memory.
func physicalMemoryBytes() (uint64, error) {
	var info unix.Sysinfo_t
	if err := unix.Sysinfo(&info); err != nil {
		return 0, err
	}
	return uint64(info.Totalram) * uint64(info.Unit), nil //nolint:unconvert // Totalram is uint32 on 32-bit Linux
}
