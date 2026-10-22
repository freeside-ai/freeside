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
// §5.4, issue #1425). The lineup decides which agent a role is, which prompt
// it runs, and the model and effort its call launches with, and the call
// authenticates with the token stored in the credential volume of the line's
// enrollment generation (#1426), which the record names as credential_source
// enrollment. The judgment flags decide only the pinned CLI and the
// publication-author prompt file; no flag names a model.

// judgmentCallAudits are the hand audits that prove the judgment call launch,
// one per call adapter, each naming its adapter by the digest that adapter
// had when the audit was recorded. Both ran against Claude CLI 2.1.267.
//
//   - claude_code_call_v1, audited 2026-09-09: the launch with no effort
//     (devlog/2026-09-09-2145-subscription-judgments.md).
//   - claude_code_call_v2, audited 2026-10-11: the same launch with
//     no effort and with each effort the adapter can send
//     (devlog/2026-10-11-0927-judgment-call-model-effort.md).
//
// Wardless admission compares a record to the adapter a line resolves, so
// the records are code-owned and written out here: built from the adapter
// under admission, a record would prove whatever the lineup named.
var judgmentCallAudits = []domain.InterimCallLaunchAudit{{
	AdapterDigest: "sha256:3a7d6167dd93ef441a2533456f754b5b4dd643f0a81543f9c232de73f9fa4938",
	HarnessBuild:  "claude-code 2.1.267",
	AuditedOn:     "2026-09-09",
}, {
	AdapterDigest: "sha256:b6b0ddc8d05d55d198a72b7a15824c943b702cf7a50a855b040c2436bab55a5b",
	HarnessBuild:  "claude-code 2.1.267",
	AuditedOn:     "2026-10-11",
}}

