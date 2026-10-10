package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/wardstore"
)

const (
	adoptClaudeVolume  = "freeside-claude-main-auth"
	adoptClaudeAccount = "acct-fixture-claude"
	adoptCodexAccount  = "acct-fixture-codex"
)

// authAdoptFixture is a store holding the two flag-era identities the daemon
// runs on before the cutover, with the Codex identity's live store on disk.
type authAdoptFixture struct {
	dbPath, storeRoot, storePath, promptDir string
	observed                                int
}

func newAuthAdoptFixture(t *testing.T) *authAdoptFixture {
	t.Helper()
	// The Codex store path is recorded resolved, as enroll-codex records it.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &authAdoptFixture{
		dbPath: filepath.Join(root, "freeside.db"), storeRoot: filepath.Join(root, "store"),
		promptDir: filepath.Join(root, "prompts"),
	}
	f.storePath = filepath.Join(f.storeRoot, "auth.json")
	for _, dir := range []string{f.storeRoot, f.promptDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f.writeCodexStore(t, adoptCodexAccount)
	for _, role := range []string{"specifier", "implementer", "remediator"} {
		if err := os.WriteFile(filepath.Join(f.promptDir, role), []byte("prompt for "+role+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f.recordIdentity(t, domain.AuthIdentity{
		ID: "claude-main", Provider: "claude", AuthStoreMutationLease: true,
		MaxParallelExecutions: 1, Enabled: true,
		Interim: domain.InterimClientFacts{
			AuthStoreVolume: adoptClaudeVolume, RefreshStrategy: domain.RefreshOnDemand,
		},
	})
	f.recordIdentity(t, domain.AuthIdentity{
		ID: "codex-review", Provider: "openai", AuthStoreMutationLease: true,
		MaxParallelExecutions: 1, Enabled: true,
		Interim: domain.InterimClientFacts{
			AuthStoreVolume: f.storePath, RefreshStrategy: domain.RefreshOnDemand,
			SupportsReadOnlyAuthSnapshot: true,
		},
	})
	return f
}

// writeCodexStore writes the live store; an empty account leaves the tokens
// naming none.
func (f *authAdoptFixture) writeCodexStore(t *testing.T, account string) {
	t.Helper()
	var auth map[string]any
	if err := json.Unmarshal(commandCodexAuth("operator-refresh"), &auth); err != nil {
		t.Fatal(err)
	}
	if account != "" {
		auth["tokens"].(map[string]any)["account_id"] = account
	}
	body, err := json.Marshal(auth)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.storePath, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *authAdoptFixture) withStore(t *testing.T, fn func(*store.Store)) {
	t.Helper()
	st, _, err := openStoreWithTopicKey(context.Background(), f.dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	fn(st)
}

func (f *authAdoptFixture) recordIdentity(t *testing.T, identity domain.AuthIdentity) {
	t.Helper()
	f.withStore(t, func(st *store.Store) {
		if err := st.WriteInternal(context.Background(), func(tx *store.InternalTx) error {
			return tx.RecordAuthIdentity(context.Background(), identity, time.Now().UTC())
		}); err != nil {
			t.Fatal(err)
		}
	})
}

// authAdoptSnapshot is everything adoption may write for the two identities.
type authAdoptSnapshot struct {
	Identities  []domain.AuthIdentity
	Enrollments []domain.ClientEnrollment
	Generations []domain.EnrollmentGeneration
}

func (f *authAdoptFixture) snapshot(t *testing.T) authAdoptSnapshot {
	t.Helper()
	var snap authAdoptSnapshot
	f.withStore(t, func(st *store.Store) {
		ctx := context.Background()
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			if snap.Identities, err = tx.ListAuthIdentities(ctx); err != nil {
				return err
			}
			for _, identity := range snap.Identities {
				enrollments, err := tx.ListClientEnrollments(ctx, identity.ID)
				if err != nil {
					return err
				}
				for _, enrollment := range enrollments {
					snap.Enrollments = append(snap.Enrollments, enrollment)
					generation, err := tx.CurrentEnrollmentGeneration(ctx, enrollment.ID)
					if err != nil {
						return err
					}
					snap.Generations = append(snap.Generations, generation)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
	return snap
}

func (s authAdoptSnapshot) identity(t *testing.T, id domain.AuthIdentityID) domain.AuthIdentity {
	t.Helper()
	for _, identity := range s.Identities {
		if identity.ID == id {
			return identity
		}
	}
	t.Fatalf("identity %s is not recorded", id)
	return domain.AuthIdentity{}
}

func (f *authAdoptFixture) deps() authAdoptDeps {
	return authAdoptDeps{
		observeVolume: func(string, string) func(context.Context, string) (domain.Digest, error) {
			return func(_ context.Context, volume string) (domain.Digest, error) {
				f.observed++
				return domain.Digest(contentaddr.Sum([]byte("tree of " + volume))), nil
			}
		},
		now: func() time.Time { return time.Now().UTC() },
	}
}

// args is the adoption of both identities with the shadow arm off; extra
// flags append, and a later flag overrides an earlier one.
func (f *authAdoptFixture) args(extra ...string) []string {
	args := []string{
		"-db", f.dbPath, "-auth-identity", "claude-main", "-review-auth-identity", "codex-review",
		"-cost-owner", "operator", "-review-cost-owner", "review-operator",
		"-claude-account", adoptClaudeAccount, "-auth-store-root", f.storeRoot,
		"-exporter-image", "example.test/exporter@sha256:" + strings.Repeat("0", 64),
		"-review-model", "gpt-fixture-1",
		"-prompt-package", filepath.Join(f.promptDir, "implementer"),
		"-specification-prompt-package", filepath.Join(f.promptDir, "specifier"),
		"-remediation-prompt-package", filepath.Join(f.promptDir, "remediator"),
	}
	return append(args, extra...)
}

// run runs the command with the patch on stdout and the report on stderr.
func (f *authAdoptFixture) run(t *testing.T, args []string) (authAdoptReport, []byte, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := runAuthAdoptCommand(context.Background(), args, &stdout, &stderr, f.deps())
	var report authAdoptReport
	for _, out := range []*bytes.Buffer{&stderr, &stdout} {
		if json.Unmarshal(out.Bytes(), &report) == nil && len(report.Identities) != 0 {
			break
		}
	}
	return report, stdout.Bytes(), err
}

func gitForAdoptTest(t *testing.T, dir string, stdin []byte, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...) //nolint:gosec // G204: fixed test arguments
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commitAdoptedPatch commits the emitted patch in a fresh checkout and
// returns the checkout and the commit, as the operator hands them to the
// daemon.
func commitAdoptedPatch(t *testing.T, patch []byte) (checkout, commit string) {
	t.Helper()
	checkout = t.TempDir()
	gitForAdoptTest(t, checkout, nil, "init", "-q")
	gitForAdoptTest(t, checkout, patch, "apply", "--index", "-")
	gitForAdoptTest(t, checkout, nil, "commit", "-q", "-m", "adopt")
	return checkout, gitForAdoptTest(t, checkout, nil, "rev-parse", "HEAD")
}

// loadAdoptedPatch commits the emitted patch in a fresh checkout and reads
// it back through the loader the daemon uses.
func loadAdoptedPatch(t *testing.T, patch []byte) agenttree.Tree {
	t.Helper()
	checkout, commit := commitAdoptedPatch(t, patch)
	files, err := agenttree.ReadCommit(context.Background(), checkout, t.TempDir(), commit)
	if err != nil {
		t.Fatalf("read committed patch: %v", err)
	}
	tree, err := agenttree.Parse(files)
	if err != nil {
		t.Fatalf("parse committed patch: %v", err)
	}
	return tree
}

func lineupRoles(tree agenttree.Tree) []string {
	var roles []string
	for _, line := range tree.Lineup {
		roles = append(roles, line.Key+"="+line.Selection.AgentName)
	}
	return roles
}

// judgmentLines is the lineup lines adoption emits for the judgment roles
// whose prompt the daemon owns, in lineup order.
func judgmentLines() []string {
	var lines []string
	for _, role := range agentbaseline.JudgmentRoles() {
		if role != domain.RolePublicationAuthor {
			lines = append(lines, string(role)+"="+agentbaseline.ClaudeCallAgentName)
		}
	}
	return lines
}

// TestAuthAdoptEnrollsBothIdentitiesAndEmitsAResolvingTree covers the two
// adoptions, the interim facts the flag path still reads, and the round trip
// of the emitted patch through the loader.
func TestAuthAdoptEnrollsBothIdentitiesAndEmitsAResolvingTree(t *testing.T) {
	f := newAuthAdoptFixture(t)
	before := f.snapshot(t)
	report, patch, err := f.run(t, f.args())
	if err != nil {
		t.Fatalf("auth adopt: %v", err)
	}
	want := []authAdoptIdentity{
		{
			AuthIdentityID: "claude-main", Client: domain.HarnessClientClaudeCode, Status: authAdoptAdopted,
			EnrollmentID: "claude-main/claude_code", Generation: 1,
		},
		{
			AuthIdentityID: "codex-review", Client: domain.HarnessClientCodexCLI, Status: authAdoptAdopted,
			EnrollmentID: "codex-review/codex_cli", Generation: 1,
		},
	}
	if !reflect.DeepEqual(report.Identities, want) || report.Patch != "stdout" {
		t.Fatalf("report = %+v", report)
	}
	after := f.snapshot(t)
	if len(after.Enrollments) != 2 || len(after.Generations) != 2 {
		t.Fatalf("snapshot = %+v", after)
	}
	for _, id := range []domain.AuthIdentityID{"claude-main", "codex-review"} {
		was, is := before.identity(t, id), after.identity(t, id)
		// The daemon still runs on the flags until the cutover, and they read
		// exactly these: the interim facts and the enabled bit.
		if !is.SameFixedBindings(was) || !is.Enabled || is.AccountBinding == "" {
			t.Fatalf("identity %s after adoption = %+v", id, is)
		}
	}
	if owner := after.identity(t, "codex-review").CostOwner; owner != "review-operator" {
		t.Fatalf("review cost owner = %q", owner)
	}
	if owner := after.identity(t, "claude-main").CostOwner; owner != "operator" {
		t.Fatalf("implementation cost owner = %q", owner)
	}
	f.withStore(t, func(st *store.Store) {
		adapters, err := wardstore.New(st)
		if err != nil {
			t.Fatal(err)
		}
		volume, err := adapters.Leaser.AuthStoreVolume(context.Background(), "claude-main", "legacy-holder")
		if err != nil || volume != adoptClaudeVolume {
			t.Fatalf("flag-path volume = %q, %v", volume, err)
		}
	})
	for _, generation := range after.Generations {
		wantVolume, wantExpiry := adoptClaudeVolume, false
		if generation.EnrollmentID == "codex-review/codex_cli" {
			wantVolume, wantExpiry = f.storePath, true
		}
		if generation.AuthStoreVolume != wantVolume || (generation.TokenExpiry != nil) != wantExpiry {
			t.Fatalf("generation = %+v", generation)
		}
	}
	if strings.Contains(string(patch), adoptClaudeAccount) || strings.Contains(string(patch), adoptCodexAccount) {
		t.Fatal("the tree patch carries an account binding")
	}

	tree := loadAdoptedPatch(t, patch)
	wantRoles := []string{
		"specifier=" + agentbaseline.ClaudeAgentName, "implementer=" + agentbaseline.ClaudeAgentName,
		"remediator=" + agentbaseline.ClaudeAgentName, "reviewer=" + agentbaseline.CodexReviewAgentName,
	}
	wantRoles = append(wantRoles, judgmentLines()...)
	if got := lineupRoles(tree); !reflect.DeepEqual(got, wantRoles) {
		t.Fatalf("lineup = %v, want %v", got, wantRoles)
	}
	if len(tree.Marks) != 3 {
		t.Fatalf("attended marks = %+v", tree.Marks)
	}
	for i, name := range []string{agentbaseline.ClaudeAgentName, agentbaseline.CodexReviewAgentName} {
		enrollment := after.Enrollments[i]
		resolved, err := tree.ResolveAgent(name, enrollment, after.identity(t, enrollment.AuthIdentityID))
		if err != nil {
			t.Fatalf("resolve %s: %v", name, err)
		}
		if resolved.Definition.EnrollmentID != enrollment.ID {
			t.Fatalf("agent %s enrollment = %s", name, resolved.Definition.EnrollmentID)
		}
	}
}

// TestAuthAdoptTwiceChangesNothing reruns adoption: each identity keeps its
// enrollment, no generation is appended, and no volume is observed again.
func TestAuthAdoptTwiceChangesNothing(t *testing.T) {
	f := newAuthAdoptFixture(t)
	if _, _, err := f.run(t, f.args()); err != nil {
		t.Fatalf("first adopt: %v", err)
	}
	first, observed := f.snapshot(t), f.observed
	report, _, err := f.run(t, f.args())
	if err != nil {
		t.Fatalf("second adopt: %v", err)
	}
	for _, identity := range report.Identities {
		if identity.Status != authAdoptReused || identity.Generation != 1 {
			t.Fatalf("second adopt reported %+v", identity)
		}
	}
	if second := f.snapshot(t); !reflect.DeepEqual(first, second) || f.observed != observed {
		t.Fatalf("second adopt changed the store or observed the volume:\n%+v\n%+v", first, second)
	}
	// A reused enrollment still refuses a cost owner that is not the stored one.
	if _, _, err := f.run(t, f.args("-review-cost-owner", "someone-else")); err == nil ||
		!strings.Contains(err.Error(), `has cost owner "review-operator"`) {
		t.Fatalf("adopt with another cost owner = %v", err)
	}
}

// TestAuthAdoptOneIdentityIsOneAdoption names one identity under both flags.
func TestAuthAdoptOneIdentityIsOneAdoption(t *testing.T) {
	f := newAuthAdoptFixture(t)
	args := []string{
		"-db", f.dbPath, "-auth-identity", "claude-main", "-review-auth-identity", "claude-main",
		"-cost-owner", "operator", "-review-cost-owner", "operator", "-claude-account", adoptClaudeAccount,
		"-exporter-image", "example.test/exporter@sha256:" + strings.Repeat("0", 64),
		"-prompt-package", filepath.Join(f.promptDir, "implementer"),
		"-specification-prompt-package", filepath.Join(f.promptDir, "specifier"),
		"-remediation-prompt-package", filepath.Join(f.promptDir, "remediator"),
	}
	report, patch, err := f.run(t, args)
	if err != nil {
		t.Fatalf("auth adopt: %v", err)
	}
	if len(report.Identities) != 1 || report.Identities[0].Status != authAdoptAdopted {
		t.Fatalf("report = %+v", report)
	}
	if snap := f.snapshot(t); len(snap.Enrollments) != 1 {
		t.Fatalf("enrollments = %+v", snap.Enrollments)
	}
	tree := loadAdoptedPatch(t, patch)
	if len(tree.Agents) != 2 || len(tree.Lineup) != 3+len(judgmentLines()) {
		t.Fatalf("tree agents = %+v, lineup = %v", tree.Agents, lineupRoles(tree))
	}

	mismatch := append([]string{}, args...)
	mismatch = append(mismatch, "-review-cost-owner", "someone-else")
	if _, _, err := f.run(t, mismatch); err == nil || !strings.Contains(err.Error(), "must equal -cost-owner") {
		t.Fatalf("one identity with two cost owners = %v", err)
	}
}

// TestAuthAdoptKeepsAnEnrolledIdentityDisabled covers the ordering rule:
// adoption enables only in its first-enrollment write. An identity disabled
// after it was enrolled (the write here stands in for `auth disable`) is
// reused as it stands, and the report says so, because the emitted tree
// refuses to resolve it.
func TestAuthAdoptKeepsAnEnrolledIdentityDisabled(t *testing.T) {
	ctx := context.Background()
	f := newAuthAdoptFixture(t)
	if _, _, err := f.run(t, f.args()); err != nil {
		t.Fatalf("first adopt: %v", err)
	}
	f.withStore(t, func(st *store.Store) { setIdentityEnabled(t, st, "codex-review", false) })
	before := f.snapshot(t)
	report, patch, err := f.run(t, f.args())
	if err != nil || len(report.Identities) != 2 ||
		report.Identities[0].Status != authAdoptReused || report.Identities[0].Disabled ||
		report.Identities[1].Status != authAdoptReused || !report.Identities[1].Disabled ||
		report.Identities[0].Enabled || report.Identities[1].Enabled {
		t.Fatalf("second adopt = %+v, %v", report, err)
	}
	if after := f.snapshot(t); !reflect.DeepEqual(before, after) {
		t.Fatalf("second adopt changed the store:\n%+v\n%+v", before, after)
	}
	tree := loadAdoptedPatch(t, patch)
	f.withStore(t, func(st *store.Store) {
		if _, err := engine.ResolveRole(ctx, st, tree, domain.RoleImplementer); err != nil {
			t.Fatalf("resolve implementer: %v", err)
		}
		if _, err := engine.ResolveRole(ctx, st, tree, domain.RoleReviewer); !errors.Is(err, engine.ErrAgentNotAdmissible) {
			t.Fatalf("resolve reviewer under a disabled identity = %v", err)
		}
	})
}

func setIdentityEnabled(t *testing.T, st *store.Store, id domain.AuthIdentityID, enabled bool) {
	t.Helper()
	ctx := context.Background()
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		identity, err := tx.GetAuthIdentity(ctx, id)
		if err != nil {
			return err
		}
		identity.Enabled = enabled
		return tx.RecordAuthIdentity(ctx, identity, time.Now().UTC())
	}); err != nil {
		t.Fatalf("set identity %s enabled=%t: %v", id, enabled, err)
	}
}

// TestAuthAdoptShadowArm records the shadow reviewer line while the shadow
// arm is on, and refuses a shadow cost owner that is not the implementation
// identity's.
func TestAuthAdoptShadowArm(t *testing.T) {
	f := newAuthAdoptFixture(t)
	if _, _, err := f.run(t, f.args("-shadow-review-cost-owner", "someone-else")); err == nil ||
		!strings.Contains(err.Error(), "-shadow-review-cost-owner must equal -cost-owner") {
		t.Fatalf("mismatched shadow cost owner = %v", err)
	}
	if snap := f.snapshot(t); len(snap.Enrollments) != 0 {
		t.Fatalf("a refused adoption recorded %+v", snap.Enrollments)
	}
	_, patch, err := f.run(t, f.args("-shadow-review-cost-owner", "operator"))
	if err != nil {
		t.Fatalf("auth adopt: %v", err)
	}
	roles := lineupRoles(loadAdoptedPatch(t, patch))
	if len(roles) != 5+len(judgmentLines()) || roles[4] != "shadow_reviewer="+agentbaseline.ClaudeAgentName {
		t.Fatalf("lineup = %v", roles)
	}
}

// TestAuthAdoptJudgmentLines covers the judgment roles' lines: every role
// with a built site gets one on the call agent, the publication author only
// when its prompt file is named, and that line records the file's digest.
func TestAuthAdoptJudgmentLines(t *testing.T) {
	f := newAuthAdoptFixture(t)
	if _, _, err := f.run(t, f.args("-judgment-publication-author-prompt",
		filepath.Join(f.promptDir, "absent"))); err == nil ||
		!strings.Contains(err.Error(), "-judgment-publication-author-prompt") {
		t.Fatalf("missing author prompt file = %v", err)
	}
	if snap := f.snapshot(t); len(snap.Enrollments) != 0 {
		t.Fatalf("a refused adoption recorded %+v", snap.Enrollments)
	}
	body := []byte("write the pull request for a reviewer\n")
	authorPrompt := filepath.Join(f.promptDir, "author")
	if err := os.WriteFile(authorPrompt, body, 0o600); err != nil {
		t.Fatal(err)
	}
	_, patch, err := f.run(t, f.args("-judgment-publication-author-prompt", authorPrompt))
	if err != nil {
		t.Fatalf("auth adopt: %v", err)
	}
	lineup, err := loadAdoptedPatch(t, patch).ResolveLineup()
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range agentbaseline.JudgmentRoles() {
		line, ok := lineup.Line(role)
		if !ok || line.AgentName != agentbaseline.ClaudeCallAgentName {
			t.Fatalf("line for %s = %+v, %v", role, line, ok)
		}
		want, owned := inference.CodeOwnedRolePrompt(role)
		if !owned {
			want = inference.OperatorRolePrompt(role, body)
		}
		if line.PromptName != want.Name || line.PromptDigest != want.Digest {
			t.Fatalf("line for %s names prompt %s %s, want %s %s",
				role, line.PromptName, line.PromptDigest, want.Name, want.Digest)
		}
	}
}

// TestAuthAdoptRefusesACostOwnerMismatchBeforeRecording gives the review
// identity a stored cost owner the flags disagree with: nothing is recorded
// for either identity.
func TestAuthAdoptRefusesACostOwnerMismatchBeforeRecording(t *testing.T) {
	f := newAuthAdoptFixture(t)
	f.withStore(t, func(st *store.Store) {
		ctx := context.Background()
		if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
			identity, err := tx.GetAuthIdentity(ctx, "codex-review")
			if err != nil {
				return err
			}
			identity.CostOwner = "stored-owner"
			return tx.RecordAuthIdentity(ctx, identity, time.Now().UTC())
		}); err != nil {
			t.Fatal(err)
		}
	})
	before := f.snapshot(t)
	if _, _, err := f.run(t, f.args()); err == nil ||
		!strings.Contains(err.Error(), `auth identity codex-review has cost owner "stored-owner"`) {
		t.Fatalf("auth adopt = %v", err)
	}
	if after := f.snapshot(t); !reflect.DeepEqual(before, after) || f.observed != 0 {
		t.Fatalf("a refused adoption changed the store:\n%+v\n%+v", before, after)
	}
}

// TestAuthAdoptReportsAnUnadoptableReviewIdentity covers both unadoptable
// shapes for the review identity: nothing is recorded for it, the Claude
// identity is still adopted, and the tree carries no review agent. Each shape
// also runs with the identity stored disabled: an adoption that records no
// enrollment enables nothing, including the one refused inside the
// first-enrollment transaction.
func TestAuthAdoptReportsAnUnadoptableReviewIdentity(t *testing.T) {
	for name, arrange := range map[string]func(*testing.T, *authAdoptFixture){
		"store names no account": func(t *testing.T, f *authAdoptFixture) { f.writeCodexStore(t, "") },
		"account bound to another identity": func(t *testing.T, f *authAdoptFixture) {
			f.recordIdentity(t, domain.AuthIdentity{
				ID: "codex-other", Provider: "openai", AccountBinding: adoptCodexAccount,
				MaxParallelExecutions: 1, Enabled: true, CostOwner: "operator",
			})
		},
	} {
		for state, enabled := range map[string]bool{"stored enabled": true, "stored disabled": false} {
			t.Run(name+"/"+state, func(t *testing.T) {
				f := newAuthAdoptFixture(t)
				arrange(t, f)
				f.withStore(t, func(st *store.Store) { setIdentityEnabled(t, st, "codex-review", enabled) })
				before := f.snapshot(t).identity(t, "codex-review")
				report, patch, err := f.run(t, f.args())
				if err != nil {
					t.Fatalf("auth adopt: %v", err)
				}
				if len(report.Identities) != 2 || report.Identities[0].Status != authAdoptAdopted ||
					report.Identities[1].Status != authAdoptUnadoptable || report.Identities[1].Reason == "" ||
					report.Identities[1].EnrollmentID != "" || report.Identities[1].Enabled ||
					strings.Contains(report.Identities[1].Reason, adoptCodexAccount) {
					t.Fatalf("report = %+v", report)
				}
				after := f.snapshot(t)
				if got := after.identity(t, "codex-review"); got != before || got.Enabled != enabled {
					t.Fatalf("unadoptable identity changed: %+v", got)
				}
				if len(after.Enrollments) != 1 || after.Enrollments[0].AuthIdentityID != "claude-main" {
					t.Fatalf("enrollments = %+v", after.Enrollments)
				}
				tree := loadAdoptedPatch(t, patch)
				if len(tree.Agents) != 2 || len(tree.Lineup) != 3+len(judgmentLines()) {
					t.Fatalf("tree agents = %+v, lineup = %v", tree.Agents, lineupRoles(tree))
				}
			})
		}
	}
}

// TestAuthAdoptUnadoptableClaudeIdentityEmitsNoPatch leaves the Claude
// account unattested: the identity is reported, nothing is recorded for it,
// and no tree is emitted because every writer role runs the Claude agent.
func TestAuthAdoptUnadoptableClaudeIdentityEmitsNoPatch(t *testing.T) {
	f := newAuthAdoptFixture(t)
	before := f.snapshot(t).identity(t, "claude-main")
	var stdout, stderr bytes.Buffer
	err := runAuthAdoptCommand(context.Background(), f.args("-claude-account", ""), &stdout, &stderr, f.deps())
	if err == nil || !strings.Contains(err.Error(), "no tree patch was emitted") {
		t.Fatalf("auth adopt = %v", err)
	}
	var report authAdoptReport
	// The patch would have owned stdout, so the report is on stderr and
	// stdout stays empty for the pipe into git apply.
	if err := json.Unmarshal(stderr.Bytes(), &report); err != nil || stdout.Len() != 0 {
		t.Fatalf("decode %q: %v; stdout = %q", stderr.String(), err, stdout.String())
	}
	if len(report.Identities) != 2 || report.Identities[0].Status != authAdoptUnadoptable || report.Patch != "" {
		t.Fatalf("report = %+v", report)
	}
	if after := f.snapshot(t).identity(t, "claude-main"); after != before || f.observed != 0 {
		t.Fatalf("unadoptable identity changed: %+v", after)
	}
}

// TestAuthAdoptWritesThePatchToAFileOutsideAnyCheckout covers -patch.
func TestAuthAdoptWritesThePatchToAFileOutsideAnyCheckout(t *testing.T) {
	f := newAuthAdoptFixture(t)
	checkout := t.TempDir()
	gitForAdoptTest(t, checkout, nil, "init", "-q")
	inside := filepath.Join(checkout, "sub")
	if err := os.Mkdir(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.run(t, f.args("-patch", filepath.Join(inside, "adopt.patch"))); err == nil ||
		!strings.Contains(err.Error(), "is inside the checkout") {
		t.Fatalf("patch inside a checkout = %v", err)
	}
	if snap := f.snapshot(t); len(snap.Enrollments) != 0 {
		t.Fatalf("a refused adoption recorded %+v", snap.Enrollments)
	}
	path := filepath.Join(t.TempDir(), "adopt.patch")
	report, stdout, err := f.run(t, f.args("-patch", path))
	if err != nil {
		t.Fatalf("auth adopt: %v", err)
	}
	patch, err := os.ReadFile(path) //nolint:gosec // G304: test-owned path
	if err != nil {
		t.Fatal(err)
	}
	if report.Patch != path || bytes.Contains(stdout, []byte("diff --git")) {
		t.Fatalf("report = %+v, stdout = %s", report, stdout)
	}
	tree := loadAdoptedPatch(t, patch)
	if revision, err := treeRevision(tree); err != nil || revision != report.LineupRevision {
		t.Fatalf("lineup revision = %s, %v; report = %s", revision, err, report.LineupRevision)
	}
}

func treeRevision(tree agenttree.Tree) (domain.Digest, error) {
	files, err := agenttree.Render(tree)
	if err != nil {
		return "", err
	}
	return agenttree.Revision(files)
}

func TestAuthAdoptRefusesAMissingIdentity(t *testing.T) {
	f := newAuthAdoptFixture(t)
	_, _, err := f.run(t, f.args("-auth-identity", "absent"))
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("auth adopt = %v, want not found", err)
	}
}

// TestAuthAdoptReportsTheFirstAdoptionWhenTheSecondFails removes the Codex
// store: the Claude adoption is already recorded, so the report says so
// beside the error, and a rerun reuses it.
func TestAuthAdoptReportsTheFirstAdoptionWhenTheSecondFails(t *testing.T) {
	f := newAuthAdoptFixture(t)
	if err := os.Remove(f.storePath); err != nil {
		t.Fatal(err)
	}
	report, patch, err := f.run(t, f.args())
	if err == nil || !strings.Contains(err.Error(), "read live auth store") || len(patch) != 0 {
		t.Fatalf("auth adopt = %v, patch = %q", err, patch)
	}
	if len(report.Identities) != 1 || report.Identities[0].Status != authAdoptAdopted {
		t.Fatalf("report = %+v", report)
	}
	f.writeCodexStore(t, adoptCodexAccount)
	report, _, err = f.run(t, f.args())
	if err != nil || report.Identities[0].Status != authAdoptReused || report.Identities[1].Status != authAdoptAdopted {
		t.Fatalf("rerun = %+v, %v", report, err)
	}
}

// TestDaemonLoadsTheAdoptedTreeAtItsCommit covers the daemon's side of the
// hand-off: the tree flags read the committed patch, cite the revision adopt
// reported, and ignore the checkout's working tree.
func TestDaemonLoadsTheAdoptedTreeAtItsCommit(t *testing.T) {
	f := newAuthAdoptFixture(t)
	report, patch, err := f.run(t, f.args())
	if err != nil {
		t.Fatalf("auth adopt: %v", err)
	}
	checkout, commit := commitAdoptedPatch(t, patch)
	// An uncommitted edit must not reach admission.
	if err := os.WriteFile(filepath.Join(checkout, "policy", "lineup"), []byte("edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := claudeDriverConfig{
		AgentTreeCheckout: checkout, AgentTreeCommit: commit,
		ProviderEndpoints: []string{"b.example:443", "a.example:443"},
	}
	selection, err := loadAgentSelection(context.Background(), cfg)
	if err != nil {
		t.Fatalf("load agent selection: %v", err)
	}
	if selection.LineupRevision != report.LineupRevision ||
		!slices.Equal(selection.EffectiveEgress, []string{"a.example:443", "b.example:443"}) {
		t.Fatalf("selection = %+v, want revision %s", selection, report.LineupRevision)
	}
	if _, err := selection.Tree.ResolveLineup(); err != nil {
		t.Fatalf("loaded lineup: %v", err)
	}

	cfg.AgentTreeCommit = "HEAD"
	if _, err := loadAgentSelection(context.Background(), cfg); !errors.Is(err, agenttree.ErrCommit) {
		t.Fatalf("a symbolic commit = %v, want %v", err, agenttree.ErrCommit)
	}
}

// TestAdoptedPatchResolvesAndAdmitsEveryRole is the cutover's admission
// proof: against the store adoption wrote, the committed patch resolves all
// five roles, and each writer role passes the five admission steps for the
// prompt the daemon runs it with, attended and unattended. It runs from both
// starting stores: identities stored enabled, and the upgrade case, the rows
// the pre-#867 harness seed step wrote (disabled, with no account binding and
// no cost owner), which adoption enables.
func TestAdoptedPatchResolvesAndAdmitsEveryRole(t *testing.T) {
	for name, seedDisabled := range map[string]bool{
		"identities stored enabled":                  false,
		"identities the old seed step left disabled": true,
	} {
		t.Run(name, func(t *testing.T) {
			f := newAuthAdoptFixture(t)
			if seedDisabled {
				f.withStore(t, func(st *store.Store) {
					setIdentityEnabled(t, st, "claude-main", false)
					setIdentityEnabled(t, st, "codex-review", false)
				})
			}
			report, patch, err := f.run(t, f.args("-shadow-review-cost-owner", "operator"))
			if err != nil || len(report.Identities) != 2 {
				t.Fatalf("auth adopt = %+v, %v", report, err)
			}
			after := f.snapshot(t)
			for _, entry := range report.Identities {
				if entry.Status != authAdoptAdopted || entry.Enabled != seedDisabled || entry.Disabled ||
					!after.identity(t, entry.AuthIdentityID).Enabled {
					t.Fatalf("report entry = %+v, stored = %+v", entry, after.identity(t, entry.AuthIdentityID))
				}
			}
			assertAdoptedPatchResolvesAndAdmitsEveryRole(t, f, patch)
		})
	}
}

func assertAdoptedPatchResolvesAndAdmitsEveryRole(t *testing.T, f *authAdoptFixture, patch []byte) {
	t.Helper()
	ctx := context.Background()
	checkout, commit := commitAdoptedPatch(t, patch)
	selection, err := loadAgentSelection(ctx, claudeDriverConfig{
		AgentTreeCheckout: checkout, AgentTreeCommit: commit,
		ProviderEndpoints: []string{"api.anthropic.com:443"},
	})
	if err != nil {
		t.Fatalf("load agent selection: %v", err)
	}
	selection.AttemptBudget = time.Hour
	prompts, err := readAdoptPrompts(authAdoptConfig{
		PromptPackage:              filepath.Join(f.promptDir, "implementer"),
		SpecificationPromptPackage: filepath.Join(f.promptDir, "specifier"),
		RemediationPromptPackage:   filepath.Join(f.promptDir, "remediator"),
	})
	if err != nil {
		t.Fatal(err)
	}
	wantIdentity := map[domain.RoleName]domain.AuthIdentityID{
		domain.RoleSpecifier: "claude-main", domain.RoleImplementer: "claude-main",
		domain.RoleRemediator: "claude-main", domain.RoleReviewer: "codex-review",
		domain.RoleShadowReviewer: "claude-main",
	}
	f.withStore(t, func(st *store.Store) {
		now := time.Now().UTC()
		if err := recordBaselineAdapterConformance(ctx, st, now); err != nil {
			t.Fatal(err)
		}
		for role, identity := range wantIdentity {
			agent, err := engine.ResolveRole(ctx, st, selection.Tree, role)
			if err != nil {
				t.Fatalf("resolve %s: %v", role, err)
			}
			if agent.Identity.ID != identity || agent.Generation.Ordinal != 1 {
				t.Fatalf("role %s resolved identity %s generation %d", role, agent.Identity.ID, agent.Generation.Ordinal)
			}
			prompt, writer := prompts[role]
			if !writer {
				// The review roles' launch coverage arrives with the review
				// record (#898); they have no ward stage to admit.
				if _, err := selection.CheckRole(
					ctx, st, role, agent.Line.PromptDigest, domain.ModeAttendedDev, now,
				); !errors.Is(err, engine.ErrAgentNotAdmissible) {
					t.Fatalf("stage admission of review role %s = %v", role, err)
				}
				continue
			}
			for _, mode := range []domain.OperatingMode{domain.ModeAttendedDev, domain.ModeUnattended} {
				binding, err := selection.CheckRole(ctx, st, role, prompt.Digest, mode, now)
				if err != nil {
					t.Fatalf("admit %s in %s: %v", role, mode, err)
				}
				if binding.EnrollmentID != agent.Enrollment.ID || binding.EnrollmentGeneration != 1 ||
					binding.LineupRevision != selection.LineupRevision ||
					binding.Attended != (mode != domain.ModeUnattended) {
					t.Fatalf("role %s binding in %s = %+v", role, mode, binding)
				}
			}
		}
		// A lineup without the role refuses, naming it.
		_, err := engine.ResolveRole(ctx, st, agenttree.Tree{}, domain.RoleReviewer)
		if !errors.Is(err, engine.ErrAgentNotAdmissible) || !strings.Contains(err.Error(), "role reviewer") {
			t.Fatalf("resolve against an empty tree = %v", err)
		}
	})
}
