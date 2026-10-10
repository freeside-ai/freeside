package main

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// Each judgment site looks its role up in the lineup at every call (plan
// §5.4, issue #1425). The lineup decides which agent a role is and which
// prompt it runs; the judgment flags still decide what makes the call (the
// pinned CLI and the model it is launched with) and which credential it
// carries. The record says so: credential_source is interim_flag until #1426
// reads the credential from the line's enrollment, and the requested model is
// the agent's while the observed one is whatever the CLI reports (#1619).

// judgmentCallAudit is the hand audit that proves the judgment call launch:
// Claude CLI 2.1.267, audited on 2026-09-09
// (devlog/2026-09-09-2145-subscription-judgments.md). It names the baseline
// call adapter by the digest that adapter had when the audit was recorded.
// Wardless admission compares it to the adapter a line resolves, so the
// record is code-owned and written out here: built from the adapter under
// admission it would prove whatever the lineup named.
var judgmentCallAudit = domain.InterimCallLaunchAudit{
	AdapterDigest: "sha256:3a7d6167dd93ef441a2533456f754b5b4dd643f0a81543f9c232de73f9fa4938",
	HarnessBuild:  "claude-code 2.1.267",
	AuditedOn:     "2026-09-09",
}

// judgmentSites is every judgment site the daemon registers. Preflight checks
// the same list's roles, so a site added here is checked there.
func judgmentSites(budget inference.Budget) []inference.Site {
	return []inference.Site{
		inference.ClassifierSite(budget), inference.AdjudicatorSite(budget),
		inference.DriftAuditorSite(budget),
		inference.DiagnosticSite(budget), inference.DiscussionSite(budget),
		inference.TaskNamerSite(budget),
		inference.PublicationAuthorExplainSite(budget),
		inference.PublicationAuthorProposeSite(budget),
	}
}

// judgmentRuntime is what the judgment flags compose: the driver that makes
// every call, the credential it carries, and the publication author's prompt
// file. The zero value is judgments switched off.
type judgmentRuntime struct {
	Driver       inference.Driver
	Credential   inference.Secret
	AuthorPrompt []byte
}

// judgmentRoles resolves a judgment role through the lineup. It returns what
// it resolved and decides nothing: the inference client runs the admission.
type judgmentRoles struct {
	st       *store.Store
	tree     agenttree.Tree
	revision domain.Digest
	runtime  judgmentRuntime
}

// judgmentRolePrompt returns the prompt this daemon runs a judgment role
// with: the operator's file for the publication author, the code-owned
// identity for every other role with a site. It reports false for the
// publication author with no file configured, which is that role left off.
func judgmentRolePrompt(role domain.RoleName, authorPrompt []byte) (inference.RolePrompt, bool) {
	if role == domain.RolePublicationAuthor {
		if len(authorPrompt) == 0 {
			return inference.RolePrompt{}, false
		}
		return inference.OperatorRolePrompt(role, authorPrompt), true
	}
	return inference.CodeOwnedRolePrompt(role)
}

// ResolveRole implements inference.RoleSource.
func (j judgmentRoles) ResolveRole(ctx context.Context, role domain.RoleName) (inference.RoleCall, error) {
	prompt, ok := judgmentRolePrompt(role, j.runtime.AuthorPrompt)
	if !ok {
		return inference.RoleCall{}, inference.ErrRoleOff
	}
	agent, err := engine.ResolveRole(ctx, j.st, j.tree, role)
	if err != nil {
		return inference.RoleCall{}, err
	}
	audit := judgmentCallAudit
	call := inference.RoleCall{
		Line: agent.Line, LineupRevision: j.revision,
		Agent: agent.Resolved.Definition, Route: agent.Resolved.Route,
		Adapter: agent.Resolved.Adapter, Offer: agent.Resolved.Offer,
		Enrollment: agent.Enrollment, Generation: agent.Generation, ExpiryMargin: agentExpiryMargin,
		LaunchProof: &audit, Prompt: prompt,
		Driver: j.runtime.Driver, Credential: j.runtime.Credential,
		CredentialSource: inference.CredentialSourceInterimFlag,
	}
	if role.JudgesWrittenWork() {
		call.WriterLineage = map[domain.RoleName]string{}
		for _, writer := range domain.WritingRoles {
			call.WriterLineage[writer] = writerLineage(j.tree, writer)
		}
	}
	return call, nil
}

