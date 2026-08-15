package domain_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const (
	testImage                = "ghcr.io/freeside-ai/agent@sha256:abababababababababababababababababababababababababababababababab"
	testBackendConfiguration = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
)

func admissionInput() domain.ExecutionAdmissionInput {
	identity := domain.AuthIdentityID("auth-1")
	stageInputs, err := domain.NewStageInputSnapshot(domain.StageInputSnapshotInput{
		InputDigest:          stageDigest("1"),
		SpecificationDigest:  stageDigest("2"),
		PromptPackageDigest:  stageDigest("3"),
		PolicyDigest:         stageDigest("4"),
		PriorArtifactDigests: []domain.Digest{stageDigest("5"), stageDigest("6")},
		ImageInputDigests:    []domain.Digest{stageDigest("7")},
	})
	if err != nil {
		panic(err)
	}
	return domain.ExecutionAdmissionInput{
		InvocationID: "inv-1", RunID: "run-1", StageID: "stage-1", AttemptID: "attempt-1",
		Backend:                    "fresh_vm_read_only_volume_handoff",
		BackendConfigurationDigest: testBackendConfiguration,
		Capabilities: domain.CapabilitySnapshot{
			domain.CapPostExitExport, domain.CapDetachableWorkspace,
		},
		OperatingMode:  domain.ModeAttendedDev,
		CredentialMode: domain.CredentialSubscriptionContained,
		EgressProfile:  domain.EgressProviderOnly,
		ImageRef:       testImage,
		SpecDigest:     stageDigest("2"), PolicyDigest: stageDigest("4"), InputDigest: stageDigest("1"),
		StageInputs:    &stageInputs,
		Base:           domain.BaseRevision{Repo: "owner/repo", RepositoryID: 424242, BaseRef: "refs/heads/main", BaseSHA: "deadbeef"},
		Workspace:      "ws-1",
		AuthIdentityID: &identity,
		AdmittedAt:     time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

func mustAdmission(t *testing.T, in domain.ExecutionAdmissionInput) domain.ExecutionAdmission {
	t.Helper()
	a, err := domain.NewExecutionAdmission(in)
	if err != nil {
		t.Fatalf("NewExecutionAdmission: %v", err)
	}
	return a
}

// TestNewExecutionAdmissionCanonicalizes pins the write-once replay contract:
// the same admitted facts, however the caller ordered or located them, produce
// one body. A retried admission must converge on the stored row instead of
// colliding under a false immutable conflict (the #33 lesson).
func TestNewExecutionAdmissionCanonicalizes(t *testing.T) {
	in := admissionInput()
	first := mustAdmission(t, in)

	shuffled := admissionInput()
	shuffled.Capabilities = domain.CapabilitySnapshot{
		domain.CapDetachableWorkspace, domain.CapPostExitExport, domain.CapPostExitExport,
	}
	shuffled.AdmittedAt = in.AdmittedAt.In(time.FixedZone("UTC+2", 2*60*60))
	second := mustAdmission(t, shuffled)

	firstBody, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	secondBody, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstBody) != string(secondBody) {
		t.Fatalf("bodies diverge:\n%s\n%s", firstBody, secondBody)
	}
	if first.ID != second.ID {
		t.Fatalf("ids diverge: %s vs %s", first.ID, second.ID)
	}
}

// TestNewExecutionAdmissionDetachesCallerState proves the record cannot be
// rewritten through the values the caller passed in: an admitted class that
// followed its caller's later edits would not be a spawn-time snapshot.
func TestNewExecutionAdmissionDetachesCallerState(t *testing.T) {
	in := admissionInput()
	// A waiver only belongs to an unattended admission, which must also name
	// the trust profile revision it was admitted under.
	in.OperatingMode = domain.ModeUnattended
	caps := domain.CapabilitySnapshot{domain.CapDetachableWorkspace, domain.CapPostExitExport}
	identity := domain.AuthIdentityID("auth-1")
	profile := domain.Digest("sha256:profile-v1")
	waiver := domain.BackupEncryptionWaiver{RepositoryID: 424242, Reason: "supervised"}
	in.Capabilities, in.AuthIdentityID = caps, &identity
	in.TrustProfileDigest, in.BackupEncryptionWaiver = &profile, &waiver
	inputs := *in.StageInputs
	in.StageInputs = &inputs

	a := mustAdmission(t, in)
	caps[0] = domain.CapNetworklessExport
	identity = "auth-other"
	profile = "sha256:profile-v2"
	waiver.RepositoryID = 9
	in.StageInputs.PriorArtifactDigests[0] = "sha256:changed"

	if a.Capabilities[0] != domain.CapDetachableWorkspace {
		t.Errorf("capabilities followed the caller's slice: %v", a.Capabilities)
	}
	if *a.AuthIdentityID != "auth-1" {
		t.Errorf("auth identity followed the caller's pointer: %q", *a.AuthIdentityID)
	}
	if a.BackupEncryptionWaiver.RepositoryID != 424242 {
		t.Errorf("waiver followed the caller's pointer: %d", a.BackupEncryptionWaiver.RepositoryID)
	}
	if *a.TrustProfileDigest != "sha256:profile-v1" {
		t.Errorf("trust profile digest followed the caller's pointer: %q", *a.TrustProfileDigest)
	}
	if a.StageInputs.PriorArtifactDigests[0] != stageDigest("5") {
		t.Errorf("stage inputs followed the caller's slice: %q", a.StageInputs.PriorArtifactDigests[0])
	}
	if err := a.Validate(); err != nil {
		t.Errorf("detached record must still validate: %v", err)
	}
}

// TestExecutionAdmissionTamperFailsClosed is the reconstruction backstop: the
// identity is a content address, so a row edited in place resolves to a
// different address and fails on every boundary that re-runs Validate.
func TestExecutionAdmissionTamperFailsClosed(t *testing.T) {
	a := mustAdmission(t, admissionInput())
	cases := []struct {
		name  string
		edit  func(*domain.ExecutionAdmission)
		wantE error
	}{
		{"widened capabilities", func(a *domain.ExecutionAdmission) {
			a.Capabilities = domain.NewCapabilitySnapshot(domain.AllRunnerCapabilities...)
		}, domain.ErrAdmissionInconsistent},
		{
			// Upgrading the mode in place leaves the record structurally
			// invalid before the digest is even recomputed: unattended
			// running must name the trust profile it was admitted under.
			"upgraded operating mode", func(a *domain.ExecutionAdmission) {
				a.OperatingMode = domain.ModeUnattended
			}, domain.ErrEmptyField,
		},
		{"retargeted base", func(a *domain.ExecutionAdmission) {
			a.Base.BaseSHA = "0ther"
		}, domain.ErrAdmissionInconsistent},
		{"swapped image", func(a *domain.ExecutionAdmission) {
			a.ImageRef = domain.ImageRef("evil/agent@sha256:" + strings.Repeat("ef", 32))
		}, domain.ErrAdmissionInconsistent},
		{"retargeted prompt", func(a *domain.ExecutionAdmission) {
			a.StageInputs.PromptPackageDigest = stageDigest("f")
		}, domain.ErrStageInputDigestMismatch},
		{
			// A waiver pasted onto an attended record is refused by the mode
			// rule; the digest never gets a chance to disagree.
			"forged waiver", func(a *domain.ExecutionAdmission) {
				a.BackupEncryptionWaiver = &domain.BackupEncryptionWaiver{RepositoryID: 1, Reason: "forged"}
			}, domain.ErrWaiverModeMismatch,
		},
		{
			// A profile digest pasted onto a record that needs none.
			"forged trust profile", func(a *domain.ExecutionAdmission) {
				digest := domain.Digest("sha256:profile-v9")
				a.TrustProfileDigest = &digest
			}, domain.ErrTrustProfileInconsistent,
		},
		{"cleared id", func(a *domain.ExecutionAdmission) { a.ID = "" }, domain.ErrEmptyID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tampered := a
			if a.StageInputs != nil {
				inputs := *a.StageInputs
				inputs.PriorArtifactDigests = append(
					[]domain.Digest{}, a.StageInputs.PriorArtifactDigests...)
				inputs.ImageInputDigests = append(
					[]domain.Digest{}, a.StageInputs.ImageInputDigests...)
				tampered.StageInputs = &inputs
			}
			tc.edit(&tampered)
			if err := tampered.Validate(); !errors.Is(err, tc.wantE) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantE)
			}
		})
	}
}

