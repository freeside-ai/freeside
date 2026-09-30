package ward

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

//nolint:gosec // G101 false positive: a fixture token, not a credential.
const claudeEnrollmentTestToken = "sk-ant-oat01-fixture-token-value-0123456789abcdef"

// fakeClaudeEnrollmentStore records what the enrollment persisted and hands
// the leaser the lease Begin grants, so the deferred release matches it.
type fakeClaudeEnrollmentStore struct {
	leaser      *fakeLeaser
	identity    domain.AuthIdentity
	bootstrap   EnrollmentBootstrap
	generations []domain.EnrollmentGeneration
	beginErr    error
	appendErr   error
}

func (s *fakeClaudeEnrollmentStore) Begin(
	_ context.Context, identity domain.AuthIdentity, bootstrap EnrollmentBootstrap,
	holder domain.InvocationID, now, expiresAt time.Time,
) (domain.AuthStoreMutationLease, error) {
	if s.beginErr != nil {
		return domain.AuthStoreMutationLease{}, s.beginErr
	}
	s.identity, s.bootstrap = identity, bootstrap
	binding := bootstrap.Binding
	s.leaser.lease = domain.AuthStoreMutationLease{
		AuthIdentityID: identity.ID, Holder: holder, Fence: 7,
		AcquiredAt: now, ExpiresAt: expiresAt, GenerationBinding: &binding,
	}
	return s.leaser.lease, nil
}

func (s *fakeClaudeEnrollmentStore) AppendGeneration(
	_ context.Context, generation domain.EnrollmentGeneration, _ time.Time,
) (domain.EnrollmentGeneration, error) {
	if s.appendErr != nil {
		return domain.EnrollmentGeneration{}, s.appendErr
	}
	generation.Ordinal = len(s.generations) + 1
	s.generations = append(s.generations, generation)
	return generation, nil
}

type claudeEnrollmentFixture struct {
	rt     *fakeRuntime
	store  *fakeClaudeEnrollmentStore
	leaser *fakeLeaser
	cfg    ClaudeAuthEnrollmentConfig
	// seeded is the token file the simulated seeder copied onto the volume.
	seeded map[string][]byte
}

// newClaudeEnrollmentFixture wires a fake runtime whose seeder places the
// staged token on the volume and whose observer reports the setup-token
// manifest only once a nonempty token is there, as the real observer would.
func newClaudeEnrollmentFixture(t *testing.T) *claudeEnrollmentFixture {
	t.Helper()
	token, err := NewSetupToken([]byte(claudeEnrollmentTestToken))
	if err != nil {
		t.Fatal(err)
	}
	fx := &claudeEnrollmentFixture{rt: newFakeRuntime(t), leaser: &fakeLeaser{}, seeded: map[string][]byte{}}
	fx.store = &fakeClaudeEnrollmentStore{leaser: fx.leaser}
	var stagedToken []byte
	fx.rt.onCopyIntoContainer = func(_, hostDir, targetDir string) error {
		switch targetDir {
		case claudeCredentialStageDir:
			body, err := os.ReadFile(filepath.Join(hostDir, "token")) //nolint:gosec // test-owned staging dir
			if err != nil {
				return err
			}
			stagedToken = body
		case claudeCredentialReadyDir:
			fx.seeded[fx.cfg.Volume] = stagedToken
		}
		return nil
	}
	fx.rt.observerProof = func(_ string, proof []byte) []byte {
		if len(fx.seeded[fx.cfg.Volume]) == 0 {
			return append(proof, "cred_manifest=invalid\n"...)
		}
		return append(proof, "cred_manifest=setup_token\n"...)
	}
	base := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	fx.cfg = ClaudeAuthEnrollmentConfig{
		Token: token, AuthIdentityID: "claude-main", EnrollmentID: "claude-main/claude_code",
		Route: "anthropic-subscription", CostOwner: "operator", AccountBinding: "acct-fixture-1234",
		Volume: "freeside-claude-main-auth", Runtime: fx.rt, ExporterImage: "example.test/exporter@sha256:" + strings.Repeat("0", 64),
		Store: fx.store, AuthStoreLeaser: fx.leaser,
		Now: func() time.Time { return base },
	}
	return fx
}

