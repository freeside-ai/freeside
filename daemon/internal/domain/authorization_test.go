package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func validAuthorizationInput() domain.CandidateAuthorizationInput {
	return domain.CandidateAuthorizationInput{
		Repo: "freeside-ai/demo", BaseSHA: "beefcafe", HeadSHA: "cafebabe",
		ImportResultDigest:       "sha256:import-result",
		VerificationRecipeDigest: "sha256:recipe-approved",
		VerificationOutcome:      domain.VerificationPassed,
		TrustProfileDigest:       "sha256:profile",
		InvocationID:             "inv-1",
		CreatedAt:                time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

func validWaiver() *domain.WaiverRecord {
	return &domain.WaiverRecord{
		DecisionID: "decision-1", DecidedBy: domain.AuthorUser,
		DecidedAt:      time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Justification:  "reviewed and accepted",
		DecisionDigest: "sha256:decision",
	}
}

func blockingFinding() domain.CandidateFinding {
	return domain.CandidateFinding{
		Class: domain.FindingClassRepoChangePolicy, Origin: domain.FindingOriginImport,
		Kind: "size_violation", Path: "assets/big.bin",
		Disposition: domain.DispositionBlocking,
	}
}

// TestAuthorizationComputedTrust: ID and AuthorizesPublication are policy
// computations over the bound facts, never caller labels (the #52 invariant
// applied to the authorization). A forged identity, a flipped trust bit, or
// content altered under a bound identity all fail closed on the literal and
// decode paths that bypass the constructor.
func TestAuthorizationComputedTrust(t *testing.T) {
	base, err := domain.NewCandidateAuthorization(validAuthorizationInput())
	if err != nil {
		t.Fatalf("NewCandidateAuthorization: %v", err)
	}
	if base.ID == "" {
		t.Fatal("constructor left an empty id")
	}
	if !base.AuthorizesPublication {
		t.Fatal("passed, findings-free authorization does not authorize publication")
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("constructed authorization rejected: %v", err)
	}

	forgedID := base
	forgedID.ID = "sha256:forged"
	if err := forgedID.Validate(); !errors.Is(err, domain.ErrAuthorizationInconsistent) {
		t.Fatalf("forged id error = %v, want ErrAuthorizationInconsistent", err)
	}

	// The forged-bit direction the publication gate depends on: a failed
	// verification whose record claims authorization.
	in := validAuthorizationInput()
	in.VerificationOutcome = domain.VerificationFailed
	failed, err := domain.NewCandidateAuthorization(in)
	if err != nil {
		t.Fatalf("NewCandidateAuthorization failed-outcome: %v", err)
	}
	if failed.AuthorizesPublication {
		t.Fatal("failed verification authorizes publication")
	}
	forgedBit := failed
	forgedBit.AuthorizesPublication = true
	if err := forgedBit.Validate(); !errors.Is(err, domain.ErrAuthorizationInconsistent) {
		t.Fatalf("forged bit error = %v, want ErrAuthorizationInconsistent", err)
	}

	// Content altered under the bound identity: a swapped head cannot reuse
	// the old authorization.
	swappedHead := base
	swappedHead.HeadSHA = "0ddba11"
	if err := swappedHead.Validate(); !errors.Is(err, domain.ErrAuthorizationInconsistent) {
		t.Fatalf("swapped head error = %v, want ErrAuthorizationInconsistent", err)
	}

	// A blocking finding withdraws authorization; the same finding waived
	// (waivable class, trusted waiver) restores it. Both bits are computed.
	in = validAuthorizationInput()
	in.Findings = []domain.CandidateFinding{blockingFinding()}
	blocked, err := domain.NewCandidateAuthorization(in)
	if err != nil {
		t.Fatalf("NewCandidateAuthorization blocked: %v", err)
	}
	if blocked.AuthorizesPublication {
		t.Fatal("blocking finding did not withdraw authorization")
	}
	waived := blockingFinding()
	waived.Disposition = domain.DispositionWaived
	waived.Waiver = validWaiver()
	in.Findings = []domain.CandidateFinding{waived}
	waivedAuth, err := domain.NewCandidateAuthorization(in)
	if err != nil {
		t.Fatalf("NewCandidateAuthorization waived: %v", err)
	}
	if !waivedAuth.AuthorizesPublication {
		t.Fatal("waived finding still withdraws authorization")
	}

	// Finding order does not change the identity: the constructor
	// canonicalizes, so one finding set has one record.
	second := blockingFinding()
	second.Kind = "allowlist_violation"
	second.Path = "vendor/dep.go"
	in.Findings = []domain.CandidateFinding{blockingFinding(), second}
	forward, err := domain.NewCandidateAuthorization(in)
	if err != nil {
		t.Fatalf("NewCandidateAuthorization forward: %v", err)
	}
	in.Findings = []domain.CandidateFinding{second, blockingFinding()}
	reversed, err := domain.NewCandidateAuthorization(in)
	if err != nil {
		t.Fatalf("NewCandidateAuthorization reversed: %v", err)
	}
	if forward.ID != reversed.ID {
		t.Fatalf("identity depends on finding order: %q vs %q", forward.ID, reversed.ID)
	}

	// A literal with out-of-order or duplicate findings is rejected:
	// canonical order is what makes the stored body carry exactly the
	// finding set the ID addresses.
	outOfOrder := forward
	outOfOrder.Findings = []domain.CandidateFinding{forward.Findings[1], forward.Findings[0]}
	if err := outOfOrder.Validate(); !errors.Is(err, domain.ErrFindingsNotCanonical) {
		t.Fatalf("out-of-order findings error = %v, want ErrFindingsNotCanonical", err)
	}
	in.Findings = []domain.CandidateFinding{blockingFinding(), blockingFinding()}
	if _, err := domain.NewCandidateAuthorization(in); !errors.Is(err, domain.ErrDuplicate) {
		t.Fatalf("duplicate findings error = %v, want ErrDuplicate", err)
	}
	// One representation per content on the literal path: a non-nil empty
	// finding list ("[]") is the nil content ("null") in different bytes,
	// and a non-UTC created_at is the same instant under a different
	// encoding, so each would give one bound-fact set two identities.
	emptyFindings := base
	emptyFindings.Findings = []domain.CandidateFinding{}
	if err := emptyFindings.Validate(); !errors.Is(err, domain.ErrFindingsNotCanonical) {
		t.Fatalf("empty findings error = %v, want ErrFindingsNotCanonical", err)
	}
	offset := base
	offset.CreatedAt = base.CreatedAt.In(time.FixedZone("CEST", 2*60*60))
	if err := offset.Validate(); !errors.Is(err, domain.ErrTimestampNotUTC) {
		t.Fatalf("non-UTC created_at error = %v, want ErrTimestampNotUTC", err)
	}
	// The constructor normalizes the same offset instant to UTC, so both
	// spellings converge on one identity.
	offsetIn := validAuthorizationInput()
	offsetIn.CreatedAt = offsetIn.CreatedAt.In(time.FixedZone("CEST", 2*60*60))
	normalized, err := domain.NewCandidateAuthorization(offsetIn)
	if err != nil {
		t.Fatalf("NewCandidateAuthorization offset instant: %v", err)
	}
	if normalized.ID != base.ID {
		t.Fatalf("identity depends on input time zone: %q vs %q", normalized.ID, base.ID)
	}
}

// TestAuthorizationValidation rejects each malformed bound fact with its
// sentinel: every field the publication gate trusts must be present.
func TestAuthorizationValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*domain.CandidateAuthorizationInput)
		want   error
	}{
		{"empty repo", func(in *domain.CandidateAuthorizationInput) { in.Repo = "" }, domain.ErrEmptyField},
		{"empty base sha", func(in *domain.CandidateAuthorizationInput) { in.BaseSHA = "" }, domain.ErrEmptyField},
		{"empty head sha", func(in *domain.CandidateAuthorizationInput) { in.HeadSHA = "" }, domain.ErrEmptyField},
		{"empty import result", func(in *domain.CandidateAuthorizationInput) { in.ImportResultDigest = "" }, domain.ErrEmptyField},
		{"empty recipe digest", func(in *domain.CandidateAuthorizationInput) { in.VerificationRecipeDigest = "" }, domain.ErrEmptyField},
		{"invalid outcome", func(in *domain.CandidateAuthorizationInput) { in.VerificationOutcome = "inconclusive" }, domain.ErrInvalidOutcome},
		{"empty outcome", func(in *domain.CandidateAuthorizationInput) { in.VerificationOutcome = "" }, domain.ErrInvalidOutcome},
		{"empty profile digest", func(in *domain.CandidateAuthorizationInput) { in.TrustProfileDigest = "" }, domain.ErrEmptyField},
		{"empty invocation", func(in *domain.CandidateAuthorizationInput) { in.InvocationID = "" }, domain.ErrEmptyID},
		{"zero created_at", func(in *domain.CandidateAuthorizationInput) { in.CreatedAt = time.Time{} }, domain.ErrMissingTimestamp},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := validAuthorizationInput()
			tt.mutate(&in)
			if _, err := domain.NewCandidateAuthorization(in); !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

// TestCandidateFindingValidation: the finding shape's paired invariants —
// category exactly for control-plane findings, waiver exactly for waived
// findings, and waivability bounded by the §3.1 non-waivable classes.
func TestCandidateFindingValidation(t *testing.T) {
	category := domain.ControlPlaneWorkflowConfiguration
	tests := []struct {
		name    string
		finding domain.CandidateFinding
		want    error
	}{
		{"invalid class", domain.CandidateFinding{
			Class: "advisory", Origin: domain.FindingOriginImport, Kind: "k",
			Disposition: domain.DispositionBlocking,
		}, domain.ErrInvalidFindingClass},
		{"control plane without category", domain.CandidateFinding{
			Class: domain.FindingClassControlPlane, Origin: domain.FindingOriginImport,
			Kind: "automation_control_path", Disposition: domain.DispositionBlocking,
		}, domain.ErrCategoryInconsistent},
		{"category on non-control-plane", domain.CandidateFinding{
			Class: domain.FindingClassSecret, Category: &category,
			Origin: domain.FindingOriginImport, Kind: "secret",
			Disposition: domain.DispositionBlocking,
		}, domain.ErrCategoryInconsistent},
		{"invalid category", func() domain.CandidateFinding {
			bad := domain.ControlPlaneCategory("plumbing")
			return domain.CandidateFinding{
				Class: domain.FindingClassControlPlane, Category: &bad,
				Origin: domain.FindingOriginImport, Kind: "automation_control_path",
				Disposition: domain.DispositionBlocking,
			}
		}(), domain.ErrInvalidFindingCategory},
		{"invalid origin", domain.CandidateFinding{
			Class: domain.FindingClassSecret, Origin: "scanner", Kind: "secret",
			Disposition: domain.DispositionBlocking,
		}, domain.ErrInvalidFindingOrigin},
		{"empty kind", domain.CandidateFinding{
			Class: domain.FindingClassSecret, Origin: domain.FindingOriginImport,
			Disposition: domain.DispositionBlocking,
		}, domain.ErrEmptyField},
		{"path and path_hex", domain.CandidateFinding{
			Class: domain.FindingClassSecret, Origin: domain.FindingOriginImport,
			Kind: "secret", Path: "a", PathHex: "61",
			Disposition: domain.DispositionBlocking,
		}, domain.ErrFindingPathConflict},
		{"invalid disposition", domain.CandidateFinding{
			Class: domain.FindingClassSecret, Origin: domain.FindingOriginImport,
			Kind: "secret", Disposition: "ignored",
		}, domain.ErrInvalidFindingDisposition},
		{"waived without waiver", func() domain.CandidateFinding {
			f := blockingFinding()
			f.Disposition = domain.DispositionWaived
			return f
		}(), domain.ErrWaiverInconsistent},
		{"blocking with waiver", func() domain.CandidateFinding {
			f := blockingFinding()
			f.Waiver = validWaiver()
			return f
		}(), domain.ErrWaiverInconsistent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.finding.Validate(); !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

// TestNonWaivableClasses pins each class's waivability to plan §3.1:
// control-plane modifications and secret detection never offer a bypass, so
// a waived finding of those classes is unrepresentable, whatever waiver
// record it carries.
func TestNonWaivableClasses(t *testing.T) {
	waivable := map[domain.CandidateFindingClass]bool{
		domain.FindingClassControlPlane:     false,
		domain.FindingClassImportIntegrity:  false,
		domain.FindingClassRepoChangePolicy: true,
		domain.FindingClassSecret:           false,
	}
	if len(waivable) != len(domain.AllCandidateFindingClasses) {
		t.Fatalf("waivability table covers %d classes, vocabulary has %d", len(waivable), len(domain.AllCandidateFindingClasses))
	}
	category := domain.ControlPlaneReviewerInstructions
	for _, class := range domain.AllCandidateFindingClasses {
		if got := class.Waivable(); got != waivable[class] {
			t.Errorf("%s.Waivable() = %v, want %v", class, got, waivable[class])
		}
		if waivable[class] {
			continue
		}
		f := domain.CandidateFinding{
			Class: class, Origin: domain.FindingOriginImport, Kind: "k",
			Disposition: domain.DispositionWaived, Waiver: validWaiver(),
		}
		if class == domain.FindingClassControlPlane {
			f.Category = &category
		}
		if err := f.Validate(); !errors.Is(err, domain.ErrNonWaivableFinding) {
			t.Errorf("waived %s finding error = %v, want ErrNonWaivableFinding", class, err)
		}
	}
	// The invalid zero value fails closed on both dispatches.
	if domain.CandidateFindingClass("").Waivable() {
		t.Error("zero-value class is waivable")
	}
}

// TestWaiverValidation: a waiver is a trusted decision record — identified,
// attributed to a non-agent author, timestamped, justified, and digest-bound.
func TestWaiverValidation(t *testing.T) {
	if err := validWaiver().Validate(); err != nil {
		t.Fatalf("valid waiver rejected: %v", err)
	}
	daemonWaiver := validWaiver()
	daemonWaiver.DecidedBy = domain.AuthorDaemon
	if err := daemonWaiver.Validate(); err != nil {
		t.Fatalf("daemon (trusted policy) waiver rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*domain.WaiverRecord)
		want   error
	}{
		{"empty decision id", func(w *domain.WaiverRecord) { w.DecisionID = "" }, domain.ErrEmptyID},
		{"invalid author", func(w *domain.WaiverRecord) { w.DecidedBy = "bot" }, domain.ErrInvalidAuthor},
		{"agent author", func(w *domain.WaiverRecord) { w.DecidedBy = domain.AuthorAgent }, domain.ErrAgentWaiver},
		{"zero decided_at", func(w *domain.WaiverRecord) { w.DecidedAt = time.Time{} }, domain.ErrMissingTimestamp},
		{"non-UTC decided_at", func(w *domain.WaiverRecord) {
			w.DecidedAt = w.DecidedAt.In(time.FixedZone("CEST", 2*60*60))
		}, domain.ErrTimestampNotUTC},
		{"empty justification", func(w *domain.WaiverRecord) { w.Justification = "" }, domain.ErrEmptyField},
		{"empty decision digest", func(w *domain.WaiverRecord) { w.DecisionDigest = "" }, domain.ErrEmptyField},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := validWaiver()
			tt.mutate(w)
			if err := w.Validate(); !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}
