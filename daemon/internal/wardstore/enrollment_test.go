package wardstore_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
	"github.com/freeside-ai/freeside/daemon/internal/wardstore"
)

var enrollmentTestAt = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

// fixedCodexRefresher answers every refresh with the same rotated tokens, so
// two enrollments at one fixed clock write byte-identical stores.
type fixedCodexRefresher struct{}

func (fixedCodexRefresher) RefreshCodexAuth(context.Context, string) (ward.CodexAuthRefreshTokens, error) {
	return ward.CodexAuthRefreshTokens{
		IDToken: "rotated-id", AccessToken: codexTestJWT(enrollmentTestAt.Add(4 * time.Hour)),
		RefreshToken: "rotated-refresh",
	}, nil
}

func codexTestJWT(expires time.Time) string {
	payload, _ := json.Marshal(map[string]int64{"exp": expires.Unix()})
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func codexTestAuth(t *testing.T, account string) []byte {
	t.Helper()
	tokens := map[string]any{
		"id_token": "operator-id", "access_token": codexTestJWT(enrollmentTestAt.Add(30 * time.Minute)),
		"refresh_token": "operator-refresh",
	}
	if account != "" {
		tokens["account_id"] = account
	}
	body, err := json.Marshal(map[string]any{
		"OPENAI_API_KEY": nil, "tokens": tokens,
		"last_refresh": enrollmentTestAt.Add(-time.Hour).Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

type codexEnrollmentRig struct {
	st        *store.Store
	adapters  wardstore.Adapters
	storePath string
	cfg       ward.CodexAuthEnrollmentConfig
}

func newCodexEnrollmentRig(t *testing.T, account string, request *ward.CodexClientEnrollmentRequest) codexEnrollmentRig {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(root, "freeside.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	adapters, err := wardstore.New(st)
	if err != nil {
		t.Fatal(err)
	}
	inputRoot, storeRoot := filepath.Join(root, "input"), filepath.Join(root, "store")
	for _, dir := range []string{inputRoot, storeRoot} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	inputPath, storePath := filepath.Join(inputRoot, "auth.json"), filepath.Join(storeRoot, "auth.json")
	if err := os.WriteFile(inputPath, codexTestAuth(t, account), 0o600); err != nil {
		t.Fatal(err)
	}
	return codexEnrollmentRig{
		st: st, adapters: *adapters, storePath: storePath,
		cfg: ward.CodexAuthEnrollmentConfig{
			InputRoot: inputRoot, InputFile: inputPath, AuthStoreRoot: storeRoot, AuthStorePath: storePath,
			AuthIdentityID: "codex-primary", ProjectID: "project-1", Enrollment: request,
			Journal: adapters.Enrollment, AuthStoreLeaser: adapters.Leaser,
			AuthRefresher: fixedCodexRefresher{}, Now: func() time.Time { return enrollmentTestAt },
		},
	}
}

type codexEnrollmentState struct {
	identity   domain.AuthIdentity
	journal    store.CodexReenrollmentJournal
	item       domain.AttentionItem
	storeBytes []byte
}

func (r codexEnrollmentRig) state(t *testing.T, result ward.CodexAuthEnrollmentResult) codexEnrollmentState {
	t.Helper()
	ctx := context.Background()
	var state codexEnrollmentState
	if err := r.st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if state.identity, err = tx.GetAuthIdentity(ctx, r.cfg.AuthIdentityID); err != nil {
			return err
		}
		var found bool
		if state.journal, found, err = tx.LatestCodexReenrollmentJournal(ctx, r.cfg.AuthIdentityID); err != nil || !found {
			return errors.Join(err, errors.New("no journal"))
		}
		state.item, err = tx.GetAttentionItem(ctx, result.AttentionItemID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(r.storePath)
	if err != nil {
		t.Fatal(err)
	}
	state.storeBytes = body
	return state
}

// TestCodexAuthAddMatchesEnrollCodex is the acceptance round trip: `auth add`
// for codex_cli leaves exactly the state enroll-codex leaves (identity,
// verified journal, rotated store bytes, recovery item) and adds only the
// account and cost-owner facts, one enrollment, and generation one bound to
// the verified store.
func TestCodexAuthAddMatchesEnrollCodex(t *testing.T) {
	ctx := context.Background()
	legacy := newCodexEnrollmentRig(t, "acct-fixture-0001", nil)
	legacyResult, err := ward.EnrollCodexAuth(ctx, legacy.cfg)
	if err != nil {
		t.Fatalf("enroll-codex sequence: %v", err)
	}
	request := &ward.CodexClientEnrollmentRequest{
		EnrollmentID: "codex-primary/codex_cli", Route: "openai-subscription", CostOwner: "operator",
	}
	added := newCodexEnrollmentRig(t, "acct-fixture-0001", request)
	addedResult, err := ward.EnrollCodexAuth(ctx, added.cfg)
	if err != nil {
		t.Fatalf("auth add sequence: %v", err)
	}
	if addedResult.EnrollmentID != request.EnrollmentID || legacyResult.EnrollmentID != "" {
		t.Fatalf("enrollment ids = %q, %q", legacyResult.EnrollmentID, addedResult.EnrollmentID)
	}

	want, got := legacy.state(t, legacyResult), added.state(t, addedResult)
	if !bytes.Equal(want.storeBytes, got.storeBytes) {
		t.Fatalf("stored auth differs:\n%s\n%s", want.storeBytes, got.storeBytes)
	}
	if got.identity.AccountBinding != "acct-fixture-0001" || got.identity.CostOwner != "operator" {
		t.Fatalf("auth add identity = %+v, want the account and cost owner bound", got.identity)
	}
	gotIdentity := got.identity
	gotIdentity.AccountBinding, gotIdentity.CostOwner = "", ""
	// Each rig has its own temporary store root.
	gotIdentity.Interim.AuthStoreVolume = want.identity.Interim.AuthStoreVolume
	if gotIdentity != want.identity {
		t.Fatalf("identity differs beyond account and cost owner:\n%+v\n%+v", want.identity, got.identity)
	}
	// The holder and marker are minted per run; everything else is the same
	// verified operation.
	wantJournal, gotJournal := want.journal, got.journal
	gotJournal.Holder, gotJournal.MarkerItemID = wantJournal.Holder, wantJournal.MarkerItemID
	if wantJournal.Terminal == nil || gotJournal.Terminal == nil ||
		*wantJournal.Terminal.AuthStoreDigest != *gotJournal.Terminal.AuthStoreDigest ||
		!wantJournal.Terminal.AccessTokenExpiresAt.Equal(*gotJournal.Terminal.AccessTokenExpiresAt) ||
		wantJournal.Terminal.Outcome != gotJournal.Terminal.Outcome ||
		wantJournal.LeaseFence != gotJournal.LeaseFence || !wantJournal.OpenedAt.Equal(gotJournal.OpenedAt) {
		t.Fatalf("journal differs:\n%+v\n%+v", want.journal, got.journal)
	}
	if want.item.Type != got.item.Type || want.item.Reason != got.item.Reason ||
		legacyResult.AttentionItemVersion != addedResult.AttentionItemVersion ||
		!got.item.Offers(domain.ActionResolveReenrollment) ||
		*want.item.CodexReenrollmentRecoveryBinding != *got.item.CodexReenrollmentRecoveryBinding {
		t.Fatalf("recovery item differs:\n%+v\n%+v", want.item, got.item)
	}

	if err := added.st.Read(ctx, func(tx *store.ReadTx) error {
		enrollments, err := tx.ListClientEnrollments(ctx, "codex-primary")
		if err != nil {
			return err
		}
		if len(enrollments) != 1 || enrollments[0].AuthMethod != domain.AuthMethodOAuth ||
			enrollments[0].AccountBinding != "acct-fixture-0001" || enrollments[0].Route != request.Route {
			t.Fatalf("enrollments = %+v", enrollments)
		}
		generation, err := tx.CurrentEnrollmentGeneration(ctx, request.EnrollmentID)
		if err != nil {
			return err
		}
		if generation.Ordinal != 1 || generation.StoreManifestDigest != addedResult.AuthStoreDigest ||
			generation.TokenExpiry == nil || !generation.TokenExpiry.Equal(addedResult.AccessTokenExpiresAt) ||
			generation.LeaseFence != addedResult.LeaseFence || generation.AuthStoreVolume != addedResult.AuthStorePath {
			t.Fatalf("generation = %+v, result = %+v", generation, addedResult)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := legacy.st.Read(ctx, func(tx *store.ReadTx) error {
		enrollments, err := tx.ListClientEnrollments(ctx, "codex-primary")
		if err == nil && len(enrollments) != 0 {
			t.Fatalf("enroll-codex recorded enrollments %+v", enrollments)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCodexAuthAddRefusesAuthWithoutAccountID(t *testing.T) {
	rig := newCodexEnrollmentRig(t, "", &ward.CodexClientEnrollmentRequest{
		EnrollmentID: "codex-primary/codex_cli", Route: "openai-subscription", CostOwner: "operator",
	})
	if _, err := ward.EnrollCodexAuth(context.Background(), rig.cfg); err == nil {
		t.Fatal("enrollment without an account id succeeded")
	}
	if _, err := os.Stat(rig.storePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused enrollment wrote the store: %v", err)
	}
	assertNoIdentity(t, rig.st, "codex-primary")
}

func TestCodexAuthAddRefusesASecondIdentityForOneAccount(t *testing.T) {
	ctx := context.Background()
	rig := newCodexEnrollmentRig(t, "acct-fixture-0001", &ward.CodexClientEnrollmentRequest{
		EnrollmentID: "codex-primary/codex_cli", Route: "openai-subscription", CostOwner: "operator",
	})
	if err := rig.st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordAuthIdentity(ctx, domain.AuthIdentity{
			ID: "codex-other", Provider: "openai", AccountBinding: "acct-fixture-0001",
			AuthStoreMutationLease: true, MaxParallelExecutions: 1, Enabled: true, CostOwner: "operator",
		}, enrollmentTestAt)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ward.EnrollCodexAuth(ctx, rig.cfg); !errors.Is(err, domain.ErrAccountBindingTaken) {
		t.Fatalf("second identity for one account = %v, want ErrAccountBindingTaken", err)
	}
	assertNoIdentity(t, rig.st, "codex-primary")
}

func TestClaudeEnrollmentBeginBindsIdentityEnrollmentAndLease(t *testing.T) {
	ctx := context.Background()
	st, adapters := openEnrollmentStore(t)
	identity, bootstrap := claudeEnrollmentFixture("claude-main", "acct-fixture-0002")
	lease, err := adapters.Claude.Begin(ctx, identity, bootstrap, "holder-1", enrollmentTestAt, enrollmentTestAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if lease.GenerationBinding == nil || *lease.GenerationBinding != bootstrap.Binding {
		t.Fatalf("lease binding = %+v, want the bootstrap binding", lease.GenerationBinding)
	}
	generation, err := adapters.Claude.AppendGeneration(ctx, domain.EnrollmentGeneration{
		EnrollmentID: bootstrap.Enrollment.ID, AuthStoreVolume: "claude-main-auth",
		StoreManifestDigest: bootstrap.Binding.StoreManifestDigest, LeaseFence: lease.Fence,
		AccountBinding: "acct-fixture-0002", RecordedAt: enrollmentTestAt,
	}, enrollmentTestAt)
	if err != nil || generation.Ordinal != 1 {
		t.Fatalf("append generation = %+v, %v", generation, err)
	}
	if err := adapters.Leaser.Release(ctx, identity.ID, "holder-1", lease.Fence, enrollmentTestAt); err != nil {
		t.Fatal(err)
	}
	later := enrollmentTestAt.Add(2 * time.Minute)
	if _, err := adapters.Claude.Begin(ctx, identity, bootstrap, "holder-2", later, later.Add(time.Minute)); !errors.Is(err, ward.ErrEnrollmentExists) {
		t.Fatalf("second bootstrap of an enrolled client = %v, want ErrEnrollmentExists", err)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		stored, err := tx.GetAuthIdentity(ctx, identity.ID)
		if err == nil && stored != identity {
			t.Fatalf("stored identity = %+v, want %+v", stored, identity)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// TestClaudeEnrollmentBeginRetriesAFailedBootstrap proves a bootstrap that
// never appended a generation leaves an enrollment a retry can reuse, rather
// than one that strands the client.
func TestClaudeEnrollmentBeginRetriesAFailedBootstrap(t *testing.T) {
	ctx := context.Background()
	_, adapters := openEnrollmentStore(t)
	identity, bootstrap := claudeEnrollmentFixture("claude-main", "acct-fixture-0002")
	lease, err := adapters.Claude.Begin(ctx, identity, bootstrap, "holder-1", enrollmentTestAt, enrollmentTestAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := adapters.Leaser.Release(ctx, identity.ID, "holder-1", lease.Fence, enrollmentTestAt); err != nil {
		t.Fatal(err)
	}
	retry := bootstrap
	retry.Binding.StoreManifestDigest = domain.Digest(contentaddr.Sum([]byte("retry-token")))
	later := enrollmentTestAt.Add(time.Minute)
	if _, err := adapters.Claude.Begin(ctx, identity, retry, "holder-2", later, later.Add(time.Minute)); err != nil {
		t.Fatalf("retry after failed bootstrap: %v", err)
	}
}

func TestClaudeEnrollmentBeginEnforcesIdentityBindings(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		seed   *domain.AuthIdentity
		mutate func(*domain.AuthIdentity, *ward.EnrollmentBootstrap)
		want   error
	}{
		"new identity without cost owner": {
			mutate: func(identity *domain.AuthIdentity, _ *ward.EnrollmentBootstrap) { identity.CostOwner = "" },
		},
		"account taken by another identity": {
			seed: &domain.AuthIdentity{
				ID: "claude-other", Provider: "claude", AccountBinding: "acct-fixture-0002",
				AuthStoreMutationLease: true, MaxParallelExecutions: 1, Enabled: true, CostOwner: "operator",
			},
			want: domain.ErrAccountBindingTaken,
		},
		"identity bound to another account": {
			seed: func() *domain.AuthIdentity {
				identity, _ := claudeEnrollmentFixture("claude-main", "acct-fixture-9999")
				return &identity
			}(),
			want: domain.ErrAccountBindingMismatch,
		},
		"enrollment binding differs from its identity": {
			mutate: func(_ *domain.AuthIdentity, bootstrap *ward.EnrollmentBootstrap) {
				bootstrap.Enrollment.AccountBinding = "acct-fixture-7777"
			},
			want: domain.ErrAccountBindingMismatch,
		},
		"existing identity with another cost owner": {
			seed: func() *domain.AuthIdentity {
				identity, _ := claudeEnrollmentFixture("claude-main", "acct-fixture-0002")
				identity.CostOwner = "someone-else"
				return &identity
			}(),
		},
		"existing ownerless identity without cost owner": {
			seed: func() *domain.AuthIdentity {
				identity, _ := claudeEnrollmentFixture("claude-main", "acct-fixture-0002")
				identity.CostOwner = ""
				return &identity
			}(),
			mutate: func(identity *domain.AuthIdentity, _ *ward.EnrollmentBootstrap) { identity.CostOwner = "" },
		},
		"existing identity with other fixed bindings": {
			seed: func() *domain.AuthIdentity {
				identity, _ := claudeEnrollmentFixture("claude-main", "")
				identity.Interim.AuthStoreVolume = "other-volume"
				return &identity
			}(),
			want: domain.ErrImmutableTransition,
		},
	} {
		t.Run(name, func(t *testing.T) {
			st, adapters := openEnrollmentStore(t)
			if tc.seed != nil {
				if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
					return tx.RecordAuthIdentity(ctx, *tc.seed, enrollmentTestAt)
				}); err != nil {
					t.Fatal(err)
				}
			}
			identity, bootstrap := claudeEnrollmentFixture("claude-main", "acct-fixture-0002")
			if tc.mutate != nil {
				tc.mutate(&identity, &bootstrap)
			}
			_, err := adapters.Claude.Begin(ctx, identity, bootstrap, "holder-1", enrollmentTestAt, enrollmentTestAt.Add(time.Minute))
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("Begin = %v, want refusal %v", err, tc.want)
			}
			if err := st.Read(ctx, func(tx *store.ReadTx) error {
				enrollments, err := tx.ListClientEnrollments(ctx, "claude-main")
				if err == nil && len(enrollments) != 0 {
					t.Fatalf("refused bootstrap recorded %+v", enrollments)
				}
				if errors.Is(err, store.ErrNotFound) {
					return nil
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestClaudeEnrollmentBeginBindsAnUnboundInterimIdentity proves a flag-era
// identity (no account, no cost owner) takes both on its first enrollment
// and keeps every other stored fact.
func TestClaudeEnrollmentBeginBindsAnUnboundInterimIdentity(t *testing.T) {
	ctx := context.Background()
	st, adapters := openEnrollmentStore(t)
	stored, _ := claudeEnrollmentFixture("claude-main", "")
	stored.CostOwner, stored.Enabled, stored.MaxParallelExecutions = "", false, 3
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordAuthIdentity(ctx, stored, enrollmentTestAt)
	}); err != nil {
		t.Fatal(err)
	}
	identity, bootstrap := claudeEnrollmentFixture("claude-main", "acct-fixture-0002")
	later := enrollmentTestAt.Add(time.Minute)
	if _, err := adapters.Claude.Begin(ctx, identity, bootstrap, "holder-1", later, later.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	want := stored
	want.AccountBinding, want.CostOwner = "acct-fixture-0002", "operator"
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.GetAuthIdentity(ctx, "claude-main")
		if err == nil && got != want {
			t.Fatalf("bound identity = %+v, want %+v", got, want)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// TestClaudeEnrollmentBeginEnablesAnIdentityWhenAsked covers the bootstrap's
// EnableIdentity: a disabled flag-era identity is enabled in the write that
// binds it, keeping every other stored fact, and a bootstrap the transaction
// refuses leaves it disabled and unbound.
func TestClaudeEnrollmentBeginEnablesAnIdentityWhenAsked(t *testing.T) {
	ctx := context.Background()
	stored, _ := claudeEnrollmentFixture("claude-main", "")
	stored.CostOwner, stored.Enabled, stored.MaxParallelExecutions = "", false, 3
	bound := stored
	bound.AccountBinding, bound.CostOwner, bound.Enabled = "acct-fixture-0002", "operator", true
	for name, tc := range map[string]struct {
		accountHolder bool
		wantErr       error
		want          domain.AuthIdentity
	}{
		"first enrollment":                  {want: bound},
		"account taken by another identity": {accountHolder: true, wantErr: domain.ErrAccountBindingTaken, want: stored},
	} {
		t.Run(name, func(t *testing.T) {
			st, adapters := openEnrollmentStore(t)
			if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
				if tc.accountHolder {
					if err := tx.RecordAuthIdentity(ctx, domain.AuthIdentity{
						ID: "claude-other", Provider: "claude", AccountBinding: "acct-fixture-0002",
						AuthStoreMutationLease: true, MaxParallelExecutions: 1, Enabled: true, CostOwner: "operator",
					}, enrollmentTestAt); err != nil {
						return err
					}
				}
				return tx.RecordAuthIdentity(ctx, stored, enrollmentTestAt)
			}); err != nil {
				t.Fatal(err)
			}
			identity, bootstrap := claudeEnrollmentFixture("claude-main", "acct-fixture-0002")
			bootstrap.EnableIdentity = true
			later := enrollmentTestAt.Add(time.Minute)
			_, err := adapters.Claude.Begin(ctx, identity, bootstrap, "holder-1", later, later.Add(time.Minute))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Begin = %v, want %v", err, tc.wantErr)
			}
			if err := st.Read(ctx, func(tx *store.ReadTx) error {
				got, err := tx.GetAuthIdentity(ctx, "claude-main")
				if err == nil && got != tc.want {
					t.Fatalf("identity = %+v, want %+v", got, tc.want)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestClaudeEnrollmentBeginRefusesEnablingAnEnrolledIdentity covers the
// store's gate on EnableIdentity: an identity whose enrollment holds a
// generation, disabled since, is not enabled by a bootstrap under another
// enrollment id. The bootstrap refuses and records nothing.
func TestClaudeEnrollmentBeginRefusesEnablingAnEnrolledIdentity(t *testing.T) {
	ctx := context.Background()
	st, adapters := openEnrollmentStore(t)
	identity, bootstrap := claudeEnrollmentFixture("claude-main", "acct-fixture-0002")
	lease, err := adapters.Claude.Begin(ctx, identity, bootstrap, "holder-1", enrollmentTestAt, enrollmentTestAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapters.Claude.AppendGeneration(ctx, domain.EnrollmentGeneration{
		EnrollmentID: bootstrap.Enrollment.ID, AuthStoreVolume: "claude-main-auth",
		StoreManifestDigest: bootstrap.Binding.StoreManifestDigest, LeaseFence: lease.Fence,
		AccountBinding: "acct-fixture-0002", RecordedAt: enrollmentTestAt,
	}, enrollmentTestAt); err != nil {
		t.Fatal(err)
	}
	if err := adapters.Leaser.Release(ctx, identity.ID, "holder-1", lease.Fence, enrollmentTestAt); err != nil {
		t.Fatal(err)
	}
	disabled := identity
	disabled.Enabled = false
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordAuthIdentity(ctx, disabled, enrollmentTestAt.Add(time.Minute))
	}); err != nil {
		t.Fatal(err)
	}

	second := bootstrap
	second.Enrollment.ID = "claude-main/second"
	second.Binding.EnrollmentID = second.Enrollment.ID
	second.EnableIdentity = true
	later := enrollmentTestAt.Add(2 * time.Minute)
	_, err = adapters.Claude.Begin(ctx, identity, second, "holder-2", later, later.Add(time.Minute))
	if err == nil || !strings.Contains(err.Error(), "is already enrolled") {
		t.Fatalf("enabling bootstrap of an enrolled identity = %v, want the already-enrolled refusal", err)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.GetAuthIdentity(ctx, identity.ID)
		if err != nil {
			return err
		}
		if got != disabled {
			t.Fatalf("identity = %+v, want it left disabled: %+v", got, disabled)
		}
		if _, err := tx.GetClientEnrollment(ctx, second.Enrollment.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("second enrollment = %v, want it unrecorded", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func openEnrollmentStore(t *testing.T) (*store.Store, *wardstore.Adapters) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	adapters, err := wardstore.New(st)
	if err != nil {
		t.Fatal(err)
	}
	return st, adapters
}

func claudeEnrollmentFixture(id domain.AuthIdentityID, account string) (domain.AuthIdentity, ward.EnrollmentBootstrap) {
	enrollmentID := domain.ClientEnrollmentID(string(id) + "/claude_code")
	return domain.AuthIdentity{
			ID: id, Provider: "claude", AccountBinding: account, AuthStoreMutationLease: true,
			MaxParallelExecutions: 1, Enabled: true, CostOwner: "operator",
			Interim: domain.InterimClientFacts{AuthStoreVolume: "claude-main-auth", RefreshStrategy: domain.RefreshOnDemand},
		}, ward.EnrollmentBootstrap{
			Enrollment: domain.ClientEnrollment{
				ID: enrollmentID, AuthIdentityID: id, HarnessClient: domain.HarnessClientClaudeCode,
				Route: "anthropic-subscription", AuthMethod: domain.AuthMethodSetupToken,
				CredentialMode:  domain.CredentialSubscriptionContained,
				RefreshStrategy: domain.RefreshExternal, SupportsReadOnlyAuthSnapshot: true,
				AccountBinding: account,
			},
			Binding: domain.LeaseGenerationBinding{
				EnrollmentID: enrollmentID, AuthStoreVolume: "claude-main-auth",
				StoreManifestDigest: domain.Digest(contentaddr.Sum([]byte("token"))),
			},
		}
}

func assertNoIdentity(t *testing.T, st *store.Store, id domain.AuthIdentityID) {
	t.Helper()
	ctx := context.Background()
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetAuthIdentity(ctx, id)
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("refused enrollment left identity %s: %v", id, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
