package integration_test

import (
	"encoding/json"
	"errors"
	"html"
	"os"
	"path/filepath"
	"slices"
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
	return newAuthoredMetadataHarnessFromBase(t, newPublicationHarness(t), metadata, extraKeys, nil)
}

func newAuthoredMetadataHarnessFromBase(
	t *testing.T, base *publicationHarness, metadata engine.ProductionPublication, extraKeys []domain.PolicyKey,
	candidateFiles map[string]string,
) *productionPublicationHarness {
	t.Helper()
	p := newProductionPublicationHarnessWithMetadata(t, base, "", extraKeys, nil, candidateFiles, metadata, nil)
	declaredPaths := []string{"README.md"}
	for path := range candidateFiles {
		declaredPaths = append(declaredPaths, path)
	}
	slices.Sort(declaredPaths)
	declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
		CompletionCriterion: domain.CompletionBoundPRMerged, DeclaredPaths: declaredPaths,
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

func TestPublicationAuthorOversizedRuntimeInputReasonsSurviveRestart(t *testing.T) {
	metadata := productionPublicationMetadata()
	metadata.Title, metadata.Body = "", ""
	metadata.Recipe = "freeside.client-publication/v2"
	metadata.SourceIssue = "https://github.com/" + fakePublicationRepo + "/issues/82"
	longRules := strings.Repeat("a", 256<<10)
	base := newPublicationHarnessWithBaseFiles(t,
		[]byte(`{"commands":[["/usr/bin/true"]],"capture":"none"}`),
		map[string]string{"AGENTS.md": longRules},
	)
	p := newAuthoredMetadataHarnessFromBase(t, base, metadata, nil, map[string]string{"AGENTS.md": "candidate rules\n"})
	p.replay = withPublicAccount(t, p, p.replay, publicAccount)
	driver := scriptPublicationSites(t, p,
		inferencefake.Script{Response: inference.Response{Output: []byte(authoredExplainOutput), ComputeUnits: 5}},
		&inferencefake.Script{Response: inference.Response{Output: []byte(`{"resolves":true}`), ComputeUnits: 1}},
	)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.startAndRecordExport(t)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	prs := p.forge.pullRequests()
	if len(prs) != 1 || strings.Contains(prs[0].Body, "Closes #82") || len(driver.Requests()) != 0 {
		t.Fatalf("oversized input published a close or reached the driver: prs=%d calls=%d", len(prs), len(driver.Requests()))
	}
	authorKey := "production-authoring/" + string(p.runID) + "/" + prs[0].HeadSHA + "/" + p.baseSHA
	closureKey := "production-closure/" + string(p.runID) + "/publish-production-" + string(p.runID)
	assertReasons := func() {
		t.Helper()
		for _, key := range []string{authorKey, closureKey} {
			var reason string
			if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
				entry, err := tx.GetInbox(p.ctx, key)
				if err != nil {
					return err
				}
				var row struct {
					Reason string `json:"reason"`
				}
				if err := json.Unmarshal(entry.Payload, &row); err != nil {
					return err
				}
				reason = row.Reason
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(reason, "instruction_snapshot:") || !strings.Contains(reason, "limit 262144") || strings.Contains(reason, "aaaa") {
				t.Fatalf("checkpoint %q reason = %q", key, reason)
			}
		}
	}
	assertReasons()
	p.restartDurableState(t)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	assertReasons()
	if len(driver.Requests()) != 0 {
		t.Fatal("restart called an author site after a durable input refusal")
	}
}

func TestPublicationAuthorStoresTextAndPolicyApprovedClose(t *testing.T) {
	metadata := productionPublicationMetadata()
	metadata.Title, metadata.Body = "", ""
	metadata.Recipe = "freeside.client-publication/v2"
	metadata.SourceIssue = "https://github.com/" + fakePublicationRepo + "/issues/82"
	// Public gh-imgup template at 5ad83e9b8e7fe096af323128d7af6d1d4020cfcc.
	template, err := os.ReadFile("../publicationtext/testdata/gh-imgup-template.md")
	if err != nil {
		t.Fatal(err)
	}
	base := newPublicationHarnessWithBaseFiles(t,
		[]byte(`{"commands":[["/usr/bin/true"]],"capture":"none"}`),
		map[string]string{".github/pull_request_template.md": string(template)},
	)
	p := newAuthoredMetadataHarnessFromBase(t, base, metadata, nil,
		map[string]string{".github/pull_request_template.md": string(template)})
	const disclosure = "Check evidence and the source reference are supplied separately by the publisher. Its fixed check format does not use the template's requested status prefixes."
	const body = "## Why\n\nAuthored body prose from the recipe v2 author.\n\n## What\n\n- Handle empty input.\n\n## Review Notes\n\n" + disclosure
	output, err := json.Marshal(map[string]any{"title": "Authored PR title", "body": body, "reviewer_notes": nil, "evidence_refs": []string{}, "outcome_summary": "Reported checks passed."})
	if err != nil {
		t.Fatal(err)
	}
	p.replay = withPublicAccount(t, p, p.replay, publicAccount)
	driver := scriptPublicationSites(t, p,
		inferencefake.Script{Response: inference.Response{Output: output, ComputeUnits: 5}},
		&inferencefake.Script{Response: inference.Response{Output: []byte(`{"resolves":true}`), ComputeUnits: 1}},
	)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.startAndRecordExport(t)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	if body := assertAuthoredMetadata(t, p); !strings.Contains(body, "Closes #82") {
		t.Fatal("publisher omitted the policy-approved close reference")
	}
	rendered := p.forge.pullRequests()[0].Body
	if strings.Count(rendered, "## Verification") != 1 || !strings.Contains(rendered, html.EscapeString(disclosure)) {
		t.Fatal("publisher ownership or visible conflict disclosure lost")
	}
	for _, request := range driver.Requests() {
		if request.Fields["pr_template"] != string(template) {
			t.Fatal("trusted template was changed before the author call")
		}
	}
	authorKey := "production-authoring/" + string(p.runID) + "/" + p.forge.pullRequests()[0].HeadSHA + "/" + p.baseSHA
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		entry, err := tx.GetInbox(p.ctx, authorKey)
		if err != nil {
			return err
		}
		var cp struct {
			ArtifactDigest domain.Digest `json:"artifact_digest"`
		}
		if err := json.Unmarshal(entry.Payload, &cp); err != nil {
			return err
		}
		artifact, err := tx.GetPublicationAuthoring(p.ctx, cp.ArtifactDigest)
		if err != nil {
			return err
		}
		if artifact.Body != body {
			t.Fatal("stored authoring differs from the compatible fragment")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if p.forge.pullRequests()[0].Draft || len(driver.Requests()) != 2 {
		t.Fatal("valid inputs did not reach both author sites and publish a ready PR")
	}
	key := "production-closure/" + string(p.runID) + "/publish-production-" + string(p.runID)
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		entry, err := tx.GetInbox(p.ctx, key)
		if err != nil {
			return err
		}
		var checkpoint struct {
			InstanceID domain.ProposalInstanceID `json:"instance_id"`
		}
		if err := json.Unmarshal(entry.Payload, &checkpoint); err != nil {
			return err
		}
		approval, err := tx.ClosureApprovalForInstance(p.ctx, checkpoint.InstanceID)
		if err != nil {
			return err
		}
		if approval == nil || approval.Actor != domain.ClosureApprovalActorPolicy {
			t.Fatalf("closure approval = %+v, want policy actor", approval)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestClientSubmissionPublishesPolicyApprovedClose is the #1535 end-to-end
// proof: a real client submission whose source is a bare issue URL in the
// daemon's own repository reaches the closure gate with no fixture-registered
// project, records a policy-approved proposal, and publishes Closes.
func TestClientSubmissionPublishesPolicyApprovedClose(t *testing.T) {
	source := engine.ProductionPublication{SourceIssue: "https://github.com/" + fakePublicationRepo + "/issues/82"}
	p := newProductionPublicationHarnessWithMetadata(t, newPublicationHarness(t), "", nil, nil, nil, source, nil, "")
	output, err := json.Marshal(map[string]any{
		"title": "Authored PR title", "body": "## Why\n\nAuthored body prose.\n\n## What\n\n- Handle empty input.",
		"reviewer_notes": nil, "evidence_refs": []string{}, "outcome_summary": "Reported checks passed.",
	})
	if err != nil {
		t.Fatal(err)
	}
	p.replay = withPublicAccount(t, p, p.replay, publicAccount)
	scriptPublicationSites(t, p,
		inferencefake.Script{Response: inference.Response{Output: output, ComputeUnits: 5}},
		&inferencefake.Script{Response: inference.Response{Output: []byte(`{"resolves":true}`), ComputeUnits: 1}},
	)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.startAndRecordExport(t)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	prs := p.forge.pullRequests()
	if len(prs) != 1 || !strings.Contains(prs[0].Body, "Closes #82") {
		t.Fatalf("client submission did not publish the policy-approved close reference: %d PRs", len(prs))
	}
	key := "production-closure/" + string(p.runID) + "/publish-production-" + string(p.runID)
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		entry, err := tx.GetInbox(p.ctx, key)
		if err != nil {
			return err
		}
		var checkpoint struct {
			HasProposal bool                      `json:"has_proposal"`
			InstanceID  domain.ProposalInstanceID `json:"instance_id"`
		}
		if err := json.Unmarshal(entry.Payload, &checkpoint); err != nil {
			return err
		}
		if !checkpoint.HasProposal {
			t.Fatal("closure checkpoint recorded no proposal")
		}
		approval, err := tx.ClosureApprovalForInstance(p.ctx, checkpoint.InstanceID)
		if err != nil {
			return err
		}
		if approval == nil || approval.Actor != domain.ClosureApprovalActorPolicy {
			t.Fatalf("closure approval = %+v, want policy actor", approval)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPublicationAuthorScreenFailureKeepsIndependentClosure(t *testing.T) {
	secret := "ghp_" + strings.Repeat("x", 36)
	for _, tc := range []struct {
		name, body, reason string
		providerErr        error
	}{
		{"closing directive", "Closes #82", "publication author body: message_rules", nil},
		{"reserved section", "## Verification\n\nAll passed.", "publication author body: candidate_body_size_or_reserved_section", nil},
		{"spliced secret", "ghp_" + strings.Repeat("x", 18) + "**xx**" + strings.Repeat("x", 16), "publication author body: secret_detection", nil},
		{"provider error", "", "author returned a fallback", errors.New(secret)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metadata := productionPublicationMetadata()
			metadata.Title, metadata.Body = "", ""
			metadata.Recipe = "freeside.client-publication/v2"
			metadata.SourceIssue = "https://github.com/" + fakePublicationRepo + "/issues/82"
			p := newAuthoredMetadataHarness(t, metadata, nil)
			p.replay = withPublicAccount(t, p, p.replay, publicAccount)
			output, err := json.Marshal(map[string]any{"title": "Authored PR title", "body": tc.body, "reviewer_notes": nil, "evidence_refs": []string{}, "outcome_summary": "Done"})
			if err != nil {
				t.Fatal(err)
			}
			driver := scriptPublicationSites(t, p,
				inferencefake.Script{Response: inference.Response{Output: output, ComputeUnits: 5}, Err: tc.providerErr},
				&inferencefake.Script{Response: inference.Response{Output: []byte(`{"resolves":true}`), ComputeUnits: 1}},
			)
			p.workflow = p.newEngine(t, productionCrashSeams{}, true)
			p.startAndRecordExport(t)
			if _, err := p.reconcileLanes(); err != nil {
				t.Fatal(err)
			}
			prs := p.forge.pullRequests()
			if len(prs) != 1 || !strings.Contains(prs[0].Body, "## Agent-reported implementation (claim)") ||
				!strings.Contains(prs[0].Body, "Closes #82") || len(driver.Requests()) != 2 {
				t.Fatalf("screen fallback suppressed separately approved closure: prs=%d calls=%d", len(prs), len(driver.Requests()))
			}
			authorKey := "production-authoring/" + string(p.runID) + "/" + prs[0].HeadSHA + "/" + p.baseSHA
			assertCheckpoint := func() {
				t.Helper()
				if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
					entry, err := tx.GetInbox(p.ctx, authorKey)
					if err != nil {
						return err
					}
					var cp struct {
						Fallback       bool   `json:"fallback"`
						Reason         string `json:"reason"`
						ArtifactDigest string `json:"artifact_digest"`
					}
					if err := json.Unmarshal(entry.Payload, &cp); err != nil {
						return err
					}
					if !cp.Fallback || cp.ArtifactDigest != "" || cp.Reason != tc.reason {
						t.Fatalf("unexpected durable refusal: %+v", cp)
					}
					if strings.Contains(string(entry.Payload), secret) {
						t.Fatal("checkpoint retained a secret")
					}
					closure, err := tx.GetInbox(p.ctx, "production-closure/"+string(p.runID)+"/publish-production-"+string(p.runID))
					if err != nil {
						return err
					}
					var row struct {
						InstanceID domain.ProposalInstanceID `json:"instance_id"`
					}
					if err := json.Unmarshal(closure.Payload, &row); err != nil {
						return err
					}
					approval, err := tx.ClosureApprovalForInstance(p.ctx, row.InstanceID)
					if err != nil {
						return err
					}
					if approval == nil || approval.Actor != domain.ClosureApprovalActorPolicy {
						t.Fatal("independent policy approval missing")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			assertCheckpoint()
			p.restartDurableState(t)
			p.workflow = p.newEngine(t, productionCrashSeams{}, true)
			if _, err := p.reconcileLanes(); err != nil {
				t.Fatal(err)
			}
			assertCheckpoint()
			if len(driver.Requests()) != 2 || p.forge.pullRequests()[0].Body != prs[0].Body {
				t.Fatal("restart re-authored or changed the fallback")
			}
		})
	}
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
