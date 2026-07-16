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

// TestFindingKindBlocking pins the blocking classification: the three
// kinds a git tree cannot faithfully represent withhold the commit;
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
		FindingAllowlistViolation:      false,
		FindingSizeViolation:           false,
		FindingPathCollision:           false,
		FindingSecret:                  false,
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
