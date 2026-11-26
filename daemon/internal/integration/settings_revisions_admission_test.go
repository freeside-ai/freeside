package integration_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const settingsProject domain.ProjectID = "project-widgets"

func settingsImage(name string) string {
	return "registry.local/" + name + "@sha256:" + strings.Repeat("a", 64)
}

// storeRunSettings appends one revision per required section of the
// instance and the project settings.
func storeRunSettings(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	instance := map[domain.InstanceSettingsSection]string{
		domain.InstanceSectionHostTools: `{"container":"/usr/local/bin/container"}`,
		domain.InstanceSectionImages: `{"exporter":"` + settingsImage("exporter") + `","review":"` +
			settingsImage("review") + `"}`,
		domain.InstanceSectionSeedRoot: `"/rig/seed"`,
		domain.InstanceSectionPrompts: `{"prompt_package":"/prompts/impl.json","specification_prompt_package":"/prompts/spec.json",` +
			`"remediation_prompt_package":"/prompts/rem.json","vendor_instructions":"/prompts/CLAUDE.md"}`,
		domain.InstanceSectionAgentTree: `{"checkout":"/src/freeside","commit":"` + strings.Repeat("d", 40) + `"}`,
		domain.InstanceSectionReview: `{"input_root":"/review","auth_mode":"subscription",` +
			`"auth_snapshot":"/review/auth.json","instructions":"/review/AGENTS.md"}`,
		domain.InstanceSectionTimeouts: `{"writer_stop":"20m","writer_stall":"0s"}`,
		domain.InstanceSectionBuild:    `{"proxy":"","base_build_ref":"","dns":[]}`,
	}
	project := map[domain.ProjectSettingsSection]string{
		domain.ProjectSectionRepository:     `{"repo":"acme/widgets","repository_id":42,"base_ref":"main"}`,
		domain.ProjectSectionAllowedPaths:   `["src/**"]`,
		domain.ProjectSectionApprovedRecipe: `"sha256:` + strings.Repeat("b", 64) + `"`,
		domain.ProjectSectionAgentImage:     `"` + settingsImage("agent") + `"`,
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		for _, section := range domain.AllInstanceSettingsSections {
			if value, ok := instance[section]; ok {
				if _, err := tx.AppendInstanceSettingsRevision(ctx, domain.InstanceSettingsInput{
					Section: section, Value: json.RawMessage(value), SetBy: "cli:operator",
				}, admittedAt); err != nil {
					return err
				}
			}
		}
		for _, section := range domain.AllProjectSettingsSections {
			if value, ok := project[section]; ok {
				if _, err := tx.AppendProjectSettingsRevision(ctx, domain.ProjectSettingsInput{
					ProjectID: settingsProject, Section: section, Value: json.RawMessage(value), SetBy: "cli:operator",
				}, admittedAt); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestAdmissionRecordsTheComposedSettingsRevisions drives the revisions a
// daemon loads from the store through NewClaudeDriverConfig and the
// engine's admission onto the stored admission, then appends newer
// revisions of both records and reads the admission back unchanged.
func TestAdmissionRecordsTheComposedSettingsRevisions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	declared := []exec.Capability{exec.CapDetachableWorkspace, exec.CapPostExitExport, exec.CapReadOnlyRemount}
	floor := []exec.Capability{exec.CapPostExitExport}
	var composed *domain.SettingsRevisions
	f := openAdmittingFixtureWith(t, declared, floor, domain.NewCapabilitySnapshot(floor...),
		func(st *store.Store) (engine.AdmissionEnvironment, engine.ProjectAuthoritySource) {
			storeRunSettings(t, st)
			settings, err := daemoncompose.LoadRunSettings(ctx, st, settingsProject)
			if err != nil {
				t.Fatalf("LoadRunSettings: %v", err)
			}
			cfg := daemoncompose.NewClaudeDriverConfig(settings, daemoncompose.RunLaunch{})
			composed = cfg.SettingsRevisions
			env := admissionEnvironment()
			env.InstanceSettingsRevision = cfg.SettingsRevisions.Instance
			// The fixture's run is not of the stored project, so the
			// project revision reaches admission through a fixed authority;
			// the stored lookup's own revision is tested in daemoncompose.
			return env, engine.FixedProjectAuthority(engine.ProjectAuthority{
				SettingsRevision: cfg.SettingsRevisions.Project,
				Repo:             env.Base.Repo, RepositoryID: env.Base.RepositoryID, BaseRef: env.Base.BaseRef,
				WriterClass: new(domain.BackendFreshVMReadOnlyVolumeHandoff),
			})
		})
	want := domain.SettingsRevisions{Instance: 8, Project: 4}
	if composed == nil || *composed != want {
		t.Fatalf("composed revisions = %v, want %+v", composed, want)
	}

	invocationID, result, err := dispatchOneInvocation(t, f)
	if err != nil || result.InvocationsStarted != 1 {
		t.Fatalf("Reconcile = %#v, %v; want one start", result, err)
	}
	read := func() domain.ExecutionAdmission {
		t.Helper()
		var admission domain.ExecutionAdmission
		if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			admission, err = tx.GetExecutionAdmission(ctx, invocationID)
			return err
		}); err != nil {
			t.Fatalf("GetExecutionAdmission: %v", err)
		}
		return admission
	}
	admitted := read()
	if admitted.SettingsRevisions == nil || *admitted.SettingsRevisions != want {
		t.Fatalf("admitted revisions = %v, want %+v", admitted.SettingsRevisions, want)
	}

	// Newer revisions of both records leave the admitted ones in place.
	if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if _, err := tx.AppendInstanceSettingsRevision(ctx, domain.InstanceSettingsInput{
			Section: domain.InstanceSectionSeedRoot, Value: json.RawMessage(`"/rig/seed-2"`), SetBy: "cli:operator",
		}, admittedAt); err != nil {
			return err
		}
		_, err := tx.AppendProjectSettingsRevision(ctx, domain.ProjectSettingsInput{
			ProjectID: settingsProject, Section: domain.ProjectSectionAllowedPaths,
			Value: json.RawMessage(`["src/**","docs/**"]`), SetBy: "cli:operator",
		}, admittedAt)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	reread := read()
	if reread.ID != admitted.ID || reread.SettingsRevisions == nil || *reread.SettingsRevisions != want {
		t.Fatalf("after newer revisions: admission %s with %v, want %s with %+v",
			reread.ID, reread.SettingsRevisions, admitted.ID, want)
	}
}
