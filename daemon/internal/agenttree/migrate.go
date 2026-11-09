package agenttree

import (
	"slices"
	"strings"
)

// Migrate moves a revision in the layout before #2599 to the current one:
// it drops every agents/ and fragments/offers/ file and every agents.lock
// line of kind agent or offer, and touches nothing else. The lineup needs no
// rewrite, because its lines have held selections since #2734. The result
// must parse, so a revision that was malformed for another reason stays
// refused, by Parse's own error. A revision already in the current layout
// migrates to itself.
//
// Like adoption, Migrate only computes files: `freesided auth migrate-tree`
// prints the change as a patch, and a human reviews and commits it.
func Migrate(old Files) (Files, error) {
	next := make(Files, len(old))
	for path, body := range old {
		if strings.HasPrefix(path, removedAgentsDir) || strings.HasPrefix(path, removedOffersDir) {
			continue
		}
		next[path] = body
	}
	if lock, ok := next[lockPath]; ok {
		next[lockPath] = dropRemovedLockLines(lock)
	}
	if _, err := Parse(next); err != nil {
		return nil, err
	}
	return next, nil
}

// dropRemovedLockLines keeps every line of an agents.lock whose first field
// is not a removed kind, byte for byte. A malformed line is kept for Parse
// to refuse.
func dropRemovedLockLines(body []byte) []byte {
	var out []byte
	for line := range strings.Lines(string(body)) {
		fields := strings.Fields(line)
		if len(fields) > 0 && slices.Contains(removedLockKinds, fields[0]) {
			continue
		}
		out = append(out, line...)
	}
	return out
}