func TestEnrollClaudeSetupTokenAuthorsTheVolumeAndRecordsGenerationOne(t *testing.T) {
	fx := newClaudeEnrollmentFixture(t)
	result, err := enrollClaudeInBubble(t, fx.cfg)
	if err != nil {
		t.Fatalf("EnrollClaudeSetupToken: %v", err)
	}
	digest := domain.Digest(contentaddr.Sum([]byte(claudeEnrollmentTestToken)))
	want := ClaudeAuthEnrollmentResult{
		AuthIdentityID: "claude-main", EnrollmentID: "claude-main/claude_code",
		AuthStoreVolume: fx.cfg.Volume, StoreManifestDigest: digest, Generation: 1,
	}
	if result != want {
		t.Fatalf("result = %+v, want %+v", result, want)
	}
	if got := string(fx.seeded[fx.cfg.Volume]); got != claudeEnrollmentTestToken {
		t.Fatalf("seeded token = %q, want the entered token", got)
	}
	if _, ok := fx.rt.vols[fx.cfg.Volume]; !ok {
		t.Fatal("enrollment did not leave the credential volume")
	}
	if len(fx.rt.ctrs) != 0 {
		t.Fatalf("enrollment left %d helper container(s)", len(fx.rt.ctrs))
	}
	if !fx.leaser.released {
		t.Fatal("enrollment did not release its lease")
	}

	identity := fx.store.identity
	if identity.Provider != "claude" || !identity.AuthStoreMutationLease ||
		identity.AccountBinding != fx.cfg.AccountBinding || identity.CostOwner != "operator" ||
		identity.Interim.AuthStoreVolume != fx.cfg.Volume ||
		identity.Interim.RefreshStrategy != domain.RefreshOnDemand {
		t.Fatalf("identity = %+v, want the interim Claude identity the flag path runs", identity)
	}
	enrollment := fx.store.bootstrap.Enrollment
	if enrollment.HarnessClient != domain.HarnessClientClaudeCode ||
		enrollment.AuthMethod != domain.AuthMethodSetupToken ||
		enrollment.CredentialMode != domain.CredentialSubscriptionContained ||
		enrollment.RefreshStrategy != domain.RefreshExternal ||
		enrollment.AccountBinding != fx.cfg.AccountBinding {
		t.Fatalf("enrollment = %+v", enrollment)
	}
	if len(fx.store.generations) != 1 {
		t.Fatalf("generations = %d, want 1", len(fx.store.generations))
	}
	generation := fx.store.generations[0]
	if generation.TokenExpiry != nil || generation.StoreManifestDigest != digest ||
		generation.LeaseFence != 7 || generation.AuthStoreVolume != fx.cfg.Volume {
		t.Fatalf("generation = %+v", generation)
	}
	if err := domain.ValidateEnrollmentGenerationBinding(enrollment, generation); err != nil {
		t.Fatalf("generation does not fit its enrollment: %v", err)
	}
}

// TestEnrollClaudeSetupTokenKeepsTheTokenOutOfEveryRuntimeSurface proves the
// token leaves the process only through the staged file: no container
// command, environment, label, mount, runtime call, result, or recorded
// identity carries it.
func TestEnrollClaudeSetupTokenKeepsTheTokenOutOfEveryRuntimeSurface(t *testing.T) {
	fx := newClaudeEnrollmentFixture(t)
	var specs []ContainerSpec
	fx.rt.onCreateContainer = func(spec ContainerSpec) error {
		specs = append(specs, spec)
		return nil
	}
	result, err := enrollClaudeInBubble(t, fx.cfg)
	if err != nil {
		t.Fatalf("EnrollClaudeSetupToken: %v", err)
	}
	if len(specs) < 2 {
		t.Fatalf("created %d container(s), want the seeder and the observer", len(specs))
	}
	surfaces := []any{result, fx.store.identity, fx.store.bootstrap, fx.store.generations, fx.rt.calls, fx.cfg}
	for _, spec := range specs {
		surfaces = append(surfaces, spec.Command, spec.Env, spec.Labels, spec.Mounts, spec.Name)
	}
	for _, surface := range surfaces {
		assertNoSetupToken(t, fmt.Sprintf("%+v %#v %x", surface, surface, surface))
		if encoded, err := json.Marshal(surface); err == nil {
			assertNoSetupToken(t, string(encoded))
		}
	}
}

func TestEnrollClaudeSetupTokenDeletesTheVolumeWhenTheManifestProofFails(t *testing.T) {
	fx := newClaudeEnrollmentFixture(t)
	fx.rt.observerProof = func(_ string, proof []byte) []byte {
		return append(proof, "cred_manifest=invalid\n"...)
	}
	_, err := enrollClaudeInBubble(t, fx.cfg)
	if err == nil || !strings.Contains(err.Error(), "manifest proof") {
		t.Fatalf("err = %v, want the manifest proof failure", err)
	}
	assertNoSetupToken(t, err.Error())
	assertClaudeEnrollmentLeftNothing(t, fx)
}

