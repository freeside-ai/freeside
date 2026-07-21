package importer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/export"
)

const commitPlanVersion = "freeside.commit-plan/v1"

var errPlanStructural = errors.New("commit plan is structurally invalid")

type commitPlan struct {
	Version string               `json:"version"`
	Groups  []rawCommitPlanGroup `json:"groups"`
}

type rawCommitPlanGroup struct {
	Name      string          `json:"name"`
	Message   string          `json:"message"`
	Paths     json.RawMessage `json:"paths"`
	Remainder json.RawMessage `json:"remainder"`
}

type resolvedCommitGroup struct {
	name    string
	message string
	changes []plannedChange
}

func loadCommitPlan(handoffDir string, pol Policy) ([]byte, bool, error) {
	name := filepath.Join(handoffDir, export.CommitPlanFilename)
	f, err := openRegular(name, ErrCommitPlanUnreadable)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("open commit plan: %w: %w", ErrCommitPlanUnreadable, err)
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, pol.MaxCommitPlanBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("read commit plan: %w: %w", ErrCommitPlanUnreadable, err)
	}
	if int64(len(raw)) > pol.MaxCommitPlanBytes {
		return nil, false, fmt.Errorf("commit plan exceeds %d bytes: %w", pol.MaxCommitPlanBytes, ErrCommitPlanUnreadable)
	}
	return raw, true, nil
}

// scanCommitPlanStrings performs the tolerant stage-6 pass. Findings are
// emitted only after the entire document has parsed as exactly one JSON value;
// a malformed document therefore contributes no partially decoded strings.
func scanCommitPlanStrings(raw []byte) []Finding {
	if !utf8.Valid(raw) {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	// Keep syntactically valid, arbitrarily large JSON numbers as json.Number.
	// Float conversion failure must not suppress decoded-string scanning.
	dec.UseNumber()
	stringOrdinal := 0
	depth := 0
	topLevelValues := 0
	var firstFinding *Finding
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				if depth == 0 {
					topLevelValues++
				}
				depth++
			case '}', ']':
				depth--
			}
		} else if depth == 0 {
			topLevelValues++
		}
		if s, ok := tok.(string); ok {
			stringOrdinal++
			// Continue scanning after the first hit so the tolerant pass still
			// covers every decoded string token. Retaining one location is enough
			// to withhold construction and keeps memory independent of match count.
			if firstFinding == nil {
				if matches := scanText(export.CommitPlanFilename, []byte(s)); len(matches) > 0 {
					match := matches[0]
					match.Kind = FindingCommitPlanSecret
					match.Detail = fmt.Sprintf("decoded commit-plan JSON string %d", stringOrdinal)
					firstFinding = &match
				}
			} else {
				_ = scanText(export.CommitPlanFilename, []byte(s))
			}
		}
	}
	if topLevelValues != 1 || depth != 0 {
		return nil
	}
	if firstFinding == nil {
		return nil
	}
	return []Finding{*firstFinding}
}

