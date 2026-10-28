package projectimage

import (
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestParseGoSumAdmitsOnlyLinesOfGosOwnShape(t *testing.T) {
	t.Parallel()
	const (
		zip   = "github.com/google/uuid v1.6.0 h1:NIvaJDMOsjHA8n1jAhLSgzrAzy1Hgr+hNrb57e+94F0="
		goMod = "github.com/google/uuid v1.6.0/go.mod h1:TIyPZe4MgqvfeYDBFedMoGGpEw/LqOeaOT+nhxU+yHo="
		hash  = " h1:TIyPZe4MgqvfeYDBFedMoGGpEw/LqOeaOT+nhxU+yHo="
	)
	for _, test := range []struct {
		name  string
		goSum string
		// pins is the number of keys read; -1 means refused.
		pins int
	}{
		{name: "empty", goSum: "", pins: 0},
		{name: "go's own output", goSum: uuidSum, pins: 2},
		{name: "blank lines", goSum: "\n" + zip + "\n\n" + goMod + "\n", pins: 2},
		// Go splits each line into fields, so these read as Go reads them.
		{name: "trailing spaces", goSum: zip + "  \n" + goMod + "\t\n", pins: 2},
		{name: "CRLF line ends", goSum: zip + "\r\n" + goMod + "\r\n", pins: 2},
		{name: "repeated line", goSum: zip + "\n" + zip + "\n", pins: 1},
		// Go drops a line holding an empty go.mod's hash, an old bug's output.
		{name: "empty go.mod hash", goSum: zip + "\ngithub.com/google/uuid v1.6.0/go.mod " + goEmptyGoModHash, pins: 1},
		{name: "prerelease", goSum: "github.com/google/uuid v1.6.0-rc.1" + hash, pins: 1},
		{name: "pseudo-version", goSum: "gopkg.in/check.v1 v0.0.0-20161208181325-20d25e280405" + hash, pins: 1},
		{name: "incompatible major", goSum: "github.com/x/y v2.0.0+incompatible" + hash, pins: 1},
		{name: "another hash kind", goSum: "github.com/google/uuid v1.6.0 h2:TIyPZe4MgqvfeYDBFedMoGGpEw/LqOeaOT+nhxU+yHo=", pins: -1},
		{name: "short hash", goSum: "github.com/google/uuid v1.6.0 h1:y=", pins: -1},
		{name: "missing hash", goSum: "github.com/google/uuid v1.6.0", pins: -1},
		{name: "extra field", goSum: zip + " x", pins: -1},
		{name: "repeated key with another hash", goSum: zip + "\ngithub.com/google/uuid v1.6.0" + hash, pins: -1},
		{name: "non-canonical version", goSum: "github.com/google/uuid v1.6" + hash, pins: -1},
		{name: "build metadata", goSum: "github.com/google/uuid v1.6.0+meta" + hash, pins: -1},
		{name: "leading zero in a prerelease", goSum: "github.com/google/uuid v1.6.0-01" + hash, pins: -1},
		{name: "path without a dot", goSum: "uuid v1.6.0" + hash, pins: -1},
		{name: "uppercase host", goSum: "GitHub.com/google/uuid v1.6.0" + hash, pins: -1},
		{name: "local path", goSum: "./dep v1.6.0" + hash, pins: -1},
		{name: "other suffix", goSum: "github.com/google/uuid v1.6.0/go.zip" + hash, pins: -1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			sums, err := parseGoSum([]byte(test.goSum))
			if test.pins < 0 {
				if err == nil {
					t.Fatalf("parseGoSum(%q) = %v, want a refusal", test.goSum, sums)
				}
				return
			}
			if err != nil || len(sums) != test.pins {
				t.Fatalf("parseGoSum(%q) = %v, %v; want %d pins", test.goSum, sums, err, test.pins)
			}
		})
	}
}

