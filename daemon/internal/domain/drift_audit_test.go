package domain_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

// TestDriftEnumRegistrationReferences pins both drift vocabularies'
// registration slices so the enum-registration ratchet sees a test reference.
func TestDriftEnumRegistrationReferences(t *testing.T) {
	t.Parallel()
	if !slices.Equal(domain.AllDriftVerdicts, []domain.DriftVerdict{"converged", "over_hardened", "stuck"}) {
		t.Fatalf("drift verdicts = %v, want the plan §7 set", domain.AllDriftVerdicts)
	}
	if !slices.Equal(domain.AllDriftAuditRoutes, []domain.DriftAuditRoute{"auto", "park"}) {
		t.Fatalf("drift audit routes = %v, want the plan §7 set", domain.AllDriftAuditRoutes)
	}
	if domain.DefaultDriftAuditRoute != domain.DriftAuditRouteAuto {
		t.Fatalf("default drift audit route = %q, want auto", domain.DefaultDriftAuditRoute)
	}
	for _, route := range domain.AllDriftAuditRoutes {
		if got, err := domain.ParseDriftAuditRoute(string(route)); err != nil || got != route {
			t.Fatalf("parse %q = %q, %v", route, got, err)
		}
	}
	for _, value := range []string{"", "Auto", "off"} {
		if _, err := domain.ParseDriftAuditRoute(value); !errors.Is(err, domain.ErrInvalidDriftAuditRoute) {
			t.Fatalf("parse %q = %v, want ErrInvalidDriftAuditRoute", value, err)
		}
	}
}

// TestReviewDiminishingCauseRegistration pins the stop-cause vocabulary: the
// four deterministic causes and the audit's, in registration order. Their
// values are stored in item reasons, so none may be renamed.
func TestReviewDiminishingCauseRegistration(t *testing.T) {
	t.Parallel()
	want := []domain.ReviewDiminishingCause{
		"low_value_streak", "fixed_recurrence", "final_review_findings",
		"growth_without_blockers", "drift_audit",
	}
	if !slices.Equal(domain.AllReviewDiminishingCauses, want) {
		t.Fatalf("causes = %v, want %v", domain.AllReviewDiminishingCauses, want)
	}
	for _, cause := range domain.AllReviewDiminishingCauses {
		if got, err := domain.ParseReviewDiminishingCause(string(cause)); err != nil || got != cause {
			t.Fatalf("parse %q = %q, %v", cause, got, err)
		}
	}
	for _, value := range []string{"", "Drift_Audit", "hard_round_limit"} {
		if _, err := domain.ParseReviewDiminishingCause(value); !errors.Is(err, domain.ErrInvalidReviewDiminishingCause) {
			t.Fatalf("parse %q = %v, want ErrInvalidReviewDiminishingCause", value, err)
		}
	}
}

func driftReversals() []domain.DriftReversal {
	return []domain.DriftReversal{
		{
			FindingID: "finding-0002",
			Undo:      "drop the retry wrapper around the config read",
			Rationale: "the specification reads the config once at start",
		},
		{
			FindingID: "finding-0001",
			Undo:      "remove the nil guard on the injected clock",
			Rationale: "the specification makes the clock a required argument",
		},
	}
}

// driftAuditInput is one valid input per verdict. The over_hardened reversals
// are deliberately unsorted so the fixture shows the constructor's ordering.
func driftAuditInput(verdict domain.DriftVerdict) domain.DriftAuditInput {
	input := domain.DriftAuditInput{
		RunID: "run-abc", Round: 6,
		BaseSHA: "deadbeef", HeadSHA: "0a1b2c3d",
		ApprovedSpecDigest:   adjDigest("approved-spec"),
		ResolvedPolicyDigest: adjDigest("resolved-policy"),
		Verdict:              verdict,
		CreatedAt:            time.Date(2026, 10, 1, 15, 0, 0, 0, time.UTC),
	}
	switch verdict {
	case domain.DriftVerdictConverged:
		input.Confidence = domain.ConfidenceHigh
		input.Explanation = "the change matches the specification and nothing needs undoing"
	case domain.DriftVerdictOverHardened:
		input.Confidence = domain.ConfidenceMedium
		input.Reversals = driftReversals()
		input.Explanation = "two fixes guard cases the specification rules out"
	case domain.DriftVerdictStuck:
		input.Confidence = domain.ConfidenceLow
		input.Explanation = "rounds keep producing findings and the change converges neither way"
	}
	return input
}