// writerLineage is the lineage group of the offer a writing role's line
// names now, or "" (which the record carries as unknown) when the lineup has
// no line for the role or the line names another agent than the tree holds.
// Lineage is a fact of the offer, so it is read from the tree alone: a writer
// whose enrollment cannot run today still wrote on that lineage.
func writerLineage(tree agenttree.Tree, role domain.RoleName) string {
	lineup, err := tree.ResolveLineup()
	if err != nil {
		return ""
	}
	line, ok := lineup.Line(role)
	if !ok {
		return ""
	}
	if digest, err := tree.AgentDigest(line.AgentName); err != nil || digest != line.AgentDigest {
		return ""
	}
	agent, _ := tree.Agent(line.AgentName)
	offer := slices.IndexFunc(tree.Offers, func(o agenttree.Offer) bool {
		return o.Route == agent.Route && o.Name == agent.Offer
	})
	if offer < 0 {
		return ""
	}
	return tree.Offers[offer].Fragment.LineageGroup
}

const judgmentRoleItemPrefix = "system-health-judgment-role-"

var judgmentRoleItemKind = healthItemKind{Prefix: judgmentRoleItemPrefix, Code: "judgment_role_unbound"}

// judgmentRoleCauseKey keys a role's item by the role and by why it is
// unbound, so an item never outlives its reason: when the reason changes, the
// converge resolves the old item and opens one that says the new reason. No
// role name holds a "-", so the role is the key up to the first one.
func judgmentRoleCauseKey(role domain.RoleName, reason string) string {
	return string(role) + "-" + contentaddr.Hex(contentaddr.Sum([]byte(reason)))[:12]
}

func judgmentRoleOfCause(cause string) domain.RoleName {
	role, _, _ := strings.Cut(cause, "-")
	return domain.RoleName(role)
}

// judgmentRoleCause is the item for one unbound judgment role. The drift
// auditor's blocks: its fail-safe carries the run on as if the audit were
// switched off, and nothing else surfaces the standing fault. Every other
// role's is advisory, because its fail-safe already hands the work to a human
// or a conservative default, or the role only ever advises (plan §5.4).
func judgmentRoleCause(role domain.RoleName, reason string) healthCause {
	cause := healthCause{
		Reason: fmt.Sprintf(
			"Judgment role %s has no admissible lineup line (%s). Its sites return their fail-safe until "+
				"it does: add or fix the %s line in the lineup (auth adopt writes the baseline lines) and "+
				"restart.", role, reason, role),
		Posture: domain.HealthPostureAdvisory, Impairs: domain.ImpairedCapabilityNone,
	}
	if role == domain.RoleDriftAuditor {
		cause.Posture, cause.Impairs = domain.HealthPostureBlocking, domain.ImpairedCapabilityUnattendedAdmission
	}
	return cause
}

// judgmentRoleHealth keeps one open system_health item per unbound judgment
// role. The client reports on every call, so the last state written per role
// is remembered and only a change between unbound and resolved reaches the
// store. A process's first report for a role also replaces an item whose
// reason is no longer the role's: the startup check is that report, so a
// restart after a partial fix says what is still wrong. A reason that changes
// while the role stays unbound is not rewritten before the next start,
// because some reasons name the call's own deadline and would rewrite the
// item on every call. A failed write is logged and retried on the next
// report: a health item never fails a judgment call. Only the startup check
// stops on one (checkJudgmentRolesAtStartup).
type judgmentRoleHealth struct {
	st     *store.Store
	now    func() time.Time
	logger *slog.Logger

	mu      sync.Mutex
	unbound map[domain.RoleName]bool
	// failed holds the write error of each role whose last report the store
	// does not hold.
	failed map[domain.RoleName]error
}

func newJudgmentRoleHealth(st *store.Store, now func() time.Time, logger *slog.Logger) *judgmentRoleHealth {
	return &judgmentRoleHealth{
		st: st, now: now, logger: logger,
		unbound: map[domain.RoleName]bool{}, failed: map[domain.RoleName]error{},
	}
}

