package projectimage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixtures in testdata/npm are the output of `npm ci --ignore-scripts`
// from npm 11.16.0, the npm the image's pinned Node 24.18.0 ships. The
// buildkit-*.txt pair ran inside a real `container build`, so it keeps
// BuildKit's line prefixes; the rest ran npm directly against a local server
// or an unreachable address. eai-again.txt is enotfound.txt with the code
// replaced, because a temporary resolver failure can't be produced on demand.
func readNPMFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "npm", name)) //nolint:gosec // G304: test-named fixture under testdata.
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestInstallFailureMatchesOnlyCandidateCausedNPMFailures(t *testing.T) {
	t.Parallel()
	deprecated := readNPMFixture(t, "deprecated-message.txt")
	cases := []struct {
		name    string
		output  []byte
		want    bool
		wantHas []string
	}{
		{
			name: "integrity mismatch through BuildKit", output: readNPMFixture(t, "buildkit-eintegrity.txt"), want: true,
			wantHas: []string{"npm error code EINTEGRITY\nnpm error sha512-", "integrity checksum failed"},
		},
		{
			name: "lockfile out of sync through BuildKit", output: readNPMFixture(t, "buildkit-out-of-sync.txt"), want: true,
			wantHas: []string{
				"npm error code EUSAGE\nnpm error `npm ci` can only install packages",
				"\nnpm error Missing: is-odd@3.0.1 from lock file\nnpm error Missing: is-number@6.0.0 from lock file",
			},
		},
		{name: "connection refused", output: readNPMFixture(t, "econnrefused.txt")},
		{name: "connection reset", output: readNPMFixture(t, "econnreset.txt")},
		{name: "name not found", output: readNPMFixture(t, "enotfound.txt")},
		{name: "temporary name resolution failure", output: readNPMFixture(t, "eai-again.txt")},
		{name: "connect timeout", output: readNPMFixture(t, "etimedout.txt")},
		{name: "fetch timeout", output: readNPMFixture(t, "fetch-timeout.txt")},
		{name: "registry 5xx", output: readNPMFixture(t, "e503.txt")},
		{name: "registry 404", output: readNPMFixture(t, "e404.txt")},
		{name: "no matching version", output: readNPMFixture(t, "etarget.txt")},
		{
			// A lockfile's deprecation message is the candidate's text; npm
			// prefixes each of its lines, so it can't pass for npm's own code.
			name:   "candidate deprecation text before a network failure",
			output: append(append([]byte{}, deprecated...), readNPMFixture(t, "econnreset.txt")...),
		},
		{
			name: "usage error without the out-of-sync message",
			output: []byte("#7 1.322 npm error code EUSAGE\n#7 1.335 npm error\n" +
				"#7 1.335 npm error Unknown command\n"),
		},
		{name: "empty output", output: nil},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, ok := installFailure(test.output)
			if ok != test.want {
				t.Fatalf("installFailure matched = %v, want %v (lines %q)", ok, test.want, got)
			}
			if len(got) > maxInstallFailureBytes {
				t.Fatalf("installFailure returned %d bytes, want at most %d", len(got), maxInstallFailureBytes)
			}
			for _, want := range test.wantHas {
				if !strings.Contains(got, want) {
					t.Fatalf("installFailure lines = %q, want them to contain %q", got, want)
				}
			}
			if strings.Contains(got, "#7") || strings.Contains(got, "Clean install a project") {
				t.Fatalf("installFailure lines = %q, want npm's lines without BuildKit prefixes or npm ci's help", got)
			}
		})
	}
}

func TestInstallFailureBoundsLongEntryLists(t *testing.T) {
	t.Parallel()
	output := "npm error code EUSAGE\nnpm error\n" + npmOutOfSync + " Please update your lock file.\n"
	for range 50 {
		output += "npm error Missing: some-long-package-name@1.2.3 from lock file\n"
	}
	got, ok := installFailure([]byte(output))
	if !ok {
		t.Fatal("installFailure did not match an out-of-sync lockfile with many entries")
	}
	if len(got) > maxInstallFailureBytes || !strings.HasSuffix(got, "from lock file") {
		t.Fatalf("installFailure returned %d bytes ending %q, want whole lines within %d bytes",
			len(got), got[max(0, len(got)-20):], maxInstallFailureBytes)
	}
}
