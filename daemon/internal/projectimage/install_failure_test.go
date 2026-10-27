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

// The fixtures in testdata/go are the output of `go mod download` from Go
// 1.26.10, the Go a Go image pins, run with the warm step's environment
// against proxy.golang.org: a go.sum with one zip or go.mod hash altered, a
// version the proxy does not serve, and the build proxy refusing or failing
// to resolve. proxy-410.txt is proxy-404.txt with the status replaced, since
// the proxy's 410 can't be produced on demand.
func readGoFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "go", name)) //nolint:gosec // G304: test-named fixture under testdata.
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// buildKitPrefixed gives every line BuildKit's plain-progress prefix.
func buildKitPrefixed(output []byte) []byte {
	var prefixed strings.Builder
	for line := range strings.Lines(string(output)) {
		prefixed.WriteString("#12 3.141 " + line)
	}
	return []byte(prefixed.String())
}

func TestInstallFailureMatchesOnlyCandidateCausedGoFailures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		output  []byte
		want    bool
		wantHas []string
	}{
		{
			name: "module checksum mismatch", output: readGoFixture(t, "checksum-mismatch.txt"), want: true,
			wantHas: []string{
				"verifying github.com/google/uuid@v1.6.0: checksum mismatch\n\tdownloaded: h1:",
				"\n\tgo.sum:     h1:AAAA",
			},
		},
		{
			name: "go.mod checksum mismatch through BuildKit", want: true,
			output:  buildKitPrefixed(readGoFixture(t, "gomod-checksum-mismatch.txt")),
			wantHas: []string{"verifying github.com/google/uuid@v1.6.0/go.mod: checksum mismatch"},
		},
		{
			name: "version the proxy does not serve", output: readGoFixture(t, "proxy-404.txt"), want: true,
			wantHas: []string{
				"reading https://proxy.golang.org/github.com/google/uuid/@v/v1.99.0.mod: 404 Not Found",
				"\n\tserver response: not found:",
			},
		},
		{
			name: "version the proxy has dropped through BuildKit", want: true,
			output:  buildKitPrefixed(readGoFixture(t, "proxy-410.txt")),
			wantHas: []string{": 410 Gone"},
		},
		{
			name: "dependency's requirement the proxy does not serve", want: true,
			output: buildKitPrefixed(readGoFixture(t, "proxy-404-requires.txt")),
			wantHas: []string{
				"go: rsc.io/quote@v1.5.2 requires\n\trsc.io/sampler@v1.3.0 (replaced by rsc.io/sampler@v1.99.0): reading",
				"\n\tserver response: not found:",
			},
		},
		{
			name: "replacement the proxy does not serve", output: readGoFixture(t, "proxy-404-replaced.txt"), want: true,
			wantHas: []string{"go: rsc.io/quote@v1.5.2 (replaced by rsc.io/quote@v1.5.99): reading"},
		},
		{
			name: "go.mod version the proxy does not resolve", output: readGoFixture(t, "invalid-version.txt"), want: true,
			wantHas: []string{"go: errors parsing go.mod:\ngo.mod:5: require rsc.io/quote: version \"v1.5.2-01\" invalid"},
		},
		{
			name: "go.mod Go cannot parse", output: buildKitPrefixed(readGoFixture(t, "godebug-usage.txt")), want: true,
			wantHas: []string{"go: errors parsing go.mod:\ngo.mod:7: usage: godebug key=value"},
		},
		{
			name: "go.mod Go cannot load", output: readGoFixture(t, "godebug-unknown.txt"), want: true,
			wantHas: []string{"go: error loading go.mod:\ngo.mod:7: unknown godebug \"nosuchkey\""},
		},
		{
			// Go resolves a non-canonical version through the proxy while
			// it reads go.mod, so a fetch can fail inside its refusal.
			name:   "go.mod query the build proxy could not answer",
			output: readGoFixture(t, "retract-query-no-such-host.txt"),
		},
		{
			name: "indented 404 heading no requires chain",
			output: []byte("go: something else\n\texample.test/a@v1.0.0: reading " +
				"https://proxy.golang.org/example.test/a/@v/v1.0.0.mod: 404 Not Found\n"),
		},
		{name: "build proxy refused", output: readGoFixture(t, "proxyconnect-refused.txt")},
		{name: "build proxy not resolved", output: readGoFixture(t, "proxy-no-such-host.txt")},
		{
			// A disagreement with the checksum database names no go.sum
			// line, so nothing the candidate wrote caused it. The text is
			// the format cmd/go/internal/modfetch writes for it.
			name: "checksum database mismatch",
			output: []byte("go: github.com/google/uuid@v1.6.0: verifying module: checksum mismatch\n" +
				"\tdownloaded: h1:NIvaJDMOsjHA8n1jAhLSgzrAzy1Hgr+hNrb57e+94F0=\n" +
				"\tsum.golang.org: h1:AAAAJDMOsjHA8n1jAhLSgzrAzy1Hgr+hNrb57e+94F0=\n"),
		},
		{
			name: "404 from another host",
			output: []byte("go: example.test/a@v1.0.0: reading " +
				"https://proxy.golang.org.example.test/example.test/a/@v/v1.0.0.mod: 404 Not Found\n"),
		},
		{
			name:   "server error from the proxy",
			output: []byte("go: example.test/a@v1.0.0: reading https://proxy.golang.org/example.test/a/@v/v1.0.0.mod: 502 Bad Gateway\n"),
		},
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
			if strings.Contains(got, "#12") || strings.Contains(got, "SECURITY ERROR") {
				t.Fatalf("installFailure lines = %q, want Go's lines without BuildKit prefixes or the advice", got)
			}
		})
	}
}
