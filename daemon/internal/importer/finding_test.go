package importer

import "testing"

func TestFindingKindValid(t *testing.T) {
	for _, k := range AllFindingKinds {
		if !k.valid() {
			t.Errorf("registered kind %q reported invalid", k)
		}
	}
	for _, k := range []FindingKind{"", "bogus"} {
		if k.valid() {
			t.Errorf("kind %q reported valid", k)
		}
	}
}

// TestFindingKindBlocking pins the blocking classification: the three kinds a
// git tree cannot faithfully represent, plus decoded plan secrets, withhold the commit;
// every policy-only kind leaves it available for the §5.5 control-plane
// route. A new kind must be added here deliberately.
func TestFindingKindBlocking(t *testing.T) {
	want := map[FindingKind]bool{
		FindingNonRegularChange:        true,
		FindingInvalidPathEntry:        true,
		FindingBlobOmitted:             true,
		FindingAutomationControlPath:   false,
		FindingReviewerInstructionPath: false,
		FindingGitMetadataPath:         false,
		FindingVerificationRecipePath:  false,
		FindingPromptsPolicyPath:       false,
		FindingEgressTrustPath:         false,
		FindingMaterialityRulesPath:    false,
		FindingAllowlistViolation:      false,
		FindingSizeViolation:           false,
		FindingPathCollision:           false,
		FindingSecret:                  false,
		FindingSecretScanSkipped:       false,
		FindingCommitPlanSecret:        true,
	}
	if len(want) != len(AllFindingKinds) {
		t.Fatalf("blocking table lists %d kinds, registry has %d", len(want), len(AllFindingKinds))
	}
	for _, k := range AllFindingKinds {
		got, ok := want[k]
		if !ok {
			t.Errorf("kind %q missing from the blocking table", k)
			continue
		}
		if k.blocksCommit() != got {
			t.Errorf("blocksCommit(%q) = %v, want %v", k, k.blocksCommit(), got)
		}
	}
	if FindingKind("").blocksCommit() {
		t.Error("invalid zero kind must not block (it never occurs); it must also not panic")
	}
}

func TestFindingProfileValid(t *testing.T) {
	// Every named member is nonempty and the zero value "" is invalid, per the
	// daemon enum convention; the default is a nil *FindingProfile, not a ""
	// member.
	for _, p := range AllFindingProfiles {
		if !p.valid() {
			t.Errorf("registered profile %q reported invalid", p)
		}
	}
	if FindingProfile("").valid() {
		t.Error("the zero value must be invalid")
	}
	if FindingProfile("bogus").valid() {
		t.Error("unknown profile reported valid")
	}
}

// TestFindingFatalPublishStrict pins that every finding, of every kind, is
// fatal under the default publish-strict profile — both the explicit member
// and the nil (absent) profile, since repo-channel content is published so
// nothing is tolerated — except the advisory reviewer-instruction kind,
// which publishes surfaced (plan §5.8, revision 42; TestFindingAdvisoryStance).
func TestFindingFatalPublishStrict(t *testing.T) {
	strict := FindingProfilePublishStrict
	for _, k := range AllFindingKinds {
		want := k != FindingReviewerInstructionPath
		if (Finding{Kind: k}).Fatal(&strict) != want {
			t.Errorf("Fatal(%q, publish-strict) = %v, want %v", k, !want, want)
		}
		if (Finding{Kind: k}).Fatal(nil) != want {
			t.Errorf("Fatal(%q, nil) = %v, want %v (nil is the strict default)", k, !want, want)
		}
	}
	// An unknown profile fails closed to fully strict.
	bogus := FindingProfile("bogus")
	if !(Finding{Kind: FindingAllowlistViolation}).Fatal(&bogus) {
		t.Error("an unknown profile must treat every finding as fatal")
	}
}

// TestFindingFatalSpecification is the fatality table for the elaboration
// profile: an exhaustive per-kind decision so a new FindingKind is forced to
// choose its class rather than defaulting silently either way.
func TestFindingFatalSpecification(t *testing.T) {
	want := map[FindingKind]bool{
		FindingNonRegularChange:        true,
		FindingInvalidPathEntry:        true,
		FindingBlobOmitted:             true,
		FindingCommitPlanSecret:        true,
		FindingSecret:                  true,
		FindingAutomationControlPath:   true,
		FindingReviewerInstructionPath: true,
		FindingVerificationRecipePath:  true,
		FindingPromptsPolicyPath:       true,
		FindingEgressTrustPath:         true,
		FindingMaterialityRulesPath:    true,
		FindingGitMetadataPath:         true,
		FindingAllowlistViolation:      false,
		FindingSizeViolation:           false,
		FindingPathCollision:           false,
		FindingSecretScanSkipped:       false,
	}
	if len(want) != len(AllFindingKinds) {
		t.Fatalf("fatality table lists %d kinds, registry has %d", len(want), len(AllFindingKinds))
	}
	spec := FindingProfileSpecification
	for _, k := range AllFindingKinds {
		got, ok := want[k]
		if !ok {
			t.Errorf("kind %q missing from the specification fatality table", k)
			continue
		}
		if fatal := (Finding{Kind: k}).Fatal(&spec); fatal != got {
			t.Errorf("Fatal(%q, specification) = %v, want %v", k, fatal, got)
		}
	}
	// A tolerated kind that withholds the commit never occurs (blocksCommit and
	// tolerated are disjoint here), but the invalid zero kind must fail closed.
	if !(Finding{Kind: FindingKind("")}).Fatal(&spec) {
		t.Error("invalid zero kind must be fatal under any profile")
	}
	// Every inherently-commit-blocking kind must also be fatal for elaboration:
	// a withheld commit cannot become a completed candidate.
	for _, k := range AllFindingKinds {
		if k.blocksCommit() && !(Finding{Kind: k}).Fatal(&spec) {
			t.Errorf("commit-blocking kind %q tolerated under specification", k)
		}
	}
}
