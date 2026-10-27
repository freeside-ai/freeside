package projectimage

import (
	"cmp"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// The Go module reader is the Go counterpart of the lockfile reader, and the
// same trust boundary: go.mod and go.sum are the candidate's bytes, and Go
// reads them again inside the build with network access. Anything it does not
// positively recognize refuses the gate.
//
// What Go does with these files decides what must be recognized. The warm
// step fetches every module from goModuleProxyHost, never from the module's
// own VCS host, and verifies each download against go.sum, or against the
// checksum database for a module go.sum does not pin. So go.mod and go.sum
// choose which module versions are fetched, never where from: the gate checks
// that go.sum pins what go.mod requires and that the project declares the
// proxy, and leaves the content to Go's own verification.

var (
	// goModulePathPattern is a module path as Go's module.CheckPath accepts
	// it, narrowed to plain elements: a lowercase first element holding a
	// dot, then elements of letters, digits, and "-_.~" that neither start
	// nor end with a dot.
	goModulePathPattern = regexp.MustCompile(
		`^[a-z0-9-]+(?:\.[a-z0-9-]+)+(?:/[A-Za-z0-9_~-]+(?:\.[A-Za-z0-9_~-]+)*)*$`)
	// goModuleVersionPattern is a canonical module version: a semantic
	// version with an optional prerelease (a pseudo-version is one) and no
	// build metadata but +incompatible. A prerelease identifier of digits
	// alone has no leading zero, as semver requires; Go reads a version that
	// breaks this as a query and asks the proxy to resolve it.
	goModuleVersionPattern = regexp.MustCompile(
		`^v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)` +
			`(?:-` + goPrereleaseIdentifier + `(?:\.` + goPrereleaseIdentifier + `)*)?(?:\+incompatible)?$`)
	// goSumHashPattern is the one hash kind Go checks a download against.
	// Go reads a line holding another kind and then checks nothing against
	// it, so such a line pins nothing.
	goSumHashPattern = regexp.MustCompile(`^h1:[A-Za-z0-9+/]{43}=$`)
)

const (
	goPrereleaseIdentifier = `(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)`
	// goEmptyGoModHash is the h1 hash of an empty go.mod, which an old Go bug
	// wrote into go.sum files. Go drops a go.sum line holding it, so such a
	// line pins nothing.
	goEmptyGoModHash = "h1:G7mAYYxgmS0lVkHyy2hEOLQCFB0DlQFTMLWggykrydY="
)

// goModVerbs are the directives Go 1.26 reads in a main module's go.mod; it
// refuses any other.
var goModVerbs = []string{
	"module", "go", "toolchain", "godebug", "require", "exclude", "replace", "retract", "tool", "ignore",
}

// goModuleVersion is one version of one module.
type goModuleVersion struct {
	path, version string
}

func (m goModuleVersion) String() string { return m.path + "@" + m.version }

func compareGoModuleVersions(a, b goModuleVersion) int {
	return cmp.Or(strings.Compare(a.path, b.path), strings.Compare(a.version, b.version))
}

// goSumKey is what one go.sum line pins: a module version's zip, or with
// goMod its go.mod alone.
type goSumKey struct {
	module goModuleVersion
	goMod  bool
}

// goModuleManifests is what one commit's go.mod and go.sum say about the
// modules a build fetches.
type goModuleManifests struct {
	// requires are go.mod's requirements, each already resolved through its
	// replace directives to the module version Go fetches for it.
	requires []goModuleVersion
	// sums are go.sum's pins by key.
	sums map[goSumKey]string
}

// readGoModuleManifests reads one commit's go.mod and go.sum. An absent file
// reads as empty bytes; the caller refuses a candidate that lacks either,
// since the builder copies both.
func readGoModuleManifests(goMod, goSum []byte) (goModuleManifests, error) {
	requires, err := goModuleRequirements(goMod)
	if err != nil {
		return goModuleManifests{}, err
	}
	sums, err := parseGoSum(goSum)
	if err != nil {
		return goModuleManifests{}, err
	}
	return goModuleManifests{requires: requires, sums: sums}, nil
}

// readGoModuleChange reads the go.mod and go.sum of the module in moduleDir at
// baseSHA and at the head that observed holds. A head lacking either file, or
// either commit holding one the reader refuses, is the gate's
// go_dependency_changed refusal; an error is a fault reading the checkout.
func readGoModuleChange(
	read func(commitSHA, name string) ([]byte, error),
	moduleDir string,
	observed domain.ProjectImageBaseInputs,
	baseSHA string,
) (base, head goModuleManifests, refusal *lockfileRefusal, err error) {
	goModPath, goSumPath := path.Join(moduleDir, "go.mod"), path.Join(moduleDir, "go.sum")
	// The builder copies both files, so a candidate must hold both. The base
	// is the commit an image was built from, so an absent file there reads
	// as empty.
	for _, manifest := range []struct{ name, hash string }{
		{goModPath, observed.GoModSHA256}, {goSumPath, observed.GoSumSHA256},
	} {
		if manifest.hash == "" {
			return base, head, refuseLockfile(RebuildRefusalGoDependencyChanged,
				"the candidate holds no %s, which every Go image builds from", manifest.name), nil
		}
	}
	var contents [4][]byte
	for index, file := range []struct{ commitSHA, name string }{
		{observed.CommitSHA, goModPath}, {observed.CommitSHA, goSumPath}, {baseSHA, goModPath}, {baseSHA, goSumPath},
	} {
		if contents[index], err = read(file.commitSHA, file.name); err != nil {
			return base, head, nil, err
		}
	}
	if head, err = readGoModuleManifests(contents[0], contents[1]); err != nil {
		return base, head, refuseLockfile(RebuildRefusalGoDependencyChanged,
			"the candidate's Go module is not one an image serves: %s", quotedLockfileText(err.Error())), nil
	}
	if base, err = readGoModuleManifests(contents[2], contents[3]); err != nil {
		return base, head, refuseLockfile(RebuildRefusalGoDependencyChanged,
			"the base's Go module is not one the gate reads, so the change cannot be bounded: %s",
			quotedLockfileText(err.Error())), nil
	}
	return base, head, nil, nil
}

// parseGoSum reads go.sum as Go does, splitting each line into fields and
// dropping a line that holds goEmptyGoModHash, and admits only lines of the
// shape Go writes: a module path, a version with or without /go.mod, and an
// h1 hash. A key go.sum pins twice must pin the same hash both times.
func parseGoSum(goSum []byte) (map[goSumKey]string, error) {
	sums := map[goSumKey]string{}
	for index, line := range strings.Split(string(goSum), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		malformed := fmt.Errorf("go.sum line %d is not a module path, a version, and an h1 hash", index+1)
		if len(fields) != 3 {
			return nil, malformed
		}
		if fields[2] == goEmptyGoModHash {
			continue
		}
		version, goMod := strings.CutSuffix(fields[1], "/go.mod")
		if !goModulePathPattern.MatchString(fields[0]) || !goModuleVersionPattern.MatchString(version) ||
			!goSumHashPattern.MatchString(fields[2]) {
			return nil, malformed
		}
		key := goSumKey{module: goModuleVersion{path: fields[0], version: version}, goMod: goMod}
		if held, repeated := sums[key]; repeated && held != fields[2] {
			return nil, fmt.Errorf("go.sum line %d pins %s a second time with another hash", index+1, fields[1])
		}
		sums[key] = fields[2]
	}
	return sums, nil
}

// goModuleRequirements reads go.mod's requirements, resolved through its
// replace directives as Go resolves them: a replace naming a version applies
// to that version alone and wins over one naming none. It first applies
// checkGoModule, and refuses a directive Go does not read; a require,
// exclude, or replace that is not plain module paths and canonical versions;
// a retract of anything but canonical versions, which Go would ask the proxy
// to resolve; and two replaces of one module version with different targets,
// which Go refuses as conflicting.
func goModuleRequirements(goMod []byte) ([]goModuleVersion, error) {
	if err := checkGoModule(goMod); err != nil {
		return nil, err
	}
	directives, err := goModDirectives(goMod)
	if err != nil {
		return nil, err
	}
	module := func(path, version string) (goModuleVersion, bool) {
		return goModuleVersion{path: path, version: version},
			goModulePathPattern.MatchString(path) && goModuleVersionPattern.MatchString(version)
	}
	var requires []goModuleVersion
	pathReplace := map[string]goModuleVersion{}
	versionReplace := map[goModuleVersion]goModuleVersion{}
	for _, directive := range directives {
		switch directive.verb {
		case "require":
			if len(directive.args) != 2 {
				return nil, fmt.Errorf("go.mod line %d: malformed require directive", directive.line)
			}
			required, ok := module(directive.args[0], directive.args[1])
			if !ok {
				return nil, fmt.Errorf("go.mod line %d: require is not a module path and a canonical version",
					directive.line)
			}
			requires = append(requires, required)
		case "exclude":
			if len(directive.args) != 2 {
				return nil, fmt.Errorf("go.mod line %d: malformed exclude directive", directive.line)
			}
			if _, ok := module(directive.args[0], directive.args[1]); !ok {
				return nil, fmt.Errorf("go.mod line %d: exclude is not a module path and a canonical version",
					directive.line)
			}
		case "retract":
			if !goRetractIsCanonical(directive.args) {
				return nil, fmt.Errorf("go.mod line %d: retract is not a canonical version or interval",
					directive.line)
			}
		case "replace":
			// Go's own shape: "old [version] => new version". checkGoModule
			// has already refused a target that is a filesystem path, the
			// one target that takes no version.
			args := directive.args
			arrow := slices.Index(args, "=>")
			if (arrow != 1 && arrow != 2) || len(args) != arrow+3 {
				return nil, fmt.Errorf("go.mod line %d: malformed replace directive", directive.line)
			}
			target, ok := module(args[arrow+1], args[arrow+2])
			if !ok || !goModulePathPattern.MatchString(args[0]) ||
				(arrow == 2 && !goModuleVersionPattern.MatchString(args[1])) {
				return nil, fmt.Errorf("go.mod line %d: replace is not module paths and canonical versions",
					directive.line)
			}
			conflicting := false
			if arrow == 2 {
				old := goModuleVersion{path: args[0], version: args[1]}
				previous, repeated := versionReplace[old]
				conflicting = repeated && previous != target
				versionReplace[old] = target
			} else {
				previous, repeated := pathReplace[args[0]]
				conflicting = repeated && previous != target
				pathReplace[args[0]] = target
			}
			if conflicting {
				return nil, fmt.Errorf("go.mod line %d: conflicting replacements for %s",
					directive.line, quotedLockfileText(strings.Join(args[:arrow], " ")))
			}
		default:
			if !slices.Contains(goModVerbs, directive.verb) {
				return nil, fmt.Errorf("go.mod line %d: unknown directive %s",
					directive.line, quotedLockfileText(directive.verb))
			}
		}
	}
	for index, required := range requires {
		if target, replaced := versionReplace[required]; replaced {
			requires[index] = target
		} else if target, replaced := pathReplace[required.path]; replaced {
			requires[index] = target
		}
	}
	return requires, nil
}

// goRetractIsCanonical reports whether a retract directive's arguments are a
// canonical version or an interval "[low, high]" of canonical versions.
func goRetractIsCanonical(args []string) bool {
	switch len(args) {
	case 1:
		return goModuleVersionPattern.MatchString(args[0])
	case 2:
		low, opened := strings.CutPrefix(args[0], "[")
		low, separated := strings.CutSuffix(low, ",")
		high, closed := strings.CutSuffix(args[1], "]")
		return opened && separated && closed &&
			goModuleVersionPattern.MatchString(low) && goModuleVersionPattern.MatchString(high)
	default:
		return false
	}
}

// evaluateGoModuleDelta applies the Go module clauses of the rebuild gate to
// a candidate whose go.mod or go.sum differs from its base's. It holds when:
//
//   - go.sum holds the go.mod hash of every module version go.mod requires,
//     after its replace directives. Only that hash is required: a tidy go.sum
//     holds it for every required module, but no zip hash for a module that
//     provides no packages.
//   - When go.sum adds or changes any module version, or go.mod comes to
//     require one its base did not, the project's registry set declares
//     goModuleProxyHost, the one host the build fetches from. The second
//     case matters because a tidy go.sum already holds the go.mod hash of
//     versions the build never fetched, so go.mod alone can switch to one.
//
// Each refusing clause is returned once, in that order; no findings means the
// delta holds. A nil registries skips only the authority check, as in
// evaluateLockfileDelta.
func evaluateGoModuleDelta(
	base, head goModuleManifests, registries *domain.RegistrySet,
) (LockfileDelta, gateFindings) {
	var findings gateFindings
	requires := slices.SortedFunc(slices.Values(head.requires), compareGoModuleVersions)
	for _, required := range requires {
		if _, pinned := head.sums[goSumKey{module: required, goMod: true}]; !pinned {
			findings.add(refuseLockfile(RebuildRefusalUnpinnedSource,
				"module %s, which go.mod requires, has no go.mod hash in go.sum",
				quotedLockfileText(required.String())))
			break
		}
	}
	// A module version counts once, whichever of its two lines changed.
	held, changed, removed := map[goModuleVersion]bool{}, map[goModuleVersion]bool{}, map[goModuleVersion]bool{}
	for key, hash := range head.sums {
		held[key.module] = true
		if previous, existed := base.sums[key]; !existed || previous != hash {
			changed[key.module] = true
		}
	}
	for key := range base.sums {
		if !held[key.module] {
			removed[key.module] = true
		}
	}
	for _, required := range head.requires {
		if !slices.Contains(base.requires, required) {
			changed[required] = true
		}
	}
	if len(changed) != 0 && registries != nil && !slices.Contains(registries.Hosts, goModuleProxyHost) {
		first := slices.SortedFunc(maps.Keys(changed), compareGoModuleVersions)[0]
		findings.add(refuseLockfile(RebuildRefusalUndeclaredAuthority,
			"module %s resolves from %s, which the project's registry set does not declare",
			quotedLockfileText(first.String()), quotedLockfileText(goModuleProxyHost)))
	}
	if len(findings) != 0 {
		return LockfileDelta{}, findings
	}
	delta := LockfileDelta{Changed: len(changed), Removed: len(removed)}
	if len(changed) != 0 {
		delta.Hosts = []string{goModuleProxyHost}
	}
	return delta, nil
}