func TestExecutionAdmissionValidate(t *testing.T) {
	cleanVerification := func(in *domain.ExecutionAdmissionInput) {
		in.EgressProfile = domain.EgressCleanVerification
		in.AuthIdentityID = nil
	}
	cases := []struct {
		name    string
		mutate  func(*domain.ExecutionAdmissionInput)
		wantErr error
	}{
		{"valid", func(*domain.ExecutionAdmissionInput) {}, nil},
		{"clean verification without an identity", cleanVerification, nil},
		{
			// §5.7's exception is about unattended backup health; a dev-loop
			// record may not claim it.
			"waiver under attended_dev", func(in *domain.ExecutionAdmissionInput) {
				in.BackupEncryptionWaiver = &domain.BackupEncryptionWaiver{RepositoryID: 424242, Reason: "1a.2"}
			}, domain.ErrWaiverModeMismatch,
		},
		{
			"unattended without a trust profile", func(in *domain.ExecutionAdmissionInput) {
				in.OperatingMode = domain.ModeUnattended
			}, domain.ErrEmptyField,
		},
		{
			"unattended without a backend configuration", func(in *domain.ExecutionAdmissionInput) {
				in.OperatingMode = domain.ModeUnattended
				profile := domain.Digest("sha256:profile-v1")
				in.TrustProfileDigest = &profile
				in.BackendConfigurationDigest = ""
			}, domain.ErrConformanceConfigurationUnbound,
		},
		{
			"trust profile named where none is required", func(in *domain.ExecutionAdmissionInput) {
				digest := domain.Digest("sha256:profile-v1")
				in.TrustProfileDigest = &digest
			}, domain.ErrTrustProfileInconsistent,
		},
		{"no invocation", func(in *domain.ExecutionAdmissionInput) { in.InvocationID = "" }, domain.ErrEmptyID},
		{"no attempt", func(in *domain.ExecutionAdmissionInput) { in.AttemptID = "" }, domain.ErrEmptyID},
		{"no backend", func(in *domain.ExecutionAdmissionInput) { in.Backend = "" }, domain.ErrEmptyField},
		{"unknown capability", func(in *domain.ExecutionAdmissionInput) {
			in.Capabilities = domain.CapabilitySnapshot{domain.RunnerCapability("supports_teleportation")}
		}, domain.ErrInvalidRunnerCapability},
		{"unknown operating mode", func(in *domain.ExecutionAdmissionInput) {
			in.OperatingMode = domain.OperatingMode("yolo")
		}, domain.ErrInvalidOperatingMode},
		{"zero credential mode", func(in *domain.ExecutionAdmissionInput) {
			in.CredentialMode = ""
		}, domain.ErrInvalidCredentialMode},
		{"zero egress profile", func(in *domain.ExecutionAdmissionInput) {
			in.EgressProfile = ""
		}, domain.ErrInvalidEgressProfile},
		{"tag-only image", func(in *domain.ExecutionAdmissionInput) {
			in.ImageRef = "ghcr.io/freeside-ai/agent:1.2.3"
		}, domain.ErrImageNotDigestPinned},
		{"short digest image", func(in *domain.ExecutionAdmissionInput) {
			in.ImageRef = domain.ImageRef("ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("ab", 16))
		}, domain.ErrImageNotDigestPinned},
		{"no policy digest", func(in *domain.ExecutionAdmissionInput) {
			in.PolicyDigest = ""
		}, domain.ErrEmptyField},
		{"unresolved base", func(in *domain.ExecutionAdmissionInput) {
			in.Base.BaseSHA = ""
		}, domain.ErrEmptyField},
		{"no workspace", func(in *domain.ExecutionAdmissionInput) { in.Workspace = "" }, domain.ErrEmptyField},
		{"provider egress without an identity", func(in *domain.ExecutionAdmissionInput) {
			in.AuthIdentityID = nil
		}, domain.ErrEmptyID},
		{"clean verification naming an identity", func(in *domain.ExecutionAdmissionInput) {
			in.EgressProfile = domain.EgressCleanVerification
		}, domain.ErrAuthIdentityInconsistent},
		{"waiver without a repository", func(in *domain.ExecutionAdmissionInput) {
			in.BackupEncryptionWaiver = &domain.BackupEncryptionWaiver{Reason: "why"}
		}, domain.ErrNonPositive},
		{"waiver without a reason", func(in *domain.ExecutionAdmissionInput) {
			in.BackupEncryptionWaiver = &domain.BackupEncryptionWaiver{RepositoryID: 3}
		}, domain.ErrEmptyField},
		{"no admitted_at", func(in *domain.ExecutionAdmissionInput) {
			in.AdmittedAt = time.Time{}
		}, domain.ErrMissingTimestamp},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := admissionInput()
			tc.mutate(&in)
			_, err := domain.NewExecutionAdmission(in)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("NewExecutionAdmission = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestLegacyUnboundUnattendedAdmissionRemainsReconstructable(t *testing.T) {
	t.Parallel()
	in := admissionInput()
	in.OperatingMode = domain.ModeUnattended
	profile := domain.Digest("sha256:" + strings.Repeat("77", 32))
	in.TrustProfileDigest = &profile
	admission := mustAdmission(t, in)

	// Admissions written before the configuration-binding encoding have no
	// field and resolve under v2. They remain readable audit history, but the
	// constructor above still refuses to mint another one.
	admission.BackendConfigurationDigest = ""
	id, err := admission.ComputeID()
	if err != nil {
		t.Fatalf("legacy ComputeID: %v", err)
	}
	admission.ID = id
	if err := admission.Validate(); err != nil {
		t.Fatalf("legacy unattended admission is not reconstructable: %v", err)
	}
}

// TestExecutionAdmissionRejectsNonUTC covers the decode path the constructor
// normalizes away: a literal or decoded record in another zone would give one
// instant two valid identities.
func TestExecutionAdmissionRejectsNonUTC(t *testing.T) {
	a := mustAdmission(t, admissionInput())
	a.AdmittedAt = a.AdmittedAt.In(time.FixedZone("UTC+2", 2*60*60))
	if err := a.Validate(); !errors.Is(err, domain.ErrTimestampNotUTC) {
		t.Fatalf("Validate() = %v, want %v", err, domain.ErrTimestampNotUTC)
	}
}

func TestLegacyExecutionAdmissionRemainsReconstructable(t *testing.T) {
	in := admissionInput()
	in.StageInputs = nil
	legacy := mustAdmission(t, in)
	if legacy.StageInputs != nil {
		t.Fatal("legacy admission unexpectedly carries stage inputs")
	}
	body, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	var reconstructed domain.ExecutionAdmission
	if err := json.Unmarshal(body, &reconstructed); err != nil {
		t.Fatal(err)
	}
	if err := reconstructed.Validate(); err != nil {
		t.Fatalf("legacy admission no longer reconstructs: %v", err)
	}
}

func TestAdmittedUnder(t *testing.T) {
	attended := mustAdmission(t, admissionInput())

	profile := domain.Digest("sha256:profile-v1")
	unattendedInput := admissionInput()
	unattendedInput.OperatingMode = domain.ModeUnattended
	unattendedInput.Capabilities = domain.NewCapabilitySnapshot(domain.AllRunnerCapabilities...)
	unattendedInput.TrustProfileDigest = &profile
	unattended := mustAdmission(t, unattendedInput)

	// The same run evaluated without backup-health evidence.
	unbackedInput := admissionInput()
	unbackedInput.OperatingMode = domain.ModeUnattended
	unbackedInput.Capabilities = domain.NewCapabilitySnapshot(domain.AllRunnerCapabilities...)
	unbackedInput.TrustProfileDigest = &profile
	unbacked := mustAdmission(t, unbackedInput)

	waivedInput := admissionInput()
	waivedInput.OperatingMode = domain.ModeUnattended
	waivedInput.Capabilities = domain.NewCapabilitySnapshot(domain.AllRunnerCapabilities...)
	waivedInput.TrustProfileDigest = &profile
	waivedInput.BackupEncryptionWaiver = &domain.BackupEncryptionWaiver{RepositoryID: 424242, Reason: "1a.2"}
	waived := mustAdmission(t, waivedInput)

	// A run against a different repository, carrying the waiver the operator
	// did configure: the number matches, the target does not.
	foreignInput := admissionInput()
	foreignInput.OperatingMode = domain.ModeUnattended
	foreignInput.Capabilities = domain.NewCapabilitySnapshot(domain.AllRunnerCapabilities...)
	foreignInput.TrustProfileDigest = &profile
	foreignInput.Base.Repo, foreignInput.Base.RepositoryID = "other/repo", 999
	foreignInput.BackupEncryptionWaiver = &domain.BackupEncryptionWaiver{RepositoryID: 424242, Reason: "1a.2"}
	foreignTarget := mustAdmission(t, foreignInput)

	floor := map[domain.OperatingMode]domain.CapabilitySnapshot{
		domain.ModeAttendedDev: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		domain.ModeUnattended:  domain.NewCapabilitySnapshot(domain.CapPostExitExport),
	}
	approved := []domain.CredentialMode{domain.CredentialSubscriptionContained}
	waiverID := int64(424242)
	backupHealth := healthyBackupHealth()

	cases := []struct {
		name    string
		record  domain.ExecutionAdmission
		policy  domain.AdmissionPolicy
		wantErr error
	}{
		{"admitted", attended, domain.AdmissionPolicy{Floors: floor, ApprovedCredentialModes: approved}, nil},
		{
			// An unconfigured floor is not an empty floor.
			"nil floors admit nothing", attended,
			domain.AdmissionPolicy{},
			domain.ErrUnknownAdmissionFloor,
		},
		{
			"mode without a floor", unattended,
			domain.AdmissionPolicy{Floors: map[domain.OperatingMode]domain.CapabilitySnapshot{
				domain.ModeAttendedDev: nil,
			}},
			domain.ErrUnknownAdmissionFloor,
		},
		{
			// The floor policy states now, not the floor at spawn.
			"floor raised since admission", attended,
			domain.AdmissionPolicy{Floors: map[domain.OperatingMode]domain.CapabilitySnapshot{
				domain.ModeAttendedDev: domain.NewCapabilitySnapshot(domain.CapWorkspaceSnapshot),
			}},
			domain.ErrCapabilityBelowFloor,
		},
		{
			"unattended with the full class", unattended,
			domain.AdmissionPolicy{
				Floors: floor, ApprovedCredentialModes: approved,
				BackupHealth: &backupHealth,
			},
			nil,
		},
		{
			"unattended with no backup health", unbacked,
			domain.AdmissionPolicy{Floors: floor, ApprovedCredentialModes: approved},
			domain.ErrBackupHealthUnavailable,
		},
		{
			// §5.7 requires networkless export of an unattended run, whatever
			// the configured floor says.
			"unattended without networkless export",
			mustAdmission(t, func() domain.ExecutionAdmissionInput {
				in := admissionInput()
				in.OperatingMode = domain.ModeUnattended
				in.TrustProfileDigest = &profile
				return in
			}()),
			domain.AdmissionPolicy{Floors: floor, ApprovedCredentialModes: approved},
			domain.ErrCapabilityBelowFloor,
		},
		{
			// §5.7 likewise requires the enforced provider-egress proof of an
			// unattended run (issue #327): a snapshot carrying everything but
			// it fails closed, whatever the configured floor says.
			"unattended without enforced provider egress",
			mustAdmission(t, func() domain.ExecutionAdmissionInput {
				in := admissionInput()
				in.OperatingMode = domain.ModeUnattended
				in.Capabilities = domain.NewCapabilitySnapshot(
					domain.CapDetachableWorkspace, domain.CapPostExitExport,
					domain.CapReadOnlyRemount, domain.CapNetworklessExport)
				in.TrustProfileDigest = &profile
				return in
			}()),
			domain.AdmissionPolicy{Floors: floor, ApprovedCredentialModes: approved},
			domain.ErrCapabilityBelowFloor,
		},
		{
			"historical waiver matching the operator's", waived,
			domain.AdmissionPolicy{
				Floors: floor, ApprovedCredentialModes: approved,
				BackupEncryptionWaiverRepositoryID: &waiverID,
				BackupHealth:                       &backupHealth,
			},
			nil,
		},
		{
			"historical waiver is inert under encrypted health", waived,
			domain.AdmissionPolicy{
				Floors: floor, ApprovedCredentialModes: approved,
				BackupHealth: &backupHealth,
			},
			nil,
		},
		{
			// The operator waived repository 424242, and this run targets 999:
			// the waiver must cover the repository the run actually runs
			// against, not merely exist.
			"waiver covering another target", foreignTarget,
			domain.AdmissionPolicy{Floors: floor, ApprovedCredentialModes: approved, BackupEncryptionWaiverRepositoryID: &waiverID},
			domain.ErrWaiverRepositoryMismatch,
		},
		{
			// §5.7 requires an approved containment of an unattended run, and
			// a correctly spelled mode is not an approved one.
			"unattended under an unapproved credential mode", unattended,
			domain.AdmissionPolicy{Floors: floor},
			domain.ErrCredentialModeNotApproved,
		},
		{
			// attended_dev is deliberately not held to the approved set: §5.7
			// admits the weaker class there.
			"attended under an unapproved credential mode", attended,
			domain.AdmissionPolicy{Floors: floor},
			nil,
		},
		{
			// The gate re-runs Validate first, so a malformed record cannot
			// slip through a caller that only asked about policy.
			"malformed record",
			domain.ExecutionAdmission{},
			domain.AdmissionPolicy{Floors: floor, ApprovedCredentialModes: approved},
			domain.ErrEmptyID,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := domain.AdmittedUnder(tc.record, tc.policy); !errors.Is(err, tc.wantErr) {
				t.Fatalf("AdmittedUnder = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestAdmittedUnderDoesNotMutateTheFloor pins that the plan-mandated
// unattended addition is applied to a copy: appending to a caller's snapshot
// in place could grow the stored policy map's slice.
func TestAdmittedUnderDoesNotMutateTheFloor(t *testing.T) {
	in := admissionInput()
	in.OperatingMode = domain.ModeUnattended
	in.Capabilities = domain.NewCapabilitySnapshot(domain.AllRunnerCapabilities...)
	profile := domain.Digest("sha256:profile-v1")
	in.TrustProfileDigest = &profile
	record := mustAdmission(t, in)

	configured := domain.NewCapabilitySnapshot(domain.CapPostExitExport, domain.CapDetachableWorkspace)
	backupHealth := healthyBackupHealth()
	policy := domain.AdmissionPolicy{
		Floors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeUnattended: configured,
		},
		ApprovedCredentialModes: []domain.CredentialMode{domain.CredentialSubscriptionContained},
		BackupHealth:            &backupHealth,
	}
	before := slices.Clone(configured)
	if err := domain.AdmittedUnder(record, policy); err != nil {
		t.Fatalf("AdmittedUnder: %v", err)
	}
	if !slices.Equal(policy.Floors[domain.ModeUnattended], before) {
		t.Fatalf("floor mutated to %v, want %v", policy.Floors[domain.ModeUnattended], before)
	}
}

func exportInput(a domain.ExecutionAdmission) domain.ExecutionExportInput {
	return domain.ExecutionExportInput{
		InvocationID: a.InvocationID, AdmissionID: a.ID,
		ObservedBaseSHA: a.Base.BaseSHA, HeadSHA: "cafebabe",
		ManifestDigest: "sha256:manifest",
		RecordedAt:     time.Date(2026, 1, 2, 4, 4, 5, 0, time.UTC),
	}
}

func TestExecutionExportValidate(t *testing.T) {
	a := mustAdmission(t, admissionInput())
	empty := domain.Digest("")
	cases := []struct {
		name    string
		mutate  func(*domain.ExecutionExportInput)
		wantErr error
	}{
		{"valid", func(*domain.ExecutionExportInput) {}, nil},
		{"no invocation", func(in *domain.ExecutionExportInput) { in.InvocationID = "" }, domain.ErrEmptyID},
		{"no admission", func(in *domain.ExecutionExportInput) { in.AdmissionID = "" }, domain.ErrEmptyID},
		{"no observed base", func(in *domain.ExecutionExportInput) { in.ObservedBaseSHA = "" }, domain.ErrEmptyField},
		{"no head", func(in *domain.ExecutionExportInput) { in.HeadSHA = "" }, domain.ErrEmptyField},
		{"no manifest", func(in *domain.ExecutionExportInput) { in.ManifestDigest = "" }, domain.ErrEmptyField},
		{"empty evidence digest", func(in *domain.ExecutionExportInput) {
			in.EvidenceManifestDigest = &empty
		}, domain.ErrEmptyField},
		{"no recorded_at", func(in *domain.ExecutionExportInput) { in.RecordedAt = time.Time{} }, domain.ErrMissingTimestamp},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := exportInput(a)
			tc.mutate(&in)
			_, err := domain.NewExecutionExport(in)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("NewExecutionExport = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestValidateExportBinding covers the join a foreign export would have to
// pass: same invocation, that admission's exact identity, and the base the
// workspace was observed at equal to the admitted base.
func TestValidateExportBinding(t *testing.T) {
	a := mustAdmission(t, admissionInput())
	valid, err := domain.NewExecutionExport(exportInput(a))
	if err != nil {
		t.Fatal(err)
	}
	if err := domain.ValidateExportBinding(a, valid); err != nil {
		t.Fatalf("matching export must bind: %v", err)
	}

	cases := []struct {
		name    string
		mutate  func(*domain.ExecutionExportInput)
		wantErr error
	}{
		{"other invocation", func(in *domain.ExecutionExportInput) {
			in.InvocationID = "inv-other"
		}, domain.ErrParentKeyMismatch},
		{"other admission", func(in *domain.ExecutionExportInput) {
			in.AdmissionID = "sha256:other"
		}, domain.ErrParentKeyMismatch},
		{"other base", func(in *domain.ExecutionExportInput) {
			in.ObservedBaseSHA = "0ther"
		}, domain.ErrExportBaseMismatch},
		{
			// An export is what the attempt handed back, so a record dated
			// before the admission reads the audit trail backwards.
			"recorded before admission", func(in *domain.ExecutionExportInput) {
				in.RecordedAt = a.AdmittedAt.Add(-time.Second)
			}, domain.ErrTimestampOutOfOrder,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := exportInput(a)
			tc.mutate(&in)
			x, err := domain.NewExecutionExport(in)
			if err != nil {
				t.Fatalf("NewExecutionExport: %v", err)
			}
			if err := domain.ValidateExportBinding(a, x); !errors.Is(err, tc.wantErr) {
				t.Fatalf("ValidateExportBinding = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func validExportRejectionInput(a domain.ExecutionAdmission) domain.ExportRejectionInput {
	return domain.ExportRejectionInput{
		InvocationID: a.InvocationID,
		AdmissionID:  a.ID,
		Findings: []domain.ExportRejectionFinding{
			{Kind: "secret", Path: "config.yaml", Rule: "aws_key", Line: 12, Detail: "match"},
			{Kind: "allowlist_violation", Path: "dist/bundle.js"},
		},
		TotalFindings: 2,
		RecordedAt:    a.AdmittedAt.Add(time.Minute),
	}
}

// TestNewExportRejectionValidates covers the record's own well-formedness: a
// findings-free rejection is meaningless, a finding needs a kind, path and
// path_hex are mutually exclusive, and the timestamp is a UTC instant.
func TestNewExportRejectionValidates(t *testing.T) {
	a := mustAdmission(t, admissionInput())
	cases := []struct {
		name    string
		mutate  func(in *domain.ExportRejectionInput)
		wantErr error
	}{
		{"no invocation", func(in *domain.ExportRejectionInput) { in.InvocationID = "" }, domain.ErrEmptyID},
		{"no admission", func(in *domain.ExportRejectionInput) { in.AdmissionID = "" }, domain.ErrEmptyID},
		{"no findings", func(in *domain.ExportRejectionInput) { in.Findings = nil }, domain.ErrExportRejectionEmpty},
		{"finding without kind", func(in *domain.ExportRejectionInput) {
			in.Findings = []domain.ExportRejectionFinding{{Path: "x"}}
		}, domain.ErrEmptyField},
		{"finding path and path_hex", func(in *domain.ExportRejectionInput) {
			in.Findings = []domain.ExportRejectionFinding{{Kind: "invalid_path_entry", Path: "x", PathHex: "6162"}}
		}, domain.ErrFindingPathConflict},
		{"zero time", func(in *domain.ExportRejectionInput) { in.RecordedAt = time.Time{} }, domain.ErrMissingTimestamp},
		{"total below retained", func(in *domain.ExportRejectionInput) { in.TotalFindings = 1 }, domain.ErrOutcomeInconsistent},
		{"truncated subset ok", func(in *domain.ExportRejectionInput) { in.TotalFindings = 9999 }, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validExportRejectionInput(a)
			tc.mutate(&in)
			if _, err := domain.NewExportRejection(in); !errors.Is(err, tc.wantErr) {
				t.Fatalf("NewExportRejection = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestExportRejectionValidateRejectsNonUTC covers the decode backstop: the
// constructor renders UTC, but a stored body carrying a zoned instant would
// address two identities for one moment, so Validate refuses it directly.
func TestExportRejectionValidateRejectsNonUTC(t *testing.T) {
	a := mustAdmission(t, admissionInput())
	in := validExportRejectionInput(a)
	r := domain.ExportRejection{
		InvocationID: in.InvocationID, AdmissionID: in.AdmissionID,
		Findings: in.Findings, TotalFindings: in.TotalFindings,
		RecordedAt: in.RecordedAt.In(time.FixedZone("east", 3600)),
	}
	if err := r.Validate(); !errors.Is(err, domain.ErrTimestampNotUTC) {
		t.Fatalf("Validate = %v, want %v", err, domain.ErrTimestampNotUTC)
	}
}

// TestNewExportRejectionNormalizesTime pins that the constructor renders the
// instant UTC, so a caller-supplied zoned instant still converges on one body.
func TestNewExportRejectionNormalizesTime(t *testing.T) {
	a := mustAdmission(t, admissionInput())
	in := validExportRejectionInput(a)
	in.RecordedAt = a.AdmittedAt.Add(time.Minute).In(time.FixedZone("east", 3600))
	r, err := domain.NewExportRejection(in)
	if err != nil {
		t.Fatalf("NewExportRejection: %v", err)
	}
	if r.RecordedAt.Location() != time.UTC {
		t.Fatalf("recorded_at location = %v, want UTC", r.RecordedAt.Location())
	}
	body, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var round domain.ExportRejection
	if err := json.Unmarshal(body, &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := round.Validate(); err != nil {
		t.Fatalf("round-trip Validate: %v", err)
	}
}

func TestValidateExportRejectionBinding(t *testing.T) {
	a := mustAdmission(t, admissionInput())
	cases := []struct {
		name    string
		mutate  func(in *domain.ExportRejectionInput)
		wantErr error
	}{
		{"ok", func(*domain.ExportRejectionInput) {}, nil},
		{"foreign admission", func(in *domain.ExportRejectionInput) {
			in.AdmissionID = domain.Digest("sha256:" + strings.Repeat("ab", 32))
		}, domain.ErrParentKeyMismatch},
		{"recorded before admission", func(in *domain.ExportRejectionInput) {
			in.RecordedAt = a.AdmittedAt.Add(-time.Second)
		}, domain.ErrTimestampOutOfOrder},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validExportRejectionInput(a)
			tc.mutate(&in)
			r, err := domain.NewExportRejection(in)
			if err != nil {
				t.Fatalf("NewExportRejection: %v", err)
			}
			if err := domain.ValidateExportRejectionBinding(a, r); !errors.Is(err, tc.wantErr) {
				t.Fatalf("ValidateExportRejectionBinding = %v, want %v", err, tc.wantErr)
			}
		})
	}
}
