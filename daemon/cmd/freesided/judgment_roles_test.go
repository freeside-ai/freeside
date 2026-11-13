package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/advisory"
	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	inferencefake "github.com/freeside-ai/freeside/daemon/internal/inference/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func judgmentTestSites() []inference.Site {
	limits := inference.Limits{Calls: 100, ComputeUnits: 10_000_000, AttentionItems: 100, Starvation: 10 * time.Hour}
	budget := inference.Budget{
		Window: 24 * time.Hour, Site: limits, Project: limits, Global: limits,
		MaxCallsPerRoot: 100, MaxStarvationPerRoot: 10 * time.Hour,
	}
	return daemoncompose.JudgmentSites(budget)
}

func judgmentRoleItems(t *testing.T, st *store.Store) map[domain.RoleName]domain.AttentionItem {
	t.Helper()
	items := map[domain.RoleName]domain.AttentionItem{}
	if err := st.Read(context.Background(), func(tx *store.ReadTx) error {
		open, err := tx.ListOpenAttentionItems(context.Background(), domain.AttentionSystemHealth)
		for _, item := range open {
			rest, ok := strings.CutPrefix(string(item.ID), daemoncompose.JudgmentRoleItemPrefix)
			if !ok {
				continue
			}
			role := daemoncompose.JudgmentRoleOfCause(rest)
			if _, dup := items[role]; dup {
				t.Fatalf("role %s has two open items", role)
			}
			items[role] = item
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return items
}

func inferenceLedgerItems(t *testing.T, st *store.Store) []domain.AttentionItem {
	t.Helper()
	var items []domain.AttentionItem
	if err := st.Read(context.Background(), func(tx *store.ReadTx) error {
		open, err := tx.ListOpenAttentionItems(context.Background(), domain.AttentionSystemHealth)
		for _, item := range open {
			if strings.HasPrefix(string(item.ID), daemoncompose.InferenceLedgerItemPrefix) {
				items = append(items, item)
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return items
}

// TestJudgmentSitesReadTheAdoptedLineup runs every judgment site through the
// daemon's role source over an adopted tree and a real store. Each role the
// lineup names admits and its call is stamped; a role with no line, a line
// naming another prompt than the daemon runs, and a marked generation each
// return the site's fail-safe and raise one item with the contracted posture;
// the item resolves once the role admits; and the publication author with no
// prompt file is off, which raises nothing.
func TestJudgmentSitesReadTheAdoptedLineup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAuthAdoptFixture(t)
	authorBody := []byte("write the pull request for a reviewer\n")
	authorPath := filepath.Join(f.promptDir, "author")
	if err := os.WriteFile(authorPath, authorBody, 0o600); err != nil {
		t.Fatal(err)
	}
	// The escalated specifier asks for the triager's work, so every judgment
	// role is asked for.
	_, patch, err := f.run(t, f.args(append([]string{"-judgment-publication-author-prompt", authorPath}, adoptEscalatedArgs()...)...))
	if err != nil {
		t.Fatalf("auth adopt: %v", err)
	}
	adopted := loadAdoptedPatch(t, patch)
	revision := domain.Digest("sha256:" + strings.Repeat("4", 64))
	sites := judgmentTestSites()
	fields := func(site inference.Site) map[string]inference.InputField {
		out := map[string]inference.InputField{}
		for _, policy := range site.Fields {
			out[policy.Name] = inference.InputField{Value: "data", Sensitivity: policy.Sensitivity}
		}
		return out
	}
	without := func(tree agenttree.Tree, roles ...domain.RoleName) agenttree.Tree {
		tree.Lineup = slices.DeleteFunc(slices.Clone(tree.Lineup), func(line agenttree.LineupLine) bool {
			return slices.Contains(roles, domain.RoleName(line.Key))
		})
		return tree
	}
	const unbound = inference.FallbackRoleUnbound

	f.withStore(t, func(st *store.Store) {
		now := func() time.Time { return time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC) }
		driver := inferencefake.New()
		runtime := daemoncompose.JudgmentRuntime{Driver: driver, AuthorPrompt: authorBody}
		credentials := fixedJudgmentCredentials(t)
		health := daemoncompose.NewJudgmentRoleHealth(st, now, nil)
		newClient := func(tree agenttree.Tree, runtime daemoncompose.JudgmentRuntime, health inference.HealthReporter) *inference.Client {
			t.Helper()
			client, err := inference.New(inference.Config{
				StatePath: filepath.Join(t.TempDir(), "ledger.json"),
				Roles: daemoncompose.JudgmentRoles{
					Store: st, Tree: tree, Revision: revision, Runtime: runtime, Credentials: credentials,
				},
				Health: health, Sites: sites,
				Advisory: advisory.Unavailable(errors.New("unused")), Now: now,
			})
			if err != nil {
				t.Fatal(err)
			}
			return client
		}
		call := func(client *inference.Client, site inference.Site) inference.CallResult {
			t.Helper()
			result, err := client.Call(ctx, site.ID, "project-1", "run-1", fields(site))
			if err != nil {
				t.Fatal(err)
			}
			return result
		}

		// Every role the adopted lineup names admits, at startup and per call.
		whole := newClient(adopted, runtime, health)
		for _, check := range whole.CheckRoles(ctx) {
			if check.Err != nil || check.Off {
				t.Fatalf("adopted role %s = %+v", check.Role, check)
			}
		}
		for _, site := range sites {
			result := call(whole, site)
			role, _ := domain.RoleForSite(site.ID)
			if result.Identity == nil || result.Identity.Role != role || result.Producer == "unavailable/unbound" {
				t.Fatalf("%s on the adopted lineup = %+v", site.ID, result)
			}
		}
		requests := driver.Requests()
		if len(requests) != len(sites) {
			t.Fatalf("driver saw %d calls, want %d", len(requests), len(sites))
		}
		// Every judgment role launches with the model and effort the lineup
		// decides: the ones adoption wrote onto the call offer and agent.
		launched := map[domain.RoleName]bool{}
		for _, request := range requests {
			role, _ := domain.RoleForSite(request.SiteID)
			if request.Model != adoptJudgmentModel || request.NativeEffort != "medium" {
				t.Fatalf("%s launched with model %q effort %q, want the adopted %q at medium",
					request.SiteID, request.Model, request.NativeEffort, adoptJudgmentModel)
			}
			launched[role] = true
		}
		for _, role := range agentbaseline.JudgmentRoles() {
			if !launched[role] {
				t.Fatalf("judgment role %s made no call on the adopted lineup", role)
			}
		}
		// Every other role is sent an empty prompt.
		triagerPrompt, _ := inference.CodeOwnedRolePrompt(domain.RoleTriager)
		prompts := map[domain.RoleName]string{
			domain.RolePublicationAuthor: string(authorBody),
			domain.RoleTriager:           string(triagerPrompt.Body),
		}
		for _, request := range requests {
			role, _ := domain.RoleForSite(request.SiteID)
			if want := prompts[role]; string(request.RolePrompt) != want {
				t.Fatalf("%s was sent role prompt %q, want %q", request.SiteID, request.RolePrompt, want)
			}
		}
		if items := judgmentRoleItems(t, st); len(items) != 0 {
			t.Fatalf("items with every role admitted = %+v", items)
		}

		// No line: the fail-safe, and one item per role with its posture.
		missing := []domain.RoleName{domain.RoleDriftAuditor, domain.RoleFindingClassifier}
		partial := newClient(without(adopted, missing...), runtime, health)
		before := len(driver.Requests())
		for range 2 {
			for _, site := range sites {
				role, _ := domain.RoleForSite(site.ID)
				result := call(partial, site)
				if slices.Contains(missing, role) != (result.Reason == unbound) {
					t.Fatalf("%s with no %v lines = %+v", site.ID, missing, result)
				}
				if result.Reason == unbound && (!result.Fallback || string(result.Output) != site.FailSafe || result.Identity != nil) {
					t.Fatalf("%s did not return its fail-safe: %+v", site.ID, result)
				}
			}
		}
		if got := len(driver.Requests()) - before; got != 2*(len(sites)-len(missing)) {
			t.Fatalf("driver saw %d calls with two roles unbound", got)
		}
		items := judgmentRoleItems(t, st)
		if len(items) != len(missing) {
			t.Fatalf("items with two roles unbound = %+v", items)
		}
		for role, item := range items {
			posture, impairs := domain.HealthPostureAdvisory, domain.ImpairedCapabilityNone
			if role == domain.RoleDriftAuditor {
				posture, impairs = domain.HealthPostureBlocking, domain.ImpairedCapabilityUnattendedAdmission
			}
			if !slices.Contains(missing, role) || item.Posture == nil || *item.Posture != posture ||
				item.HealthDiagnostic == nil || item.HealthDiagnostic.Impairs != impairs ||
				item.HealthDiagnostic.Code != "judgment_role_unbound" ||
				!strings.Contains(item.Reason, "Judgment role "+string(role)) ||
				!strings.Contains(item.Reason, "the lineup has no line for it") {
				t.Fatalf("item for %s = %+v", role, item)
			}
		}

		// A restarted daemon knows nothing of the items it left. Reporting
		// the same state again writes nothing; the startup check resolves the
		// item of a role fixed since, with no call needed.
		health = daemoncompose.NewJudgmentRoleHealth(st, now, nil)
		at := storeRevision(t, st)
		newClient(without(adopted, missing...), runtime, health).CheckRoles(ctx)
		if storeRevision(t, st) != at || len(judgmentRoleItems(t, st)) != len(missing) {
			t.Fatal("reporting an unchanged state wrote to the store")
		}
		newClient(without(adopted, domain.RoleFindingClassifier), runtime, health).CheckRoles(ctx)
		items = judgmentRoleItems(t, st)
		if _, open := items[domain.RoleFindingClassifier]; len(items) != 1 || !open {
			t.Fatalf("items after the drift auditor's line returned = %+v", items)
		}
		// And a call that admits resolves its role's item.
		classifier := sites[0]
		whole = newClient(adopted, runtime, health)
		if result := call(whole, classifier); result.Identity == nil {
			t.Fatalf("classifier on the adopted lineup = %+v", result)
		}
		if items := judgmentRoleItems(t, st); len(items) != 0 {
			t.Fatalf("items after every role admits = %+v", items)
		}

		// With no escalated specifier line nothing asks for the triager's
		// work: it is off even with no line of its own, and nothing is raised.
		unescalated := without(adopted, domain.RoleTriager)
		unescalated.Lineup = slices.DeleteFunc(slices.Clone(unescalated.Lineup), func(line agenttree.LineupLine) bool {
			return strings.HasPrefix(line.Key, string(domain.RoleSpecifier)+".escalated.")
		})
		triager := sites[slices.IndexFunc(sites, func(site inference.Site) bool { return site.ID == inference.TriagerSiteID })]
		if result := call(newClient(unescalated, runtime, health), triager); !result.Fallback || result.Reason != inference.FallbackRoleOff {
			t.Fatalf("triager with no escalated specifier = %+v", result)
		}
		// With the escalated line, a missing triager line is unbound.
		if result := call(newClient(without(adopted, domain.RoleTriager), runtime, health), triager); result.Reason != unbound {
			t.Fatalf("triager with no line under an escalated specifier = %+v", result)
		}
		if items := judgmentRoleItems(t, st); len(items) != 1 {
			t.Fatalf("items with the triager unbound = %+v", items)
		}
		if result := call(whole, triager); result.Identity == nil {
			t.Fatalf("triager on the adopted lineup = %+v", result)
		}
		if items := judgmentRoleItems(t, st); len(items) != 0 {
			t.Fatalf("items after the triager admits = %+v", items)
		}

		// The publication author with no prompt file is off: both sites fail
		// safe as inference being down, and nothing is raised.
		authorSites := slices.DeleteFunc(slices.Clone(sites), func(site inference.Site) bool {
			return site.ID != inference.PublicationAuthorExplainSiteID && site.ID != inference.PublicationAuthorProposeSiteID
		})
		off := newClient(adopted, daemoncompose.JudgmentRuntime{Driver: driver}, health)
		for _, site := range authorSites {
			if result := call(off, site); !result.Fallback || result.Reason != inference.FallbackRoleOff {
				t.Fatalf("%s with no prompt file = %+v", site.ID, result)
			}
		}
		if items := judgmentRoleItems(t, st); len(items) != 0 {
			t.Fatalf("items with the publication author off = %+v", items)
		}
		// With another prompt file than the line names, it is unbound.
		other := runtime
		other.AuthorPrompt = []byte("another prompt\n")
		if result := call(newClient(adopted, other, health), authorSites[0]); result.Reason != unbound {
			t.Fatalf("publication author under another prompt = %+v", result)
		}
		items = judgmentRoleItems(t, st)
		if item, open := items[domain.RolePublicationAuthor]; len(items) != 1 || !open ||
			*item.Posture != domain.HealthPostureAdvisory {
			t.Fatalf("items with the author's prompt changed = %+v", items)
		}

		// A marked generation fails the admission of every role on it.
		source := lineupSelection(t, adopted, agentbaseline.JudgmentRoles()[0])
		if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
			generation, err := tx.CurrentEnrollmentGeneration(ctx, source.EnrollmentID)
			if err != nil {
				return err
			}
			_, err = tx.RecordGenerationIntegrityMark(ctx, domain.GenerationIntegrityMark{
				EnrollmentID: generation.EnrollmentID, Ordinal: generation.Ordinal,
				Finding: domain.CredentialIntegrityCorruption, ObservedAt: now(),
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		before = len(driver.Requests())
		for _, site := range sites {
			if result := call(whole, site); result.Reason != unbound || string(result.Output) != site.FailSafe {
				t.Fatalf("%s on a marked generation = %+v", site.ID, result)
			}
		}
		if len(driver.Requests()) != before {
			t.Fatal("a role on a marked generation reached the driver")
		}
		items = judgmentRoleItems(t, st)
		if len(items) != len(agentbaseline.JudgmentRoles()) {
			t.Fatalf("items on a marked generation = %+v", items)
		}
		for role, item := range items {
			if blocking := *item.Posture == domain.HealthPostureBlocking; blocking != (role == domain.RoleDriftAuditor) {
				t.Fatalf("item for %s has posture %s", role, *item.Posture)
			}
		}
	})
}

// With the judgment flags absent every judgment role is off, whatever the
// tree holds: the startup check reports each role off and raises no item, a
// call at any site returns its fail-safe as inference being unavailable
// under the off reason and never the unbound one, and nothing past the off
// answer is read, so no line, store row, or credential is needed. An item an
// earlier start with judgments on had left is resolved, because a deployment
// that switched judgments off has nothing for the item to hold (#1945).
func TestJudgmentRolesAreOffWithTheFlagsAbsent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAuthAdoptFixture(t)
	authorPath := filepath.Join(f.promptDir, "author")
	if err := os.WriteFile(authorPath, []byte("write the pull request for a reviewer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, patch, err := f.run(t, f.args("-judgment-publication-author-prompt", authorPath))
	if err != nil {
		t.Fatalf("auth adopt: %v", err)
	}
	withLines := loadAdoptedPatch(t, patch)
	withoutLines := withLines
	withoutLines.Lineup = slices.DeleteFunc(slices.Clone(withLines.Lineup), func(line agenttree.LineupLine) bool {
		return slices.Contains(agentbaseline.JudgmentRoles(), domain.RoleName(line.Key))
	})
	if len(withoutLines.Lineup) == len(withLines.Lineup) {
		t.Fatal("the adopted tree has no judgment lines to remove")
	}
	sites := judgmentTestSites()
	roles := map[domain.RoleName]bool{}
	for _, site := range sites {
		role, _ := domain.RoleForSite(site.ID)
		roles[role] = true
	}
	now := func() time.Time { return time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC) }
	f.withStore(t, func(st *store.Store) {
		// An earlier start with judgments on left the drift auditor's
		// blocking item behind.
		daemoncompose.NewJudgmentRoleHealth(st, now, nil).RoleUnbound(ctx, domain.RoleDriftAuditor, "the lineup has no line for it")
		if len(judgmentRoleItems(t, st)) != 1 {
			t.Fatal("the item of the earlier start is missing")
		}
		for _, tree := range []struct {
			name string
			tree agenttree.Tree
		}{{"with judgment lines", withLines}, {"without judgment lines", withoutLines}} {
			health := daemoncompose.NewJudgmentRoleHealth(st, now, nil)
			client, err := inference.New(inference.Config{
				StatePath: filepath.Join(t.TempDir(), "ledger.json"),
				// No credentials: the off answer comes before they are read.
				Roles: daemoncompose.JudgmentRoles{
					Store: st, Tree: tree.tree, Revision: domain.Digest("sha256:" + strings.Repeat("4", 64)),
					Runtime: daemoncompose.JudgmentRuntime{},
				},
				Health: health, Sites: sites,
				Advisory: advisory.Unavailable(errors.New("unused")), Now: now,
			})
			if err != nil {
				t.Fatal(err)
			}
			checks := client.CheckRoles(ctx)
			if len(checks) != len(roles) {
				t.Fatalf("%s: %d roles checked, want %d", tree.name, len(checks), len(roles))
			}
			for _, check := range checks {
				if !check.Off || check.Err != nil || !roles[check.Role] {
					t.Fatalf("%s: role %s = %+v, want off", tree.name, check.Role, check)
				}
			}
			if err := daemoncompose.CheckJudgmentRolesAtStartup(ctx, client, health, nil); err != nil {
				t.Fatalf("%s: startup check returned %v", tree.name, err)
			}
			if items := judgmentRoleItems(t, st); len(items) != 0 {
				t.Fatalf("%s: items after the startup check = %+v", tree.name, items)
			}
			at := storeRevision(t, st)
			for _, site := range sites {
				fields := map[string]inference.InputField{}
				for _, policy := range site.Fields {
					fields[policy.Name] = inference.InputField{Value: "data", Sensitivity: policy.Sensitivity}
				}
				result, err := client.Call(ctx, site.ID, "project-1", "run-1", fields)
				if err != nil {
					t.Fatalf("%s: %s returned %v", tree.name, site.ID, err)
				}
				if !result.Fallback || result.Reason != inference.FallbackRoleOff ||
					result.Producer != "unavailable/unbound" || string(result.Output) != site.FailSafe ||
					result.Identity != nil {
					t.Fatalf("%s: %s with judgments off = %+v", tree.name, site.ID, result)
				}
			}
			if items := judgmentRoleItems(t, st); len(items) != 0 || storeRevision(t, st) != at {
				t.Fatalf("%s: a call with judgments off wrote to the store: items = %+v", tree.name, items)
			}
		}
	})
}

// callLinesNamingNoModel returns the tree with every judgment line on the
// native-default model id at harness_default, the shape a deployment adopted
// before the call offer ran on, now written as a line's selection.
func callLinesNamingNoModel(t *testing.T, tree agenttree.Tree) agenttree.Tree {
	t.Helper()
	tree.Lineup = slices.Clone(tree.Lineup)
	moved := false
	for i, line := range tree.Lineup {
		if slices.Contains(agentbaseline.JudgmentRoles(), domain.RoleName(line.Key)) {
			names := &tree.Lineup[i].Selection.Names
			names.Model, names.Effort = domain.ClaudeCodeNativeDefaultRouteModelID, string(domain.EffortHarnessDefault)
			moved = true
		}
	}
	if !moved {
		t.Fatal("the adopted lineup has no judgment line")
	}
	return tree
}

// A judgment line on the native-default model id leaves its role unbound:
// the site returns its fail-safe, nothing reaches the driver, and the role's
// item says the line names the native-default model. The drift auditor's
// item blocks unattended admission meanwhile.
func TestJudgmentLineOnTheNativeDefaultModelIsUnbound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAuthAdoptFixture(t)
	_, patch, err := f.run(t, f.args(adoptEscalatedArgs()...))
	if err != nil {
		t.Fatalf("auth adopt: %v", err)
	}
	tree := callLinesNamingNoModel(t, loadAdoptedPatch(t, patch))
	f.withStore(t, func(st *store.Store) {
		now := func() time.Time { return time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC) }
		driver := inferencefake.New()
		sites := judgmentTestSites()
		client, err := inference.New(inference.Config{
			StatePath: filepath.Join(t.TempDir(), "ledger.json"),
			Roles: daemoncompose.JudgmentRoles{
				Store: st, Tree: tree, Revision: domain.Digest("sha256:" + strings.Repeat("4", 64)),
				Runtime: daemoncompose.JudgmentRuntime{Driver: driver}, Credentials: fixedJudgmentCredentials(t),
			},
			Health: daemoncompose.NewJudgmentRoleHealth(st, now, nil), Sites: sites,
			Advisory: advisory.Unavailable(errors.New("unused")), Now: now,
		})
		if err != nil {
			t.Fatal(err)
		}
		// The fixture configures no publication author prompt, so that role
		// is off and every other judgment role is asked.
		asked := map[domain.RoleName]bool{}
		for _, site := range sites {
			role, _ := domain.RoleForSite(site.ID)
			if role == domain.RolePublicationAuthor {
				continue
			}
			asked[role] = true
			fields := map[string]inference.InputField{}
			for _, policy := range site.Fields {
				fields[policy.Name] = inference.InputField{Value: "data", Sensitivity: policy.Sensitivity}
			}
			result, err := client.Call(ctx, site.ID, "project-1", "run-1", fields)
			if err != nil || !result.Fallback || result.Identity != nil || string(result.Output) != site.FailSafe {
				t.Fatalf("%s on the native-default model = %+v, %v", site.ID, result, err)
			}
		}
		if requests := driver.Requests(); len(requests) != 0 {
			t.Fatalf("a line on the native-default model reached the driver: %+v", requests)
		}
		items := judgmentRoleItems(t, st)
		if len(items) != len(asked) {
			t.Fatalf("items = %+v, want one for each of %v", items, asked)
		}
		for role, item := range items {
			if !asked[role] || !strings.Contains(item.Reason, "names the native-default model") {
				t.Fatalf("item for %s = %+v", role, item)
			}
			if blocking := *item.Posture == domain.HealthPostureBlocking; blocking != (role == domain.RoleDriftAuditor) {
				t.Fatalf("item for %s has posture %s", role, *item.Posture)
			}
		}
	})
}

// One daemon admits a line of each model and effort and hands each call its
// own line's model and the effort the harness names for it.
func TestJudgmentLinesLaunchTheirOwnModelAndEffort(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAuthAdoptFixture(t)
	_, patch, err := f.run(t, f.args(adoptEscalatedArgs()...))
	if err != nil {
		t.Fatalf("auth adopt: %v", err)
	}
	tree := loadAdoptedPatch(t, patch)
	const explicitModel = "claude-explicit-1"
	edits := map[domain.RoleName]func(*domain.AgentSelectionNames){
		domain.RoleFindingClassifier: func(n *domain.AgentSelectionNames) {
			n.Effort = string(domain.EffortHarnessDefault)
		},
		domain.RoleFindingAdjudicator: func(n *domain.AgentSelectionNames) { n.Model, n.Effort = explicitModel, "high" },
	}
	tree.Lineup = slices.Clone(tree.Lineup)
	for i, line := range tree.Lineup {
		if edit, ok := edits[domain.RoleName(line.Key)]; ok {
			edit(&tree.Lineup[i].Selection.Names)
		}
	}

	f.withStore(t, func(st *store.Store) {
		now := func() time.Time { return time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC) }
		driver := inferencefake.New()
		sites := judgmentTestSites()
		client, err := inference.New(inference.Config{
			StatePath: filepath.Join(t.TempDir(), "ledger.json"),
			Roles: daemoncompose.JudgmentRoles{
				Store: st, Tree: tree, Revision: domain.Digest("sha256:" + strings.Repeat("4", 64)),
				Runtime: daemoncompose.JudgmentRuntime{Driver: driver}, Credentials: fixedJudgmentCredentials(t),
			},
			Health: daemoncompose.NewJudgmentRoleHealth(st, now, nil), Sites: sites,
			Advisory: advisory.Unavailable(errors.New("unused")), Now: now,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, check := range client.CheckRoles(ctx) {
			// The fixture configures no publication author prompt.
			if check.Off != (check.Role == domain.RolePublicationAuthor) || check.Err != nil {
				t.Fatalf("role %s = %+v", check.Role, check)
			}
		}
		type launch struct{ model, effort string }
		want := map[domain.RoleName]launch{
			domain.RoleFindingClassifier:  {adoptJudgmentModel, ""},
			domain.RoleFindingAdjudicator: {explicitModel, "high"},
			domain.RoleDriftAuditor:       {adoptJudgmentModel, "medium"},
		}
		for _, site := range sites {
			role, _ := domain.RoleForSite(site.ID)
			if _, called := want[role]; !called {
				continue
			}
			fields := map[string]inference.InputField{}
			for _, policy := range site.Fields {
				fields[policy.Name] = inference.InputField{Value: "data", Sensitivity: policy.Sensitivity}
			}
			before := len(driver.Requests())
			result, err := client.Call(ctx, site.ID, "project-1", "run-1", fields)
			if err != nil {
				t.Fatal(err)
			}
			requests := driver.Requests()
			if result.Identity == nil || len(requests) != before+1 {
				t.Fatalf("%s = %+v after %d driver calls", site.ID, result, len(requests)-before)
			}
			if got := (launch{requests[before].Model, requests[before].NativeEffort}); got != want[role] {
				t.Fatalf("%s launched with %+v, want %+v", site.ID, got, want[role])
			}
		}
	})
}

// An item never outlives its reason across a restart: a daemon started after
// a partial fix replaces the item with one that says what is still wrong. In
// one process a changed reason is not rewritten, because some reasons name
// the call's own deadline.
func TestJudgmentRoleItemSaysTheCurrentReasonAfterARestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := func() time.Time { return time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC) }
	const noLine, marked = "the lineup has no line for it", "its generation is marked"
	newAuthAdoptFixture(t).withStore(t, func(st *store.Store) {
		reason := func() string {
			t.Helper()
			items := judgmentRoleItems(t, st)
			item, ok := items[domain.RoleDriftAuditor]
			if len(items) != 1 || !ok || *item.Posture != domain.HealthPostureBlocking {
				t.Fatalf("items = %+v", items)
			}
			return item.Reason
		}
		health := daemoncompose.NewJudgmentRoleHealth(st, now, nil)
		health.RoleUnbound(ctx, domain.RoleDriftAuditor, noLine)
		at := storeRevision(t, st)
		health.RoleUnbound(ctx, domain.RoleDriftAuditor, marked)
		if got := reason(); !strings.Contains(got, noLine) || storeRevision(t, st) != at {
			t.Fatalf("a changed reason in one process rewrote the item: %q", got)
		}

		health = daemoncompose.NewJudgmentRoleHealth(st, now, nil)
		health.RoleUnbound(ctx, domain.RoleDriftAuditor, marked)
		if got := reason(); !strings.Contains(got, marked) || strings.Contains(got, noLine) {
			t.Fatalf("item after a restart with another reason = %q", got)
		}
		at = storeRevision(t, st)
		daemoncompose.NewJudgmentRoleHealth(st, now, nil).RoleUnbound(ctx, domain.RoleDriftAuditor, marked)
		if storeRevision(t, st) != at {
			t.Fatal("a restart with the same reason wrote to the store")
		}
		daemoncompose.NewJudgmentRoleHealth(st, now, nil).RoleResolved(ctx, domain.RoleDriftAuditor)
		if items := judgmentRoleItems(t, st); len(items) != 0 {
			t.Fatalf("items after the role resolved = %+v", items)
		}
	})
}

// The startup check stops the daemon when it cannot leave the store saying
// what it found: started anyway, an unbound drift auditor would have no item
// holding unattended admission. A start cancelled during the check is
// reported as cancelled, never as a role the lineup cannot fill, and writes
// nothing.
func TestJudgmentRoleStartupCheckStopsWhenItsItemsAreNotWritten(t *testing.T) {
	t.Parallel()
	f := newAuthAdoptFixture(t)
	_, patch, err := f.run(t, f.args())
	if err != nil {
		t.Fatalf("auth adopt: %v", err)
	}
	tree := loadAdoptedPatch(t, patch)
	tree.Lineup = slices.DeleteFunc(slices.Clone(tree.Lineup), func(line agenttree.LineupLine) bool {
		return domain.RoleName(line.Key) == domain.RoleDriftAuditor
	})
	now := func() time.Time { return time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC) }
	credentials := fixedJudgmentCredentials(t)
	check := func(ctx context.Context, st *store.Store) error {
		t.Helper()
		health := daemoncompose.NewJudgmentRoleHealth(st, now, nil)
		client, err := inference.New(inference.Config{
			StatePath: filepath.Join(t.TempDir(), "ledger.json"),
			Roles: daemoncompose.JudgmentRoles{
				Store: st, Tree: tree, Revision: domain.Digest("sha256:" + strings.Repeat("4", 64)),
				Runtime: daemoncompose.JudgmentRuntime{Driver: inferencefake.New()}, Credentials: credentials,
			},
			Health: health, Sites: judgmentTestSites(),
			Advisory: advisory.Unavailable(errors.New("unused")), Now: now,
		})
		if err != nil {
			t.Fatal(err)
		}
		return daemoncompose.CheckJudgmentRolesAtStartup(ctx, client, health, nil)
	}
	st, _, err := openStoreWithTopicKey(context.Background(), f.dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	at := storeRevision(t, st)
	if err := check(cancelled, st); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled start returned %v", err)
	}
	if items := judgmentRoleItems(t, st); len(items) != 0 || storeRevision(t, st) != at {
		t.Fatalf("a cancelled start wrote to the store: items = %+v", items)
	}

	if err := check(context.Background(), st); err != nil {
		t.Fatalf("a start that wrote its items returned %v", err)
	}
	items := judgmentRoleItems(t, st)
	if item, ok := items[domain.RoleDriftAuditor]; len(items) != 1 || !ok ||
		*item.Posture != domain.HealthPostureBlocking {
		t.Fatalf("items after a start with the drift auditor unbound = %+v", items)
	}

	// A report that fails is remembered until a later one reaches the store.
	health := daemoncompose.NewJudgmentRoleHealth(st, now, nil)
	health.RoleResolved(cancelled, domain.RoleDriftAuditor)
	if err := health.Unwritten(); !errors.Is(err, context.Canceled) ||
		!strings.Contains(err.Error(), string(domain.RoleDriftAuditor)) {
		t.Fatalf("a failed report left unwritten() = %v", err)
	}
	if len(judgmentRoleItems(t, st)) != 1 {
		t.Fatal("a failed report resolved the item")
	}
	health.RoleResolved(context.Background(), domain.RoleDriftAuditor)
	if err := health.Unwritten(); err != nil || len(judgmentRoleItems(t, st)) != 0 {
		t.Fatalf("a report that reached the store left unwritten() = %v", err)
	}

	// The store fails under a live context: the roles cannot be resolved,
	// their items cannot be written, and the start stops.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	err = check(context.Background(), st)
	if err == nil || errors.Is(err, context.Canceled) ||
		!strings.Contains(err.Error(), string(domain.RoleDriftAuditor)) {
		t.Fatalf("a start that could not write its items returned %v", err)
	}
}

// A writing role's lineage derives from its line's enrollment's client kind
// and its model, so it is known whatever the state of the writer's
// credential, and unknown when the lineup has no line for the role, the
// line's enrollment is not in the store, or the model's lineage is not
// known.
func TestWriterLineageDerivesFromTheLinesEnrollmentAndModel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAuthAdoptFixture(t)
	_, patch, err := f.run(t, f.args())
	if err != nil {
		t.Fatalf("auth adopt: %v", err)
	}
	adopted := loadAdoptedPatch(t, patch)
	edited := func(edit func(*domain.AgentSelectionNames)) agenttree.Tree {
		tree := adopted
		tree.Lineup = slices.Clone(adopted.Lineup)
		for i := range tree.Lineup {
			if domain.RoleName(tree.Lineup[i].Key) == domain.RoleImplementer {
				edit(&tree.Lineup[i].Selection.Names)
			}
		}
		return tree
	}
	f.withStore(t, func(st *store.Store) {
		lineages, err := daemoncompose.WriterLineages(ctx, st, adopted)
		if err != nil {
			t.Fatal(err)
		}
		for _, writer := range domain.WritingRoles {
			if got := lineages[writer]; got != "anthropic" {
				t.Fatalf("%s lineage = %q, want anthropic", writer, got)
			}
		}
		// The writer's identity disabled: the line still wrote on its
		// lineage.
		setIdentityEnabled(t, st, "claude-main", false)
		if lineages, err := daemoncompose.WriterLineages(ctx, st, adopted); err != nil || lineages[domain.RoleImplementer] != "anthropic" {
			t.Fatalf("lineage with the writer disabled = %v, %v", lineages, err)
		}
		unlined := adopted
		unlined.Lineup = slices.DeleteFunc(slices.Clone(adopted.Lineup), func(line agenttree.LineupLine) bool {
			return domain.RoleName(line.Key) == domain.RoleImplementer
		})
		for name, tree := range map[string]agenttree.Tree{
			"no implementer line":   unlined,
			"an absent enrollment":  edited(func(n *domain.AgentSelectionNames) { n.Enrollment = "absent/claude_code" }),
			"a model of no lineage": edited(func(n *domain.AgentSelectionNames) { n.Model = "gpt-5.6-sol" }),
		} {
			lineages, err := daemoncompose.WriterLineages(ctx, st, tree)
			if err != nil {
				t.Fatal(err)
			}
			if got, ok := lineages[domain.RoleImplementer]; !ok || got != "" {
				t.Fatalf("lineage with %s = %q, %v; want unknown", name, got, ok)
			}
		}
	})
}

// A disabled inference ledger raises one blocking system_health item that
// names the cause and nothing the file held; a repeated report with the same
// cause writes nothing, in one process or across a restart; a restart that
// fails for another cause replaces the item; a start that loads the ledger
// resolves it; an exhausted budget, which is the ledger working, opens
// none; and a start that cannot write the item stops.
func TestInferenceLedgerDisabledRaisesOneBlockingItem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := func() time.Time { return time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC) }
	// A key-shaped epoch sits where validation looks and the record id
	// where a decoder would name it, so a cause that quoted either would
	// show in the item.
	const keyShapedEpoch, record = "sk-ant-api03-ledger-epoch", "call-7f3a"
	undecodable := filepath.Join(t.TempDir(), "ledger.json")
	body := `{"version":"freeside.inference-budget/v4","epoch":"` + keyShapedEpoch + `","usage":{},"calls":[{"id":"` + record + `"}]}`
	if err := os.WriteFile(undecodable, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	driver := inferencefake.New()
	roles := inferencefake.Roles{Provider: "fake", Model: "test", Driver: driver}
	newClient := func(path string, calls int64, health *daemoncompose.JudgmentRoleHealth) *inference.Client {
		t.Helper()
		limits := inference.Limits{Calls: calls, ComputeUnits: 10_000_000, AttentionItems: 100, Starvation: 10 * time.Hour}
		client, err := inference.New(inference.Config{
			StatePath: path, Roles: roles, Health: health,
			Sites: daemoncompose.JudgmentSites(inference.Budget{
				Window: 24 * time.Hour, Site: limits, Project: limits, Global: limits,
				MaxCallsPerRoot: 100, MaxStarvationPerRoot: 10 * time.Hour,
			}),
			Advisory: advisory.Unavailable(errors.New("unused")), Now: now,
		})
		if err != nil {
			t.Fatal(err)
		}
		return client
	}
	site := inference.ClassifierSite(inference.Budget{})
	fields := map[string]inference.InputField{}
	for _, policy := range site.Fields {
		fields[policy.Name] = inference.InputField{Value: "data", Sensitivity: policy.Sensitivity}
	}
	call := func(client *inference.Client) inference.CallResult {
		t.Helper()
		result, err := client.Call(ctx, site.ID, "project-1", "run-1", fields)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}

	st, _, err := openStoreWithTopicKey(ctx, newAuthAdoptFixture(t).dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	{
		health := daemoncompose.NewJudgmentRoleHealth(st, now, nil)
		disabled := newClient(undecodable, 100, health)
		if err := daemoncompose.CheckJudgmentRolesAtStartup(ctx, disabled, health, nil); err != nil {
			t.Fatalf("startup check over a disabled ledger: %v", err)
		}
		items := inferenceLedgerItems(t, st)
		if len(items) != 1 {
			t.Fatalf("items over a disabled ledger = %+v", items)
		}
		item := items[0]
		if item.Posture == nil || *item.Posture != domain.HealthPostureBlocking ||
			item.HealthDiagnostic == nil || item.HealthDiagnostic.Code != "inference_ledger_disabled" ||
			item.HealthDiagnostic.Impairs != domain.ImpairedCapabilityUnattendedAdmission ||
			!strings.Contains(item.Reason, "inference ledger is disabled (decode inference ledger)") {
			t.Fatalf("item = %+v", item)
		}
		if strings.Contains(item.Reason, keyShapedEpoch) || strings.Contains(item.Reason, record) {
			t.Fatalf("the item carries what the file held: %q", item.Reason)
		}
		if roleItems := judgmentRoleItems(t, st); len(roleItems) != 0 {
			t.Fatalf("role items with every role admitted = %+v", roleItems)
		}

		// Every later call returns its fail-safe and writes nothing new; nor
		// does a restart that fails for the same cause.
		at := storeRevision(t, st)
		if result := call(disabled); !result.Fallback || result.Reason != inference.FallbackLedgerUnavailable {
			t.Fatalf("call over a disabled ledger = %+v", result)
		}
		if storeRevision(t, st) != at || len(driver.Requests()) != 0 {
			t.Fatal("a call over a disabled ledger wrote to the store or reached the driver")
		}
		health = daemoncompose.NewJudgmentRoleHealth(st, now, nil)
		if err := daemoncompose.CheckJudgmentRolesAtStartup(ctx, newClient(undecodable, 100, health), health, nil); err != nil {
			t.Fatalf("restart over the same disabled ledger: %v", err)
		}
		if storeRevision(t, st) != at || len(inferenceLedgerItems(t, st)) != 1 {
			t.Fatal("a restart with the same cause wrote to the store")
		}

		// A restart that fails for another cause replaces the item: here the
		// file is gone while its anchor remains.
		missing := filepath.Join(t.TempDir(), "ledger.json")
		if err := os.WriteFile(missing+".anchor", []byte("sha256:"+strings.Repeat("5", 64)), 0o600); err != nil {
			t.Fatal(err)
		}
		health = daemoncompose.NewJudgmentRoleHealth(st, now, nil)
		if err := daemoncompose.CheckJudgmentRolesAtStartup(ctx, newClient(missing, 100, health), health, nil); err != nil {
			t.Fatalf("restart over a missing ledger with an anchor: %v", err)
		}
		if items := inferenceLedgerItems(t, st); len(items) != 1 || items[0].ID == item.ID ||
			!strings.Contains(items[0].Reason, "(inference ledger missing after initialization)") {
			t.Fatalf("items after a restart with another cause = %+v", items)
		}

		// A start that loads the ledger resolves the item.
		health = daemoncompose.NewJudgmentRoleHealth(st, now, nil)
		loaded := newClient(filepath.Join(t.TempDir(), "ledger.json"), 1, health)
		if err := daemoncompose.CheckJudgmentRolesAtStartup(ctx, loaded, health, nil); err != nil {
			t.Fatalf("startup check over a loaded ledger: %v", err)
		}
		if items := inferenceLedgerItems(t, st); len(items) != 0 {
			t.Fatalf("items after a start loaded the ledger = %+v", items)
		}

		// An exhausted budget is the ledger working: no item, no write.
		at = storeRevision(t, st)
		if first := call(loaded); first.Reason == inference.FallbackLedgerUnavailable ||
			first.Reason == inference.FallbackBudgetExhausted {
			t.Fatalf("first call under a one-call budget = %+v", first)
		}
		if second := call(loaded); second.Reason != inference.FallbackBudgetExhausted {
			t.Fatalf("second call under a one-call budget = %+v", second)
		}
		if storeRevision(t, st) != at || len(inferenceLedgerItems(t, st)) != 0 {
			t.Fatal("an exhausted budget wrote a ledger item")
		}
	}

	// A start that cannot write the ledger's item stops, naming the ledger.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	health := daemoncompose.NewJudgmentRoleHealth(st, now, nil)
	err = daemoncompose.CheckJudgmentRolesAtStartup(ctx, newClient(undecodable, 100, health), health, nil)
	if err == nil || !strings.Contains(err.Error(), string(daemoncompose.LedgerHealthSubject)) {
		t.Fatalf("a start that could not write the ledger item returned %v", err)
	}
}
