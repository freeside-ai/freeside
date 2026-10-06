package projectimage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// RebuildRefusal names the clause of the policy-gated rebuild (plan §5.7) that
// kept a dependency change on the fail-loud path. The zero value "" is invalid
// by design: a decision that holds carries no refusal.
type RebuildRefusal string

const (
	// RebuildRefusalUndeclaredAuthority: an added or changed package resolves
	// from a host outside the project policy's declared registry set.
	RebuildRefusalUndeclaredAuthority RebuildRefusal = "undeclared_authority"
	// RebuildRefusalUnpinnedSource: an added or changed package is not a
	// registry tarball pinned by an https URL and a sha512 integrity value (a
	// link, a workspace member, a VCS or file source, a bundled package, a
	// missing pin), or the change declares a dependency by anything but a
	// registry version, range, or tag.
	RebuildRefusalUnpinnedSource RebuildRefusal = "unpinned_source"
	// RebuildRefusalRecipeChanged: the candidate changes the verification
	// recipe its tree declares.
	RebuildRefusalRecipeChanged RebuildRefusal = "recipe_changed"
	// RebuildRefusalLockfileInconsistent: the lockfile is not a readable npm
	// v2 or v3 lockfile whose root agrees with package.json and that holds an
	// entry npm accepts for every dependency the change reaches.
	RebuildRefusalLockfileInconsistent RebuildRefusal = "lockfile_inconsistent"
	// RebuildRefusalUnsupportedInput: the candidate holds an npm input no
	// project image supports, or changes one a rebuild cannot bound
	// (package.json's overrides, workspaces, or packageManager).
	RebuildRefusalUnsupportedInput RebuildRefusal = "unsupported_npm_input"
	// RebuildRefusalNoRegistrySet: the run's policy declares no registry set,
	// or a malformed one.
	RebuildRefusalNoRegistrySet RebuildRefusal = "no_registry_set"
	// RebuildRefusalNotConfigured: the gate holds but this daemon has no
	// builder inputs to rebuild with.
	RebuildRefusalNotConfigured RebuildRefusal = "rebuild_not_configured"
	// RebuildRefusalProofFailed: the gate holds but the rebuilt image failed
	// its build-time proof (the networkless positive run or the masked-cache
	// probe), the builder refused the derived request, or the build did not
	// finish within the caller's bound.
	RebuildRefusalProofFailed RebuildRefusal = "build_or_proof_failed"
)

// AllRebuildRefusals lists every valid RebuildRefusal.
var AllRebuildRefusals = []RebuildRefusal{
	RebuildRefusalUndeclaredAuthority, RebuildRefusalUnpinnedSource,
	RebuildRefusalRecipeChanged, RebuildRefusalLockfileInconsistent,
	RebuildRefusalUnsupportedInput, RebuildRefusalNoRegistrySet,
	RebuildRefusalNotConfigured, RebuildRefusalProofFailed,
}

func (r RebuildRefusal) valid() bool {
	switch r {
	case RebuildRefusalUndeclaredAuthority, RebuildRefusalUnpinnedSource,
		RebuildRefusalRecipeChanged, RebuildRefusalLockfileInconsistent,
		RebuildRefusalUnsupportedInput, RebuildRefusalNoRegistrySet,
		RebuildRefusalNotConfigured, RebuildRefusalProofFailed:
		return true
	default:
		return false
	}
}

// LockfileDelta summarizes what a candidate's package-lock.json changes
// against its base's. It is the account a rebuild record keeps, never the
// parsed tree.
type LockfileDelta struct {
	// Changed counts packages the head lockfile adds or pins differently.
	Changed int `json:"changed"`
	// Removed counts packages only the base lockfile holds.
	Removed int `json:"removed"`
	// Hosts are the registry hosts the changed packages resolve from, sorted.
	Hosts []string `json:"hosts"`
}

