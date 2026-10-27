package domain_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

func TestReviewDisputeRoutesRespectCanonicalScope(t *testing.T) {
	for _, path := range []string{"daemon/a.go", "app/a.go", "daemon/../app/a.go", "daemon//a.go", "/daemon/a.go", "daemon/./a.go", "daemon/a\\b.go", "daemon/a\n.go"} {
		finding := domain.Finding{Location: &domain.FindingLocation{Path: path}}
		routes := domain.ReviewDisputeRoutes(finding, []string{"daemon/**"})
		if slices.Contains(routes, domain.RouteRemediate) != (path == "daemon/a.go") {
			t.Errorf("%q routes = %v", path, routes)
		}
	}
	if slices.Contains(domain.ReviewDisputeRoutes(domain.Finding{}, []string{"**"}), domain.RouteRemediate) {
		t.Fatal("locationless remediation offered")
	}
}

// TestHumanAdjudicationEntryCarriesTheReason pins a person's reason as the
// entry's rationale, the golden of such an entry, and the refusals (#2352).
func TestHumanAdjudicationEntryCarriesTheReason(t *testing.T) {
	entry, err := domain.NewHumanAdjudicationEntry("finding-1", domain.RouteDecline, "The handle is closed by the caller.")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Rationale != "The handle is closed by the caller." || entry.HumanReason() != entry.Rationale {
		t.Fatalf("entry = %#v", entry)
	}
	body, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "finding_adjudication_entry_human_reason", append(body, '\n'))
	reasonless, err := domain.NewHumanAdjudicationEntry("finding-1", domain.RouteDefer, "")
	if err != nil || reasonless.Rationale != domain.HumanAdjudicationRationale || reasonless.HumanReason() != "" {
		t.Fatalf("reasonless entry = %#v, %v", reasonless, err)
	}
	model := domain.FindingAdjudicationEntry{Producer: domain.AdjudicationProducerModel, Rationale: "A model's view."}
	if model.HumanReason() != "" {
		t.Fatal("a model rationale read as a person's reason")
	}
	for name, tc := range map[string]struct {
		route  domain.AdjudicationRoute
		reason string
	}{
		"remediate":  {domain.RouteRemediate, "Fix it."},
		"untrimmed":  {domain.RouteDecline, " why"},
		"blank":      {domain.RouteDecline, " "},
		"invalid":    {domain.RouteDecline, "why \xff"},
		"over bound": {domain.RouteDefer, strings.Repeat("x", domain.MaxHumanAdjudicationReasonBytes+1)},
	} {
		if _, err := domain.NewHumanAdjudicationEntry("finding-1", tc.route, tc.reason); !errors.Is(err, domain.ErrInvalidHumanAdjudicationReason) {
			t.Errorf("%s: error = %v", name, err)
		}
	}
}

func TestReviewDisputeBindingPreservesUnfingerprintableMessage(t *testing.T) {
	row := domain.DisputedFinding{
		FindingID: "finding-1", Message: " \t\n", Severity: domain.FindingSeverityP1,
		OfferedRoutes: []domain.AdjudicationRoute{domain.RouteDecline, domain.RouteDefer},
	}
	binding := domain.ReviewDisputeBinding{
		RunID: "run-human", Round: 1, FindingIDs: []domain.FindingID{row.FindingID},
		CompletionEvidence: adjDigest("completion"), Findings: []domain.DisputedFinding{row},
	}
	if err := binding.Validate(); err != nil {
		t.Fatalf("unfingerprintable message rejected: %v", err)
	}
}

