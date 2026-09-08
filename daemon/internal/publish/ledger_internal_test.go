package publish

import (
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestIntentsCompatibleAllowsOnlyLegacyHistoryDigestUpgrade(t *testing.T) {
	t.Parallel()
	legacy := Intent{
		FormatVersion: IntentFormatLegacy,
		Identity:      "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		InvocationID:  "publish-1", Repo: "owner/repo", BaseRef: "main", SourceHeadSHA: "head",
		AuthorizationID: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	upgraded := legacy
	upgraded.FormatVersion = IntentFormatCurrent
	upgraded.Branch = "freeside/publish/aaaaaaaaaaaaaaaa"
	if !intentsCompatible(legacy, upgraded) {
		t.Fatal("legacy intent did not accept its format-only upgrade")
	}
	upgraded.DispositionHistoryDigest = domain.Digest(
		"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
	)
	if !intentsCompatible(legacy, upgraded) {
		t.Fatal("legacy intent did not accept its history-digest-only upgrade")
	}
	changed := upgraded
	changed.SourceHeadSHA = "other-head"
	if intentsCompatible(legacy, changed) {
		t.Fatal("legacy compatibility accepted a changed publication coordinate")
	}
}

func TestIntentsCompatiblePreservesV2HistoryAndDefaultBranch(t *testing.T) {
	t.Parallel()
	v2 := Intent{
		FormatVersion:            IntentFormatHistory,
		Identity:                 "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		DispositionHistoryDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	v3 := v2
	v3.FormatVersion = IntentFormatCurrent
	v3.Branch = "freeside/publish/aaaaaaaaaaaaaaaa"
	if !intentsCompatible(v2, v3) {
		t.Fatal("v2 retry refused its default branch")
	}
	v3.Branch = "feat/different-name"
	if intentsCompatible(v2, v3) {
		t.Fatal("v2 retry renamed its branch")
	}
	v3.Branch = "freeside/publish/aaaaaaaaaaaaaaaa"
	v3.DispositionHistoryDigest = ""
	if intentsCompatible(v2, v3) {
		t.Fatal("v2 retry lost disposition history")
	}
}