func driftAuditFixture(t *testing.T, verdict domain.DriftVerdict) domain.DriftAudit {
	t.Helper()
	audit, err := domain.NewDriftAudit(driftAuditInput(verdict))
	if err != nil {
		t.Fatalf("drift audit %q: %v", verdict, err)
	}
	return audit
}

// TestDriftAuditGolden pins the canonical encoding, one fixture per verdict.
// The converged and stuck goldens show the empty reversal list as [], never
// null. Each fixture is built by the validating constructor, so it doubles as
// a validation-positive case.
func TestDriftAuditGolden(t *testing.T) {
	t.Parallel()
	for _, verdict := range domain.AllDriftVerdicts {
		body, err := json.MarshalIndent(driftAuditFixture(t, verdict), "", "  ")
		if err != nil {
			t.Fatalf("marshal %q: %v", verdict, err)
		}
		golden.Assert(t, "drift_audit_"+string(verdict), append(body, '\n'))
	}
}

func TestDriftAuditRoundTripAndDigestStability(t *testing.T) {
	t.Parallel()
	for _, verdict := range domain.AllDriftVerdicts {
		audit := driftAuditFixture(t, verdict)
		body, err := audit.Encode()
		if err != nil {
			t.Fatalf("%q encode: %v", verdict, err)
		}
		decoded, err := domain.DecodeDriftAudit(body)
		if err != nil {
			t.Fatalf("%q decode: %v", verdict, err)
		}
		reencoded, err := decoded.Encode()
		if err != nil {
			t.Fatalf("%q re-encode: %v", verdict, err)
		}
		if string(body) != string(reencoded) {
			t.Fatalf("%q re-encode not deterministic:\n%s\n%s", verdict, body, reencoded)
		}
		if decoded.Digest != audit.Digest {
			t.Fatalf("%q digest changed across round-trip: %q vs %q", verdict, decoded.Digest, audit.Digest)
		}
	}
}

// TestNewDriftAuditCanonicalizesReversals proves the reversal list has one
// encoding: the constructor sorts by finding id, detaches the caller's slice,
// and turns a nil list into the empty one.
func TestNewDriftAuditCanonicalizesReversals(t *testing.T) {
	t.Parallel()
	input := driftAuditInput(domain.DriftVerdictOverHardened)
	audit, err := domain.NewDriftAudit(input)
	if err != nil {
		t.Fatal(err)
	}
	if audit.Reversals[0].FindingID != "finding-0001" || audit.Reversals[1].FindingID != "finding-0002" {
		t.Fatalf("reversals not sorted: %+v", audit.Reversals)
	}
	if input.Reversals[0].FindingID != "finding-0002" {
		t.Fatalf("constructor reordered the caller's slice: %+v", input.Reversals)
	}
	slices.Reverse(input.Reversals)
	reordered, err := domain.NewDriftAudit(input)
	if err != nil {
		t.Fatal(err)
	}
	if reordered.Digest != audit.Digest {
		t.Fatalf("input order changed the digest: %q vs %q", reordered.Digest, audit.Digest)
	}

	converged := driftAuditInput(domain.DriftVerdictConverged)
	fromNil, err := domain.NewDriftAudit(converged)
	if err != nil {
		t.Fatal(err)
	}
	converged.Reversals = []domain.DriftReversal{}
	fromEmpty, err := domain.NewDriftAudit(converged)
	if err != nil {
		t.Fatal(err)
	}
	if fromNil.Reversals == nil || fromNil.Digest != fromEmpty.Digest {
		t.Fatalf("nil and empty reversal lists differ: %+v vs %+v", fromNil, fromEmpty)
	}
}

