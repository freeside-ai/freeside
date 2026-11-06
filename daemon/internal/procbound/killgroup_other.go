//go:build !darwin

package procbound

// groupOnlyZombies never reports a group done off darwin. Linux keeps a
// zombie in its group and the group kill succeeds, so EPERM there means a
// member the daemon cannot signal and stays an error.
func groupOnlyZombies(int) (bool, error) {
	return false, nil
}
