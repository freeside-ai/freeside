package integration_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/advisory"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	inferencefake "github.com/freeside-ai/freeside/daemon/internal/inference/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// authoredExplainOutput is a schema-valid publication-author answer: the five
// fields recipe v2 stores, all non-empty and screen-clean.
const authoredExplainOutput = `{"title":"Authored PR title","body":"Authored body prose from the recipe v2 author.","reviewer_notes":null,"evidence_refs":[],"outcome_summary":"All required checks passed."}`

// newPublicV2MetadataHarness mirrors newPublicMetadataHarness but freezes recipe
// v2, so the reviewed candidate is authored and its stored artifact rendered.
func newPublicV2MetadataHarness(t *testing.T) *productionPublicationHarness {
	t.Helper()
	metadata := productionPublicationMetadata()
	metadata.Title, metadata.Body = "", ""
	metadata.Recipe = "freeside.client-publication/v2"
	metadata.SourceIssue = "https://github.com/example/project/issues/82"
	return newAuthoredMetadataHarness(t, metadata, nil)
}

// newAuthoredMetadataHarness builds a declared production harness for an
// authored publication record under the given extra policy keys.
func newAuthoredMetadataHarness(
	t *testing.T, metadata engine.ProductionPublication, extraKeys []domain.PolicyKey,
) *productionPublicationHarness {
	t.Helper()
	p := newProductionPublicationHarnessWithMetadata(t, newPublicationHarness(t), "", extraKeys, nil, nil, metadata, nil)
	declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
		CompletionCriterion: domain.CompletionBoundPRMerged, DeclaredPaths: []string{"README.md"},
	}, p.runID, p.projectID, p.now)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.store.WriteInternal(p.ctx, func(tx *store.InternalTx) error {
		return tx.RecordWorkUnitDeclaration(p.ctx, declaration)
	}); err != nil {
		t.Fatal(err)
	}
	p.declaration = &declaration
	return p
}

// scriptPublicationAuthor builds the harness inference client with the
// publication-author explain site scripted with one response. It returns the
// fake driver so a test can count author calls.
func scriptPublicationAuthor(t *testing.T, p *productionPublicationHarness, script inferencefake.Script) *inferencefake.Driver {
	t.Helper()
	return scriptPublicationSites(t, p, script, nil)
}

