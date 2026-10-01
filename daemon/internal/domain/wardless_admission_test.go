package domain_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

const (
	wardlessPromptDigest   = domain.Digest("sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	wardlessLineupRevision = domain.Digest("sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
	auditedHarnessBuild    = "claude-code 2.1.267"
)

func claudeCallAdapter(t *testing.T, mutate func(*domain.AdapterFragment)) domain.AdapterFragment {
	t.Helper()
	f := domain.AdapterFragment{
		EncodingVersion: domain.AgentFragmentEncodingVersion,
		AdapterBuild:    "claude_call_v1@build-3", HarnessBuild: auditedHarnessBuild,
		ClientKind: domain.HarnessClientClaudeCode, Vendor: domain.AgentVendorClaude,
		LaunchCapabilities: domain.NewLaunchCapabilitySet(domain.LaunchCapStructuredOutput),
		SendableEfforts:    []domain.EffortLevel{domain.EffortHigh},
	}
	if mutate != nil {
		mutate(&f)
	}
	digest, err := f.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	f.Digest = digest
	return f
}

func wardlessOffer(t *testing.T, lineage string) domain.OfferFragment {
	t.Helper()
	f := offerFragment(t)
	f.RouteModelID, f.LineageGroup = "claude-judge", lineage
	digest, err := f.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	f.Digest = digest
	return f
}

// wardlessInput is a valid admission of the task namer under the audited
// Claude call adapter. The enrollment authenticates with a setup token, so
// its generation records no expiry.
func wardlessInput(t *testing.T, adapter domain.AdapterFragment, offer domain.OfferFragment) domain.WardlessAdmissionInput {
	t.Helper()
	return wardlessInputAtEffort(t, adapter, offer, domain.EffortHigh)
}

func wardlessInputAtEffort(
	t *testing.T, adapter domain.AdapterFragment, offer domain.OfferFragment, effort domain.EffortLevel,
) domain.WardlessAdmissionInput {
	t.Helper()
	e := enrollment()
	e.HarnessClient, e.Route, e.AuthMethod = adapter.ClientKind, "anthropic_subscription", domain.AuthMethodSetupToken
	g := generation()
	g.TokenExpiry = nil
	route := routeFragment(t)
	agent := domain.AgentDefinition{
		Name: "haiku-judge", EncodingVersion: domain.AgentEncodingVersion,
		EnrollmentID: e.ID, RouteDigest: route.Digest,
		AdapterDigest: adapter.Digest, OfferDigest: offer.Digest, Effort: effort,
	}
	digest, err := agent.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	agent.Digest = digest
	return domain.WardlessAdmissionInput{
		Role: domain.RoleTaskNamer,
		Line: domain.LineupSelection{
			AgentName: agent.Name, AgentDigest: agent.Digest,
			PromptName: "task-namer", PromptDigest: wardlessPromptDigest,
		},
		LineupRevision: wardlessLineupRevision,
		Agent:          agent, Route: route, Adapter: adapter, Offer: offer,
		PromptDigest: wardlessPromptDigest,
		Enrollment:   e, Generation: g,
		Deadline:     time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
		ExpiryMargin: time.Minute,
		LaunchProof: &domain.InterimCallLaunchAudit{
			AdapterDigest: adapter.Digest, HarnessBuild: adapter.HarnessBuild, AuditedOn: "2026-09-09",
		},
	}
}

func validWardlessInput(t *testing.T) domain.WardlessAdmissionInput {
	t.Helper()
	return wardlessInput(t, claudeCallAdapter(t, nil), wardlessOffer(t, "anthropic"))
}

func TestAdmitWardlessRole(t *testing.T) {
	in := validWardlessInput(t)
	admission, err := domain.AdmitWardlessRole(in)
	if err != nil {
		t.Fatalf("AdmitWardlessRole = %v", err)
	}
	want := domain.WardlessAdmission{
		Role: domain.RoleTaskNamer, AgentDigest: in.Agent.Digest, PromptDigest: wardlessPromptDigest,
		LineupRevision: wardlessLineupRevision, EnrollmentID: "enroll-1", EnrollmentGeneration: 1,
		InterimLaunchAudit: *in.LaunchProof,
	}
	if admission != want {
		t.Fatalf("admission = %+v, want %+v", admission, want)
	}
	if err := admission.Validate(); err != nil {
		t.Fatalf("admission.Validate = %v", err)
	}
	for _, role := range domain.AllRoleNames {
		in := validWardlessInput(t)
		in.Role = role
		_, err := domain.AdmitWardlessRole(in)
		if role.LaunchShape() == domain.LaunchShapeWardless {
			if err != nil {
				t.Fatalf("wardless role %s = %v", role, err)
			}
			continue
		}
		if !errors.Is(err, domain.ErrRoleNotWardless) {
			t.Fatalf("ward role %s = %v, want %v", role, err, domain.ErrRoleNotWardless)
		}
	}
}

func TestAdmitWardlessRoleFailsClosed(t *testing.T) {
	otherDigest := domain.Digest("sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd")
	cases := []struct {
		name    string
		build   func(t *testing.T) domain.WardlessAdmissionInput
		wantErr error
	}{
		{"unknown role", func(t *testing.T) domain.WardlessAdmissionInput {
			in := validWardlessInput(t)
			in.Role = "researcher"
			return in
		}, domain.ErrInvalidRoleName},
		{"missing launch proof", func(t *testing.T) domain.WardlessAdmissionInput {
			in := validWardlessInput(t)
			in.LaunchProof = nil
			return in
		}, domain.ErrCallLaunchUnproved},
		{"audit of another adapter build", func(t *testing.T) domain.WardlessAdmissionInput {
			in := validWardlessInput(t)
			in.LaunchProof.AdapterDigest = otherDigest
			return in
		}, domain.ErrCallLaunchUnproved},
		{"audit of another harness build", func(t *testing.T) domain.WardlessAdmissionInput {
			in := validWardlessInput(t)
			in.LaunchProof.HarnessBuild = "claude-code 2.1.286"
			return in
		}, domain.ErrCallLaunchUnproved},
		{"adapter moved to another harness build", func(t *testing.T) domain.WardlessAdmissionInput {
			audited := claudeCallAdapter(t, nil)
			moved := claudeCallAdapter(t, func(f *domain.AdapterFragment) { f.HarnessBuild = "claude-code 2.1.286" })
			in := wardlessInput(t, moved, wardlessOffer(t, "anthropic"))
			in.LaunchProof = &domain.InterimCallLaunchAudit{
				AdapterDigest: audited.Digest, HarnessBuild: audited.HarnessBuild, AuditedOn: "2026-09-09",
			}
			return in
		}, domain.ErrCallLaunchUnproved},
		// No second call driver joins on a hand audit, even one whose audit
		// record names its own adapter and harness build.
		{"another client kind", func(t *testing.T) domain.WardlessAdmissionInput {
			codex := claudeCallAdapter(t, func(f *domain.AdapterFragment) {
				f.ClientKind, f.Vendor = domain.HarnessClientCodexCLI, domain.AgentVendorCodex
			})
			return wardlessInput(t, codex, wardlessOffer(t, "openai"))
		}, domain.ErrCallLaunchUnproved},
		{"malformed audit date", func(t *testing.T) domain.WardlessAdmissionInput {
			in := validWardlessInput(t)
			in.LaunchProof.AuditedOn = "last month"
			return in
		}, domain.ErrMissingTimestamp},
		{"line names another agent", func(t *testing.T) domain.WardlessAdmissionInput {
			in := validWardlessInput(t)
			in.Line.AgentDigest = otherDigest
			return in
		}, domain.ErrLineupSelectionMismatch},
		{"line names another prompt", func(t *testing.T) domain.WardlessAdmissionInput {
			in := validWardlessInput(t)
			in.Line.PromptDigest = otherDigest
			return in
		}, domain.ErrLineupSelectionMismatch},
		{"prompt unresolved", func(t *testing.T) domain.WardlessAdmissionInput {
			in := validWardlessInput(t)
			in.PromptDigest = ""
			return in
		}, domain.ErrInvalidDigest},
		{"lineup revision missing", func(t *testing.T) domain.WardlessAdmissionInput {
			in := validWardlessInput(t)
			in.LineupRevision = ""
			return in
		}, domain.ErrInvalidDigest},
		{"agent digest forged", func(t *testing.T) domain.WardlessAdmissionInput {
			in := validWardlessInput(t)
			in.Agent.Effort = domain.EffortMax
			return in
		}, domain.ErrAgentDigestMismatch},
		{"route is not the agent's", func(t *testing.T) domain.WardlessAdmissionInput {
			in := validWardlessInput(t)
			in.Route.TermsBasisDate = "2026-09-01"
			digest, err := in.Route.ComputeDigest()
			if err != nil {
				t.Fatal(err)
			}
			in.Route.Digest = digest
			return in
		}, domain.ErrAdmissionDerivationMismatch},
		{"effort the offer does not allow", func(t *testing.T) domain.WardlessAdmissionInput {
			adapter := claudeCallAdapter(t, func(f *domain.AdapterFragment) {
				f.SendableEfforts = []domain.EffortLevel{domain.EffortLow, domain.EffortHigh}
			})
			return wardlessInputAtEffort(t, adapter, wardlessOffer(t, "anthropic"), domain.EffortLow)
		}, domain.ErrAgentJoinInvalid},
		{"effort the adapter cannot send", func(t *testing.T) domain.WardlessAdmissionInput {
			return wardlessInputAtEffort(t, claudeCallAdapter(t, nil), wardlessOffer(t, "anthropic"), domain.EffortMax)
		}, domain.ErrAgentJoinInvalid},
		{"adapter is not the agent's", func(t *testing.T) domain.WardlessAdmissionInput {
			in := validWardlessInput(t)
			in.Adapter = claudeCallAdapter(t, func(f *domain.AdapterFragment) { f.AdapterBuild = "claude_call_v1@build-4" })
			return in
		}, domain.ErrAdmissionDerivationMismatch},
		{"offer is not the agent's", func(t *testing.T) domain.WardlessAdmissionInput {
			in := validWardlessInput(t)
			in.Offer = wardlessOffer(t, "other")
			return in
		}, domain.ErrAdmissionDerivationMismatch},
		{"expired offer", func(t *testing.T) domain.WardlessAdmissionInput {
			in := validWardlessInput(t)
			in.Deadline = in.Offer.NotAfter.Add(time.Hour)
			return in
		}, domain.ErrOfferExpired},
		{"enrollment is not the agent's", func(t *testing.T) domain.WardlessAdmissionInput {
			in := validWardlessInput(t)
			in.Enrollment.ID, in.Generation.EnrollmentID = "enroll-2", "enroll-2"
			return in
		}, domain.ErrAdmissionDerivationMismatch},
		{"enrollment binds another client", func(t *testing.T) domain.WardlessAdmissionInput {
			in := validWardlessInput(t)
			in.Enrollment.HarnessClient = domain.HarnessClientCodexCLI
			return in
		}, domain.ErrAdmissionDerivationMismatch},
		{"generation of another enrollment", func(t *testing.T) domain.WardlessAdmissionInput {
			in := validWardlessInput(t)
			in.Generation.EnrollmentID = "enroll-2"
			return in
		}, domain.ErrParentKeyMismatch},
		{"generation on another account", func(t *testing.T) domain.WardlessAdmissionInput {
			in := validWardlessInput(t)
			in.Generation.AccountBinding = "acct-2"
			return in
		}, domain.ErrAccountBindingMismatch},
		{"generation not persisted", func(t *testing.T) domain.WardlessAdmissionInput {
			in := validWardlessInput(t)
			in.Generation.Ordinal = 0
			return in
		}, domain.ErrNonPositive},
		{"expiry the method cannot observe", func(t *testing.T) domain.WardlessAdmissionInput {
			in := validWardlessInput(t)
			expiry := in.Deadline.Add(time.Hour)
			in.Generation.TokenExpiry = &expiry
			return in
		}, domain.ErrGenerationExpiryInconsistent},
		{"credential expires inside the margin", func(t *testing.T) domain.WardlessAdmissionInput {
			in := validWardlessInput(t)
			in.Enrollment.AuthMethod = domain.AuthMethodOAuth
			expiry := in.Deadline.Add(30 * time.Second)
			in.Generation.TokenExpiry = &expiry
			return in
		}, domain.ErrGenerationExpiryInsufficient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			admission, err := domain.AdmitWardlessRole(tc.build(t))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("AdmitWardlessRole = %v, want %v", err, tc.wantErr)
			}
			if admission != (domain.WardlessAdmission{}) {
				t.Fatalf("refused admission returned %+v", admission)
			}
		})
	}
}

