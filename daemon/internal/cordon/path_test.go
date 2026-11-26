package cordon

import (
	"slices"
	"testing"
)

// TestLaunchPathIsTheCLIThenTheManagersThenTheSystem pins the list and its
// order, and shows a CLI directory that is also on the list appears once,
// first.
func TestLaunchPathIsTheCLIThenTheManagersThenTheSystem(t *testing.T) {
	t.Parallel()
	managers := []string{
		"/opt/homebrew/bin", "/opt/homebrew/opt/rustup/bin",
		"/Users/run/.cargo/bin", "/Users/run/.local/bin", "/Users/run/.local/share/mise/shims",
	}
	system := []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin"}
	cases := []struct {
		cli  string
		want []string
	}{
		{"/opt/claude/bin", slices.Concat([]string{"/opt/claude/bin"}, managers, system)},
		{"/usr/bin", slices.Concat([]string{"/usr/bin"}, managers, []string{"/bin", "/usr/sbin", "/sbin"})},
		{"/Users/run/.local/bin", slices.Concat([]string{"/Users/run/.local/bin"},
			slices.Delete(slices.Clone(managers), 3, 4), system)},
	}
	for _, tc := range cases {
		if got := LaunchPath(tc.cli, "/Users/run"); !slices.Equal(got, tc.want) {
			t.Errorf("LaunchPath(%q) = %q, want %q", tc.cli, got, tc.want)
		}
	}
}
