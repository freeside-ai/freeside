package integration_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// reviewGuideContent points at the harness candidate: README.md is "base" at
// the base and "production change" at the head. The second stop's pointers
// are stale, so they resolve as unresolved instead of failing the item.
const reviewGuideContent = `{
  "stops": [
    {"title": "The change", "summary": "Read the new line first.", "attention": "product",
     "excerpts": [
       {"path": "README.md", "side": "head", "start_line": 1, "end_line": 1, "caption": "New text"},
       {"path": "README.md", "side": "base", "start_line": 1, "end_line": 1, "caption": "Old text"}
     ]},
    {"title": "Stale pointers", "summary": "These no longer resolve.", "attention": "risk",
     "excerpts": [
       {"path": "gone.txt", "side": "head", "start_line": 1, "end_line": 1, "caption": "Missing"},
       {"path": "README.md", "side": "head", "start_line": 2, "end_line": 3, "caption": "Past the end"}
     ]}
  ],
  "questions": ["Is the new wording right?"],
  "notes": null
}`

// remediationReviewGuideContent is the remediator's own guide. Its head
// line 2 exists only at the remediated head.
const remediationReviewGuideContent = `{"stops":[{"title":"The remediation","summary":"Read the fix.",` +
	`"attention":"risk","excerpts":[{"path":"README.md","side":"head","start_line":2,"end_line":2,` +
	`"caption":"The fix"}]}],"questions":[],"notes":"Second round."}`

// withReviewGuide exports content as the review guide through the launcher's evidence
// channel, declared exactly as the Claude launcher declares it, and stores the
// blobs the import reads.
func withReviewGuide(
	t *testing.T, p *productionPublicationHarness, replay engine.ProductionReplay, content string,
) engine.ProductionReplay {
	t.Helper()
	workspace := t.TempDir()
	writeFile(t, workspace, export.ReviewGuideEvidencePath, content)
	descriptor := export.EvidenceSourceManifest{Version: export.EvidenceSourceVersion, Sources: []export.EvidenceSource{{
		Label: export.ReviewGuideEvidenceLabel, Path: export.ReviewGuideEvidencePath, MediaType: "application/json",
		HeadBinding: export.EvidenceHeadIndependent, SensitivityClass: export.EvidenceSensitivitySensitive,
		ProducerInvocationID: string(replay.InvocationID),
	}}}
	body, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, workspace, export.EvidenceDescriptorPath, string(body))
	handoff := filepath.Join(t.TempDir(), "handoff")
	if _, err := export.Export(os.DirFS(workspace), handoff, export.Options{}); err != nil {
		t.Fatal(err)
	}
	body, err = os.ReadFile(filepath.Join(handoff, export.EvidenceFilename)) //nolint:gosec // test export
	if err != nil {
		t.Fatal(err)
	}
	replay.Evidence, err = export.DecodeEvidenceManifest(body)
	if err != nil {
		t.Fatal(err)
	}
	digest := productionDigest(body)
	putProductionBlob(t, p.publicationHarness, digest, body)
	replay.EvidenceManifestDigest = &digest
	for _, entry := range replay.Evidence.Entries {
		body, err := os.ReadFile(filepath.Join(handoff, export.EvidenceBlobsDirname, "sha256", strings.TrimPrefix(string(entry.Digest), "sha256:"))) //nolint:gosec // exporter-generated digest
		if err != nil {
			t.Fatal(err)
		}
		putProductionBlob(t, p.publicationHarness, domain.Digest(entry.Digest), body)
	}
	return replay
}

func (p *productionPublicationHarness) readyItem(t *testing.T) domain.AttentionItem {
	t.Helper()
	var ready domain.AttentionItem
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		ready, err = tx.GetAttentionItem(p.ctx, domain.ItemID("production-ready-"+string(p.runID)))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return ready
}

