package domain_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

func adjDigest(seed string) domain.Digest {
	return domain.Digest(contentaddr.Sum([]byte(seed)))
}

// TestAdjudicationEnumRegistrationReferences pins every new vocabulary's
// registration slice so the enum-registration ratchet sees a test reference.
func TestAdjudicationEnumRegistrationReferences(t *testing.T) {
	t.Parallel()
	if len(domain.AllGoalRelationships) != 4 ||
		len(domain.AllWorkUnitCompatibilities) != 5 ||
		len(domain.AllProposedCompatibilities) != 4 ||
		len(domain.AllAdjudicationRoutes) != 9 ||
		len(domain.AllAdjudicationProducers) != 3 ||
		len(domain.AllAdjudicationConfidences) != 3 ||
		len(domain.AllDispatchThresholds) != 2 {
		t.Fatalf("adjudication vocabulary sizes changed; update the §7 fixtures")
	}
	if domain.DefaultDispatchThreshold != domain.DispatchThresholdHigh {
		t.Fatalf("default dispatch threshold = %q, want high", domain.DefaultDispatchThreshold)
	}
}

// validAdjudicationRows is the §7 validity table: the complete set of valid
// (goal, compatibility, route) rows. Compatibility is the empty string where it
// is absent (present exactly under `required`).
var validAdjudicationRows = map[[3]string]struct{}{
	{"required", "allowed", "remediate"}:                                {},
	{"required", "work_unit_revision_required", "park_revision"}:        {},
	{"required", "separate_work_required", "park_separate_work"}:        {},
	{"required", "human_decision_required", "attention_human_decision"}: {},
	{"required", "unknown", "park_unknown"}:                             {},
	{"adjacent", "", "defer"}:                                           {},
	{"contradictory", "", "decline"}:                                    {},
	{"contradictory", "", "dispute"}:                                    {},
	{"unclear", "", "attention_unclear"}:                                {},
}

// engineFastPathRows is the single §7 row an engine fast-path fact may carry:
// the presumptive-`allowed` remediation row. The no-model fast path is
// one-directional toward remediation; every other valid row (including the
// `unknown` park, which is a model not-accepted representation) is model
// residue.
var engineFastPathRows = map[[3]string]struct{}{
	{"required", "allowed", "remediate"}: {},
}

// TestAdjudicationRowCrossProduct enumerates every (producer × goal ×
// compatibility-option × route) cell and asserts exactly the valid combinations
// pass. A cell is valid when it is one of the eight §7 axis/route rows AND its
// producer may carry that row: an engine entry and an engine-model composition
// only the deterministic allowed-remediation row, and a model entry every row
// but that engine-authorized row. The rest of the cross product is rejected —
// compatibility present exactly under `required`, the route a function of the
// axes, and the producer/row trust boundary enforced.
func TestAdjudicationRowCrossProduct(t *testing.T) {
	t.Parallel()
	compatOptions := []*domain.WorkUnitCompatibility{nil}
	for _, c := range domain.AllWorkUnitCompatibilities {
		compatOptions = append(compatOptions, ptr(c))
	}
	allowedRow := [3]string{"required", "allowed", "remediate"}
	for _, producer := range domain.AllAdjudicationProducers {
		valid := 0
		for _, goal := range domain.AllGoalRelationships {
			for _, compat := range compatOptions {
				for _, route := range domain.AllAdjudicationRoutes {
					compatKey := ""
					if compat != nil {
						compatKey = string(*compat)
					}
					key := [3]string{string(goal), compatKey, string(route)}
					_, rowValid := validAdjudicationRows[key]
					wantValid := rowValid
					switch producer {
					case domain.AdjudicationProducerEngine:
						_, engineOK := engineFastPathRows[key]
						wantValid = wantValid && engineOK
					case domain.AdjudicationProducerModel:
						wantValid = wantValid && key != allowedRow
					case domain.AdjudicationProducerEngineModel:
						_, engineOK := engineFastPathRows[key]
						wantValid = wantValid && engineOK
					}
					entry := domain.FindingAdjudicationEntry{
						FindingID: "finding-a", Producer: producer,
						GoalRelationship: goal, Compatibility: compat, Route: route,
						Rationale: "because",
					}
					// A model-backed entry additionally requires proposal confidence;
					// supply it so the row is judged on the axis/producer rules.
					if producer == domain.AdjudicationProducerModel ||
						producer == domain.AdjudicationProducerEngineModel {
						entry.Confidence = ptr(domain.ConfidenceHigh)
					}
					err := entry.Validate()
					if wantValid {
						valid++
						if err != nil {
							t.Errorf("producer %s row %v: want valid, got %v", producer, key, err)
						}
					} else if err == nil {
						t.Errorf("producer %s row %v: want rejected, got valid", producer, key)
					}
				}
			}
		}
		want := len(engineFastPathRows)
		if producer == domain.AdjudicationProducerModel {
			want = len(validAdjudicationRows) - 1 // every row but the engine-only allowed row
		}
		if valid != want {
			t.Fatalf("producer %s validated %d rows, want %d", producer, valid, want)
		}
	}
}

