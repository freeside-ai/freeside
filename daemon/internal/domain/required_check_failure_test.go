package domain_test

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

func requiredCheckFailure() domain.RequiredCheckFailure {
	return domain.RequiredCheckFailure{
		Version: domain.RequiredCheckFailureVersion, RunID: "run-1", Repo: "owner/repo", PRNumber: 7,
		HeadSHA: strings.Repeat("a", 40),
		Jobs: []domain.CheckFailureJob{
			{Name: "daemon / linux (test)", JobID: 11, WorkflowRunID: 5, Attempt: 1, ExcerptDigest: domain.Digest("sha256:" + strings.Repeat("1", 64))},
			{Name: "app / test", JobID: 12, WorkflowRunID: 6, Attempt: 2, ExcerptDigest: domain.Digest("sha256:" + strings.Repeat("2", 64))},
		},
	}
}

func TestRequiredCheckFailureRecord(t *testing.T) {
	valid := requiredCheckFailure()
	body, digest, err := valid.Encode()
	if err != nil {
		t.Fatal(err)
	}
	indented, err := json.MarshalIndent(valid, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "required-check-failure-v1", indented)
	decoded, err := domain.DecodeRequiredCheckFailure(body)
	if err != nil || decoded.Key() != valid.Key() {
		t.Fatalf("round trip: %#v, %v", decoded, err)
	}
	if _, err := domain.DecodeRequiredCheckFailure(indented); err == nil {
		t.Fatal("noncanonical record accepted")
	}
	if !strings.HasPrefix(string(digest), "sha256:") {
		t.Fatalf("digest %q", digest)
	}
	if want := "required-check-failure/run-1/" + valid.HeadSHA + "/5.1,6.2"; valid.Key() != want {
		t.Fatalf("key %q, want %q", valid.Key(), want)
	}
	for name, mutate := range map[string]func(*domain.RequiredCheckFailure){
		"version":          func(f *domain.RequiredCheckFailure) { f.Version = "v0" },
		"no run":           func(f *domain.RequiredCheckFailure) { f.RunID = "" },
		"no pull request":  func(f *domain.RequiredCheckFailure) { f.PRNumber = 0 },
		"no head":          func(f *domain.RequiredCheckFailure) { f.HeadSHA = "" },
		"repository":       func(f *domain.RequiredCheckFailure) { f.Repo = "owner" },
		"no jobs":          func(f *domain.RequiredCheckFailure) { f.Jobs = nil },
		"too many jobs":    func(f *domain.RequiredCheckFailure) { f.Jobs = append(f.Jobs, f.Jobs...) },
		"job id":           func(f *domain.RequiredCheckFailure) { f.Jobs[0].JobID = 0 },
		"workflow run":     func(f *domain.RequiredCheckFailure) { f.Jobs[0].WorkflowRunID = 0 },
		"attempt":          func(f *domain.RequiredCheckFailure) { f.Jobs[0].Attempt = 0 },
		"excerpt digest":   func(f *domain.RequiredCheckFailure) { f.Jobs[0].ExcerptDigest = "sha256:short" },
		"empty name":       func(f *domain.RequiredCheckFailure) { f.Jobs[0].Name = "" },
		"credential named": func(f *domain.RequiredCheckFailure) { f.Jobs[0].Name = "ghp_" + strings.Repeat("a", 36) },
	} {
		invalid := requiredCheckFailure()
		mutate(&invalid)
		if invalid.Validate() == nil {
			t.Errorf("%s: invalid record accepted", name)
		}
	}
}

// The excerpt keeps the log's tail, bounded, valid UTF-8, and screened: a
// token anywhere in it, including one that straddles the cut, never
// survives as a fragment.
func TestScreenCheckFailureExcerpt(t *testing.T) {
	token := "ghp_" + strings.Repeat("b", 36)
	long := strings.Repeat("ok line\n", 4096) + "FAIL TestThing\n"
	for name, tc := range map[string]struct {
		log  string
		want func(string) bool
	}{
		"tail kept": {long, func(s string) bool {
			return len(s) <= domain.MaxCheckFailureExcerptBytes && strings.HasSuffix(s, "FAIL TestThing\n")
		}},
		"token redacted": {"before " + token + " after\n", func(s string) bool {
			return !strings.Contains(s, "ghp_") && strings.Contains(s, "[redacted]")
		}},
		"token straddles the cut": {token + strings.Repeat("x", domain.MaxCheckFailureExcerptBytes-20), func(s string) bool {
			return !strings.Contains(s, strings.Repeat("b", 8))
		}},
		"invalid UTF-8": {"bad \xff byte", func(s string) bool { return utf8.ValidString(s) }},
		"empty log":     {"", func(s string) bool { return s == "[redacted]" }},
	} {
		got := domain.ScreenCheckFailureExcerpt([]byte(tc.log))
		if !tc.want(got) || domain.ValidateCheckFailureExcerpt(got) != nil {
			t.Errorf("%s: excerpt %q", name, got[max(0, len(got)-80):])
		}
	}
	if domain.ValidateCheckFailureExcerpt("leak "+token) == nil {
		t.Fatal("unscreened excerpt accepted")
	}
}
