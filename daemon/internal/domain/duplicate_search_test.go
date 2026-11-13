package domain_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

func duplicateSearchInput(duplicateOf string) domain.DuplicateSearchInput {
	return domain.DuplicateSearchInput{
		RunID:              "run-1",
		AdjudicationDigest: domain.Digest("sha256:" + strings.Repeat("a", 64)),
		FindingID:          "finding-1",
		Kind:               domain.FollowUpSourceDeferredDisposition,
		Repository:         domain.FollowUpFilingRepository{Repo: "acme/widgets", RepositoryID: 42},
		CandidateLimit:     domain.MaxDuplicateSearchCandidates,
		Truncated:          true,
		Candidates: []domain.DuplicateSearchCandidate{
			{Handle: "candidate-1", Number: 17, Title: "Retry the config read on start"},
			{Handle: "candidate-2", Number: 9, Title: "Document the cache bound"},
		},
		DuplicateOf: duplicateOf,
		Confidence:  domain.ConfidenceHigh,
		Reason:      "Issue candidate-1 tracks the same retry.",
		CreatedAt:   time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC),
	}
}

func duplicateSearchFixture(t *testing.T, duplicateOf string) domain.DuplicateSearch {
	t.Helper()
	search, err := domain.NewDuplicateSearch(duplicateSearchInput(duplicateOf))
	if err != nil {
		t.Fatalf("NewDuplicateSearch: %v", err)
	}
	return search
}

// TestDuplicateSearchGolden pins the canonical encoding: a duplicate answer,
// a "new" answer over candidates, and an empty search, which carries no
// judgment. Each fixture is built by the validating constructor, so it
// doubles as a validation-positive case.
func TestDuplicateSearchGolden(t *testing.T) {
	t.Parallel()
	empty := duplicateSearchInput("")
	empty.Candidates, empty.Truncated, empty.Confidence, empty.Reason = nil, false, "", ""
	emptySearch, err := domain.NewDuplicateSearch(empty)
	if err != nil {
		t.Fatalf("NewDuplicateSearch empty: %v", err)
	}
	for name, search := range map[string]domain.DuplicateSearch{
		"duplicate_search_duplicate": duplicateSearchFixture(t, "candidate-1"),
		"duplicate_search_new":       duplicateSearchFixture(t, ""),
		"duplicate_search_empty":     emptySearch,
	} {
		body, err := json.MarshalIndent(search, "", "  ")
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		golden.Assert(t, name, append(body, '\n'))
	}
}

func TestDuplicateSearchRoundTripAndAnswer(t *testing.T) {
	t.Parallel()
	search := duplicateSearchFixture(t, "candidate-1")
	body, err := search.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	decoded, err := domain.DecodeDuplicateSearch(body)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if decoded.Digest != search.Digest || decoded.IsNew() {
		t.Fatalf("decoded %+v, want the duplicate record back", decoded)
	}
	if number, ok := decoded.DuplicateIssue(); !ok || number != 17 {
		t.Fatalf("DuplicateIssue = %d, %v; want 17, true", number, ok)
	}
	fresh := duplicateSearchFixture(t, "")
	if !fresh.IsNew() {
		t.Fatal("a record with no duplicate_of is not new")
	}
	if _, ok := fresh.DuplicateIssue(); ok {
		t.Fatal("a new record names a duplicate issue")
	}
}

