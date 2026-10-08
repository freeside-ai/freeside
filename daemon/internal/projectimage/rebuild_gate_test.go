package projectimage

import (
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

// gateIntegrity is a well-formed sha512 SRI value. The gate checks the shape
// of a pin, never a tarball.
var gateIntegrity = "sha512-" + strings.Repeat("A", 86) + "=="

// gatePinned is a lock entry pinned to resolved, with any further members
// given as raw JSON.
func gatePinned(version, resolved string, extra ...string) string {
	entry := `{"version":"` + version + `","resolved":"` + resolved + `","integrity":"` + gateIntegrity + `"`
	for _, member := range extra {
		entry += "," + member
	}
	return entry + "}"
}

// gateLock is a v3 lockfile with the given root entry and package members,
// each written as raw `"key":{...}` JSON.
func gateLock(root string, members ...string) string {
	return `{"lockfileVersion":3,"packages":{"":` + root + "," + strings.Join(members, ",") + `}}`
}

const (
	gateBasePackageJSON = `{"name":"app","dependencies":{"left":"^1.0.0"}}`
	gateHeadPackageJSON = `{"name":"app","dependencies":{"left":"^1.0.0","right":"^2.0.0"}}`
	gateBundledInner    = `"node_modules/left/node_modules/inner":{"version":"1.0.0","inBundle":true}`
)

var (
	gateLeftMember      = `"node_modules/left":` + gatePinned("1.0.0", "https://registry.npmjs.org/left/-/left-1.0.0.tgz")
	gateBasePackageLock = gateLock(gateBasePackageJSON, gateLeftMember)
	gateDeclaredEntry   = gatePinned("2.0.0", "https://registry.npmjs.org/right/-/right-2.0.0.tgz")
)

// gateLeftWithPeer is the base's package with a peer dependency, and
// gateNestedInner an entry for that peer nested under it.
var (
	gateLeftWithPeer = `"node_modules/left":` + gatePinned("1.0.0",
		"https://registry.npmjs.org/left/-/left-1.0.0.tgz", `"peerDependencies":{"inner":"*"}`)
	gateNestedInner = `"node_modules/left/node_modules/inner":` +
		gatePinned("1.0.0", "https://registry.npmjs.org/inner/-/inner-1.0.0.tgz")
)

// gateDeclaredWith is the passing added entry with further raw members.
func gateDeclaredWith(extra ...string) string {
	return gatePinned("2.0.0", "https://registry.npmjs.org/right/-/right-2.0.0.tgz", extra...)
}

// gateResolvedFrom is the passing added entry resolved from another URL.
func gateResolvedFrom(resolved string) string {
	return gatePinned("2.0.0", resolved)
}

// gateHeadLock is a head lockfile that keeps the base's package and adds one
// entry, written as the raw JSON of that entry.
func gateHeadLock(added string, members ...string) string {
	return gateLock(gateHeadPackageJSON,
		append([]string{gateLeftMember, `"node_modules/right":` + added}, members...)...)
}

func gateRegistrySet(t *testing.T, hosts ...string) domain.RegistrySet {
	t.Helper()
	set, err := domain.NewRegistrySet(hosts)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func TestLockfileDeltaAdmitsOnlyPinnedPackagesFromDeclaredHosts(t *testing.T) {
	registries := gateRegistrySet(t, "registry.npmjs.org")
	const overridden = `{"name":"app","overrides":{"left":"1.0.0"},"dependencies":{"left":"^1.0.0"`
	for _, test := range []struct {
		name string
		// Each field defaults to the passing fixture.
		basePackageJSON, basePackageLock, headPackageJSON, headPackageLock string
		want                                                               RebuildRefusal
	}{
		{name: "one package from a declared host"},
		{
			name: "entry with registry dependencies and inert metadata",
			headPackageLock: gateHeadLock(gateDeclaredWith(
				`"dependencies":{"left":"^1.0.0 || >=3"}`, `"optionalDependencies":{"left":"latest"}`,
				`"peerDependencies":{"left":"*","absent":"*"}`,
				`"peerDependenciesMeta":{"absent":{"optional":true}}`, `"engines":{"node":">=18"}`,
				`"dev":true`, `"hasInstallScript":true`, `"license":"MIT"`, `"inBundle":false`)),
		},
		{
			// npm does not accept it either, but the base's image was built so.
			name:            "peer nested under its dependent in the base already",
			basePackageLock: gateLock(gateBasePackageJSON, gateLeftWithPeer, gateNestedInner),
			headPackageLock: gateLock(gateHeadPackageJSON, gateLeftWithPeer, gateNestedInner,
				`"node_modules/right":`+gateDeclaredEntry),
		},
		{
			name:            "bundled package that still ships inside its unchanged package",
			basePackageLock: gateLock(gateBasePackageJSON, gateLeftMember, gateBundledInner),
			headPackageLock: gateHeadLock(gateDeclaredEntry, gateBundledInner),
		},
		{
			name:            "overrides the base already holds",
			basePackageJSON: overridden + `}}`,
			headPackageJSON: `{"name":"app","overrides": { "left": "1.0.0" },"dependencies":{"left":"^1.0.0","right":"^2.0.0"}}`,
		},
		{
			// A source a person approved with the base is not re-examined.
			name:            "non-registry dependency the base already declares",
			basePackageJSON: `{"name":"app","dependencies":{"left":"github:owner/left#v1"}}`,
			headPackageJSON: `{"name":"app","dependencies":{"left":"github:owner/left#v1","right":"^2.0.0"}}`,
			headPackageLock: gateLock(
				`{"name":"app","dependencies":{"left":"github:owner/left#v1","right":"^2.0.0"}}`,
				gateLeftMember, `"node_modules/right":`+gateDeclaredEntry),
		},

		{
			name:            "undeclared host",
			headPackageLock: gateHeadLock(gateResolvedFrom("https://registry.example.com/right.tgz")),
			want:            RebuildRefusalUndeclaredAuthority,
		},
		{
			name:            "declared host on another port",
			headPackageLock: gateHeadLock(gateResolvedFrom("https://registry.npmjs.org:8443/right.tgz")),
			want:            RebuildRefusalUndeclaredAuthority,
		},
		{
			name:            "declared host in another case",
			headPackageLock: gateHeadLock(gateResolvedFrom("https://Registry.npmjs.org/right.tgz")),
			want:            RebuildRefusalUndeclaredAuthority,
		},
		{
			name:            "declared host as a subdomain label",
			headPackageLock: gateHeadLock(gateResolvedFrom("https://registry.npmjs.org.example.com/right.tgz")),
			want:            RebuildRefusalUndeclaredAuthority,
		},
		{
			name: "existing package repinned to an undeclared host",
			headPackageLock: gateLock(gateHeadPackageJSON,
				`"node_modules/left":`+gatePinned("1.0.0", "https://registry.example.com/left.tgz"),
				`"node_modules/right":`+gateDeclaredEntry),
			want: RebuildRefusalUndeclaredAuthority,
		},
		{
			// A WHATWG parser reads the backslash as a slash and the host as
			// the declared one; Go reads userinfo. Refused either way.
			name:            "declared host before a backslash and an at sign",
			headPackageLock: gateHeadLock(gateResolvedFrom(`https://registry.npmjs.org\\@registry.example.com/right.tgz`)),
			want:            RebuildRefusalUnpinnedSource,
		},
		{
			name:            "userinfo before a declared host",
			headPackageLock: gateHeadLock(gateResolvedFrom("https://user@registry.npmjs.org/right.tgz")),
			want:            RebuildRefusalUnpinnedSource,
		},
		{
			name:            "git source",
			headPackageLock: gateHeadLock(gateResolvedFrom("git+https://registry.npmjs.org/right.git#abc")),
			want:            RebuildRefusalUnpinnedSource,
		},
		{
			name:            "file source",
			headPackageLock: gateHeadLock(gateResolvedFrom("file:../right")),
			want:            RebuildRefusalUnpinnedSource,
		},
		{
			name:            "plain http",
			headPackageLock: gateHeadLock(gateResolvedFrom("http://registry.npmjs.org/right.tgz")),
			want:            RebuildRefusalUnpinnedSource,
		},
		{
			name:            "missing integrity",
			headPackageLock: gateHeadLock(`{"version":"2.0.0","resolved":"https://registry.npmjs.org/right.tgz"}`),
			want:            RebuildRefusalUnpinnedSource,
		},
		{
			// npm verifies against the strongest hash it recognizes, and
			// against nothing when it recognizes none.
			name:            "integrity npm does not recognize",
			headPackageLock: gateHeadLock(`{"version":"2.0.0","resolved":"https://registry.npmjs.org/right.tgz","integrity":"x"}`),
			want:            RebuildRefusalUnpinnedSource,
		},
		{
			name:            "sha1 integrity",
			headPackageLock: gateHeadLock(`{"version":"2.0.0","resolved":"https://registry.npmjs.org/right.tgz","integrity":"sha1-2jmj7l5rSw0yVb/vlWAYkK/YBwk="}`),
			want:            RebuildRefusalUnpinnedSource,
		},
		{
			name: "two integrity values",
			headPackageLock: gateHeadLock(`{"version":"2.0.0","resolved":"https://registry.npmjs.org/right.tgz","integrity":"` +
				gateIntegrity + ` sha1-2jmj7l5rSw0yVb/vlWAYkK/YBwk="}`),
			want: RebuildRefusalUnpinnedSource,
		},
		{
			// Without a resolved URL npm fetches the entry by name and version
			// from the default registry.
			name:            "missing resolved",
			headPackageLock: gateHeadLock(`{"version":"2.0.0","integrity":"` + gateIntegrity + `"}`),
			want:            RebuildRefusalUnpinnedSource,
		},
		{
			name:            "missing version",
			headPackageLock: gateHeadLock(`{"resolved":"https://registry.npmjs.org/right.tgz","integrity":"` + gateIntegrity + `"}`),
			want:            RebuildRefusalUnpinnedSource,
		},
		{
			name:            "version that is a URL",
			headPackageLock: gateHeadLock(gatePinned("https://registry.example.com/right.tgz", "https://registry.npmjs.org/right.tgz")),
			want:            RebuildRefusalUnpinnedSource,
		},
		{
			name:            "version that is a range",
			headPackageLock: gateHeadLock(gatePinned("^2.0.0", "https://registry.npmjs.org/right.tgz")),
			want:            RebuildRefusalUnpinnedSource,
		},
		{
			name:            "link",
			headPackageLock: gateHeadLock(gateDeclaredWith(`"link":true`)),
			want:            RebuildRefusalUnpinnedSource,
		},
		{
			// npm fetches a bundled entry no tarball ships by name and version.
			name:            "added bundled package",
			headPackageLock: gateHeadLock(`{"version":"2.0.0","inBundle":true}`),
			want:            RebuildRefusalUnpinnedSource,
		},
		{
			// With no resolved URL, npm fetches a version that is a URL.
			name:            "added bundled package whose version is a URL",
			headPackageLock: gateHeadLock(`{"version":"https://registry.example.com/right.tgz","inBundle":true}`),
			want:            RebuildRefusalUnpinnedSource,
		},
		{
			name:            "added bundled package with a declared pin",
			headPackageLock: gateHeadLock(gateDeclaredWith(`"inBundle":true`)),
			want:            RebuildRefusalUnpinnedSource,
		},
		{
			name:            "bundled package of the base with its version rewritten",
			basePackageLock: gateLock(gateBasePackageJSON, gateLeftMember, gateBundledInner),
			headPackageLock: gateHeadLock(gateDeclaredEntry,
				`"node_modules/left/node_modules/inner":{"version":"https://registry.example.com/inner.tgz","inBundle":true}`),
			want: RebuildRefusalUnpinnedSource,
		},
		{
			// The entry is byte for byte the base's, but the package that
			// shipped it is replaced, so nothing ships it any more.
			name:            "bundled package whose enclosing package changed",
			basePackageLock: gateLock(gateBasePackageJSON, gateLeftMember, gateBundledInner),
			headPackageLock: gateLock(gateHeadPackageJSON,
				`"node_modules/left":`+gatePinned("1.0.1", "https://registry.npmjs.org/left/-/left-1.0.1.tgz"),
				`"node_modules/right":`+gateDeclaredEntry, gateBundledInner),
			want: RebuildRefusalUnpinnedSource,
		},
		{
			name: "bundled package with no enclosing package",
			basePackageLock: gateLock(gateBasePackageJSON, gateLeftMember,
				`"node_modules/inner":{"version":"1.0.0","inBundle":true}`),
			headPackageLock: gateHeadLock(gateDeclaredEntry, `"node_modules/inner":{"version":"1.0.0","inBundle":true}`),
			want:            RebuildRefusalUnpinnedSource,
		},
		{
			// A shrinkwrapped package hands resolution beneath it to a file
			// inside its own tarball.
			name:            "added package with its own shrinkwrap",
			headPackageLock: gateHeadLock(gateDeclaredWith(`"hasShrinkwrap":true`)),
			want:            RebuildRefusalUnpinnedSource,
		},
		{
			name: "existing package newly marked as shrinkwrapped",
			headPackageLock: gateLock(gateHeadPackageJSON,
				`"node_modules/left":`+gatePinned("1.0.0", "https://registry.npmjs.org/left/-/left-1.0.0.tgz", `"hasShrinkwrap":true`),
				`"node_modules/right":`+gateDeclaredEntry),
			want: RebuildRefusalUnpinnedSource,
		},
		{
			// The name an entry is fetched under when it differs from its key.
			name:            "entry fetched under another name",
			headPackageLock: gateHeadLock(gateDeclaredWith(`"name":"other"`)),
			want:            RebuildRefusalUnpinnedSource,
		},
		{
			name:            "entry that bundles dependencies",
			headPackageLock: gateHeadLock(gateDeclaredWith(`"bundleDependencies":["inner"]`)),
			want:            RebuildRefusalUnpinnedSource,
		},
		{
			name:            "field the gate does not know",
			headPackageLock: gateHeadLock(gateDeclaredWith(`"resolvedFrom":"https://registry.example.com/right.tgz"`)),
			want:            RebuildRefusalUnpinnedSource,
		},
		{
			// encoding/json would match "Resolved" to a struct field named
			// for "resolved"; npm reads only the exact key. The shared
			// repeated-key check refuses the pair before any field is read.
			name:            "field that differs from a known one only by case",
			headPackageLock: gateHeadLock(gateDeclaredWith(`"Resolved":"https://registry.example.com/right.tgz"`)),
			want:            RebuildRefusalLockfileInconsistent,
		},
		{
			// npm satisfies a URL dependency only from that URL, whatever the
			// entry it reaches is pinned to.
			name:            "entry that depends on a URL",
			headPackageLock: gateHeadLock(gateDeclaredWith(`"dependencies":{"inner":"https://registry.example.com/inner.tgz"}`)),
			want:            RebuildRefusalUnpinnedSource,
		},
		{
			name:            "entry with a peer dependency on a repository",
			headPackageLock: gateHeadLock(gateDeclaredWith(`"peerDependencies":{"inner":"owner/inner"}`)),
			want:            RebuildRefusalUnpinnedSource,
		},
		{
			name:            "entry with an aliased dependency",
			headPackageLock: gateHeadLock(gateDeclaredWith(`"optionalDependencies":{"inner":"npm:other@^1.0.0"}`)),
			want:            RebuildRefusalUnpinnedSource,
		},
		{
			name:            "entry whose dependency name is a source",
			headPackageLock: gateHeadLock(gateDeclaredWith(`"dependencies":{"https://registry.example.com/inner.tgz#":"1"}`)),
			want:            RebuildRefusalUnpinnedSource,
		},
		{
			name:            "entry whose dependencies are not a map",
			headPackageLock: gateHeadLock(gateDeclaredWith(`"dependencies":["inner"]`)),
			want:            RebuildRefusalLockfileInconsistent,
		},
		{
			// npm asks the default registry for a dependency the lockfile does
			// not hold, and follows what that registry's metadata names.
			name:            "dependency with no lock entry",
			headPackageLock: gateHeadLock(gateDeclaredWith(`"dependencies":{"inner":"*"}`)),
			want:            RebuildRefusalLockfileInconsistent,
		},
		{
			name:            "optional dependency with no lock entry",
			headPackageLock: gateHeadLock(gateDeclaredWith(`"optionalDependencies":{"inner":"*"}`)),
			want:            RebuildRefusalLockfileInconsistent,
		},
		{
			name:            "required peer dependency with no lock entry",
			headPackageLock: gateHeadLock(gateDeclaredWith(`"peerDependencies":{"inner":"*"}`)),
			want:            RebuildRefusalLockfileInconsistent,
		},
		{
			name:            "root dependency with no lock entry",
			headPackageJSON: `{"name":"app","dependencies":{"left":"^1.0.0","right":"^2.0.0","ghost":"1"}}`,
			headPackageLock: gateLock(`{"name":"app","dependencies":{"left":"^1.0.0","right":"^2.0.0","ghost":"1"}}`,
				gateLeftMember, `"node_modules/right":`+gateDeclaredEntry),
			want: RebuildRefusalLockfileInconsistent,
		},
		{
			// npm rejects a peer that is a child of its dependent whatever its
			// version, re-resolves it from the registry, and from there follows
			// the registry's metadata and not the lockfile.
			name: "peer dependency nested under its dependent",
			headPackageLock: gateHeadLock(gateDeclaredWith(`"peerDependencies":{"inner":"*"}`),
				`"node_modules/right/node_modules/inner":`+gatePinned("1.0.0", "https://registry.npmjs.org/inner/-/inner-1.0.0.tgz"),
				`"node_modules/inner":`+gatePinned("1.0.0", "https://registry.npmjs.org/inner/-/inner-1.0.0.tgz")),
			want: RebuildRefusalLockfileInconsistent,
		},
		{
			name: "peer nested under an unchanged dependent by the candidate",
			basePackageLock: gateLock(gateBasePackageJSON, gateLeftWithPeer,
				`"node_modules/inner":`+gatePinned("1.0.0", "https://registry.npmjs.org/inner/-/inner-1.0.0.tgz")),
			headPackageLock: gateLock(gateHeadPackageJSON, gateLeftWithPeer, gateNestedInner,
				`"node_modules/inner":`+gatePinned("1.0.0", "https://registry.npmjs.org/inner/-/inner-1.0.0.tgz"),
				`"node_modules/right":`+gateDeclaredEntry),
			want: RebuildRefusalLockfileInconsistent,
		},
		{
			// The entry is the base's, but the candidate dropped what it needs.
			name: "unchanged package whose dependency lost its entry",
			basePackageLock: gateLock(gateBasePackageJSON, gateLeftWithPeer,
				`"node_modules/inner":`+gatePinned("1.0.0", "https://registry.npmjs.org/inner/-/inner-1.0.0.tgz")),
			headPackageLock: gateLock(gateHeadPackageJSON, gateLeftWithPeer, `"node_modules/right":`+gateDeclaredEntry),
			want:            RebuildRefusalLockfileInconsistent,
		},
		{
			// npm splices the key's tail into the spec it fetches.
			name:            "key that is a URL",
			headPackageLock: gateHeadLock(gateDeclaredEntry, `"node_modules/https:registry.example.com#":`+gateDeclaredEntry),
			want:            RebuildRefusalUnpinnedSource,
		},
		{
			name:            "key with a spec after the package name",
			headPackageLock: gateHeadLock(gateDeclaredEntry, `"node_modules/@a/b@..":`+gateDeclaredEntry),
			want:            RebuildRefusalUnpinnedSource,
		},
		{
			name:            "key that leaves node_modules",
			headPackageLock: gateHeadLock(gateDeclaredEntry, `"node_modules/../right":`+gateDeclaredEntry),
			want:            RebuildRefusalUnpinnedSource,
		},
		{
			name:            "key outside node_modules",
			headPackageLock: gateHeadLock(gateDeclaredEntry, `"packages/member":`+gateDeclaredEntry),
			want:            RebuildRefusalUnpinnedSource,
		},
		{
			name:            "repeated package key",
			headPackageLock: gateHeadLock(gateDeclaredEntry, `"node_modules/right":`+gateResolvedFrom("https://registry.example.com/right.tgz")),
			want:            RebuildRefusalLockfileInconsistent,
		},
		{
			// JavaScript keeps an unpaired surrogate and Go decodes it as
			// U+FFFD, so npm sees two keys where Go would see one.
			name: "package keys only one parser tells apart",
			headPackageLock: gateHeadLock(gateDeclaredEntry,
				`"node_modules/\ud800":`+gateDeclaredEntry, `"node_modules/\ufffd":`+gateDeclaredEntry),
			want: RebuildRefusalLockfileInconsistent,
		},
		{
			// A repeated key keeps its last value in JSON.parse.
			name:            "repeated field",
			headPackageLock: gateHeadLock(gateDeclaredWith(`"resolved":"https://registry.example.com/right.tgz"`)),
			want:            RebuildRefusalLockfileInconsistent,
		},
		{
			name:            "null resolved",
			headPackageLock: gateHeadLock(`{"version":"2.0.0","resolved":null,"integrity":"` + gateIntegrity + `"}`),
			want:            RebuildRefusalLockfileInconsistent,
		},
		{
			name:            "lockfile root disagrees with package.json",
			headPackageJSON: `{"dependencies":{"left":"^1.0.0","right":"^3.0.0"}}`,
			want:            RebuildRefusalLockfileInconsistent,
		},
		{
			name:            "dependency declared only in package.json",
			headPackageJSON: `{"dependencies":{"left":"^1.0.0","right":"^2.0.0"},"devDependencies":{"extra":"1"}}`,
			want:            RebuildRefusalLockfileInconsistent,
		},
		{
			name:            "peer dependency declared only in package.json",
			headPackageJSON: `{"dependencies":{"left":"^1.0.0","right":"^2.0.0"},"peerDependencies":{"extra":"1"}}`,
			want:            RebuildRefusalLockfileInconsistent,
		},
		{
			name:            "overrides added",
			headPackageJSON: overridden + `,"right":"^2.0.0"}}`,
			want:            RebuildRefusalUnsupportedInput,
		},
		{
			name:            "overrides changed",
			basePackageJSON: overridden + `}}`,
			headPackageJSON: `{"name":"app","overrides":{"left":"https://registry.example.com/left.tgz"},"dependencies":{"left":"^1.0.0","right":"^2.0.0"}}`,
			want:            RebuildRefusalUnsupportedInput,
		},
		{
			name:            "overrides removed",
			basePackageJSON: overridden + `}}`,
			want:            RebuildRefusalUnsupportedInput,
		},
		{
			name:            "workspaces added",
			headPackageJSON: `{"name":"app","workspaces":["packages/*"],"dependencies":{"left":"^1.0.0","right":"^2.0.0"}}`,
			want:            RebuildRefusalUnsupportedInput,
		},
		{
			name:            "package manager added",
			headPackageJSON: `{"name":"app","packageManager":"npm@https://registry.example.com/npm.tgz","dependencies":{"left":"^1.0.0","right":"^2.0.0"}}`,
			want:            RebuildRefusalUnsupportedInput,
		},
		{
			name:            "lockfile version 1",
			headPackageLock: `{"lockfileVersion":1,"dependencies":{}}`,
			want:            RebuildRefusalLockfileInconsistent,
		},
		{
			name:            "lockfile version as a string",
			headPackageLock: strings.Replace(gateHeadLock(gateDeclaredEntry), `"lockfileVersion":3`, `"lockfileVersion":"3"`, 1),
			want:            RebuildRefusalLockfileInconsistent,
		},
		{
			name:            "lockfile without a root entry",
			headPackageLock: `{"lockfileVersion":3,"packages":{"node_modules/right":` + gateDeclaredEntry + `}}`,
			want:            RebuildRefusalLockfileInconsistent,
		},
		{
			name:            "lockfile that is not JSON",
			headPackageLock: `{"lockfileVersion":3,`,
			want:            RebuildRefusalLockfileInconsistent,
		},
		{
			name:            "lockfile with content after its object",
			headPackageLock: gateHeadLock(gateDeclaredEntry) + `{}`,
			want:            RebuildRefusalLockfileInconsistent,
		},
		{
			name:            "absent head lockfile",
			headPackageLock: "absent",
			want:            RebuildRefusalLockfileInconsistent,
		},
		{
			name:            "unreadable base lockfile",
			basePackageLock: `{"lockfileVersion":1}`,
			want:            RebuildRefusalLockfileInconsistent,
		},
		{
			name:            "absent base package.json",
			basePackageJSON: "absent",
			want:            RebuildRefusalLockfileInconsistent,
		},
		{
			name:            "package.json that is not an object",
			headPackageJSON: `[]`,
			want:            RebuildRefusalLockfileInconsistent,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			pick := func(value, fallback string) []byte {
				switch value {
				case "":
					return []byte(fallback)
				case "absent":
					return nil
				}
				return []byte(value)
			}
			delta, refusal := evaluateLockfileDelta(
				pick(test.basePackageJSON, gateBasePackageJSON),
				pick(test.basePackageLock, gateBasePackageLock),
				pick(test.headPackageJSON, gateHeadPackageJSON),
				pick(test.headPackageLock, gateHeadLock(gateDeclaredEntry)),
				registries,
			)
			if test.want == "" {
				if refusal != nil {
					t.Fatalf("refused under %s: %s", refusal.clause, refusal.detail)
				}
				if delta.Changed != 1 || delta.Removed != 0 {
					t.Fatalf("delta = %+v, want one changed package", delta)
				}
				return
			}
			if refusal == nil {
				t.Fatalf("admitted with delta %+v, want refusal %s", delta, test.want)
			}
			if refusal.clause != test.want || refusal.detail == "" {
				t.Fatalf("refusal = %s (%s), want %s", refusal.clause, refusal.detail, test.want)
			}
		})
	}
}

// npm trusts a lock entry only while the dependency that reaches it is
// satisfied by it, and a dependency declared by URL, repository, or path is
// satisfied only from that source. So a candidate that declares one makes npm
// fetch from it whatever the entry is pinned to.
func TestLockfileDeltaAdmitsOnlyRegistrySpecsInWhatTheCandidateDeclares(t *testing.T) {
	registries := gateRegistrySet(t, "registry.npmjs.org")
	evaluate := func(t *testing.T, field, spec string) *lockfileRefusal {
		t.Helper()
		declared := map[string]map[string]string{"dependencies": {"left": "^1.0.0"}}
		if declared[field] == nil {
			declared[field] = map[string]string{}
		}
		declared[field]["right"] = spec
		manifest, err := json.Marshal(declared)
		if err != nil {
			t.Fatal(err)
		}
		// The lockfile root agrees with package.json and pins the package to a
		// declared host, as a lockfile npm wrote for a registry spec would.
		lock := gateLock(string(manifest), gateLeftMember, `"node_modules/right":`+gateDeclaredEntry)
		_, refusal := evaluateLockfileDelta(
			[]byte(gateBasePackageJSON), []byte(gateBasePackageLock), manifest, []byte(lock), registries)
		return refusal
	}
	for _, field := range dependencyFields {
		for _, spec := range []string{"^2.0.0", "2.0.0", "", "*", "latest", ">=2.0.0 <3.0.0", "1.x || 2.x", "~2.0.0-rc.1"} {
			if refusal := evaluate(t, field, spec); refusal != nil {
				t.Errorf("%s %q refused under %s: %s", field, spec, refusal.clause, refusal.detail)
			}
		}
		for _, spec := range []string{
			"https://registry.example.com/right.tgz",
			"http://registry.example.com/right.tgz",
			"git+https://github.com/owner/right.git#abc",
			"git://github.com/owner/right.git",
			"github:owner/right",
			"owner/right",
			"file:../right",
			"./right",
			"../right",
			"/right",
			"~/right",
			"right.tgz",
			"right.tar.gz",
			// npm's file test leaves its dot unescaped, so this is a file too.
			"right.tar-gz",
			"link:../right",
			"workspace:*",
			"npm:other@^2.0.0",
			"npm:other@https://registry.example.com/right.tgz",
			"2.0.0\n",
		} {
			refusal := evaluate(t, field, spec)
			if refusal == nil || refusal.clause != RebuildRefusalUnpinnedSource {
				t.Errorf("%s %q: refusal = %+v, want %s", field, spec, refusal, RebuildRefusalUnpinnedSource)
			}
		}
	}
}

func TestLockfileDeltaSummarizesTheChange(t *testing.T) {
	// The head drops the base's package and adds two from two declared hosts.
	head := `{"lockfileVersion":2,"packages":{` +
		`"":{"dependencies":{"right":"^2.0.0"},"optionalDependencies":{"@scope/other":"1"}},` +
		`"node_modules/right":` + gateDeclaredEntry + `,` +
		`"node_modules/@scope/other":` + gatePinned("1.2.3", "https://npm.pkg.github.com/other.tgz") + `}}`
	// npm may record an optional dependency under either name at the root.
	manifest := `{"dependencies":{"right":"^2.0.0","@scope/other":"1"}}`
	delta, refusal := evaluateLockfileDelta(
		[]byte(gateBasePackageJSON), []byte(gateBasePackageLock), []byte(manifest), []byte(head),
		gateRegistrySet(t, "npm.pkg.github.com", "registry.npmjs.org"),
	)
	if refusal != nil {
		t.Fatalf("refused under %s: %s", refusal.clause, refusal.detail)
	}
	want := LockfileDelta{Changed: 2, Removed: 1, Hosts: []string{"npm.pkg.github.com", "registry.npmjs.org"}}
	if delta.Changed != want.Changed || delta.Removed != want.Removed || !slices.Equal(delta.Hosts, want.Hosts) {
		t.Fatalf("delta = %+v, want %+v", delta, want)
	}
}

// npm reads a URL on a git forge it knows as a repository and clones it with
// no integrity check, so declaring such a host does not make it a registry.
func TestLockfileDeltaRefusesARegistryOnAGitForgeHost(t *testing.T) {
	registries := gateRegistrySet(t, "gitlab.com", "www.github.com")
	for _, resolved := range []string{"https://gitlab.com/owner/right", "https://www.github.com/owner/right.tgz"} {
		_, refusal := evaluateLockfileDelta(
			[]byte(gateBasePackageJSON), []byte(gateBasePackageLock), []byte(gateHeadPackageJSON),
			[]byte(gateHeadLock(gateResolvedFrom(resolved))), registries)
		if refusal == nil || refusal.clause != RebuildRefusalUnpinnedSource {
			t.Errorf("%s: refusal = %+v, want %s", resolved, refusal, RebuildRefusalUnpinnedSource)
		}
	}
}

func TestLockfileRefusalBoundsCandidateText(t *testing.T) {
	long := strings.Repeat("a", 4096)
	for name, lock := range map[string]string{
		"host":  gateHeadLock(gateResolvedFrom("https://" + long + ".example.com/right.tgz")),
		"key":   gateHeadLock(gateDeclaredEntry, `"node_modules/`+long+`:":`+gateDeclaredEntry),
		"field": gateHeadLock(gateDeclaredWith(`"` + long + `":1`)),
		"spec":  gateHeadLock(gateDeclaredWith(`"dependencies":{"inner":"` + long + `:"}`)),
	} {
		_, refusal := evaluateLockfileDelta(
			[]byte(gateBasePackageJSON), []byte(gateBasePackageLock), []byte(gateHeadPackageJSON),
			[]byte(lock), gateRegistrySet(t, "registry.npmjs.org"),
		)
		if refusal == nil || len(refusal.detail) > 1000 {
			t.Fatalf("%s: refusal = %+v, want a bounded detail", name, refusal)
		}
	}
}

func TestRebuildRefusalSetIsClosed(t *testing.T) {
	seen := map[RebuildRefusal]bool{}
	for _, refusal := range AllRebuildRefusals {
		if !refusal.valid() || seen[refusal] {
			t.Fatalf("refusal %q is invalid or listed twice", refusal)
		}
		seen[refusal] = true
	}
	if RebuildRefusal("").valid() || RebuildRefusal("other").valid() {
		t.Fatal("a value outside the set is valid")
	}
}

func gatePolicy(t *testing.T, registrySet string) domain.ResolvedPolicy {
	t.Helper()
	if registrySet == "" {
		return domain.ResolvedPolicy{}
	}
	return domain.ResolvedPolicy{Keys: []domain.PolicyKey{
		{Key: domain.RegistrySetPolicyKey, Value: registrySet},
	}}
}

func TestEvaluateRebuildAppliesEachClauseAtTheExactCommits(t *testing.T) {
	const declared = `["registry.npmjs.org"]`
	repo := newBaseInputsRepo(t)
	base := repo.commit(map[string]string{
		"package.json": gateBasePackageJSON, "package-lock.json": gateBasePackageLock,
		verify.DefaultRecipePath: testRecipe, "src/index.js": "export {};\n",
	})
	image, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: "freeasinbird/gh-imgup", RepositoryID: 1278475858,
		CommitSHA: base, RecipeDigest: verify.RecipeDigest([]byte(testRecipe)),
		PreparationCommand: []string{PreparationPath},
		BaseImageRef:       validRequest().BaseImageRef,
		ImageRef:           domain.ImageRef("127.0.0.1:5100/project@" + testImageDigest),
		Environment: &domain.ProjectImageEnvironment{
			PackageJSONSHA256: manifestSHA256([]byte(gateBasePackageJSON)),
			PackageLockSHA256: manifestSHA256([]byte(gateBasePackageLock)),
			PreparationDigest: PreparationDigest(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	passing := map[string]string{
		"package.json": gateHeadPackageJSON, "package-lock.json": gateHeadLock(gateDeclaredEntry),
	}
	for _, test := range []struct {
		name    string
		changes map[string]string
		policy  string
		legacy  bool
		// needed is false for a head the gate does not apply to.
		needed bool
		want   RebuildRefusal
	}{
		{name: "source-only head", changes: map[string]string{"src/index.js": "export default 1;\n"}, policy: declared},
		{
			// With no recorded environment there is nothing to compare the
			// head with, so the image verifies it as it always did.
			name: "image without environment evidence", changes: passing, policy: declared, legacy: true,
		},
		{name: "declared dependency change", changes: passing, policy: declared, needed: true},
		{
			name: "undeclared host", policy: declared, needed: true,
			changes: map[string]string{
				"package.json":      gateHeadPackageJSON,
				"package-lock.json": gateHeadLock(gateResolvedFrom("https://registry.example.com/right.tgz")),
			},
			want: RebuildRefusalUndeclaredAuthority,
		},
		{name: "no registry set", changes: passing, needed: true, want: RebuildRefusalNoRegistrySet},
		{
			name: "malformed registry set", changes: passing, policy: `["Registry.npmjs.org"]`,
			needed: true, want: RebuildRefusalNoRegistrySet,
		},
		{
			name: "recipe changed", policy: declared, needed: true,
			changes: map[string]string{
				"package.json": gateHeadPackageJSON, "package-lock.json": gateHeadLock(gateDeclaredEntry),
				verify.DefaultRecipePath: `{"commands":[["true"]],"capture":"none"}`,
			},
			want: RebuildRefusalRecipeChanged,
		},
		{
			name: "recipe removed", policy: declared, needed: true,
			changes: map[string]string{
				"package.json": gateHeadPackageJSON, "package-lock.json": gateHeadLock(gateDeclaredEntry),
				verify.DefaultRecipePath: "",
			},
			want: RebuildRefusalRecipeChanged,
		},
		{
			name: "npmrc added", policy: declared, needed: true,
			changes: map[string]string{
				"package.json": gateHeadPackageJSON, "package-lock.json": gateHeadLock(gateDeclaredEntry),
				".npmrc": "registry=https://registry.example.com/\n",
			},
			want: RebuildRefusalUnsupportedInput,
		},
		{
			name: "shrinkwrap added", policy: declared, needed: true,
			changes: map[string]string{
				"package.json": gateHeadPackageJSON, "package-lock.json": gateHeadLock(gateDeclaredEntry),
				"npm-shrinkwrap.json": `{}`,
			},
			want: RebuildRefusalUnsupportedInput,
		},
		{
			name: "lockfile removed", policy: declared, needed: true,
			changes: map[string]string{"package.json": gateHeadPackageJSON, "package-lock.json": ""},
			want:    RebuildRefusalLockfileInconsistent,
		},
		{
			name: "package.json changed without its lockfile", policy: declared, needed: true,
			changes: map[string]string{"package.json": gateHeadPackageJSON},
			want:    RebuildRefusalLockfileInconsistent,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo.git("checkout", "-q", "--detach", base)
			head := repo.commit(test.changes)
			admitted := image
			if test.legacy {
				admitted.Environment = nil
			}
			decision, err := EvaluateRebuild(
				t.Context(), "git", repo.dir, admitted, base, head, gatePolicy(t, test.policy))
			if err != nil {
				t.Fatalf("EvaluateRebuild: %v", err)
			}
			if decision.Needed != test.needed || decision.Refusal != test.want ||
				decision.Head.CommitSHA != head {
				t.Fatalf("decision = %+v, want needed=%t refusal=%q", decision, test.needed, test.want)
			}
			if (decision.Refusal == "") != (decision.Detail == "") {
				t.Fatalf("decision = %+v: a refusal and its detail travel together", decision)
			}
			if test.needed && test.want == "" &&
				(decision.Delta.Changed != 1 || !slices.Equal(decision.Delta.Hosts, []string{"registry.npmjs.org"})) {
				t.Fatalf("delta = %+v, want one package from registry.npmjs.org", decision.Delta)
			}
		})
	}
}

func TestEvaluateRebuildRefusesAnUnreadableManifest(t *testing.T) {
	repo := newBaseInputsRepo(t)
	base := repo.commit(map[string]string{
		"package.json": gateBasePackageJSON, "package-lock.json": gateBasePackageLock,
	})
	repo.git("rm", "-q", "package-lock.json")
	repo.git("update-index", "--add", "--cacheinfo", "120000,"+
		repo.git("hash-object", "-w", "package.json")+",package-lock.json")
	repo.git("commit", "-q", "-m", "symlink")
	head := repo.git("rev-parse", "HEAD")
	image := domain.ProjectImage{Environment: &domain.ProjectImageEnvironment{
		PackageJSONSHA256: manifestSHA256([]byte(gateBasePackageJSON)),
		PackageLockSHA256: manifestSHA256([]byte(gateBasePackageLock)),
	}}
	decision, err := EvaluateRebuild(
		t.Context(), "git", repo.dir, image, base, head, gatePolicy(t, `["registry.npmjs.org"]`))
	if err != nil {
		t.Fatalf("EvaluateRebuild: %v", err)
	}
	if !decision.Needed || decision.Refusal != RebuildRefusalLockfileInconsistent {
		t.Fatalf("decision = %+v, want a lockfile refusal", decision)
	}
	// An image with no recorded environment takes no gate, readable or not.
	image.Environment = nil
	decision, err = EvaluateRebuild(
		t.Context(), "git", repo.dir, image, base, head, gatePolicy(t, `["registry.npmjs.org"]`))
	if err != nil || decision.Needed || decision.Head.CommitSHA != head {
		t.Fatalf("decision = %+v, %v; want no gate for an image without environment evidence", decision, err)
	}
}

func TestEvaluateRebuildReturnsACheckoutFaultAsAnError(t *testing.T) {
	repo := newBaseInputsRepo(t)
	base := repo.commit(map[string]string{"package.json": gateBasePackageJSON})
	_, err := EvaluateRebuild(t.Context(), "git", repo.dir, domain.ProjectImage{},
		base, strings.Repeat("0", 40), domain.ResolvedPolicy{})
	if err == nil {
		t.Fatal("a head the checkout does not hold produced a decision")
	}
}

func TestEvaluateRebuildPinsOnceAcrossBothCommitsAndClosesItsReader(t *testing.T) {
	repo := newBaseInputsRepo(t)
	base := repo.commit(map[string]string{
		"package.json": gateBasePackageJSON, "package-lock.json": gateBasePackageLock,
		verify.DefaultRecipePath: testRecipe,
	})
	head := repo.commit(map[string]string{
		"package.json": gateHeadPackageJSON, "package-lock.json": gateHeadLock(gateDeclaredEntry),
	})
	image := domain.ProjectImage{Environment: &domain.ProjectImageEnvironment{
		PackageJSONSHA256: manifestSHA256([]byte(gateBasePackageJSON)),
		PackageLockSHA256: manifestSHA256([]byte(gateBasePackageLock)),
	}}
	git, check := countedCommitGit(t)
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	decision, err := EvaluateRebuild(t.Context(), git, repo.dir, image, base, head, gatePolicy(t, `["registry.npmjs.org"]`))
	if err != nil || !decision.Needed || decision.Refusal != "" || decision.Delta.Changed != 1 {
		t.Fatalf("decision = %+v, %v; want one admitted dependency", decision, err)
	}
	check(14, 10)
	if _, err := EvaluateRebuild(t.Context(), "git", repo.dir, image, base, strings.Repeat("0", 40), domain.ResolvedPolicy{}); !errors.Is(err, verify.ErrGitPlumbing) {
		t.Fatalf("missing commit = %v, want plumbing fault", err)
	}
	entries, err := os.ReadDir(scratch)
	if err != nil || len(entries) != 0 {
		t.Fatalf("scratch after calls = %v, %v", entries, err)
	}
}

func rebuildTestImage(t *testing.T, ref string) domain.ProjectImage {
	t.Helper()
	image, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: "freeasinbird/gh-imgup", RepositoryID: 1278475858,
		CommitSHA: testCommit, RecipeDigest: verify.RecipeDigest([]byte(testRecipe)),
		PreparationCommand: []string{PreparationPath},
		BaseImageRef:       validRequest().BaseImageRef,
		ImageRef:           domain.ImageRef(ref),
	})
	if err != nil {
		t.Fatal(err)
	}
	return image
}

