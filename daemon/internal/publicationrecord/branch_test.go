package publicationrecord

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestBranchFormatsAndReconstruction(t *testing.T) {
	t.Parallel()
	intent := Intent{
		Identity:     domain.Digest("sha256:" + strings.Repeat("a", 64)),
		InvocationID: "publish-1", Repo: "owner/repo", BaseRef: "main", SourceHeadSHA: "head",
		AuthorizationID: domain.Digest("sha256:" + strings.Repeat("b", 64)),
	}
	intent.FormatVersion = IntentFormatCurrent
	intent.Branch = "feat/meaningful-task"
	payload, err := intent.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeIntent(payload)
	if err != nil || got != intent || ExpectedBranch(got) != intent.Branch {
		t.Fatalf("current format round trip = %+v, %v", got, err)
	}
	unnamed := intent
	unnamed.Branch = ""
	payload, err = json.Marshal(unnamed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeIntent(payload); err == nil {
		t.Fatal("current format accepted an intent that names no branch")
	}
	// Formats 1 and 2 named no branch and are no longer read (#3323), with
	// or without one.
	for _, version := range []int{1, 2} {
		for _, branch := range []string{"", BranchName(intent.Identity)} {
			old := intent
			old.FormatVersion, old.Branch = version, branch
			payload, err := json.Marshal(old)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeIntent(payload); err == nil || !strings.Contains(err.Error(), "unsupported format version") {
				t.Fatalf("format %d with branch %q decoded: %v", version, branch, err)
			}
		}
	}
}

func TestDeclaredBranchPolicy(t *testing.T) {
	t.Parallel()
	for _, branch := range []string{"feat/meaningful-task", "fix/token-detection", "release"} {
		if err := ValidateDeclaredBranch(branch, "main"); err != nil {
			t.Fatal(err)
		}
	}
	for _, branch := range []string{"", "main", "refs/heads/task", "freeside/task", "a..b", "a//b", "a/.b", "a/b.lock", "a b", "-branch", "a@{b", "a:b", strings.Repeat("a", 256)} {
		if err := ValidateDeclaredBranch(branch, "main"); err == nil || !strings.Contains(err.Error(), "branch") {
			t.Fatalf("branch %q: %v", branch, err)
		}
	}
}
