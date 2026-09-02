package importer

import (
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/export"
)

const blockedOutcomeJSON = `{"version":"freeside.blocked-outcome/v1","kind":"owner_decision","decisions":[{"question":"Which retention period applies?","why_blocking":"The schema depends on it.","options":[{"label":"30 days","tradeoffs":"cheaper"},{"label":"1 year","tradeoffs":"longer audit"}],"recommendation":"30 days"}]}`

// TestImportExpectNoChanges: a blocked handoff imports its evidence channel
// and builds no commit; any repo-channel change or a commit plan is a
// definitive ErrUnexpectedChanges.
func TestImportExpectNoChanges(t *testing.T) {
	blocked := evidenceEntry(export.BlockedEvidenceLabel, "application/json", blockedOutcomeJSON)
	body, err := evidenceManifest(blocked).Encode()
	if err != nil {
		t.Fatalf("encode evidence: %v", err)
	}
	withEvidence := func(handoff string) {
		writeEvidence(t, handoff, body, blobFor(blockedOutcomeJSON))
	}

	t.Run("no changes", func(t *testing.T) {
		clone, base, handoff := evidenceFixture(t, map[string]string{"a.txt": "old\n"})
		withEvidence(handoff)
		opts := testImportOptions(base)
		opts.ExpectNoChanges = true
		res, err := Import(t.Context(), handoff, clone, opts)
		if err != nil {
			t.Fatalf("Import: %v", err)
		}
		if res.CommitSHA != "" || res.TreeSHA != "" {
			t.Fatalf("blocked import built a commit: %+v", res)
		}
		if len(res.Claims) != 1 || res.Claims[0].Label != export.BlockedEvidenceLabel ||
			res.Claims[0].Metadata.MediaType != domain.EvidenceMediaApplicationJSON {
			t.Fatalf("claims = %+v, want the blocked outcome claim", res.Claims)
		}
		if len(res.Findings) != 0 {
			t.Fatalf("findings = %+v, want none", res.Findings)
		}
	})

	t.Run("changed path", func(t *testing.T) {
		clone, base, handoff := evidenceFixture(t, map[string]string{"a.txt": "new\n"})
		withEvidence(handoff)
		opts := testImportOptions(base)
		opts.ExpectNoChanges = true
		if _, err := Import(t.Context(), handoff, clone, opts); !errors.Is(err, ErrUnexpectedChanges) {
			t.Fatalf("Import with a change = %v, want ErrUnexpectedChanges", err)
		}
	})

	t.Run("commit plan present", func(t *testing.T) {
		clone, base, handoff := evidenceFixture(t, map[string]string{
			"a.txt": "old\n", export.CommitPlanFilename: `{"version":"freeside.commit-plan/v1","groups":[]}`,
		})
		withEvidence(handoff)
		opts := testImportOptions(base)
		opts.ExpectNoChanges = true
		if _, err := Import(t.Context(), handoff, clone, opts); !errors.Is(err, ErrUnexpectedChanges) {
			t.Fatalf("Import with a commit plan = %v, want ErrUnexpectedChanges", err)
		}
	})

	t.Run("unchanged policy still builds a commit", func(t *testing.T) {
		clone, base, handoff := evidenceFixture(t, map[string]string{"a.txt": "old\n"})
		withEvidence(handoff)
		res, err := Import(t.Context(), handoff, clone, testImportOptions(base))
		if err != nil {
			t.Fatalf("Import: %v", err)
		}
		if res.CommitSHA == "" {
			t.Fatal("ordinary import of an unchanged workspace built no commit")
		}
	})
}

// TestImportEvidenceJSONMediaType: application/json evidence must hold
// exactly one valid UTF-8 JSON value.
func TestImportEvidenceJSONMediaType(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		want    error
	}{
		{"object", blockedOutcomeJSON, nil},
		{"array", `[1,2]`, nil},
		{"two values", `{}{}`, ErrEvidenceMediaMismatch},
		{"prose", `not json`, ErrEvidenceMediaMismatch},
		{"invalid utf-8", "{\"a\":\"\xff\"}", ErrEvidenceMediaMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clone, base, handoff := evidenceFixture(t, map[string]string{"a.txt": "new\n"})
			entry := evidenceEntry("doc", "application/json", tc.content)
			body, err := evidenceManifest(entry).Encode()
			if err != nil {
				t.Fatalf("encode evidence: %v", err)
			}
			writeEvidence(t, handoff, body, blobFor(tc.content))
			_, err = Import(t.Context(), handoff, clone, testImportOptions(base))
			if tc.want == nil && err != nil {
				t.Fatalf("Import: %v", err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("Import = %v, want %v", err, tc.want)
			}
		})
	}
}
