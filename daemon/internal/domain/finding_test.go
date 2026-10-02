package domain_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestClassificationHasNoFixedVerdict is acceptance criterion 7 (annotation
// half): the classification carries a version and has no field by which a
// classifier could declare a finding fixed. The classifier cannot mark a
// finding fixed (plan §5.12), so no such field may exist.
func TestClassificationHasNoFixedVerdict(t *testing.T) {
	rt := reflect.TypeOf(domain.Classification{})
	var hasVersion bool
	for i := range rt.NumField() {
		name := strings.ToLower(rt.Field(i).Name)
		if name == "version" {
			hasVersion = true
		}
		if strings.Contains(name, "fixed") || strings.Contains(name, "verdict") || name == "resolved" {
			t.Errorf("Classification exposes %q; the classifier can never declare a finding fixed", rt.Field(i).Name)
		}
	}
	if !hasVersion {
		t.Error("Classification has no Version field; it must be a versioned annotation")
	}
}

// TestClassificationValidate checks the finding join key and the positive
// version counter.
func TestClassificationValidate(t *testing.T) {
	if err := (domain.Classification{FindingID: "f1", Version: 1}).Validate(); err != nil {
		t.Fatalf("valid classification rejected: %v", err)
	}
	if err := (domain.Classification{Version: 1}).Validate(); !errors.Is(err, domain.ErrEmptyID) {
		t.Errorf("classification without finding_id accepted")
	}
	if err := (domain.Classification{FindingID: "f1", Version: 0}).Validate(); !errors.Is(err, domain.ErrNonPositive) {
		t.Errorf("classification with non-positive version accepted")
	}
}

// TestClassificationAnnotateIsNewVersion checks a correction is a new version,
// not an in-place edit: Annotate increments the version and returns a new value,
// leaving the receiver unchanged.
func TestClassificationAnnotateIsNewVersion(t *testing.T) {
	c := domain.Classification{FindingID: "f1", Version: 1, Materiality: "low", Note: "first pass"}
	next := c.Annotate("high", "confident", "on reflection, material")
	if next.Version != 2 {
		t.Errorf("annotated version = %d, want 2", next.Version)
	}
	if c.Version != 1 || c.Materiality != "low" {
		t.Error("Annotate mutated the receiver; corrections must be new versions")
	}
	if next.Materiality != "high" {
		t.Errorf("annotated materiality = %q, want high", next.Materiality)
	}
}

// TestFindingHasNoMutators is acceptance criterion 7 (immutability half): a raw
// Finding is immutable. It exposes no pointer-receiver methods (the only way a
// method could mutate a struct value) and no verdict field of its own.
func TestFindingHasNoMutators(t *testing.T) {
	pt := reflect.TypeOf(&domain.Finding{})
	for i := range pt.NumMethod() {
		// Validate is a value-receiver method and appears on the value type
		// too; a pointer-only method would be a mutator.
		m := pt.Method(i)
		if _, onValue := reflect.TypeOf(domain.Finding{}).MethodByName(m.Name); !onValue {
			t.Errorf("Finding has pointer-only method %q; a raw finding must be immutable", m.Name)
		}
	}
	rt := reflect.TypeOf(domain.Finding{})
	for i := range rt.NumField() {
		if name := strings.ToLower(rt.Field(i).Name); strings.Contains(name, "fixed") || strings.Contains(name, "verdict") {
			t.Errorf("Finding exposes %q; a raw finding carries no verdict", rt.Field(i).Name)
		}
	}
}

func TestFindingValidate(t *testing.T) {
	f := domain.Finding{ID: "f1", RunID: "run-1", Message: "x", CreatedAt: time.Now()}
	if err := f.Validate(); err != nil {
		t.Fatalf("valid finding rejected: %v", err)
	}
	if err := (domain.Finding{RunID: "run-1"}).Validate(); err == nil {
		t.Error("finding without id accepted")
	}
	if err := (domain.Finding{ID: "f1", RunID: "run-1"}).Validate(); err == nil {
		t.Error("finding without created_at accepted")
	}
	// Severity is optional but validated when present.
	valid := domain.Finding{ID: "f1", RunID: "run-1", Severity: "P0", Message: "x", CreatedAt: time.Now()}
	if err := valid.Validate(); err != nil {
		t.Errorf("finding with valid severity rejected: %v", err)
	}
	bad := domain.Finding{ID: "f1", RunID: "run-1", Severity: "high", Message: "x", CreatedAt: time.Now()}
	if err := bad.Validate(); !errors.Is(err, domain.ErrInvalidFindingSeverity) {
		t.Errorf("finding with invalid severity accepted: %v", err)
	}
	// A present location is validated; a nil location stays representable.
	located := domain.Finding{
		ID: "f1", RunID: "run-1", Message: "x", CreatedAt: time.Now(),
		Location: &domain.FindingLocation{Path: "a.go", StartLine: 3, EndLine: 5},
	}
	if err := located.Validate(); err != nil {
		t.Errorf("finding with valid location rejected: %v", err)
	}
	badLoc := domain.Finding{
		ID: "f1", RunID: "run-1", Message: "x", CreatedAt: time.Now(),
		Location: &domain.FindingLocation{Path: "", StartLine: 1, EndLine: 1},
	}
	if err := badLoc.Validate(); !errors.Is(err, domain.ErrEmptyField) {
		t.Errorf("finding with a pathless location accepted: %v", err)
	}
}

func TestFindingLocationValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		loc  domain.FindingLocation
		want error
	}{
		{"line range", domain.FindingLocation{Path: "a.go", StartLine: 3, EndLine: 7}, nil},
		{"single line", domain.FindingLocation{Path: "a.go", StartLine: 4, EndLine: 4}, nil},
		{"whole file", domain.FindingLocation{Path: "a.go"}, nil},
		{"empty path", domain.FindingLocation{StartLine: 1, EndLine: 1}, domain.ErrEmptyField},
		{"partial range", domain.FindingLocation{Path: "a.go", StartLine: 5, EndLine: 0}, domain.ErrNonPositive},
		{"negative endpoint", domain.FindingLocation{Path: "a.go", StartLine: -1, EndLine: 3}, domain.ErrNonPositive},
		{"inverted range", domain.FindingLocation{Path: "a.go", StartLine: 9, EndLine: 4}, domain.ErrInvertedRange},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.loc.Validate()
			if tc.want == nil {
				if err != nil {
					t.Errorf("Validate(%+v) = %v, want nil", tc.loc, err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("Validate(%+v) = %v, want %v", tc.loc, err, tc.want)
			}
		})
	}
}

