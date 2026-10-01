package agenttree

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Root is the tree's directory inside the checkout that holds it.
const Root = "policy"

const (
	agentsDir    = "agents/"
	routesDir    = "fragments/routes/"
	adaptersDir  = "fragments/adapters/"
	offersDir    = "fragments/offers/"
	lineupPath   = "lineup"
	lockPath     = "agents.lock"
	markSuffix   = ".attended"
	maxTreeFiles = 4096
)

var (
	// ErrMalformed marks a tree file that does not parse or a tree whose
	// documents do not fit together. The whole load fails: a partly read
	// tree would admit against configuration nobody reviewed as written.
	ErrMalformed = errors.New("agent tree is malformed")
	// ErrLockMismatch marks an agents.lock that disagrees with the content it
	// names: a missing or extra entry, or a digest the content does not hash
	// to.
	ErrLockMismatch = errors.New("agents.lock disagrees with the tree")
	// ErrUnknownAgent marks a lookup of an agent name the tree does not carry.
	ErrUnknownAgent = errors.New("agent is not in the tree")
)

// Files maps a tree-relative slash path ("agents/x", "lineup") to its bytes.
type Files map[string][]byte

// Route, Adapter, and Offer pair a fragment with its tree name. The name is
// the file name and enters no digest.
type Route struct {
	Name     string
	Fragment domain.RouteFragment
}

type Adapter struct {
	Name     string
	Fragment domain.AdapterFragment
}

// Offer also carries the route it is authored under, which is its directory.
type Offer struct {
	Route    string
	Name     string
	Fragment domain.OfferFragment
}

// Mark is one line of an agent's .attended file: the operator's statement
// that this exact agent, under this exact launch, may run unattended.
type Mark struct {
	Agent        string
	AgentDigest  domain.Digest
	LaunchDigest domain.Digest
}

// LineupLine is one line of the lineup file. Key is the part after
// domain.LineupRoleKeyPrefix: a role name, or "<role>.shadow.<name>".
type LineupLine struct {
	Key       string
	Selection domain.LineupSelection
}

// Tree is one parsed revision. Every slice is sorted by name, so two trees
// with the same content are equal and render to the same bytes.
type Tree struct {
	Agents   []domain.AgentSource
	Marks    []Mark
	Routes   []Route
	Adapters []Adapter
	Offers   []Offer
	Lineup   []LineupLine
}