// TestEngineEntryRestrictedToFastPath pins the engine trust boundary: an engine
// label on any row but `required`/`allowed`/`remediate` is rejected, even a
// valid §7 axis/route combination. The `unknown` park is included because it is
// a model not-accepted representation, not an engine fast-path fact — the
// no-model fast path is one-directional toward remediation (plan §7).
func TestEngineEntryRestrictedToFastPath(t *testing.T) {
	t.Parallel()
	rejected := []struct {
		name   string
		goal   domain.GoalRelationship
		compat *domain.WorkUnitCompatibility
		route  domain.AdjudicationRoute
	}{
		{"adjacent deferral", domain.GoalAdjacent, nil, domain.RouteDefer},
		{"contradictory decline", domain.GoalContradictory, nil, domain.RouteDecline},
		{"contradictory dispute", domain.GoalContradictory, nil, domain.RouteDispute},
		{"unclear attention", domain.GoalUnclear, nil, domain.RouteAttentionUnclear},
		{"unknown park", domain.GoalRequired, ptr(domain.CompatibilityUnknown), domain.RouteParkUnknown},
		{"human decision", domain.GoalRequired, ptr(domain.CompatibilityHumanDecision), domain.RouteAttentionHumanDecision},
		{"work unit revision", domain.GoalRequired, ptr(domain.CompatibilityWorkUnitRevision), domain.RouteParkRevision},
		{"separate work", domain.GoalRequired, ptr(domain.CompatibilitySeparateWork), domain.RouteParkSeparateWork},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			entry := domain.FindingAdjudicationEntry{
				FindingID: "f", Producer: domain.AdjudicationProducerEngine,
				GoalRelationship: tc.goal, Compatibility: tc.compat, Route: tc.route, Rationale: "x",
			}
			if err := entry.Validate(); !errors.Is(err, domain.ErrEngineEntryNonDeterministicRow) {
				t.Fatalf("engine+%s: got %v, want ErrEngineEntryNonDeterministicRow", tc.name, err)
			}
		})
	}
	// The sole engine fast-path row remains valid.
	remediate := domain.FindingAdjudicationEntry{
		FindingID: "f", Producer: domain.AdjudicationProducerEngine,
		GoalRelationship: domain.GoalRequired, Compatibility: ptr(domain.CompatibilityAllowed),
		Route: domain.RouteRemediate, Rationale: "x",
	}
	if err := remediate.Validate(); err != nil {
		t.Errorf("engine+required+allowed+remediate: got %v, want valid", err)
	}
}