func TestFindingLocationString(t *testing.T) {
	for _, tc := range []struct {
		loc  domain.FindingLocation
		want string
	}{
		{domain.FindingLocation{Path: "a.go", StartLine: 3, EndLine: 7}, "a.go:3-7"},
		{domain.FindingLocation{Path: "a.go", StartLine: 4, EndLine: 4}, "a.go:4"},
		{domain.FindingLocation{Path: "a.go"}, "a.go"},
	} {
		if got := tc.loc.String(); got != tc.want {
			t.Errorf("String(%+v) = %q, want %q", tc.loc, got, tc.want)
		}
	}
}

// codexFinding builds a codex_local-shaped raw finding for the fingerprint
// fixtures: the fields the source populates, with the per-round-varying inputs
// (id, run, severity, line range) as arguments so a "round 2" finding differs
// exactly where a real remediation review differs.
func codexFinding(id domain.FindingID, run domain.RunID, sev domain.FindingSeverity, path, msg string, start, end int) domain.Finding {
	return domain.Finding{
		ID: id, RunID: run, Source: "codex_local", Severity: sev,
		Location:  &domain.FindingLocation{Path: path, StartLine: start, EndLine: end},
		Message:   msg,
		RawText:   msg,
		CreatedAt: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC),
	}
}

