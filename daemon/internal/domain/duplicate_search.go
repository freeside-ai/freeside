package domain

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const (
	// DuplicateSearchEncodingVersion tags the canonical encoding; a change to
	// the field set or order is a new version, never a silent re-hash.
	DuplicateSearchEncodingVersion = 1
	// MaxDuplicateSearchCandidates bounds one search's candidate set: one page
	// of the forge's open-issue listing (plan §7 Finding Adjudication).
	MaxDuplicateSearchCandidates = 100
	// MaxDuplicateSearchTitleBytes bounds a stored candidate title. The
	// listing cuts a longer title at a rune boundary before it gets here.
	MaxDuplicateSearchTitleBytes = 256
	// MaxDuplicateSearchReasonBytes bounds the site's stated reason.
	MaxDuplicateSearchReasonBytes = 1024
	// MaxDuplicateSearchBytes bounds a decoded record. A full candidate set
	// whose titles escape at the encoder's worst case stays well inside it.
	MaxDuplicateSearchBytes = 256 << 10

	duplicateSearchHandlePrefix = "candidate-"
)

// DuplicateSearchHandle is the opaque handle the daemon gives the candidate at
// a 1-based position. The site sees handles, never issue numbers, so an
// answer can only name a candidate the daemon listed.
func DuplicateSearchHandle(ordinal int) string {
	return duplicateSearchHandlePrefix + strconv.Itoa(ordinal)
}

// DuplicateSearchCandidate is one open issue a search showed the site: its
// handle, the issue number the handle stands for, and its title as listed.
type DuplicateSearchCandidate struct {
	Handle string `json:"handle"`
	Number int    `json:"number"`
	Title  string `json:"title"`
}

// DuplicateSearch is the immutable, digest-addressed record of one duplicate
// search (plan §7 Finding Adjudication): before a deferred or separate-work
// finding proposes a follow-up filing, the daemon lists the repository's open
// issues and a judgment site answers whether one already tracks the finding.
// It is keyed by the adjudication artifact and the finding, so a revised
// adjudication searches again.
//
// DuplicateOf is empty for "new" or one of the record's own handles. A record
// is the only authority for which issue a duplicate names: the filing gate
// admits a filing only under a "new" record, and a later comment on the
// duplicate reads its target from Candidates through DuplicateOf. A record
// with no candidates made no site call, so it answers "new" and carries no
// confidence or reason; a record with candidates always carries both.
type DuplicateSearch struct {
	EncodingVersion    int                        `json:"encoding_version"`
	RunID              RunID                      `json:"run_id"`
	AdjudicationDigest Digest                     `json:"adjudication_digest"`
	FindingID          FindingID                  `json:"finding_id"`
	Kind               FollowUpSourceKind         `json:"kind"`
	Repository         FollowUpFilingRepository   `json:"repository"`
	CandidateLimit     int                        `json:"candidate_limit"`
	Truncated          bool                       `json:"truncated"`
	Candidates         []DuplicateSearchCandidate `json:"candidates"`
	DuplicateOf        string                     `json:"duplicate_of"`
	Confidence         AdjudicationConfidence     `json:"confidence"`
	Reason             string                     `json:"reason"`
	CreatedAt          time.Time                  `json:"created_at"`
	Digest             Digest                     `json:"digest"`
}

// canonicalDuplicateSearch is the digest preimage: every field but Digest, in
// the record's own order.
type canonicalDuplicateSearch struct {
	EncodingVersion    int                        `json:"encoding_version"`
	RunID              RunID                      `json:"run_id"`
	AdjudicationDigest Digest                     `json:"adjudication_digest"`
	FindingID          FindingID                  `json:"finding_id"`
	Kind               FollowUpSourceKind         `json:"kind"`
	Repository         FollowUpFilingRepository   `json:"repository"`
	CandidateLimit     int                        `json:"candidate_limit"`
	Truncated          bool                       `json:"truncated"`
	Candidates         []DuplicateSearchCandidate `json:"candidates"`
	DuplicateOf        string                     `json:"duplicate_of"`
	Confidence         AdjudicationConfidence     `json:"confidence"`
	Reason             string                     `json:"reason"`
	CreatedAt          time.Time                  `json:"created_at"`
}

// DuplicateSearchInput is the caller-supplied content of a record. The
// encoding version and digest are omitted so no input path can set them.
type DuplicateSearchInput struct {
	RunID              RunID
	AdjudicationDigest Digest
	FindingID          FindingID
	Kind               FollowUpSourceKind
	Repository         FollowUpFilingRepository
	CandidateLimit     int
	Truncated          bool
	Candidates         []DuplicateSearchCandidate
	DuplicateOf        string
	Confidence         AdjudicationConfidence
	Reason             string
	CreatedAt          time.Time
}

