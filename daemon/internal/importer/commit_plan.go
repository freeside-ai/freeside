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
	"github.com/freeside-ai/freeside/daemon/internal/pathfold"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const commitPlanVersion = "freeside.commit-plan/v1"

// MaxReportedUncoveredPaths bounds how many uncovered changed paths an
// exact-cover structural failure lists in one CheckCommitPlan report.
const MaxReportedUncoveredPaths = 50

var errPlanStructural = errors.New("commit plan is structurally invalid")

// planStructuralError attaches a stable PlanProblemCode and optional location
// to a commit-plan structural failure while preserving errors.Is matching on
// errPlanStructural for Import.
type planStructuralError struct {
	code    PlanProblemCode
	group   int
	path    string
	paths   []string
	total   int
	message string
	cause   error
}

func (e *planStructuralError) Error() string {
	return e.message
}

func (e *planStructuralError) Is(target error) bool {
	return target == errPlanStructural
}

func (e *planStructuralError) Unwrap() error {
	return e.cause
}

func newPlanStructuralError(code PlanProblemCode, group int, path, message string, cause error) error {
	return &planStructuralError{
		code:    code,
		group:   group,
		path:    path,
		message: message,
		cause:   cause,
	}
}

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
	// Keep invalid UTF-8 ahead of the structural allocation gates, matching
	// this boundary's established classification. strictjson rechecks it.
	if !utf8.Valid(raw) {
		return nil, newPlanStructuralError(PlanProblemInvalidUTF8, 0, "", "commit plan is not UTF-8", nil)
	}
	if err := preboundCommitPlan(raw, pol.MaxCommitPlanGroups, pol.MaxEntries); err != nil {
		return nil, err
	}
	var plan commitPlan
	if err := strictjson.Decode(
		raw, &plan, strictjson.RejectInvalidUTF8, strictjson.Limit(pol.MaxCommitPlanBytes),
	); err != nil {
		if errors.Is(err, strictjson.ErrInvalidUTF8) {
			return nil, newPlanStructuralError(PlanProblemInvalidUTF8, 0, "", "commit plan is not UTF-8", err)
		}
		if errors.Is(err, strictjson.ErrTrailingData) {
			return nil, newPlanStructuralError(PlanProblemTrailingData, 0, "", "commit plan carries trailing content", err)
		}
		return nil, newPlanStructuralError(PlanProblemInvalidJSON, 0, "", fmt.Sprintf("decode commit plan: %v", err), err)
	}
	if plan.Version != commitPlanVersion {
		return nil, newPlanStructuralError(PlanProblemVersionUnsupported, 0, "", fmt.Sprintf("commit plan version %q", plan.Version), nil)
	}
	if len(plan.Groups) == 0 {
		return nil, newPlanStructuralError(PlanProblemGroupsEmpty, 0, "", "commit plan has 0 groups", nil)
	}
	if len(plan.Groups) > pol.MaxCommitPlanGroups {
		return nil, newPlanStructuralError(PlanProblemGroupCapExceeded, 0, "", fmt.Sprintf("commit plan has %d groups", len(plan.Groups)), nil)
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
		groupNum := i + 1
		if strings.TrimSpace(group.Name) == "" {
			return nil, newPlanStructuralError(PlanProblemGroupNameEmpty, groupNum, "", fmt.Sprintf("group %d has no name", groupNum), nil)
		}
		hasPaths := len(group.Paths) != 0
		hasRemainder := len(group.Remainder) != 0
		if hasPaths == hasRemainder {
			return nil, newPlanStructuralError(PlanProblemGroupDiscriminator, groupNum, "", fmt.Sprintf("group %d discriminator", groupNum), nil)
		}
		rg := resolvedCommitGroup{name: group.Name, message: group.Message}
		if hasRemainder {
			var remainder bool
			if bytes.Equal(bytes.TrimSpace(group.Remainder), []byte("null")) ||
				json.Unmarshal(group.Remainder, &remainder) != nil || !remainder ||
				remainderSeen || i != len(plan.Groups)-1 {
				return nil, newPlanStructuralError(PlanProblemGroupRemainderInvalid, groupNum, "", fmt.Sprintf("group %d remainder", groupNum), nil)
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
				return nil, newPlanStructuralError(PlanProblemGroupPathsInvalid, groupNum, "", fmt.Sprintf("group %d paths", groupNum), nil)
			}
			var paths []string
			if err := json.Unmarshal(group.Paths, &paths); err != nil || len(paths) == 0 {
				return nil, newPlanStructuralError(PlanProblemGroupPathsInvalid, groupNum, "", fmt.Sprintf("group %d paths", groupNum), nil)
			}
			for _, p := range paths {
				if err := validateCommitPlanPath(groupNum, p, pol); err != nil {
					return nil, err
				}
				c, ok := byPath[p]
				if !ok {
					return nil, newPlanStructuralError(PlanProblemPathNotInChanges, groupNum, p, fmt.Sprintf("group %d path is not in the derived change set", groupNum), nil)
				}
				if assigned[p] {
					return nil, newPlanStructuralError(PlanProblemPathDuplicated, groupNum, p, fmt.Sprintf("group %d path is duplicated", groupNum), nil)
				}
				assigned[p] = true
				rg.changes = append(rg.changes, c)
			}
		}
		resolved = append(resolved, rg)
	}
	if len(assigned) != len(changes) {
		var uncovered []string
		totalUncovered := 0
		for _, c := range changes {
			if !assigned[c.path] {
				totalUncovered++
				if len(uncovered) < MaxReportedUncoveredPaths {
					uncovered = append(uncovered, c.path)
				}
			}
		}
		return nil, &planStructuralError{
			code:    PlanProblemPathUncovered,
			paths:   uncovered,
			total:   totalUncovered,
			message: "commit plan does not exactly cover the derived change set",
		}
	}
	if err := validateCommitPlanOrder(resolved, base); err != nil {
		return nil, err
	}
	return resolved, nil
}