// scriptPublicationSites is scriptPublicationAuthor with the source-issue-closure
// propose site also scripted when propose is non-nil.
func scriptPublicationSites(
	t *testing.T, p *productionPublicationHarness, explain inferencefake.Script, propose *inferencefake.Script,
) *inferencefake.Driver {
	t.Helper()
	driver := inferencefake.New()
	driver.Script(inference.PublicationAuthorExplainSiteID, explain)
	advisoryStore, err := advisory.Open(
		filepath.Join(t.TempDir(), "advisory.json"), 20, 16<<10,
		advisory.WithClock(func() time.Time { return p.now }),
	)
	if err != nil {
		t.Fatal(err)
	}
	limits := inference.Limits{Calls: 10, ComputeUnits: 100_000, AttentionItems: 10, Starvation: time.Hour}
	budget := inference.Budget{
		Window: time.Hour, Site: limits, Project: limits, Global: limits,
		MaxCallsPerRoot: 10, MaxStarvationPerRoot: time.Hour,
	}
	sites := []inference.Site{inference.PublicationAuthorExplainSite(budget)}
	if propose != nil {
		driver.Script(inference.PublicationAuthorProposeSiteID, *propose)
		sites = append(sites, inference.PublicationAuthorProposeSite(budget))
	}
	client, err := inference.New(inference.Config{
		StatePath: filepath.Join(t.TempDir(), "ledger.json"),
		Binding:   inference.Binding{Provider: "fake", Model: "author", Driver: driver},
		Sites:     sites,
		Advisory:  advisoryStore, Now: func() time.Time { return p.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	p.judgments = client
	return driver
}

func authorCallCount(d *inferencefake.Driver) int {
	n := 0
	for _, req := range d.Requests() {
		if req.SiteID == inference.PublicationAuthorExplainSiteID {
			n++
		}
	}
	return n
}

// assertAuthoredMetadata asserts the single PR renders the stored authored
// artifact (recipe v2), not the v1 claim fallback, and returns the PR body.
func assertAuthoredMetadata(t *testing.T, p *productionPublicationHarness) string {
	t.Helper()
	if refs, count := p.forge.counts(); refs != 1 || count != 1 {
		t.Fatalf("effects: refs=%d PRs=%d", refs, count)
	}
	prs := p.forge.pullRequests()
	if prs[0].Title != "Authored PR title" {
		t.Fatalf("title = %q", prs[0].Title)
	}
	body := prs[0].Body
	for _, want := range []string{
		"Authored body prose from the recipe v2 author.",
		"Produced by the", "## Verification", "<!-- freeside:publication-identity=",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q", want)
		}
	}
	if strings.Contains(body, "## Agent-reported implementation (claim)") {
		t.Fatal("recipe v2 render fell back to the v1 claim")
	}
	if strings.Contains(body, "Private operational context") {
		t.Fatal("body leaked forbidden content")
	}
	return body
}

// TestPublicationAuthorStoresAndReplays proves the author runs once for the
// reviewed candidate, its stored artifact renders the PR, and retry, restart,
// and external-drift repair replay identical bytes without calling the author
// again.
func TestPublicationAuthorStoresAndReplays(t *testing.T) {
	p := newPublicV2MetadataHarness(t)
	p.replay = withPublicAccount(t, p, p.replay, publicAccount)
	driver := scriptPublicationAuthor(t, p, inferencefake.Script{Response: inference.Response{
		Output: []byte(authoredExplainOutput), ComputeUnits: 5,
	}})
	// Interrupt after the ready item commits (the PR is already published),
	// mirroring the v1 drift-repair fixture.
	p.workflow = p.newEngine(t, productionCrashSeams{afterReady: func() error {
		return errors.New("interrupt after ready")
	}}, true)
	p.startAndRecordExport(t)
	if _, err := p.reconcileLanes(); err == nil {
		t.Fatal("ready crash seam did not fire")
	}
	want := assertAuthoredMetadata(t, p)
	if got := authorCallCount(driver); got != 1 {
		t.Fatalf("author call count after first publication = %d", got)
	}

	// External drift: someone edits the PR title and prose. Repair must restore
	// the stored authored rendering byte-for-byte.
	p.forge.mu.Lock()
	p.forge.prs[0].Title = "external title"
	p.forge.prs[0].Body = strings.Replace(p.forge.prs[0].Body, "Authored body prose", "Externally changed prose", 1)
	p.forge.mu.Unlock()
	p.restartDurableState(t)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	if got := assertAuthoredMetadata(t, p); got != want {
		t.Fatal("drift repair changed the authored rendering")
	}
	if got := authorCallCount(driver); got != 1 {
		t.Fatalf("author called again on drift repair: count = %d", got)
	}
}

// TestPublicationAuthorReplayToleratesVisibilityReadFailure proves a transient
// GitHub visibility read failure during drift repair does not flip an already
// published v2 PR to v1: the stored artifact was already screened for public
// output, so the render keeps the stored class and replays identical bytes.
func TestPublicationAuthorReplayToleratesVisibilityReadFailure(t *testing.T) {
	p := newPublicV2MetadataHarness(t)
	p.replay = withPublicAccount(t, p, p.replay, publicAccount)
	scriptPublicationAuthor(t, p, inferencefake.Script{Response: inference.Response{
		Output: []byte(authoredExplainOutput), ComputeUnits: 5,
	}})
	p.workflow = p.newEngine(t, productionCrashSeams{afterReady: func() error {
		return errors.New("interrupt after ready")
	}}, true)
	p.startAndRecordExport(t)
	if _, err := p.reconcileLanes(); err == nil {
		t.Fatal("ready crash seam did not fire")
	}
	want := assertAuthoredMetadata(t, p)

	// External edit plus a sustained visibility read outage. Repair must restore
	// the stored v2 rendering, not fall back to v1 on the failed read.
	p.forge.mu.Lock()
	p.forge.prs[0].Body = strings.Replace(p.forge.prs[0].Body, "Authored body prose", "Externally changed prose", 1)
	p.forge.failRepoRead = true
	p.forge.mu.Unlock()
	p.restartDurableState(t)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	if got := assertAuthoredMetadata(t, p); got != want {
		t.Fatal("transient visibility read failure flipped the authored rendering")
	}
}

// TestPublicationAuthorFallsBackToV1 proves an unavailable author and a
// screen-failing answer both fall back to the deterministic v1 rendering, the
// fallback is recorded, and a retry never calls the author again.
func TestPublicationAuthorFallsBackToV1(t *testing.T) {
	for _, mode := range []string{"unavailable", "screen failure", "multiline title"} {
		t.Run(mode, func(t *testing.T) {
			p := newPublicV2MetadataHarness(t)
			p.replay = withPublicAccount(t, p, p.replay, publicAccount)
			var script inferencefake.Script
			switch mode {
			case "unavailable":
				script = inferencefake.Script{Err: errors.New("provider unavailable")}
			case "screen failure":
				// Passes the JSON shape but carries a banned close directive, so
				// the screen refuses the artifact and the candidate falls back.
				script = inferencefake.Script{Response: inference.Response{
					Output:       []byte(`{"title":"Authored PR title","body":"Body prose. Closes #82 on merge.","reviewer_notes":null,"evidence_refs":[],"outcome_summary":"Done."}`),
					ComputeUnits: 5,
				}}
			case "multiline title":
				// Passes the explain-site schema (non-empty, in-bound, screen-clean)
				// but violates the single-line title contract, so the candidate
				// falls back rather than emit malformed forge metadata.
				script = inferencefake.Script{Response: inference.Response{
					Output:       []byte(`{"title":"Authored PR title\nsecond line","body":"Authored body prose from the recipe v2 author.","reviewer_notes":null,"evidence_refs":[],"outcome_summary":"All required checks passed."}`),
					ComputeUnits: 5,
				}}
			}
			driver := scriptPublicationAuthor(t, p, script)
			p.workflow = p.newEngine(t, productionCrashSeams{}, true)
			p.startAndRecordExport(t)
			if _, err := p.reconcileLanes(); err != nil {
				t.Fatal(err)
			}
			// v1 rendering: the fallback path produces the v1 claim body.
			want := assertPublicMetadata(t, p)
			calls := authorCallCount(driver)
			if calls != 1 {
				t.Fatalf("author calls = %d, want 1", calls)
			}
			// Retry after a recorded fallback must not call the author again.
			p.restartDurableState(t)
			p.workflow = p.newEngine(t, productionCrashSeams{}, true)
			if _, err := p.reconcileLanes(); err != nil {
				t.Fatal(err)
			}
			if got := assertPublicMetadata(t, p); got != want {
				t.Fatal("retry after fallback changed the v1 rendering")
			}
			if got := authorCallCount(driver); got != calls {
				t.Fatalf("author re-called after recorded fallback: %d then %d", calls, got)
			}
		})
	}
}

// TestPublicationAuthorTargetClassOpens proves the stored artifact's class comes
// from the repository's visibility, and that a repository that later becomes
// more open than the stored class allows renders v1 instead of the stored v2.
func TestPublicationAuthorTargetClassOpens(t *testing.T) {
	p := newPublicV2MetadataHarness(t)
	p.replay = withPublicAccount(t, p, p.replay, publicAccount)
	// The repository is private at author time, so the stored class is sensitive.
	p.forge.mu.Lock()
	p.forge.visibility = "private"
	p.forge.mu.Unlock()
	scriptPublicationAuthor(t, p, inferencefake.Script{Response: inference.Response{
		Output: []byte(authoredExplainOutput), ComputeUnits: 5,
	}})
	// Interrupt after the ready item commits so the drift-repair pass re-renders.
	p.workflow = p.newEngine(t, productionCrashSeams{afterReady: func() error {
		return errors.New("interrupt after ready")
	}}, true)
	p.startAndRecordExport(t)
	if _, err := p.reconcileLanes(); err == nil {
		t.Fatal("ready crash seam did not fire")
	}
	assertAuthoredMetadata(t, p)

	// The repository becomes public: now more open than the stored sensitive
	// class allows, so the repair pass re-renders v1 in place of the stored v2.
	p.forge.mu.Lock()
	p.forge.visibility = "public"
	p.forge.mu.Unlock()
	p.restartDurableState(t)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	assertPublicMetadata(t, p)
}

// TestPublicationAuthorFailsClosedOnUnknownVisibility proves that when the
// repository visibility read fails and the stored class is more restrictive than
// public, the render fails closed to v1. An unknown current visibility cannot
// confirm the repository is still as restricted as the stored sensitive class
// assumes, so private-authored prose must not risk a now-public PR. The
// normal-class companion (TestPublicationAuthorReplayToleratesVisibilityReadFailure)
// keeps v2 on the same read failure, because public-grade content is safe under
// any visibility.
func TestPublicationAuthorFailsClosedOnUnknownVisibility(t *testing.T) {
	p := newPublicV2MetadataHarness(t)
	p.replay = withPublicAccount(t, p, p.replay, publicAccount)
	// Private at author time, so the stored class is sensitive.
	p.forge.mu.Lock()
	p.forge.visibility = "private"
	p.forge.mu.Unlock()
	scriptPublicationAuthor(t, p, inferencefake.Script{Response: inference.Response{
		Output: []byte(authoredExplainOutput), ComputeUnits: 5,
	}})
	// Interrupt after the ready item commits so the drift-repair pass re-renders.
	p.workflow = p.newEngine(t, productionCrashSeams{afterReady: func() error {
		return errors.New("interrupt after ready")
	}}, true)
	p.startAndRecordExport(t)
	if _, err := p.reconcileLanes(); err == nil {
		t.Fatal("ready crash seam did not fire")
	}
	// Published while private with a successful read: the stored v2 renders.
	assertAuthoredMetadata(t, p)

	// The visibility read now fails. The stored class is sensitive, so the repair
	// pass cannot confirm the repository is still restricted and falls back to v1.
	p.forge.mu.Lock()
	p.forge.failRepoRead = true
	p.forge.mu.Unlock()
	p.restartDurableState(t)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	assertPublicMetadata(t, p)
}