func decodeAndResolveCommitPlan(raw []byte, changes []plannedChange, base map[string]treeEntry, pol Policy) ([]resolvedCommitGroup, error) {
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("commit plan is not UTF-8: %w", errPlanStructural)
	}
	if err := preboundCommitPlan(raw, pol.MaxCommitPlanGroups, pol.MaxEntries); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var plan commitPlan
	if err := dec.Decode(&plan); err != nil {
		return nil, fmt.Errorf("decode commit plan: %w: %w", errPlanStructural, err)
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("commit plan carries trailing content: %w", errPlanStructural)
	}
	if plan.Version != commitPlanVersion {
		return nil, fmt.Errorf("commit plan version %q: %w", plan.Version, errPlanStructural)
	}
	if len(plan.Groups) == 0 || len(plan.Groups) > pol.MaxCommitPlanGroups {
		return nil, fmt.Errorf("commit plan has %d groups: %w", len(plan.Groups), errPlanStructural)
	}

	byPath := make(map[string]plannedChange, len(changes))
	for _, c := range changes {
		if c.pathHex == "" {
			byPath[c.path] = c
		}
	}
	assigned := make(map[string]bool, len(changes))
	resolved := make([]resolvedCommitGroup, 0, len(plan.Groups))
	remainderSeen := false
	for i, group := range plan.Groups {
		if strings.TrimSpace(group.Name) == "" {
			return nil, fmt.Errorf("group %d has no name: %w", i+1, errPlanStructural)
		}
		hasPaths := len(group.Paths) != 0
		hasRemainder := len(group.Remainder) != 0
		if hasPaths == hasRemainder {
			return nil, fmt.Errorf("group %d discriminator: %w", i+1, errPlanStructural)
		}
		rg := resolvedCommitGroup{name: group.Name, message: group.Message}
		if hasRemainder {
			var remainder bool
			if bytes.Equal(bytes.TrimSpace(group.Remainder), []byte("null")) ||
				json.Unmarshal(group.Remainder, &remainder) != nil || !remainder ||
				remainderSeen || i != len(plan.Groups)-1 {
				return nil, fmt.Errorf("group %d remainder: %w", i+1, errPlanStructural)
			}
			remainderSeen = true
			for _, c := range changes {
				if !assigned[c.path] {
					rg.changes = append(rg.changes, c)
					assigned[c.path] = true
				}
			}
		} else {
			if bytes.Equal(bytes.TrimSpace(group.Paths), []byte("null")) {
				return nil, fmt.Errorf("group %d paths: %w", i+1, errPlanStructural)
			}
			var paths []string
			if err := json.Unmarshal(group.Paths, &paths); err != nil || len(paths) == 0 {
				return nil, fmt.Errorf("group %d paths: %w", i+1, errPlanStructural)
			}
			for _, p := range paths {
				if err := validateCommitPlanPath(p, pol); err != nil {
					return nil, fmt.Errorf("group %d path: %w", i+1, err)
				}
				c, ok := byPath[p]
				if !ok || assigned[p] {
					return nil, fmt.Errorf("group %d path is absent or duplicated: %w", i+1, errPlanStructural)
				}
				assigned[p] = true
				rg.changes = append(rg.changes, c)
			}
		}
		resolved = append(resolved, rg)
	}
	if len(assigned) != len(changes) {
		return nil, fmt.Errorf("commit plan does not exactly cover the derived change set: %w", errPlanStructural)
	}
	if err := validateCommitPlanOrder(resolved, base); err != nil {
		return nil, err
	}
	return resolved, nil
}

func validateCommitPlanPath(p string, pol Policy) error {
	if p == "." || !fs.ValidPath(p) || !utf8.ValidString(p) || strings.ContainsRune(p, 0) ||
		int64(len(p)) > pol.MaxPathBytes || strings.Count(p, "/")+1 > pol.MaxPathDepth {
		return fmt.Errorf("path is not canonical or exceeds its cap: %w", errPlanStructural)
	}
	if export.IsCommitPlanNamespacePath(p) ||
		p == export.EvidenceWorkspaceDir || strings.HasPrefix(p, export.EvidenceWorkspaceDir+"/") {
		return fmt.Errorf("path occupies a reserved channel: %w", errPlanStructural)
	}
	for _, comp := range strings.Split(p, "/") {
		if gitUnsafeComponent(comp) {
			return fmt.Errorf("path contains git metadata: %w", errPlanStructural)
		}
	}
	return nil
}

func validateCommitPlanOrder(groups []resolvedCommitGroup, base map[string]treeEntry) error {
	state := make(map[string]treeEntry, len(base))
	for p, e := range base {
		state[p] = e
	}
	for i, group := range groups {
		for _, c := range group.changes {
			if c.kind == ChangeDeleted {
				delete(state, c.path)
			} else {
				state[c.path] = treeEntry{mode: c.mode, oid: c.oid}
			}
		}
		if err := validateTreePathSet(state); err != nil {
			return fmt.Errorf("group %d creates an invalid intermediate tree: %w: %w", i+1, err, errPlanStructural)
		}
	}
	return nil
}

