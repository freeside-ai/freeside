package domain

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/pathfold"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const (
	ScopeConflictEncodingVersion = "freeside.scope-conflict/v1"
	MaxScopeConflictBytes        = strictjson.Limit(64 << 10)
	MaxScopeConflictPaths        = 8
	MaxScopeConflictPathBytes    = 1 << 10
)

// ScopeConflict accompanies a completed candidate whose required work extends
// beyond its approved paths. It grants no permission to change those paths.
type ScopeConflict struct {
	Version  string   `json:"version"`
	Paths    []string `json:"paths"`
	Decision Decision `json:"decision"`
}

func validateScopePaths(paths []string) error {
	if len(paths) == 0 || len(paths) > MaxScopeConflictPaths {
		return fmt.Errorf("scope conflict path count: %w", ErrCardFactInconsistent)
	}
	for index, p := range paths {
		if len(p) > MaxScopeConflictPathBytes || !export.ValidCanonicalPath(p) || slices.Contains(paths[:index], p) {
			return fmt.Errorf("scope conflict path: %w", ErrCardFactInconsistent)
		}
	}
	return nil
}

func (c ScopeConflict) Validate() error {
	if c.Version != ScopeConflictEncodingVersion {
		return fmt.Errorf("scope conflict version: %w", ErrCardFactInconsistent)
	}
	if err := validateScopePaths(c.Paths); err != nil {
		return err
	}
	return c.Decision.Validate()
}

func DecodeScopeConflict(data []byte) (ScopeConflict, error) {
	var out ScopeConflict
	if err := strictjson.Decode(data, &out, strictjson.RejectInvalidUTF8, MaxScopeConflictBytes); err != nil {
		return ScopeConflict{}, err
	}
	if err := out.Validate(); err != nil {
		return ScopeConflict{}, err
	}
	return out, nil
}

func EncodeScopeConflict(c ScopeConflict) ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	if len(body) > int(MaxScopeConflictBytes) {
		return nil, strictjson.ErrLimitExceeded
	}
	return body, nil
}

// ScopeConflictFacts binds the untouched paths to the immutable run policy and
// imported candidate. The reason for the conflict remains an agent claim.
type ScopeConflictFacts struct {
	Paths         []string `json:"paths"`
	DeclaredPaths []string `json:"declared_paths"`
	HeadSHA       string   `json:"head_sha"`
}

func (f ScopeConflictFacts) Validate() error {
	if err := validateScopePaths(f.Paths); err != nil {
		return err
	}
	if f.HeadSHA == "" || len(f.DeclaredPaths) == 0 || !slices.IsSorted(f.DeclaredPaths) {
		return ErrCardFactInconsistent
	}
	for index, p := range f.DeclaredPaths {
		if p == "" || strings.TrimSpace(p) != p || !utf8.ValidString(p) || slices.Contains(f.DeclaredPaths[:index], p) {
			return ErrCardFactInconsistent
		}
	}
	for _, p := range f.Paths {
		// The importer uses this same matcher; domain cannot import importer.
		if pathfold.MatchAny(f.DeclaredPaths, p, false) {
			return ErrCardFactInconsistent
		}
	}
	return nil
}

func cloneScopeConflictFacts(in *ScopeConflictFacts) *ScopeConflictFacts {
	if in == nil {
		return nil
	}
	out := *in
	out.Paths = slices.Clone(in.Paths)
	out.DeclaredPaths = slices.Clone(in.DeclaredPaths)
	return &out
}

// ScopeDecisionFacts records the accepted narrower scope, without claiming the
// omitted obligation is satisfied. It is reconstructed from an accepted command.
type ScopeDecisionFacts struct {
	Paths         []string  `json:"paths"`
	DeclaredPaths []string  `json:"declared_paths"`
	HeadSHA       string    `json:"head_sha"`
	CommandID     string    `json:"command_id"`
	Answer        string    `json:"answer"`
	DecidedAt     time.Time `json:"decided_at"`
}

func (f ScopeDecisionFacts) Validate() error {
	if err := (ScopeConflictFacts{Paths: f.Paths, DeclaredPaths: f.DeclaredPaths, HeadSHA: f.HeadSHA}).Validate(); err != nil {
		return err
	}
	if f.CommandID == "" || strings.TrimSpace(f.Answer) == "" || !utf8.ValidString(f.Answer) || f.DecidedAt.IsZero() {
		return ErrCardFactInconsistent
	}
	return nil
}

func cloneScopeDecisionFacts(in *ScopeDecisionFacts) *ScopeDecisionFacts {
	if in == nil {
		return nil
	}
	out := *in
	out.Paths = slices.Clone(in.Paths)
	out.DeclaredPaths = slices.Clone(in.DeclaredPaths)
	return &out
}
