package daemoncompose

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestListScenarios(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := ListScenarios(&out); err != nil {
		t.Fatal(err)
	}
	want := "clean\t" + CleanScenario().Description + "\n" +
		"quota-park\t" + QuotaParkScenario().Description + "\n" +
		"quota-move\t" + QuotaMoveScenario().Description + "\n" +
		"quota-no-reset\t" + QuotaNoResetScenario().Description + "\n"
	if got := out.String(); got != want {
		t.Fatalf("ListScenarios = %q, want %q", got, want)
	}
	if _, err := LookupScenario("nope"); err == nil ||
		!strings.Contains(err.Error(), "not one of: clean, quota-park, quota-move, quota-no-reset") {
		t.Fatalf("LookupScenario(nope) = %v, want the known names", err)
	}
}

// TestSeedScenarioRepositoryRebuildsAPartialStart covers a start that died
// after git init but before the base commit: the next start rebuilds the
// repository instead of failing on the missing ref forever.
func TestSeedScenarioRepositoryRebuildsAPartialStart(t *testing.T) {
	t.Parallel()
	files := CleanScenario().BaseFiles
	fresh, err := seedScenarioRepository(filepath.Join(t.TempDir(), "repository"), files)
	if err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(t.TempDir(), "repository")
	if err := os.MkdirAll(partial, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := scenarioGit(partial, "init", "-q", "-b", scenarioBaseRef); err != nil {
		t.Fatal(err)
	}
	for pass := range 2 {
		got, err := seedScenarioRepository(partial, files)
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if got != fresh {
			t.Fatalf("pass %d: base %s, want %s", pass, got, fresh)
		}
	}
}
