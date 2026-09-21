package inference_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/inference/fake"
)

// controlFile builds a digest-consistent control file with a trusted-base
// commit, the shape a resolved template or instruction snapshot has.
func controlFile(content string) inference.ControlFile {
	return inference.ControlFile{
		Content: content, Digest: contentaddr.Sum([]byte(content)), TrustedBaseCommit: "base-sha",
	}
}

// authorInput is a valid public-target input with no evidence.
func authorInput() inference.PublicationAuthorInput {
	return inference.PublicationAuthorInput{
		Project: "p", RootLineage: "r",
		TargetRepository: "owner/repo", TargetVisibility: inference.RepositoryPublic,
		SourceIssueRef: "#42", SourceIssueTitle: "Fix the bug", SourceIssueBody: "It crashes on empty input.",
		SourceVisibility:    inference.RepositoryPublic,
		Diff:                "diff --git a/main.go b/main.go",
		VerificationOutcome: "passed", ReviewOutcome: "clean",
		PRTemplate:          controlFile("## Why\n\n## What\n\n## Verification"),
		InstructionSnapshot: controlFile("Follow the repository AGENTS.md."),
		ApprovedRecipes:     map[domain.Digest]bool{},
	}
}

// evidenceArtifact builds a well-formed verifier artifact whose publish-
// eligibility bit is computed from approved against the given recipe.
func evidenceArtifact(
	t *testing.T, id string, class domain.SensitivityClass, recipe domain.Digest, approved map[domain.Digest]bool,
) domain.Artifact {
	t.Helper()
	a, err := domain.NewArtifact(domain.ArtifactInput{
		ID: domain.ArtifactID(id), Type: domain.ArtifactKindEvidence,
		Digest: domain.Digest(contentaddr.Sum([]byte(id))),
		Provenance: domain.Provenance{
			ProducerClass:            domain.ProducerVerifier,
			ProducerInvocationID:     domain.InvocationID("inv-" + id),
			HeadBinding:              domain.HeadIndependent,
			VerificationRecipeDigest: &recipe,
			SensitivityClass:         class,
		},
		Metadata: domain.EvidenceMetadata{
			MediaType: domain.EvidenceMediaTextPlain, SizeBytes: 1,
			CreatedAt: time.Unix(1, 0).UTC(), Source: domain.EvidenceSourceRun,
			Availability: domain.EvidenceAvailable,
		},
	}, approved)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func scriptExplain(driver *fake.Driver, output string) {
	driver.Script(inference.PublicationAuthorExplainSiteID,
		fake.Script{Response: inference.Response{Output: []byte(output), ComputeUnits: 1}})
}

func scriptPropose(driver *fake.Driver, output string) {
	driver.Script(inference.PublicationAuthorProposeSiteID,
		fake.Script{Response: inference.Response{Output: []byte(output), ComputeUnits: 1}})
}

func TestPublicationAuthorSiteContracts(t *testing.T) {
	explain := inference.PublicationAuthorExplainSite(testBudget(1))
	if explain.ID != inference.PublicationAuthorExplainSiteID || explain.Authority != inference.AuthorityExplain ||
		explain.FailSafe != `{"title":"","body":"","reviewer_notes":null,"evidence_refs":[],"outcome_summary":""}` ||
		explain.Timeout != 120*time.Second || explain.MaxOutputBytes != 64<<10 || explain.Retention != 30*24*time.Hour ||
		explain.AuditEvery != 10 || len(explain.Fields) != 12 || explain.Annotation != nil {
		t.Fatalf("explain contract = %+v", explain)
	}
	propose := inference.PublicationAuthorProposeSite(testBudget(1))
	if propose.ID != inference.PublicationAuthorProposeSiteID || propose.Authority != inference.AuthorityPropose ||
		propose.FailSafe != `{"resolves":false}` || propose.Timeout != 60*time.Second || propose.MaxOutputBytes != 1<<10 ||
		propose.Annotation != nil {
		t.Fatalf("propose contract = %+v", propose)
	}
	// Both sites register together with the five existing sites.
	client, _, _ := testClient(t, nil, 10)
	if !client.SupportsSite(inference.PublicationAuthorExplainSiteID) ||
		!client.SupportsSite(inference.PublicationAuthorProposeSiteID) {
		t.Fatal("publication-author sites not registered alongside the existing sites")
	}
}

func TestAuthorPublicationValidWithReference(t *testing.T) {
	recipe := domain.Digest(contentaddr.Sum([]byte("recipe")))
	approved := map[domain.Digest]bool{recipe: true}
	driver := fake.New()
	scriptExplain(driver, `{"title":"Fix crash on empty input","body":"Handles the nil case that crashed.","reviewer_notes":"Watch the boundary test.","evidence_refs":["ev-1"],"outcome_summary":"Verification passed; review clean."}`)
	client, claims, _ := testClient(t, driver, 10)
	input := authorInput()
	input.ApprovedRecipes = approved
	input.Evidence = []domain.Artifact{evidenceArtifact(t, "ev-1", domain.SensitivityNormal, recipe, approved)}

	result, err := client.AuthorPublication(t.Context(), input)
	if err != nil || result.Fallback || result.Title != "Fix crash on empty input" ||
		result.Producer != "fake/test" || result.OutcomeSummary != "Verification passed; review clean." {
		t.Fatalf("AuthorPublication = %+v, %v", result, err)
	}
	if result.ReviewerNotes == nil || *result.ReviewerNotes != "Watch the boundary test." {
		t.Fatalf("reviewer notes = %v", result.ReviewerNotes)
	}
	if len(result.EvidenceRefs) != 1 || result.EvidenceRefs[0].ArtifactID != "ev-1" ||
		result.EvidenceRefs[0].Digest != domain.Digest(contentaddr.Sum([]byte("ev-1"))) {
		t.Fatalf("evidence refs = %+v", result.EvidenceRefs)
	}
	if result.TargetClass != domain.SensitivityNormal || result.InputDigest == "" ||
		result.InputClasses["diff"] != domain.SensitivityNormal {
		t.Fatalf("classes = %q, %+v", result.TargetClass, result.InputClasses)
	}
	// Exactly the declared allowlist crosses the boundary, and a sampled audit
	// entry records the untrusted-input call.
	requests := driver.Requests()
	if len(requests) != 1 || len(requests[0].Fields) != 12 {
		t.Fatalf("outbound fields = %+v", requests)
	}
	for _, forbidden := range []string{"credential", "private_summary", "token"} {
		if _, ok := requests[0].Fields[forbidden]; ok {
			t.Fatalf("outbound carried a forbidden field %q", forbidden)
		}
	}
	entries, err := claims.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	sampled := false
	for _, e := range entries {
		if e.Kind == "audit_sample" && e.Site == inference.PublicationAuthorExplainSiteID {
			sampled = true
		}
	}
	if !sampled {
		t.Fatalf("no audit_sample entry for the explain site: %+v", entries)
	}
}

func TestAuthorPublicationExplainRejections(t *testing.T) {
	overTitle := strings.Repeat("x", domain.MaxPublicationAuthoringTitleBytes+1)
	for _, tc := range []struct{ label, output string }{
		{"extra field", `{"title":"t","body":"b","reviewer_notes":null,"evidence_refs":[],"outcome_summary":"s","approve":true}`},
		{"empty title", `{"title":"","body":"b","reviewer_notes":null,"evidence_refs":[],"outcome_summary":"s"}`},
		{"over-bound title", `{"title":"` + overTitle + `","body":"b","reviewer_notes":null,"evidence_refs":[],"outcome_summary":"s"}`},
		{"repeated evidence id", `{"title":"t","body":"b","reviewer_notes":null,"evidence_refs":["ev-1","ev-1"],"outcome_summary":"s"}`},
		{"close directive", `{"title":"t","body":"Closes #1 for good.","reviewer_notes":null,"evidence_refs":[],"outcome_summary":"s"}`},
		{"ci skip", `{"title":"t","body":"Ready [skip ci] now.","reviewer_notes":null,"evidence_refs":[],"outcome_summary":"s"}`},
		{"trailer", `{"title":"t","body":"Real change.\nSigned-off-by: X <x@example.com>","reviewer_notes":null,"evidence_refs":[],"outcome_summary":"s"}`},
		{"secret", `{"title":"t","body":"token ghp_` + strings.Repeat("x", 36) + `","reviewer_notes":null,"evidence_refs":[],"outcome_summary":"s"}`},
		{"entity-hidden close", `{"title":"t","body":"This &#99;loses #1 now.","reviewer_notes":null,"evidence_refs":[],"outcome_summary":"s"}`},
	} {
		t.Run(tc.label, func(t *testing.T) {
			driver := fake.New()
			scriptExplain(driver, tc.output)
			client, _, _ := testClient(t, driver, 10)
			result, err := client.AuthorPublication(t.Context(), authorInput())
			if err != nil || !result.Fallback || result.Title != "" || len(result.EvidenceRefs) != 0 {
				t.Fatalf("expected fallback, got %+v, %v", result, err)
			}
		})
	}
}

func TestAuthorPublicationCitesUnsuppliedID(t *testing.T) {
	driver := fake.New()
	scriptExplain(driver, `{"title":"t","body":"b","reviewer_notes":null,"evidence_refs":["ev-missing"],"outcome_summary":"s"}`)
	client, _, _ := testClient(t, driver, 10)
	result, err := client.AuthorPublication(t.Context(), authorInput())
	if err != nil || !result.Fallback {
		t.Fatalf("citing an unsupplied id should fall back: %+v, %v", result, err)
	}
}

func TestProposeSourceIssueClosure(t *testing.T) {
	for _, tc := range []struct {
		label, output string
		want, fall    bool
	}{
		{"resolves true", `{"resolves":true}`, true, false},
		{"resolves false", `{"resolves":false}`, false, false},
		{"missing field", `{}`, false, true},
		{"extra field", `{"resolves":true,"why":"x"}`, false, true},
	} {
		t.Run(tc.label, func(t *testing.T) {
			driver := fake.New()
			scriptPropose(driver, tc.output)
			client, _, _ := testClient(t, driver, 10)
			result, err := client.ProposeSourceIssueClosure(t.Context(), authorInput())
			if err != nil || result.Resolves != tc.want || result.Fallback != tc.fall {
				t.Fatalf("ProposeSourceIssueClosure = %+v, %v", result, err)
			}
		})
	}
}

func TestPublicationAuthorNilDriverFailsafe(t *testing.T) {
	client, _, _ := testClient(t, nil, 10)
	authored, err := client.AuthorPublication(t.Context(), authorInput())
	if err != nil || !authored.Fallback || authored.Title != "" {
		t.Fatalf("explain nil-driver = %+v, %v", authored, err)
	}
	proposed, err := client.ProposeSourceIssueClosure(t.Context(), authorInput())
	if err != nil || !proposed.Fallback || proposed.Resolves {
		t.Fatalf("propose nil-driver = %+v, %v", proposed, err)
	}
}

func TestPublicationAuthorSitesFailIndependently(t *testing.T) {
	driver := fake.New()
	driver.Script(inference.PublicationAuthorExplainSiteID,
		fake.Script{Err: errors.New("unavailable")})
	scriptPropose(driver, `{"resolves":true}`)
	client, _, _ := testClient(t, driver, 10)
	authored, err := client.AuthorPublication(t.Context(), authorInput())
	if err != nil || !authored.Fallback {
		t.Fatalf("explain should fall back: %+v, %v", authored, err)
	}
	proposed, err := client.ProposeSourceIssueClosure(t.Context(), authorInput())
	if err != nil || proposed.Fallback || !proposed.Resolves {
		t.Fatalf("propose should be unaffected by the explain failure: %+v, %v", proposed, err)
	}
}

func TestAuthorInputVisibilityClasses(t *testing.T) {
	valid := `{"title":"t","body":"b","reviewer_notes":null,"evidence_refs":[],"outcome_summary":"s"}`

	// Private target: diff and both control files are sensitive; a public source
	// issue keeps its own repository's (normal) class.
	driver := fake.New()
	scriptExplain(driver, valid)
	client, _, _ := testClient(t, driver, 10)
	input := authorInput()
	input.TargetVisibility = inference.RepositoryPrivate
	input.SourceVisibility = inference.RepositoryPublic
	result, err := client.AuthorPublication(t.Context(), input)
	if err != nil || result.Fallback || result.TargetClass != domain.SensitivitySensitive ||
		result.InputClasses["diff"] != domain.SensitivitySensitive ||
		result.InputClasses["pr_template"] != domain.SensitivitySensitive ||
		result.InputClasses["instruction_snapshot"] != domain.SensitivitySensitive ||
		result.InputClasses["source_issue_title"] != domain.SensitivityNormal {
		t.Fatalf("private-target classes = %q, %+v, %v", result.TargetClass, result.InputClasses, err)
	}

	// Private source issue under a public target contributes only its reference.
	driver = fake.New()
	scriptExplain(driver, valid)
	client, _, _ = testClient(t, driver, 10)
	input = authorInput()
	input.SourceVisibility = inference.RepositoryPrivate
	if _, err := client.AuthorPublication(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	fields := driver.Requests()[0].Fields
	if fields["source_issue_title"] != "" || fields["source_issue_body"] != "" || fields["source_issue_ref"] != "#42" {
		t.Fatalf("cross-visibility fields = %+v", fields)
	}

	// Unknown visibility falls back before any call.
	driver = fake.New()
	client, _, _ = testClient(t, driver, 10)
	input = authorInput()
	input.TargetVisibility = "bogus"
	result, err = client.AuthorPublication(t.Context(), input)
	if err != nil || !result.Fallback || len(driver.Requests()) != 0 {
		t.Fatalf("invalid visibility should fall back with no call: %+v, %v, %d", result, err, len(driver.Requests()))
	}
}

func TestAuthorControlFileRejections(t *testing.T) {
	base := controlFile("valid content")
	for _, tc := range []struct {
		label string
		file  inference.ControlFile
	}{
		{"digest mismatch", inference.ControlFile{Content: "valid content", Digest: contentaddr.Sum([]byte("other")), TrustedBaseCommit: "base-sha"}},
		{"missing trusted-base", inference.ControlFile{Content: base.Content, Digest: base.Digest}},
		{"oversized", controlFile(strings.Repeat("x", (64<<10)+1))},
	} {
		t.Run(tc.label, func(t *testing.T) {
			driver := fake.New()
			client, _, _ := testClient(t, driver, 10)
			input := authorInput()
			input.PRTemplate = tc.file
			result, err := client.AuthorPublication(t.Context(), input)
			if err != nil || !result.Fallback || len(driver.Requests()) != 0 {
				t.Fatalf("control-file rejection = %+v, %v, requests=%d", result, err, len(driver.Requests()))
			}
		})
	}
}

func TestAuthorEvidenceFiltering(t *testing.T) {
	recipe := domain.Digest(contentaddr.Sum([]byte("recipe")))
	approved := map[domain.Digest]bool{recipe: true}
	// A legally publish-eligible-false artifact: an agent artifact.
	ineligible, err := domain.NewArtifact(domain.ArtifactInput{
		ID: "ev-agent", Type: domain.ArtifactKindEvidence, Digest: domain.Digest(contentaddr.Sum([]byte("ev-agent"))),
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerAgent, ProducerInvocationID: "inv-agent",
			HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: domain.EvidenceMetadata{
			MediaType: domain.EvidenceMediaTextPlain, SizeBytes: 1, CreatedAt: time.Unix(1, 0).UTC(),
			Source: domain.EvidenceSourceRun, Availability: domain.EvidenceAvailable,
		},
	}, approved)
	if err != nil {
		t.Fatal(err)
	}
	eligibleNormal := evidenceArtifact(t, "ev-normal", domain.SensitivityNormal, recipe, approved)
	eligibleSensitive := evidenceArtifact(t, "ev-sensitive", domain.SensitivitySensitive, recipe, approved)

	// Public target: only the normal eligible artifact survives.
	driver := fake.New()
	scriptExplain(driver, `{"title":"t","body":"b","reviewer_notes":null,"evidence_refs":[],"outcome_summary":"s"}`)
	client, _, _ := testClient(t, driver, 10)
	input := authorInput()
	input.ApprovedRecipes = approved
	input.Evidence = []domain.Artifact{ineligible, eligibleNormal, eligibleSensitive}
	if _, err := client.AuthorPublication(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	got := evidenceIDs(t, driver.Requests()[0].Fields["evidence_refs"])
	if len(got) != 1 || got[0] != "ev-normal" {
		t.Fatalf("public-target kept evidence = %v", got)
	}

	// Private target: the sensitive eligible artifact is also kept.
	driver = fake.New()
	scriptExplain(driver, `{"title":"t","body":"b","reviewer_notes":null,"evidence_refs":[],"outcome_summary":"s"}`)
	client, _, _ = testClient(t, driver, 10)
	input = authorInput()
	input.TargetVisibility = inference.RepositoryPrivate
	input.ApprovedRecipes = approved
	input.Evidence = []domain.Artifact{eligibleNormal, eligibleSensitive}
	if _, err := client.AuthorPublication(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	got = evidenceIDs(t, driver.Requests()[0].Fields["evidence_refs"])
	if len(got) != 2 {
		t.Fatalf("private-target kept evidence = %v", got)
	}
}

// TestAuthorForgedBitExcluded isolates the stale-bit case: a true publish_eligible
// bit that policy over the current approved set would not produce is excluded.
func TestAuthorForgedBitExcluded(t *testing.T) {
	recipe := domain.Digest(contentaddr.Sum([]byte("recipe")))
	forged := evidenceArtifact(t, "ev-forged", domain.SensitivityNormal, recipe, map[domain.Digest]bool{recipe: true})
	driver := fake.New()
	scriptExplain(driver, `{"title":"t","body":"b","reviewer_notes":null,"evidence_refs":[],"outcome_summary":"s"}`)
	client, _, _ := testClient(t, driver, 10)
	input := authorInput()
	input.Evidence = []domain.Artifact{forged}
	input.ApprovedRecipes = map[domain.Digest]bool{} // recipe no longer approved
	if _, err := client.AuthorPublication(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	if got := evidenceIDs(t, driver.Requests()[0].Fields["evidence_refs"]); len(got) != 0 {
		t.Fatalf("forged-bit artifact was not excluded: %v", got)
	}
}

func evidenceIDs(t *testing.T, field string) []string {
	t.Helper()
	var refs []struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal([]byte(field), &refs); err != nil {
		t.Fatalf("evidence_refs field %q: %v", field, err)
	}
	ids := make([]string, 0, len(refs))
	for _, r := range refs {
		ids = append(ids, r.ID)
	}
	return ids
}

// TestAuthorResultFitsDomainArtifact confirms an accepted explain result builds
// a valid domain.PublicationAuthoring.
func TestAuthorResultFitsDomainArtifact(t *testing.T) {
	recipe := domain.Digest(contentaddr.Sum([]byte("recipe")))
	approved := map[domain.Digest]bool{recipe: true}
	driver := fake.New()
	scriptExplain(driver, `{"title":"Fix crash","body":"Handles the nil case.","reviewer_notes":null,"evidence_refs":["ev-1"],"outcome_summary":"Passed and clean."}`)
	client, _, _ := testClient(t, driver, 10)
	input := authorInput()
	input.ApprovedRecipes = approved
	input.Evidence = []domain.Artifact{evidenceArtifact(t, "ev-1", domain.SensitivityNormal, recipe, approved)}
	result, err := client.AuthorPublication(t.Context(), input)
	if err != nil || result.Fallback {
		t.Fatalf("author = %+v, %v", result, err)
	}
	artifact, err := domain.NewPublicationAuthoring(domain.PublicationAuthoringInput{
		RunID: "run-1", Title: result.Title, Body: result.Body, ReviewerNotes: result.ReviewerNotes,
		EvidenceRefs: result.EvidenceRefs, OutcomeSummary: result.OutcomeSummary,
		Producer: domain.PublicationProducer{
			Site: inference.PublicationAuthorExplainSiteID, Producer: result.Producer, InputDigest: domain.Digest(result.InputDigest),
		},
		SensitivityClass: result.TargetClass, CreatedAt: time.Unix(1, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("NewPublicationAuthoring rejected an accepted result: %v", err)
	}
	if artifact.Title != result.Title || len(artifact.EvidenceRefs) != 1 {
		t.Fatalf("artifact = %+v", artifact)
	}
}