func TestAdjudicationProducerAndConfidenceRules(t *testing.T) {
	t.Parallel()
	required := ptr(domain.CompatibilityUnknown)

	// A model entry carrying `allowed` is rejected at validation.
	modelAllowed := domain.FindingAdjudicationEntry{
		FindingID: "f", Producer: domain.AdjudicationProducerModel,
		GoalRelationship: domain.GoalRequired, Compatibility: ptr(domain.CompatibilityAllowed),
		Route: domain.RouteRemediate, Rationale: "x", Confidence: ptr(domain.ConfidenceHigh),
	}
	if err := modelAllowed.Validate(); !errors.Is(err, domain.ErrModelEntryMintsAllowed) {
		t.Errorf("model+allowed: got %v, want ErrModelEntryMintsAllowed", err)
	}

	// Confidence is present exactly on model-backed entries. Use the engine
	// fast-path row so the confidence rule, not the engine-row rule, rejects it.
	engineWithConfidence := domain.FindingAdjudicationEntry{
		FindingID: "f", Producer: domain.AdjudicationProducerEngine,
		GoalRelationship: domain.GoalRequired, Compatibility: ptr(domain.CompatibilityAllowed),
		Route: domain.RouteRemediate, Rationale: "x", Confidence: ptr(domain.ConfidenceHigh),
	}
	if err := engineWithConfidence.Validate(); !errors.Is(err, domain.ErrAdjudicationConfidenceMisplaced) {
		t.Errorf("engine+confidence: got %v, want ErrAdjudicationConfidenceMisplaced", err)
	}
	modelNoConfidence := domain.FindingAdjudicationEntry{
		FindingID: "f", Producer: domain.AdjudicationProducerModel,
		GoalRelationship: domain.GoalRequired, Compatibility: required,
		Route: domain.RouteParkUnknown, Rationale: "x",
	}
	if err := modelNoConfidence.Validate(); !errors.Is(err, domain.ErrAdjudicationConfidenceMisplaced) {
		t.Errorf("model-no-confidence: got %v, want ErrAdjudicationConfidenceMisplaced", err)
	}

	// The model constructor cannot mint `allowed`: ProposedCompatibility has no
	// `allowed` member, and its widening yields the twin non-`allowed` value.
	entry, err := domain.NewModelAdjudicationEntry(
		"f", domain.GoalRequired, ptr(domain.ProposedWorkUnitRevision),
		domain.RouteParkRevision, domain.ConfidenceHigh, "x", nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("model entry construct: %v", err)
	}
	if entry.Compatibility == nil || *entry.Compatibility != domain.CompatibilityWorkUnitRevision {
		t.Errorf("widened compatibility = %v, want work_unit_revision_required", entry.Compatibility)
	}

	// The mixed constructor fixes the engine half to allowed/remediate while
	// retaining the model-produced goal and confidence.
	mixed, err := domain.NewEngineModelAdjudicationEntry(
		"f", domain.GoalRequired, domain.ConfidenceHigh, "x", nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("engine-model entry construct: %v", err)
	}
	if mixed.Compatibility == nil || *mixed.Compatibility != domain.CompatibilityAllowed ||
		mixed.Route != domain.RouteRemediate || mixed.Confidence == nil {
		t.Errorf("engine-model entry = %+v, want allowed remediation with confidence", mixed)
	}
	if _, err := domain.NewEngineModelAdjudicationEntry(
		"f", domain.GoalAdjacent, domain.ConfidenceHigh, "x", nil, nil, nil, nil, nil,
	); !errors.Is(err, domain.ErrAdjudicationAxisMismatch) {
		t.Errorf("engine-model adjacent = %v, want ErrAdjudicationAxisMismatch", err)
	}
	forgedMixed := mixed
	forgedMixed.Compatibility = ptr(domain.CompatibilityUnknown)
	forgedMixed.Route = domain.RouteParkUnknown
	if err := forgedMixed.Validate(); !errors.Is(err, domain.ErrEngineModelEntryNonRemediationRow) {
		t.Errorf("forged engine-model row = %v, want ErrEngineModelEntryNonRemediationRow", err)
	}
}

func TestAdjudicationEntryRejectsInvalidUTF8FreeText(t *testing.T) {
	t.Parallel()
	// json.Marshal would silently rewrite invalid UTF-8 to U+FFFD, so every
	// free-text field must be rejected before it can be hashed or persisted.
	base := domain.FindingAdjudicationEntry{
		FindingID: "f", Producer: domain.AdjudicationProducerEngine,
		GoalRelationship: domain.GoalRequired, Compatibility: ptr(domain.CompatibilityAllowed),
		Route: domain.RouteRemediate, Rationale: "ok",
	}
	rationaleBad := base
	rationaleBad.Rationale = "bad \xff byte"
	if err := rationaleBad.Validate(); !errors.Is(err, domain.ErrFindingAdjudicationInconsistent) {
		t.Errorf("invalid-utf8 rationale: got %v, want ErrFindingAdjudicationInconsistent", err)
	}
	listBad := base
	listBad.Evidence = []string{"bad \xff item"}
	if err := listBad.Validate(); !errors.Is(err, domain.ErrFindingAdjudicationInconsistent) {
		t.Errorf("invalid-utf8 list item: got %v, want ErrFindingAdjudicationInconsistent", err)
	}
}

