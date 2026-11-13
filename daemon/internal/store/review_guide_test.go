package store

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

// readyReviewGuideItem is a stored-shape ready item carrying a review guide
// and the reserved claim it was parsed from. The item body is a JSON
// document, so the guide needs no column and no migration.
func readyReviewGuideItem(t *testing.T) domain.AttentionItem {
	t.Helper()
	item := degradedReadyDetailItem(t)
	item.ID = "item-review-guide"
	claim := domain.AgentClaim{
		Label: domain.ReviewGuideClaimLabel, Artifact: "art-review-guide", Digest: "sha256:review-guide",
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerAgent, ProducerInvocationID: "inv-2",
			HeadBinding: domain.HeadBound, SourceHeadSHA: item.PRHeadSHA,
			SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: domain.EvidenceMetadata{
			MediaType: domain.EvidenceMediaApplicationJSON, SizeBytes: 1024,
			CreatedAt: time.Date(2026, 8, 24, 9, 27, 0, 0, time.UTC),
			Source:    domain.EvidenceSourceClaim, Availability: domain.EvidenceAvailable,
		},
	}
	item.AgentClaims = append(slices.Clone(item.AgentClaims), claim)
	item.ArtifactDigests = slices.Compact(slices.Sorted(slices.Values(append(slices.Clone(item.ArtifactDigests), claim.Digest))))
	text := "func (g ReviewGuide) Validate() error {\n"
	missing := domain.ReviewExcerptPathMissing
	item.ReviewGuide = &domain.ReviewGuide{
		HeadSHA: item.PRHeadSHA, ClaimDigest: claim.Digest,
		Stops: []domain.ReviewStop{
			{
				Title: "Start with the guide's validation", Summary: "Validate enforces the caps.",
				Attention: domain.ReviewAttentionContract,
				Excerpts: []domain.ReviewExcerpt{{
					Path: "daemon/internal/domain/review_guide.go", Side: domain.ReviewExcerptSideHead,
					StartLine: 10, EndLine: 10, Caption: "The entry point.", Text: &text,
				}},
			},
			{
				Title: "The removed helper", Summary: "Nothing else called it.",
				Attention: domain.ReviewAttentionRisk,
				Excerpts: []domain.ReviewExcerpt{{
					Path: "daemon/internal/domain/old_helper.go", Side: domain.ReviewExcerptSideBase,
					StartLine: 1, EndLine: 20, Caption: "Before its removal.", UnresolvedReason: &missing,
				}},
			},
		},
		Questions: []string{},
	}
	stop, excerpt := 0, 0
	item.ReviewGuideNotice = &domain.ReviewGuideNotice{Trims: []domain.ReviewGuideTrim{{
		Cap: domain.ReviewGuideCapExcerptLines, Stop: &stop, Excerpt: &excerpt,
		Limit: domain.MaxReviewExcerptLines, Found: 116,
	}}}
	if err := item.Validate(); err != nil {
		t.Fatalf("fixture item: %v", err)
	}
	return item
}

// TestReadyItemReviewGuideRoundTripsAndLegacyRowsDecode: a guide and its
// notice survive put, get, and list unchanged, and a row stored before the
// fields existed, with no review_guide or review_guide_notice key in its
// body, decodes to a nil guide and notice.
func TestReadyItemReviewGuideRoundTripsAndLegacyRowsDecode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	item := readyReviewGuideItem(t)
	st := openTestStore(t)
	if err := st.Write(ctx, func(tx *WriteTx) error {
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatal(err)
	}
	read := func(t *testing.T) (got, listed domain.AttentionItem) {
		t.Helper()
		if err := st.Read(ctx, func(tx *ReadTx) error {
			var err error
			if got, err = tx.GetAttentionItem(ctx, item.ID); err != nil {
				return err
			}
			items, err := tx.ListAttentionItems(ctx)
			for _, candidate := range items {
				if candidate.Value.ID == item.ID {
					listed = candidate.Value
				}
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return got, listed
	}
	got, listed := read(t)
	for _, decoded := range []domain.AttentionItem{got, listed} {
		if decoded.ReviewGuide == nil || !reflect.DeepEqual(*decoded.ReviewGuide, *item.ReviewGuide) {
			t.Fatalf("review guide = %+v, want %+v", decoded.ReviewGuide, item.ReviewGuide)
		}
		if decoded.ReviewGuideNotice == nil || !reflect.DeepEqual(*decoded.ReviewGuideNotice, *item.ReviewGuideNotice) {
			t.Fatalf("review guide notice = %+v, want %+v", decoded.ReviewGuideNotice, item.ReviewGuideNotice)
		}
	}
	pretty, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "attention_item_card_review_guide", append(pretty, '\n'))

	if _, err := st.db.ExecContext(ctx,
		`UPDATE attention_items SET body = json_remove(body, '$.review_guide', '$.review_guide_notice') WHERE id = ?`, item.ID,
	); err != nil {
		t.Fatal(err)
	}
	got, listed = read(t)
	for _, decoded := range []domain.AttentionItem{got, listed} {
		if decoded.ReviewGuide != nil || decoded.ReviewGuideNotice != nil {
			t.Fatalf("legacy row review guide = %+v, notice %+v, want nil", decoded.ReviewGuide, decoded.ReviewGuideNotice)
		}
	}
}
