package importer

import (
	"sort"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/pathfold"
)

// detectCollisions flags candidate-introduced paths that would collide
// on a case-insensitive or Unicode-normalization-insensitive checkout
// (APFS, the reference deployment, is both). A collision means the
// built tree holds two distinct paths that the daemon's own working
// checkout, or a later verification or reviewer checkout, cannot keep
// apart, so which content wins is filesystem-defined rather than
// committed: publish-blocking, never silently resolved.
//
// Only a newly added regular-file path participates as the introducing
// side. A deletion removes a path, and a modification (content or a
// fromBase mode-only change) keeps a path that already existed in base,
// so neither can introduce a new fold-collision — any collision on a
// modified path pre-existed in base and is not the candidate's doing. A
// non-regular add (symlink, submodule, special, unusual mode) is already
// publish-blocking on its own class, so a collision finding on it would
// be redundant. The introducing add is tested against the full
// post-import path set — base paths the candidate leaves in place
// included — because a new file colliding with an untouched base entry
// is exactly the smuggle this catches, whether by identical folded name
// or by a file/directory fold conflict. The two members of each reported
// collision are the added path and the path it collides with.
func detectCollisions(changes []plannedChange, base map[string]treeEntry) []Finding {
	// Post-import path set: base minus deletions plus adds (modifies are
	// already in base; adds are not).
	deleted := make(map[string]bool)
	for _, c := range changes {
		if c.kind == ChangeDeleted {
			deleted[c.path] = true
		}
	}
	post := make([]string, 0, len(base)+len(changes))
	seen := make(map[string]bool, len(base)+len(changes))
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			post = append(post, p)
		}
	}
	for p := range base {
		if !deleted[p] {
			add(p)
		}
	}
	for _, c := range changes {
		if c.kind != ChangeDeleted {
			add(c.path)
		}
	}
	// Base is a map, so its iteration order is deliberately unstable.
	// Sort before retaining the first two owners for a fold, otherwise a
	// three-way collision can name a different partner on each import.
	sort.Strings(post)

	// Record only each post-import path's folded *full* path as a leaf,
	// keeping at most two distinct owners per fold (enough to always find
	// a partner that is not the queried path). Deliberately no ancestor
	// (directory) map: materializing every folded ancestor prefix costs
	// memory quadratic in a single path's length, a denial-of-service
	// vector at this untrusted boundary. The directory direction is
	// answered instead from a sorted leaf list, and the ancestor-is-file
	// direction by walking the queried add's own (length-capped)
	// ancestors — both bounded, neither retaining ancestor strings.
	fileOwner := make(map[string][]string, len(post))
	for _, p := range post {
		key := pathfold.FoldPath(p)
		lst := fileOwner[key]
		if len(lst) < 2 && (len(lst) == 0 || lst[0] != p) {
			fileOwner[key] = append(lst, p)
		}
	}
	sortedLeaves := make([]string, 0, len(fileOwner))
	for k := range fileOwner {
		sortedLeaves = append(sortedLeaves, k)
	}
	sort.Strings(sortedLeaves)

	otherOwner := func(key, self string) (string, bool) {
		for _, o := range fileOwner[key] {
			if o != self {
				return o, true
			}
		}
		return "", false
	}

	var findings []Finding
	for _, c := range changes {
		// Only a newly added regular-file path is queried: a modify keeps
		// an existing path (any collision on it pre-existed in base), and
		// a non-regular add (symlink, submodule, special, unusual mode)
		// is already publish-blocking on its own, so layering a collision
		// finding on it is redundant. Regular adds carry a git mode;
		// non-regular changes do not.
		if c.kind != ChangeAdded || c.mode == "" {
			continue
		}
		comps := strings.Split(pathfold.FoldPath(c.path), "/")
		key := strings.Join(comps, "/")
		partner, ok := otherOwner(key, c.path) // another leaf folds to the same name
		for i := 1; !ok && i < len(comps); i++ {
			// a folded ancestor of c is itself a leaf (a file where c
			// needs a directory)
			partner, ok = otherOwner(strings.Join(comps[:i], "/"), c.path)
		}
		if !ok {
			// c is a directory another leaf occupies: the first leaf at or
			// after key+"/" that carries that prefix is a descendant of c.
			prefix := key + "/"
			if i := sort.SearchStrings(sortedLeaves, prefix); i < len(sortedLeaves) && strings.HasPrefix(sortedLeaves[i], prefix) {
				partner, ok = leafPartner(fileOwner[sortedLeaves[i]], c.path)
			}
		}
		if ok {
			findings = append(findings, Finding{
				Path:   c.path,
				Kind:   FindingPathCollision,
				Detail: "collides with " + partner + " under case/normalization folding",
			})
		}
	}
	return findings
}

// leafPartner returns an owner of a fold that is not self.
func leafPartner(owners []string, self string) (string, bool) {
	for _, o := range owners {
		if o != self {
			return o, true
		}
	}
	return "", false
}

// CheckoutFoldComponent folds one repository path component using the case
// and Unicode normalization posture of the reference checkout filesystem.
func CheckoutFoldComponent(component string) string {
	return pathfold.FoldComponent(component)
}
