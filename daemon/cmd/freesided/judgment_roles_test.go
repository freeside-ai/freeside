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
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	inferencefake "github.com/freeside-ai/freeside/daemon/internal/inference/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// The audit records are written out, not derived, so this is what notices a
// call adapter changing under its record: a changed adapter is a launch
// nobody audited, and the record must be re-earned, not regenerated. Each
// record is held to its own adapter, so the v2 audit never stands in for v1.
func TestJudgmentCallAuditsNameTheCallAdapters(t *testing.T) {
	v1, err := agentbaseline.ClaudeCallAdapterV1()
	if err != nil {
		t.Fatal(err)
	}
	v2, err := agentbaseline.ClaudeCallAdapter()
	if err != nil {
		t.Fatal(err)
	}
	audited := []struct {
		adapter domain.AdapterFragment
		on      string
	}{{v1, "2026-09-09"}, {v2, "2026-10-11"}}
	if len(judgmentCallAudits) != len(audited) {
		t.Fatalf("%d audit records for %d call adapters", len(judgmentCallAudits), len(audited))
	}
	for i, want := range audited {
		audit, adapter := judgmentCallAudits[i], want.adapter
		if err := audit.Validate(); err != nil {
			t.Fatal(err)
		}
		if audit.AdapterDigest != adapter.Digest || audit.HarnessBuild != adapter.HarnessBuild || audit.AuditedOn != want.on {
			t.Fatalf("audit covers %s %q on %s, the call adapter %s is %s %q, audited %s",
				audit.AdapterDigest, audit.HarnessBuild, audit.AuditedOn,
				adapter.AdapterBuild, adapter.Digest, adapter.HarnessBuild, want.on)
		}
		if got := judgmentCallAuditFor(adapter.Digest); got == nil || *got != audit {
			t.Fatalf("audit for %s = %+v, want %+v", adapter.AdapterBuild, got, audit)
		}
	}
	if got := judgmentCallAuditFor(domain.Digest("sha256:" + strings.Repeat("0", 64))); got != nil {
		t.Fatalf("audit for an adapter nobody audited = %+v", got)
	}
}

func judgmentTestSites() []inference.Site {
	limits := inference.Limits{Calls: 100, ComputeUnits: 10_000_000, AttentionItems: 100, Starvation: 10 * time.Hour}
	budget := inference.Budget{
		Window: 24 * time.Hour, Site: limits, Project: limits, Global: limits,
		MaxCallsPerRoot: 100, MaxStarvationPerRoot: 10 * time.Hour,
	}
	return judgmentSites(budget)
}