// TestFindingFingerprintStableAcrossRounds is the core acceptance case: the
// same logical finding, observed in two remediation rounds that differ in
// invocation-scoped ID, run, severity re-tag, and shifted line range, derives
// one identity. The exact literal is pinned so any change to the derivation
// (inputs, order, version, truncation) is visible in the diff.
func TestFindingFingerprintStableAcrossRounds(t *testing.T) {
	const path, msg = "daemon/main.go", "unchecked error from Close"
	round1 := codexFinding("review-1111111111111111", "run-1", "P1", path, msg, 10, 12)
	round2 := codexFinding("review-2222222222222222", "run-2", "P2", path, msg, 14, 16)

	fp1, err := round1.Fingerprint()
	if err != nil {
		t.Fatalf("round1 fingerprint: %v", err)
	}
	fp2, err := round2.Fingerprint()
	if err != nil {
		t.Fatalf("round2 fingerprint: %v", err)
	}
	if fp1 != fp2 {
		t.Errorf("fingerprint changed across rounds: %q != %q", fp1, fp2)
	}
	if round1.ID == round2.ID {
		t.Fatal("fixture rounds must carry distinct FindingIDs")
	}
	const want = domain.FindingFingerprint("fpv1-a256def93b535bec95265451")
	if fp1 != want {
		t.Errorf("fingerprint = %q, want pinned %q", fp1, want)
	}
}

// TestFindingFingerprintNormalization pins the message normalization: internal
// whitespace runs collapse and leading/trailing whitespace is trimmed (equal
// identity), while case differences are deliberately preserved (distinct
// identity), and the location line range never enters the identity.
func TestFindingFingerprintNormalization(t *testing.T) {
	base := codexFinding("f-a", "run-1", "P1", "a.go", "unchecked error", 3, 5)
	baseFP, err := base.Fingerprint()
	if err != nil {
		t.Fatalf("base fingerprint: %v", err)
	}

	for _, msg := range []string{
		"  unchecked error  ",  // trimmed
		"unchecked\terror",     // tab collapses
		"unchecked   error",    // run collapses
		"unchecked\n  error\n", // newline + run collapse and trim
	} {
		f := codexFinding("f-b", "run-9", "P3", "a.go", msg, 40, 41)
		got, err := f.Fingerprint()
		if err != nil {
			t.Fatalf("fingerprint(%q): %v", msg, err)
		}
		if got != baseFP {
			t.Errorf("fingerprint(%q) = %q, want equal to %q", msg, got, baseFP)
		}
	}

	cased := codexFinding("f-c", "run-1", "P1", "a.go", "Unchecked Error", 3, 5)
	casedFP, err := cased.Fingerprint()
	if err != nil {
		t.Fatalf("cased fingerprint: %v", err)
	}
	if casedFP == baseFP {
		t.Error("case-differing messages must not share an identity")
	}
}

// TestFindingFingerprintFailsClosed covers the fail-closed set: a finding with
// no computable identity yields ErrUnfingerprintableFinding rather than an
// invented one, so it can never satisfy the §7 absence proof.
func TestFindingFingerprintFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    domain.Finding
	}{
		{"nil location", domain.Finding{Source: "codex_local", Message: "x"}},
		{"empty path", domain.Finding{Source: "codex_local", Location: &domain.FindingLocation{Path: ""}, Message: "x"}},
		{"empty message", codexFinding("f", "run-1", "P1", "a.go", "", 1, 1)},
		{"whitespace-only message", codexFinding("f", "run-1", "P1", "a.go", "  \t\n ", 1, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.f.Fingerprint(); !errors.Is(err, domain.ErrUnfingerprintableFinding) {
				t.Errorf("Fingerprint() err = %v, want ErrUnfingerprintableFinding", err)
			}
		})
	}
}