func TestGoModuleRequirementsResolveReplacesAsGoDoes(t *testing.T) {
	t.Parallel()
	const module = "module example.test/m\n\ngo 1.26\n\n"
	for _, test := range []struct {
		name  string
		goMod string
		want  []string
		// refusal is a fragment of the error; empty means admitted.
		refusal string
	}{
		{name: "no requirements", goMod: module},
		{
			name:  "line and block forms with comments",
			goMod: module + "require example.test/a v1.0.0\n\nrequire (\n\texample.test/b v0.1.0 // indirect\n)\n",
			want:  []string{"example.test/a@v1.0.0", "example.test/b@v0.1.0"},
		},
		{
			name:  "quoted path",
			goMod: module + "require \"example.test/a\" v1.0.0\n",
			want:  []string{"example.test/a@v1.0.0"},
		},
		{
			name:  "replace of every version",
			goMod: module + "require example.test/a v1.0.0\nreplace example.test/a => example.test/fork v1.2.0\n",
			want:  []string{"example.test/fork@v1.2.0"},
		},
		{
			name: "replace of this version wins",
			goMod: module + "require example.test/a v1.0.0\n" +
				"replace example.test/a v1.0.0 => example.test/exact v1.0.1\n" +
				"replace example.test/a => example.test/any v1.2.0\n",
			want: []string{"example.test/exact@v1.0.1"},
		},
		{
			name:  "replace of another version",
			goMod: module + "require example.test/a v1.0.0\nreplace example.test/a v0.9.0 => example.test/fork v1.2.0\n",
			want:  []string{"example.test/a@v1.0.0"},
		},
		{
			name: "directives that choose no module",
			goMod: module + "toolchain go1.26.10\ngodebug default=go1.21\nexclude example.test/a v0.1.0\n" +
				"retract [v0.1.0, v0.2.0]\ntool example.test/a/cmd\nignore ./node_modules\n",
		},
		{
			name: "repeated identical replace",
			goMod: module + "require example.test/a v1.0.0\n" +
				"replace example.test/a => example.test/fork v1.2.0\nreplace example.test/a => example.test/fork v1.2.0\n",
			want: []string{"example.test/fork@v1.2.0"},
		},
		{name: "retract of one version", goMod: module + "retract v0.1.0 // published by mistake\n"},
		{name: "unknown directive", goMod: module + "vendor example.test/a\n", refusal: "unknown directive"},
		{
			name: "conflicting replaces of every version",
			goMod: module + "require example.test/a v1.0.0\n" +
				"replace example.test/a => example.test/fork v1.2.0\nreplace example.test/a => example.test/fork v1.3.0\n",
			refusal: "conflicting replacements",
		},
		{
			name: "conflicting replaces of one version",
			goMod: module + "replace example.test/a v1.0.0 => example.test/b v1.0.0\n" +
				"replace example.test/a v1.0.0 => example.test/c v1.0.0\n",
			refusal: "conflicting replacements",
		},
		{name: "exclude without a version", goMod: module + "exclude example.test/a\n", refusal: "malformed exclude"},
		{
			name: "exclude of a non-canonical version", goMod: module + "exclude example.test/a v1.0\n",
			refusal: "exclude is not",
		},
		// Go would ask the proxy to resolve these as queries.
		{name: "retract of a query", goMod: module + "retract latest\n", refusal: "retract is not"},
		{name: "retract of an open interval", goMod: module + "retract [v0.1.0, latest]\n", refusal: "retract is not"},
		{
			name: "require of a prerelease with a leading zero", goMod: module + "require example.test/a v1.0.0-01\n",
			refusal: "canonical version",
		},
		{name: "require without a version", goMod: module + "require example.test/a\n", refusal: "malformed require"},
		{
			name: "require of a non-canonical version", goMod: module + "require example.test/a v1\n",
			refusal: "canonical version",
		},
		{
			name: "replace without a target version", goMod: module + "replace example.test/a => example.test/b\n",
			refusal: "malformed replace",
		},
		{
			name: "replace to a local path", goMod: module + "replace example.test/a => ./a\n",
			refusal: "local path",
		},
		{name: "go above the baked Go", goMod: "module m\n\ngo 1.99\n", refusal: "above the baked Go"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			requires, err := goModuleRequirements([]byte(test.goMod))
			if test.refusal != "" {
				if err == nil || !strings.Contains(err.Error(), test.refusal) {
					t.Fatalf("goModuleRequirements = %v, %v; want a refusal naming %q", requires, err, test.refusal)
				}
				return
			}
			if err != nil {
				t.Fatalf("goModuleRequirements: %v", err)
			}
			var got []string
			for _, required := range requires {
				got = append(got, required.String())
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("requirements = %v, want %v", got, test.want)
			}
		})
	}
}