func judgmentRoleItems(t *testing.T, st *store.Store) map[domain.RoleName]domain.AttentionItem {
	t.Helper()
	items := map[domain.RoleName]domain.AttentionItem{}
	if err := st.Read(context.Background(), func(tx *store.ReadTx) error {
		open, err := tx.ListOpenAttentionItems(context.Background(), domain.AttentionSystemHealth)
		for _, item := range open {
			rest, ok := strings.CutPrefix(string(item.ID), judgmentRoleItemPrefix)
			if !ok {
				continue
			}
			role := judgmentRoleOfCause(rest)
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
			if strings.HasPrefix(string(item.ID), inferenceLedgerItemPrefix) {
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
		runtime := judgmentRuntime{Driver: driver, AuthorPrompt: authorBody}
		credentials := fixedJudgmentCredentials(t)
		health := newJudgmentRoleHealth(st, now, nil)
		newClient := func(tree agenttree.Tree, runtime judgmentRuntime, health inference.HealthReporter) *inference.Client {
			t.Helper()
			client, err := inference.New(inference.Config{
				StatePath: filepath.Join(t.TempDir(), "ledger.json"),
				Roles: judgmentRoles{
					st: st, tree: tree, revision: revision, runtime: runtime, credentials: credentials,
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
		for _, request := range requests {
			role, _ := domain.RoleForSite(request.SiteID)
			want := ""
			if role == domain.RolePublicationAuthor {
				want = string(authorBody)
			}
			if string(request.RolePrompt) != want {
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
		health = newJudgmentRoleHealth(st, now, nil)
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

		// The publication author with no prompt file is off: both sites fail
		// safe as inference being down, and nothing is raised.
		authorSites := []inference.Site{sites[6], sites[7]}
		off := newClient(adopted, judgmentRuntime{Driver: driver}, health)
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
		source, ok := adopted.Agent(agentbaseline.ClaudeCallAgentName)
		if !ok {
			t.Fatal("the adopted tree has no call agent")
		}
		if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
			generation, err := tx.CurrentEnrollmentGeneration(ctx, domain.ClientEnrollmentID(source.Enrollment))
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
		newJudgmentRoleHealth(st, now, nil).RoleUnbound(ctx, domain.RoleDriftAuditor, "the lineup has no line for it")
		if len(judgmentRoleItems(t, st)) != 1 {
			t.Fatal("the item of the earlier start is missing")
		}
		for _, tree := range []struct {
			name string
			tree agenttree.Tree
		}{{"with judgment lines", withLines}, {"without judgment lines", withoutLines}} {
			health := newJudgmentRoleHealth(st, now, nil)
			client, err := inference.New(inference.Config{
				StatePath: filepath.Join(t.TempDir(), "ledger.json"),
				// No credentials: the off answer comes before they are read.
				Roles: judgmentRoles{
					st: st, tree: tree.tree, revision: domain.Digest("sha256:" + strings.Repeat("4", 64)),
					runtime: judgmentRuntime{},
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
			if err := checkJudgmentRolesAtStartup(ctx, client, health, nil); err != nil {
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

// adoptedBeforeTheCallOffer returns the tree as a deployment adopted before
// the call offer existed holds it: the call agent on the native-default
// offer, which names no model, and every judgment line naming that agent.
// The baseline no longer emits that offer, so it is put back here, shaped
// as adoption once wrote it.
func adoptedBeforeTheCallOffer(t *testing.T, tree agenttree.Tree) agenttree.Tree {
	t.Helper()
	tree.Agents, tree.Offers = slices.Clone(tree.Agents), slices.Clone(tree.Offers)
	for i, agent := range tree.Agents {
		if agent.Name == agentbaseline.ClaudeCallAgentName {
			tree.Agents[i].Offer, tree.Agents[i].Effort = agentbaseline.ClaudeOfferName, domain.EffortHarnessDefault
		}
	}
	call := slices.IndexFunc(tree.Offers, func(o agenttree.Offer) bool {
		return o.Name == agentbaseline.ClaudeCallOfferName
	})
	if call < 0 {
		t.Fatal("the adopted tree has no call offer")
	}
	native := tree.Offers[call]
	native.Name = agentbaseline.ClaudeOfferName
	native.Fragment.RouteModelID = domain.ClaudeCodeNativeDefaultRouteModelID
	native.Fragment.AllowedEfforts = []domain.EffortLevel{domain.EffortHarnessDefault}
	var err error
	if native.Fragment.Digest, err = native.Fragment.ComputeDigest(); err != nil {
		t.Fatal(err)
	}
	tree.Offers = append(tree.Offers, native)
	tree.Sort()
	digest, err := tree.AgentDigest(agentbaseline.ClaudeCallAgentName)
	if err != nil {
		t.Fatal(err)
	}
	tree.Lineup = slices.Clone(tree.Lineup)
	for i, line := range tree.Lineup {
		if line.Selection.AgentName == agentbaseline.ClaudeCallAgentName {
			tree.Lineup[i].Selection.AgentDigest = digest
		}
	}
	if _, err := agenttree.Render(tree); err != nil {
		t.Fatalf("the earlier tree does not render: %v", err)
	}
	return tree
}

// A judgment line whose offer names no model leaves its role unbound: the
// site returns its fail-safe, nothing reaches the driver, and the role's item
// says the offer names no model. That is every judgment role of a deployment
// adopted before the call offer, until the operator applies a fresh patch,
// and the drift auditor's item blocks unattended admission meanwhile.
func TestJudgmentLineOnAnOfferThatNamesNoModelIsUnbound(t *testing.T) {
	ctx := context.Background()
	f := newAuthAdoptFixture(t)
	_, patch, err := f.run(t, f.args())
	if err != nil {
		t.Fatalf("auth adopt: %v", err)
	}
	tree := adoptedBeforeTheCallOffer(t, loadAdoptedPatch(t, patch))
	f.withStore(t, func(st *store.Store) {
		now := func() time.Time { return time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC) }
		driver := inferencefake.New()
		sites := judgmentTestSites()
		client, err := inference.New(inference.Config{
			StatePath: filepath.Join(t.TempDir(), "ledger.json"),
			Roles: judgmentRoles{
				st: st, tree: tree, revision: domain.Digest("sha256:" + strings.Repeat("4", 64)),
				runtime: judgmentRuntime{Driver: driver}, credentials: fixedJudgmentCredentials(t),
			},
			Health: newJudgmentRoleHealth(st, now, nil), Sites: sites,
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
				t.Fatalf("%s on an offer that names no model = %+v, %v", site.ID, result, err)
			}
		}
		if requests := driver.Requests(); len(requests) != 0 {
			t.Fatalf("a line on an offer that names no model reached the driver: %+v", requests)
		}
		items := judgmentRoleItems(t, st)
		if len(items) != len(asked) {
			t.Fatalf("items = %+v, want one for each of %v", items, asked)
		}
		for role, item := range items {
			if !asked[role] || !strings.Contains(item.Reason, "offer names no model") {
				t.Fatalf("item for %s = %+v", role, item)
			}
			if blocking := *item.Posture == domain.HealthPostureBlocking; blocking != (role == domain.RoleDriftAuditor) {
				t.Fatalf("item for %s has posture %s", role, *item.Posture)
			}
		}
	})
}

// One daemon admits a line on each audited call adapter and hands each call
// its own agent's model and effort. A line on the earlier adapter keeps
// admitting under the earlier audit; a call adapter no record names is
// refused for want of a launch proof, whatever else about it matches.
func TestJudgmentLinesAdmitOnEachAuditedCallAdapter(t *testing.T) {
	ctx := context.Background()
	f := newAuthAdoptFixture(t)
	_, patch, err := f.run(t, f.args())
	if err != nil {
		t.Fatalf("auth adopt: %v", err)
	}
	tree := loadAdoptedPatch(t, patch)
	baseline, ok := tree.Agent(agentbaseline.ClaudeCallAgentName)
	if !ok {
		t.Fatal("the adopted tree has no call agent")
	}
	v1, err := agentbaseline.ClaudeCallAdapterV1()
	if err != nil {
		t.Fatal(err)
	}
	// The v2 launch under a build id no audit covers.
	unaudited, err := agentbaseline.ClaudeCallAdapter()
	if err != nil {
		t.Fatal(err)
	}
	unaudited.AdapterBuild = "claude_code_call_v3"
	if unaudited.Digest, err = unaudited.ComputeDigest(); err != nil {
		t.Fatal(err)
	}
	const explicitModel, explicitOffer = "claude-explicit-1", "explicit-high"
	var explicit domain.OfferFragment
	for _, offer := range tree.Offers {
		if offer.Route == baseline.Route && offer.Name == baseline.Offer {
			explicit = offer.Fragment
		}
	}
	explicit.RouteModelID, explicit.AllowedEfforts = explicitModel, []domain.EffortLevel{domain.EffortHigh}
	if explicit.Digest, err = explicit.ComputeDigest(); err != nil {
		t.Fatal(err)
	}
	tree.Adapters = append(slices.Clone(tree.Adapters),
		agenttree.Adapter{Name: agentbaseline.ClaudeCallAdapterV1Build, Fragment: v1},
		agenttree.Adapter{Name: unaudited.AdapterBuild, Fragment: unaudited})
	// The v1 adapter sends harness_default only, which the adopted call
	// offer no longer allows, so the v1 and unaudited agents sit on an
	// offer of the adopted model that allows it.
	const harnessOffer = "call-harness-default"
	harness := explicit
	harness.RouteModelID, harness.AllowedEfforts = adoptJudgmentModel, []domain.EffortLevel{domain.EffortHarnessDefault}
	if harness.Digest, err = harness.ComputeDigest(); err != nil {
		t.Fatal(err)
	}
	tree.Offers = append(slices.Clone(tree.Offers),
		agenttree.Offer{Route: baseline.Route, Name: explicitOffer, Fragment: explicit},
		agenttree.Offer{Route: baseline.Route, Name: harnessOffer, Fragment: harness})
	agent := func(name, adapter, offer string, effort domain.EffortLevel) domain.AgentSource {
		source := baseline
		source.Name, source.Adapter, source.Offer, source.Effort = name, adapter, offer, effort
		return source
	}
	const onV1, onV2, onUnaudited = "call-on-v1", "call-explicit-high", "call-unaudited"
	tree.Agents = append(slices.Clone(tree.Agents),
		agent(onV1, agentbaseline.ClaudeCallAdapterV1Build, harnessOffer, domain.EffortHarnessDefault),
		agent(onV2, agentbaseline.ClaudeCallAdapterName, explicitOffer, domain.EffortHigh),
		agent(onUnaudited, unaudited.AdapterBuild, harnessOffer, domain.EffortHarnessDefault))
	agents := map[domain.RoleName]string{
		domain.RoleFindingClassifier: onV1, domain.RoleFindingAdjudicator: onV2, domain.RoleDiagnostic: onUnaudited,
	}
	tree.Lineup = slices.Clone(tree.Lineup)
	for i, line := range tree.Lineup {
		name, moved := agents[domain.RoleName(line.Key)]
		if !moved {
			continue
		}
		digest, err := tree.AgentDigest(name)
		if err != nil {
			t.Fatal(err)
		}
		tree.Lineup[i].Selection.AgentName, tree.Lineup[i].Selection.AgentDigest = name, digest
	}
	tree.Sort()
	if _, err := agenttree.Render(tree); err != nil {
		t.Fatalf("the edited tree does not render: %v", err)
	}

	f.withStore(t, func(st *store.Store) {
		now := func() time.Time { return time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC) }
		driver := inferencefake.New()
		sites := judgmentTestSites()
		client, err := inference.New(inference.Config{
			StatePath: filepath.Join(t.TempDir(), "ledger.json"),
			Roles: judgmentRoles{
				st: st, tree: tree, revision: domain.Digest("sha256:" + strings.Repeat("4", 64)),
				runtime: judgmentRuntime{Driver: driver}, credentials: fixedJudgmentCredentials(t),
			},
			Health: newJudgmentRoleHealth(st, now, nil), Sites: sites,
			Advisory: advisory.Unavailable(errors.New("unused")), Now: now,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, check := range client.CheckRoles(ctx) {
			unbound := check.Role == domain.RoleDiagnostic
			// The fixture configures no publication author prompt.
			if check.Off != (check.Role == domain.RolePublicationAuthor) || (check.Err != nil) != unbound {
				t.Fatalf("role %s = %+v", check.Role, check)
			}
			if unbound && !strings.Contains(check.Err.Error(), "launch proof") {
				t.Fatalf("role on an unaudited call adapter was refused for another reason: %v", check.Err)
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
			if _, called := want[role]; !called && role != domain.RoleDiagnostic {
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
			if role == domain.RoleDiagnostic {
				if !result.Fallback || result.Identity != nil || len(requests) != before {
					t.Fatalf("%s on an unaudited call adapter = %+v", site.ID, result)
				}
				continue
			}
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
		health := newJudgmentRoleHealth(st, now, nil)
		health.RoleUnbound(ctx, domain.RoleDriftAuditor, noLine)
		at := storeRevision(t, st)
		health.RoleUnbound(ctx, domain.RoleDriftAuditor, marked)
		if got := reason(); !strings.Contains(got, noLine) || storeRevision(t, st) != at {
			t.Fatalf("a changed reason in one process rewrote the item: %q", got)
		}

		health = newJudgmentRoleHealth(st, now, nil)
		health.RoleUnbound(ctx, domain.RoleDriftAuditor, marked)
		if got := reason(); !strings.Contains(got, marked) || strings.Contains(got, noLine) {
			t.Fatalf("item after a restart with another reason = %q", got)
		}
		at = storeRevision(t, st)
		newJudgmentRoleHealth(st, now, nil).RoleUnbound(ctx, domain.RoleDriftAuditor, marked)
		if storeRevision(t, st) != at {
			t.Fatal("a restart with the same reason wrote to the store")
		}
		newJudgmentRoleHealth(st, now, nil).RoleResolved(ctx, domain.RoleDriftAuditor)
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
		health := newJudgmentRoleHealth(st, now, nil)
		client, err := inference.New(inference.Config{
			StatePath: filepath.Join(t.TempDir(), "ledger.json"),
			Roles: judgmentRoles{
				st: st, tree: tree, revision: domain.Digest("sha256:" + strings.Repeat("4", 64)),
				runtime: judgmentRuntime{Driver: inferencefake.New()}, credentials: credentials,
			},
			Health: health, Sites: judgmentTestSites(),
			Advisory: advisory.Unavailable(errors.New("unused")), Now: now,
		})
		if err != nil {
			t.Fatal(err)
		}
		return checkJudgmentRolesAtStartup(ctx, client, health, nil)
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
	health := newJudgmentRoleHealth(st, now, nil)
	health.RoleResolved(cancelled, domain.RoleDriftAuditor)
	if err := health.unwritten(); !errors.Is(err, context.Canceled) ||
		!strings.Contains(err.Error(), string(domain.RoleDriftAuditor)) {
		t.Fatalf("a failed report left unwritten() = %v", err)
	}
	if len(judgmentRoleItems(t, st)) != 1 {
		t.Fatal("a failed report resolved the item")
	}
	health.RoleResolved(context.Background(), domain.RoleDriftAuditor)
	if err := health.unwritten(); err != nil || len(judgmentRoleItems(t, st)) != 0 {
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

// A writing role's lineage is read from the tree alone, so it is known
// whatever the state of the writer's enrollment, and unknown only when the
// lineup does not name an agent the tree holds.
func TestWriterLineageReadsTheTreeAlone(t *testing.T) {
	f := newAuthAdoptFixture(t)
	_, patch, err := f.run(t, f.args())
	if err != nil {
		t.Fatalf("auth adopt: %v", err)
	}
	adopted := loadAdoptedPatch(t, patch)
	want := ""
	for _, offer := range adopted.Offers {
		if offer.Name == agentbaseline.WriterAgentName(domain.RoleImplementer) {
			want = offer.Fragment.LineageGroup
		}
	}
	for _, writer := range domain.WritingRoles {
		if got := writerLineage(adopted, writer); got == "" || got != want {
			t.Fatalf("%s lineage = %q, want %q", writer, got, want)
		}
	}
	unlined := adopted
	unlined.Lineup = slices.DeleteFunc(slices.Clone(adopted.Lineup), func(line agenttree.LineupLine) bool {
		return domain.RoleName(line.Key) == domain.RoleImplementer
	})
	if got := writerLineage(unlined, domain.RoleImplementer); got != "" {
		t.Fatalf("lineage with no implementer line = %q", got)
	}
	other := adopted
	other.Lineup = slices.Clone(adopted.Lineup)
	for i := range other.Lineup {
		if domain.RoleName(other.Lineup[i].Key) == domain.RoleImplementer {
			other.Lineup[i].Selection.AgentDigest = domain.Digest("sha256:" + strings.Repeat("0", 64))
		}
	}
	if got := writerLineage(other, domain.RoleImplementer); got != "" {
		t.Fatalf("lineage with a line naming another agent digest = %q", got)
	}
}

// A disabled inference ledger raises one blocking system_health item that
// names the cause and nothing the file held; a repeated report with the same
// cause writes nothing, in one process or across a restart; a restart that
// fails for another cause replaces the item; a start that loads the ledger
// resolves it; an exhausted budget, which is the ledger working, opens
// none; and a start that cannot write the item stops.
func TestInferenceLedgerDisabledRaisesOneBlockingItem(t *testing.T) {
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
	newClient := func(path string, calls int64, health *judgmentRoleHealth) *inference.Client {
		t.Helper()
		limits := inference.Limits{Calls: calls, ComputeUnits: 10_000_000, AttentionItems: 100, Starvation: 10 * time.Hour}
		client, err := inference.New(inference.Config{
			StatePath: path, Roles: roles, Health: health,
			Sites: judgmentSites(inference.Budget{
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
		health := newJudgmentRoleHealth(st, now, nil)
		disabled := newClient(undecodable, 100, health)
		if err := checkJudgmentRolesAtStartup(ctx, disabled, health, nil); err != nil {
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
		health = newJudgmentRoleHealth(st, now, nil)
		if err := checkJudgmentRolesAtStartup(ctx, newClient(undecodable, 100, health), health, nil); err != nil {
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
		health = newJudgmentRoleHealth(st, now, nil)
		if err := checkJudgmentRolesAtStartup(ctx, newClient(missing, 100, health), health, nil); err != nil {
			t.Fatalf("restart over a missing ledger with an anchor: %v", err)
		}
		if items := inferenceLedgerItems(t, st); len(items) != 1 || items[0].ID == item.ID ||
			!strings.Contains(items[0].Reason, "(inference ledger missing after initialization)") {
			t.Fatalf("items after a restart with another cause = %+v", items)
		}

		// A start that loads the ledger resolves the item.
		health = newJudgmentRoleHealth(st, now, nil)
		loaded := newClient(filepath.Join(t.TempDir(), "ledger.json"), 1, health)
		if err := checkJudgmentRolesAtStartup(ctx, loaded, health, nil); err != nil {
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
	health := newJudgmentRoleHealth(st, now, nil)
	err = checkJudgmentRolesAtStartup(ctx, newClient(undecodable, 100, health), health, nil)
	if err == nil || !strings.Contains(err.Error(), string(ledgerHealthSubject)) {
		t.Fatalf("a start that could not write the ledger item returned %v", err)
	}
}
