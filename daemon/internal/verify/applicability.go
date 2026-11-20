package verify

import (
	"encoding/json"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/pathfold"
)

// NotApplicableProof is plan §6's NotApplicable { resolution_proof } for
// a recipe command that did not run: the digest of the rule that skipped
// it and the digest of the change set the rule was read against. Anyone
// holding the approved recipe and the change set can recompute both and
// re-derive the skip.
type NotApplicableProof struct {
	// RuleDigest addresses the command's paths and the recipe's
	// run_all_on, in recipe order, under ruleEncodingV1.
	RuleDigest domain.Digest `json:"rule_digest"`
	// ChangesDigest addresses the sorted, distinct changed paths under
	// changesEncodingV1.
	ChangesDigest domain.Digest `json:"changes_digest"`
}

// valid reports whether both digests are well-formed content addresses.
func (p NotApplicableProof) valid() bool {
	return contentaddr.Valid(string(p.RuleDigest)) && contentaddr.Valid(string(p.ChangesDigest))
}

// The proof encodings are versioned so a later change to either can't
// make an old digest read as a new one.
const (
	ruleEncodingV1    = "freeside-recipe-rule-v1"
	changesEncodingV1 = "freeside-recipe-changes-v1"
)

// Skips is the applicability gate: for each of r's commands in recipe
// order, nil when the command runs against changes and its proof when it
// doesn't. It re-checks nothing stored; it reads only the trusted recipe
// the caller parsed and the change set the caller supplies, which must be
// the full change set from the verification's base to the tree it
// verifies (Options.Changes). A partial change set would skip a command
// that a missing path should run. A skip also assumes the command passes on
// that base: a re-entry or handoff whose base is a tip CI never passed can
// report a failing command it didn't run as not applicable.
//
// A command runs when it carries no paths, or when a changed path matches
// one of its paths or one of the recipe's run_all_on rules. Two change
// sets run every command, because neither can show that a command is
// unaffected: an empty one (nothing to read, so nothing to rule out), and
// one holding a path the daemon can't read as UTF-8 (a rule can't be
// matched against bytes it can't read). A rule ending in "/" matches every
// path under that directory; any other rule matches one exact path. This
// is the rule changed_under applies in .github/workflows/pull-request.yml,
// so a recipe can copy CI's selectors. A path also matches when its
// case-folded, alias-normalized form matches the folded rule, because a
// room on a case- or normalization-insensitive filesystem checks DAEMON/x
// out into daemon/x. Folding only adds matches, so it only makes more
// commands run.
func (r Recipe) Skips(changes []importer.Change) []*NotApplicableProof {
	proofs := make([]*NotApplicableProof, len(r.Commands))
	paths, readable := changedPaths(changes)
	folded := foldedPaths(paths)
	if !readable || len(paths) == 0 || anyMatch(paths, folded, r.RunAllOn) {
		return proofs
	}
	var changesDigest domain.Digest
	for i, cmd := range r.Commands {
		if cmd.Paths == nil || anyMatch(paths, folded, cmd.Paths) {
			continue
		}
		if changesDigest == "" {
			changesDigest = encodingDigest(changesEncodingV1, paths)
		}
		proofs[i] = &NotApplicableProof{
			RuleDigest: encodingDigest(ruleEncodingV1, struct {
				Paths    []string `json:"paths"`
				RunAllOn []string `json:"run_all_on"`
			}{cmd.Paths, r.RunAllOn}),
			ChangesDigest: changesDigest,
		}
	}
	return proofs
}

// RunsClass reports whether at least one command of class runs against
// changes. A class named only by skipped commands needs no room.
func (r Recipe) RunsClass(class ExecutionClass, changes []importer.Change) bool {
	skips := r.Skips(changes)
	for i, cmd := range r.Commands {
		if cmd.Class == class && skips[i] == nil {
			return true
		}
	}
	return false
}

// changedPaths returns the sorted, distinct changed paths, and false when
// any change lacks a path readable as UTF-8 (the importer then carries
// its raw bytes in PathHex).
func changedPaths(changes []importer.Change) ([]string, bool) {
	paths := make([]string, 0, len(changes))
	for _, c := range changes {
		if c.PathHex != "" || c.Path == "" || !utf8.ValidString(c.Path) {
			return nil, false
		}
		paths = append(paths, c.Path)
	}
	slices.Sort(paths)
	return slices.Compact(paths), true
}

// foldedPaths returns each path's matching key under the importer's
// collision model: the aliases a checkout collapses removed, then case and
// Unicode normalization folded.
func foldedPaths(paths []string) []string {
	folded := make([]string, len(paths))
	for i, p := range paths {
		folded[i] = pathfold.FoldPath(pathfold.NormalizeAliases(p))
	}
	return folded
}

// anyMatch reports whether a path, or its folded key in folded, matches
// any rule.
func anyMatch(paths, folded, rules []string) bool {
	for _, rule := range rules {
		foldedRule := pathfold.FoldPath(rule)
		for i, p := range paths {
			if ruleMatches(rule, p) || ruleMatches(foldedRule, folded[i]) {
				return true
			}
		}
	}
	return false
}

// ruleMatches matches one path against one rule: a rule ending in "/" as a
// directory prefix, any other rule exactly.
func ruleMatches(rule, p string) bool {
	return p == rule || (strings.HasSuffix(rule, "/") && strings.HasPrefix(p, rule))
}

// encodingDigest addresses a versioned JSON encoding of v. encoding/json
// writes a struct's fields in declaration order and a slice in its order,
// so the bytes are a function of v alone.
func encodingDigest(version string, v any) domain.Digest {
	body, err := json.Marshal(struct {
		Version string `json:"version"`
		Value   any    `json:"value"`
	}{version, v})
	if err != nil {
		// Only strings and string slices reach here, which always encode.
		panic("verify: encode proof: " + err.Error())
	}
	return domain.Digest(contentaddr.Sum(body))
}
