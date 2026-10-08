package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

type submissionEgressCase struct {
	name    string
	values  map[string]string
	wantErr error
	wantKey string
}

func seedHistoricalManualSubmission(t *testing.T, st *store.Store, blobs *signet.BlobStore, req submitApplyRequest) {
	t.Helper()
	spec := submissionFile{digest: req.SpecDigest, body: req.SpecBody}
	policy := submissionFile{digest: req.PolicyDigest, body: req.PolicyBody}
	if _, err := blobs.Put(spec.digest, bytes.NewReader(spec.body)); err != nil {
		t.Fatal(err)
	}
	if _, err := blobs.Put(policy.digest, bytes.NewReader(policy.body)); err != nil {
		t.Fatal(err)
	}
	specArtifact, err := engine.SubmissionArtifact(domain.ArtifactKindSpecification, spec.digest, domain.EvidenceMediaTextMarkdown, int64(len(spec.body)))
	if err != nil {
		t.Fatal(err)
	}
	policyArtifact, err := engine.SubmissionArtifact(domain.ArtifactKindPolicy, policy.digest, domain.EvidenceMediaApplicationJSON, int64(len(policy.body)))
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := json.Marshal(struct {
		Project                                            domain.ProjectID
		Source, Policy, Publication, WorkUnit, Composition domain.Digest
	}{req.ProjectID, req.SpecDigest, req.PolicyDigest, req.PublicationBodyDigest, req.WorkUnitDigest, req.CompositionDigest})
	if err != nil {
		t.Fatal(err)
	}
	manual := &domain.ManualSubmission{Identity: "cli:" + req.SubmissionID, ProjectID: req.ProjectID, SourceArtifactID: specArtifact.ID, SourceDigest: req.SpecDigest, RequestDigest: domain.Digest(contentaddr.Sum(fingerprint)), ImplementationRunID: req.ImplementationRunID}
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		if err := engine.RegisterSubmissionArtifact(t.Context(), tx, specArtifact); err != nil {
			return err
		}
		if err := engine.RegisterSubmissionArtifact(t.Context(), tx, policyArtifact); err != nil {
			return err
		}
		_, err := engine.SubmitSpecificationRunTx(t.Context(), tx, engine.SpecificationRunSpec{ManualSubmission: manual, SpecificationRunID: req.SpecificationRunID, ImplementationRunID: req.ImplementationRunID, ProjectID: req.ProjectID, SourceArtifactID: specArtifact.ID, SourceBytes: req.SpecBody, PolicyArtifactID: policyArtifact.ID, ResolvedPolicy: req.ResolvedPolicy, Publication: req.Publication, PublicationDigest: req.PublicationDigest, CampaignID: req.CampaignID, AttemptNumber: 1})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestHistoricalManualEgressReplay(t *testing.T) {
	t.Parallel()
	for _, values := range []map[string]string{{domain.EgressProfilePolicyKey: "provider_unknown"}, {domain.EgressProfilePolicyKey: "provider_only", domain.RegistrySetPolicyKey: "[]"}} {
		t.Run("historical", func(t *testing.T) {
			req := egressApplyRequest(t, values)
			st := storetest.Open(t, filepath.Join(t.TempDir(), "state.db"), store.Options{})
			blobs, err := signet.NewBlobStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			seedHistoricalManualSubmission(t, st, blobs, req)
			if _, err := applySubmission(t.Context(), st, blobs, req); err != nil {
				t.Fatalf("historical replay = %v", err)
			}
			legacy := req
			legacy.LegacyRunID, legacy.SubmissionID = req.ImplementationRunID, ""
			if _, err := applySubmission(t.Context(), st, blobs, legacy); err != nil {
				t.Fatalf("specification-backed legacy replay = %v", err)
			}
		})
	}
}

func TestHistoricalManualEgressCLIRetry(t *testing.T) {
	t.Parallel()
	for name, values := range map[string]map[string]string{
		"unknown profile":             {domain.EgressProfilePolicyKey: "provider_unknown"},
		"provider only malformed set": {domain.EgressProfilePolicyKey: "provider_only", domain.RegistrySetPolicyKey: "[]"},
	} {
		t.Run(name, func(t *testing.T) {
			req := egressApplyRequest(t, values)
			root := t.TempDir()
			taskPath := filepath.Join(root, "task.md")
			policyPath := filepath.Join(root, "policy.json")
			publicationPath := filepath.Join(root, "publication.json")
			publicationBody, err := json.Marshal(req.Publication)
			if err != nil {
				t.Fatal(err)
			}
			for path, body := range map[string][]byte{taskPath: req.SpecBody, policyPath: req.PolicyBody, publicationPath: publicationBody} {
				if err := os.WriteFile(path, body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cfg := submitCommandConfig{DBPath: filepath.Join(root, "state.db"), SubmissionID: req.SubmissionID, ProjectID: req.ProjectID, TaskPath: taskPath, PolicyPath: policyPath, PublicationPath: publicationPath}
			prepared, err := prepareSubmission(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := retainSubmission(prepared); err != nil {
				t.Fatal(err)
			}
			if _, err := loadOrCreateTopicKey(cfg.DBPath, false); err != nil {
				t.Fatal(err)
			}
			st := storetest.Open(t, cfg.DBPath, store.Options{})
			blobs, err := signet.NewBlobStore(cfg.DBPath + ".blobs")
			if err != nil {
				t.Fatal(err)
			}
			seedHistoricalManualSubmission(t, st, blobs, req)
			replayed, err := runSubmitCommand(t.Context(), submitCommandConfig{DBPath: cfg.DBPath, RetrySubmissionID: req.SubmissionID})
			if err != nil {
				t.Fatalf("retry = %v", err)
			}
			if replayed.ImplementationRunID != req.ImplementationRunID || replayed.SpecificationRunID != req.SpecificationRunID || replayed.SpecificationPolicyDigest != req.PolicyDigest {
				t.Fatalf("retry = %+v, want seeded identities", replayed)
			}
		})
	}
}

func TestSpecificationBackedLegacyMalformedPolicyDoesNotWriteBlob(t *testing.T) {
	t.Parallel()
	seed := egressApplyRequest(t, map[string]string{domain.EgressProfilePolicyKey: "provider_only"})
	st := storetest.Open(t, filepath.Join(t.TempDir(), "state.db"), store.Options{})
	blobs, err := signet.NewBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seedHistoricalManualSubmission(t, st, blobs, seed)
	request := seed
	request.LegacyRunID = seed.ImplementationRunID
	request.SubmissionID = ""
	request.Keys = withSubmissionEgress(seed.Keys, map[string]string{domain.RegistrySetPolicyKey: "[]"})
	request.ResolvedPolicy, err = domain.NewResolvedPolicy(request.SpecificationRunID, request.Keys)
	if err != nil {
		t.Fatal(err)
	}
	request.PolicyDigest = request.ResolvedPolicy.Digest
	request.PolicyBody, err = json.Marshal(request.ResolvedPolicy.Keys)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applySubmission(t.Context(), st, blobs, request); !errors.Is(err, domain.ErrRegistrySetInvalid) {
		t.Fatalf("mismatched legacy request = %v, want invalid registry set", err)
	}
	if found, err := blobs.Has(request.PolicyDigest); err != nil || found {
		t.Fatalf("mismatched legacy request stored policy blob: found=%t err=%v", found, err)
	}
}

func TestRetainedMalformedEgressSubmissionDoesNotCreateWork(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	task, policy, publication := writeSubmissionInputs(t, root)
	writeSubmissionEgressPolicy(t, policy, map[string]string{domain.EgressProfilePolicyKey: "provider_unknown"})
	cfg := submitCommandConfig{DBPath: filepath.Join(root, "state.db"), SubmissionID: "historical-journal", TaskPath: task, PolicyPath: policy, PublicationPath: publication, ProjectID: "project-journal"}
	prepared, err := prepareSubmission(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := retainSubmission(prepared); err != nil {
		t.Fatal(err)
	}
	if _, err := runSubmitCommand(t.Context(), submitCommandConfig{DBPath: cfg.DBPath, RetrySubmissionID: cfg.SubmissionID}); !errors.Is(err, domain.ErrInvalidEgressProfile) {
		t.Fatalf("journal-only retry = %v, want invalid egress profile", err)
	}
	policyFile, err := readSubmissionFile(policy)
	if err != nil {
		t.Fatal(err)
	}
	var keys []domain.PolicyKey
	if err := json.Unmarshal(policyFile.body, &keys); err != nil {
		t.Fatal(err)
	}
	spec, err := readSubmissionFile(task)
	if err != nil {
		t.Fatal(err)
	}
	publicationFile, err := readSubmissionFile(publication)
	if err != nil {
		t.Fatal(err)
	}
	var metadata engine.ProductionPublication
	if err := json.Unmarshal(publicationFile.body, &metadata); err != nil {
		t.Fatal(err)
	}
	publicationBody, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	keysDigest, err := (domain.ResolvedPolicy{Keys: keys}).ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	runID := engine.ManualSubmissionRunID("cli:"+cfg.SubmissionID, cfg.ProjectID, spec.digest, keysDigest, submissionBytes(publicationBody).digest, "")
	specificationRunID, err := engine.SpecificationRunIDForImplementation(runID)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := domain.NewResolvedPolicy(specificationRunID, keys)
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := signet.NewBlobStore(cfg.DBPath + ".blobs")
	if err != nil {
		t.Fatal(err)
	}
	if found, err := blobs.Has(resolved.Digest); err != nil || found {
		t.Fatalf("journal-only retry stored malformed policy blob: found=%t err=%v", found, err)
	}
	st := storetest.Open(t, cfg.DBPath, store.Options{})
	assertEgressPolicyAbsent(t, st, blobs, specificationRunID, resolved.Digest)
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		if _, err := tx.GetManualSubmission(t.Context(), "cli:"+cfg.SubmissionID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("journal-only retry manual submission = %v, want absent", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func submissionEgressCases() []submissionEgressCase {
	return []submissionEgressCase{
		{"unknown profile", map[string]string{domain.EgressProfilePolicyKey: "provider_unknown"}, domain.ErrInvalidEgressProfile, domain.EgressProfilePolicyKey},
		{"empty profile", map[string]string{domain.EgressProfilePolicyKey: ""}, domain.ErrInvalidEgressProfile, domain.EgressProfilePolicyKey},
		{"padded profile", map[string]string{domain.EgressProfilePolicyKey: "provider_only "}, domain.ErrInvalidEgressProfile, domain.EgressProfilePolicyKey},
		{"missing set", map[string]string{domain.EgressProfilePolicyKey: "provider_registry"}, domain.ErrRegistrySetInvalid, domain.RegistrySetPolicyKey},
		{"malformed set alone", map[string]string{domain.RegistrySetPolicyKey: "null"}, domain.ErrRegistrySetInvalid, domain.RegistrySetPolicyKey},
		{"malformed provider only set", map[string]string{domain.EgressProfilePolicyKey: "provider_only", domain.RegistrySetPolicyKey: "[]"}, domain.ErrRegistrySetInvalid, domain.RegistrySetPolicyKey},
		{"malformed provider registry set", map[string]string{domain.EgressProfilePolicyKey: "provider_registry", domain.RegistrySetPolicyKey: `["registry.local"]`}, domain.ErrRegistrySetInvalid, domain.RegistrySetPolicyKey},
		{"defaults", nil, nil, ""},
		{"provider only", map[string]string{domain.EgressProfilePolicyKey: "provider_only"}, nil, ""},
		{"set alone", map[string]string{domain.RegistrySetPolicyKey: `["pypi.org"]`}, nil, ""},
		{"provider only set", map[string]string{domain.EgressProfilePolicyKey: "provider_only", domain.RegistrySetPolicyKey: `["pypi.org"]`}, nil, ""},
		{"provider registry set", map[string]string{domain.EgressProfilePolicyKey: "provider_registry", domain.RegistrySetPolicyKey: `["pypi.org"]`}, nil, ""},
	}
}

func withSubmissionEgress(keys []domain.PolicyKey, values map[string]string) []domain.PolicyKey {
	out := slices.Clone(keys)
	for key, value := range values {
		out = append(out, domain.PolicyKey{Key: key, Value: value, Provenance: keys[0].Provenance})
	}
	return out
}

func checkSubmissionEgressError(t *testing.T, err error, tc submissionEgressCase) {
	t.Helper()
	if !errors.Is(err, tc.wantErr) || (tc.wantErr != nil && !strings.Contains(err.Error(), tc.wantKey)) {
		t.Fatalf("error = %v, want %v naming %s", err, tc.wantErr, tc.wantKey)
	}
}

func writeSubmissionEgressPolicy(t *testing.T, path string, values map[string]string) {
	t.Helper()
	keys := withSubmissionEgress(manualConfigFixture(t).Projects[0].PolicyKeys, values)
	body, err := json.Marshal(keys)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestManualSubmissionConfigEgressPolicy(t *testing.T) {
	t.Parallel()
	for _, tc := range submissionEgressCases() {
		t.Run(tc.name, func(t *testing.T) {
			cfg := manualConfigFixture(t)
			later := cfg.Projects[0]
			later.ProjectID = "project-later"
			later.PolicyKeys = withSubmissionEgress(later.PolicyKeys, tc.values)
			cfg.Projects = append(cfg.Projects, later)
			path := filepath.Join(t.TempDir(), "manual.json")
			writeManualConfig(t, path, cfg)
			lookup, err := loadManualSubmissionConfig(path)
			checkSubmissionEgressError(t, err, tc)
			if tc.wantErr != nil {
				if lookup != nil || !strings.Contains(err.Error(), "project 1") {
					t.Fatalf("invalid later project: lookup returned %t, error %v", lookup != nil, err)
				}
				return
			}
			got, ok := lookup(later.ProjectID)
			want, err := domain.NewResolvedPolicy("manual-submission-config", later.PolicyKeys)
			if err != nil {
				t.Fatal(err)
			}
			if !ok || !reflect.DeepEqual(got.PolicyKeys, want.Keys) {
				t.Fatal("config changed valid policy keys")
			}
		})
	}
}

func TestSubmitCommandEgressPolicy(t *testing.T) {
	t.Parallel()
	for _, tc := range submissionEgressCases() {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			task, policy, publication := writeSubmissionInputs(t, root)
			writeSubmissionEgressPolicy(t, policy, tc.values)
			cfg := submitCommandConfig{DBPath: filepath.Join(root, "state.db"), SubmissionID: "egress", ProjectID: "proj-egress", TaskPath: task, PolicyPath: policy, PublicationPath: publication}
			_, err := runSubmitCommand(t.Context(), cfg)
			checkSubmissionEgressError(t, err, tc)
			if tc.wantErr != nil {
				for _, path := range []string{cfg.DBPath, cfg.DBPath + ".blobs", cfg.DBPath + ".submissions"} {
					if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("rejected submission left %s: %v", path, err)
					}
				}
			}
		})
	}
}

// The apply request binds canonical policy bytes, digests, and derived identities
// so a refused request exercises egress validation rather than a binding error.
func egressApplyRequest(t *testing.T, values map[string]string) submitApplyRequest {
	t.Helper()
	keys := withSubmissionEgress(manualConfigFixture(t).Projects[0].PolicyKeys, values)
	policy, err := domain.NewResolvedPolicy("temporary", keys)
	if err != nil {
		t.Fatal(err)
	}
	policyBody, err := json.Marshal(policy.Keys)
	if err != nil {
		t.Fatal(err)
	}
	publication := engine.ProductionPublication{Title: "Check the policy", Body: "Reviewer context.", CommitAuthor: engine.ProductionCommitAuthor{AppSlug: "freeside-test", BotUserID: 12345}}
	publicationBody, err := json.Marshal(publication)
	if err != nil {
		t.Fatal(err)
	}
	spec := submissionBytes([]byte("# Check egress policy\n"))
	publicationDigest := submissionBytes(publicationBody).digest
	runID := engine.ManualSubmissionRunID("cli:egress", "proj-egress", spec.digest, policy.Digest, publicationDigest, "")
	specID, err := engine.SpecificationRunIDForImplementation(runID)
	if err != nil {
		t.Fatal(err)
	}
	campaignID, err := engine.ProductionCampaignIDForImplementation(runID)
	if err != nil {
		t.Fatal(err)
	}
	policy, err = domain.NewResolvedPolicy(specID, policy.Keys)
	if err != nil {
		t.Fatal(err)
	}
	return submitApplyRequest{
		SubmissionID: "egress", ProjectID: "proj-egress",
		SpecBody: spec.body, SpecDigest: spec.digest, PolicyBody: policyBody, PolicyDigest: policy.Digest,
		Keys: policy.Keys, ResolvedPolicy: policy, Publication: publication,
		PublicationDigest: publicationDigest, PublicationBodyDigest: publicationDigest,
		ImplementationRunID: runID, SpecificationRunID: specID, CampaignID: campaignID,
	}
}

func assertEgressPolicyAbsent(t *testing.T, st *store.Store, blobs *signet.BlobStore, runID domain.RunID, digest domain.Digest) {
	t.Helper()
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		if _, err := tx.GetRun(t.Context(), runID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("run %s = %v, want absent", runID, err)
		}
		if _, err := tx.GetResolvedPolicy(t.Context(), runID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("resolved policy = %v, want absent", err)
		}
		artifact, err := engine.SubmissionArtifact(domain.ArtifactKindPolicy, digest, domain.EvidenceMediaApplicationJSON, 1)
		if err != nil {
			return err
		}
		if _, err := tx.GetArtifact(t.Context(), artifact.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("policy artifact = %v, want absent", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if found, err := blobs.Has(digest); err != nil || found {
		t.Fatalf("policy blob = %t, %v, want absent", found, err)
	}
}

func TestSubmitApplyEgressPolicy(t *testing.T) {
	t.Parallel()
	for _, tc := range submissionEgressCases() {
		t.Run(tc.name, func(t *testing.T) {
			req := egressApplyRequest(t, tc.values)
			if err := validateSubmitApply(req); err != nil {
				t.Fatalf("structural request validation = %v", err)
			}
			st := storetest.Open(t, filepath.Join(t.TempDir(), "state.db"), store.Options{})
			blobs, err := signet.NewBlobStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			_, err = applySubmission(t.Context(), st, blobs, req)
			checkSubmissionEgressError(t, err, tc)
			if tc.wantErr == nil {
				return
			}
			assertEgressPolicyAbsent(t, st, blobs, req.SpecificationRunID, req.PolicyDigest)
			if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
				if _, err := tx.GetRun(t.Context(), req.ImplementationRunID); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("implementation run = %v, want absent", err)
				}
				if _, err := tx.GetManualSubmission(t.Context(), "cli:egress"); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("manual submission = %v, want absent", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPreflightEgressPolicy(t *testing.T) {
	t.Parallel()
	for _, tc := range submissionEgressCases() {
		t.Run(tc.name, func(t *testing.T) {
			args, env := preflightFixture(t)
			args = append(args, "-submission-id", "egress")
			cfg, err := parsePreflightConfig(args, &bytes.Buffer{})
			if err != nil {
				t.Fatal(err)
			}
			writeSubmissionEgressPolicy(t, cfg.PolicyPath, tc.values)
			_, err = inspectCompositionIdentity(t.Context(), cfg)
			checkSubmissionEgressError(t, err, tc)
			var stdout bytes.Buffer
			err = runPreflightCommandWithEnvironment(t.Context(), args, &stdout, &bytes.Buffer{}, env,
				time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC), "2dce6570ee23")
			var manifest compositionManifest
			if decodeErr := json.Unmarshal(stdout.Bytes(), &manifest); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			want := compositionPassed
			if tc.wantErr != nil {
				want = compositionFailed
				if !errors.Is(err, errCompositionPreflight) || manifest.Status != compositionFailed {
					t.Fatalf("preflight = %v, status %s, want failed manifest", err, manifest.Status)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if got := checkStatus(manifest, "source_implementation_identity"); got != want {
				t.Fatalf("composition input check = %s, want %s", got, want)
			}
		})
	}
}

func TestPreflightLegacyEgressPolicyInspection(t *testing.T) {
	t.Parallel()
	for name, values := range map[string]map[string]string{
		"unknown profile":             {domain.EgressProfilePolicyKey: "provider_unknown"},
		"provider only malformed set": {domain.EgressProfilePolicyKey: "provider_only", domain.RegistrySetPolicyKey: "[]"},
	} {
		t.Run(name, func(t *testing.T) {
			args, env := preflightFixture(t)
			cfg, err := parsePreflightConfig(args, &bytes.Buffer{})
			if err != nil {
				t.Fatal(err)
			}
			writeSubmissionEgressPolicy(t, cfg.PolicyPath, values)
			identity, err := inspectCompositionIdentity(t.Context(), cfg)
			if err != nil {
				t.Fatalf("legacy inspection = %v", err)
			}
			if identity.SubmissionID != "" {
				t.Fatalf("legacy identity submission = %q", identity.SubmissionID)
			}
			var stdout bytes.Buffer
			if err := runPreflightCommandWithEnvironment(t.Context(), args, &stdout, &bytes.Buffer{}, env, time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC), "2dce6570ee23"); err != nil {
				t.Fatal(err)
			}
			var manifest compositionManifest
			if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
				t.Fatal(err)
			}
			if got := checkStatus(manifest, "source_implementation_identity"); got != compositionPassed {
				t.Fatalf("identity check = %s, want passed", got)
			}
		})
	}
}

func TestPreflightRetainedModernEgressPolicyInspection(t *testing.T) {
	t.Parallel()
	for name, values := range map[string]map[string]string{
		"unknown profile":             {domain.EgressProfilePolicyKey: "provider_unknown"},
		"provider only malformed set": {domain.EgressProfilePolicyKey: "provider_only", domain.RegistrySetPolicyKey: "[]"},
	} {
		t.Run(name, func(t *testing.T) {
			args, env := preflightFixture(t)
			args = append(args, "-submission-id", "retained-modern")
			cfg, err := parsePreflightConfig(args, &bytes.Buffer{})
			if err != nil {
				t.Fatal(err)
			}
			writeSubmissionEgressPolicy(t, cfg.PolicyPath, values)
			cfg.DBPath = filepath.Join(t.TempDir(), "state.db")
			env.rig.Resources.DatabasePath = cfg.DBPath
			policy := preflightResolvedPolicy(t, cfg)
			seedPreflightResolvedPolicy(t, cfg.DBPath, cfg.ProjectID, policy, cfg.TaskPath)
			identity, err := inspectCompositionIdentity(t.Context(), cfg)
			if err != nil {
				t.Fatalf("retained modern inspection = %v", err)
			}
			if identity.SubmissionID != cfg.SubmissionID {
				t.Fatalf("retained modern identity submission = %q, want %q", identity.SubmissionID, cfg.SubmissionID)
			}
			var stdout bytes.Buffer
			if err := runPreflightCommandWithEnvironment(t.Context(), args, &stdout, &bytes.Buffer{}, env, time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC), "2dce6570ee23"); err != nil {
				t.Fatal(err)
			}
			var manifest compositionManifest
			if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
				t.Fatal(err)
			}
			if got := checkStatus(manifest, "source_implementation_identity"); got != compositionPassed {
				t.Fatalf("identity check = %s, want passed", got)
			}
		})
	}
}

func TestPreflightRetainedModernEgressPolicyRequiresMatchingStoredPolicy(t *testing.T) {
	t.Parallel()
	args, env := preflightFixture(t)
	args = append(args, "-submission-id", "retained-modern")
	cfg, err := parsePreflightConfig(args, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	writeSubmissionEgressPolicy(t, cfg.PolicyPath, map[string]string{domain.EgressProfilePolicyKey: "provider_unknown"})
	cfg.DBPath = filepath.Join(t.TempDir(), "state.db")
	env.rig.Resources.DatabasePath = cfg.DBPath
	other := cfg
	other.SubmissionID = "other-retained-modern"
	seedPreflightResolvedPolicy(t, cfg.DBPath, cfg.ProjectID, preflightResolvedPolicy(t, other), cfg.TaskPath)
	if _, err := inspectCompositionIdentity(t.Context(), cfg); !errors.Is(err, domain.ErrInvalidEgressProfile) {
		t.Fatalf("reused identity with another stored policy = %v, want invalid egress profile", err)
	}
}

func preflightResolvedPolicy(t *testing.T, cfg preflightConfig) domain.ResolvedPolicy {
	t.Helper()
	spec, err := readSubmissionFile(cfg.TaskPath)
	if err != nil {
		t.Fatal(err)
	}
	policyFile, err := readSubmissionFile(cfg.PolicyPath)
	if err != nil {
		t.Fatal(err)
	}
	var keys []domain.PolicyKey
	if err := json.Unmarshal(policyFile.body, &keys); err != nil {
		t.Fatal(err)
	}
	policyDigest, err := (domain.ResolvedPolicy{Keys: keys}).ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	publicationFile, err := readSubmissionFile(cfg.PublicationPath)
	if err != nil {
		t.Fatal(err)
	}
	var publication engine.ProductionPublication
	if err := json.Unmarshal(publicationFile.body, &publication); err != nil {
		t.Fatal(err)
	}
	publicationBody, err := json.Marshal(publication)
	if err != nil {
		t.Fatal(err)
	}
	runID := engine.ManualSubmissionRunID("cli:"+cfg.SubmissionID, cfg.ProjectID, spec.digest, policyDigest, submissionBytes(publicationBody).digest, "")
	specificationRunID, err := engine.SpecificationRunIDForImplementation(runID)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := domain.NewResolvedPolicy(specificationRunID, keys)
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func seedPreflightResolvedPolicy(t *testing.T, dbPath string, projectID domain.ProjectID, policy domain.ResolvedPolicy, taskPath string) {
	t.Helper()
	spec, err := readSubmissionFile(taskPath)
	if err != nil {
		t.Fatal(err)
	}
	st := storetest.Open(t, dbPath, store.Options{})
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		if err := tx.PutRun(t.Context(), engine.NewReservedSpecificationRun(policy.RunID, projectID, spec.digest, policy.Digest)); err != nil {
			return err
		}
		return tx.PutResolvedPolicy(t.Context(), policy)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIntakeEgressPolicyBeforePersistence(t *testing.T) {
	t.Parallel()
	for _, tc := range submissionEgressCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := newIntakeFixture(t)
			init := intakeInitiatorFor(t, domain.InitiatorModePropose, domain.ProvenanceOverride, 5)
			init.PolicyKeys = withSubmissionEgress(init.PolicyKeys, tc.values)
			r := f.reconciler([]intakeInitiator{init}, labeledOpen(7), nil)
			r.reconcile(t.Context(), nil)
			occurrence := f.latestOccurrence(t, 7)
			if tc.wantErr == nil {
				if occurrence.Admission == nil {
					t.Fatal("valid policy was not admitted")
				}
				return
			}
			if occurrence.Admission != nil {
				t.Fatal("malformed policy was admitted")
			}
			_, err := r.admit(t.Context(), init, occurrence)
			checkSubmissionEgressError(t, err, tc)
			specID, err := engine.SpecificationRunIDForImplementation(intakeImplementationRunID(occurrence))
			if err != nil {
				t.Fatal(err)
			}
			policy, err := domain.NewResolvedPolicy(specID, init.PolicyKeys)
			if err != nil {
				t.Fatal(err)
			}
			assertEgressPolicyAbsent(t, f.store, f.blobs, specID, policy.Digest)
		})
	}
}
