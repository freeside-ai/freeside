package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/advisory"
	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	inferencefake "github.com/freeside-ai/freeside/daemon/internal/inference/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
	"github.com/freeside-ai/freeside/daemon/internal/wardstore"
)

// Made-up tokens in the setup-token shape. The sentinel is the one the leak
// test searches every output for.
const (
	judgmentSentinelToken = "sk-ant-oat-JUDGMENT-SENTINEL-0123456789abcdefghijklmnopq" //nolint:gosec // G101: test fixture
	judgmentSecondToken   = "sk-ant-oat-JUDGMENT-SECOND-0123456789abcdefghijklmnopqrs" //nolint:gosec // G101: test fixture
	judgmentRotatedToken  = "sk-ant-oat-JUDGMENT-ROTATED-0123456789abcdefghijklmnopqr" //nolint:gosec // G101: test fixture
	judgmentSecondVolume  = "freeside-claude-second-auth"
)

func judgmentTestToken(t *testing.T, value string) ward.SetupToken {
	t.Helper()
	token, err := ward.NewSetupToken([]byte(value))
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// fixedJudgmentCredentials is a credential source that answers every read
// with one token, for a test whose subject is not the credential.
func fixedJudgmentCredentials(t *testing.T) *judgmentCredentials {
	t.Helper()
	token := judgmentTestToken(t, judgmentSentinelToken)
	return newJudgmentCredentials(func(context.Context, domain.ClientEnrollmentID, int) (ward.SetupToken, error) {
		return token, nil
	}, nil)
}

// judgmentVolumes stands in for the runtime: it answers a volume read by
// volume name and counts the reads.
type judgmentVolumes struct {
	mu      sync.Mutex
	answers map[string]func() (ward.SetupTokenRead, error)
	reads   map[string]int
}

func newJudgmentVolumes() *judgmentVolumes {
	return &judgmentVolumes{answers: map[string]func() (ward.SetupTokenRead, error){}, reads: map[string]int{}}
}

func (v *judgmentVolumes) read(_ context.Context, volume string) (ward.SetupTokenRead, error) {
	v.mu.Lock()
	v.reads[volume]++
	answer := v.answers[volume]
	v.mu.Unlock()
	if answer == nil {
		return ward.SetupTokenRead{}, fmt.Errorf("credential volume %q: %w", volume, ward.ErrCredentialStoreAbsent)
	}
	return answer()
}

func (v *judgmentVolumes) set(volume string, answer func() (ward.SetupTokenRead, error)) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.answers[volume] = answer
}

func (v *judgmentVolumes) count(volume string) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.reads[volume]
}

// secretRecordingDriver records the credential each call carried, by site.
type secretRecordingDriver struct {
	*inferencefake.Driver
	mu      sync.Mutex
	secrets map[string][]string
}

func (d *secretRecordingDriver) Complete(
	ctx context.Context, request inference.Request, secret inference.Secret,
) (inference.Response, error) {
	d.mu.Lock()
	d.secrets[request.SiteID] = append(d.secrets[request.SiteID], secret.Reveal())
	d.mu.Unlock()
	return d.Driver.Complete(ctx, request, secret)
}

func (d *secretRecordingDriver) last(site string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	seen := d.secrets[site]
	if len(seen) == 0 {
		return ""
	}
	return seen[len(seen)-1]
}

func (d *secretRecordingDriver) calls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, seen := range d.secrets {
		n += len(seen)
	}
	return n
}

// judgmentCredentialRig is an adopted tree and store whose judgment calls
// read their credential through the real hold path over fake volumes.
type judgmentCredentialRig struct {
	t        *testing.T
	st       *store.Store
	tree     agenttree.Tree
	revision domain.Digest
	sites    []inference.Site
	volumes  *judgmentVolumes
	driver   *secretRecordingDriver
	logs     *bytes.Buffer
	ledger   string
	// first is the enrollment adoption recorded.
	first domain.ClientEnrollment
}