// TestFindingIdentityAbsent is the fixed-disposition primitive: a fixed finding
// is absent from the remediation round; a re-emitted finding is present (its
// route back into adjudication as a failed fix is the consumers' behavior);
// and an unfingerprintable prior or current finding fails the comparison closed.
func TestFindingIdentityAbsent(t *testing.T) {
	const path, msg = "daemon/main.go", "unchecked error from Close"
	prior := codexFinding("review-1111111111111111", "run-1", "P1", path, msg, 10, 12)
	reemitted := codexFinding("review-2222222222222222", "run-2", "P2", path, msg, 14, 16)
	other := codexFinding("review-3333333333333333", "run-2", "P1", "daemon/other.go", "nil deref", 1, 1)

	// Fixed: the prior finding's identity is absent from the remediation round.
	absent, err := domain.FindingIdentityAbsent(prior, []domain.Finding{other})
	if err != nil {
		t.Fatalf("absent (fixed) err: %v", err)
	}
	if !absent {
		t.Error("a fixed finding must be absent from the remediation round")
	}

	// Re-emitted: identity present, so not absent — it re-enters adjudication as
	// a failed fix rather than counting as fixed.
	absent, err = domain.FindingIdentityAbsent(prior, []domain.Finding{other, reemitted})
	if err != nil {
		t.Fatalf("absent (re-emitted) err: %v", err)
	}
	if absent {
		t.Error("a re-emitted finding must not be reported absent")
	}

	// Empty remediation round: trivially absent.
	absent, err = domain.FindingIdentityAbsent(prior, nil)
	if err != nil || !absent {
		t.Errorf("empty round: absent=%v err=%v, want true/nil", absent, err)
	}

	bad := domain.Finding{Source: "codex_local", Message: "x"} // nil location
	if _, err := domain.FindingIdentityAbsent(bad, []domain.Finding{other}); !errors.Is(err, domain.ErrUnfingerprintableFinding) {
		t.Errorf("unfingerprintable prior err = %v, want ErrUnfingerprintableFinding", err)
	}
	// An unfingerprintable current fails closed even when a matching finding
	// precedes it in the batch.
	if _, err := domain.FindingIdentityAbsent(prior, []domain.Finding{reemitted, bad}); !errors.Is(err, domain.ErrUnfingerprintableFinding) {
		t.Errorf("unfingerprintable current err = %v, want ErrUnfingerprintableFinding", err)
	}
}

func externalFindingInput() domain.ExternalFindingInput {
	return domain.ExternalFindingInput{
		RunID: "run-1", Forge: domain.ExternalReviewForgeGitHub,
		ReviewerAccountID: 41, ReviewerLogin: "codex[bot]",
		ThreadID: "PRRT_kwDOexample", HeadSHA: "cafebabe",
		Severity:  domain.FindingSeverityP1,
		Location:  &domain.FindingLocation{Path: "daemon/main.go", StartLine: 42, EndLine: 42},
		Message:   "unchecked error",
		RawText:   "P1: the error return is dropped",
		CreatedAt: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC),
	}
}

// TestNewExternalFinding: the constructor derives every field a caller must
// not choose (plan §5.19), and the ID keys on where the finding was left and
// what it says, so a replayed ingest converges and an edit is a new finding.
func TestNewExternalFinding(t *testing.T) {
	in := externalFindingInput()
	f, err := domain.NewExternalFinding(in)
	if err != nil {
		t.Fatalf("NewExternalFinding: %v", err)
	}
	if f.External == nil || f.External.Class != domain.FindingProvenanceExternalUntrusted {
		t.Fatalf("external provenance = %+v, want external_untrusted", f.External)
	}
	if f.Source != "external_github" || f.Source != domain.ExternalFindingSource(domain.ExternalReviewForgeGitHub) {
		t.Fatalf("source = %q, want external_github", f.Source)
	}
	sum := sha256.Sum256([]byte(in.RawText))
	if want := domain.Digest("sha256:" + hex.EncodeToString(sum[:])); f.External.RawSourceDigest != want {
		t.Fatalf("raw source digest = %q, want %q", f.External.RawSourceDigest, want)
	}
	// The constructor detaches the location from the caller's value.
	in.Location.Path = "elsewhere.go"
	if f.Location.Path != "daemon/main.go" {
		t.Fatal("finding location aliases the caller's value")
	}

	in = externalFindingInput()
	replayed, err := domain.NewExternalFinding(in)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ID != f.ID {
		t.Fatalf("replayed ID %q, want %q", replayed.ID, f.ID)
	}
	for _, tt := range []struct {
		name   string
		mutate func(*domain.ExternalFindingInput)
	}{
		{"edited text", func(in *domain.ExternalFindingInput) { in.RawText += " (edited)" }},
		{"another thread", func(in *domain.ExternalFindingInput) { in.ThreadID = "PRRT_other" }},
		{"another head", func(in *domain.ExternalFindingInput) { in.HeadSHA = "deadbeef" }},
		{"another run", func(in *domain.ExternalFindingInput) { in.RunID = "run-2" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			in := externalFindingInput()
			tt.mutate(&in)
			other, err := domain.NewExternalFinding(in)
			if err != nil {
				t.Fatal(err)
			}
			if other.ID == f.ID {
				t.Fatalf("%s kept finding ID %q", tt.name, f.ID)
			}
		})
	}
}