func reconcileToReady(t *testing.T, p *productionPublicationHarness) {
	t.Helper()
	result, err := p.reconcileLanes()
	if err != nil || result.PublicationTasksCompleted != 1 || result.ReadyItemsCreated != 1 {
		t.Fatalf("reconcile = %#v, %v; want one ready item", result, err)
	}
}

func TestProductionReadyItemCarriesTheResolvedReviewGuide(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.replay = withReviewGuide(t, p, p.replay, reviewGuideContent)
	p.startAndRecordExport(t)
	reconcileToReady(t, p)
	p.assertReady(t)
	ready := p.readyItem(t)
	guide := ready.ReviewGuide
	if guide == nil || guide.HeadSHA != ready.PRHeadSHA || len(guide.Stops) != 2 ||
		guide.Stops[0].Title != "The change" || guide.Stops[1].Title != "Stale pointers" {
		t.Fatalf("review guide = %#v", guide)
	}
	var claim *domain.AgentClaim
	for i := range ready.AgentClaims {
		if ready.AgentClaims[i].Label == export.ReviewGuideEvidenceLabel {
			claim = &ready.AgentClaims[i]
		}
	}
	if claim == nil || claim.Digest != guide.ClaimDigest || claim.Text != nil ||
		claim.Provenance.HeadBinding != domain.HeadBound || claim.Provenance.SourceHeadSHA != ready.PRHeadSHA {
		t.Fatalf("review guide claim = %#v, guide digest %s", claim, guide.ClaimDigest)
	}
	resolved := guide.Stops[0].Excerpts
	if resolved[0].Text == nil || *resolved[0].Text != "production change" ||
		resolved[1].Text == nil || *resolved[1].Text != "base" {
		t.Fatalf("resolved excerpts = %#v", resolved)
	}
	stale := guide.Stops[1].Excerpts
	if stale[0].Text != nil || stale[0].UnresolvedReason == nil ||
		*stale[0].UnresolvedReason != domain.ReviewExcerptPathMissing ||
		stale[1].Text != nil || stale[1].UnresolvedReason == nil ||
		*stale[1].UnresolvedReason != domain.ReviewExcerptRangeOutOfFile {
		t.Fatalf("stale excerpts = %#v", stale)
	}
}

func TestProductionReadyItemWithoutReviewGuideCarriesNone(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	reconcileToReady(t, p)
	if guide := p.readyItem(t).ReviewGuide; guide != nil {
		t.Fatalf("review guide = %#v, want none", guide)
	}
}

// TestProductionCheckpointReviewGuideTamperFailsClosed rewrites the stored
// guide between verification and the ready item. The checkpoint re-checks it
// against the import it rides on, so a guide for another head, or one naming
// a claim the import does not carry, never reaches an item.
func TestProductionCheckpointReviewGuideTamperFailsClosed(t *testing.T) {
	t.Parallel()
	for name, tamper := range map[string]func(guide map[string]any){
		"other head":            func(guide map[string]any) { guide["head_sha"] = strings.Repeat("b", 40) },
		"digest names no claim": func(guide map[string]any) { guide["claim_digest"] = "sha256:" + strings.Repeat("e", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := newProductionPublicationHarness(t, "")
			p.replay = withReviewGuide(t, p, p.replay, reviewGuideContent)
			p.workflow = p.newEngine(t, productionCrashSeams{
				afterVerification: func() error { return errors.New("stop after the checkpoint") },
			}, true)
			p.startAndRecordExport(t)
			if _, err := p.reconcileLanes(); err == nil {
				t.Fatal("verification seam did not interrupt reconciliation")
			}
			key := "production-verification/" + string(p.runID) + "/" + p.replay.HeadSHA
			raw, err := sql.Open("sqlite", p.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = raw.Close() })
			var payload []byte
			if err := raw.QueryRowContext(p.ctx, `SELECT payload FROM inbox WHERE idempotency_key = ?`, key).Scan(&payload); err != nil {
				t.Fatal(err)
			}
			var checkpoint map[string]any
			if err := json.Unmarshal(payload, &checkpoint); err != nil {
				t.Fatal(err)
			}
			guide, ok := checkpoint["review_guide"].(map[string]any)
			if !ok {
				t.Fatalf("checkpoint review guide = %#v", checkpoint["review_guide"])
			}
			tamper(guide)
			forged, err := json.Marshal(checkpoint)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := raw.ExecContext(p.ctx, `UPDATE inbox SET payload = ? WHERE idempotency_key = ?`, forged, key); err != nil {
				t.Fatal(err)
			}
			p.workflow = p.newEngine(t, productionCrashSeams{}, true)
			if _, err := p.reconcileLanes(); !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("tampered checkpoint reconcile error = %v, want parent-key mismatch", err)
			}
			if _, err := p.attention.GetAttentionItem(
				p.ctx, domain.ItemID("production-ready-"+string(p.runID)),
			); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("tampered checkpoint created a ready item: %v", err)
			}
		})
	}
}