func TestEnrollClaudeSetupTokenDeletesTheVolumeWhenTheSeederFails(t *testing.T) {
	fx := newClaudeEnrollmentFixture(t)
	fx.rt.onCopyIntoContainer = func(string, string, string) error {
		return errors.New("copy refused")
	}
	_, err := enrollClaudeInBubble(t, fx.cfg)
	if err == nil || !strings.Contains(err.Error(), "copy credential input") {
		t.Fatalf("err = %v, want the seeder copy failure", err)
	}
	assertNoSetupToken(t, err.Error())
	assertClaudeEnrollmentLeftNothing(t, fx)
}

func TestEnrollClaudeSetupTokenDeletesTheVolumeWhenTheGenerationAppendFails(t *testing.T) {
	fx := newClaudeEnrollmentFixture(t)
	fx.store.appendErr = errors.New("lease fence moved")
	_, err := enrollClaudeInBubble(t, fx.cfg)
	if err == nil || !strings.Contains(err.Error(), "lease fence moved") {
		t.Fatalf("err = %v, want the append failure", err)
	}
	assertClaudeEnrollmentLeftNothing(t, fx)
}

// TestEnrollClaudeSetupTokenRefusesAnExistingVolume proves enrollment never
// authors over, or deletes, a volume it did not create: it may hold another
// identity's live credential.
func TestEnrollClaudeSetupTokenRefusesAnExistingVolume(t *testing.T) {
	fx := newClaudeEnrollmentFixture(t)
	fx.rt.vols[fx.cfg.Volume] = &fakeVol{created: "prior"}
	_, err := enrollClaudeInBubble(t, fx.cfg)
	if !errors.Is(err, ErrCredentialVolumeExists) {
		t.Fatalf("err = %v, want ErrCredentialVolumeExists", err)
	}
	if fx.rt.vols[fx.cfg.Volume] == nil || fx.rt.callIndex("create-volume "+fx.cfg.Volume) >= 0 ||
		fx.rt.callIndex("delete-volume "+fx.cfg.Volume) >= 0 {
		t.Fatalf("existing volume was touched; calls = %v", fx.rt.calls)
	}
	if fx.store.identity.ID != "" || len(fx.store.generations) != 0 {
		t.Fatalf("existing volume still recorded identity %+v", fx.store.identity)
	}
}

// TestEnrollClaudeSetupTokenRefusesInvalidRuntimeInputsBeforeRecording
// proves a volume name the runtime would refuse, or an exporter image without
// a digest pin, is rejected before Begin records the volume as the
// identity's fixed binding, so a rerun with valid input stays possible.
func TestEnrollClaudeSetupTokenRefusesInvalidRuntimeInputsBeforeRecording(t *testing.T) {
	for name, mutate := range map[string]func(*ClaudeAuthEnrollmentConfig){
		"volume over the runtime limit": func(cfg *ClaudeAuthEnrollmentConfig) {
			cfg.Volume = strings.Repeat("v", appleContainerIDLimit+1)
		},
		"tag-only exporter image": func(cfg *ClaudeAuthEnrollmentConfig) {
			cfg.ExporterImage = "example.test/exporter:latest"
		},
		"short exporter digest": func(cfg *ClaudeAuthEnrollmentConfig) {
			cfg.ExporterImage = "example.test/exporter@sha256:abc"
		},
	} {
		t.Run(name, func(t *testing.T) {
			fx := newClaudeEnrollmentFixture(t)
			mutate(&fx.cfg)
			if _, err := enrollClaudeInBubble(t, fx.cfg); err == nil {
				t.Fatal("enrollment accepted invalid runtime input")
			}
			if fx.store.identity.ID != "" || fx.rt.callCount() != 0 {
				t.Fatalf("invalid input recorded identity %+v or drove runtime calls %v",
					fx.store.identity, fx.rt.calls)
			}
		})
	}
}

// TestEnrollClaudeSetupTokenDeletesAVolumeThatFailsInspection proves a
// volume this run created is removed even when its first inspection fails,
// so a retry with the same name is not refused as an existing volume.
func TestEnrollClaudeSetupTokenDeletesAVolumeThatFailsInspection(t *testing.T) {
	fx := newClaudeEnrollmentFixture(t)
	fx.rt.onInspectVolume = func(string, VolumeSummary) (VolumeSummary, error) {
		return VolumeSummary{}, errors.New("inspect failed")
	}
	if _, err := enrollClaudeInBubble(t, fx.cfg); err == nil {
		t.Fatal("enrollment succeeded past a failed volume inspection")
	}
	assertClaudeEnrollmentLeftNothing(t, fx)
}