// judgmentCallAuditFor returns the audit record that names the adapter, or
// nil. It picks a record and decides nothing: admission refuses a call with
// no proof, and compares the digest and the harness build again itself.
func judgmentCallAuditFor(adapter domain.Digest) *domain.InterimCallLaunchAudit {
	for _, audit := range judgmentCallAudits {
		if audit.AdapterDigest == adapter {
			return &audit
		}
	}
	return nil
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
// every call and the publication author's prompt file. The zero value is
// judgments switched off: with no driver, every role is off.
type judgmentRuntime struct {
	Driver       inference.Driver
	AuthorPrompt []byte
}

// judgmentRoles resolves a judgment role through the lineup. It returns what
// it resolved and decides nothing: the inference client runs the admission.
type judgmentRoles struct {
	st       *store.Store
	tree     agenttree.Tree
	revision domain.Digest
	runtime  judgmentRuntime
	// credentials reads the token a call authenticates with. A role whose
	// token cannot be read is unbound, like a role with no line.
	credentials *judgmentCredentials
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

// ResolveRole implements inference.RoleSource. With the judgment flags absent
// the runtime has no driver and every role is off: the answer is
// inference.ErrRoleOff before the lineup, the store, or a credential is read,
// so a tree with no judgment lines leaves no role unbound and raises no item
// on a deployment that never asked for judgments (#1945). Policy asks for a
// role's line only while it asks for the role's work (plan §5.4).
func (j judgmentRoles) ResolveRole(ctx context.Context, role domain.RoleName) (inference.RoleCall, error) {
	if j.runtime.Driver == nil {
		return inference.RoleCall{}, inference.ErrRoleOff
	}
	prompt, ok := judgmentRolePrompt(role, j.runtime.AuthorPrompt)
	if !ok {
		return inference.RoleCall{}, inference.ErrRoleOff
	}
	agent, err := engine.ResolveRole(ctx, j.st, j.tree, role)
	if err != nil {
		return inference.RoleCall{}, err
	}
	credential, err := j.credentials.token(ctx, agent.Enrollment.ID, agent.Generation.Ordinal)
	if err != nil {
		return inference.RoleCall{}, fmt.Errorf("role %s: %w", role, err)
	}
	call := inference.RoleCall{
		Line: agent.Line, LineupRevision: j.revision,
		Agent: agent.Resolved.Definition, Route: agent.Resolved.Route,
		Adapter: agent.Resolved.Adapter, Offer: agent.Resolved.Offer,
		Enrollment: agent.Enrollment, Generation: agent.Generation, ExpiryMargin: agentExpiryMargin,
		LaunchProof: judgmentCallAuditFor(agent.Resolved.Adapter.Digest), Prompt: prompt,
		Driver: j.runtime.Driver, Credential: credential,
		CredentialSource: inference.CredentialSourceEnrollment,
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
				"it does. For a missing or inadmissible line, add or fix the %s line in the lineup (auth "+
				"adopt -judgment-model writes the baseline lines) and restart. For a stored credential that could not be "+
				"read, restore the credential volume of the line's enrollment; the role binds at its next "+
				"call, or at a restart when nothing calls it.",
			role, reason, role),
		Posture: domain.HealthPostureAdvisory, Impairs: domain.ImpairedCapabilityNone,
	}
	if role == domain.RoleDriftAuditor {
		cause.Posture, cause.Impairs = domain.HealthPostureBlocking, domain.ImpairedCapabilityUnattendedAdmission
	}
	return cause
}

const inferenceLedgerItemPrefix = "system-health-inference-ledger-"

var inferenceLedgerItemKind = healthItemKind{Prefix: inferenceLedgerItemPrefix, Code: "inference_ledger_disabled"}

// inferenceLedgerCauseKey keys the ledger's item by why it is disabled, as a
// role's item is keyed by its reason, so a restart that fails for another
// reason replaces the item instead of keeping the old cause.
func inferenceLedgerCauseKey(cause string) string {
	return contentaddr.Hex(contentaddr.Sum([]byte(cause)))[:12]
}

// inferenceLedgerCause is the item for a disabled inference ledger (#1943).
// It blocks for the reason the drift auditor's item does: with the ledger
// disabled every judgment site returns its fail-safe, the drift auditor's
// among them, so each run carries on as if the audit were switched off and
// nothing else surfaces the standing fault (plan §5.4). The cause names the
// step that failed and the system error, never a path, a record, or a
// credential: the ledger holds no credential, and a file that fails to
// decode is reported without the decoder's text. The daemon never repairs
// the file (#1943, non-goals); the item says what the operator can do.
func inferenceLedgerCause(cause string) healthCause {
	return healthCause{
		Reason: fmt.Sprintf(
			"The inference ledger is disabled (%s). Every judgment site returns its fail-safe until a start "+
				"loads it. The daemon leaves the file as it found it. For a file that cannot be decoded or "+
				"anchored, restore inference-budget.json in the daemon state directory and its anchor beside "+
				"the database, or remove both to start a new ledger, which discards the recorded budget usage "+
				"and audit debt. For a directory or file that cannot be created, read, or written, fix its "+
				"permissions or free the space. Then restart.", cause),
		Posture: domain.HealthPostureBlocking, Impairs: domain.ImpairedCapabilityUnattendedAdmission,
	}
}

// healthSubject is what one report to judgmentRoleHealth is about: a
// judgment role, named by its role name, or the inference ledger.
type healthSubject string

const ledgerHealthSubject healthSubject = "inference ledger"

// judgmentRoleHealth keeps one open system_health item per unbound judgment
// role and one for a disabled inference ledger. The client reports on every
// call, so the last state written per subject is remembered and only a
// change between faulted and resolved reaches the store. A process's first
// report for a subject also replaces an item whose reason is no longer the
// subject's: the startup check is that report, so a restart after a partial
// fix says what is still wrong. A reason that changes while the subject
// stays faulted is not rewritten before the next start, because some role
// reasons name the call's own deadline and would rewrite the item on every
// call. A failed write is logged and retried on the next report: a health
// item never fails a judgment call. Only the startup check stops on one
// (checkJudgmentRolesAtStartup).
type judgmentRoleHealth struct {
	st     *store.Store
	now    func() time.Time
	logger *slog.Logger

	mu sync.Mutex
	// faulted holds, per subject, what its last report to reach the store
	// said: true for a role unbound or the ledger disabled.
	faulted map[healthSubject]bool
	// failed holds the write error of each subject whose last report the
	// store does not hold.
	failed map[healthSubject]error
}

func newJudgmentRoleHealth(st *store.Store, now func() time.Time, logger *slog.Logger) *judgmentRoleHealth {
	return &judgmentRoleHealth{
		st: st, now: now, logger: logger,
		faulted: map[healthSubject]bool{}, failed: map[healthSubject]error{},
	}
}

// RoleUnbound implements inference.HealthReporter.
func (h *judgmentRoleHealth) RoleUnbound(ctx context.Context, role domain.RoleName, reason string) {
	h.converge(ctx, healthSubject(role), true, func() error { return h.writeRole(ctx, role, true, reason) })
}

// RoleResolved implements inference.HealthReporter.
func (h *judgmentRoleHealth) RoleResolved(ctx context.Context, role domain.RoleName) {
	h.converge(ctx, healthSubject(role), false, func() error { return h.writeRole(ctx, role, false, "") })
}

// LedgerDisabled implements inference.HealthReporter.
func (h *judgmentRoleHealth) LedgerDisabled(ctx context.Context, cause string) {
	h.converge(ctx, ledgerHealthSubject, true, func() error { return h.writeLedger(ctx, true, cause) })
}

// LedgerLoaded implements inference.HealthReporter.
func (h *judgmentRoleHealth) LedgerLoaded(ctx context.Context) {
	h.converge(ctx, ledgerHealthSubject, false, func() error { return h.writeLedger(ctx, false, "") })
}

func (h *judgmentRoleHealth) converge(ctx context.Context, subject healthSubject, faulted bool, write func() error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if last, known := h.faulted[subject]; known && last == faulted {
		// The store holds this report already, whatever a write since tried.
		delete(h.failed, subject)
		return
	}
	if err := write(); err != nil {
		h.failed[subject] = err
		if h.logger != nil {
			h.logger.Warn("judgment health item not converged", "subject", subject, "error", err)
		}
		return
	}
	delete(h.failed, subject)
	h.faulted[subject] = faulted
}

// unwritten returns an error naming the subjects whose last report did not
// reach the store, with the first one's write error, or nil when every
// report did.
func (h *judgmentRoleHealth) unwritten() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.failed) == 0 {
		return nil
	}
	subjects := slices.Sorted(maps.Keys(h.failed))
	names := make([]string, len(subjects))
	for i, subject := range subjects {
		names[i] = string(subject)
	}
	return fmt.Errorf("health item not written for %s: %w", strings.Join(names, ", "), h.failed[subjects[0]])
}

