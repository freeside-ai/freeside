//go:build darwin

package daemoncompose

import (
	"path/filepath"
	"testing"
)

// TestLookupCordonAccountResolvesTheAccountsOwnDirectories reads, and
// writes nothing to, the account's real directories.
func TestLookupCordonAccountResolvesTheAccountsOwnDirectories(t *testing.T) {
	t.Parallel()
	account, err := LookupCordonAccount()
	if err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{"Home": account.Home, "UserTemp": account.UserTemp, "UserCache": account.UserCache} {
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil || resolved != dir {
			t.Errorf("%s = %q, want a resolved directory (%q, %v)", name, dir, resolved, err)
		}
	}
	if filepath.Base(account.UserTemp) != "T" || filepath.Base(account.UserCache) != "C" {
		t.Errorf("UserTemp %q and UserCache %q are not confstr's per-user directories", account.UserTemp, account.UserCache)
	}
}
