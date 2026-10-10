package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// Preflight reports every judgment role beside judgment_configuration and
// fails naming the ones the lineup cannot fill; a role left off passes. The
// check waits for the judgment configuration and for the lineup.
func TestPreflightReportsJudgmentRoles(t *testing.T) {
	args, environment := preflightFixture(t)
	cfg, _ := judgmentFixture(t)
	root := args[slices.Index(args, "-review-input-root")+1]
	tokenPath := filepath.Join(root, "judgment-token")
	if err := os.WriteFile(tokenPath, []byte("synthetic-subscription-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	args = append(args, "-judgment-claude-bin", cfg.CLI.Binary, "-judgment-claude-sha256", cfg.CLI.SHA256,
		"-judgment-model", cfg.CLI.Model, "-judgment-auth-snapshot", "judgment-token")
	run := func() (compositionManifest, error) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		err := runPreflightCommandWithEnvironment(t.Context(), args, &stdout, &stderr, environment,
			time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC), "test-build")
		var manifest compositionManifest
		if decodeErr := json.Unmarshal(stdout.Bytes(), &manifest); decodeErr != nil {
			t.Fatal(decodeErr)
		}
		return manifest, err
	}
	check := func(manifest compositionManifest) compositionCheck {
		t.Helper()
		for _, check := range manifest.Checks {
			if check.Name == "judgment_roles" {
				return check
			}
		}
		t.Fatal("manifest has no judgment_roles check")
		return compositionCheck{}
	}

	environment.judgmentRoles = []inference.RoleCheck{
		{Role: domain.RoleDriftAuditor}, {Role: domain.RoleTaskNamer}, {Role: domain.RolePublicationAuthor, Off: true},
	}
	manifest, err := run()
	if got := check(manifest); err != nil || got.Status != compositionPassed ||
		!strings.Contains(got.Evidence, "admits 2 judgment roles") || !strings.Contains(got.Evidence, "publication_author") {
		t.Fatalf("admitted roles: err = %v, check = %+v", err, got)
	}
	if want := []compositionJudgmentRole{
		{Role: domain.RoleDriftAuditor, Status: judgmentRoleAdmitted},
		{Role: domain.RoleTaskNamer, Status: judgmentRoleAdmitted},
		{Role: domain.RolePublicationAuthor, Status: judgmentRoleOff},
	}; !slices.Equal(manifest.JudgmentRoles, want) {
		t.Fatalf("judgment roles = %+v", manifest.JudgmentRoles)
	}

	environment.judgmentRoles[0].Err = errors.New("role drift_auditor has no lineup line")
	manifest, err = run()
	if got := check(manifest); err == nil || manifest.Status != compositionFailed || got.Status != compositionFailed ||
		!strings.Contains(got.Evidence, "drift_auditor") || strings.Contains(got.Evidence, "task_namer") ||
		got.Remediation == "" {
		t.Fatalf("unbound role: err = %v, check = %+v", err, got)
	}
	if got := manifest.JudgmentRoles[0]; got.Status != judgmentRoleUnbound || got.Reason == "" {
		t.Fatalf("unbound role entry = %+v", got)
	}

	environment.judgmentRoles, environment.judgmentRolesError = nil, errors.New("tree unreadable")
	manifest, err = run()
	if got := check(manifest); err == nil || got.Status != compositionFailed || len(manifest.JudgmentRoles) != 0 {
		t.Fatalf("unreadable tree: err = %v, check = %+v", err, got)
	}

	environment.judgmentRolesError = nil
	environment.agentsError = errors.New("no tree")
	manifest, _ = run()
	if got := check(manifest); got.Status != compositionNotRun {
		t.Fatalf("lineup unresolved: check = %+v", got)
	}

	environment.agentsError = nil
	if err := os.WriteFile(tokenPath, []byte("invalid\ntoken"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, _ = run()
	if got := check(manifest); got.Status != compositionNotRun ||
		checkStatus(manifest, "judgment_configuration") != compositionFailed {
		t.Fatalf("judgment configuration failed: check = %+v", got)
	}
}

// The check preflight runs is the daemon's own lookup: over an adopted tree
// and a real store it admits every role, names a role whose line is gone, and
// reports the publication author off when no prompt file is configured.
func TestPreflightJudgmentRoleCheckReadsTheAdoptedLineup(t *testing.T) {
	ctx := context.Background()
	f := newAuthAdoptFixture(t)
	authorBody := []byte("write the pull request for a reviewer\n")
	authorPath := filepath.Join(f.promptDir, "author")
	if err := os.WriteFile(authorPath, authorBody, 0o600); err != nil {
		t.Fatal(err)
	}
	_, patch, err := f.run(t, f.args("-judgment-publication-author-prompt", authorPath))
	if err != nil {
		t.Fatalf("auth adopt: %v", err)
	}
	adopted := loadAdoptedPatch(t, patch)
	revision := domain.Digest("sha256:" + strings.Repeat("4", 64))
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	f.withStore(t, func(st *store.Store) {
		evaluate := func(tree agenttree.Tree, runtime judgmentRuntime) compositionManifest {
			t.Helper()
			manifest := compositionManifest{
				Status: compositionPassed,
				Checks: []compositionCheck{{Name: "judgment_roles", Status: compositionNotRun}},
			}
			checks, err := inference.CheckRoles(ctx,
				judgmentRoles{st: st, tree: tree, revision: revision, runtime: runtime},
				judgmentSites(inference.Budget{}), now)
			evaluateJudgmentRoles(&manifest, checks, err)
			return manifest
		}
		statuses := func(manifest compositionManifest) map[domain.RoleName]judgmentRoleStatus {
			out := map[domain.RoleName]judgmentRoleStatus{}
			for _, role := range manifest.JudgmentRoles {
				out[role.Role] = role.Status
			}
			return out
		}

		manifest := evaluate(adopted, judgmentRuntime{AuthorPrompt: authorBody})
		if got := statuses(manifest); manifest.Status != compositionPassed || len(got) != 7 {
			t.Fatalf("adopted lineup: status = %s, roles = %+v", manifest.Status, manifest.JudgmentRoles)
		}
		for _, role := range manifest.JudgmentRoles {
			if role.Status != judgmentRoleAdmitted {
				t.Fatalf("adopted role = %+v", role)
			}
		}

		unlined := adopted
		unlined.Lineup = slices.DeleteFunc(slices.Clone(adopted.Lineup), func(line agenttree.LineupLine) bool {
			return domain.RoleName(line.Key) == domain.RoleDriftAuditor
		})
		manifest = evaluate(unlined, judgmentRuntime{})
		got := statuses(manifest)
		if manifest.Status != compositionFailed || got[domain.RoleDriftAuditor] != judgmentRoleUnbound ||
			got[domain.RolePublicationAuthor] != judgmentRoleOff || got[domain.RoleTaskNamer] != judgmentRoleAdmitted ||
			!strings.Contains(manifest.Checks[0].Evidence, "drift_auditor") ||
			strings.Contains(manifest.Checks[0].Evidence, "publication_author") {
			t.Fatalf("unlined drift auditor: manifest = %+v", manifest)
		}
	})
}

func TestJudgmentRoleStatusRegistration(t *testing.T) {
	want := []judgmentRoleStatus{judgmentRoleAdmitted, judgmentRoleOff, judgmentRoleUnbound}
	if !slices.Equal(AllJudgmentRoleStatuses, want) {
		t.Fatalf("statuses = %v, want %v", AllJudgmentRoleStatuses, want)
	}
	for _, status := range AllJudgmentRoleStatuses {
		if !status.valid() {
			t.Fatalf("registered status %q is invalid", status)
		}
	}
	if judgmentRoleStatus("").valid() || judgmentRoleStatus("unknown").valid() {
		t.Fatal("invalid judgment role status passed validation")
	}
}