// lockfileRefusal is a gate verdict from the lockfile reader: the clause and
// a short account of what failed it. Detail may quote text the candidate
// wrote, always bounded and quoted.
type lockfileRefusal struct {
	clause RebuildRefusal
	detail string
}

func refuseLockfile(clause RebuildRefusal, format string, args ...any) *lockfileRefusal {
	return &lockfileRefusal{clause: clause, detail: fmt.Sprintf(format, args...)}
}

// quotedLockfileText bounds and quotes candidate-written text for a refusal
// detail: a lockfile key or URL can be megabytes long and hold any bytes.
func quotedLockfileText(text string) string {
	const limit = 200
	if len(text) > limit {
		text = strings.ToValidUTF8(text[:limit], "") + "..."
	}
	return strconv.Quote(text)
}

// The lockfile and package.json are the candidate's bytes, and npm reads them
// again inside the build with network access. So this reader is a
// returned-object trust boundary with one rule: anything it does not
// positively recognize in what the candidate changed refuses the gate.
//
// What npm does with these files decides what must be recognized. `npm ci`
// trusts a lock entry's `resolved` only while every dependency edge that
// reaches the entry is satisfied by it. An edge whose spec is a URL, a git
// reference, or a path is satisfied only by an entry resolved from that exact
// source, so npm quietly re-resolves it from the spec, and its lockfile check
// compares nothing but location and version. An entry with no `resolved` is
// fetched by name and version, or by its `version` when that is a URL. The
// package name npm fetches is spliced from the entry's key. A `hasShrinkwrap`
// entry hands resolution to a file inside its tarball.

const (
	packageNamePattern = `[A-Za-z0-9][A-Za-z0-9._-]*`
	scopedNamePattern  = `(?:@` + packageNamePattern + `/)?` + packageNamePattern
)