// TestNewDuplicateSearchRejects is the handle rule and its neighbours.
func TestNewDuplicateSearchRejects(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		mutate func(*domain.DuplicateSearchInput)
		want   error
	}{
		"duplicate_of outside the candidates": {func(in *domain.DuplicateSearchInput) {
			in.DuplicateOf = "candidate-3"
		}, domain.ErrDuplicateSearchInconsistent},
		"duplicate_of an issue number": {func(in *domain.DuplicateSearchInput) {
			in.DuplicateOf = "17"
		}, domain.ErrDuplicateSearchInconsistent},
		"handles out of order": {func(in *domain.DuplicateSearchInput) {
			in.Candidates[0].Handle, in.Candidates[1].Handle = "candidate-2", "candidate-1"
		}, domain.ErrDuplicateSearchInconsistent},
		"handle not daemon-numbered": {func(in *domain.DuplicateSearchInput) {
			in.Candidates[1].Handle = "issue-9"
		}, domain.ErrDuplicateSearchInconsistent},
		"repeated number": {func(in *domain.DuplicateSearchInput) {
			in.Candidates[1].Number = 17
		}, domain.ErrDuplicateSearchInconsistent},
		"non-positive number": {func(in *domain.DuplicateSearchInput) {
			in.Candidates[1].Number = 0
		}, domain.ErrDuplicateSearchInconsistent},
		"more candidates than the limit": {func(in *domain.DuplicateSearchInput) {
			in.CandidateLimit = 1
		}, domain.ErrDuplicateSearchInconsistent},
		"limit over the maximum": {func(in *domain.DuplicateSearchInput) {
			in.CandidateLimit = domain.MaxDuplicateSearchCandidates + 1
		}, domain.ErrDuplicateSearchInconsistent},
		"zero limit": {func(in *domain.DuplicateSearchInput) {
			in.CandidateLimit = 0
		}, domain.ErrDuplicateSearchInconsistent},
		"title over its bound": {func(in *domain.DuplicateSearchInput) {
			in.Candidates[0].Title = strings.Repeat("x", domain.MaxDuplicateSearchTitleBytes+1)
		}, domain.ErrDuplicateSearchInconsistent},
		"title with a control character": {func(in *domain.DuplicateSearchInput) {
			in.Candidates[0].Title = "two\nlines"
		}, domain.ErrDuplicateSearchInconsistent},
		"title with invalid UTF-8": {func(in *domain.DuplicateSearchInput) {
			in.Candidates[0].Title = "bad \xff byte"
		}, domain.ErrDuplicateSearchInconsistent},
		"reason over its bound": {func(in *domain.DuplicateSearchInput) {
			in.Reason = strings.Repeat("x", domain.MaxDuplicateSearchReasonBytes+1)
		}, domain.ErrDuplicateSearchInconsistent},
		"blank reason": {func(in *domain.DuplicateSearchInput) {
			in.Reason = " "
		}, domain.ErrEmptyField},
		"confidence outside the lattice": {func(in *domain.DuplicateSearchInput) {
			in.Confidence = "certain"
		}, domain.ErrInvalidAdjudicationConfidence},
		"judgment without candidates": {func(in *domain.DuplicateSearchInput) {
			in.Candidates, in.DuplicateOf = nil, ""
		}, domain.ErrDuplicateSearchInconsistent},
		"unknown kind": {func(in *domain.DuplicateSearchInput) {
			in.Kind = "comment"
		}, domain.ErrDuplicateSearchInconsistent},
		"malformed repository": {func(in *domain.DuplicateSearchInput) {
			in.Repository.RepositoryID = 0
		}, domain.ErrNonPositive},
		"malformed adjudication digest": {func(in *domain.DuplicateSearchInput) {
			in.AdjudicationDigest = "sha256:short"
		}, domain.ErrDuplicateSearchInconsistent},
		"empty finding": {func(in *domain.DuplicateSearchInput) {
			in.FindingID = ""
		}, domain.ErrEmptyID},
		"local created_at": {func(in *domain.DuplicateSearchInput) {
			in.CreatedAt = in.CreatedAt.In(time.FixedZone("x", 3600))
		}, domain.ErrTimestampNotUTC},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			input := duplicateSearchInput("candidate-1")
			input.Candidates = append([]domain.DuplicateSearchCandidate{}, input.Candidates...)
			tc.mutate(&input)
			if _, err := domain.NewDuplicateSearch(input); !errors.Is(err, tc.want) {
				t.Fatalf("NewDuplicateSearch error = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestDecodeDuplicateSearchRejectsTamper: a stored body whose duplicate_of
// names no candidate is refused even when its digest is recomputed to match,
// and a body edited without recomputing the digest is refused by the address.
func TestDecodeDuplicateSearchRejectsTamper(t *testing.T) {
	t.Parallel()
	search := duplicateSearchFixture(t, "candidate-1")

	retargeted := search
	retargeted.DuplicateOf = "candidate-7"
	digest, err := retargeted.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	retargeted.Digest = digest
	body, err := json.Marshal(retargeted)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := domain.DecodeDuplicateSearch(body); !errors.Is(err, domain.ErrDuplicateSearchInconsistent) {
		t.Fatalf("retargeted decode error = %v, want ErrDuplicateSearchInconsistent", err)
	}

	renumbered := search
	renumbered.Candidates = append([]domain.DuplicateSearchCandidate{}, search.Candidates...)
	renumbered.Candidates[0].Number = 99
	body, err = json.Marshal(renumbered)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := domain.DecodeDuplicateSearch(body); !errors.Is(err, domain.ErrDuplicateSearchDigestMismatch) {
		t.Fatalf("renumbered decode error = %v, want ErrDuplicateSearchDigestMismatch", err)
	}

	good, err := search.Encode()
	if err != nil {
		t.Fatal(err)
	}
	unknown := strings.Replace(string(good), `"truncated"`, `"extra":1,"truncated"`, 1)
	if _, err := domain.DecodeDuplicateSearch([]byte(unknown)); err == nil {
		t.Fatal("decode accepted an unknown field")
	}
}