func TestAdjudicationAcceptedAndNotAcceptedRepresentation(t *testing.T) {
	t.Parallel()
	// Engine entries are always accepted.
	engine := domain.FindingAdjudicationEntry{
		FindingID: "f", Producer: domain.AdjudicationProducerEngine,
		GoalRelationship: domain.GoalRequired, Compatibility: ptr(domain.CompatibilityAllowed),
		Route: domain.RouteRemediate, Rationale: "x",
	}
	if ok, err := engine.Accepted(domain.DispatchThresholdHigh); err != nil || !ok {
		t.Errorf("engine accepted = (%v, %v), want (true, nil)", ok, err)
	}
	mixed, err := domain.NewEngineModelAdjudicationEntry(
		"mixed", domain.GoalRequired, domain.ConfidenceMedium, "x", nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := mixed.Accepted(domain.DispatchThresholdHigh); err != nil || ok {
		t.Errorf("below-threshold engine-model accepted = (%v, %v), want (false, nil)", ok, err)
	}
	if ok, err := mixed.Accepted(domain.DispatchThresholdMedium); err != nil || !ok {
		t.Errorf("meeting-threshold engine-model accepted = (%v, %v), want (true, nil)", ok, err)
	}

	cases := []struct {
		name       string
		confidence *domain.AdjudicationConfidence
		threshold  domain.DispatchThreshold
		wantOK     bool
		wantErr    error
	}{
		// Accepted validates first, so a model entry with absent or out-of-scale
		// confidence fails closed with an error rather than a silent not-accepted.
		{"absent under high", nil, domain.DispatchThresholdHigh, false, domain.ErrAdjudicationConfidenceMisplaced},
		{"out of scale", ptr(domain.AdjudicationConfidence("elevated")), domain.DispatchThresholdMedium, false, domain.ErrInvalidAdjudicationConfidence},
		{"below threshold", ptr(domain.ConfidenceLow), domain.DispatchThresholdMedium, false, nil},
		{"below high", ptr(domain.ConfidenceMedium), domain.DispatchThresholdHigh, false, nil},
		{"meets medium", ptr(domain.ConfidenceMedium), domain.DispatchThresholdMedium, true, nil},
		{"meets high", ptr(domain.ConfidenceHigh), domain.DispatchThresholdHigh, true, nil},
		{"invalid threshold", ptr(domain.ConfidenceHigh), domain.DispatchThreshold("low"), false, domain.ErrInvalidDispatchThreshold},
		{"zero threshold", ptr(domain.ConfidenceHigh), domain.DispatchThreshold(""), false, domain.ErrInvalidDispatchThreshold},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry := domain.FindingAdjudicationEntry{
				FindingID: "f", Producer: domain.AdjudicationProducerModel,
				GoalRelationship: domain.GoalContradictory, Route: domain.RouteDecline,
				Rationale: "x", Confidence: tc.confidence,
			}
			ok, err := entry.Accepted(tc.threshold)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil || ok != tc.wantOK {
				t.Fatalf("accepted = (%v, %v), want (%v, nil)", ok, err, tc.wantOK)
			}
		})
	}

	// Accepted fails closed on a structurally invalid entry that bypassed a
	// constructor, rather than routing it to dispatch. A model-minted `allowed`
	// with meeting confidence is not accepted, and a forged engine row is not
	// accepted, even though the naive producer switch would have returned true.
	modelAllowed := domain.FindingAdjudicationEntry{
		FindingID: "f", Producer: domain.AdjudicationProducerModel,
		GoalRelationship: domain.GoalRequired, Compatibility: ptr(domain.CompatibilityAllowed),
		Route: domain.RouteRemediate, Rationale: "x", Confidence: ptr(domain.ConfidenceHigh),
	}
	if ok, err := modelAllowed.Accepted(domain.DispatchThresholdHigh); ok || !errors.Is(err, domain.ErrModelEntryMintsAllowed) {
		t.Errorf("model+allowed accepted = (%v, %v), want (false, ErrModelEntryMintsAllowed)", ok, err)
	}
	forgedEngine := domain.FindingAdjudicationEntry{
		FindingID: "f", Producer: domain.AdjudicationProducerEngine,
		GoalRelationship: domain.GoalAdjacent, Route: domain.RouteDefer, Rationale: "x",
	}
	if ok, err := forgedEngine.Accepted(domain.DispatchThresholdHigh); ok || !errors.Is(err, domain.ErrEngineEntryNonDeterministicRow) {
		t.Errorf("forged engine accepted = (%v, %v), want (false, ErrEngineEntryNonDeterministicRow)", ok, err)
	}

	// unknown is the not-accepted representation only where compatibility exists.
	required := domain.FindingAdjudicationEntry{GoalRelationship: domain.GoalRequired}
	if rep := required.NotAcceptedRepresentation(); rep == nil || *rep != domain.CompatibilityUnknown {
		t.Errorf("required not-accepted representation = %v, want unknown", rep)
	}
	for _, goal := range []domain.GoalRelationship{domain.GoalAdjacent, domain.GoalContradictory, domain.GoalUnclear} {
		nonRequired := domain.FindingAdjudicationEntry{GoalRelationship: goal}
		if rep := nonRequired.NotAcceptedRepresentation(); rep != nil {
			t.Errorf("%s not-accepted representation = %v, want nil", goal, rep)
		}
	}
}

