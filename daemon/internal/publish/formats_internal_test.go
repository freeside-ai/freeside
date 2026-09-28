package publish

import "testing"

// TestCurrentStateFormatVersions pins the reported versions to the ones the
// decoders accept, so the two can't drift apart.
func TestCurrentStateFormatVersions(t *testing.T) {
	t.Parallel()
	got := CurrentStateFormatVersions()
	want := StateFormatVersions{
		InstallationAuthority:      installationAuthoritySnapshotVersion,
		InstallationJanitorJournal: installationJanitorJournalVersion,
	}
	if got != want {
		t.Fatalf("CurrentStateFormatVersions() = %+v, want %+v", got, want)
	}
}