func TestHumanFindingAdjudicationGoldenAndRoundTrip(t *testing.T) {
	decision := domain.AdjudicationDecision{CommandID: "human-command", ItemID: domain.ProductionFindingAdjudicationItemID("run-human", 1, 1), ItemVersion: 2}
	entry, err := domain.NewHumanAdjudicationEntry("finding-1", domain.RouteDecline, "")
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := domain.NewHumanFindingAdjudication("run-human", 1, adjDigest("spec"), adjDigest("instruction"), adjDigest("policy"),
		[]domain.FindingAdjudicationEntry{entry}, decision, time.Date(2026, 10, 13, 14, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "finding_adjudication_human", append(body, '\n'))
	var decoded domain.FindingAdjudication
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatal(err)
	}
	if decoded.Digest != artifact.Digest {
		t.Fatal("human round-trip changed digest")
	}
	row := domain.DisputedFinding{
		FindingID: "finding-1", Message: "The update can be applied twice.", Severity: domain.FindingSeverityP1,
		Location:      &domain.FindingLocation{Path: "daemon/update.go", StartLine: 42, EndLine: 45},
		External:      &domain.DisputedFindingExternal{ReviewerLogin: "reviewer", ThreadID: "thread-1"},
		OfferedRoutes: []domain.AdjudicationRoute{domain.RouteRemediate, domain.RouteDecline, domain.RouteDefer},
		Adjudication: &domain.DisputedFindingAdjudication{
			Lean: ptr(domain.RouteRemediate), Confidence: domain.ConfidenceMedium,
			Rationale: "The spec requires idempotent updates.", BelowThreshold: true,
		},
	}
	binding := domain.ReviewDisputeBinding{RunID: "run-human", Round: 1, FindingIDs: []domain.FindingID{row.FindingID}, CompletionEvidence: adjDigest("completion"), Findings: []domain.DisputedFinding{row}}
	if err := binding.Validate(); err != nil {
		t.Fatal(err)
	}
	body, err = json.MarshalIndent(row, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "disputed_finding", append(body, '\n'))
	binding.FindingIDs = append(binding.FindingIDs, "finding-2")
	if err := binding.Validate(); err != nil {
		t.Fatalf("proper subset rejected: %v", err)
	}
	for name, mutate := range map[string]func(*domain.ReviewDisputeBinding){
		"duplicate": func(b *domain.ReviewDisputeBinding) {
			b.Findings = append(b.Findings, b.Findings[0])
			b.FindingIDs = append(b.FindingIDs, "finding-3")
		},
		"unbound":        func(b *domain.ReviewDisputeBinding) { b.Findings[0].FindingID = "foreign" },
		"empty message":  func(b *domain.ReviewDisputeBinding) { b.Findings[0].Message = "" },
		"missing routes": func(b *domain.ReviewDisputeBinding) { b.Findings[0].OfferedRoutes = nil },
		"invalid route": func(b *domain.ReviewDisputeBinding) {
			b.Findings[0].OfferedRoutes = []domain.AdjudicationRoute{domain.RouteDispute}
		},
		"empty rationale": func(b *domain.ReviewDisputeBinding) {
			b.Findings[0].Adjudication = &domain.DisputedFindingAdjudication{Confidence: domain.ConfidenceLow, Rationale: " "}
		},
		"invalid confidence": func(b *domain.ReviewDisputeBinding) {
			b.Findings[0].Adjudication = &domain.DisputedFindingAdjudication{Confidence: "certain", Rationale: "x"}
		},
		"invalid lean": func(b *domain.ReviewDisputeBinding) {
			b.Findings[0].Adjudication = &domain.DisputedFindingAdjudication{
				Lean: ptr(domain.AdjudicationRoute("ship")), Confidence: domain.ConfidenceLow, Rationale: "x",
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := binding
			candidate.Findings = slices.Clone(binding.Findings)
			mutate(&candidate)
			if candidate.Validate() == nil {
				t.Fatal("invalid subset accepted")
			}
		})
	}
}

