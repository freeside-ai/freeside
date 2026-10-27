package domain

// RebuildRefusal names the clause of the policy-gated rebuild (plan §5.7) that
// kept a dependency change on the fail-loud path. It lives in domain so the
// image-upkeep refusal record can name its clauses; projectimage aliases it.
// The zero value "" is invalid by design: a decision that holds carries no
// refusal.
type RebuildRefusal string

const (
	// RebuildRefusalUndeclaredAuthority: an added or changed package resolves
	// from a host outside the project policy's declared registry set.
	RebuildRefusalUndeclaredAuthority RebuildRefusal = "undeclared_authority"
	// RebuildRefusalUnpinnedSource: an added or changed package is not a
	// registry tarball pinned by an https URL and a sha512 integrity value (a
	// link, a workspace member, a VCS or file source, a bundled package, a
	// missing pin), or the change declares a dependency by anything but a
	// registry version, range, or tag. For a Go image: a module version
	// go.mod requires has no go.mod hash in go.sum.
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
	// RebuildRefusalGoDependencyChanged: the candidate holds a Go input no
	// project image supports (a vendor directory, a go.work, a replace with a
	// filesystem path), or a go.mod or go.sum no build reads: absent, not a
	// regular file, a go or toolchain directive the baked Go refuses, or a
	// line not of Go's own shape. Records written before Go module changes
	// were bounded also hold it for any go.mod or go.sum change.
	RebuildRefusalGoDependencyChanged RebuildRefusal = "go_dependency_changed"
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
	RebuildRefusalGoDependencyChanged,
}

func (r RebuildRefusal) valid() bool {
	switch r {
	case RebuildRefusalUndeclaredAuthority, RebuildRefusalUnpinnedSource,
		RebuildRefusalRecipeChanged, RebuildRefusalLockfileInconsistent,
		RebuildRefusalUnsupportedInput, RebuildRefusalNoRegistrySet,
		RebuildRefusalNotConfigured, RebuildRefusalProofFailed,
		RebuildRefusalGoDependencyChanged:
		return true
	default:
		return false
	}
}
