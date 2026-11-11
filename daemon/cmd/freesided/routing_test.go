package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// routingFixture is an adopted agent tree committed in a fresh checkout,
// with the adopt fixture's store, which holds the enrollments the tree's
// lineup names.
type routingFixture struct {
	dbPath, checkout, commit string
	revision                 domain.Digest
	tree                     agenttree.Tree
}

func newRoutingFixture(t *testing.T) routingFixture {
	t.Helper()
	f := newAuthAdoptFixture(t)
	report, patch, err := f.run(t, f.args())
	if err != nil {
		t.Fatalf("auth adopt: %v", err)
	}
	checkout, commit := commitAdoptedPatch(t, patch)
	return routingFixture{
		dbPath: f.dbPath, checkout: checkout, commit: commit, revision: report.LineupRevision,
		tree: loadAdoptedPatch(t, patch),
	}
}

// lineup returns the enrollment and model the tree's lineup binds the role
// to.
func (f routingFixture) lineup(t *testing.T, role domain.RoleName) domain.TaskLineSelection {
	t.Helper()
	for _, line := range f.tree.Lineup {
		if line.Key == string(role) {
			return line.Selection.TaskLineSelection()
		}
	}
	t.Fatalf("lineup binds no %s", role)
	return domain.TaskLineSelection{}
}