func TestDeriveRemediationSurfaceFailsClosed(t *testing.T) {
	t.Parallel()
	existsBoth := func(string) (bool, bool, error) { return true, true, nil }
	existsNeither := func(string) (bool, bool, error) { return false, false, nil }
	candidateAdded := func(string) (bool, bool, error) { return false, true, nil }

	cases := []struct {
		name     string
		location *domain.FindingLocation
		resolve  func(string) (bool, bool, error)
		wantPath string
	}{
		{"nil location", nil, existsBoth, ""},
		{"absolute path", &domain.FindingLocation{Path: "/etc/passwd"}, existsBoth, ""},
		{"traversal path", &domain.FindingLocation{Path: "a/../b"}, existsBoth, ""},
		{"backslash path", &domain.FindingLocation{Path: "a\\b"}, existsBoth, ""},
		{"control character path", &domain.FindingLocation{Path: "a\x00b"}, existsBoth, ""},
		{"unresolvable", &domain.FindingLocation{Path: "daemon/x.go"}, existsNeither, ""},
		{"resolves in base and candidate", &domain.FindingLocation{Path: "daemon/x.go"}, existsBoth, "daemon/x.go"},
		{"candidate-added file keeps its route", &domain.FindingLocation{Path: "daemon/new.go"}, candidateAdded, "daemon/new.go"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := domain.DeriveRemediationSurface(tc.location, tc.resolve)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantPath == "" {
				if got != nil {
					t.Fatalf("got surface %v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("got nil surface, want derived surface")
			}
			var matchedPath string
			compatibility := domain.EngineCompatibility(got, nil, func(_ []string, path string) bool {
				matchedPath = path
				return true
			})
			if compatibility != domain.CompatibilityAllowed || matchedPath != tc.wantPath {
				t.Fatalf("derived surface = (%q, %q), want (%q, %q)",
					compatibility, matchedPath, domain.CompatibilityAllowed, tc.wantPath)
			}
		})
	}

	// A resolve error propagates.
	boom := errors.New("resolve failed")
	if _, err := domain.DeriveRemediationSurface(&domain.FindingLocation{Path: "daemon/x.go"},
		func(string) (bool, bool, error) { return false, false, boom }); !errors.Is(err, boom) {
		t.Fatalf("resolve error = %v, want boom", err)
	}
}

func TestEngineCompatibilityIsSoleAllowedProducer(t *testing.T) {
	t.Parallel()
	declared := []string{"daemon/internal/domain/", "daemon/migrations/"}
	derive := func(path string) *domain.RemediationSurface {
		t.Helper()
		surface, err := domain.DeriveRemediationSurface(
			&domain.FindingLocation{Path: path},
			func(string) (bool, bool, error) { return true, false, nil },
		)
		if err != nil || surface == nil {
			t.Fatalf("derive surface %q = (%v, %v), want non-nil without error", path, surface, err)
		}
		return surface
	}
	// A test-local prefix matcher stands in for the injected importer allowlist
	// matcher; the glob semantics are pathfold's own tested concern.
	match := func(prefixes []string, p string) bool {
		for _, prefix := range prefixes {
			if strings.HasPrefix(p, prefix) {
				return true
			}
		}
		return false
	}

	// A nil surface never yields allowed, even with a matcher that would match.
	if got := domain.EngineCompatibility(nil, declared, func([]string, string) bool { return true }); got != domain.CompatibilityUnknown {
		t.Errorf("nil surface = %q, want unknown", got)
	}
	// The externally constructible zero value is not a derived capability and
	// never yields allowed, even with a matcher that would match.
	if got := domain.EngineCompatibility(&domain.RemediationSurface{}, declared, func([]string, string) bool { return true }); got != domain.CompatibilityUnknown {
		t.Errorf("zero surface = %q, want unknown", got)
	}
	// A contained surface is allowed.
	if got := domain.EngineCompatibility(derive("daemon/internal/domain/x.go"), declared, match); got != domain.CompatibilityAllowed {
		t.Errorf("contained surface = %q, want allowed", got)
	}
	// A surface that exits the declared paths fails closed to unknown.
	if got := domain.EngineCompatibility(derive("app/x.swift"), declared, match); got != domain.CompatibilityUnknown {
		t.Errorf("out-of-scope surface = %q, want unknown", got)
	}
}

func validAdjudicationFixture(t *testing.T) domain.FindingAdjudication {
	t.Helper()
	engine, err := domain.NewEngineAdjudicationEntry(
		"finding-0001", domain.GoalRequired, ptr(domain.CompatibilityAllowed),
		domain.RouteRemediate, "in declared scope", []string{"declared-path containment"}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("engine entry: %v", err)
	}
	model, err := domain.NewModelAdjudicationEntry(
		"finding-0002", domain.GoalContradictory, nil, domain.RouteDecline,
		domain.ConfidenceHigh, "contradicts the approved spec",
		[]string{"spec §2"}, []string{"AGENTS.md scope discipline"}, []string{"spec is authoritative"},
		[]string{"defer to a separate work unit"}, []string{"which spec clause governs?"})
	if err != nil {
		t.Fatalf("model entry: %v", err)
	}
	artifact, err := domain.NewFindingAdjudication(
		"run-abc", 2,
		adjDigest("approved-spec"), adjDigest("instruction-snapshot"), adjDigest("resolved-policy"),
		[]domain.FindingAdjudicationEntry{model, engine}, // deliberately unsorted
		time.Date(2026, 8, 21, 15, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("artifact: %v", err)
	}
	return artifact
}

func successorAdjudicationFixture(t *testing.T) domain.FindingAdjudication {
	t.Helper()
	prior := validAdjudicationFixture(t)
	entries := slices.Clone(prior.Entries)
	entries[1].Rationale = "feedback identified the governing exception"
	successor, err := domain.NewSuccessorFindingAdjudication(
		prior,
		domain.AdjudicationFeedback{
			InvocationID: "invocation-discuss-1", ConversationID: "conversation-adjudication-1",
			ThroughSequence: 2, PrefixDigest: adjDigest("conversation-prefix"),
		},
		entries,
		time.Date(2026, 8, 21, 15, 5, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("successor adjudication: %v", err)
	}
	return successor
}

func TestFindingAdjudicationRoundTripAndDigestStability(t *testing.T) {
	t.Parallel()
	artifact := validAdjudicationFixture(t)

	// Constructor sorts entries by finding id.
	if artifact.Entries[0].FindingID != "finding-0001" || artifact.Entries[1].FindingID != "finding-0002" {
		t.Fatalf("entries not sorted: %v", artifact.Entries)
	}

	body, err := artifact.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := domain.DecodeFindingAdjudication(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	reencoded, err := decoded.Encode()
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if string(body) != string(reencoded) {
		t.Fatalf("re-encode not deterministic:\n%s\n%s", body, reencoded)
	}
	if decoded.Digest != artifact.Digest {
		t.Fatalf("digest changed across round-trip: %q vs %q", decoded.Digest, artifact.Digest)
	}
	if decoded.Revision != 1 || decoded.PredecessorDigest != nil || decoded.Feedback != nil {
		t.Fatalf("legacy initial normalized to %+v, want revision 1 without provenance", decoded)
	}
	recomputed, err := decoded.ComputeDigest()
	if err != nil {
		t.Fatalf("recompute: %v", err)
	}
	if recomputed != artifact.Digest {
		t.Fatalf("recomputed digest %q, want %q", recomputed, artifact.Digest)
	}
}

func TestFindingAdjudicationSuccessorRoundTrip(t *testing.T) {
	t.Parallel()
	successor := successorAdjudicationFixture(t)
	prior := validAdjudicationFixture(t)
	if successor.Revision != 2 || successor.PredecessorDigest == nil ||
		*successor.PredecessorDigest != prior.Digest || successor.Feedback == nil {
		t.Fatalf("successor provenance = %+v", successor)
	}
	body, err := successor.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := domain.DecodeFindingAdjudication(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Digest != successor.Digest || decoded.Revision != 2 ||
		decoded.PredecessorDigest == nil || *decoded.PredecessorDigest != prior.Digest ||
		decoded.Feedback == nil || *decoded.Feedback != *successor.Feedback {
		t.Fatalf("decoded successor = %+v, want %+v", decoded, successor)
	}
}

func TestFindingAdjudicationRevisionRules(t *testing.T) {
	t.Parallel()
	initial := validAdjudicationFixture(t)
	digest := adjDigest("predecessor")
	feedback := domain.AdjudicationFeedback{
		InvocationID: "invocation-1", ConversationID: "conversation-1",
		ThroughSequence: 1, PrefixDigest: adjDigest("prefix"),
	}
	for _, tc := range []struct {
		name   string
		mutate func(*domain.FindingAdjudication)
	}{
		{"non-positive revision", func(a *domain.FindingAdjudication) { a.Revision = 0 }},
		{"initial predecessor", func(a *domain.FindingAdjudication) { a.PredecessorDigest = &digest }},
		{"initial feedback", func(a *domain.FindingAdjudication) { a.Feedback = &feedback }},
		{"successor missing predecessor", func(a *domain.FindingAdjudication) { a.Revision = 2; a.Feedback = &feedback }},
		{"successor invalid predecessor", func(a *domain.FindingAdjudication) {
			a.Revision = 2
			bad := domain.Digest("not-a-digest")
			a.PredecessorDigest, a.Feedback = &bad, &feedback
		}},
		{"successor missing feedback", func(a *domain.FindingAdjudication) { a.Revision = 2; a.PredecessorDigest = &digest }},
		{"feedback missing invocation", func(a *domain.FindingAdjudication) {
			a.Revision, a.PredecessorDigest = 2, &digest
			bad := feedback
			bad.InvocationID = ""
			a.Feedback = &bad
		}},
		{"feedback missing conversation", func(a *domain.FindingAdjudication) {
			a.Revision, a.PredecessorDigest = 2, &digest
			bad := feedback
			bad.ConversationID = ""
			a.Feedback = &bad
		}},
		{"feedback non-positive sequence", func(a *domain.FindingAdjudication) {
			a.Revision, a.PredecessorDigest = 2, &digest
			bad := feedback
			bad.ThroughSequence = 0
			a.Feedback = &bad
		}},
		{"feedback invalid prefix digest", func(a *domain.FindingAdjudication) {
			a.Revision, a.PredecessorDigest = 2, &digest
			bad := feedback
			bad.PrefixDigest = "invalid"
			a.Feedback = &bad
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			artifact := initial
			tc.mutate(&artifact)
			if err := artifact.Validate(); err == nil {
				t.Fatal("Validate succeeded, want revision-shape rejection")
			}
		})
	}

	changed := slices.Clone(initial.Entries[:1])
	if _, err := domain.NewSuccessorFindingAdjudication(
		initial, feedback, changed, initial.CreatedAt.Add(time.Minute),
	); !errors.Is(err, domain.ErrFindingAdjudicationInconsistent) {
		t.Fatalf("changed finding batch = %v, want ErrFindingAdjudicationInconsistent", err)
	}
}

func TestFindingAdjudicationAuthorizesFinalDisposition(t *testing.T) {
	t.Parallel()
	declined := validAdjudicationFixture(t)
	adjacentEntry, err := domain.NewModelAdjudicationEntry(
		"finding-adjacent", domain.GoalAdjacent, nil, domain.RouteDefer,
		domain.ConfidenceHigh, "adjacent to the approved work unit",
		nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	disputeEntry, err := domain.NewModelAdjudicationEntry(
		"finding-dispute", domain.GoalContradictory, nil, domain.RouteDispute,
		domain.ConfidenceHigh, "requires operator judgment",
		nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	disputed, err := domain.NewFindingAdjudication(
		"run-disputed", 1,
		adjDigest("approved-spec"), adjDigest("instruction-snapshot"), adjDigest("resolved-policy"),
		[]domain.FindingAdjudicationEntry{disputeEntry},
		time.Date(2026, 8, 21, 16, 1, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	deferred, err := domain.NewFindingAdjudication(
		"run-adjacent", 1,
		adjDigest("approved-spec"), adjDigest("instruction-snapshot"), adjDigest("resolved-policy"),
		[]domain.FindingAdjudicationEntry{adjacentEntry},
		time.Date(2026, 8, 21, 16, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name        string
		artifact    domain.FindingAdjudication
		findingID   domain.FindingID
		disposition domain.ReviewDisposition
		wantErr     bool
	}{
		{"contradictory decline", declined, "finding-0002", domain.ReviewDispositionDeclined, false},
		{"contradictory dispute admits decline", disputed, "finding-dispute", domain.ReviewDispositionDeclined, false},
		{"adjacent defer", deferred, "finding-adjacent", domain.ReviewDispositionDeferred, false},
		{"contradictory defer", declined, "finding-0002", domain.ReviewDispositionDeferred, true},
		{"adjacent decline", deferred, "finding-adjacent", domain.ReviewDispositionDeclined, true},
		{"fixed", declined, "finding-0002", domain.ReviewDispositionFixed, true},
		{"invalid disposition", declined, "finding-0002", domain.ReviewDisposition("ignored"), true},
		{"absent finding", declined, "finding-absent", domain.ReviewDispositionDeclined, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.artifact.AuthorizesFinalDisposition(tc.findingID, tc.disposition)
			if tc.wantErr && !errors.Is(err, domain.ErrInvalidDispositionAdjudication) {
				t.Fatalf("authorization = %v, want ErrInvalidDispositionAdjudication", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("authorization = %v, want nil", err)
			}
		})
	}
	tampered := declined
	tampered.Digest = adjDigest("tampered")
	if err := tampered.AuthorizesFinalDisposition(
		"finding-0002", domain.ReviewDispositionDeclined,
	); !errors.Is(err, domain.ErrFindingAdjudicationDigestMismatch) {
		t.Fatalf("tampered artifact authorization = %v, want digest mismatch", err)
	}
}

// TestFindingAdjudicationGolden pins the canonical encoding. The full-artifact
// golden shows an engine entry with an explicit null confidence beside a model
// entry with confidence present; entry goldens pin the model-only and composed
// engine-model shapes.
func TestFindingAdjudicationGolden(t *testing.T) {
	t.Parallel()
	fixture := validAdjudicationFixture(t)
	body, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		t.Fatalf("marshal artifact: %v", err)
	}
	golden.Assert(t, "finding_adjudication", append(body, '\n'))

	successorBody, err := json.MarshalIndent(successorAdjudicationFixture(t), "", "  ")
	if err != nil {
		t.Fatalf("marshal successor artifact: %v", err)
	}
	golden.Assert(t, "finding_adjudication_successor", append(successorBody, '\n'))

	model := fixture.Entries[1]
	entryBody, err := json.MarshalIndent(model, "", "  ")
	if err != nil {
		t.Fatalf("marshal model entry: %v", err)
	}
	golden.Assert(t, "finding_adjudication_entry_model", append(entryBody, '\n'))

	mixed, err := domain.NewEngineModelAdjudicationEntry(
		"finding-mixed", domain.GoalRequired, domain.ConfidenceMedium,
		"the model judged the finding required and the engine kept remediation in scope",
		[]string{"declared-path containment", "approved spec §2"}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("mixed entry: %v", err)
	}
	mixedBody, err := json.MarshalIndent(mixed, "", "  ")
	if err != nil {
		t.Fatalf("marshal mixed entry: %v", err)
	}
	golden.Assert(t, "finding_adjudication_entry_engine_model", append(mixedBody, '\n'))
}

func TestFindingAdjudicationDecodeRejects(t *testing.T) {
	t.Parallel()
	artifact := validAdjudicationFixture(t)
	body, err := artifact.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	// A tampered digest is rejected by revalidation on decode.
	tampered := artifact
	tampered.Digest = adjDigest("not-the-real-digest")
	tamperedBody, err := json.Marshal(tampered)
	if err != nil {
		t.Fatalf("marshal tampered: %v", err)
	}
	if _, err := domain.DecodeFindingAdjudication(tamperedBody); !errors.Is(err, domain.ErrFindingAdjudicationDigestMismatch) {
		t.Fatalf("tampered digest decode = %v, want ErrFindingAdjudicationDigestMismatch", err)
	}

	// Re-sign forged producer tuples so digest validation cannot mask the
	// returned-object trust-boundary checks.
	for _, tc := range []struct {
		name    string
		mutate  func(*domain.FindingAdjudicationEntry)
		wantErr error
	}{
		{
			name: "pure model mints allowed",
			mutate: func(entry *domain.FindingAdjudicationEntry) {
				entry.Producer = domain.AdjudicationProducerModel
				entry.Confidence = ptr(domain.ConfidenceHigh)
			},
			wantErr: domain.ErrModelEntryMintsAllowed,
		},
		{
			name: "engine-model carries model-only row",
			mutate: func(entry *domain.FindingAdjudicationEntry) {
				entry.Producer = domain.AdjudicationProducerEngineModel
				entry.Compatibility = ptr(domain.CompatibilityUnknown)
				entry.Route = domain.RouteParkUnknown
				entry.Confidence = ptr(domain.ConfidenceHigh)
			},
			wantErr: domain.ErrEngineModelEntryNonRemediationRow,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forged := artifact
			forged.Entries = slices.Clone(artifact.Entries)
			tc.mutate(&forged.Entries[0])
			forged.Digest, err = forged.ComputeDigest()
			if err != nil {
				t.Fatalf("compute forged digest: %v", err)
			}
			forgedBody, err := json.Marshal(forged)
			if err != nil {
				t.Fatalf("marshal forged artifact: %v", err)
			}
			if _, err := domain.DecodeFindingAdjudication(forgedBody); !errors.Is(err, tc.wantErr) {
				t.Fatalf("decode = %v, want %v", err, tc.wantErr)
			}
		})
	}

	// Trailing data is rejected.
	if _, err := domain.DecodeFindingAdjudication(append(body, '!')); err == nil {
		t.Fatalf("trailing data: want error, got nil")
	}

	// The compatibility decoder uses raw messages to distinguish absent
	// revision fields from explicit nulls. Nested feedback remains strict: an
	// unknown member must not disappear before digest recomputation.
	successorBody, err := successorAdjudicationFixture(t).Encode()
	if err != nil {
		t.Fatalf("encode successor: %v", err)
	}
	var successorFields map[string]json.RawMessage
	if err := json.Unmarshal(successorBody, &successorFields); err != nil {
		t.Fatalf("decode successor fields: %v", err)
	}
	var feedbackFields map[string]json.RawMessage
	if err := json.Unmarshal(successorFields["feedback"], &feedbackFields); err != nil {
		t.Fatalf("decode feedback fields: %v", err)
	}
	feedbackFields["unexpected"] = json.RawMessage(`true`)
	successorFields["feedback"], err = json.Marshal(feedbackFields)
	if err != nil {
		t.Fatalf("encode feedback fields: %v", err)
	}
	unknownFeedbackBody, err := json.Marshal(successorFields)
	if err != nil {
		t.Fatalf("encode successor with unknown feedback: %v", err)
	}
	if _, err := domain.DecodeFindingAdjudication(unknownFeedbackBody); err == nil {
		t.Fatal("unknown feedback field: want error, got nil")
	}
}