// Sort puts every slice but the lineup in the order Parse yields (file path
// order), so a tree built in code compares equal to its own round trip. The
// lineup keeps its authored order, which is the file's line order.
func (t *Tree) Sort() {
	slices.SortFunc(t.Agents, func(a, b domain.AgentSource) int { return strings.Compare(a.Name, b.Name) })
	slices.SortStableFunc(t.Marks, func(a, b Mark) int {
		return strings.Compare(a.Agent+markSuffix, b.Agent+markSuffix)
	})
	slices.SortFunc(t.Routes, func(a, b Route) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(t.Adapters, func(a, b Adapter) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(t.Offers, func(a, b Offer) int {
		return strings.Compare(a.Route+"/"+a.Name, b.Route+"/"+b.Name)
	})
}

// Revision is the content address of a tree revision's files, recorded on an
// admission as its lineup revision. It covers the bytes, not the commit, so
// a commit that leaves the tree alone keeps the revision.
func Revision(files Files) (domain.Digest, error) {
	type entry struct {
		Path   string `json:"path"`
		Digest string `json:"digest"`
	}
	body := struct {
		Version string  `json:"version"`
		Files   []entry `json:"files"`
	}{Version: "freeside-agent-tree/v1", Files: make([]entry, 0, len(files))}
	for _, path := range sortedPaths(files) {
		body.Files = append(body.Files, entry{Path: path, Digest: contentaddr.Sum(files[path])})
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("agent tree revision: %w", err)
	}
	return domain.Digest(contentaddr.Sum(encoded)), nil
}

// Parse reads one revision. It fails closed on a file outside the layout, a
// document that does not parse, an agent naming a fragment the tree lacks, a
// lineup the role rules refuse, a mark for an agent the tree lacks, and an
// agents.lock that disagrees with the content.
func Parse(files Files) (Tree, error) {
	if len(files) > maxTreeFiles {
		return Tree{}, fmt.Errorf("%d files: %w", len(files), ErrMalformed)
	}
	var tree Tree
	var lineupSeen, lockSeen bool
	for _, path := range sortedPaths(files) {
		body := files[path]
		if len(body) > domain.MaxAgentFragmentBytes {
			return Tree{}, fmt.Errorf("%s is %d bytes: %w", path, len(body), ErrMalformed)
		}
		if err := tree.parseFile(path, body, &lineupSeen, &lockSeen); err != nil {
			return Tree{}, err
		}
	}
	if !lineupSeen || !lockSeen {
		return Tree{}, fmt.Errorf("lineup or agents.lock is missing: %w", ErrMalformed)
	}
	if err := tree.validate(); err != nil {
		return Tree{}, err
	}
	want, err := tree.lockEntries()
	if err != nil {
		return Tree{}, err
	}
	got, err := parseLock(files[lockPath])
	if err != nil {
		return Tree{}, err
	}
	if !slices.Equal(got, want) {
		return Tree{}, fmt.Errorf("have %d entries, content resolves to %d or to other digests: %w",
			len(got), len(want), ErrLockMismatch)
	}
	return tree, nil
}

func (t *Tree) parseFile(path string, body []byte, lineupSeen, lockSeen *bool) error {
	switch {
	case path == lineupPath:
		lines, err := parseLineup(body)
		if err != nil {
			return err
		}
		t.Lineup, *lineupSeen = lines, true
	case path == lockPath:
		// Compared against the content once every document is read.
		*lockSeen = true
	case strings.HasPrefix(path, agentsDir):
		name := strings.TrimPrefix(path, agentsDir)
		if agent, isMark := strings.CutSuffix(name, markSuffix); isMark {
			marks, err := parseMarks(agent, body)
			if err != nil {
				return err
			}
			t.Marks = append(t.Marks, marks...)
			return nil
		}
		source, err := parseAgent(name, body)
		if err != nil {
			return err
		}
		t.Agents = append(t.Agents, source)
	case strings.HasPrefix(path, routesDir):
		name := strings.TrimPrefix(path, routesDir)
		fragment, err := decodeNamed(path, name, body, domain.DecodeRouteFragment)
		if err != nil {
			return err
		}
		t.Routes = append(t.Routes, Route{Name: name, Fragment: fragment})
	case strings.HasPrefix(path, adaptersDir):
		name := strings.TrimPrefix(path, adaptersDir)
		fragment, err := decodeNamed(path, name, body, domain.DecodeAdapterFragment)
		if err != nil {
			return err
		}
		t.Adapters = append(t.Adapters, Adapter{Name: name, Fragment: fragment})
	case strings.HasPrefix(path, offersDir):
		route, name, ok := strings.Cut(strings.TrimPrefix(path, offersDir), "/")
		if !ok || !validName(route) {
			return fmt.Errorf("%s is not fragments/offers/<route>/<offer>: %w", path, ErrMalformed)
		}
		fragment, err := decodeNamed(path, name, body, domain.DecodeOfferFragment)
		if err != nil {
			return err
		}
		t.Offers = append(t.Offers, Offer{Route: route, Name: name, Fragment: fragment})
	default:
		return fmt.Errorf("%s is outside the tree layout: %w", path, ErrMalformed)
	}
	return nil
}

func decodeNamed[T any](path, name string, body []byte, decode func([]byte) (T, error)) (T, error) {
	var zero T
	if !validName(name) {
		return zero, fmt.Errorf("%s: name %q: %w", path, name, ErrMalformed)
	}
	// One final newline, like every other tree file, so the patch renderer
	// never needs git's no-newline marker.
	if len(body) == 0 || body[len(body)-1] != '\n' {
		return zero, fmt.Errorf("%s does not end with a newline: %w", path, ErrMalformed)
	}
	fragment, err := decode(body[:len(body)-1])
	if err != nil {
		return zero, fmt.Errorf("%s: %w", path, errors.Join(ErrMalformed, err))
	}
	return fragment, nil
}

// validate checks what one file cannot: every agent's fragments are present,
// the lineup follows the role rules, and every mark has its agent.
func (t Tree) validate() error {
	for _, agent := range t.Agents {
		if _, err := t.closure(agent); err != nil {
			return err
		}
	}
	if _, err := t.ResolveLineup(); err != nil {
		return err
	}
	for _, mark := range t.Marks {
		if _, ok := t.Agent(mark.Agent); !ok {
			return fmt.Errorf("attended mark for %q: %w", mark.Agent, errors.Join(ErrMalformed, ErrUnknownAgent))
		}
	}
	return nil
}

// Agent returns the named agent's source lines.
func (t Tree) Agent(name string) (domain.AgentSource, bool) {
	for _, agent := range t.Agents {
		if agent.Name == name {
			return agent, true
		}
	}
	return domain.AgentSource{}, false
}

// ResolveLineup applies the lineup contract's key rules to the lineup file.
func (t Tree) ResolveLineup() (domain.Lineup, error) {
	keys := make([]domain.PolicyKey, 0, len(t.Lineup))
	for _, line := range t.Lineup {
		keys = append(keys, domain.PolicyKey{
			Key: domain.LineupRoleKeyPrefix + line.Key, Value: renderSelection(line.Selection),
		})
	}
	lineup, err := domain.ResolveLineup(keys)
	if err != nil {
		return domain.Lineup{}, fmt.Errorf("lineup: %w", errors.Join(ErrMalformed, err))
	}
	return lineup, nil
}

// Attended reports whether the agent, under the launch, still has to run
// attended: true unless a mark names exactly this agent digest and launch
// digest. A mark for an earlier digest of the same name does not carry.
func (t Tree) Attended(agent string, agentDigest, launchDigest domain.Digest) bool {
	return !slices.Contains(t.Marks, Mark{Agent: agent, AgentDigest: agentDigest, LaunchDigest: launchDigest})
}

// agentClosure is the fragments an agent's lines name.
type agentClosure struct {
	route   domain.RouteFragment
	adapter domain.AdapterFragment
	offer   domain.OfferFragment
}

func (t Tree) closure(agent domain.AgentSource) (agentClosure, error) {
	var closure agentClosure
	missing := func(kind, name string) (agentClosure, error) {
		return agentClosure{}, fmt.Errorf("agent %s names %s %q, which the tree lacks: %w",
			agent.Name, kind, name, ErrMalformed)
	}
	route := slices.IndexFunc(t.Routes, func(r Route) bool { return r.Name == agent.Route })
	if route < 0 {
		return missing("route", agent.Route)
	}
	closure.route = t.Routes[route].Fragment
	adapter := slices.IndexFunc(t.Adapters, func(a Adapter) bool { return a.Name == agent.Adapter })
	if adapter < 0 {
		return missing("adapter", agent.Adapter)
	}
	closure.adapter = t.Adapters[adapter].Fragment
	// The offer is looked up under the agent's own route: an offer of the
	// same name under another route is another offer.
	offer := slices.IndexFunc(t.Offers, func(o Offer) bool {
		return o.Route == agent.Route && o.Name == agent.Offer
	})
	if offer < 0 {
		return missing("offer", agent.Route+"/"+agent.Offer)
	}
	closure.offer = t.Offers[offer].Fragment
	return closure, nil
}

// AgentDigest computes the agent's digest from the tree alone. The canonical
// body holds the enrollment id and the fragment digests, all of which the
// tree carries; whether the join holds against the enrollment record is
// ResolveAgent's question.
func (t Tree) AgentDigest(name string) (domain.Digest, error) {
	agent, ok := t.Agent(name)
	if !ok {
		return "", fmt.Errorf("agent %q: %w", name, ErrUnknownAgent)
	}
	closure, err := t.closure(agent)
	if err != nil {
		return "", err
	}
	return domain.AgentDefinition{
		EncodingVersion: domain.AgentEncodingVersion,
		EnrollmentID:    domain.ClientEnrollmentID(agent.Enrollment),
		RouteDigest:     closure.route.Digest, AdapterDigest: closure.adapter.Digest,
		OfferDigest: closure.offer.Digest, Effort: agent.Effort,
	}.ComputeDigest()
}

// Render emits the tree's files, agents.lock included. Parse of the result
// returns an equal tree.
func Render(tree Tree) (Files, error) {
	if err := tree.validate(); err != nil {
		return nil, err
	}
	files := Files{}
	for _, agent := range tree.Agents {
		if _, dup := files[agentsDir+agent.Name]; dup {
			return nil, fmt.Errorf("agent %s is authored twice: %w", agent.Name, ErrMalformed)
		}
		body, err := renderAgent(agent)
		if err != nil {
			return nil, err
		}
		files[agentsDir+agent.Name] = body
	}
	for _, mark := range tree.Marks {
		line, err := renderMark(mark)
		if err != nil {
			return nil, err
		}
		path := agentsDir + mark.Agent + markSuffix
		files[path] = append(files[path], line...)
	}
	for _, route := range tree.Routes {
		if err := putFragment(files, routesDir, route.Name, route.Fragment.Encode); err != nil {
			return nil, err
		}
	}
	for _, adapter := range tree.Adapters {
		if err := putFragment(files, adaptersDir, adapter.Name, adapter.Fragment.Encode); err != nil {
			return nil, err
		}
	}
	for _, offer := range tree.Offers {
		if !validName(offer.Route) {
			return nil, fmt.Errorf("offer route %q: %w", offer.Route, ErrMalformed)
		}
		if err := putFragment(files, offersDir+offer.Route+"/", offer.Name, offer.Fragment.Encode); err != nil {
			return nil, err
		}
	}
	files[lineupPath] = renderLineup(tree.Lineup)
	lock, err := tree.lockEntries()
	if err != nil {
		return nil, err
	}
	files[lockPath] = renderLock(lock)
	return files, nil
}

func putFragment(files Files, dir, name string, encode func() ([]byte, error)) error {
	if !validName(name) {
		return fmt.Errorf("%s name %q: %w", dir, name, ErrMalformed)
	}
	if _, dup := files[dir+name]; dup {
		return fmt.Errorf("%s%s is authored twice: %w", dir, name, ErrMalformed)
	}
	body, err := encode()
	if err != nil {
		return fmt.Errorf("%s%s: %w", dir, name, err)
	}
	files[dir+name] = append(body, '\n')
	return nil
}

func sortedPaths(files Files) []string {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// validName is the rule for a fragment's tree name, which is a file name:
// lowercase ASCII alphanumeric, with '-', '_', and '.' as interior
// separators ("gpt-5.6-sol"). An agent's name follows the stricter
// domain.AgentSource rule, which leaves room for the ".attended" suffix.
func validName(name string) bool {
	if name == "" || len(name) > 255 {
		return false
	}
	for i := range len(name) {
		char := name[i]
		if ('a' <= char && char <= 'z') || ('0' <= char && char <= '9') {
			continue
		}
		if 0 < i && i < len(name)-1 && (char == '-' || char == '_' || char == '.') {
			continue
		}
		return false
	}
	return true
}
