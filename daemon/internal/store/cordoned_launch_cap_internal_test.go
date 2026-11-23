package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

var cordonedCapAt = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

// cordonedCapStore records one task, run, attempt, identity, and pending
// launch intent per name, in order, so each task is submitted before the
// next one's. Each launch has its own identity, so its own pool slot, and
// the pool gate never decides a case here.
func cordonedCapStore(t *testing.T, names ...string) *Store {
	t.Helper()
	ctx := t.Context()
	st := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "cap.db"), Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: domain.NewCapabilitySnapshot(),
		},
		ApprovedCredentialModes: []domain.CredentialMode{domain.CredentialSubscriptionContained},
	})
	if err := st.Write(ctx, func(tx *WriteTx) error {
		return tx.RegisterProject(ctx, domain.Project{
			ID: "proj-1", Repo: originTestRepository, RepositoryID: originTestRepositoryID,
		})
	}); err != nil {
		t.Fatal(err)
	}
	for i, name := range names {
		if err := st.Write(ctx, func(tx *WriteTx) error {
			task, err := tx.GetOrCreateTask(ctx, "proj-1", domain.SpecificationSource{
				Kind: domain.SpecificationSourceIssueSubject,
				IssueSubject: &domain.IssueSubjectRef{
					Repo: originTestRepository, RepositoryID: originTestRepositoryID, IssueNumber: i + 1,
				},
			})
			if err != nil {
				return err
			}
			run, stage := capacityRun(name), domain.StageID("stage-"+name)
			if err := tx.PutRun(ctx, domain.Run{
				ID: run, ProjectID: "proj-1", TaskID: task.ID,
				SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
				Stages: []domain.Stage{{
					ID: stage, RunID: run, Name: "implementation",
					Attempts: []domain.Attempt{{
						ID: domain.AttemptID("attempt-" + name), StageID: stage, Number: 1,
						InvocationID: domain.InvocationID("inv-" + name),
					}},
				}},
			}); err != nil {
				return err
			}
			if err := tx.RecordAuthIdentity(ctx, domain.AuthIdentity{
				ID: domain.AuthIdentityID("auth-" + name), Provider: "claude", AuthStoreMutationLease: true,
				Interim: domain.InterimClientFacts{AuthStoreVolume: "provider-cred", RefreshStrategy: domain.RefreshOnDemand},
			}, cordonedCapAt); err != nil {
				return err
			}
			_, _, err = tx.EnqueueOutbox(ctx, "inv-"+name, string(domain.AgentInvocationRequestedKind), []byte("{}"))
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

// cordonedCapAdmission is name's attended admission on class.
func cordonedCapAdmission(t *testing.T, name string, class domain.RunnerBackendClass) domain.ExecutionAdmission {
	t.Helper()
	identity := domain.AuthIdentityID("auth-" + name)
	capabilities := domain.CordonedCapabilities()
	if class != domain.BackendCordonedHostProcess {
		capabilities = domain.NewCapabilitySnapshot(domain.CapDetachableWorkspace, domain.CapPostExitExport)
	}
	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: domain.InvocationID("inv-" + name), RunID: capacityRun(name),
		StageID: domain.StageID("stage-" + name), AttemptID: domain.AttemptID("attempt-" + name),
		Backend: string(class), Capabilities: capabilities,
		OperatingMode: domain.ModeAttendedDev, CredentialMode: domain.CredentialSubscriptionContained,
		EgressProfile: domain.EgressProviderOnly,
		ImageRef:      domain.ImageRef("ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("ab", 32)),
		SpecDigest:    "sha256:spec", PolicyDigest: "sha256:policy", InputDigest: "sha256:input",
		Base: domain.BaseRevision{
			Repo: originTestRepository, RepositoryID: originTestRepositoryID, BaseRef: "refs/heads/main", BaseSHA: "deadbeef",
		},
		Workspace: "ws-" + name, AuthIdentityID: &identity, AdmittedAt: cordonedCapAt,
	})
	if err != nil {
		t.Fatalf("NewExecutionAdmission %s: %v", name, err)
	}
	return admission
}

func admitCordonedCap(t *testing.T, st *Store, admission domain.ExecutionAdmission) error {
	t.Helper()
	return st.Write(t.Context(), func(tx *WriteTx) error {
		return tx.RecordExecutionAdmission(t.Context(), admission)
	})
}

func setCordonedCap(t *testing.T, st *Store, maxConcurrent int) {
	t.Helper()
	value, err := json.Marshal(domain.CordonedLaunchSettings{MaxConcurrent: maxConcurrent})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(t.Context(), func(tx *InternalTx) error {
		_, err := tx.AppendInstanceSettingsRevision(t.Context(), domain.InstanceSettingsInput{
			Section: domain.InstanceSectionCordonedLaunches, Value: value, SetBy: "cli:operator",
		}, cordonedCapAt)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func recordCordonedWait(t *testing.T, st *Store, name string) {
	t.Helper()
	if err := st.Write(t.Context(), func(tx *WriteTx) error {
		return tx.RecordLaunchWait(t.Context(), domain.InvocationID("inv-"+name), capacityRun(name),
			LaunchWaitCordonedLaunch, cordonedCapAt)
	}); err != nil {
		t.Fatal(err)
	}
}

func wantCordonedCapRefusal(t *testing.T, err error, when string) {
	t.Helper()
	if !errors.Is(err, domain.ErrCordonedLaunchCap) {
		t.Fatalf("%s: admission = %v, want ErrCordonedLaunchCap", when, err)
	}
}

// TestCordonedLaunchCapRefusesWithNoCapAndAtTheCap: with no cap set no
// cordoned launch is admitted; under a cap of one the first is, a second is
// refused while the first runs, and a recorded export frees its place. An
// exact replay of the running admission passes at the cap.
func TestCordonedLaunchCapRefusesWithNoCapAndAtTheCap(t *testing.T) {
	t.Parallel()
	st := cordonedCapStore(t, "1-first", "2-second")
	first := cordonedCapAdmission(t, "1-first", domain.BackendCordonedHostProcess)
	second := cordonedCapAdmission(t, "2-second", domain.BackendCordonedHostProcess)
	wantCordonedCapRefusal(t, admitCordonedCap(t, st, first), "no cap set")

	setCordonedCap(t, st, 1)
	if err := admitCordonedCap(t, st, first); err != nil {
		t.Fatalf("first launch under the cap: %v", err)
	}
	if err := admitCordonedCap(t, st, first); err != nil {
		t.Fatalf("replay of the running launch at the cap: %v", err)
	}
	wantCordonedCapRefusal(t, admitCordonedCap(t, st, second), "a second launch at a cap of one")

	export, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: first.InvocationID, AdmissionID: first.ID,
		ObservedBaseSHA: first.Base.BaseSHA, HeadSHA: "cafebabe",
		ManifestDigest: "sha256:manifest", RecordedAt: cordonedCapAt.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(t.Context(), func(tx *WriteTx) error {
		return tx.RecordExecutionExport(t.Context(), export)
	}); err != nil {
		t.Fatal(err)
	}
	if err := admitCordonedCap(t, st, second); err != nil {
		t.Fatalf("second launch once the first exported: %v", err)
	}
}

// TestCordonedLaunchCapGrantsInTaskSubmissionOrder: a place that frees goes
// to the older task's waiting launch, not a newer one that asks first, and
// the older launch's admission ends its wait.
func TestCordonedLaunchCapGrantsInTaskSubmissionOrder(t *testing.T) {
	t.Parallel()
	st := cordonedCapStore(t, "1-older", "2-newer")
	setCordonedCap(t, st, 1)
	recordCordonedWait(t, st, "1-older")
	newer := cordonedCapAdmission(t, "2-newer", domain.BackendCordonedHostProcess)
	wantCordonedCapRefusal(t, admitCordonedCap(t, st, newer), "a newer launch behind an older wait")

	if err := admitCordonedCap(t, st, cordonedCapAdmission(t, "1-older", domain.BackendCordonedHostProcess)); err != nil {
		t.Fatalf("older launch under the cap: %v", err)
	}
	if err := st.Read(t.Context(), func(tx *ReadTx) error {
		waiting, err := tx.LaunchWaiting(t.Context(), "inv-1-older")
		if err == nil && waiting {
			err = errors.New("the admitted launch still waits")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// TestCordonedLaunchCapCountsOnlyItsOwnClassAndWaits: a warded admission
// and a cordoned one that names no identity pass the gate with no cap set and
// an older cordoned wait ahead, and neither a running warded launch nor an
// older memory wait takes a cordoned place.
func TestCordonedLaunchCapCountsOnlyItsOwnClassAndWaits(t *testing.T) {
	t.Parallel()
	st := cordonedCapStore(t, "1-memory", "2-cordoned", "3-warded", "4-cordoned")
	recordCordonedWait(t, st, "2-cordoned")
	if err := admitCordonedCap(t, st, cordonedCapAdmission(t, "3-warded", domain.BackendFreshVMReadOnlyVolumeHandoff)); err != nil {
		t.Fatalf("warded launch with no cap set and a cordoned wait ahead: %v", err)
	}
	anonymous := cordonedCapAdmission(t, "5-anonymous", domain.BackendCordonedHostProcess)
	anonymous.AuthIdentityID = nil
	if err := st.Write(t.Context(), func(tx *WriteTx) error {
		return tx.RequireCordonedLaunchCap(t.Context(), anonymous)
	}); err != nil {
		t.Fatalf("cordoned launch with no identity, no cap set, and a cordoned wait ahead: %v", err)
	}
	if err := st.Write(t.Context(), func(tx *WriteTx) error {
		return tx.RecordLaunchWait(t.Context(), "inv-1-memory", capacityRun("1-memory"), LaunchWaitMemory, cordonedCapAt)
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Write(t.Context(), func(tx *WriteTx) error { return tx.LeaveLaunchWait(t.Context(), "inv-2-cordoned") }); err != nil {
		t.Fatal(err)
	}
	setCordonedCap(t, st, 1)
	if err := admitCordonedCap(t, st, cordonedCapAdmission(t, "4-cordoned", domain.BackendCordonedHostProcess)); err != nil {
		t.Fatalf("cordoned launch beside a running warded launch and an older memory wait: %v", err)
	}
}

// TestCordonedLaunchCapPassesARunningLaunchItReads: the gate passes an
// admission that already runs without a new decision, under a cap cleared
// since, as a supersession of one whose dispatch stands needs.
func TestCordonedLaunchCapPassesARunningLaunchItReads(t *testing.T) {
	t.Parallel()
	st := cordonedCapStore(t, "1-running")
	running := cordonedCapAdmission(t, "1-running", domain.BackendCordonedHostProcess)
	setCordonedCap(t, st, 1)
	if err := admitCordonedCap(t, st, running); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(t.Context(), func(tx *InternalTx) error {
		_, err := tx.AppendInstanceSettingsRevision(t.Context(), domain.InstanceSettingsInput{
			Section: domain.InstanceSectionCordonedLaunches, SetBy: "cli:operator",
		}, cordonedCapAt)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Write(t.Context(), func(tx *WriteTx) error {
		return tx.RequireCordonedLaunchCap(context.Background(), running)
	}); err != nil {
		t.Fatalf("running launch under a cleared cap: %v", err)
	}
}