// TestWardlessShadowAdmissionIsItsOwn proves a shadow line's failure is the
// shadow's alone. Each line of a role is admitted in its own call: the shadow
// line's refusal is that call's error, and the primary line still admits to
// the same result, with its input untouched.
func TestWardlessShadowAdmissionIsItsOwn(t *testing.T) {
	primaryIn := validWardlessInput(t)
	shadowPrompt := "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	value := func(prompt string) string {
		return primaryIn.Agent.Name + "@" + string(primaryIn.Agent.Digest) + "/task-namer@" + prompt
	}
	lineup, err := domain.ResolveLineup([]domain.PolicyKey{
		{Key: "lineup.role.task_namer", Value: value(string(wardlessPromptDigest))},
		{Key: "lineup.role.task_namer.shadow.candidate", Value: value(shadowPrompt)},
	})
	if err != nil {
		t.Fatal(err)
	}
	primaryLine, bound := lineup.Line(domain.RoleTaskNamer)
	shadowLine, present := lineup.ShadowLine(domain.RoleTaskNamer)
	if !bound || !present {
		t.Fatalf("lineup lines: primary %t, shadow %t", bound, present)
	}
	primaryIn.Line = primaryLine
	wantInput := primaryIn
	primary, err := domain.AdmitWardlessRole(primaryIn)
	if err != nil {
		t.Fatal(err)
	}

	// The shadow line names a prompt digest the revision does not resolve.
	shadowIn := primaryIn
	proof := *primaryIn.LaunchProof
	shadowIn.LaunchProof = &proof
	shadowIn.Line = shadowLine.Selection
	shadow, err := domain.AdmitWardlessRole(shadowIn)
	if !errors.Is(err, domain.ErrLineupSelectionMismatch) {
		t.Fatalf("shadow admission = %v, want %v", err, domain.ErrLineupSelectionMismatch)
	}
	if shadow != (domain.WardlessAdmission{}) {
		t.Fatalf("refused shadow returned %+v", shadow)
	}
	if !reflect.DeepEqual(primaryIn, wantInput) {
		t.Fatal("shadow admission changed the primary line's input")
	}
	if again, err := domain.AdmitWardlessRole(primaryIn); err != nil || again != primary {
		t.Fatalf("primary admission after the shadow refusal = %+v, %v, want %+v", again, err, primary)
	}

	// With its prompt resolved, the same shadow line admits on its own.
	shadowIn.PromptDigest = domain.Digest(shadowPrompt)
	if shadow, err = domain.AdmitWardlessRole(shadowIn); err != nil || shadow.PromptDigest != domain.Digest(shadowPrompt) {
		t.Fatalf("resolved shadow admission = %+v, %v", shadow, err)
	}
}