// TestProductionRemediatedReadyItemCarriesTheRemediationGuide: the first
// head's import carries a guide, but the ready item opens on the remediated
// head, so its guide is the one the remediation import carries, read at that
// head.
func TestProductionRemediatedReadyItemCarriesTheRemediationGuide(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.replay = withReviewGuide(t, p, p.replay, reviewGuideContent)
	testProductionAdjudicatedRemediation(t, p, false, true)
	ready := p.readyItem(t)
	guide := ready.ReviewGuide
	if guide == nil || guide.HeadSHA != ready.PRHeadSHA || ready.PRHeadSHA != p.replay.HeadSHA ||
		len(guide.Stops) != 1 || guide.Stops[0].Title != "The remediation" {
		t.Fatalf("remediated review guide = %#v", guide)
	}
	excerpt := guide.Stops[0].Excerpts[0]
	if excerpt.Text == nil || *excerpt.Text != "remediated review finding" {
		t.Fatalf("remediated excerpt = %#v", excerpt)
	}
	named := 0
	for _, claim := range ready.AgentClaims {
		if claim.Digest == guide.ClaimDigest {
			named++
			if claim.Provenance.ProducerInvocationID != p.replay.InvocationID {
				t.Fatalf("guide claim producer = %q, want the remediation %q",
					claim.Provenance.ProducerInvocationID, p.replay.InvocationID)
			}
		}
	}
	if named != 1 {
		t.Fatalf("guide digest names %d claims, want 1", named)
	}
}

// trimmedReviewGuideContent is reviewGuideContent's first stop with its head
// excerpt running 1-200, the over-long range the #2753 exit run's writer
// wrote. The import trims it to its first 80 lines.
const trimmedReviewGuideContent = `{
  "stops": [
    {"title": "The change", "summary": "Read the new line first.", "attention": "product",
     "excerpts": [
       {"path": "README.md", "side": "head", "start_line": 1, "end_line": 200, "caption": "New text"}
     ]}
  ],
  "questions": [],
  "notes": null
}`

// TestProductionReadyItemCarriesTheTrimmedReviewGuide: a guide whose only
// fault is a size cap no longer costs the run. The ready item carries the
// guide cut to the cap and the notice that says what was cut.
func TestProductionReadyItemCarriesTheTrimmedReviewGuide(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.replay = withReviewGuide(t, p, p.replay, trimmedReviewGuideContent)
	p.startAndRecordExport(t)
	reconcileToReady(t, p)
	p.assertReady(t)
	ready := p.readyItem(t)
	guide := ready.ReviewGuide
	if guide == nil || len(guide.Stops) != 1 || len(guide.Stops[0].Excerpts) != 1 {
		t.Fatalf("review guide = %#v", guide)
	}
	if excerpt := guide.Stops[0].Excerpts[0]; excerpt.StartLine != 1 || excerpt.EndLine != 80 {
		t.Fatalf("trimmed excerpt = %d-%d, want 1-80", excerpt.StartLine, excerpt.EndLine)
	}
	notice := ready.ReviewGuideNotice
	if notice == nil || notice.Omission != nil || len(notice.Trims) != 1 {
		t.Fatalf("review guide notice = %#v, want one trim", notice)
	}
	trim := notice.Trims[0]
	if trim.Cap != domain.ReviewGuideCapExcerptLines || trim.Stop == nil || *trim.Stop != 0 ||
		trim.Excerpt == nil || *trim.Excerpt != 0 || trim.Question != nil || trim.Limit != 80 || trim.Found != 200 {
		t.Fatalf("trim = %#v, want stop 0 excerpt 0 at 80 of 200 lines", trim)
	}
}