// TestExternalFindingCarriesNoAdmission pins the shape: nothing stored on a
// finding can say its reviewer is admitted or what they may do. Admission is
// the trust profile's answer at read time (plan §5.19), so a field here
// would be a trust bit a decoded row could forge.
func TestExternalFindingCarriesNoAdmission(t *testing.T) {
	rt := reflect.TypeOf(domain.ExternalFindingProvenance{})
	want := []string{"Class", "Forge", "ReviewerAccountID", "ReviewerLogin", "ThreadID", "RawSourceDigest", "HeadSHA"}
	var got []string
	for i := range rt.NumField() {
		got = append(got, rt.Field(i).Name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("external provenance fields = %v, want exactly %v", got, want)
	}
}

// TestExternalFindingValidate is the literal path a decoded row takes: each
// derived field is recomputed and each third-party string is bounded.
func TestExternalFindingValidate(t *testing.T) {
	tooLarge := strings.Repeat("x", domain.MaxNativeReviewTextBytes+1)
	tests := []struct {
		name   string
		mutate func(*domain.Finding)
		want   error
	}{
		{"raw text edited under its digest", func(f *domain.Finding) { f.RawText = "LGTM" }, domain.ErrExternalFindingInconsistent},
		{"forged raw source digest", func(f *domain.Finding) { f.External.RawSourceDigest = "sha256:forged" }, domain.ErrExternalFindingInconsistent},
		{"a Freeside review source's label", func(f *domain.Finding) { f.Source = "codex_github" }, domain.ErrExternalFindingInconsistent},
		{"caller-chosen ID", func(f *domain.Finding) { f.ID = "find-1" }, domain.ErrExternalFindingInconsistent},
		{"another run under the same ID", func(f *domain.Finding) { f.RunID = "run-2" }, domain.ErrExternalFindingInconsistent},
		{"another head under the same ID", func(f *domain.Finding) { f.External.HeadSHA = "deadbeef" }, domain.ErrExternalFindingInconsistent},
		{"another thread under the same ID", func(f *domain.Finding) { f.External.ThreadID = "PRRT_other" }, domain.ErrExternalFindingInconsistent},
		{"another account under the same ID", func(f *domain.Finding) { f.External.ReviewerAccountID++ }, domain.ErrExternalFindingInconsistent},
		{"unknown class", func(f *domain.Finding) { f.External.Class = "external_trusted" }, domain.ErrInvalidFindingProvenanceClass},
		{"empty class", func(f *domain.Finding) { f.External.Class = "" }, domain.ErrInvalidFindingProvenanceClass},
		{"unknown forge", func(f *domain.Finding) { f.External.Forge = "gitlab" }, domain.ErrInvalidExternalReviewForge},
		{"zero reviewer account", func(f *domain.Finding) { f.External.ReviewerAccountID = 0 }, domain.ErrNonPositive},
		{"empty reviewer login", func(f *domain.Finding) { f.External.ReviewerLogin = "" }, domain.ErrEmptyField},
		{"reviewer login with whitespace", func(f *domain.Finding) { f.External.ReviewerLogin = "codex bot" }, domain.ErrExternalReviewerLoginInvalid},
		{"empty thread", func(f *domain.Finding) { f.External.ThreadID = "" }, domain.ErrEmptyField},
		{"empty head", func(f *domain.Finding) { f.External.HeadSHA = "" }, domain.ErrEmptyField},
		{"message not UTF-8", func(f *domain.Finding) { f.Message = "bad\xff" }, domain.ErrNativeReviewTextNotUTF8},
		{"message too large", func(f *domain.Finding) { f.Message = tooLarge }, domain.ErrNativeReviewTextTooLarge},
		{"location path not UTF-8", func(f *domain.Finding) { f.Location.Path = "bad\xff.go" }, domain.ErrNativeReviewTextNotUTF8},
		{"thread too large", func(f *domain.Finding) { f.External.ThreadID = tooLarge }, domain.ErrNativeReviewTextTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := domain.NewExternalFinding(externalFindingInput())
			if err != nil {
				t.Fatal(err)
			}
			tt.mutate(&f)
			if err := f.Validate(); !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}

	// Raw text is bounded before it is digested, through the constructor too.
	for _, tt := range []struct {
		name string
		raw  string
		want error
	}{
		{"empty raw text", "", domain.ErrEmptyField},
		{"raw text not UTF-8", "bad\xff", domain.ErrNativeReviewTextNotUTF8},
		{"raw text too large", tooLarge, domain.ErrNativeReviewTextTooLarge},
	} {
		t.Run(tt.name, func(t *testing.T) {
			in := externalFindingInput()
			in.RawText = tt.raw
			if _, err := domain.NewExternalFinding(in); !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}

	// The label is reserved in the other direction too: a finding without
	// external provenance cannot wear it, or its fingerprint would be an
	// external finding's.
	native := codexFinding("find-1", "run-1", domain.FindingSeverityP2, "daemon/main.go", "unchecked error", 42, 42)
	if err := native.Validate(); err != nil {
		t.Fatalf("native finding rejected: %v", err)
	}
	native.Source = domain.ExternalFindingSource(domain.ExternalReviewForgeGitHub)
	if err := native.Validate(); !errors.Is(err, domain.ErrExternalFindingInconsistent) {
		t.Fatalf("native finding with the external label error = %v, want ErrExternalFindingInconsistent", err)
	}
}

// TestNativeReviewObservationRefusesExternalFinding is one third of the
// §5.19 quarantine (the store pins the review-record and shadow-record
// thirds): an external finding never rides inside a native observation, and
// re-deriving an identical external finding is not a material change.
func TestNativeReviewObservationRefusesExternalFinding(t *testing.T) {
	external, err := domain.NewExternalFinding(externalFindingInput())
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 2, 13, 0, 0, 0, time.UTC)
	observation := domain.NativeReviewObservation{
		Repo: "owner/repo", RepositoryID: 84958515, PRNumber: 450,
		Provider: domain.NativeReviewCodexGitHub, Kind: domain.NativeReviewFindings,
		NativeID: 900100, AuthorLogin: "chatgpt-codex-connector",
		ReviewCommitSHA: "cafebabe", ReviewState: "COMMENTED", BindingHeadSHA: "cafebabe",
		SubmittedAt: at, ObservedAt: at,
		Findings: []domain.Finding{external},
	}
	if err := observation.Validate(); !errors.Is(err, domain.ErrExternalFindingQuarantined) {
		t.Fatalf("observation with an external finding error = %v, want ErrExternalFindingQuarantined", err)
	}

	rederived, err := domain.NewExternalFinding(externalFindingInput())
	if err != nil {
		t.Fatal(err)
	}
	if rederived.External == external.External {
		t.Fatal("fixture shares one provenance pointer; the comparison below would prove nothing")
	}
	previous := observation
	previous.Findings = []domain.Finding{rederived}
	if observation.MaterialChangeFrom(previous) {
		t.Fatal("identical external provenance behind a different pointer read as a material change")
	}
	rederived.External.ReviewerLogin = "someone-else"
	if !observation.MaterialChangeFrom(previous) {
		t.Fatal("a changed external provenance did not read as a material change")
	}
}