func validateTreePathSet(state map[string]treeEntry) error {
	paths := make([]string, 0, len(state))
	for p := range state {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	exact := make(map[string]bool, len(paths))
	folded := make(map[string]bool, len(paths))
	for _, p := range paths {
		exact[p] = true
		fold := foldPath(p)
		if folded[fold] {
			return fmt.Errorf("paths collide under case/normalization folding")
		}
		folded[fold] = true
	}
	for _, p := range paths {
		for dir := parentDir(p); dir != ""; dir = parentDir(dir) {
			if exact[dir] {
				return fmt.Errorf("path is both a file and directory")
			}
		}
		fold := foldPath(p)
		// Consult the complete folded leaf set, not only earlier byte-sorted
		// paths. NFC/NFD normalization can sort a descendant before its folded
		// parent even though the parent must be rejected as a file/directory
		// collision on the target checkout.
		for dir := parentDir(fold); dir != ""; dir = parentDir(dir) {
			if folded[dir] {
				return fmt.Errorf("paths collide under case/normalization folding")
			}
		}
	}
	return nil
}

// preboundCommitPlan counts every case-folded groups array and every paths
// array within it, duplicate keys included, before typed decoding allocates the
// corresponding slices. It also pins version as the first top-level member.
func preboundCommitPlan(raw []byte, maxGroups, maxPaths int) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	open, err := dec.Token()
	if err != nil || open != json.Delim('{') || !dec.More() {
		return fmt.Errorf("commit plan root: %w", errPlanStructural)
	}
	key, err := dec.Token()
	if err != nil || !strings.EqualFold(fmt.Sprint(key), "version") {
		return fmt.Errorf("commit plan version is not first: %w", errPlanStructural)
	}
	version, err := dec.Token()
	if err != nil || version != commitPlanVersion {
		return fmt.Errorf("commit plan version: %w", errPlanStructural)
	}
	groups, paths := 0, 0
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return fmt.Errorf("commit plan token: %w", errPlanStructural)
		}
		if !strings.EqualFold(fmt.Sprint(key), "groups") {
			if err := skipJSONValue(dec); err != nil {
				return fmt.Errorf("commit plan value: %w", errPlanStructural)
			}
			continue
		}
		open, err := dec.Token()
		if err != nil || open != json.Delim('[') {
			return fmt.Errorf("commit plan groups is not an array: %w", errPlanStructural)
		}
		for dec.More() {
			groups++
			if groups > maxGroups {
				return fmt.Errorf("commit plan group cap exceeded: %w", errPlanStructural)
			}
			if err := preboundCommitPlanGroup(dec, &paths, maxPaths); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil {
			return fmt.Errorf("commit plan groups: %w", errPlanStructural)
		}
	}
	if _, err := dec.Token(); err != nil {
		return fmt.Errorf("commit plan root: %w", errPlanStructural)
	}
	return nil
}

func preboundCommitPlanGroup(dec *json.Decoder, paths *int, maxPaths int) error {
	open, err := dec.Token()
	if err != nil || open != json.Delim('{') {
		return fmt.Errorf("commit plan group is not an object: %w", errPlanStructural)
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return fmt.Errorf("commit plan group: %w", errPlanStructural)
		}
		if !strings.EqualFold(fmt.Sprint(key), "paths") {
			if err := skipJSONValue(dec); err != nil {
				return fmt.Errorf("commit plan group value: %w", errPlanStructural)
			}
			continue
		}
		open, err := dec.Token()
		if err != nil {
			return fmt.Errorf("commit plan paths: %w", errPlanStructural)
		}
		if open != json.Delim('[') {
			if d, ok := open.(json.Delim); ok && (d == '{' || d == '[') {
				_ = skipUntilJSONClose(dec)
			}
			continue
		}
		for dec.More() {
			if err := skipJSONValue(dec); err != nil {
				return fmt.Errorf("commit plan path: %w", errPlanStructural)
			}
			(*paths)++
			if *paths > maxPaths {
				return fmt.Errorf("commit plan path cap exceeded: %w", errPlanStructural)
			}
		}
		if _, err := dec.Token(); err != nil {
			return fmt.Errorf("commit plan paths: %w", errPlanStructural)
		}
	}
	if _, err := dec.Token(); err != nil {
		return fmt.Errorf("commit plan group: %w", errPlanStructural)
	}
	return nil
}

func preflightCommitPlanBase(base map[string]treeEntry) error {
	for p := range base {
		if export.IsCommitPlanNamespacePath(p) {
			return fmt.Errorf("trusted base tracks reserved commit-plan namespace: %w", ErrCommitPlanCollision)
		}
	}
	return nil
}