// withStore runs fn against the fixture's store while no daemon holds it.
func (f routingFixture) withStore(t *testing.T, fn func(*store.Store) error) {
	t.Helper()
	st := storetest.Open(t, f.dbPath, store.Options{})
	if err := fn(st); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

// identity returns the auth identity that holds the enrollment.
func (f routingFixture) identity(t *testing.T, id domain.ClientEnrollmentID) domain.AuthIdentity {
	t.Helper()
	var identity domain.AuthIdentity
	f.withStore(t, func(st *store.Store) error {
		return st.Read(t.Context(), func(tx *store.ReadTx) error {
			enrollment, err := tx.GetClientEnrollment(t.Context(), id)
			if err != nil {
				return err
			}
			identity, err = tx.GetAuthIdentity(t.Context(), enrollment.AuthIdentityID)
			return err
		})
	})
	return identity
}

func (f routingFixture) set(ctx context.Context, args ...string) (string, error) {
	return runRouting(ctx, append([]string{
		"set", "-db", f.dbPath, "-agent-tree", f.checkout, "-agent-tree-commit", f.commit,
	}, args...)...)
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

func TestRoutingSetAndShowOnAStoppedDaemon(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newRoutingFixture(t)
	enrollment := string(f.lineup(t, domain.RoleImplementer).EnrollmentID)

	if output, err := runRouting(ctx, "show", "-db", f.dbPath); err != nil || strings.TrimSpace(output) != "null" {
		t.Fatalf("show before any set = %q, %v", output, err)
	}
	if output, err := runRouting(ctx, "show", "-db", f.dbPath, "-revisions"); err != nil || strings.TrimSpace(output) != "[]" {
		t.Fatalf("show -revisions before any set = %q, %v", output, err)
	}

	output, err := f.set(ctx, "-order", "implementer="+enrollment)
	if err != nil {
		t.Fatal(err)
	}
	first := decodeRoutingSet(t, output)
	if !first.Recorded || first.Policy.Revision != 1 || first.Policy.LineupRevision != f.revision ||
		first.Policy.Source != domain.RoutingPolicySourceCLISet || !strings.HasPrefix(first.Policy.SetBy, "cli:") ||
		len(first.Policy.Roles) != 1 ||
		!slices.Equal(first.Policy.Roles[0].Enrollments, []domain.ClientEnrollmentID{domain.ClientEnrollmentID(enrollment)}) {
		t.Fatalf("first set = %+v", first)
	}

	output, err = f.set(ctx, "-order", "implementer="+enrollment)
	if err != nil {
		t.Fatal(err)
	}
	if repeat := decodeRoutingSet(t, output); repeat.Recorded || repeat.Policy.Revision != 1 {
		t.Fatalf("repeat = %+v", repeat)
	}

	output, err = f.set(ctx, "-order", "specifier="+enrollment)
	if err != nil {
		t.Fatal(err)
	}
	if changed := decodeRoutingSet(t, output); !changed.Recorded || changed.Policy.Revision != 2 {
		t.Fatalf("changed = %+v", changed)
	}

	output, err = runRouting(ctx, "show", "-db", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var current domain.RoutingPolicy
	if err := json.Unmarshal([]byte(output), &current); err != nil {
		t.Fatalf("decode %q: %v", output, err)
	}
	if current.Revision != 2 || current.Roles[0].Role != domain.RoleSpecifier {
		t.Fatalf("show = %+v", current)
	}
	output, err = runRouting(ctx, "show", "-db", f.dbPath, "-revisions")
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

// TestRoutingSetRefusesAnEnrollmentThatCannotRoute: an enrollment the store
// lacks, one on a disabled identity, and one whose recorded catalog does not
// serve the role's lineup model are refused, the refusal names the role and
// position but not the enrollment, and nothing is written.
func TestRoutingSetRefusesAnEnrollmentThatCannotRoute(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		change func(t *testing.T, f routingFixture, enrollment domain.ClientEnrollmentID)
		named  domain.ClientEnrollmentID
		want   error
	}{
		"unknown": {named: "not-enrolled/claude_code", want: domain.ErrRoutingPolicyEnrollmentUnknown},
		"disabled identity": {
			change: func(t *testing.T, f routingFixture, enrollment domain.ClientEnrollmentID) {
				identity := f.identity(t, enrollment)
				identity.Enabled = false
				f.withStore(t, func(st *store.Store) error {
					return st.Write(t.Context(), func(tx *store.WriteTx) error {
						return tx.RecordAuthIdentity(t.Context(), identity, time.Now().UTC())
					})
				})
			},
			want: domain.ErrRoutingPolicyEnrollmentDisabled,
		},
		"unserved catalog": {
			change: func(t *testing.T, f routingFixture, enrollment domain.ClientEnrollmentID) {
				f.withStore(t, func(st *store.Store) error {
					return st.Write(t.Context(), func(tx *store.WriteTx) error {
						return tx.RecordDiscoveredModelCatalog(t.Context(), domain.DiscoveredModelCatalog{
							EnrollmentID: enrollment, HarnessBuild: "2.1.0 (Claude Code)", ObservedAt: time.Now().UTC(),
							Models: []domain.DiscoveredModel{{
								ID: "claude-not-the-lineup", DisplayName: "Other", EffortLevels: []string{},
							}},
						})
					})
				})
			},
			want: domain.ErrRoutingPolicyEnrollmentUnserved,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			f := newRoutingFixture(t)
			named := tc.named
			if named == "" {
				named = f.lineup(t, domain.RoleImplementer).EnrollmentID
				tc.change(t, f, named)
			}
			_, err := f.set(ctx, "-order", "implementer="+string(named))
			if !errors.Is(err, tc.want) {
				t.Fatalf("set = %v, want %v", err, tc.want)
			}
			if !strings.Contains(err.Error(), "role implementer enrollment 1") || strings.Contains(err.Error(), string(named)) {
				t.Fatalf("refusal = %v, want the role and position without the enrollment", err)
			}
			if output, err := runRouting(ctx, "show", "-db", f.dbPath); err != nil || strings.TrimSpace(output) != "null" {
				t.Fatalf("show after a refusal = %q, %v", output, err)
			}
		})
	}
}

// TestRoutingSetRecordsADrainFirstAccount: -drain-first names the pool of an
// ordered enrollment, a set without it clears the account as a new
// revision, and an account no ordered enrollment is on is refused and writes
// nothing, including the pool of an enrollment the order leaves out.
func TestRoutingSetRecordsADrainFirstAccount(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newRoutingFixture(t)
	enrollment := f.lineup(t, domain.RoleImplementer).EnrollmentID
	pool := f.identity(t, enrollment).EffectiveUsagePool()
	unordered := f.identity(t, f.lineup(t, domain.RoleReviewer).EnrollmentID).EffectiveUsagePool()
	if unordered == pool {
		t.Fatalf("the reviewer shares the implementer's pool %s", pool)
	}
	set := func(extra ...string) (string, error) {
		return f.set(ctx, append([]string{"-order", "implementer=" + string(enrollment)}, extra...)...)
	}

	output, err := set("-drain-first", string(pool))
	if err != nil {
		t.Fatal(err)
	}
	named := decodeRoutingSet(t, output)
	if !named.Recorded || named.Policy.Revision != 1 || named.Policy.LineupRevision != f.revision ||
		named.Policy.DrainFirstAccount == nil || *named.Policy.DrainFirstAccount != pool {
		t.Fatalf("named = %+v", named)
	}
	if output, err = set("-drain-first", string(pool)); err != nil {
		t.Fatal(err)
	}
	if repeat := decodeRoutingSet(t, output); repeat.Recorded || repeat.Policy.Revision != 1 {
		t.Fatalf("repeat = %+v", repeat)
	}
	for _, account := range []string{"pool-nobody-is-on", string(unordered)} {
		if _, err := set("-drain-first", account); !errors.Is(err, domain.ErrRoutingPolicyDrainFirstUnknown) {
			t.Fatalf("account %s = %v, want %v", account, err, domain.ErrRoutingPolicyDrainFirstUnknown)
		}
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
		"bad order":  {[]string{"set", "-db", "x", "-order", "implementer"}, "want role=enrollment1,enrollment2"},
		"no agents":  {[]string{"set", "-db", "x", "-order", "implementer="}, "want role=enrollment1,enrollment2"},
		"empty id":   {[]string{"set", "-db", "x", "-order", "implementer=a,,b"}, "no empty enrollment id"},
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
	t.Parallel()
	// The adopt fixture's store is a seeded template store, which a daemon
	// opens, and holds the lineup's enrollments.
	f := newRoutingFixture(t)
	enrollment := string(f.lineup(t, domain.RoleImplementer).EnrollmentID)
	dbPath := f.dbPath
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

	output, err := f.set(ctx, "-order", "implementer="+enrollment)
	if err != nil {
		t.Fatal(err)
	}
	if result := decodeRoutingSet(t, output); !result.Recorded || result.Policy.Revision != 1 {
		t.Fatalf("set = %+v", result)
	}
	output, err = f.set(ctx, "-order", "implementer="+enrollment)
	if err != nil {
		t.Fatal(err)
	}
	if repeat := decodeRoutingSet(t, output); repeat.Recorded || repeat.Policy.Revision != 1 {
		t.Fatalf("repeat = %+v", repeat)
	}
	// The daemon's refusal reaches the operator without the enrollment.
	if _, err := f.set(ctx, "-order", "implementer=not-enrolled/claude_code"); err == nil ||
		!strings.Contains(err.Error(), "role implementer enrollment 1") || strings.Contains(err.Error(), "not-enrolled") {
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
	if err := h.Store.Read(t.Context(), func(tx *store.ReadTx) error {
		policy, err := tx.CurrentRoutingPolicy(t.Context())
		if err == nil && policy.Revision != 1 {
			t.Errorf("running daemon store revision = %d, want 1", policy.Revision)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
