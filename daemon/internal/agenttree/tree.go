package agenttree

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Root is the tree's directory inside the checkout that holds it.
const Root = "policy"

const (
	routesDir    = "fragments/routes/"
	adaptersDir  = "fragments/adapters/"
	modelsDir    = "models/"
	lineupPath   = "lineup"
	lockPath     = "agents.lock"
	maxTreeFiles = 4096
	// removedAgentsDir and removedOffersDir held the agent and offer
	// documents until #2599 removed them. A leftover file, an attended mark
	// among them, is refused with the fix rather than as a path outside the
	// layout.
	removedAgentsDir = "agents/"
	removedOffersDir = "fragments/offers/"
)

// migrateFix is what a refusal of a removed document tells the operator.
const migrateFix = "agent and offer documents were removed; run `freesided auth migrate-tree` and commit its patch"

var (
	// ErrMalformed marks a tree file that does not parse or a tree whose
	// documents do not fit together. The whole load fails: a partly read
	// tree would admit against configuration nobody reviewed as written.
	ErrMalformed = errors.New("agent tree is malformed")
	// ErrLockMismatch marks an agents.lock that disagrees with the content it
	// names: a missing or extra entry, or a digest the content does not hash
	// to.
	ErrLockMismatch = errors.New("agents.lock disagrees with the tree")
)

// Files maps a tree-relative slash path ("lineup", "fragments/routes/x") to
// its bytes.
type Files map[string][]byte

// Route and Adapter pair a fragment with its tree name. The name is the file
// name and enters no digest.
type Route struct {
	Name     string
	Fragment domain.RouteFragment
	// Models is the route's models/<name> list, nil when the tree has none.
	// It is operator text outside agents.lock, like the lineup, and binds
	// at admission only (domain.ResolveAgentSelection).
	Models *domain.RouteModelList
}

type Adapter struct {
	Name     string
	Fragment domain.AdapterFragment
}

// LineupLine is one line of the lineup file. Key is the part after
// domain.LineupRoleKeyPrefix: a role name, "<role>.shadow.<name>", or
// "specifier.escalated.<agent>".
type LineupLine struct {
	Key       string
	Selection domain.LineupSelection
}

// Tree is one parsed revision. Every slice is sorted by name, so two trees
// with the same content are equal and render to the same bytes.
type Tree struct {
	Routes   []Route
	Adapters []Adapter
	Lineup   []LineupLine
}

// Sort puts every slice but the lineup in the order Parse yields (file path
// order), so a tree built in code compares equal to its own round trip. The
// lineup keeps its authored order, which is the file's line order.
func (t *Tree) Sort() {
	slices.SortFunc(t.Routes, func(a, b Route) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(t.Adapters, func(a, b Adapter) int { return strings.Compare(a.Name, b.Name) })
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
// document that does not parse, a leftover agent or offer document (an
// attended mark among them), a model list for a route the tree lacks, a
// lineup the role rules refuse, and an agents.lock that disagrees with the
// content.
func Parse(files Files) (Tree, error) {
	if len(files) > maxTreeFiles {
		return Tree{}, fmt.Errorf("%d files: %w", len(files), ErrMalformed)
	}
	var tree Tree
	var lineupSeen, lockSeen bool
	models := map[string]*domain.RouteModelList{}
	for _, path := range sortedPaths(files) {
		body := files[path]
		if len(body) > domain.MaxAgentFragmentBytes {
			return Tree{}, fmt.Errorf("%s is %d bytes: %w", path, len(body), ErrMalformed)
		}
		if err := tree.parseFile(path, body, models, &lineupSeen, &lockSeen); err != nil {
			return Tree{}, err
		}
	}
	if !lineupSeen || !lockSeen {
		return Tree{}, fmt.Errorf("lineup or agents.lock is missing: %w", ErrMalformed)
	}
	for i, route := range tree.Routes {
		tree.Routes[i].Models = models[route.Name]
		delete(models, route.Name)
	}
	if orphans := slices.Sorted(maps.Keys(models)); len(orphans) > 0 {
		return Tree{}, fmt.Errorf("%s%s lists models for a route the tree lacks: %w",
			modelsDir, orphans[0], ErrMalformed)
	}
	if err := tree.validate(); err != nil {
		return Tree{}, err
	}
	want := tree.lockEntries()
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

func (t *Tree) parseFile(
	path string, body []byte, models map[string]*domain.RouteModelList, lineupSeen, lockSeen *bool,
) error {
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
	case strings.HasPrefix(path, removedAgentsDir), strings.HasPrefix(path, removedOffersDir):
		return fmt.Errorf("%s: %s: %w", path, migrateFix, ErrMalformed)
	case strings.HasPrefix(path, modelsDir):
		route := strings.TrimPrefix(path, modelsDir)
		if !validName(route) {
			return fmt.Errorf("%s: route name %q: %w", path, route, ErrMalformed)
		}
		list, err := parseModelList(path, body)
		if err != nil {
			return err
		}
		models[route] = &list
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

// validate checks what one file cannot: the lineup follows the role rules.
func (t Tree) validate() error {
	if _, err := t.ResolveLineup(); err != nil {
		return err
	}
	return nil
}

// ResolveLineup applies the lineup contract's key rules, the escalated
// pairing rule among them, to the lineup file. A line's selection resolves
// at admission against the enrollment's catalog.
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

// Route returns the named route's fragment. Admission reads the route an
// enrollment's Route field names this way.
func (t Tree) Route(name string) (domain.RouteFragment, bool) {
	i := slices.IndexFunc(t.Routes, func(r Route) bool { return r.Name == name })
	if i < 0 {
		return domain.RouteFragment{}, false
	}
	return t.Routes[i].Fragment, true
}

// RouteModels returns the named route's model list, nil when the route has
// none or the tree lacks the route.
func (t Tree) RouteModels(name string) *domain.RouteModelList {
	i := slices.IndexFunc(t.Routes, func(r Route) bool { return r.Name == name })
	if i < 0 {
		return nil
	}
	return t.Routes[i].Models
}

// Render emits the tree's files, agents.lock included. Parse of the result
// returns an equal tree.
func Render(tree Tree) (Files, error) {
	if err := tree.validate(); err != nil {
		return nil, err
	}
	files := Files{}
	for _, route := range tree.Routes {
		if err := putFragment(files, routesDir, route.Name, route.Fragment.Encode); err != nil {
			return nil, err
		}
		if route.Models != nil {
			body, err := renderModelList(*route.Models)
			if err != nil {
				return nil, fmt.Errorf("%s%s: %w", modelsDir, route.Name, err)
			}
			files[modelsDir+route.Name] = body
		}
	}
	for _, adapter := range tree.Adapters {
		if err := putFragment(files, adaptersDir, adapter.Name, adapter.Fragment.Encode); err != nil {
			return nil, err
		}
	}
	files[lineupPath] = renderLineup(tree.Lineup)
	files[lockPath] = renderLock(tree.lockEntries())
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
// separators ("gpt-5.6-sol").
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
