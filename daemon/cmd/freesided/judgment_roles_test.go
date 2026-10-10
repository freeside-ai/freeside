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

// The audit record is written out, not derived, so this is what notices the
// baseline call adapter changing under it: a changed adapter is a launch
// nobody audited, and the record must be re-earned, not regenerated.
func TestJudgmentCallAuditNamesTheBaselineCallAdapter(t *testing.T) {
	adapter, err := agentbaseline.ClaudeCallAdapter()
	if err != nil {
		t.Fatal(err)
	}
	if err := judgmentCallAudit.Validate(); err != nil {
		t.Fatal(err)
	}
	if judgmentCallAudit.AdapterDigest != adapter.Digest || judgmentCallAudit.HarnessBuild != adapter.HarnessBuild {
		t.Fatalf("audit covers %s %q, the baseline call adapter is %s %q",
			judgmentCallAudit.AdapterDigest, judgmentCallAudit.HarnessBuild, adapter.Digest, adapter.HarnessBuild)
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
	const unbound = "judgment role has no admissible lineup line"

	f.withStore(t, func(st *store.Store) {
		now := func() time.Time { return time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC) }
		driver := inferencefake.New()
		runtime := judgmentRuntime{Driver: driver, Credential: "synthetic-token", AuthorPrompt: authorBody}
		health := newJudgmentRoleHealth(st, now, nil)
		newClient := func(tree agenttree.Tree, runtime judgmentRuntime, health inference.HealthReporter) *inference.Client {
			t.Helper()
			client, err := inference.New(inference.Config{
				StatePath: filepath.Join(t.TempDir(), "ledger.json"),
				Roles:     judgmentRoles{st: st, tree: tree, revision: revision, runtime: runtime},
				Health:    health, Sites: sites,
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
		off := newClient(adopted, judgmentRuntime{Driver: driver, Credential: runtime.Credential}, health)
		for _, site := range authorSites {
			if result := call(off, site); !result.Fallback || result.Reason != inference.ErrUnavailable.Error() {
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
	const credential = "synthetic-token"
	check := func(ctx context.Context, st *store.Store) error {
		t.Helper()
		health := newJudgmentRoleHealth(st, now, nil)
		client, err := inference.New(inference.Config{
			StatePath: filepath.Join(t.TempDir(), "ledger.json"),
			Roles: judgmentRoles{
				st: st, tree: tree, revision: domain.Digest("sha256:" + strings.Repeat("4", 64)),
				runtime: judgmentRuntime{Driver: inferencefake.New(), Credential: credential},
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
	if strings.Contains(err.Error(), credential) {
		t.Fatalf("the startup error carries the credential: %v", err)
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
		if offer.Name == agentbaseline.ClaudeOfferName {
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