// driftAuditRejections are the invalid shapes. Each mutates a valid input, so
// the same table drives the constructor and, re-signed, the decoder.
var driftAuditRejections = []struct {
	name    string
	verdict domain.DriftVerdict
	mutate  func(*domain.DriftAuditInput)
	wantErr error
}{
	{
		"zero verdict", domain.DriftVerdictConverged,
		func(in *domain.DriftAuditInput) { in.Verdict = "" }, domain.ErrInvalidDriftVerdict,
	},
	{
		"unknown verdict", domain.DriftVerdictConverged,
		func(in *domain.DriftAuditInput) { in.Verdict = "drifting" }, domain.ErrInvalidDriftVerdict,
	},
	{
		"missing confidence", domain.DriftVerdictConverged,
		func(in *domain.DriftAuditInput) { in.Confidence = "" }, domain.ErrInvalidAdjudicationConfidence,
	},
	{
		"confidence off the scale", domain.DriftVerdictConverged,
		func(in *domain.DriftAuditInput) { in.Confidence = "certain" }, domain.ErrInvalidAdjudicationConfidence,
	},
	{
		"over_hardened without reversals", domain.DriftVerdictOverHardened,
		func(in *domain.DriftAuditInput) { in.Reversals = nil }, domain.ErrDriftAuditInconsistent,
	},
	{
		"converged with a reversal", domain.DriftVerdictConverged,
		func(in *domain.DriftAuditInput) { in.Reversals = driftReversals()[:1] }, domain.ErrDriftAuditInconsistent,
	},
	{
		"stuck with a reversal", domain.DriftVerdictStuck,
		func(in *domain.DriftAuditInput) { in.Reversals = driftReversals()[:1] }, domain.ErrDriftAuditInconsistent,
	},
	{
		"repeated finding", domain.DriftVerdictOverHardened,
		func(in *domain.DriftAuditInput) { in.Reversals[1].FindingID = in.Reversals[0].FindingID },
		domain.ErrFindingsNotCanonical,
	},
	{
		"reversal without a finding", domain.DriftVerdictOverHardened,
		func(in *domain.DriftAuditInput) { in.Reversals[0].FindingID = "" }, domain.ErrEmptyID,
	},
	{
		"reversal without undo", domain.DriftVerdictOverHardened,
		func(in *domain.DriftAuditInput) { in.Reversals[0].Undo = " " }, domain.ErrEmptyField,
	},
	{
		"reversal without rationale", domain.DriftVerdictOverHardened,
		func(in *domain.DriftAuditInput) { in.Reversals[0].Rationale = "" }, domain.ErrEmptyField,
	},
	{
		"empty explanation", domain.DriftVerdictStuck,
		func(in *domain.DriftAuditInput) { in.Explanation = "\n" }, domain.ErrEmptyField,
	},
	{
		"missing run", domain.DriftVerdictConverged,
		func(in *domain.DriftAuditInput) { in.RunID = "" }, domain.ErrEmptyID,
	},
	{
		"round zero", domain.DriftVerdictConverged,
		func(in *domain.DriftAuditInput) { in.Round = 0 }, domain.ErrNonPositive,
	},
	{
		"missing base", domain.DriftVerdictConverged,
		func(in *domain.DriftAuditInput) { in.BaseSHA = "" }, domain.ErrEmptyField,
	},
	{
		"missing head", domain.DriftVerdictConverged,
		func(in *domain.DriftAuditInput) { in.HeadSHA = "" }, domain.ErrEmptyField,
	},
	{
		"malformed specification digest", domain.DriftVerdictConverged,
		func(in *domain.DriftAuditInput) { in.ApprovedSpecDigest = "spec" }, domain.ErrDriftAuditInconsistent,
	},
	{
		"malformed policy digest", domain.DriftVerdictConverged,
		func(in *domain.DriftAuditInput) { in.ResolvedPolicyDigest = "" }, domain.ErrDriftAuditInconsistent,
	},
	{
		"missing created_at", domain.DriftVerdictConverged,
		func(in *domain.DriftAuditInput) { in.CreatedAt = time.Time{} }, domain.ErrMissingTimestamp,
	},
	{
		"created_at not UTC", domain.DriftVerdictConverged,
		func(in *domain.DriftAuditInput) {
			in.CreatedAt = in.CreatedAt.In(time.FixedZone("offset", 3600))
		}, domain.ErrTimestampNotUTC,
	},
}

