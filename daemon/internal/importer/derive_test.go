package importer

import "testing"

// TestUnderAnyOpaque pins the round-9 P2 rewrite: suppression keys on a
// base path's own ancestors (O(depth)), and a prefix collision (opaque
// "a" vs base "ab/x") does not falsely suppress.
func TestUnderAnyOpaque(t *testing.T) {
	opaque := map[string]struct{}{"sub": {}, "a/b": {}}
	cases := []struct {
		path string
		want bool
	}{
		{"sub/inner.txt", true}, // directly under opaque "sub"
		{"sub/deep/x", true},    // deeper under opaque "sub"
		{"a/b/c", true},         // under opaque "a/b"
		{"sub", false},          // the opaque path itself is not "under" it
		{"submarine/x", false},  // prefix collision: not under "sub/"
		{"a/bc/x", false},       // prefix collision: not under "a/b/"
		{"other/x", false},      // unrelated
		{"a/b", false},          // the opaque path itself
	}
	for _, tc := range cases {
		if got := underAnyOpaque(opaque, tc.path); got != tc.want {
			t.Errorf("underAnyOpaque(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
	if underAnyOpaque(map[string]struct{}{}, "a/b/c") {
		t.Error("empty opaque set must suppress nothing")
	}
}