func validateCommitPlanPath(groupNum int, p string, pol Policy) error {
	if p == "." || !fs.ValidPath(p) || !utf8.ValidString(p) || strings.ContainsRune(p, 0) ||
		int64(len(p)) > pol.MaxPathBytes || strings.Count(p, "/")+1 > pol.MaxPathDepth {
		reported := ""
		if utf8.ValidString(p) && !strings.ContainsRune(p, 0) && int64(len(p)) <= pol.MaxPathBytes {
			reported = p
		}
		return newPlanStructuralError(PlanProblemPathInvalid, groupNum, reported, fmt.Sprintf("group %d path is not canonical or exceeds its cap", groupNum), nil)
	}
	if export.IsCommitPlanNamespacePath(p) ||
		p == export.EvidenceWorkspaceDir || strings.HasPrefix(p, export.EvidenceWorkspaceDir+"/") {
		return newPlanStructuralError(PlanProblemPathReserved, groupNum, p, fmt.Sprintf("group %d path occupies a reserved channel", groupNum), nil)
	}
	for _, comp := range strings.Split(p, "/") {
		if GitUnsafeComponent(comp) {
			return newPlanStructuralError(PlanProblemPathGitMetadata, groupNum, p, fmt.Sprintf("group %d path contains git metadata", groupNum), nil)
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
		if se := validateTreePathSet(state); se != nil {
			return newPlanStructuralError(
				se.code, i+1, se.path,
				fmt.Sprintf("group %d creates an invalid intermediate tree: %s", i+1, se.message),
				nil,
			)
		}
	}
	return nil
}

func validateTreePathSet(state map[string]treeEntry) *planStructuralError {
	paths := make([]string, 0, len(state))
	for p := range state {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	exact := make(map[string]bool, len(paths))
	folded := make(map[string]bool, len(paths))
	for _, p := range paths {
		exact[p] = true
		fold := pathfold.FoldPath(p)
		if folded[fold] {
			return &planStructuralError{
				code:    PlanProblemIntermediateTreeCollision,
				path:    p,
				message: "paths collide under case/normalization folding",
			}
		}
		folded[fold] = true
	}
	for _, p := range paths {
		for dir := parentDir(p); dir != ""; dir = parentDir(dir) {
			if exact[dir] {
				return &planStructuralError{
					code:    PlanProblemIntermediateTreeFileDir,
					path:    p,
					message: "path is both a file and directory",
				}
			}
		}
		fold := pathfold.FoldPath(p)
		// Consult the complete folded leaf set, not only earlier byte-sorted
		// paths. NFC/NFD normalization can sort a descendant before its folded
		// parent even though the parent must be rejected as a file/directory
		// collision on the target checkout.
		for dir := parentDir(fold); dir != ""; dir = parentDir(dir) {
			if folded[dir] {
				return &planStructuralError{
					code:    PlanProblemIntermediateTreeCollision,
					path:    p,
					message: "paths collide under case/normalization folding",
				}
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
		return newPlanStructuralError(PlanProblemInvalidJSON, 0, "", "commit plan root", err)
	}
	key, err := dec.Token()
	if err != nil || !strings.EqualFold(fmt.Sprint(key), "version") {
		return newPlanStructuralError(PlanProblemVersionNotFirst, 0, "", "commit plan version is not first", err)
	}
	version, err := dec.Token()
	if err != nil || version != commitPlanVersion {
		return newPlanStructuralError(PlanProblemVersionUnsupported, 0, "", "commit plan version", err)
	}
	groups, paths := 0, 0
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return newPlanStructuralError(PlanProblemInvalidJSON, 0, "", "commit plan token", err)
		}
		if !strings.EqualFold(fmt.Sprint(key), "groups") {
			if err := skipJSONValue(dec); err != nil {
				return newPlanStructuralError(PlanProblemInvalidJSON, 0, "", "commit plan value", err)
			}
			continue
		}
		open, err := dec.Token()
		if err != nil || open != json.Delim('[') {
			return newPlanStructuralError(PlanProblemGroupsInvalid, 0, "", "commit plan groups is not an array", err)
		}
		for dec.More() {
			groups++
			if groups > maxGroups {
				return newPlanStructuralError(PlanProblemGroupCapExceeded, 0, "", "commit plan group cap exceeded", nil)
			}
			if err := preboundCommitPlanGroup(dec, groups, &paths, maxPaths); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil {
			return newPlanStructuralError(PlanProblemInvalidJSON, 0, "", "commit plan groups", err)
		}
	}
	if _, err := dec.Token(); err != nil {
		return newPlanStructuralError(PlanProblemInvalidJSON, 0, "", "commit plan root", err)
	}
	return nil
}

func preboundCommitPlanGroup(dec *json.Decoder, groupNum int, paths *int, maxPaths int) error {
	open, err := dec.Token()
	if err != nil || open != json.Delim('{') {
		return newPlanStructuralError(PlanProblemGroupInvalid, groupNum, "", "commit plan group is not an object", err)
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return newPlanStructuralError(PlanProblemInvalidJSON, groupNum, "", "commit plan group", err)
		}
		if !strings.EqualFold(fmt.Sprint(key), "paths") {
			if err := skipJSONValue(dec); err != nil {
				return newPlanStructuralError(PlanProblemInvalidJSON, groupNum, "", "commit plan group value", err)
			}
			continue
		}
		open, err := dec.Token()
		if err != nil {
			return newPlanStructuralError(PlanProblemInvalidJSON, groupNum, "", "commit plan paths", err)
		}
		if open != json.Delim('[') {
			if d, ok := open.(json.Delim); ok && (d == '{' || d == '[') {
				_ = skipUntilJSONClose(dec)
			}
			continue
		}
		for dec.More() {
			if err := skipJSONValue(dec); err != nil {
				return newPlanStructuralError(PlanProblemInvalidJSON, groupNum, "", "commit plan path", err)
			}
			(*paths)++
			if *paths > maxPaths {
				return newPlanStructuralError(PlanProblemPathCapExceeded, groupNum, "", "commit plan path cap exceeded", nil)
			}
		}
		if _, err := dec.Token(); err != nil {
			return newPlanStructuralError(PlanProblemInvalidJSON, groupNum, "", "commit plan paths", err)
		}
	}
	if _, err := dec.Token(); err != nil {
		return newPlanStructuralError(PlanProblemInvalidJSON, groupNum, "", "commit plan group", err)
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