// checkJudgmentRolesAtStartup admits every judgment role once, before any
// site is reached, and leaves the store saying what it found: an item for
// each role the lineup cannot fill, one for a ledger that did not load, and
// none for a cause fixed since the last start. The drift auditor's item and
// the ledger's hold unattended admission, so no later call would raise or
// clear them, and a start that could not write the items stops here:
// started anyway, an unbound drift auditor or a disabled ledger would have
// nothing holding the work it audits, and a fixed one would stay held. A
// start cancelled during the check is reported as cancelled, because it
// proves nothing about the lineup.
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

func (h *judgmentRoleHealth) writeRole(ctx context.Context, role domain.RoleName, unbound bool, reason string) error {
	// Every store write advances the revision clients watch, so look first
	// and write only when the item is not already as it should be.
	open, err := h.openCauses(ctx, judgmentRoleItemKind)
	if err != nil {
		return err
	}
	open = slices.DeleteFunc(open, func(cause string) bool { return judgmentRoleOfCause(cause) != role })
	causes := map[string]healthCause{}
	var want []string
	if unbound {
		key := judgmentRoleCauseKey(role, reason)
		want, causes[key] = []string{key}, judgmentRoleCause(role, reason)
	}
	if slices.Equal(open, want) {
		return nil
	}
	// One role is converged at a time; every other role's item is left as it
	// is.
	return convergeHealthItems(ctx, h.st, judgmentRoleItemKind, causes,
		func(cause string) bool { return judgmentRoleOfCause(cause) != role }, h.now())
}

func (h *judgmentRoleHealth) writeLedger(ctx context.Context, disabled bool, cause string) error {
	open, err := h.openCauses(ctx, inferenceLedgerItemKind)
	if err != nil {
		return err
	}
	causes := map[string]healthCause{}
	var want []string
	if disabled {
		key := inferenceLedgerCauseKey(cause)
		want, causes[key] = []string{key}, inferenceLedgerCause(cause)
	}
	if slices.Equal(open, want) {
		return nil
	}
	return convergeHealthItems(ctx, h.st, inferenceLedgerItemKind, causes, nil, h.now())
}

// openCauses returns the cause keys of the kind's open items.
func (h *judgmentRoleHealth) openCauses(ctx context.Context, kind healthItemKind) ([]string, error) {
	var open []string
	err := h.st.Read(ctx, func(tx *store.ReadTx) error {
		items, err := tx.ListOpenAttentionItems(ctx, domain.AttentionSystemHealth)
		if err != nil {
			return err
		}
		for _, item := range items {
			rest, ok := strings.CutPrefix(string(item.ID), kind.Prefix)
			if !ok {
				continue
			}
			// The id ends with the revision that created it.
			open = append(open, rest[:max(strings.LastIndexByte(rest, '-'), 0)])
		}
		return nil
	})
	return open, err
}