func (r *judgmentCredentialRig) now() time.Time {
	return time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
}

func (r *judgmentCredentialRig) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func (r *judgmentCredentialRig) reader() wardstore.CredentialReader {
	return wardstore.CredentialReader{
		Store: r.st, ReadSetupToken: r.volumes.read, Now: func() time.Time { return time.Now().UTC() },
	}
}

func (r *judgmentCredentialRig) generation(id domain.ClientEnrollmentID) domain.EnrollmentGeneration {
	r.t.Helper()
	var generation domain.EnrollmentGeneration
	if err := r.st.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		generation, err = tx.CurrentEnrollmentGeneration(context.Background(), id)
		return err
	}); err != nil {
		r.t.Fatal(err)
	}
	return generation
}

// stores makes a volume answer with a token under the digest its
// enrollment's current generation recorded.
func (r *judgmentCredentialRig) stores(id domain.ClientEnrollmentID, value string) {
	r.t.Helper()
	generation := r.generation(id)
	token := judgmentTestToken(r.t, value)
	r.volumes.set(generation.AuthStoreVolume, func() (ward.SetupTokenRead, error) {
		return ward.SetupTokenRead{
			Token: token, TokenDigest: domain.Digest(contentaddr.Sum([]byte(value))),
			TreeDigest: generation.StoreManifestDigest,
		}, nil
	})
}

// client is a daemon's inference client: a restart is a new one, with a new
// credential source that remembers nothing.
func (r *judgmentCredentialRig) client(tree agenttree.Tree, health inference.HealthReporter) *inference.Client {
	r.t.Helper()
	client, err := inference.New(inference.Config{
		StatePath: r.ledger,
		Roles: judgmentRoles{
			st: r.st, tree: tree, revision: r.revision, runtime: judgmentRuntime{Driver: r.driver},
			credentials: newJudgmentCredentials(r.reader().Read, r.logger()),
		},
		Health: health, Sites: r.sites,
		Advisory: advisory.Unavailable(errors.New("unused")), Now: r.now,
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return client
}

func (r *judgmentCredentialRig) call(client *inference.Client, site inference.Site) inference.CallResult {
	r.t.Helper()
	fields := map[string]inference.InputField{}
	for _, policy := range site.Fields {
		fields[policy.Name] = inference.InputField{Value: "data", Sensitivity: policy.Sensitivity}
	}
	result, err := client.Call(context.Background(), site.ID, "project-1", "run-1", fields)
	if err != nil {
		r.t.Fatal(err)
	}
	return result
}

func (r *judgmentCredentialRig) site(role domain.RoleName) inference.Site {
	r.t.Helper()
	for _, site := range r.sites {
		if got, _ := domain.RoleForSite(site.ID); got == role {
			return site
		}
	}
	r.t.Fatalf("no site for role %s", role)
	return inference.Site{}
}

// appendGeneration re-enrolls: the enrollment's next generation, recorded
// under digest in the same volume.
func (r *judgmentCredentialRig) appendGeneration(enrollment domain.ClientEnrollment, digest domain.Digest) {
	r.t.Helper()
	ctx := context.Background()
	at := time.Now().UTC()
	if err := r.st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		current, err := tx.CurrentEnrollmentGeneration(ctx, enrollment.ID)
		if err != nil {
			return err
		}
		holder := domain.InvocationID("re-enroll-" + string(enrollment.AuthIdentityID))
		lease, err := tx.AcquireAuthStoreMutationLeaseBound(ctx, enrollment.AuthIdentityID, holder,
			&domain.LeaseGenerationBinding{
				EnrollmentID: enrollment.ID, AuthStoreVolume: current.AuthStoreVolume,
				StoreManifestDigest: current.StoreManifestDigest, Generation: current.Ordinal,
			}, at, at.Add(time.Minute))
		if err != nil {
			return err
		}
		if _, err := tx.AppendEnrollmentGeneration(ctx, domain.EnrollmentGeneration{
			EnrollmentID: enrollment.ID, AuthStoreVolume: current.AuthStoreVolume,
			StoreManifestDigest: digest, LeaseFence: lease.Fence,
			AccountBinding: current.AccountBinding, TokenExpiry: current.TokenExpiry, RecordedAt: at,
		}, at); err != nil {
			return err
		}
		return tx.ReleaseAuthStoreMutationLease(ctx, enrollment.AuthIdentityID, holder, lease.Fence, at)
	}); err != nil {
		r.t.Fatal(err)
	}
}