// NewDuplicateSearch builds one record. It detaches the candidate list,
// computes the content digest, and validates. CreatedAt must be a UTC instant.
func NewDuplicateSearch(input DuplicateSearchInput) (DuplicateSearch, error) {
	// A nil input list becomes the empty list here, which Validate requires.
	candidates := append([]DuplicateSearchCandidate{}, input.Candidates...)
	search := DuplicateSearch{
		EncodingVersion:    DuplicateSearchEncodingVersion,
		RunID:              input.RunID,
		AdjudicationDigest: input.AdjudicationDigest,
		FindingID:          input.FindingID,
		Kind:               input.Kind,
		Repository:         input.Repository,
		CandidateLimit:     input.CandidateLimit,
		Truncated:          input.Truncated,
		Candidates:         candidates,
		DuplicateOf:        input.DuplicateOf,
		Confidence:         input.Confidence,
		Reason:             input.Reason,
		CreatedAt:          input.CreatedAt,
	}
	digest, err := search.ComputeDigest()
	if err != nil {
		return DuplicateSearch{}, err
	}
	search.Digest = digest
	if err := search.Validate(); err != nil {
		return DuplicateSearch{}, err
	}
	return search, nil
}

// IsNew reports whether the search found no open issue tracking the finding.
func (s DuplicateSearch) IsNew() bool { return s.DuplicateOf == "" }

// Validate is the handle rule and the content-address backstop for a
// constructed or reconstructed record. Handles are exactly candidate-1..N in
// order, issue numbers are positive and distinct, DuplicateOf is empty or one
// of the record's own handles, and the candidate count stays within the
// recorded limit, which stays within MaxDuplicateSearchCandidates.
func (s DuplicateSearch) Validate() error {
	if s.EncodingVersion != DuplicateSearchEncodingVersion {
		return fmt.Errorf("duplicate search encoding_version %d: %w", s.EncodingVersion, ErrDuplicateSearchInconsistent)
	}
	if s.RunID == "" {
		return fmt.Errorf("duplicate search run_id: %w", ErrEmptyID)
	}
	if s.FindingID == "" {
		return fmt.Errorf("duplicate search finding_id: %w", ErrEmptyID)
	}
	// json.Marshal rewrites invalid bytes to U+FFFD, so an unguarded field
	// would let the stored record differ from the submitted content while
	// still validating against the rewritten form.
	for _, field := range []struct{ label, text string }{
		{"run_id", string(s.RunID)}, {"finding_id", string(s.FindingID)},
	} {
		if !utf8.ValidString(field.text) {
			return fmt.Errorf("duplicate search %s %q: %w", field.label, field.text, ErrDuplicateSearchInconsistent)
		}
	}
	if !contentaddr.Valid(string(s.AdjudicationDigest)) {
		return fmt.Errorf("duplicate search adjudication_digest %q: %w", s.AdjudicationDigest, ErrDuplicateSearchInconsistent)
	}
	if !s.Kind.valid() {
		return fmt.Errorf("duplicate search kind %q: %w", s.Kind, ErrDuplicateSearchInconsistent)
	}
	if err := s.Repository.Validate(); err != nil {
		return fmt.Errorf("duplicate search repository: %w", err)
	}
	if s.CandidateLimit < 1 || s.CandidateLimit > MaxDuplicateSearchCandidates {
		return fmt.Errorf("duplicate search candidate_limit %d: %w", s.CandidateLimit, ErrDuplicateSearchInconsistent)
	}
	if s.Candidates == nil || len(s.Candidates) > s.CandidateLimit {
		return fmt.Errorf("duplicate search candidates over limit %d: %w", s.CandidateLimit, ErrDuplicateSearchInconsistent)
	}
	numbers := make(map[int]bool, len(s.Candidates))
	for index, candidate := range s.Candidates {
		if candidate.Handle != DuplicateSearchHandle(index+1) {
			return fmt.Errorf("duplicate search candidate %d handle %q: %w",
				index+1, candidate.Handle, ErrDuplicateSearchInconsistent)
		}
		if candidate.Number <= 0 || numbers[candidate.Number] {
			return fmt.Errorf("duplicate search candidate %q number %d: %w",
				candidate.Handle, candidate.Number, ErrDuplicateSearchInconsistent)
		}
		numbers[candidate.Number] = true
		if err := validateDuplicateSearchText(candidate.Title, MaxDuplicateSearchTitleBytes); err != nil {
			return fmt.Errorf("duplicate search candidate %q title: %w", candidate.Handle, err)
		}
	}
	if _, ok := s.withHandle(s.DuplicateOf); s.DuplicateOf != "" && !ok {
		return fmt.Errorf("duplicate search duplicate_of %q names no candidate: %w",
			s.DuplicateOf, ErrDuplicateSearchInconsistent)
	}
	if len(s.Candidates) == 0 {
		// No candidates means no site call, so nothing judged the record.
		if s.Confidence != "" || s.Reason != "" {
			return fmt.Errorf("duplicate search without candidates carries a judgment: %w", ErrDuplicateSearchInconsistent)
		}
	} else {
		if !s.Confidence.valid() {
			return fmt.Errorf("duplicate search confidence %q: %w", s.Confidence, ErrInvalidAdjudicationConfidence)
		}
		if strings.TrimSpace(s.Reason) == "" {
			return fmt.Errorf("duplicate search reason: %w", ErrEmptyField)
		}
		if err := validateDuplicateSearchText(s.Reason, MaxDuplicateSearchReasonBytes); err != nil {
			return fmt.Errorf("duplicate search reason: %w", err)
		}
	}
	if s.CreatedAt.IsZero() {
		return fmt.Errorf("duplicate search created_at: %w", ErrMissingTimestamp)
	}
	if s.CreatedAt.Location() != time.UTC {
		return fmt.Errorf("duplicate search created_at: %w", ErrTimestampNotUTC)
	}
	if !contentaddr.Valid(string(s.Digest)) {
		return fmt.Errorf("duplicate search digest %q: %w", s.Digest, ErrDuplicateSearchDigestMismatch)
	}
	computed, err := s.ComputeDigest()
	if err != nil {
		return err
	}
	if s.Digest != computed {
		return fmt.Errorf("duplicate search digest %q, content resolves to %q: %w",
			s.Digest, computed, ErrDuplicateSearchDigestMismatch)
	}
	return nil
}

