package daemoncompose

import (
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec/claude"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// The proof pass seeds the project's real base, so it must get the workspace
// a run gets. Left at the suite's synthetic default (64 MB), a repository
// larger than that overflowed the volume and failed conformance on a
// categorical base-proof mismatch (#2382's live run).
func TestConformanceSuiteFixtureSeedsARunSizedWorkspace(t *testing.T) {
	t.Parallel()
	fx := conformanceSuiteFixture(
		"registry.example/agent@sha256:0000000000000000000000000000000000000000000000000000000000000000",
		domain.BaseRevision{Repo: "owner/repo", RepositoryID: 7, BaseRef: "main", BaseSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		"conf-0000000000000000", "conf-1111111111111111",
		[8]byte{}, "/seed/.conf-0000000000000000",
	)
	if fx.WorkspaceSizeMB != claude.WorkspaceSizeMB {
		t.Fatalf("conformance workspace = %d MB, want a run's %d MB", fx.WorkspaceSizeMB, claude.WorkspaceSizeMB)
	}
	if fx.Seed.Mode != ward.SeedBaseCheckout || fx.Seed.SourceDir != "/seed/.conf-0000000000000000" {
		t.Fatalf("conformance seed = %+v, want the base checkout it was given", fx.Seed)
	}
}
