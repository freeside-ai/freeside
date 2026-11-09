//go:build !darwin

package macverify

import "fmt"

// uidOwnsProcess cannot prove absence off macOS; the room refuses to run
// there before it would ask.
func uidOwnsProcess(uid int) (bool, error) {
	return false, fmt.Errorf("%w: cannot list processes of uid %d", ErrHostUnsupported, uid)
}