// TestWardlessAdmissionRecordsLineageWithoutGating proves review independence
// is not an admission gate (plan revision 65): a judging role admits on the
// same lineage group as a writing role's agent, and on an unknown (empty)
// lineage group.
func TestWardlessAdmissionRecordsLineageWithoutGating(t *testing.T) {
	writerLineage := offerFragment(t).LineageGroup
	for _, lineage := range []string{writerLineage, ""} {
		in := wardlessInput(t, claudeCallAdapter(t, nil), wardlessOffer(t, lineage))
		in.Role = domain.RoleFindingAdjudicator
		if _, err := domain.AdmitWardlessRole(in); err != nil {
			t.Fatalf("adjudicator on lineage %q = %v", lineage, err)
		}
	}
	// The lineup validator sees no lineage at all: one agent on a writing
	// role and a judging role resolves.
	if err := domain.ValidateLineupPolicyKeys([]domain.PolicyKey{
		lineupKey("lineup.role.implementer"), lineupKey("lineup.role.reviewer"),
		lineupKey("lineup.role.finding_adjudicator"),
	}); err != nil {
		t.Fatalf("same agent on writing and judging roles = %v", err)
	}
}

func TestWardlessAdmissionValidate(t *testing.T) {
	valid, err := domain.AdmitWardlessRole(validWardlessInput(t))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		mutate  func(*domain.WardlessAdmission)
		wantErr error
	}{
		{"unknown role", func(a *domain.WardlessAdmission) { a.Role = "verification" }, domain.ErrInvalidRoleName},
		{"ward role", func(a *domain.WardlessAdmission) { a.Role = domain.RoleReviewer }, domain.ErrRoleNotWardless},
		{"agent digest", func(a *domain.WardlessAdmission) { a.AgentDigest = "haiku-judge" }, domain.ErrInvalidDigest},
		{"prompt digest", func(a *domain.WardlessAdmission) { a.PromptDigest = "" }, domain.ErrInvalidDigest},
		{"lineup revision", func(a *domain.WardlessAdmission) { a.LineupRevision = "" }, domain.ErrInvalidDigest},
		{"enrollment id", func(a *domain.WardlessAdmission) { a.EnrollmentID = "" }, domain.ErrEmptyID},
		{"generation", func(a *domain.WardlessAdmission) { a.EnrollmentGeneration = 0 }, domain.ErrNonPositive},
		{"audit harness build", func(a *domain.WardlessAdmission) { a.InterimLaunchAudit.HarnessBuild = "" }, domain.ErrEmptyField},
		{"audit adapter digest", func(a *domain.WardlessAdmission) { a.InterimLaunchAudit.AdapterDigest = "" }, domain.ErrInvalidDigest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			admission := valid
			tc.mutate(&admission)
			if err := admission.Validate(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestWardlessAdmissionGolden pins the admission result's serialized shape.
func TestWardlessAdmissionGolden(t *testing.T) {
	admission, err := domain.AdmitWardlessRole(validWardlessInput(t))
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(admission, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "wardless_admission", append(body, '\n'))
}