func TestNewDriftAuditRejects(t *testing.T) {
	t.Parallel()
	for _, tc := range driftAuditRejections {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			input := driftAuditInput(tc.verdict)
			tc.mutate(&input)
			if _, err := domain.NewDriftAudit(input); !errors.Is(err, tc.wantErr) {
				t.Fatalf("NewDriftAudit = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestNewDriftAuditRejectsInvalidUTF8 covers every string the digest is taken
// over. Marshaling rewrites an invalid byte to U+FFFD, so without the guard two
// audits differing only in such bytes would share a digest and the constructor
// would emit a body its own decoder reads back as different content.
func TestNewDriftAuditRejectsInvalidUTF8(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*domain.DriftAuditInput){
		"run_id":      func(in *domain.DriftAuditInput) { in.RunID = "run\xff" },
		"base_sha":    func(in *domain.DriftAuditInput) { in.BaseSHA = "base\xff" },
		"head_sha":    func(in *domain.DriftAuditInput) { in.HeadSHA = "head\xff" },
		"explanation": func(in *domain.DriftAuditInput) { in.Explanation = "why\xff" },
		"finding_id":  func(in *domain.DriftAuditInput) { in.Reversals[0].FindingID = "finding\xff" },
		"undo":        func(in *domain.DriftAuditInput) { in.Reversals[0].Undo = "undo\xff" },
		"rationale":   func(in *domain.DriftAuditInput) { in.Reversals[0].Rationale = "because\xff" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			input := driftAuditInput(domain.DriftVerdictOverHardened)
			mutate(&input)
			if _, err := domain.NewDriftAudit(input); !errors.Is(err, domain.ErrDriftAuditInconsistent) {
				t.Fatalf("NewDriftAudit = %v, want ErrDriftAuditInconsistent", err)
			}
		})
	}
}

// TestDecodeDriftAuditRejects re-signs each invalid shape, so the digest check
// can't mask the rule under test: a body whose digest is right for its content
// is still refused. The decoder sees what the constructor never emits, so it
// also covers an unsorted list, a null list, and a tampered digest.
func TestDecodeDriftAuditRejects(t *testing.T) {
	t.Parallel()
	resign := func(t *testing.T, audit domain.DriftAudit) []byte {
		t.Helper()
		digest, err := audit.ComputeDigest()
		if err != nil {
			t.Fatalf("compute forged digest: %v", err)
		}
		audit.Digest = digest
		body, err := json.Marshal(audit)
		if err != nil {
			t.Fatalf("marshal forged audit: %v", err)
		}
		return body
	}
	for _, tc := range driftAuditRejections {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			input := driftAuditInput(tc.verdict)
			audit := driftAuditFixture(t, tc.verdict)
			tc.mutate(&input)
			audit.RunID, audit.Round = input.RunID, input.Round
			audit.BaseSHA, audit.HeadSHA = input.BaseSHA, input.HeadSHA
			audit.ApprovedSpecDigest, audit.ResolvedPolicyDigest = input.ApprovedSpecDigest, input.ResolvedPolicyDigest
			audit.Verdict, audit.Confidence = input.Verdict, input.Confidence
			audit.Reversals = append([]domain.DriftReversal{}, input.Reversals...)
			audit.Explanation, audit.CreatedAt = input.Explanation, input.CreatedAt
			if _, err := domain.DecodeDriftAudit(resign(t, audit)); !errors.Is(err, tc.wantErr) {
				t.Fatalf("DecodeDriftAudit = %v, want %v", err, tc.wantErr)
			}
		})
	}

	overHardened := driftAuditFixture(t, domain.DriftVerdictOverHardened)
	unsorted := overHardened
	unsorted.Reversals = slices.Clone(overHardened.Reversals)
	slices.Reverse(unsorted.Reversals)
	if _, err := domain.DecodeDriftAudit(resign(t, unsorted)); !errors.Is(err, domain.ErrFindingsNotCanonical) {
		t.Fatalf("unsorted reversals decode = %v, want ErrFindingsNotCanonical", err)
	}

	converged := driftAuditFixture(t, domain.DriftVerdictConverged)
	nullList := converged
	nullList.Reversals = nil
	if _, err := domain.DecodeDriftAudit(resign(t, nullList)); !errors.Is(err, domain.ErrDriftAuditInconsistent) {
		t.Fatalf("null reversals decode = %v, want ErrDriftAuditInconsistent", err)
	}

	wrongVersion := converged
	wrongVersion.EncodingVersion = domain.DriftAuditEncodingVersion + 1
	if _, err := domain.DecodeDriftAudit(resign(t, wrongVersion)); !errors.Is(err, domain.ErrDriftAuditInconsistent) {
		t.Fatalf("encoding version decode = %v, want ErrDriftAuditInconsistent", err)
	}

	tampered := converged
	tampered.Digest = adjDigest("not-the-real-digest")
	tamperedBody, err := json.Marshal(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := domain.DecodeDriftAudit(tamperedBody); !errors.Is(err, domain.ErrDriftAuditDigestMismatch) {
		t.Fatalf("tampered digest decode = %v, want ErrDriftAuditDigestMismatch", err)
	}
	// Content changed under an unchanged digest is the same failure.
	edited := converged
	edited.Explanation = "a different explanation"
	editedBody, err := json.Marshal(edited)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := domain.DecodeDriftAudit(editedBody); !errors.Is(err, domain.ErrDriftAuditDigestMismatch) {
		t.Fatalf("edited content decode = %v, want ErrDriftAuditDigestMismatch", err)
	}

	body, err := converged.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := domain.DecodeDriftAudit(append(slices.Clone(body), '!')); err == nil {
		t.Fatal("trailing data: want error, got nil")
	}
	withUnknown := strings.Replace(string(body), `{"encoding_version"`, `{"unexpected":true,"encoding_version"`, 1)
	if _, err := domain.DecodeDriftAudit([]byte(withUnknown)); err == nil {
		t.Fatal("unknown field: want error, got nil")
	}
	withoutConfidence := strings.Replace(string(body), `"confidence":"high",`, "", 1)
	if withoutConfidence == string(body) {
		t.Fatal("fixture body has no confidence member to drop")
	}
	if _, err := domain.DecodeDriftAudit([]byte(withoutConfidence)); !errors.Is(err, domain.ErrInvalidAdjudicationConfidence) {
		t.Fatalf("absent confidence decode = %v, want ErrInvalidAdjudicationConfidence", err)
	}
	oversized := `{"explanation":"` + strings.Repeat("x", domain.MaxDriftAuditBytes) + `"}`
	if _, err := domain.DecodeDriftAudit([]byte(oversized)); err == nil {
		t.Fatal("oversized body: want error, got nil")
	}
}

// TestDriftAuditEncodeRefusesInvalid proves a hand-built value can't reach the
// persisted form without passing the content-addressed contract.
func TestDriftAuditEncodeRefusesInvalid(t *testing.T) {
	t.Parallel()
	audit := driftAuditFixture(t, domain.DriftVerdictStuck)
	audit.Verdict = domain.DriftVerdictConverged
	if _, err := audit.Encode(); !errors.Is(err, domain.ErrDriftAuditDigestMismatch) {
		t.Fatalf("encode = %v, want ErrDriftAuditDigestMismatch", err)
	}
}
