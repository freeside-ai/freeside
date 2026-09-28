package publish

// StateFormatVersions are the versions of the two App authority state files
// in a publication state directory: the installation authority snapshot and
// the janitor journal. Each decoder accepts only its one version, so a build
// reads and writes exactly these. A build that shares a state directory with
// another build compares them before it writes, so neither build is left
// with a file it refuses to read.
type StateFormatVersions struct {
	InstallationAuthority      int
	InstallationJanitorJournal int
}

// CurrentStateFormatVersions reports the versions this build reads and
// writes.
func CurrentStateFormatVersions() StateFormatVersions {
	return StateFormatVersions{
		InstallationAuthority:      installationAuthoritySnapshotVersion,
		InstallationJanitorJournal: installationJanitorJournalVersion,
	}
}