var (
	// lockfileKeyPattern is the only shape of `packages` key a changed entry
	// may have: nested node_modules directories of plain package names. npm
	// derives the name it fetches from the key, so a key holding `:`, `@`, or
	// `#` past the scope would be parsed as a source.
	lockfileKeyPattern = regexp.MustCompile(
		`^node_modules/` + scopedNamePattern + `(?:/node_modules/` + scopedNamePattern + `)*$`)
	packageNameRegexp = regexp.MustCompile(`^` + scopedNamePattern + `$`)
	// exactVersionPattern is a plain semantic version, never a URL or a range.
	exactVersionPattern = regexp.MustCompile(
		`^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)
	// registrySpecPattern holds the characters of a version, a range, or a
	// dist-tag. It has no `:`, `/`, `@`, `#`, or `\`, the characters every
	// URL, git, alias, and path spec needs.
	registrySpecPattern = regexp.MustCompile(`^[0-9A-Za-z^~<>=|*+ .-]*$`)
	// tarballSpecPattern is npm's own test for a spec that names a local file,
	// copied with its unescaped dot so the two agree on every input.
	tarballSpecPattern = regexp.MustCompile(`(?i)[.](?:tgz|tar.gz|tar)$`)
	// sha512IntegrityPattern is one sha512 SRI value. npm verifies a tarball
	// against the strongest hash it recognizes and against nothing at all when
	// it recognizes none, so a weaker or malformed value is no pin.
	sha512IntegrityPattern = regexp.MustCompile(`^sha512-[A-Za-z0-9+/]{86}==$`)
)

// dependencyFields are the maps of name to spec that both package.json and a
// lock entry declare dependency edges with.
var dependencyFields = []string{
	"dependencies", "devDependencies", "optionalDependencies", "peerDependencies",
}

// gitForgeHosts are the hosts npm reads a URL on as a git repository, which it
// clones without checking an integrity value. A registry that shares one of
// these hosts is refused with it.
var gitForgeHosts = []string{"github.com", "gist.github.com", "gitlab.com", "bitbucket.org", "git.sr.ht"}

// inertEntryFields are the lock-entry fields that say nothing about where a
// package comes from or what resolves beneath it.
var inertEntryFields = []string{
	"dev", "optional", "devOptional", "peer", "hasInstallScript", "deprecated",
	"license", "engines", "os", "cpu", "libc", "bin", "funding", "peerDependenciesMeta",
}

// decodeObject decodes one JSON object into its members, refusing a repeated
// key. Fields are then looked up by exact key, never through struct tags:
// encoding/json matches struct fields case-insensitively and npm does not, so
// a struct decode would let `"Resolved"` shadow the `"resolved"` npm fetches.
//
// The repeat check is also what keeps two parsers on one answer. Go decodes an
// unpaired surrogate escape as U+FFFD and JavaScript keeps it, so a key npm
// sees as distinct can land on another here; the two then repeat. The check is
// the daemon's shared one, which also refuses two keys that differ only by
// case, at any depth. npm tells those apart, so this refuses a tree that holds
// both, which no case-insensitive filesystem could install either.
func decodeObject(raw []byte) (map[string]json.RawMessage, error) {
	if !bytes.HasPrefix(bytes.TrimLeft(raw, " \t\r\n"), []byte("{")) {
		return nil, errors.New("is not a JSON object")
	}
	if err := ward.RejectDuplicateJSONKeys(raw); err != nil {
		return nil, errors.New("is not one JSON object with distinct keys")
	}
	var members map[string]json.RawMessage
	if err := strictjson.Decode(raw, &members, strictjson.RejectInvalidUTF8, maxManifestBytes); err != nil {
		return nil, errors.New("is not a JSON object")
	}
	return members, nil
}

// jsonString reads a member that must be a JSON string.
func jsonString(raw json.RawMessage) (string, bool) {
	var value string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, true
}

// sameJSON reports whether two members hold the same bytes apart from
// whitespace. Byte equality is the one comparison that needs no model of what
// npm reads: an entry the base holds byte for byte is the entry a person
// already approved. An absent member equals only an absent one.
func sameJSON(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	var compactA, compactB bytes.Buffer
	if json.Compact(&compactA, a) != nil || json.Compact(&compactB, b) != nil {
		return false
	}
	return bytes.Equal(compactA.Bytes(), compactB.Bytes())
}

// registrySpec reports whether spec asks the registry for a package by
// version, range, or tag. Every other kind of spec names its own source. An
// `npm:` alias is refused with them: it asks the registry too, but for another
// package than the entry's key names, and the gate does not model how npm
// matches the two.
func registrySpec(spec string) bool {
	return registrySpecPattern.MatchString(spec) && !strings.HasPrefix(spec, ".") &&
		!tarballSpecPattern.MatchString(spec)
}

// dependencySpecs reads one dependency map of a package object: absent is
// empty, and anything but an object of strings is an error.
func dependencySpecs(pkg map[string]json.RawMessage, field string) (map[string]string, error) {
	raw, present := pkg[field]
	if !present {
		return map[string]string{}, nil
	}
	members, err := decodeObject(raw)
	if err != nil {
		return nil, fmt.Errorf("%s %w", field, err)
	}
	specs := make(map[string]string, len(members))
	for name, value := range members {
		spec, isString := jsonString(value)
		if !isString {
			return nil, fmt.Errorf("%s is not an object of version specs", field)
		}
		specs[name] = spec
	}
	return specs, nil
}

// declaredDependencies reads every dependency map of one package object.
func declaredDependencies(pkg map[string]json.RawMessage) (map[string]map[string]string, error) {
	declared := make(map[string]map[string]string, len(dependencyFields))
	for _, field := range dependencyFields {
		specs, err := dependencySpecs(pkg, field)
		if err != nil {
			return nil, err
		}
		declared[field] = specs
	}
	return declared, nil
}

type parsedLockfile struct {
	root map[string]json.RawMessage
	// packages holds every entry but the root's, undecoded.
	packages map[string]json.RawMessage
}

// parseLockfile reads the frame of an npm v2 or v3 package-lock.json: its
// version, its root entry, and its package entries by key.
func parseLockfile(content []byte) (parsedLockfile, error) {
	top, err := decodeObject(content)
	if err != nil {
		return parsedLockfile{}, fmt.Errorf("package-lock.json %w", err)
	}
	// The literal token, not a decoded number: "3" and 3.0 are forms npm does
	// not write, and nothing here needs to guess how it would read them.
	if version := string(top["lockfileVersion"]); version != "2" && version != "3" {
		return parsedLockfile{}, errors.New("package-lock.json lockfileVersion is not 2 or 3")
	}
	packages, err := decodeObject(top["packages"])
	if err != nil {
		return parsedLockfile{}, fmt.Errorf("package-lock.json packages %w", err)
	}
	rawRoot, hasRoot := packages[""]
	if !hasRoot {
		return parsedLockfile{}, errors.New("package-lock.json has no root package entry")
	}
	root, err := decodeObject(rawRoot)
	if err != nil {
		return parsedLockfile{}, fmt.Errorf("package-lock.json root package %w", err)
	}
	delete(packages, "")
	return parsedLockfile{root: root, packages: packages}, nil
}

// enclosingPackage is the key of the package whose directory holds key's.
func enclosingPackage(key string) (string, bool) {
	cut := strings.LastIndex(key, "/node_modules/")
	if cut < 0 {
		return "", false
	}
	return key[:cut], true
}

// examineChangedPackage applies the gate to one lock entry the candidate adds
// or changes, and returns the declared host it resolves from.
func examineChangedPackage(
	key string, raw json.RawMessage, registries domain.RegistrySet,
) (string, *lockfileRefusal) {
	quotedKey := quotedLockfileText(key)
	if !lockfileKeyPattern.MatchString(key) {
		return "", refuseLockfile(RebuildRefusalUnpinnedSource,
			"package %s is not installed under node_modules by a plain package name", quotedKey)
	}
	entry, err := decodeObject(raw)
	if err != nil {
		return "", refuseLockfile(RebuildRefusalLockfileInconsistent,
			"package-lock.json package %s %v", quotedKey, err)
	}
	var version, resolved, integrity string
	for _, field := range slices.Sorted(maps.Keys(entry)) {
		value := entry[field]
		switch {
		case field == "version" || field == "resolved" || field == "integrity":
			text, isString := jsonString(value)
			if !isString {
				return "", refuseLockfile(RebuildRefusalLockfileInconsistent,
					"package-lock.json package %s has a malformed %s", quotedKey, field)
			}
			switch field {
			case "version":
				version = text
			case "resolved":
				resolved = text
			case "integrity":
				integrity = text
			}
		case field == "link" || field == "inBundle" || field == "hasShrinkwrap" || field == "extraneous":
			// Each hands the package's source to something the lockfile does
			// not pin: a local directory, another package's tarball, or a
			// second lockfile inside this one's tarball.
			if string(value) != "false" {
				return "", refuseLockfile(RebuildRefusalUnpinnedSource,
					"package %s is marked %s, so the lockfile does not pin its source", quotedKey, field)
			}
		case slices.Contains(dependencyFields, field):
			specs, err := dependencySpecs(entry, field)
			if err != nil {
				return "", refuseLockfile(RebuildRefusalLockfileInconsistent,
					"package-lock.json package %s %v", quotedKey, err)
			}
			for _, name := range slices.Sorted(maps.Keys(specs)) {
				if !packageNameRegexp.MatchString(name) || !registrySpec(specs[name]) {
					return "", refuseLockfile(RebuildRefusalUnpinnedSource,
						"package %s depends on %s as %s, which is not a registry version, range, or tag",
						quotedKey, quotedLockfileText(name), quotedLockfileText(specs[name]))
				}
			}
		case slices.Contains(inertEntryFields, field):
		default:
			// Among them `name`, which an aliased entry is fetched under, and
			// `bundleDependencies`.
			return "", refuseLockfile(RebuildRefusalUnpinnedSource,
				"package %s carries %s, which the gate does not admit on a changed package",
				quotedKey, quotedLockfileText(field))
		}
	}
	if !exactVersionPattern.MatchString(version) {
		return "", refuseLockfile(RebuildRefusalUnpinnedSource,
			"package %s has version %s, which is not an exact version", quotedKey, quotedLockfileText(version))
	}
	if resolved == "" || !sha512IntegrityPattern.MatchString(integrity) {
		return "", refuseLockfile(RebuildRefusalUnpinnedSource,
			"package %s has no resolved URL or no sha512 integrity value", quotedKey)
	}
	source, err := url.Parse(resolved)
	if err != nil || source.Scheme != "https" || source.Opaque != "" || source.User != nil {
		return "", refuseLockfile(RebuildRefusalUnpinnedSource,
			"package %s resolves from %s, which is not an https registry URL",
			quotedKey, quotedLockfileText(resolved))
	}
	// The declared set holds bare lowercase host names. A port is part of the
	// authority, so a URL that names one matches nothing declared. The literal
	// prefix is the second half of the check: npm parses this string with a
	// different URL parser, and a string that begins with exactly this scheme,
	// host, and slash names that host to any parser.
	if !slices.Contains(registries.Hosts, source.Host) ||
		!strings.HasPrefix(resolved, "https://"+source.Host+"/") {
		return "", refuseLockfile(RebuildRefusalUndeclaredAuthority,
			"package %s resolves from %s, which the project's registry set does not declare",
			quotedKey, quotedLockfileText(source.Host))
	}
	if slices.Contains(gitForgeHosts, strings.TrimPrefix(source.Host, "www.")) {
		return "", refuseLockfile(RebuildRefusalUnpinnedSource,
			"package %s resolves from %s, where npm reads a URL as a git repository",
			quotedKey, quotedLockfileText(source.Host))
	}
	return source.Host, nil
}

// optionalPeers reads the names a package's peerDependenciesMeta marks
// optional. Anything it cannot read is not optional.
func optionalPeers(pkg map[string]json.RawMessage) map[string]bool {
	optional := map[string]bool{}
	meta, err := decodeObject(pkg["peerDependenciesMeta"])
	if err != nil {
		return optional
	}
	for name, raw := range meta {
		if flags, err := decodeObject(raw); err == nil && string(flags["optional"]) == "true" {
			optional[name] = true
		}
	}
	return optional
}

// resolveDependency finds the entry npm resolves name to from the package at
// key ("" for the root): the nearest node_modules directory at or above it.
func resolveDependency(packages map[string]json.RawMessage, key, name string) (string, bool) {
	for {
		candidate := "node_modules/" + name
		if key != "" {
			candidate = key + "/" + candidate
		}
		if _, held := packages[candidate]; held {
			return candidate, true
		}
		if key == "" {
			return "", false
		}
		key, _ = enclosingPackage(key)
	}
}

// evaluateLockfileDelta applies the dependency clauses of the rebuild gate to
// one candidate. It holds when:
//
//   - The candidate leaves package.json's overrides, workspaces, and
//     packageManager as its base has them. Each changes how every dependency
//     resolves, or what resolves it.
//   - Every dependency package.json adds or respecifies is a registry version,
//     range, or tag.
//   - The head lockfile's root declares the dependencies package.json does.
//   - Every lock entry the candidate adds or changes is a registry tarball
//     from a declared host (examineChangedPackage).
//   - The lockfile holds an entry for every dependency of the root and of each
//     added or changed entry, and no peer dependency resolves to an entry
//     nested under its own dependent (checkDependencyEdges).
//
// The last rule is narrower than what it guards. npm keeps a lock entry only
// while every dependency reaching it is satisfied, and otherwise asks the
// default registry for the package and follows whatever that registry's
// metadata names. This reader checks that a dependency has an entry, not that
// the entry's version satisfies the dependency's range, so it does not bound
// that case; only a control on the build's own connections can.
//
// An entry the base lockfile holds byte for byte is not re-examined: it is in
// the image a person approved. The one exception is a bundled entry with no
// source of its own, which is only as approved as the package that ships it.
// A package the head drops needs no check.
func evaluateLockfileDelta(
	basePackageJSON, basePackageLock, headPackageJSON, headPackageLock []byte,
	registries domain.RegistrySet,
) (LockfileDelta, *lockfileRefusal) {
	inconsistent := func(format string, args ...any) (LockfileDelta, *lockfileRefusal) {
		return LockfileDelta{}, refuseLockfile(RebuildRefusalLockfileInconsistent, format, args...)
	}
	head, err := parseLockfile(headPackageLock)
	if err != nil {
		return inconsistent("%v", err)
	}
	base, err := parseLockfile(basePackageLock)
	if err != nil {
		return inconsistent("the base's %v, so the change cannot be bounded", err)
	}
	manifest, err := decodeObject(headPackageJSON)
	if err != nil {
		return inconsistent("package.json %v", err)
	}
	baseManifest, err := decodeObject(basePackageJSON)
	if err != nil {
		return inconsistent("the base's package.json %v, so the change cannot be bounded", err)
	}
	for _, field := range []string{"overrides", "workspaces", "packageManager"} {
		if !sameJSON(manifest[field], baseManifest[field]) {
			return LockfileDelta{}, refuseLockfile(RebuildRefusalUnsupportedInput,
				"the candidate changes package.json %s", field)
		}
	}
	declared, err := declaredDependencies(manifest)
	if err != nil {
		return inconsistent("package.json %v", err)
	}
	baseDeclared, err := declaredDependencies(baseManifest)
	if err != nil {
		return inconsistent("the base's package.json %v, so the change cannot be bounded", err)
	}
	locked, err := declaredDependencies(head.root)
	if err != nil {
		return inconsistent("package-lock.json root %v", err)
	}
	for _, field := range dependencyFields {
		for _, name := range slices.Sorted(maps.Keys(declared[field])) {
			spec := declared[field][name]
			if previous, existed := baseDeclared[field][name]; existed && previous == spec {
				continue
			}
			if !packageNameRegexp.MatchString(name) || !registrySpec(spec) {
				return LockfileDelta{}, refuseLockfile(RebuildRefusalUnpinnedSource,
					"package.json declares %s as %s, which is not a registry version, range, or tag",
					quotedLockfileText(name), quotedLockfileText(spec))
			}
		}
	}
	// npm installs optional dependencies as dependencies, and its lockfile
	// root may record them under either name, so the two compare as one map.
	for _, side := range []map[string]map[string]string{declared, locked} {
		maps.Copy(side["dependencies"], side["optionalDependencies"])
		delete(side, "optionalDependencies")
	}
	for _, field := range slices.Sorted(maps.Keys(declared)) {
		if !maps.Equal(declared[field], locked[field]) {
			return inconsistent(
				"package-lock.json was not generated from this package.json: their %s differ", field)
		}
	}

	var delta LockfileDelta
	unchanged := func(key string) bool {
		previous, existed := base.packages[key]
		current, kept := head.packages[key]
		return existed && kept && sameJSON(previous, current)
	}
	for key := range base.packages {
		if _, kept := head.packages[key]; !kept {
			delta.Removed++
		}
	}
	hosts := map[string]bool{}
	// Sorted so the first refusal, and so the block a person reads, is the
	// same on every pass.
	for _, key := range slices.Sorted(maps.Keys(head.packages)) {
		if !unchanged(key) {
			delta.Changed++
			host, refusal := examineChangedPackage(key, head.packages[key], registries)
			if refusal != nil {
				return LockfileDelta{}, refusal
			}
			hosts[host] = true
			continue
		}
		entry, err := decodeObject(head.packages[key])
		if err != nil {
			return inconsistent("package-lock.json package %s %v", quotedLockfileText(key), err)
		}
		if _, sourced := entry["resolved"]; sourced || string(entry["inBundle"]) != "true" {
			continue
		}
		// A bundled entry ships inside the enclosing package's tarball. With
		// that package dropped or replaced nothing ships it, and npm fetches
		// it by name from the default registry, unpinned. An enclosing package
		// that is itself bundled is held to this same rule in its own turn.
		if enclosing, nested := enclosingPackage(key); !nested || !unchanged(enclosing) {
			return LockfileDelta{}, refuseLockfile(RebuildRefusalUnpinnedSource,
				"bundled package %s no longer ships inside an unchanged package", quotedLockfileText(key))
		}
	}
	if refusal := checkDependencyEdges(base, head, manifest, declared, unchanged); refusal != nil {
		return LockfileDelta{}, refusal
	}
	delta.Hosts = slices.Sorted(maps.Keys(hosts))
	return delta, nil
}

// checkDependencyEdges refuses a head lockfile in which a dependency has no
// entry to resolve to, or a peer dependency resolves to an entry nested under
// the package that declares it. npm treats both as unresolved, whatever the
// entries pin: it asks the default registry for the package, and from there
// follows that registry's metadata and not the lockfile.
//
// The root and every added or changed entry are held to it in full. An entry
// the base holds byte for byte is held only to what the candidate could have
// changed around it: a dependency the base resolved must still resolve, and a
// nested peer must be the base's own.
func checkDependencyEdges(
	base, head parsedLockfile,
	manifest map[string]json.RawMessage,
	declared map[string]map[string]string,
	unchanged func(string) bool,
) *lockfileRefusal {
	missing := func(from, name string) *lockfileRefusal {
		return refuseLockfile(RebuildRefusalLockfileInconsistent,
			"package-lock.json holds no package for %s, which %s depends on",
			quotedLockfileText(name), from)
	}
	// declared holds the root's optional dependencies among its dependencies.
	rootOptionalPeers := optionalPeers(manifest)
	for _, field := range []string{"dependencies", "devDependencies", "peerDependencies"} {
		for _, name := range slices.Sorted(maps.Keys(declared[field])) {
			if field == "peerDependencies" && rootOptionalPeers[name] {
				continue
			}
			if _, held := resolveDependency(head.packages, "", name); !held {
				return missing("package.json", name)
			}
		}
	}
	for _, key := range slices.Sorted(maps.Keys(head.packages)) {
		entry, err := decodeObject(head.packages[key])
		if err != nil {
			return refuseLockfile(RebuildRefusalLockfileInconsistent,
				"package-lock.json package %s %v", quotedLockfileText(key), err)
		}
		kept := unchanged(key)
		optional := optionalPeers(entry)
		// A dependency's own development dependencies are not installed.
		for _, field := range []string{"dependencies", "optionalDependencies", "peerDependencies"} {
			specs, err := dependencySpecs(entry, field)
			if err != nil {
				if kept {
					// The base's own shape, which npm read the same way then.
					continue
				}
				return refuseLockfile(RebuildRefusalLockfileInconsistent,
					"package-lock.json package %s %v", quotedLockfileText(key), err)
			}
			for _, name := range slices.Sorted(maps.Keys(specs)) {
				target, held := resolveDependency(head.packages, key, name)
				if !held {
					_, heldBefore := resolveDependency(base.packages, key, name)
					if kept && !heldBefore {
						continue
					}
					if !kept && field == "peerDependencies" && optional[name] {
						continue
					}
					return missing("package "+quotedLockfileText(key), name)
				}
				// A nested peer the base already holds is the image a person
				// approved; any other is the candidate's.
				if field == "peerDependencies" && target == key+"/node_modules/"+name &&
					(!kept || !unchanged(target)) {
					return refuseLockfile(RebuildRefusalLockfileInconsistent,
						"package %s has its peer dependency %s nested under it, where npm does not accept a peer",
						quotedLockfileText(key), quotedLockfileText(name))
				}
			}
		}
	}
	return nil
}