// RoleUnbound implements inference.HealthReporter.
func (h *judgmentRoleHealth) RoleUnbound(ctx context.Context, role domain.RoleName, reason string) {
	h.converge(ctx, role, true, reason)
}

// RoleResolved implements inference.HealthReporter.
func (h *judgmentRoleHealth) RoleResolved(ctx context.Context, role domain.RoleName) {
	h.converge(ctx, role, false, "")
}

func (h *judgmentRoleHealth) converge(ctx context.Context, role domain.RoleName, unbound bool, reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if last, known := h.unbound[role]; known && last == unbound {
		// The store holds this report already, whatever a write since tried.
		delete(h.failed, role)
		return
	}
	err := h.write(ctx, role, unbound, reason)
	if err != nil {
		h.failed[role] = err
		if h.logger != nil {
			h.logger.Warn("judgment role health item not converged", "role", role, "error", err)
		}
		return
	}
	delete(h.failed, role)
	h.unbound[role] = unbound
}

// unwritten returns an error naming the roles whose last report did not reach
// the store, with the first role's write error, or nil when every report did.
func (h *judgmentRoleHealth) unwritten() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.failed) == 0 {
		return nil
	}
	roles := slices.Sorted(maps.Keys(h.failed))
	names := make([]string, len(roles))
	for i, role := range roles {
		names[i] = string(role)
	}
	return fmt.Errorf("health item not written for %s: %w", strings.Join(names, ", "), h.failed[roles[0]])
}

// checkJudgmentRolesAtStartup admits every judgment role once, before any
// site is reached, and leaves the store saying what it found: an item for
// each role the lineup cannot fill, and none for a role fixed since the last
// start. The drift auditor's item holds unattended admission, so no later
// call would raise or clear it, and a start that could not write the items
// stops here: started anyway, an unbound drift auditor would have nothing
// holding the work it audits, and a fixed one would stay held. A start
// cancelled during the check is reported as cancelled, because it proves
// nothing about the lineup.
func checkJudgmentRolesAtStartup(
	ctx context.Context, judgments *inference.Client, health *judgmentRoleHealth, logger *slog.Logger,
) error {
	checks := judgments.CheckRoles(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, check := range checks {
		if check.Err != nil && logger != nil {
			logger.Warn("judgment role is unbound; its sites return their fail-safe",
				"role", check.Role, "error", check.Err)
		}
	}
	return health.unwritten()
}

func (h *judgmentRoleHealth) write(ctx context.Context, role domain.RoleName, unbound bool, reason string) error {
	// Every store write advances the revision clients watch, so look first
	// and write only when the item is not already as it should be.
	open, err := h.openCause(ctx, role)
	if err != nil {
		return err
	}
	causes := map[string]healthCause{}
	want := ""
	if unbound {
		want = judgmentRoleCauseKey(role, reason)
		causes[want] = judgmentRoleCause(role, reason)
	}
	if open == want {
		return nil
	}
	// One role is converged at a time; every other role's item is left as it
	// is.
	return convergeHealthItems(ctx, h.st, judgmentRoleItemKind, causes,
		func(cause string) bool { return judgmentRoleOfCause(cause) != role }, h.now())
}

// openCause returns the cause key of the role's open item, or "" when it has
// none.
func (h *judgmentRoleHealth) openCause(ctx context.Context, role domain.RoleName) (string, error) {
	var open string
	err := h.st.Read(ctx, func(tx *store.ReadTx) error {
		items, err := tx.ListOpenAttentionItems(ctx, domain.AttentionSystemHealth)
		if err != nil {
			return err
		}
		for _, item := range items {
			rest, ok := strings.CutPrefix(string(item.ID), judgmentRoleItemPrefix)
			if !ok {
				continue
			}
			// The id ends with the revision that created it.
			cause := rest[:max(strings.LastIndexByte(rest, '-'), 0)]
			if judgmentRoleOfCause(cause) == role {
				open = cause
			}
		}
		return nil
	})
	return open, err
}
