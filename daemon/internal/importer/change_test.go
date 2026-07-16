package importer

import "testing"

func TestChangeKindValid(t *testing.T) {
	for _, k := range AllChangeKinds {
		if !k.valid() {
			t.Errorf("registered kind %q reported invalid", k)
		}
	}
	for _, k := range []ChangeKind{"", "renamed"} {
		if k.valid() {
			t.Errorf("kind %q reported valid", k)
		}
	}
}

func TestPlannedChangePublicRendering(t *testing.T) {
	p := plannedChange{
		path: "a.txt", kind: ChangeModified, mode: "100755",
		oid: "unused-internally", digest: sha256Digest("x"), size: 1,
	}
	got := p.public()
	want := Change{Path: "a.txt", Kind: ChangeModified, Mode: "100755", Digest: sha256Digest("x")}
	if got != want {
		t.Fatalf("public() = %+v, want %+v (no internal identity leaks)", got, want)
	}
}
