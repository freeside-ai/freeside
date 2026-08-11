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