func TestEvaluateGoModuleDeltaCountsAVersionGoModAloneChooses(t *testing.T) {
	t.Parallel()
	// A tidy go.sum holds the go.mod hash of versions the build never
	// fetched, so go.mod alone can switch to one and go.sum stays the same.
	const goSum = "example.test/dep v1.0.0 h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n" +
		"example.test/dep v1.0.0/go.mod h1:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=\n" +
		"example.test/dep v0.9.0/go.mod h1:CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC=\n"
	read := func(require string) goModuleManifests {
		manifests, err := readGoModuleManifests(
			[]byte("module example.test/m\n\ngo 1.26\n\nrequire example.test/dep "+require+"\n"), []byte(goSum))
		if err != nil {
			t.Fatalf("readGoModuleManifests: %v", err)
		}
		return manifests
	}
	base, head := read("v1.0.0"), read("v0.9.0")
	_, findings := evaluateGoModuleDelta(base, head, &domain.RegistrySet{Hosts: []string{"registry.npmjs.org"}})
	if len(findings) != 1 || findings[0].clause != RebuildRefusalUndeclaredAuthority ||
		!strings.Contains(findings[0].detail, `"example.test/dep@v0.9.0"`) {
		t.Fatalf("findings = %+v, want undeclared_authority naming example.test/dep@v0.9.0", findings)
	}
	delta, findings := evaluateGoModuleDelta(base, head, &domain.RegistrySet{Hosts: []string{goModuleProxyHost}})
	if len(findings) != 0 || delta.Changed != 1 || !slices.Equal(delta.Hosts, []string{goModuleProxyHost}) {
		t.Fatalf("delta, findings = %+v, %+v; want one changed module from the proxy", delta, findings)
	}
}

func TestReadGoModuleChangeRefusesABaseTheGateCannotRead(t *testing.T) {
	t.Parallel()
	// A base built under an older pinned Go may name its toolchain, which
	// the reader refuses once the pin moves; the change then can't be
	// bounded against it.
	files := map[string]string{
		"base/go.mod": "module example.test/m\n\ngo 1.26\n\ntoolchain go1.26.0\n",
		"base/go.sum": "",
		"head/go.mod": "module example.test/m\n\ngo 1.26\n",
		"head/go.sum": "",
	}
	read := func(commitSHA, name string) ([]byte, error) { return []byte(files[commitSHA+"/"+name]), nil }
	observed := domain.ProjectImageBaseInputs{
		CommitSHA: "head", GoModSHA256: manifestSHA256([]byte(files["head/go.mod"])),
		GoSumSHA256: manifestSHA256(nil),
	}
	_, _, refusal, err := readGoModuleChange(read, ".", observed, "base")
	if err != nil {
		t.Fatalf("readGoModuleChange: %v", err)
	}
	if refusal == nil || refusal.clause != RebuildRefusalGoDependencyChanged ||
		!strings.Contains(refusal.detail, "the base's Go module is not one the gate reads") {
		t.Fatalf("refusal = %+v, want go_dependency_changed naming the base", refusal)
	}
}