func TestRebuildRequestKeepsEverythingTheImageBinds(t *testing.T) {
	const head = "1111111111111111111111111111111111111111"
	inputs := BuildInputs{
		BaseBuildRef: "freeside-agent-claude:local",
		BuildProxy:   "http://192.168.64.1:3128", DNS: []string{"192.168.64.1"},
	}
	for _, test := range []struct {
		name, ref, registry string
		port                int
	}{
		{name: "local registry", ref: "127.0.0.1:5100/freeside-project-x@" + testImageDigest, port: 5100},
		{
			name: "remote registry", ref: "ghcr.io/freeside-ai/freeside-project-x@" + testImageDigest,
			registry: "ghcr.io/freeside-ai",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			image := rebuildTestImage(t, test.ref)
			request, err := RebuildRequest(image, head, "/checkout", []byte(testRecipe), inputs)
			if err != nil {
				t.Fatalf("RebuildRequest: %v", err)
			}
			if request.Repository != image.Repository || request.RepositoryID != image.RepositoryID ||
				request.CommitSHA != head || request.SourceDir != "/checkout" ||
				request.BaseImageRef != image.BaseImageRef ||
				request.Registry != test.registry || request.LocalRegistryPort != test.port ||
				request.ImageName != "freeside-project-x" || request.RefTag != "rebuild-111111111111" ||
				request.BaseBuildRef != inputs.BaseBuildRef || request.BuildProxy != inputs.BuildProxy ||
				!slices.Equal(request.DNS, inputs.DNS) {
				t.Fatalf("request = %+v", request)
			}
			if _, _, _, err := validateRequest(request); err != nil {
				t.Fatalf("derived request is not buildable: %v", err)
			}
		})
	}
}