// enroll records another identity's enrollment at generation one, on the
// adopted enrollment's client and route, the way auth add records one: under
// the token's hash.
func (r *judgmentCredentialRig) enroll(
	id domain.AuthIdentityID, volume string, recorded domain.Digest,
) domain.ClientEnrollmentID {
	r.t.Helper()
	ctx := r.t.Context()
	adapters, err := wardstore.New(r.st)
	if err != nil {
		r.t.Fatal(err)
	}
	enrollment := r.first
	enrollment.ID = domain.ClientEnrollmentID(string(id) + "/" + string(enrollment.HarnessClient))
	enrollment.AuthIdentityID, enrollment.AccountBinding = id, string(id)+"@example.test"
	identity := domain.AuthIdentity{
		ID: id, Provider: "claude", AccountBinding: enrollment.AccountBinding, AuthStoreMutationLease: true,
		MaxParallelExecutions: 1, Enabled: true, CostOwner: "operator",
		Interim: domain.InterimClientFacts{
			AuthStoreVolume: volume, RefreshStrategy: domain.RefreshOnDemand, SupportsReadOnlyAuthSnapshot: true,
		},
	}
	at := time.Now().UTC()
	holder := domain.InvocationID("enroll-" + string(id))
	lease, err := adapters.Claude.Begin(ctx, identity, ward.EnrollmentBootstrap{
		Enrollment: enrollment,
		Binding: domain.LeaseGenerationBinding{
			EnrollmentID: enrollment.ID, AuthStoreVolume: volume, StoreManifestDigest: recorded,
		},
	}, holder, at, at.Add(time.Minute))
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := adapters.Claude.AppendGeneration(ctx, domain.EnrollmentGeneration{
		EnrollmentID: enrollment.ID, AuthStoreVolume: volume, StoreManifestDigest: recorded,
		LeaseFence: lease.Fence, AccountBinding: enrollment.AccountBinding, RecordedAt: at,
	}, at); err != nil {
		r.t.Fatal(err)
	}
	if err := adapters.Leaser.Release(ctx, id, holder, lease.Fence, at); err != nil {
		r.t.Fatal(err)
	}
	return enrollment.ID
}

