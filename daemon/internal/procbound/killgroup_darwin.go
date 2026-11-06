//go:build darwin

package procbound

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// stateZombie is SZOMB from darwin's <sys/proc.h>, the p_stat of a process
// that has exited and awaits its reap. golang.org/x/sys defines no
// constant for it.
const stateZombie = 5

// groupOnlyZombies reports whether process group pgid has no live member:
// every member the kern.proc.pgrp sysctl lists is a zombie, or it lists
// none because the group was reaped after the kill.
func groupOnlyZombies(pgid int) (bool, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
	if err != nil {
		return false, fmt.Errorf("list process group %d: %w", pgid, err)
	}
	states := make([]int8, len(procs))
	for i, p := range procs {
		states[i] = p.Proc.P_stat
	}
	return onlyZombies(states), nil
}

// onlyZombies reports whether every state is stateZombie. An empty list is
// true: a group with no members has nothing left alive.
func onlyZombies(states []int8) bool {
	for _, s := range states {
		if s != stateZombie {
			return false
		}
	}
	return true
}
