package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// routingTreeCheckout commits an adopted agent tree in a fresh checkout and
// returns the checkout, its commit, the tree's revision, and one agent it
// carries.
func routingTreeCheckout(t *testing.T) (checkout, commit string, revision domain.Digest, agent string) {
	t.Helper()
	f := newAuthAdoptFixture(t)
	report, patch, err := f.run(t, f.args())
	if err != nil {
		t.Fatalf("auth adopt: %v", err)
	}
	checkout, commit = commitAdoptedPatch(t, patch)
	return checkout, commit, report.LineupRevision, loadAdoptedPatch(t, patch).Agents[0].Name
}

func runRouting(ctx context.Context, args ...string) (string, error) {
	var stdout bytes.Buffer
	err := runRoutingCommand(ctx, args, &stdout, io.Discard)
	return stdout.String(), err
}

func decodeRoutingSet(t *testing.T, output string) routingSetResult {
	t.Helper()
	var result routingSetResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("decode %q: %v", output, err)
	}
	return result
}

func routingTestStore(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	if err := storetest.Open(t, dbPath, store.Options{}).Close(); err != nil {
		t.Fatal(err)
	}
	return dbPath
}

func TestRoutingSetAndShowOnAStoppedDaemon(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	checkout, commit, revision, agent := routingTreeCheckout(t)
	dbPath := routingTestStore(t)
	set := func(order string) (string, error) {
		return runRouting(ctx, "set", "-db", dbPath, "-agent-tree", checkout, "-agent-tree-commit", commit, "-order", order)
	}

	if output, err := runRouting(ctx, "show", "-db", dbPath); err != nil || strings.TrimSpace(output) != "null" {
		t.Fatalf("show before any set = %q, %v", output, err)
	}
	if output, err := runRouting(ctx, "show", "-db", dbPath, "-revisions"); err != nil || strings.TrimSpace(output) != "[]" {
		t.Fatalf("show -revisions before any set = %q, %v", output, err)
	}

	output, err := set("implementer=" + agent)
	if err != nil {
		t.Fatal(err)
	}
	first := decodeRoutingSet(t, output)
	if !first.Recorded || first.Policy.Revision != 1 || first.Policy.LineupRevision != revision ||
		first.Policy.Source != domain.RoutingPolicySourceCLISet || !strings.HasPrefix(first.Policy.SetBy, "cli:") ||
		len(first.Policy.Roles) != 1 || !slices.Equal(first.Policy.Roles[0].Agents, []string{agent}) {
		t.Fatalf("first set = %+v", first)
	}

	output, err = set("implementer=" + agent)
	if err != nil {
		t.Fatal(err)
	}
	if repeat := decodeRoutingSet(t, output); repeat.Recorded || repeat.Policy.Revision != 1 {
		t.Fatalf("repeat = %+v", repeat)
	}

	output, err = set("reviewer=" + agent)
	if err != nil {
		t.Fatal(err)
	}
	if changed := decodeRoutingSet(t, output); !changed.Recorded || changed.Policy.Revision != 2 {
		t.Fatalf("changed = %+v", changed)
	}

	output, err = runRouting(ctx, "show", "-db", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var current domain.RoutingPolicy
	if err := json.Unmarshal([]byte(output), &current); err != nil {
		t.Fatalf("decode %q: %v", output, err)
	}
	if current.Revision != 2 || current.Roles[0].Role != domain.RoleReviewer {
		t.Fatalf("show = %+v", current)
	}
	output, err = runRouting(ctx, "show", "-db", dbPath, "-revisions")
	if err != nil {
		t.Fatal(err)
	}
	var revisions []domain.RoutingPolicy
	if err := json.Unmarshal([]byte(output), &revisions); err != nil {
		t.Fatalf("decode %q: %v", output, err)
	}
	if len(revisions) != 2 || revisions[0].Revision != 1 || revisions[1].Revision != 2 {
		t.Fatalf("show -revisions = %+v", revisions)
	}
}

func TestRoutingSetRefusesAnAgentTheTreeLacks(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	checkout, commit, _, _ := routingTreeCheckout(t)
	dbPath := routingTestStore(t)
	_, err := runRouting(ctx, "set", "-db", dbPath, "-agent-tree", checkout, "-agent-tree-commit", commit,
		"-order", "implementer=not-in-tree")
	if !errors.Is(err, domain.ErrRoutingPolicyAgentUnknown) {
		t.Fatalf("set = %v, want %v", err, domain.ErrRoutingPolicyAgentUnknown)
	}
	if output, err := runRouting(ctx, "show", "-db", dbPath); err != nil || strings.TrimSpace(output) != "null" {
		t.Fatalf("show after a refusal = %q, %v", output, err)
	}
}

// TestRoutingSetRecordsADrainFirstAccount: -drain-first names a pool an
// agent of the tree is on, a set without it clears the account as a new
// revision, and an account no agent is on is refused and writes nothing.
func TestRoutingSetRecordsADrainFirstAccount(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	// The adopt fixture's store holds the enrollments the adopted tree's
	// agents name, which the account check reads.
	f := newAuthAdoptFixture(t)
	report, patch, err := f.run(t, f.args())
	if err != nil {
		t.Fatalf("auth adopt: %v", err)
	}
	checkout, commit := commitAdoptedPatch(t, patch)
	agent := loadAdoptedPatch(t, patch).Agents[0]
	var pool domain.UsagePoolID
	st := storetest.Open(t, f.dbPath, store.Options{})
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		enrollment, err := tx.GetClientEnrollment(ctx, domain.ClientEnrollmentID(agent.Enrollment))
		if err != nil {
			return err
		}
		identity, err := tx.GetAuthIdentity(ctx, enrollment.AuthIdentityID)
		pool = identity.EffectiveUsagePool()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	set := func(extra ...string) (string, error) {
		return runRouting(ctx, append([]string{
			"set", "-db", f.dbPath, "-agent-tree", checkout,
			"-agent-tree-commit", commit, "-order", "implementer=" + agent.Name,
		}, extra...)...)
	}

	output, err := set("-drain-first", string(pool))
	if err != nil {
		t.Fatal(err)
	}
	named := decodeRoutingSet(t, output)
	if !named.Recorded || named.Policy.Revision != 1 || named.Policy.LineupRevision != report.LineupRevision ||
		named.Policy.DrainFirstAccount == nil || *named.Policy.DrainFirstAccount != pool {
		t.Fatalf("named = %+v", named)
	}
	if output, err = set("-drain-first", string(pool)); err != nil {
		t.Fatal(err)
	}
	if repeat := decodeRoutingSet(t, output); repeat.Recorded || repeat.Policy.Revision != 1 {
		t.Fatalf("repeat = %+v", repeat)
	}
	if _, err := set("-drain-first", "pool-nobody-is-on"); !errors.Is(err, domain.ErrRoutingPolicyDrainFirstUnknown) {
		t.Fatalf("unknown account = %v, want %v", err, domain.ErrRoutingPolicyDrainFirstUnknown)
	}
	if output, err = set(); err != nil {
		t.Fatal(err)
	}
	if cleared := decodeRoutingSet(t, output); !cleared.Recorded || cleared.Policy.Revision != 2 ||
		cleared.Policy.DrainFirstAccount != nil {
		t.Fatalf("cleared = %+v", cleared)
	}

	output, err = runRouting(ctx, "show", "-db", f.dbPath, "-revisions")
	if err != nil {
		t.Fatal(err)
	}
	var revisions []domain.RoutingPolicy
	if err := json.Unmarshal([]byte(output), &revisions); err != nil {
		t.Fatalf("decode %q: %v", output, err)
	}
	if len(revisions) != 2 || revisions[0].DrainFirstAccount == nil || revisions[1].DrainFirstAccount != nil {
		t.Fatalf("show -revisions = %+v", revisions)
	}
}

func TestRoutingCommandRefusals(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	const commit = "0123456789012345678901234567890123456789"
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no verb":    {nil, "usage: freesided routing"},
		"bad verb":   {[]string{"clear"}, "unknown routing command"},
		"no db":      {[]string{"set", "-agent-tree", "/tree", "-agent-tree-commit", commit, "-order", "implementer=a"}, "-db is required"},
		"no tree":    {[]string{"set", "-db", "x", "-agent-tree-commit", commit, "-order", "implementer=a"}, "-agent-tree is required"},
		"relative":   {[]string{"set", "-db", "x", "-agent-tree", "tree", "-agent-tree-commit", commit, "-order", "implementer=a"}, "absolute path"},
		"no commit":  {[]string{"set", "-db", "x", "-agent-tree", "/tree", "-order", "implementer=a"}, "-agent-tree-commit is required"},
		"no order":   {[]string{"set", "-db", "x", "-agent-tree", "/tree", "-agent-tree-commit", commit}, "-order is required"},
		"bad order":  {[]string{"set", "-db", "x", "-order", "implementer"}, "want role=agent1,agent2"},
		"no agents":  {[]string{"set", "-db", "x", "-order", "implementer="}, "want role=agent1,agent2"},
		"set extra":  {[]string{"set", "-db", "x", "-agent-tree", "/tree", "-agent-tree-commit", commit, "-order", "implementer=a", "extra"}, "unexpected positional"},
		"show no db": {[]string{"show"}, "-db is required"},
		"show extra": {[]string{"show", "-db", "x", "extra"}, "unexpected positional"},
	} {
		if _, err := runRouting(ctx, tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

func TestRoutingUsesRunningDaemonStore(t *testing.T) {
	checkout, commit, _, agent := routingTreeCheckout(t)
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	seedTemplateStore(t, dbPath)
	h, err := run(t.Context(), nil, config{
		Environment: environmentEphemeral, DBPath: dbPath, ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx := context.WithValue(t.Context(), directStoreOpenerKey{}, directStoreOpener(
		func(context.Context, string, store.Options, storeOpenMode) (*store.Store, error) {
			return nil, errors.New("unexpected direct store open")
		}))

	output, err := runRouting(ctx, "set", "-db", dbPath, "-agent-tree", checkout, "-agent-tree-commit", commit,
		"-order", "implementer="+agent)
	if err != nil {
		t.Fatal(err)
	}
	if result := decodeRoutingSet(t, output); !result.Recorded || result.Policy.Revision != 1 {
		t.Fatalf("set = %+v", result)
	}
	output, err = runRouting(ctx, "set", "-db", dbPath, "-agent-tree", checkout, "-agent-tree-commit", commit,
		"-order", "implementer="+agent)
	if err != nil {
		t.Fatal(err)
	}
	if repeat := decodeRoutingSet(t, output); repeat.Recorded || repeat.Policy.Revision != 1 {
		t.Fatalf("repeat = %+v", repeat)
	}
	if _, err := runRouting(ctx, "set", "-db", dbPath, "-agent-tree", checkout, "-agent-tree-commit", commit,
		"-order", "implementer=not-in-tree",
	); err == nil || !strings.Contains(err.Error(), "not-in-tree") {
		t.Fatalf("forwarded refusal = %v", err)
	}
	output, err = runRouting(ctx, "show", "-db", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var current domain.RoutingPolicy
	if err := json.Unmarshal([]byte(output), &current); err != nil {
		t.Fatalf("decode %q: %v", output, err)
	}
	if current.Revision != 1 {
		t.Fatalf("show = %+v", current)
	}
	output, err = runRouting(ctx, "show", "-db", dbPath, "-revisions")
	if err != nil {
		t.Fatal(err)
	}
	var revisions []domain.RoutingPolicy
	if err := json.Unmarshal([]byte(output), &revisions); err != nil || len(revisions) != 1 {
		t.Fatalf("show -revisions = %q, %v", output, err)
	}
	if err := h.store.Read(t.Context(), func(tx *store.ReadTx) error {
		policy, err := tx.CurrentRoutingPolicy(t.Context())
		if err == nil && policy.Revision != 1 {
			t.Errorf("running daemon store revision = %d, want 1", policy.Revision)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
