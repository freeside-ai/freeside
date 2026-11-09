//go:build darwin

package macverify

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// stateZombie is SZOMB from darwin's <sys/proc.h>; golang.org/x/sys defines
// no constant for it (internal/procbound uses the same value).
const stateZombie = 5

// uidOwnsProcess reports whether any live process has uid as its effective
// or real uid. A zombie runs nothing and is left out. Listing processes
// needs no privilege, so this is the daemon's own check, independent of the
// helper that did the killing. Each slot runs one job at a time, so a slot
// uid with no live process proves nothing of the job survives, including a
// descendant that left its process group or session.
func uidOwnsProcess(uid int) (bool, error) {
	for _, name := range []string{"kern.proc.uid", "kern.proc.ruid"} {
		procs, err := unix.SysctlKinfoProcSlice(name, uid)
		if err != nil {
			return false, fmt.Errorf("list processes of uid %d (%s): %w", uid, name, err)
		}
		for _, p := range procs {
			if p.Proc.P_stat != stateZombie {
				return true, nil
			}
		}
	}
	return false, nil
}