// TestProductionReadyItemOmitsAnOversizeReviewGuide: a guide too large to
// read is left out. The ready item says so, and no claim on it names the
// dropped bytes, so they reach no card.
func TestProductionReadyItemOmitsAnOversizeReviewGuide(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	oversize := reviewGuideContent[:len(reviewGuideContent)-1] + strings.Repeat(" ", 64<<10) + "}"
	p.replay = withReviewGuide(t, p, p.replay, oversize)
	p.startAndRecordExport(t)
	reconcileToReady(t, p)
	p.assertReady(t)
	ready := p.readyItem(t)
	if ready.ReviewGuide != nil {
		t.Fatalf("review guide = %#v, want none", ready.ReviewGuide)
	}
	notice := ready.ReviewGuideNotice
	if notice == nil || notice.Omission == nil || *notice.Omission != domain.ReviewGuideOmissionFileTooLarge ||
		len(notice.Trims) != 0 {
		t.Fatalf("review guide notice = %#v, want file_too_large", notice)
	}
	for _, entry := range p.replay.Evidence.Entries {
		for _, claim := range ready.AgentClaims {
			if claim.Label == export.ReviewGuideEvidenceLabel || claim.Digest == domain.Digest(entry.Digest) {
				t.Fatalf("ready item claim %#v names the dropped guide", claim)
			}
		}
	}
}

// TestProductionCheckpointReviewGuideNoticeTamperFailsClosed removes the
// trimmed guide from a stored checkpoint whose import says it was trimmed.
// The import result is unchanged, so its digest still matches; the notice
// re-check is what refuses an item that would claim a trim of no guide.
func TestProductionCheckpointReviewGuideNoticeTamperFailsClosed(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.replay = withReviewGuide(t, p, p.replay, trimmedReviewGuideContent)
	p.workflow = p.newEngine(t, productionCrashSeams{
		afterVerification: func() error { return errors.New("stop after the checkpoint") },
	}, true)
	p.startAndRecordExport(t)
	if _, err := p.reconcileLanes(); err == nil {
		t.Fatal("verification seam did not interrupt reconciliation")
	}
	key := "production-verification/" + string(p.runID) + "/" + p.replay.HeadSHA
	raw, err := sql.Open("sqlite", p.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	var payload []byte
	if err := raw.QueryRowContext(p.ctx, `SELECT payload FROM inbox WHERE idempotency_key = ?`, key).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var checkpoint map[string]any
	if err := json.Unmarshal(payload, &checkpoint); err != nil {
		t.Fatal(err)
	}
	imported, ok := checkpoint["imported"].(map[string]any)
	if !ok || imported["review_guide_notice"] == nil || checkpoint["review_guide"] == nil {
		t.Fatalf("checkpoint = %#v, want a trimmed guide and its notice", checkpoint)
	}
	checkpoint["review_guide"] = nil
	forged, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(p.ctx, `UPDATE inbox SET payload = ? WHERE idempotency_key = ?`, forged, key); err != nil {
		t.Fatal(err)
	}
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if _, err := p.reconcileLanes(); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("tampered checkpoint reconcile error = %v, want parent-key mismatch", err)
	}
	if _, err := p.attention.GetAttentionItem(
		p.ctx, domain.ItemID("production-ready-"+string(p.runID)),
	); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("tampered checkpoint created a ready item: %v", err)
	}
}