func TestRebuildRequestRefusesInputsTheImageDoesNotBind(t *testing.T) {
	const head = "1111111111111111111111111111111111111111"
	image := rebuildTestImage(t, "127.0.0.1:5100/freeside-project-x@"+testImageDigest)
	inputs := BuildInputs{BaseBuildRef: "freeside-agent-claude:local"}
	for name, build := range map[string]func() (Request, error){
		"another recipe": func() (Request, error) {
			return RebuildRequest(image, head, "/checkout", []byte(`{"commands":[["true"]],"capture":"none"}`), inputs)
		},
		"abbreviated commit": func() (Request, error) {
			return RebuildRequest(image, head[:12], "/checkout", []byte(testRecipe), inputs)
		},
		"no checkout": func() (Request, error) {
			return RebuildRequest(image, head, "", []byte(testRecipe), inputs)
		},
		"no base build reference": func() (Request, error) {
			return RebuildRequest(image, head, "/checkout", []byte(testRecipe), BuildInputs{})
		},
		"reference without a registry": func() (Request, error) {
			return RebuildRequest(rebuildTestImage(t, "project@"+testImageDigest),
				head, "/checkout", []byte(testRecipe), inputs)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := build(); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("error = %v, want ErrInvalidRequest", err)
			}
		})
	}
}