func TestEnrollClaudeSetupTokenStopsBeforeTheRuntimeWhenBeginFails(t *testing.T) {
	fx := newClaudeEnrollmentFixture(t)
	fx.store.beginErr = domain.ErrAccountBindingTaken
	_, err := enrollClaudeInBubble(t, fx.cfg)
	if !errors.Is(err, domain.ErrAccountBindingTaken) {
		t.Fatalf("err = %v, want the store refusal", err)
	}
	// Only the read-only existence check precedes Begin.
	if fx.rt.callCount() != 1 || fx.rt.callIndex("list-volumes") != 0 {
		t.Fatalf("refused enrollment drove runtime calls %v", fx.rt.calls)
	}
}

func TestSetupTokenNeverRendersItsValue(t *testing.T) {
	token, err := NewSetupToken([]byte(claudeEnrollmentTestToken))
	if err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d"} {
		rendered := fmt.Sprintf(verb, token)
		if rendered != setupTokenPlaceholder {
			t.Fatalf("%s rendered %q", verb, rendered)
		}
	}
	wrapped := fmt.Sprintf("%+v", struct{ Token SetupToken }{token})
	assertNoSetupToken(t, wrapped)
	encoded, err := json.Marshal(struct{ Token SetupToken }{token})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"Token":"[setup token]"}` {
		t.Fatalf("json = %s", encoded)
	}
}

func TestNewSetupTokenRefusesMalformedInputWithoutEchoingIt(t *testing.T) {
	long := "sk-ant-oat01-" + strings.Repeat("a", MaxSetupTokenBytes)
	cases := map[string]string{
		"empty":       "",
		"short":       "sk-ant-oat01-short",
		"long":        long,
		"api key":     "sk-ant-api03-" + strings.Repeat("b", 40),
		"inner space": "sk-ant-oat01-" + strings.Repeat("c", 20) + " " + strings.Repeat("c", 20),
		"newline":     "sk-ant-oat01-" + strings.Repeat("d", 40) + "\n",
		"non-ascii":   "sk-ant-oat01-" + strings.Repeat("e", 40) + "é",
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := NewSetupToken([]byte(input))
			if err == nil {
				t.Fatal("malformed token accepted")
			}
			if input != "" && strings.Contains(err.Error(), input) {
				t.Fatalf("error echoes the input: %v", err)
			}
		})
	}
}

// enrollClaudeInBubble runs the enrollment inside a synctest bubble: the
// seeder and observer waits poll on real timers, which the bubble advances
// without sleeping.
func enrollClaudeInBubble(t *testing.T, cfg ClaudeAuthEnrollmentConfig) (ClaudeAuthEnrollmentResult, error) {
	t.Helper()
	var (
		result ClaudeAuthEnrollmentResult
		err    error
	)
	synctest.Test(t, func(*testing.T) {
		result, err = EnrollClaudeSetupToken(context.Background(), cfg)
	})
	return result, err
}

func assertNoSetupToken(t *testing.T, text string) {
	t.Helper()
	for _, needle := range []string{
		claudeEnrollmentTestToken,
		fmt.Sprintf("%x", claudeEnrollmentTestToken),
		// The token's distinctive tail, so a truncated render is still caught.
		claudeEnrollmentTestToken[len(claudeEnrollmentTestToken)-16:],
	} {
		if strings.Contains(text, needle) {
			t.Fatalf("setup token leaked into %q", text)
		}
	}
	if bytes.Contains([]byte(text), []byte("fixture-token")) {
		t.Fatalf("setup token leaked into %q", text)
	}
}

func assertClaudeEnrollmentLeftNothing(t *testing.T, fx *claudeEnrollmentFixture) {
	t.Helper()
	if _, ok := fx.rt.vols[fx.cfg.Volume]; ok {
		t.Fatal("failed enrollment left the credential volume")
	}
	if len(fx.rt.ctrs) != 0 {
		t.Fatalf("failed enrollment left %d helper container(s)", len(fx.rt.ctrs))
	}
	if len(fx.store.generations) != 0 {
		t.Fatalf("failed enrollment appended %d generation(s)", len(fx.store.generations))
	}
	if !fx.leaser.released {
		t.Fatal("failed enrollment did not release its lease")
	}
}
