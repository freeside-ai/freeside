package agenttree

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

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

// parseAgent reads the four-line agent document:
//
//	who      enrollment  <enrollment-id>
//	through  route       <route-name>
//	running  adapter     <adapter-name>
//	asking   offer       <offer-name>, effort <effort>
func parseAgent(name string, body []byte) (domain.AgentSource, error) {
	path := agentsDir + name
	rows, err := lines(path, body)
	if err != nil {
		return domain.AgentSource{}, err
	}
	bad := func() (domain.AgentSource, error) {
		return domain.AgentSource{}, fmt.Errorf(
			"%s is not the four lines who, through, running, asking: %w", path, ErrMalformed)
	}
	if len(rows) != 4 {
		return bad()
	}
	heads := [][2]string{{"who", "enrollment"}, {"through", "route"}, {"running", "adapter"}, {"asking", "offer"}}
	for i, head := range heads {
		want := 3
		if i == 3 {
			want = 5
		}
		if len(rows[i]) != want || rows[i][0] != head[0] || rows[i][1] != head[1] {
			return bad()
		}
	}
	offer, comma := strings.CutSuffix(rows[3][2], ",")
	if !comma || rows[3][3] != "effort" {
		return bad()
	}
	source := domain.AgentSource{
		Name: name, Enrollment: rows[0][2], Route: rows[1][2], Adapter: rows[2][2],
		Offer: offer, Effort: domain.EffortLevel(rows[3][4]),
	}
	if err := validateAgentSource(source); err != nil {
		return domain.AgentSource{}, err
	}
	return source, nil
}

func validateAgentSource(source domain.AgentSource) error {
	if err := source.Validate(); err != nil {
		return fmt.Errorf("agent %s: %w", source.Name, errors.Join(ErrMalformed, err))
	}
	for field, name := range map[string]string{
		"route": source.Route, "adapter": source.Adapter, "offer": source.Offer,
	} {
		if !validName(name) {
			return fmt.Errorf("agent %s %s name %q: %w", source.Name, field, name, ErrMalformed)
		}
	}
	// An enrollment id is a record key, not a tree name, so it follows no
	// name rule; it only has to survive as one field of its line.
	if strings.ContainsFunc(source.Enrollment, unicode.IsSpace) {
		return fmt.Errorf("agent %s enrollment id %q has whitespace: %w",
			source.Name, source.Enrollment, ErrMalformed)
	}
	return nil
}

func renderAgent(source domain.AgentSource) ([]byte, error) {
	if err := validateAgentSource(source); err != nil {
		return nil, err
	}
	return fmt.Appendf(nil,
		"who      enrollment  %s\nthrough  route       %s\nrunning  adapter     %s\nasking   offer       %s, effort %s\n",
		source.Enrollment, source.Route, source.Adapter, source.Offer, source.Effort), nil
}

// parseMarks reads an agent's .attended file: one "<agent-digest>
// <launch-digest>" line for each launch the agent may run unattended.
func parseMarks(agent string, body []byte) ([]Mark, error) {
	path := agentsDir + agent + markSuffix
	rows, err := lines(path, body)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%s is empty: %w", path, ErrMalformed)
	}
	marks := make([]Mark, 0, len(rows))
	for _, row := range rows {
		if len(row) != 2 || !contentaddr.Valid(row[0]) || !contentaddr.Valid(row[1]) {
			return nil, fmt.Errorf("%s is not <agent-digest> <launch-digest> lines: %w", path, ErrMalformed)
		}
		mark := Mark{Agent: agent, AgentDigest: domain.Digest(row[0]), LaunchDigest: domain.Digest(row[1])}
		if slices.Contains(marks, mark) {
			return nil, fmt.Errorf("%s repeats a mark: %w", path, ErrMalformed)
		}
		marks = append(marks, mark)
	}
	return marks, nil
}

func renderMark(mark Mark) ([]byte, error) {
	if !contentaddr.Valid(string(mark.AgentDigest)) || !contentaddr.Valid(string(mark.LaunchDigest)) {
		return nil, fmt.Errorf("attended mark for %q: %w", mark.Agent, ErrMalformed)
	}
	return fmt.Appendf(nil, "%s %s\n", mark.AgentDigest, mark.LaunchDigest), nil
}

// parseLineup reads the lineup file: one "<key> <selection>" line for each
// role, where the key is the role name (or "<role>.shadow.<name>") and the
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
	return fmt.Sprintf("%s@%s/%s@%s", s.AgentName, s.AgentDigest, s.PromptName, s.PromptDigest)
}

func renderLineup(lineup []LineupLine) []byte {
	var out bytes.Buffer
	for _, line := range lineup {
		fmt.Fprintf(&out, "%s %s\n", line.Key, renderSelection(line.Selection))
	}
	return out.Bytes()
}

// lockEntry is one agents.lock line: "<kind> <name> <digest>". An offer's
// name is "<route>/<offer>".
type lockEntry struct {
	Kind   string
	Name   string
	Digest domain.Digest
}

// lockEntries is the name-to-digest map the tree's content resolves to,
// sorted by kind and then name.
func (t Tree) lockEntries() ([]lockEntry, error) {
	var entries []lockEntry
	for _, adapter := range t.Adapters {
		entries = append(entries, lockEntry{"adapter", adapter.Name, adapter.Fragment.Digest})
	}
	for _, agent := range t.Agents {
		digest, err := t.AgentDigest(agent.Name)
		if err != nil {
			return nil, err
		}
		entries = append(entries, lockEntry{"agent", agent.Name, digest})
	}
	for _, offer := range t.Offers {
		entries = append(entries, lockEntry{"offer", offer.Route + "/" + offer.Name, offer.Fragment.Digest})
	}
	for _, route := range t.Routes {
		entries = append(entries, lockEntry{"route", route.Name, route.Fragment.Digest})
	}
	slices.SortFunc(entries, func(a, b lockEntry) int {
		return strings.Compare(a.Kind+" "+a.Name, b.Kind+" "+b.Name)
	})
	return entries, nil
}

func parseLock(body []byte) ([]lockEntry, error) {
	rows, err := lines(lockPath, body)
	if err != nil {
		return nil, err
	}
	var entries []lockEntry
	for _, row := range rows {
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