// DuplicateIssue returns the issue number DuplicateOf names, and false for a
// "new" record. It reads only the record's own candidates.
func (s DuplicateSearch) DuplicateIssue() (int, bool) {
	if s.DuplicateOf == "" {
		return 0, false
	}
	candidate, ok := s.withHandle(s.DuplicateOf)
	return candidate.Number, ok
}

func (s DuplicateSearch) withHandle(handle string) (DuplicateSearchCandidate, bool) {
	for _, candidate := range s.Candidates {
		if candidate.Handle == handle {
			return candidate, true
		}
	}
	return DuplicateSearchCandidate{}, false
}

// validateDuplicateSearchText bounds a stored text field: valid UTF-8, no
// control characters, and at most limit bytes.
func validateDuplicateSearchText(text string, limit int) error {
	if len(text) > limit {
		return fmt.Errorf("%d bytes over %d: %w", len(text), limit, ErrDuplicateSearchInconsistent)
	}
	if !utf8.ValidString(text) || strings.ContainsFunc(text, unicode.IsControl) {
		return fmt.Errorf("invalid text: %w", ErrDuplicateSearchInconsistent)
	}
	return nil
}

// ComputeDigest hashes the canonical encoding, which excludes only the Digest
// field. Struct field order is part of the contract and is pinned by a golden.
func (s DuplicateSearch) ComputeDigest() (Digest, error) {
	body, err := json.Marshal(canonicalDuplicateSearch{
		EncodingVersion:    s.EncodingVersion,
		RunID:              s.RunID,
		AdjudicationDigest: s.AdjudicationDigest,
		FindingID:          s.FindingID,
		Kind:               s.Kind,
		Repository:         s.Repository,
		CandidateLimit:     s.CandidateLimit,
		Truncated:          s.Truncated,
		Candidates:         s.Candidates,
		DuplicateOf:        s.DuplicateOf,
		Confidence:         s.Confidence,
		Reason:             s.Reason,
		CreatedAt:          s.CreatedAt,
	})
	if err != nil {
		return "", fmt.Errorf("duplicate search canonical encoding: %w", err)
	}
	return Digest(contentaddr.Sum(body)), nil
}

// Encode emits the validated canonical persisted form.
func (s DuplicateSearch) Encode() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("duplicate search encode: %w", err)
	}
	return body, nil
}

// DecodeDuplicateSearch rejects oversized, unknown-field, invalid-UTF8, and
// trailing-data payloads before revalidating the handle rule and the content
// address.
func DecodeDuplicateSearch(body []byte) (DuplicateSearch, error) {
	var search DuplicateSearch
	if err := strictjson.Decode(body, &search, strictjson.RejectInvalidUTF8, MaxDuplicateSearchBytes); err != nil {
		return DuplicateSearch{}, fmt.Errorf("duplicate search decode: %w", err)
	}
	if err := search.Validate(); err != nil {
		return DuplicateSearch{}, err
	}
	return search, nil
}
