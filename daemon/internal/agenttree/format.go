package agenttree

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// The text formats. Each is line-oriented with fields separated by spaces,
// so a change reads as a one-line diff. Every file ends with a newline and
// carries no comments: a comment would be operator text the daemon reads
// past, and the tree has review for that.

// lines splits a text file into its lines. The file must end with a newline
// and hold no blank line.
func lines(path string, body []byte) ([][]string, error) {
	if len(body) == 0 {
		return nil, nil
	}
	if body[len(body)-1] != '\n' {
		return nil, fmt.Errorf("%s does not end with a newline: %w", path, ErrMalformed)
	}
	var out [][]string
	for _, line := range strings.Split(string(body[:len(body)-1]), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			return nil, fmt.Errorf("%s has a blank line: %w", path, ErrMalformed)
		}
		out = append(out, fields)
	}
	return out, nil
}

// parseModelList reads a route's models/<route> file: one "<verb> <model-id>"
// line for each model, every line the same verb, allow or deny. The file
// holds at least one line; the rest of the list's rules are
// domain.RouteModelList's.
func parseModelList(path string, body []byte) (domain.RouteModelList, error) {
	rows, err := lines(path, body)
	if err != nil {
		return domain.RouteModelList{}, err
	}
	if len(rows) == 0 {
		return domain.RouteModelList{}, fmt.Errorf("%s lists no model: %w", path, ErrMalformed)
	}
	list := domain.RouteModelList{Mode: domain.ModelListMode(rows[0][0])}
	for _, row := range rows {
		if len(row) != 2 {
			return domain.RouteModelList{}, fmt.Errorf("%s line %q is not <allow|deny> <model-id>: %w",
				path, strings.Join(row, " "), ErrMalformed)
		}
		if domain.ModelListMode(row[0]) != list.Mode {
			return domain.RouteModelList{}, fmt.Errorf("%s mixes %s and %s; a list is all allow or all deny: %w",
				path, list.Mode, row[0], ErrMalformed)
		}
		list.Models = append(list.Models, row[1])
	}
	if err := list.Validate(); err != nil {
		return domain.RouteModelList{}, fmt.Errorf("%s: %w", path, errors.Join(ErrMalformed, err))
	}
	return list, nil
}

func renderModelList(list domain.RouteModelList) ([]byte, error) {
	if err := list.Validate(); err != nil {
		return nil, errors.Join(ErrMalformed, err)
	}
	var out bytes.Buffer
	for _, model := range list.Models {
		fmt.Fprintf(&out, "%s %s\n", list.Mode, model)
	}
	return out.Bytes(), nil
}

// parseLineup reads the lineup file: one "<key> <selection>" line for each
// role, where the key is the role name (or "<role>.shadow.<name>", or
// "specifier.escalated.<enrollment-id>") and the
// selection is the lineup contract's value. Which keys are legal is
// domain.ResolveLineup's rule, applied once the tree is whole.
func parseLineup(body []byte) ([]LineupLine, error) {
	rows, err := lines(lineupPath, body)
	if err != nil {
		return nil, err
	}
	out := make([]LineupLine, 0, len(rows))
	for _, row := range rows {
		if len(row) != 2 {
			return nil, fmt.Errorf("lineup line %q is not <role> <selection>: %w",
				strings.Join(row, " "), ErrMalformed)
		}
		selection, err := domain.ParseLineupSelection(row[1])
		if err != nil {
			return nil, fmt.Errorf("lineup line for %s: %w", row[0], errors.Join(ErrMalformed, err))
		}
		out = append(out, LineupLine{Key: row[0], Selection: selection})
	}
	return out, nil
}

func renderSelection(s domain.LineupSelection) string {
	n := s.Names
	return fmt.Sprintf("%s/%s/%s/%s@%s", n.Enrollment, n.Model, n.Effort, s.PromptName, s.PromptDigest)
}

func renderLineup(lineup []LineupLine) []byte {
	var out bytes.Buffer
	for _, line := range lineup {
		fmt.Fprintf(&out, "%s %s\n", line.Key, renderSelection(line.Selection))
	}
	return out.Bytes()
}

// lockEntry is one agents.lock line: "<kind> <name> <digest>", where the
// kind is adapter or route.
type lockEntry struct {
	Kind   string
	Name   string
	Digest domain.Digest
}

// Lock entry kinds. removedLockKinds are the kinds #2599 removed with the
// agent and offer documents; a line of one is refused with the fix.
const (
	lockKindAdapter = "adapter"
	lockKindRoute   = "route"
)

var removedLockKinds = []string{"agent", "offer"}

// lockEntries is the name-to-digest map the tree's content resolves to,
// sorted by kind and then name.
func (t Tree) lockEntries() []lockEntry {
	var entries []lockEntry
	for _, adapter := range t.Adapters {
		entries = append(entries, lockEntry{lockKindAdapter, adapter.Name, adapter.Fragment.Digest})
	}
	for _, route := range t.Routes {
		entries = append(entries, lockEntry{lockKindRoute, route.Name, route.Fragment.Digest})
	}
	slices.SortFunc(entries, func(a, b lockEntry) int {
		return strings.Compare(a.Kind+" "+a.Name, b.Kind+" "+b.Name)
	})
	return entries
}

func parseLock(body []byte) ([]lockEntry, error) {
	rows, err := lines(lockPath, body)
	if err != nil {
		return nil, err
	}
	var entries []lockEntry
	for _, row := range rows {
		if slices.Contains(removedLockKinds, row[0]) {
			return nil, fmt.Errorf("agents.lock line %q: %s: %w", strings.Join(row, " "), migrateFix, ErrMalformed)
		}
		if len(row) != 3 || !contentaddr.Valid(row[2]) {
			return nil, fmt.Errorf("agents.lock line %q is not <kind> <name> <digest>: %w",
				strings.Join(row, " "), ErrMalformed)
		}
		entries = append(entries, lockEntry{row[0], row[1], domain.Digest(row[2])})
	}
	return entries, nil
}

func renderLock(entries []lockEntry) []byte {
	var out bytes.Buffer
	for _, entry := range entries {
		fmt.Fprintf(&out, "%s %s %s\n", entry.Kind, entry.Name, entry.Digest)
	}
	return out.Bytes()
}
