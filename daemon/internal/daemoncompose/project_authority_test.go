package daemoncompose_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const otherSettingsProject domain.ProjectID = "project-gadgets"

// appendProjectSections appends one revision per section given, in
// registration order, to project's record.
func appendProjectSections(t *testing.T, st *store.Store, project domain.ProjectID, sections map[domain.ProjectSettingsSection]string) {
	t.Helper()
	ctx := context.Background()
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		for _, section := range domain.AllProjectSettingsSections {
			value, ok := sections[section]
			if !ok {
				continue
			}
			if _, err := tx.AppendProjectSettingsRevision(ctx, domain.ProjectSettingsInput{
				ProjectID: project, Section: section, Value: json.RawMessage(value), SetBy: "cli:operator",
			}, runSettingsSetAt); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func storedAuthority(t *testing.T, st *store.Store, project domain.ProjectID) (engine.ProjectAuthority, error) {
	t.Helper()
	var authority engine.ProjectAuthority
	err := st.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		authority, err = daemoncompose.StoredProjectAuthority(context.Background(), tx, project)
		return err
	})
	return authority, err
}

// TestStoredProjectAuthorityReadsEachProjectsRecord: with two projects'
// records stored, each project's authority is its own record's, with the
// revision it came from, and an edit reaches the next read with no restart.
func TestStoredProjectAuthorityReadsEachProjectsRecord(t *testing.T) {
	t.Parallel()
	st := seedRunSettings(t, storedInstanceSections(), storedProjectSections())
	other := storedProjectSections()
	other[domain.ProjectSectionRepository] = `{"repo":"acme/gadgets","repository_id":43,"base_ref":"trunk"}`
	other[domain.ProjectSectionAllowedPaths] = `["lib/**"]`
	other[domain.ProjectSectionHandBackLabel] = `"needs rework"`
	other[domain.ProjectSectionWriterClass] = `"fresh_vm_read_only_volume_handoff"`
	appendProjectSections(t, st, otherSettingsProject, other)

	widgets, err := storedAuthority(t, st, runSettingsProject)
	if err != nil {
		t.Fatal(err)
	}
	if widgets.ProjectID != runSettingsProject || widgets.Repo != "acme/widgets" || widgets.RepositoryID != 42 ||
		widgets.BaseRef != "main" || !slices.Equal(widgets.AllowedPaths, []string{"src/**"}) ||
		widgets.HandBackLabel != "" || widgets.WriterClass != nil ||
		widgets.SettingsRevision != len(storedProjectSections()) {
		t.Fatalf("widgets authority %+v", widgets)
	}
	gadgets, err := storedAuthority(t, st, otherSettingsProject)
	if err != nil {
		t.Fatal(err)
	}
	if gadgets.Repo != "acme/gadgets" || gadgets.RepositoryID != 43 || gadgets.BaseRef != "trunk" ||
		!slices.Equal(gadgets.AllowedPaths, []string{"lib/**"}) || gadgets.HandBackLabel != "needs rework" ||
		gadgets.WriterClass == nil || *gadgets.WriterClass != domain.BackendFreshVMReadOnlyVolumeHandoff ||
		gadgets.SettingsRevision != len(other) {
		t.Fatalf("gadgets authority %+v", gadgets)
	}

	appendProjectSections(t, st, runSettingsProject, map[domain.ProjectSettingsSection]string{
		domain.ProjectSectionAllowedPaths: `["src/**","docs/**"]`,
	})
	edited, err := storedAuthority(t, st, runSettingsProject)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(edited.AllowedPaths, []string{"src/**", "docs/**"}) || edited.SettingsRevision != widgets.SettingsRevision+1 {
		t.Fatalf("after an edit: authority %+v, want the new paths at revision %d", edited, widgets.SettingsRevision+1)
	}
}

// TestStoredProjectAuthorityRefuses: a project the stored settings grant
// nothing is refused as ErrProjectSettingsRefused, joined with why: it has
// no record, its record fails the settings gate, it names a repository
// other than the project's binding, or it has no project image.
func TestStoredProjectAuthorityRefuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("no record", func(t *testing.T) {
		t.Parallel()
		st := seedRunSettings(t, storedInstanceSections(), storedProjectSections())
		if _, err := storedAuthority(t, st, otherSettingsProject); !errors.Is(err, engine.ErrProjectSettingsRefused) ||
			!errors.Is(err, domain.ErrSettingsSectionMissing) {
			t.Fatalf("error %v, want ErrProjectSettingsRefused naming a missing section", err)
		}
	})
	t.Run("settings gate", func(t *testing.T) {
		t.Parallel()
		project := storedProjectSections()
		project[domain.ProjectSectionAllowedPaths] = `["**"]`
		st := seedRunSettings(t, storedInstanceSections(), project)
		if _, err := storedAuthority(t, st, runSettingsProject); !errors.Is(err, engine.ErrProjectSettingsRefused) {
			t.Fatalf("error %v, want ErrProjectSettingsRefused", err)
		}
	})
	t.Run("projects binding", func(t *testing.T) {
		t.Parallel()
		st := seedRunSettings(t, storedInstanceSections(), storedProjectSections())
		bound, err := domain.NewProject(runSettingsProject, "acme/gadgets", 43)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error { return tx.RegisterProject(ctx, bound) }); err != nil {
			t.Fatal(err)
		}
		if _, err := storedAuthority(t, st, runSettingsProject); !errors.Is(err, engine.ErrProjectSettingsRefused) ||
			!errors.Is(err, store.ErrProjectRepositoryMismatch) {
			t.Fatalf("error %v, want ErrProjectSettingsRefused for the binding mismatch", err)
		}
	})
	// A start composes no driver for a no-image project, so no gate grants
	// its tasks anything under another project's driver either (#3620).
	t.Run("no project image", func(t *testing.T) {
		t.Parallel()
		st := seedRunSettings(t, storedInstanceSections(), storedProjectSections())
		appendProjectSections(t, st, otherSettingsProject, noImageProjectSections())
		if _, err := storedAuthority(t, st, otherSettingsProject); !errors.Is(err, engine.ErrProjectSettingsRefused) ||
			!errors.Is(err, daemoncompose.ErrNoProjectImage) || !strings.Contains(err.Error(), "no agent_image") {
			t.Fatalf("error %v, want ErrProjectSettingsRefused naming the missing image", err)
		}
	})
}