func withJudgmentCredentialRig(t *testing.T, fn func(r *judgmentCredentialRig)) {
	t.Helper()
	f := newAuthAdoptFixture(t)
	_, patch, err := f.run(t, f.args())
	if err != nil {
		t.Fatalf("auth adopt: %v", err)
	}
	tree := loadAdoptedPatch(t, patch)
	source, ok := tree.Agent(agentbaseline.ClaudeCallAgentName)
	if !ok {
		t.Fatal("the adopted tree has no call agent")
	}
	f.withStore(t, func(st *store.Store) {
		r := &judgmentCredentialRig{
			t: t, st: st, tree: tree, revision: domain.Digest("sha256:" + strings.Repeat("4", 64)),
			volumes: newJudgmentVolumes(), logs: &bytes.Buffer{},
			driver: &secretRecordingDriver{Driver: inferencefake.New(), secrets: map[string][]string{}},
			ledger: filepath.Join(t.TempDir(), "ledger.json"),
		}
		// The publication author has no prompt file here, so it is off.
		for _, site := range judgmentTestSites() {
			if role, _ := domain.RoleForSite(site.ID); role != domain.RolePublicationAuthor {
				r.sites = append(r.sites, site)
			}
		}
		if err := st.Read(context.Background(), func(tx *store.ReadTx) error {
			var err error
			r.first, err = tx.GetClientEnrollment(context.Background(), domain.ClientEnrollmentID(source.Enrollment))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		r.stores(r.first.ID, judgmentSentinelToken)
		fn(r)
	})
}

// A call authenticates with the token stored for the enrollment generation
// its lineup line names: two roles on two enrollments carry two tokens, the
// daemon reads each generation's volume once, a new generation is read
// again, and the record says where the bytes came from.
func TestJudgmentCallsCarryTheirEnrollmentsToken(t *testing.T) {
	withJudgmentCredentialRig(t, func(r *judgmentCredentialRig) {
		// A second enrollment, with the task namer's line moved onto an
		// agent that names it.
		second := r.enroll("claude-second", judgmentSecondVolume,
			domain.Digest(contentaddr.Sum([]byte(judgmentSecondToken))))
		r.stores(second, judgmentSecondToken)
		tree := r.tree
		agent, _ := tree.Agent(agentbaseline.ClaudeCallAgentName)
		agent.Name, agent.Enrollment = "claude-call-second", string(second)
		tree.Agents = append(slices.Clone(tree.Agents), agent)
		digest, err := tree.AgentDigest(agent.Name)
		if err != nil {
			t.Fatal(err)
		}
		tree.Lineup = slices.Clone(tree.Lineup)
		moved := false
		for i, line := range tree.Lineup {
			if domain.RoleName(line.Key) == domain.RoleTaskNamer {
				tree.Lineup[i].Selection.AgentName, tree.Lineup[i].Selection.AgentDigest = agent.Name, digest
				moved = true
			}
		}
		if !moved {
			t.Fatal("the adopted lineup has no task namer line")
		}

		firstVolume := r.generation(r.first.ID).AuthStoreVolume
		health := newJudgmentRoleHealth(r.st, r.now, nil)
		client := r.client(tree, health)
		for _, check := range client.CheckRoles(context.Background()) {
			if check.Err != nil {
				t.Fatalf("role %s = %v\n%s", check.Role, check.Err, r.logs)
			}
		}
		for range 2 {
			for _, site := range r.sites {
				result := r.call(client, site)
				role, _ := domain.RoleForSite(site.ID)
				want := judgmentSentinelToken
				if role == domain.RoleTaskNamer {
					want = judgmentSecondToken
				}
				if result.Identity == nil || r.driver.last(site.ID) != want {
					t.Fatalf("%s (role %s) carried another token than its enrollment's: %+v", site.ID, role, result)
				}
			}
		}
		if first, other := r.volumes.count(firstVolume), r.volumes.count(judgmentSecondVolume); first != 1 || other != 1 {
			t.Fatalf("volume reads = %d and %d, want one per enrollment generation", first, other)
		}
		if items := judgmentRoleItems(t, r.st); len(items) != 0 {
			t.Fatalf("items with every role bound = %+v", items)
		}
		ledger, err := os.ReadFile(r.ledger)
		if err != nil {
			t.Fatal(err)
		}
		if got := bytes.Count(ledger, []byte(`"credential_source":"enrollment"`)); got != 2*len(r.sites) ||
			bytes.Contains(ledger, []byte("interim_flag")) ||
			!bytes.Contains(ledger, []byte(string(second))) || !bytes.Contains(ledger, []byte(string(r.first.ID))) {
			t.Fatalf("ledger names credential_source enrollment on %d of %d records", got, 2*len(r.sites))
		}

		// The first enrollment is re-enrolled. Its roles read the new
		// generation's token, once; the second enrollment's is not re-read.
		r.appendGeneration(r.first, domain.Digest(contentaddr.Sum([]byte(judgmentRotatedToken))))
		r.stores(r.first.ID, judgmentRotatedToken)
		for range 2 {
			for _, site := range r.sites {
				r.call(client, site)
				role, _ := domain.RoleForSite(site.ID)
				want := judgmentRotatedToken
				if role == domain.RoleTaskNamer {
					want = judgmentSecondToken
				}
				if got := r.driver.last(site.ID); got != want {
					t.Fatalf("%s after the re-enrollment carried another token than its generation's", site.ID)
				}
			}
		}
		if first, other := r.volumes.count(firstVolume), r.volumes.count(judgmentSecondVolume); first != 2 || other != 1 {
			t.Fatalf("volume reads after one re-enrollment = %d and %d, want 2 and 1", first, other)
		}
	})
}

// A role whose credential cannot be read is unbound: its sites return their
// fail-safe without reaching the driver, and it raises the role's item, the
// drift auditor's blocking. The failure is not remembered: once the store is
// readable the next call binds the role and resolves the item.
func TestJudgmentRoleIsUnboundWhileItsCredentialCannotBeRead(t *testing.T) {
	t.Parallel()
	const unbound = inference.FallbackRoleUnbound
	volume := adoptClaudeVolume
	failures := map[string]func(r *judgmentCredentialRig){
		"the volume is gone": func(r *judgmentCredentialRig) { r.volumes.set(volume, nil) },
		"the volume read fails": func(r *judgmentCredentialRig) {
			r.volumes.set(volume, func() (ward.SetupTokenRead, error) {
				return ward.SetupTokenRead{}, errors.New("stored credential is not a setup token")
			})
		},
		"the store is not the one the generation recorded": func(r *judgmentCredentialRig) {
			token := judgmentTestToken(r.t, judgmentSentinelToken)
			r.volumes.set(volume, func() (ward.SetupTokenRead, error) {
				return ward.SetupTokenRead{
					Token: token, TokenDigest: domain.Digest(contentaddr.Sum([]byte("other token"))),
					TreeDigest: domain.Digest(contentaddr.Sum([]byte("other tree"))),
				}, nil
			})
		},
	}
	for name, breakStore := range failures {
		t.Run(name, func(t *testing.T) {
			withJudgmentCredentialRig(t, func(r *judgmentCredentialRig) {
				breakStore(r)
				health := newJudgmentRoleHealth(r.st, r.now, nil)
				client := r.client(r.tree, health)
				auditor, classifier := r.site(domain.RoleDriftAuditor), r.site(domain.RoleFindingClassifier)
				for _, site := range []inference.Site{auditor, classifier, auditor} {
					result := r.call(client, site)
					if !result.Fallback || result.Reason != unbound || string(result.Output) != site.FailSafe ||
						result.Identity != nil {
						t.Fatalf("%s with an unreadable credential = %+v", site.ID, result)
					}
				}
				if r.driver.calls() != 0 {
					t.Fatal("a role with no credential reached the driver")
				}
				// Each call tried the read again.
				if got := r.volumes.count(volume); got != 3 {
					t.Fatalf("volume reads over three failing calls = %d, want 3", got)
				}
				items := judgmentRoleItems(t, r.st)
				if len(items) != 2 {
					t.Fatalf("items = %+v", items)
				}
				for role, item := range items {
					posture, impairs := domain.HealthPostureAdvisory, domain.ImpairedCapabilityNone
					if role == domain.RoleDriftAuditor {
						posture, impairs = domain.HealthPostureBlocking, domain.ImpairedCapabilityUnattendedAdmission
					}
					if item.Posture == nil || *item.Posture != posture || item.HealthDiagnostic == nil ||
						item.HealthDiagnostic.Impairs != impairs ||
						!strings.Contains(item.Reason, errJudgmentCredentialUnavailable.Error()) {
						t.Fatalf("item for %s = %+v", role, item)
					}
				}
				if !strings.Contains(r.logs.String(), "judgment credential not read") ||
					!strings.Contains(r.logs.String(), string(r.first.ID)) {
					t.Fatalf("the log does not name the failed read: %s", r.logs)
				}

				r.stores(r.first.ID, judgmentSentinelToken)
				for _, site := range []inference.Site{auditor, classifier} {
					if result := r.call(client, site); result.Identity == nil || r.driver.last(site.ID) != judgmentSentinelToken {
						t.Fatalf("%s after the store was repaired = %+v", site.ID, result)
					}
				}
				if items := judgmentRoleItems(t, r.st); len(items) != 0 {
					t.Fatalf("items after the roles bound = %+v", items)
				}
				if got := r.volumes.count(volume); got != 4 {
					t.Fatalf("volume reads after the repair = %d, want one more", got)
				}
			})
		})
	}
}

// A read that finds another generation current, or a live mutation lease,
// hands back no token either.
func TestJudgmentCredentialReadRefusesAStoreInMotion(t *testing.T) {
	t.Parallel()
	withJudgmentCredentialRig(t, func(r *judgmentCredentialRig) {
		ctx := context.Background()
		credentials := newJudgmentCredentials(r.reader().Read, r.logger())
		volume := r.generation(r.first.ID).AuthStoreVolume

		// The caller resolved generation 1 and a re-enrollment landed since.
		r.appendGeneration(r.first, domain.Digest(contentaddr.Sum([]byte(judgmentRotatedToken))))
		if token, err := credentials.token(ctx, r.first.ID, 1); !errors.Is(err, errJudgmentCredentialUnavailable) || token != "" {
			t.Fatalf("a superseded generation's read = %v", err)
		}
		if r.volumes.count(volume) != 0 {
			t.Fatal("a superseded generation's volume was read")
		}

		// A refresh or enrollment holds the store.
		at := time.Now().UTC()
		if err := r.st.WriteInternal(ctx, func(tx *store.InternalTx) error {
			_, err := tx.AcquireAuthStoreMutationLease(ctx, r.first.AuthIdentityID, "mutator", at, at.Add(time.Minute))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		r.stores(r.first.ID, judgmentRotatedToken)
		if token, err := credentials.token(ctx, r.first.ID, 2); !errors.Is(err, errJudgmentCredentialUnavailable) || token != "" {
			t.Fatalf("a read under a live mutation lease = %v", err)
		}
		if r.volumes.count(volume) != 0 {
			t.Fatal("a volume under a live mutation lease was read")
		}
		for _, cause := range []error{wardstore.ErrCredentialReadGenerationChanged, wardstore.ErrCredentialReadMutationLeaseLive} {
			if !strings.Contains(r.logs.String(), cause.Error()) {
				t.Errorf("the log does not name %q: %s", cause, r.logs)
			}
		}
	})
}

// The token reaches the driver and nothing else: not a log line, an error, a
// health item, a call record, or a preflight manifest, on the path that
// succeeds and on each that fails after the token was in hand.
func TestJudgmentCredentialStaysOutOfEveryOutput(t *testing.T) {
	t.Parallel()
	withJudgmentCredentialRig(t, func(r *judgmentCredentialRig) {
		ctx := context.Background()
		volume := r.generation(r.first.ID).AuthStoreVolume
		token := judgmentTestToken(t, judgmentSentinelToken)
		var outputs bytes.Buffer
		record := func(label string, value any) {
			t.Helper()
			body, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&outputs, "%s: %s\n%v\n%+v\n%#v\n", label, body, value, value, value)
		}
		health := newJudgmentRoleHealth(r.st, r.now, r.logger())
		client := r.client(r.tree, health)
		pass := func(label string) {
			t.Helper()
			for _, check := range client.CheckRoles(ctx) {
				record(label+" check", check)
				if check.Err != nil {
					record(label+" check error", check.Err.Error())
				}
			}
			for _, site := range r.sites {
				record(label+" result", r.call(client, site))
			}
			record(label+" items", judgmentRoleItems(t, r.st))
			manifest := compositionManifest{
				Status: compositionPassed, Checks: []compositionCheck{{Name: "judgment_roles", Status: compositionNotRun}},
			}
			checks, err := inference.CheckRoles(ctx, judgmentRoles{
				st: r.st, tree: r.tree, revision: r.revision, runtime: judgmentRuntime{Driver: r.driver},
				credentials: newJudgmentCredentials(r.reader().ReadUnheld, r.logger()),
			}, judgmentSites(inference.Budget{}), r.now())
			evaluateJudgmentRoles(&manifest, checks, err)
			record(label+" preflight manifest", manifest)
		}

		// The token is read and every role binds.
		pass("bound")
		if r.driver.calls() != len(r.sites) || r.driver.last(r.sites[0].ID) != judgmentSentinelToken {
			t.Fatalf("the sentinel did not reach the driver: %d calls", r.driver.calls())
		}

		// The token is read and then refused: the store is not the one the
		// generation recorded, on a daemon that holds nothing yet.
		r.volumes.set(volume, func() (ward.SetupTokenRead, error) {
			return ward.SetupTokenRead{
				Token: token, TokenDigest: domain.Digest(contentaddr.Sum([]byte("other token"))),
				TreeDigest: domain.Digest(contentaddr.Sum([]byte("other tree"))),
			}, nil
		})
		client = r.client(r.tree, health)
		before := r.driver.calls()
		pass("digest mismatch")
		// The token is read while a re-enrollment lands under it.
		r.volumes.set(volume, func() (ward.SetupTokenRead, error) {
			r.appendGeneration(r.first, domain.Digest(contentaddr.Sum([]byte(judgmentSentinelToken))))
			return ward.SetupTokenRead{
				Token: token, TokenDigest: domain.Digest(contentaddr.Sum([]byte(judgmentSentinelToken))),
			}, nil
		})
		reader := r.reader()
		if stored, err := reader.ReadUnheld(ctx, r.first.ID, r.generation(r.first.ID).Ordinal); err == nil {
			t.Fatal("a read that a re-enrollment landed under returned a token")
		} else {
			record("generation changed", err.Error())
			record("generation changed token", stored)
		}
		if r.driver.calls() != before {
			t.Fatal("a refused credential reached the driver")
		}
		record("token", token)
		record("secret", inference.Secret(judgmentSentinelToken))
		record("held", heldJudgmentCredential{ordinal: 1, Token: judgmentSentinelToken})
		r.logger().Info("held", "held", heldJudgmentCredential{ordinal: 1, Token: judgmentSentinelToken})
		// A source that holds the token, rendered whole.
		source := newJudgmentCredentials(func(context.Context, domain.ClientEnrollmentID, int) (ward.SetupToken, error) {
			return token, nil
		}, nil)
		if held, err := source.token(ctx, r.first.ID, 1); err != nil || held != judgmentSentinelToken {
			t.Fatalf("the source does not hold the sentinel: %v", err)
		}
		record("source", source)
		r.logger().Info("source", "source", source)
		slog.New(slog.NewJSONHandler(r.logs, nil)).Info("source", "source", source,
			"held", heldJudgmentCredential{ordinal: 1, Token: judgmentSentinelToken})

		ledger, err := os.ReadFile(r.ledger)
		if err != nil {
			t.Fatal(err)
		}
		for name, output := range map[string][]byte{
			"a result, error, item, or manifest": outputs.Bytes(), "a log line": r.logs.Bytes(), "the call ledger": ledger,
		} {
			if len(output) == 0 {
				t.Errorf("%s: nothing was captured", name)
			}
			// A token type rendered where fmt cannot call its formatter
			// prints as its bytes, in decimal or hex.
			for form, rendered := range map[string]string{
				"clear":   judgmentSentinelToken,
				"decimal": strings.Trim(fmt.Sprintf("%d", []byte(judgmentSentinelToken)), "[]"),
				"hex":     fmt.Sprintf("%x", judgmentSentinelToken),
			} {
				if bytes.Contains(output, []byte(rendered)) {
					t.Errorf("the token is in %s, in %s", name, form)
				}
			}
		}
		if !strings.Contains(r.logs.String(), wardstore.ErrCredentialReadDigestMismatch.Error()) {
			t.Errorf("the failing paths did not run: %s", r.logs)
		}
	})
}

// The credential source remembers only a token: a nil source and a read that
// hands back nothing are both the fixed refusal.
func TestJudgmentCredentialsFailClosed(t *testing.T) {
	ctx := context.Background()
	const enrollment = domain.ClientEnrollmentID("claude-main/claude_code")
	var none *judgmentCredentials
	if token, err := none.token(ctx, enrollment, 1); !errors.Is(err, errJudgmentCredentialUnavailable) || token != "" {
		t.Fatalf("nil source = %v", err)
	}
	empty := newJudgmentCredentials(func(context.Context, domain.ClientEnrollmentID, int) (ward.SetupToken, error) {
		return ward.SetupToken{}, nil
	}, nil)
	if token, err := empty.token(ctx, enrollment, 1); !errors.Is(err, errJudgmentCredentialUnavailable) || token != "" {
		t.Fatalf("a read with no token = %v", err)
	}

	// A held token answers for its own generation only: when the read of a
	// newer one fails, the older token is not the fallback and is no longer
	// held. A stale request for an older generation leaves the current token
	// held.
	failing := false
	source := newJudgmentCredentials(func(context.Context, domain.ClientEnrollmentID, int) (ward.SetupToken, error) {
		if failing {
			return ward.SetupToken{}, errors.New("volume gone")
		}
		return judgmentTestToken(t, judgmentSentinelToken), nil
	}, nil)
	if token, err := source.token(ctx, enrollment, 1); err != nil || token != judgmentSentinelToken {
		t.Fatalf("first generation = %v", err)
	}
	failing = true
	if token, err := source.token(ctx, enrollment, 2); !errors.Is(err, errJudgmentCredentialUnavailable) || token != "" {
		t.Fatalf("a failed read of a newer generation returned a token or %v", err)
	}
	if len(source.held) != 0 {
		t.Fatal("a failed read of a newer generation left the replaced token held")
	}
	failing = false
	if token, err := source.token(ctx, enrollment, 2); err != nil || token != judgmentSentinelToken {
		t.Fatalf("second generation = %v", err)
	}
	failing = true
	if token, err := source.token(ctx, enrollment, 1); !errors.Is(err, errJudgmentCredentialUnavailable) || token != "" {
		t.Fatalf("a stale request for an older generation returned a token or %v", err)
	}
	if held, ok := source.held[enrollment]; !ok || held.ordinal != 2 {
		t.Fatal("a stale request for an older generation cost the current token")
	}
}

// Callers that miss at once share one read, and a caller waiting behind a
// read that does not end leaves at its own deadline with no token.
func TestJudgmentCredentialsShareOneReadAndWaitNoLongerThanTheirDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		const enrollment = domain.ClientEnrollmentID("claude-main/claude_code")
		release := make(chan struct{})
		reads := 0
		source := newJudgmentCredentials(func(context.Context, domain.ClientEnrollmentID, int) (ward.SetupToken, error) {
			reads++
			<-release
			return judgmentTestToken(t, judgmentSentinelToken), nil
		}, nil)
		tokens := make(chan inference.Secret, 2)
		for range cap(tokens) {
			go func() {
				token, err := source.token(ctx, enrollment, 1)
				if err != nil {
					t.Error(err)
				}
				tokens <- token
			}()
		}
		synctest.Wait()

		waiting, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		token, err := source.token(waiting, "claude-second/claude_code", 1)
		if !errors.Is(err, errJudgmentCredentialUnavailable) || token != "" {
			t.Fatalf("a caller past its deadline got a token or %v", err)
		}

		close(release)
		for range cap(tokens) {
			if token := <-tokens; token != judgmentSentinelToken {
				t.Fatal("a caller that shared the read got another token")
			}
		}
		if reads != 1 {
			t.Fatalf("callers that missed at once made %d reads, want 1", reads)
		}
	})
}
