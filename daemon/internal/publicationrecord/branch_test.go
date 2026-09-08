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
	for _, version := range []int{IntentFormatLegacy, IntentFormatHistory, IntentFormatCurrent} {
		intent.FormatVersion = version
		intent.Branch = ""
		want := BranchName(intent.Identity)
		if version == IntentFormatCurrent {
			intent.Branch = "feat/meaningful-task"
			want = intent.Branch
		}
		payload, err := intent.Encode()
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecodeIntent(payload)
		if err != nil || got != intent || ExpectedBranch(got) != want {
			t.Fatalf("format %d round trip = %+v, %v", version, got, err)
		}
		if version != IntentFormatCurrent {
			intent.Branch = "feat/meaningful-task"
		} else {
			intent.Branch = ""
		}
		payload, err = json.Marshal(intent)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeIntent(payload); err == nil {
			t.Fatalf("format %d accepted wrong branch shape", version)
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