func TestDisputedFindingAdjudicationFor(t *testing.T) {
	t.Parallel()
	unknown := domain.ProposedUnknown
	noLean, err := domain.NewModelAdjudicationEntry("f", domain.GoalRequired, &unknown,
		domain.RouteParkUnknown, domain.ConfidenceLow, "no route fits", nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	decline, err := domain.NewModelAdjudicationEntry("f", domain.GoalContradictory, nil,
		domain.RouteDecline, domain.ConfidenceHigh, "contradicts the spec", nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := domain.NewEngineAdjudicationEntry("f", domain.GoalRequired,
		ptr(domain.CompatibilityAllowed), domain.RouteRemediate, "in scope", nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	human, err := domain.NewHumanAdjudicationEntry("f", domain.RouteDefer, "")
	if err != nil {
		t.Fatal(err)
	}
	demoted := demotedRemediationFixture(t)
	for _, tc := range []struct {
		name  string
		entry *domain.FindingAdjudicationEntry
		want  *domain.DisputedFindingAdjudication
	}{
		{"no entry", nil, nil},
		{"engine fact", &engine, nil},
		{"human decision", &human, nil},
		{"demoted lean", &demoted, &domain.DisputedFindingAdjudication{
			Lean: ptr(domain.RouteRemediate), Confidence: domain.ConfidenceMedium,
			Rationale: demoted.Rationale, BelowThreshold: true,
		}},
		{"unknown answer", &noLean, &domain.DisputedFindingAdjudication{
			Confidence: domain.ConfidenceLow, Rationale: "no route fits", BelowThreshold: true,
		}},
		{"accepted decline", &decline, &domain.DisputedFindingAdjudication{
			Lean: ptr(domain.RouteDecline), Confidence: domain.ConfidenceHigh,
			Rationale: "contradicts the spec", BelowThreshold: false,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := domain.DisputedFindingAdjudicationFor(tc.entry, domain.DispatchThresholdHigh)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("block = %+v, want %+v", got, tc.want)
			}
		})
	}
	if _, err := domain.DisputedFindingAdjudicationFor(&decline, "sometimes"); err == nil {
		t.Fatal("invalid threshold accepted")
	}
}

func TestReviewDisputeBindingMatchesToleratesOnlyLegacyRows(t *testing.T) {
	t.Parallel()
	block := &domain.DisputedFindingAdjudication{
		Lean: ptr(domain.RouteRemediate), Confidence: domain.ConfidenceMedium, Rationale: "x", BelowThreshold: true,
	}
	derived := &domain.ReviewDisputeBinding{
		RunID: "run", Round: 1, FindingIDs: []domain.FindingID{"f"}, CompletionEvidence: adjDigest("c"),
		Findings: []domain.DisputedFinding{{
			FindingID: "f", Message: "m", OfferedRoutes: []domain.AdjudicationRoute{domain.RouteDecline},
			Adjudication: block,
		}},
	}
	withRow := func(row domain.DisputedFinding) *domain.ReviewDisputeBinding {
		out := *derived
		out.Findings = []domain.DisputedFinding{row}
		return &out
	}
	legacy := derived.Findings[0]
	legacy.Adjudication = nil
	if !domain.ReviewDisputeBindingMatches(withRow(legacy), derived) {
		t.Fatal("legacy row without a block rejected")
	}
	if domain.ReviewDisputeBindingMatches(derived, withRow(legacy)) {
		t.Fatal("stored block accepted where none is derived")
	}
	other := derived.Findings[0]
	changed := *block
	changed.BelowThreshold = false
	other.Adjudication = &changed
	if domain.ReviewDisputeBindingMatches(withRow(other), derived) {
		t.Fatal("different block accepted")
	}
	legacy.OfferedRoutes = []domain.AdjudicationRoute{domain.RouteDefer}
	if domain.ReviewDisputeBindingMatches(withRow(legacy), derived) {
		t.Fatal("legacy tolerance relaxed another field")
	}
	if !domain.ReviewDisputeBindingMatches(nil, nil) || domain.ReviewDisputeBindingMatches(nil, derived) {
		t.Fatal("nil bindings mismatched")
	}
}
