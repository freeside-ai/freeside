package ward

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/export"
)

// suiteMarker is the inert fake credential the conformance suite tests seed
// and prove contained; it matches the spike's marker.
const suiteMarker = "FREESIDE_FAKE_PROVIDER_CREDENTIAL_DO_NOT_EXPORT"

// newSuiteTest builds a Suite over the deterministic handoff fixture (fake
// runtime, no-op sleep). The fake cannot model volume-attachment exclusion or
// carry the seeded marker through an export, so the negative-probe outcomes
// are scripted via its hooks for orchestration coverage; the real proof is
// the host-gated reference-runtime test.
func newSuiteTest(t *testing.T) (*Suite, *fakeRuntime) {
	t.Helper()
	fx := newHandoffFixture(t)
	b := fx.backend(t)
	s, err := NewSuite(b, SuiteFixture{
		AgentImage:       "example.test/agent@sha256:" + strings.Repeat("1", 64),
		CredentialMarker: suiteMarker,
		RunID:            "conf-run",
	})
	if err != nil {
		t.Fatalf("NewSuite = %v", err)
	}
	// The default writer emits this run's sentinel; a passing-path export must
	// carry it (and not the credential marker) for Full's non-vacuousness check.
	// Tests that assert a containment failure override this.
	fx.rt.exportTarPath = buildTar(t, writerArchive(t, s.fx.RunID))
	return s, fx.rt
}

// writerArchive is the workspace export a passing default writer produces: the
// run's writer sentinel in result.txt and the fixed payload in the nested state
// file (the deterministic directory tree Full requires survive), neither
// carrying the credential marker. Bytewise order: "nested/state.txt" precedes
// "result.txt".
func writerArchive(t *testing.T, runID string) []tarEntry {
	t.Helper()
	return manifestArchive(t, []manifestFile{
		{path: workspaceStateFile, body: workspaceStatePayload + "\n"},
		{path: writerResultPath, body: writerSentinel(runID) + "\n"},
	})
}

// leakArchive is a passing-shape export whose result.txt is exactly this run's
// writer sentinel (so the digest-matched sentinel check passes) and which leaks
// the credential marker in the *content* of the expected nested state file (an
// allowed path whose content is not digest-pinned), so the export-half marker
// scan fails. Entries are bytewise-sorted ("nested/state.txt" precedes
// "result.txt"), the manifest invariant.
func leakArchive(t *testing.T, runID, marker string) []tarEntry {
	t.Helper()
	return manifestArchive(t, []manifestFile{
		{path: "nested/state.txt", body: marker + "\n"},
		{path: "result.txt", body: writerSentinel(runID) + "\n"},
	})
}

type manifestFile struct{ path, body string }

// manifestArchive builds a valid export archive: a proof, a manifest listing
// each file as a regular entry (bytewise-sorted by path, the caller's
// responsibility), and the matching blobs.
func manifestArchive(t *testing.T, files []manifestFile) []tarEntry {
	t.Helper()
	entries := make([]export.Entry, 0, len(files))
	blobs := make([]tarEntry, 0, len(files))
	for _, f := range files {
		blob := []byte(f.body)
		sum := sha256.Sum256(blob)
		hexDigest := hex.EncodeToString(sum[:])
		mode := "0644"
		size := int64(len(blob))
		digest := export.Digest("sha256:" + hexDigest)
		entries = append(entries, export.Entry{Path: f.path, Kind: export.EntryRegular, Mode: &mode, Size: &size, Digest: &digest})
		blobs = append(blobs, tarEntry{name: "handoff/blobs/sha256/" + hexDigest, body: blob})
	}
	raw, err := json.Marshal(export.Manifest{Version: export.ManifestVersion, Entries: entries})
	if err != nil {
		t.Fatal(err)
	}
	out := []tarEntry{
		{name: "handoff-proof.txt", body: validProof()},
		{name: "handoff/", typeflag: tar.TypeDir},
		{name: "handoff/manifest.json", body: raw},
		{name: "handoff/blobs/", typeflag: tar.TypeDir},
		{name: "handoff/blobs/sha256/", typeflag: tar.TypeDir},
	}
	return append(out, blobs...)
}

// passAudit scripts the fake's audit rootfs export to carry this run's audit
// sentinel, the token probeCredentialContainment scans for (proving the
// detached credential was readable). Used by tests that need the containment
// audit to pass so a later probe or cleanup path is exercised.
func passAudit(s *Suite) func(string, io.Writer) error {
	return func(id string, dest io.Writer) error {
		if id == s.conformanceName("audit") {
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			body := []byte(auditSentinel(s.fx.RunID) + "\n")
			if err := tw.WriteHeader(&tar.Header{
				Name: strings.TrimPrefix(auditMarkerPath(s.fx.RunID), "/"),
				Mode: 0o644,
				Size: int64(len(body)),
			}); err != nil {
				return err
			}
			if _, err := tw.Write(body); err != nil {
				return err
			}
			if err := tw.Close(); err != nil {
				return err
			}
			_, err := dest.Write(buf.Bytes())
			return err
		}
		return nil
	}
}

// scriptHappyProbes makes the fake report the two Full negative probes as
// passing: the audit container's rootfs export carries the marker, and the
// second attach in the exclusion probe is refused (as the reference runtime's
// Virtualization.framework does at bootstrap).
func scriptHappyProbes(s *Suite, rt *fakeRuntime) {
	rt.onExport = passAudit(s)
	rt.onStart = func(id string) error {
		if id == s.conformanceName("excl-second") {
			return errors.New("VZErrorDomain Code=2: the storage device attachment is invalid")
		}
		return nil
	}
	// The exclusion probe inspects the writer three times (stopped before start,
	// running before the second attach, and running after the refusal);
	// the fake reports a started container running for only its first inspect by
	// default, so keep the live writer running across both post-start inspects.
	rt.runningInspects[s.conformanceName("excl-writer")] = 2
}

func (s *Suite) assertReaped(t *testing.T, rt *fakeRuntime) {
	t.Helper()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for name := range rt.ctrs {
		t.Errorf("container %q survived the suite", name)
	}
	for name := range rt.vols {
		t.Errorf("volume %q survived the suite", name)
	}
}

func TestSuiteFullSuccess(t *testing.T) {
	s, rt := newSuiteTest(t)
	scriptHappyProbes(s, rt)
	if err := s.Full(context.Background()); err != nil {
		t.Fatalf("Full = %v, want nil", err)
	}
	s.assertReaped(t, rt)
}

// TestSuiteFullPropagatesCheckFailure proves Full surfaces the synthetic
// handoff's typed conformance failure unchanged (naming the failed contract
// check). The per-check induced-failure coverage for checks 1-5 and 7 lives
// in the gate's own tests; here Full is the pass-through.
func TestSuiteFullPropagatesCheckFailure(t *testing.T) {
	s, rt := newSuiteTest(t)
	exporter := namesFor("conf-run").Exporter
	// A runtime that realizes an extra bind on the exporter defeats check 4
	// (exactly one persistent mount), inspected before the exporter executes.
	rt.onInspect = func(id string, rep InspectReport) (InspectReport, error) {
		if id == exporter {
			rep.Mounts = append(rep.Mounts, Mount{Type: MountBind, Source: "/etc", Target: "/etc", ReadOnly: true})
		}
		return rep, nil
	}
	err := s.Full(context.Background())
	wantCheckFailure(t, err, CheckExporterAllowlist)
	s.assertReaped(t, rt)
}

// TestSuiteFullExportContainmentBreach: the released output carries the
// credential marker, so containment fails at the export half.
func TestSuiteFullExportContainmentBreach(t *testing.T) {
	s, rt := newSuiteTest(t)
	// The writer ran (sentinel present) and leaked the marker into the export.
	rt.exportTarPath = buildTar(t, leakArchive(t, s.fx.RunID, suiteMarker))
	err := s.Full(context.Background())
	wantCheckFailure(t, err, CheckCredentialContainment)
	var cf *ConformanceFailure
	_ = errors.As(err, &cf)
	if !strings.Contains(cf.Reason, "released export") {
		t.Errorf("reason = %q, want the export-half breach", cf.Reason)
	}
	s.assertReaped(t, rt)
}

// TestSuiteFullContainmentProbeFailure: the marker is absent from the export
// (containment export half passes) but the audit export does not carry it
// either, so the detached-volume half fails (a credential that did not
// survive detachment would be deletion, not omission).
func TestSuiteFullContainmentProbeFailure(t *testing.T) {
	s, rt := newSuiteTest(t)
	// No marker scripted into any export: the default fixture archive carries
	// none, so the audit cannot read it back.
	err := s.Full(context.Background())
	wantCheckFailure(t, err, CheckCredentialContainment)
	var cf *ConformanceFailure
	_ = errors.As(err, &cf)
	if !strings.Contains(cf.Reason, "not readable from the detached") {
		t.Errorf("reason = %q, want the detached-volume half failure", cf.Reason)
	}
	s.assertReaped(t, rt)
}

// TestSuiteFullExclusionProbeFailure: both containment halves pass, but the
// second VM attaches the workspace volume the writer holds read-write, so the
// exclusion probe fails.
func TestSuiteFullExclusionProbeFailure(t *testing.T) {
	s, rt := newSuiteTest(t)
	// Marker present for the audit (containment passes) but no refusal scripted
	// for the second attach, so the exclusion does not hold.
	rt.onExport = passAudit(s)
	err := s.Full(context.Background())
	wantCheckFailure(t, err, CheckWriterVolumeExclusion)
	s.assertReaped(t, rt)
}

// TestSuiteFullAmbiguousCredVolumeCreateReaped: a CreateVolume that makes the
// credential volume and then returns an error (ambiguous create) must not
// leak it — the reap is registered before the create.
func TestSuiteFullAmbiguousCredVolumeCreateReaped(t *testing.T) {
	s, rt := newSuiteTest(t)
	rt.createVolumeThenFail = true
	if err := s.Full(context.Background()); err == nil {
		t.Fatal("Full = nil, want error on ambiguous credential-volume create")
	}
	s.assertReaped(t, rt)
}

// TestSuiteFullFailsClosedOnCleanupLeak: the probes pass, but a reap silently
// no-ops so a suite object survives; the fail-closed suite must report it
// rather than returning nil.
func TestSuiteFullFailsClosedOnCleanupLeak(t *testing.T) {
	s, rt := newSuiteTest(t)
	scriptHappyProbes(s, rt)
	rt.onDeleteVolume = func(name string) (bool, error) {
		if name == s.conformanceName("cred") {
			return true, nil // skip removal, report no error: the volume survives
		}
		return false, nil
	}
	err := s.Full(context.Background())
	if err == nil {
		t.Fatal("Full = nil, want fail-closed on a surviving suite object")
	}
	if !strings.Contains(err.Error(), "survived cleanup") {
		t.Errorf("error = %v, want a survived-cleanup failure", err)
	}
}

// TestSuiteFullJoinsCleanupFailureWithProbeError: when a probe fails AND a
// reap leaves an object behind, Full surfaces both, never suppressing the
// leak under the probe error.
func TestSuiteFullJoinsCleanupFailureWithProbeError(t *testing.T) {
	s, rt := newSuiteTest(t)
	// No audit marker scripted, so the containment probe fails; and the
	// credential-volume delete silently no-ops, so it also leaks.
	rt.onDeleteVolume = func(name string) (bool, error) {
		if name == s.conformanceName("cred") {
			return true, nil
		}
		return false, nil
	}
	err := s.Full(context.Background())
	if err == nil {
		t.Fatal("Full = nil, want a joined failure")
	}
	if !errors.Is(err, ErrConformance) {
		t.Error("joined error does not carry the conformance-probe failure")
	}
	if !strings.Contains(err.Error(), "not readable from the detached") {
		t.Errorf("error = %q, missing the containment-probe failure", err)
	}
	if !strings.Contains(err.Error(), "survived cleanup") {
		t.Errorf("error = %q, missing the cleanup-leak failure", err)
	}
}

// TestSuiteFullAuditExportCapped: an oversized audit rootfs export is capped
// host-side at MaxArchiveBytes and fails the containment probe closed, rather
// than draining unbounded until the suite budget.
func TestSuiteFullAuditExportCapped(t *testing.T) {
	fx := newHandoffFixture(t)
	fx.cfg.MaxArchiveBytes = 1 << 20 // above the fixture archive, below the flood
	b := fx.backend(t)
	s, err := NewSuite(b, SuiteFixture{
		AgentImage:       "example.test/agent@sha256:" + strings.Repeat("1", 64),
		CredentialMarker: suiteMarker,
		RunID:            "conf-run",
	})
	if err != nil {
		t.Fatal(err)
	}
	// The workspace export carries the sentinel so containment reaches the audit
	// half; only the audit export floods past the cap.
	fx.rt.exportTarPath = buildTar(t, writerArchive(t, s.fx.RunID))
	fx.rt.onExport = func(id string, dest io.Writer) error {
		if id == s.conformanceName("audit") {
			_, werr := dest.Write(bytes.Repeat([]byte("x"), 2<<20)) // 2 MiB > cap
			return werr
		}
		return nil
	}
	wantCheckFailure(t, s.Full(context.Background()), CheckCredentialContainment)
	s.assertReaped(t, fx.rt)
}

// TestSuiteFullAuditExportOverflowSwallowed: a Runtime that writes past the
// cap but returns nil (swallowing the cap error) must still fail closed via
// the overflow check.
func TestSuiteFullAuditExportOverflowSwallowed(t *testing.T) {
	fx := newHandoffFixture(t)
	fx.cfg.MaxArchiveBytes = 1 << 20
	b := fx.backend(t)
	s, err := NewSuite(b, SuiteFixture{
		AgentImage:       "example.test/agent@sha256:" + strings.Repeat("1", 64),
		CredentialMarker: suiteMarker,
		RunID:            "conf-run",
	})
	if err != nil {
		t.Fatal(err)
	}
	// The workspace export carries the sentinel so containment reaches the audit
	// half; only the audit export overflows the cap.
	fx.rt.exportTarPath = buildTar(t, writerArchive(t, s.fx.RunID))
	fx.rt.onExport = func(id string, dest io.Writer) error {
		if id == s.conformanceName("audit") {
			_, _ = dest.Write(bytes.Repeat([]byte("x"), 2<<20)) // past the cap
			return nil                                          // swallow the cap error
		}
		return nil
	}
	wantCheckFailure(t, s.Full(context.Background()), CheckCredentialContainment)
	s.assertReaped(t, fx.rt)
}

// TestSuiteFullExclusionWriterMountUnrealized: if the writer's inspected mount
// does not match the requested read-write workspace mount, the exclusion
// probe fails closed rather than trusting a refusal it never exercised.
func TestSuiteFullExclusionWriterMountUnrealized(t *testing.T) {
	s, rt := newSuiteTest(t)
	rt.onExport = passAudit(s)
	rt.onInspect = func(id string, rep InspectReport) (InspectReport, error) {
		if id == s.conformanceName("excl-writer") {
			rep.Mounts = nil // runtime dropped the workspace mount
		}
		return rep, nil
	}
	err := s.Full(context.Background())
	wantCheckFailure(t, err, CheckWriterVolumeExclusion)
	var cf *ConformanceFailure
	_ = errors.As(err, &cf)
	if !strings.Contains(cf.Reason, "did not realize") {
		t.Errorf("reason = %q, want the unrealized-mount failure", cf.Reason)
	}
	s.assertReaped(t, rt)
}

// TestSuiteFullExclusionDoesNotStartUnprovenWriter proves the exclusion probe
// binds the successful create to this invocation before executing by name. A
// contradictory post-create identity must fail closed without starting what
// could be a foreign same-name replacement.
func TestSuiteFullExclusionDoesNotStartUnprovenWriter(t *testing.T) {
	s, rt := newSuiteTest(t)
	rt.onExport = passAudit(s)
	writer := s.conformanceName("excl-writer")
	firstWriterInspect := true
	rt.onInspect = func(id string, rep InspectReport) (InspectReport, error) {
		if id == writer && firstWriterInspect {
			firstWriterInspect = false
			rep.Labels = []Label{{Key: "foreign", Value: "true"}}
		}
		return rep, nil
	}
	rt.onStart = func(id string) error {
		if id == writer {
			t.Fatalf("started unproven exclusion writer %q", id)
		}
		return nil
	}
	err := s.Full(context.Background())
	wantCheckFailure(t, err, CheckWriterVolumeExclusion)
	if !strings.Contains(err.Error(), "identity is unproven before start") {
		t.Errorf("error = %q, want pre-start ownership failure", err)
	}
	s.assertReaped(t, rt)
}

// TestSuiteFullExclusionSecondRunsDespiteError: a Runtime that returns a
// storage-attachment error from the second start yet leaves the second
// container running (the volume was not actually excluded) must fail closed,
// not pass on the error string alone.
func TestSuiteFullExclusionSecondRunsDespiteError(t *testing.T) {
	s, rt := newSuiteTest(t)
	rt.onExport = passAudit(s)
	rt.onStart = func(id string) error {
		if id == s.conformanceName("excl-second") {
			// The second VM actually started (attached the volume) but the
			// runtime reports a storage-attachment error anyway.
			rt.ctrs[id].started = true
			return errors.New("VZErrorDomain Code=2: the storage device attachment is invalid")
		}
		return nil
	}
	err := s.Full(context.Background())
	wantCheckFailure(t, err, CheckWriterVolumeExclusion)
	var cf *ConformanceFailure
	_ = errors.As(err, &cf)
	if !strings.Contains(cf.Reason, "running despite the attachment error") {
		t.Errorf("reason = %q, want the running-despite-error failure", cf.Reason)
	}
	s.assertReaped(t, rt)
}

// TestSuiteFullEmptyWorkspaceContainment: a handoff that exports an empty
// workspace makes "marker absent" vacuous, so containment must fail closed.
func TestSuiteFullEmptyWorkspaceContainment(t *testing.T) {
	s, rt := newSuiteTest(t)
	rt.exportTarPath = buildTar(t, []tarEntry{
		{name: "handoff-proof.txt", body: validProof()},
		{name: "handoff/", typeflag: tar.TypeDir},
		{name: "handoff/manifest.json", body: []byte(`{"version":"freeside.export.manifest/v1","entries":[]}`)},
	})
	err := s.Full(context.Background())
	wantCheckFailure(t, err, CheckCredentialContainment)
	var cf *ConformanceFailure
	_ = errors.As(err, &cf)
	if !strings.Contains(cf.Reason, "empty workspace") {
		t.Errorf("reason = %q, want the empty-workspace failure", cf.Reason)
	}
	s.assertReaped(t, rt)
}

// TestSuiteFullStaleWorkspaceNoSentinel: a non-empty export that does not carry
// this run's writer sentinel (stale or prepopulated content, or a writer that
// aborted before it could produce output) makes "marker absent" vacuous, so
// containment fails closed rather than passing on any non-empty content.
func TestSuiteFullStaleWorkspaceNoSentinel(t *testing.T) {
	s, rt := newSuiteTest(t)
	// Non-empty, but the blob is unrelated to this run: no writer sentinel.
	rt.exportTarPath = buildTar(t, markerArchive(t, "stale-prepopulated-content"))
	err := s.Full(context.Background())
	wantCheckFailure(t, err, CheckCredentialContainment)
	var cf *ConformanceFailure
	_ = errors.As(err, &cf)
	if !strings.Contains(cf.Reason, "writer sentinel") {
		t.Errorf("reason = %q, want the missing-sentinel failure", cf.Reason)
	}
	s.assertReaped(t, rt)
}

// TestSuiteFullDroppedNestedFixture: a lossy export that preserves result.txt
// (with this run's sentinel) but drops the nested directory-tree fixture must
// fail closed, proving the deterministic workspace tree survived the export.
func TestSuiteFullDroppedNestedFixture(t *testing.T) {
	s, rt := newSuiteTest(t)
	// Only result.txt survives; nested/state.txt was dropped.
	rt.exportTarPath = buildTar(t, markerArchive(t, writerSentinel(s.fx.RunID)))
	err := s.Full(context.Background())
	wantCheckFailure(t, err, CheckCredentialContainment)
	var cf *ConformanceFailure
	_ = errors.As(err, &cf)
	if !strings.Contains(cf.Reason, "nested workspace fixture") {
		t.Errorf("reason = %q, want the dropped-nested failure", cf.Reason)
	}
	s.assertReaped(t, rt)
}

// TestSuiteFullSentinelFilenameNotContent: a decoy workspace file merely NAMED
// like the writer sentinel (so the string appears as a path in manifest.json)
// while result.txt content is unrelated must fail closed — the check binds to
// the expected content at the expected path via the manifest digest, not any
// occurrence of the string in the export tree.
func TestSuiteFullSentinelFilenameNotContent(t *testing.T) {
	s, rt := newSuiteTest(t)
	// Bytewise order: the decoy path ("freeside-ward-writer-...") precedes
	// "result.txt". result.txt carries content that is not the sentinel.
	rt.exportTarPath = buildTar(t, manifestArchive(t, []manifestFile{
		{path: writerSentinel(s.fx.RunID), body: "unrelated\n"},
		{path: "result.txt", body: "not the sentinel\n"},
	}))
	err := s.Full(context.Background())
	wantCheckFailure(t, err, CheckCredentialContainment)
	var cf *ConformanceFailure
	_ = errors.As(err, &cf)
	if !strings.Contains(cf.Reason, "writer sentinel") {
		t.Errorf("reason = %q, want the missing-sentinel failure", cf.Reason)
	}
	s.assertReaped(t, rt)
}

// TestSuiteFullSentinelBlobOmitted: a runtime that returns a blob_omitted
// result.txt entry carrying the (publicly derivable) sentinel digest, with no
// blob, must fail closed — verifyExport does not re-hash omitted blobs, so the
// digest is unproven and cannot stand in for the writer's output.
func TestSuiteFullSentinelBlobOmitted(t *testing.T) {
	s, rt := newSuiteTest(t)
	sentinel := writerSentinel(s.fx.RunID) + "\n"
	sum := sha256.Sum256([]byte(sentinel))
	digest := export.Digest("sha256:" + hex.EncodeToString(sum[:]))
	mode := "0644"
	size := int64(len(sentinel))
	raw, err := json.Marshal(export.Manifest{Version: export.ManifestVersion, Entries: []export.Entry{{
		Path: "result.txt", Kind: export.EntryRegular, Mode: &mode, Size: &size, Digest: &digest, BlobOmitted: true,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	// No blob file: the entry claims the sentinel digest but omits its content.
	rt.exportTarPath = buildTar(t, []tarEntry{
		{name: "handoff-proof.txt", body: validProof()},
		{name: "handoff/", typeflag: tar.TypeDir},
		{name: "handoff/manifest.json", body: raw},
	})
	err = s.Full(context.Background())
	wantCheckFailure(t, err, CheckCredentialContainment)
	var cf *ConformanceFailure
	_ = errors.As(err, &cf)
	if !strings.Contains(cf.Reason, "writer sentinel") {
		t.Errorf("reason = %q, want the missing-sentinel failure", cf.Reason)
	}
	s.assertReaped(t, rt)
}

// TestSuiteFullAuditMountUnrealized: if the audit container's realized mount
// does not match the requested credential-volume mount (a runtime substituting
// a different same-marker volume), the detached-volume audit fails closed
// rather than trusting a token read from an unverified mount.
func TestSuiteFullAuditMountUnrealized(t *testing.T) {
	s, rt := newSuiteTest(t)
	rt.onExport = passAudit(s) // the audit would otherwise pass
	rt.onInspect = func(id string, rep InspectReport) (InspectReport, error) {
		if id == s.conformanceName("audit") {
			rep.Mounts = []Mount{{Type: MountVolume, Source: "impostor-volume", Target: s.fx.CredentialTarget, ReadOnly: true}}
		}
		return rep, nil
	}
	err := s.Full(context.Background())
	wantCheckFailure(t, err, CheckCredentialContainment)
	var cf *ConformanceFailure
	_ = errors.As(err, &cf)
	if !strings.Contains(cf.Reason, "did not realize the stopped suite-owned probe spec") {
		t.Errorf("reason = %q, want the unrealized-audit-mount failure", cf.Reason)
	}
	s.assertReaped(t, rt)
}

// TestSuiteFullAuditSpecMountMutationDefeated: a runtime that mutates the spec
// it receives at create AND realizes that mutated mount cannot satisfy the
// mount check, because the create got a clone and the comparison baseline is
// the suite's immutable original spec.
func TestSuiteFullAuditSpecMountMutationDefeated(t *testing.T) {
	s, rt := newSuiteTest(t)
	rt.onExport = passAudit(s)
	impostor := Mount{Type: MountVolume, Source: "impostor-volume", Target: s.fx.CredentialTarget, ReadOnly: true}
	rt.onCreateContainer = func(spec ContainerSpec) error {
		if spec.Name == s.conformanceName("audit") && len(spec.Mounts) > 0 {
			spec.Mounts[0] = impostor // would move the baseline without the clone
		}
		return nil
	}
	rt.onInspect = func(id string, rep InspectReport) (InspectReport, error) {
		if id == s.conformanceName("audit") {
			rep.Mounts = []Mount{impostor} // realize the substituted mount
		}
		return rep, nil
	}
	err := s.Full(context.Background())
	wantCheckFailure(t, err, CheckCredentialContainment)
	var cf *ConformanceFailure
	_ = errors.As(err, &cf)
	if !strings.Contains(cf.Reason, "did not realize the stopped suite-owned probe spec") {
		t.Errorf("reason = %q, want the unrealized-audit-mount failure", cf.Reason)
	}
	s.assertReaped(t, rt)
}

// TestSuiteFullOmittedBlobMarkerUnprovable: a blob_omitted workspace file has no
// bytes under ExportDir/blobs, so the marker scan cannot see whether it carries
// the credential; Full must fail closed rather than pass containment over
// content the export omitted (the omitted file could be the leak).
func TestSuiteFullOmittedBlobMarkerUnprovable(t *testing.T) {
	s, rt := newSuiteTest(t)
	sentinel := writerSentinel(s.fx.RunID) + "\n"
	sSum := sha256.Sum256([]byte(sentinel))
	sDigest := export.Digest("sha256:" + hex.EncodeToString(sSum[:]))
	oSum := sha256.Sum256([]byte("secret\n"))
	oDigest := export.Digest("sha256:" + hex.EncodeToString(oSum[:]))
	mode := "0644"
	sSize := int64(len(sentinel))
	oSize := int64(len("secret\n"))
	// Bytewise order: "nested/state.txt" precedes "result.txt". result.txt is
	// present (sentinel passes) and both are allowed paths (shape check passes);
	// the nested state file is a regular entry whose blob is omitted.
	raw, err := json.Marshal(export.Manifest{Version: export.ManifestVersion, Entries: []export.Entry{
		{Path: "nested/state.txt", Kind: export.EntryRegular, Mode: &mode, Size: &oSize, Digest: &oDigest, BlobOmitted: true},
		{Path: "result.txt", Kind: export.EntryRegular, Mode: &mode, Size: &sSize, Digest: &sDigest},
	}})
	if err != nil {
		t.Fatal(err)
	}
	rt.exportTarPath = buildTar(t, []tarEntry{
		{name: "handoff-proof.txt", body: validProof()},
		{name: "handoff/", typeflag: tar.TypeDir},
		{name: "handoff/manifest.json", body: raw},
		{name: "handoff/blobs/", typeflag: tar.TypeDir},
		{name: "handoff/blobs/sha256/", typeflag: tar.TypeDir},
		{name: "handoff/blobs/sha256/" + hex.EncodeToString(sSum[:]), body: []byte(sentinel)},
		// nested/state.txt's blob is intentionally omitted.
	})
	err = s.Full(context.Background())
	wantCheckFailure(t, err, CheckCredentialContainment)
	var cf *ConformanceFailure
	_ = errors.As(err, &cf)
	if !strings.Contains(cf.Reason, "omits workspace blob") {
		t.Errorf("reason = %q, want the omitted-blob failure", cf.Reason)
	}
	s.assertReaped(t, rt)
}

// TestSuiteFullRejectsEagerStart: Full's no-eager-start preamble catches a
// runtime whose CreateContainer executes the container (observed running after
// create), which would otherwise run the writer before the gate's pre-start
// isolation inspect.
func TestSuiteFullRejectsEagerStart(t *testing.T) {
	s, rt := newSuiteTest(t)
	rt.onInspect = func(id string, rep InspectReport) (InspectReport, error) {
		if id == namesFor(s.fx.RunID).Agent {
			rep.State = StateRunning // create eagerly started the container
		}
		return rep, nil
	}
	err := s.Full(context.Background())
	wantCheckFailure(t, err, CheckControlPlaneIsolation)
	var cf *ConformanceFailure
	_ = errors.As(err, &cf)
	if !strings.Contains(cf.Reason, "not observed stopped before execution") {
		t.Errorf("reason = %q, want the eager-start failure", cf.Reason)
	}
	s.assertReaped(t, rt)
}

// TestSuiteFullRejectsMountedEagerStart covers the path a stopped inspection
// of the finite handoff writer cannot distinguish: a runtime that executes only
// mounted creates synchronously. The mounted nonterminating liveness probe must
// still be observed running and fail before the credential is seeded.
func TestSuiteFullRejectsMountedEagerStart(t *testing.T) {
	s, rt := newSuiteTest(t)
	rt.onInspect = func(id string, rep InspectReport) (InspectReport, error) {
		if id == s.conformanceName("liveness") {
			rep.State = StateRunning
		}
		return rep, nil
	}
	err := s.Full(context.Background())
	wantCheckFailure(t, err, CheckControlPlaneIsolation)
	if !strings.Contains(err.Error(), "executed it before inspection") {
		t.Errorf("error = %q, want mounted eager-start failure", err)
	}
	s.assertReaped(t, rt)
}

func TestSuiteFullLivenessProbeUsesWriterMountTopology(t *testing.T) {
	s, rt := newSuiteTest(t)
	scriptHappyProbes(s, rt)
	var got []Mount
	rt.onCreateContainer = func(spec ContainerSpec) error {
		if spec.Name == s.conformanceName("liveness") {
			got = slices.Clone(spec.Mounts)
		}
		return nil
	}
	if err := s.Full(context.Background()); err != nil {
		t.Fatalf("Full = %v, want nil", err)
	}
	want := []Mount{
		{Type: MountVolume, Source: s.conformanceName("liveness-ws"), Target: s.b.cfg.WorkspaceTarget},
		{Type: MountVolume, Source: s.conformanceName("cred"), Target: s.fx.CredentialTarget, ReadOnly: true},
	}
	if !sameMounts(got, want) {
		t.Errorf("liveness mounts = %+v, want writer topology %+v", got, want)
	}
	s.assertReaped(t, rt)
}

// TestSuiteFullRejectsSmuggledFilename: a workspace file NAMED after the
// credential leaks the marker as a manifest path, invisible to the content-only
// blob scan; the manifest-shape check rejects the unexpected entry.
func TestSuiteFullRejectsSmuggledFilename(t *testing.T) {
	s, rt := newSuiteTest(t)
	// result.txt is the sentinel (sentinel check passes). A second regular file
	// is NAMED after the marker but its content is inert, so only the shape
	// check (not the content scan) can catch the leak. Bytewise order: the
	// uppercase marker precedes "result.txt".
	rt.exportTarPath = buildTar(t, manifestArchive(t, []manifestFile{
		{path: suiteMarker, body: "x\n"},
		{path: "result.txt", body: writerSentinel(s.fx.RunID) + "\n"},
	}))
	err := s.Full(context.Background())
	wantCheckFailure(t, err, CheckCredentialContainment)
	var cf *ConformanceFailure
	_ = errors.As(err, &cf)
	if !strings.Contains(cf.Reason, "unexpected manifest entry") {
		t.Errorf("reason = %q, want the unexpected-entry failure", cf.Reason)
	}
	if strings.Contains(err.Error(), suiteMarker) {
		t.Errorf("error re-emitted the credential marker: %q", err)
	}
	s.assertReaped(t, rt)
}

func TestSuiteFullRejectsChangedFixtureMode(t *testing.T) {
	s, rt := newSuiteTest(t)
	mode := "0755"
	// Rebuild the manifest fixture with executable normalized modes while
	// preserving both expected paths and contents.
	files := []manifestFile{
		{path: workspaceStateFile, body: workspaceStatePayload + "\n"},
		{path: writerResultPath, body: writerSentinel(s.fx.RunID) + "\n"},
	}
	archive := manifestArchive(t, files)
	var manifest export.Manifest
	if err := json.Unmarshal(archive[2].body, &manifest); err != nil {
		t.Fatal(err)
	}
	for i := range manifest.Entries {
		manifest.Entries[i].Mode = &mode
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	archive[2].body = raw
	rt.exportTarPath = buildTar(t, archive)
	err = s.Full(context.Background())
	wantCheckFailure(t, err, CheckCredentialContainment)
	if !strings.Contains(err.Error(), "manifest metadata does not exactly match") {
		t.Errorf("error = %q, want exact fixture-metadata failure", err)
	}
	s.assertReaped(t, rt)
}

// TestDefaultWriterGatesOnMarkerAndEmitsSentinel: the generated default writer
// verifies the token equals the seeded marker before producing output and emits
// this run's sentinel, so a realized-but-wrong credential aborts the writer and
// the export can be tied to this run.
func TestDefaultWriterGatesOnMarkerAndEmitsSentinel(t *testing.T) {
	s, _ := newSuiteTest(t)
	cmd := strings.Join(s.agentCommand, " ")
	if !strings.Contains(cmd, `test "$(cat `) || !strings.Contains(cmd, "= "+suiteMarker) {
		t.Errorf("default writer does not gate on the seeded marker: %q", cmd)
	}
	if !strings.Contains(cmd, writerSentinel(s.fx.RunID)) {
		t.Errorf("default writer does not emit the run sentinel: %q", cmd)
	}
}

// TestSuiteFullAuditCoincidentalMarkerNoSentinel: the audit rootfs export
// contains the credential marker only coincidentally (a short marker present in
// a base-image path or file) but not this run's audit sentinel, so the
// detached-volume audit fails closed instead of passing on a whole-rootfs match.
func TestSuiteFullAuditCoincidentalMarkerNoSentinel(t *testing.T) {
	s, rt := newSuiteTest(t)
	// Workspace export carries the writer sentinel (export half passes); the
	// audit export carries the marker but not the run-unique audit sentinel.
	rt.onExport = func(id string, dest io.Writer) error {
		if id == s.conformanceName("audit") {
			_, err := dest.Write([]byte("coincidental " + suiteMarker + " in base image"))
			return err
		}
		return nil
	}
	err := s.Full(context.Background())
	wantCheckFailure(t, err, CheckCredentialContainment)
	var cf *ConformanceFailure
	_ = errors.As(err, &cf)
	if !strings.Contains(cf.Reason, "validate credential-audit rootfs archive") {
		t.Errorf("reason = %q, want the malformed-audit-archive failure", cf.Reason)
	}
	s.assertReaped(t, rt)
}

// TestSuiteFullAuditRejectsBareSentinel proves the detached-volume result must
// be a valid rootfs tar with the sentinel at its exact path. Bare bytes carrying
// the public run sentinel are not evidence that the audit command ran.
func TestSuiteFullAuditRejectsBareSentinel(t *testing.T) {
	s, rt := newSuiteTest(t)
	rt.onExport = func(id string, dest io.Writer) error {
		if id == s.conformanceName("audit") {
			_, err := dest.Write([]byte(auditSentinel(s.fx.RunID) + "\n"))
			return err
		}
		return nil
	}
	err := s.Full(context.Background())
	wantCheckFailure(t, err, CheckCredentialContainment)
	if !strings.Contains(err.Error(), "validate credential-audit rootfs archive") {
		t.Errorf("error = %q, want strict archive-validation failure", err)
	}
	s.assertReaped(t, rt)
}

// TestSuiteFullAuditRejectsChangedCommand proves a realized credential mount
// cannot stand in for the marker-gated audit payload itself.
func TestSuiteFullAuditRejectsChangedCommand(t *testing.T) {
	s, rt := newSuiteTest(t)
	rt.onInspect = func(id string, rep InspectReport) (InspectReport, error) {
		if id == s.conformanceName("audit") {
			rep.Command = []string{"sh", "-c", "printf '%s\\n' forged > /tmp/forged"}
		}
		return rep, nil
	}
	err := s.Full(context.Background())
	wantCheckFailure(t, err, CheckCredentialContainment)
	if !strings.Contains(err.Error(), "did not realize the stopped suite-owned probe spec") {
		t.Errorf("error = %q, want unrealized audit-spec failure", err)
	}
	s.assertReaped(t, rt)
}

// TestSuiteFullExclusionUsesReadOnlyExporter verifies the negative probe
// exercises the real handoff conflict: exporter RO while the writer holds RW,
// not the weaker and different RW/RW conflict.
func TestSuiteFullExclusionUsesReadOnlyExporter(t *testing.T) {
	s, rt := newSuiteTest(t)
	scriptHappyProbes(s, rt)
	var got ContainerSpec
	rt.onCreateContainer = func(spec ContainerSpec) error {
		if spec.Name == s.conformanceName("excl-second") {
			got = cloneContainerSpec(spec)
		}
		return nil
	}
	if err := s.Full(context.Background()); err != nil {
		t.Fatalf("Full = %v, want nil", err)
	}
	if got.Image != s.b.cfg.ExporterImage || !slices.Equal(got.Command, nonterminatingProbeCommand()) {
		t.Errorf("second probe = image %q command %q, want exporter image with nonterminating probe command", got.Image, got.Command)
	}
	if len(got.Mounts) != 1 || !got.Mounts[0].ReadOnly {
		t.Errorf("second probe mounts = %+v, want one read-only workspace mount", got.Mounts)
	}
	s.assertReaped(t, rt)
}

// TestSuiteFullExclusionSecondInspectInconclusive: if the post-error inspect
// of the second container fails, the probe cannot confirm the second is not
// running and must fail closed rather than pass on the error string alone.
func TestSuiteFullExclusionSecondInspectInconclusive(t *testing.T) {
	s, rt := newSuiteTest(t)
	rt.onExport = passAudit(s)
	rt.onStart = func(id string) error {
		if id == s.conformanceName("excl-second") {
			return errors.New("VZErrorDomain Code=2: the storage device attachment is invalid")
		}
		return nil
	}
	secondInspects := 0
	rt.onInspect = func(id string, rep InspectReport) (InspectReport, error) {
		if id == s.conformanceName("excl-second") {
			secondInspects++
			if secondInspects >= 2 { // the post-error inspect
				return InspectReport{}, errors.New("XPC transiently unavailable")
			}
		}
		return rep, nil
	}
	err := s.Full(context.Background())
	wantCheckFailure(t, err, CheckWriterVolumeExclusion)
	var cf *ConformanceFailure
	_ = errors.As(err, &cf)
	if !strings.Contains(cf.Reason, "could not confirm") {
		t.Errorf("reason = %q, want the inconclusive-inspect failure", cf.Reason)
	}
	s.assertReaped(t, rt)
}

// TestSuiteFullExclusionWriterEvictedAfterRefusal: the exclusion holds only
// while a live writer keeps the rw mount. A runtime that resolved the conflict
// by evicting the holder (writer observed no longer running after the refusal)
// must fail closed, even though the second attach was refused and stopped.
func TestSuiteFullExclusionWriterEvictedAfterRefusal(t *testing.T) {
	s, rt := newSuiteTest(t)
	rt.onExport = passAudit(s)
	rt.onStart = func(id string) error {
		if id == s.conformanceName("excl-second") {
			return errors.New("VZErrorDomain Code=2: the storage device attachment is invalid")
		}
		return nil
	}
	// The writer would otherwise stay running across both post-start inspects; force it
	// stopped on the post-refusal re-check so the eviction is the sole cause.
	rt.runningInspects[s.conformanceName("excl-writer")] = 2
	writerInspects := 0
	rt.onInspect = func(id string, rep InspectReport) (InspectReport, error) {
		if id == s.conformanceName("excl-writer") {
			writerInspects++
			if writerInspects >= 3 { // the post-refusal re-check
				rep.State = StateStopped // the holder was evicted
			}
		}
		return rep, nil
	}
	err := s.Full(context.Background())
	wantCheckFailure(t, err, CheckWriterVolumeExclusion)
	var cf *ConformanceFailure
	_ = errors.As(err, &cf)
	if !strings.Contains(cf.Reason, "no longer holds the read-write mount") {
		t.Errorf("reason = %q, want the evicted-writer failure", cf.Reason)
	}
	s.assertReaped(t, rt)
}

// TestSuiteFullExclusionSecondNotStoppedAfterError: after a storage-attachment
// refusal the second container must be observed StateStopped to prove no VM
// holds the volume; any other state (here a runtime reporting "starting")
// leaves the exclusion unproven, so the probe fails closed rather than passing
// on "not running".
func TestSuiteFullExclusionSecondNotStoppedAfterError(t *testing.T) {
	s, rt := newSuiteTest(t)
	rt.onExport = passAudit(s)
	rt.onStart = func(id string) error {
		if id == s.conformanceName("excl-second") {
			return errors.New("VZErrorDomain Code=2: the storage device attachment is invalid")
		}
		return nil
	}
	secondInspects := 0
	rt.onInspect = func(id string, rep InspectReport) (InspectReport, error) {
		if id == s.conformanceName("excl-second") {
			secondInspects++
			if secondInspects >= 2 { // the post-error inspect
				rep.State = ContainerState("starting")
			}
		}
		return rep, nil
	}
	err := s.Full(context.Background())
	wantCheckFailure(t, err, CheckWriterVolumeExclusion)
	var cf *ConformanceFailure
	_ = errors.As(err, &cf)
	if !strings.Contains(cf.Reason, "not stopped after the attachment error") {
		t.Errorf("reason = %q, want the not-stopped fail-closed failure", cf.Reason)
	}
	s.assertReaped(t, rt)
}

func TestSuitePreJobSuccess(t *testing.T) {
	s, _ := newSuiteTest(t)
	if err := s.PreJob(context.Background()); err != nil {
		t.Fatalf("PreJob = %v, want nil", err)
	}
}

func TestSuitePreJobFailures(t *testing.T) {
	cases := []struct {
		name   string
		script func(*fakeRuntime)
	}{
		{
			"runtime unreachable",
			func(rt *fakeRuntime) {
				rt.onListVolumes = func([]VolumeSummary) ([]VolumeSummary, error) {
					return nil, errors.New("daemon down")
				}
			},
		},
		{
			"cannot create liveness container",
			func(rt *fakeRuntime) {
				rt.onCreateContainer = func(spec ContainerSpec) error {
					return errors.New("create refused")
				}
			},
		},
		{
			"liveness inspect wrong identity",
			func(rt *fakeRuntime) {
				rt.onInspect = func(_ string, rep InspectReport) (InspectReport, error) {
					rep.ID = "someone-else"
					return rep, nil
				}
			},
		},
		{
			// A runtime that accidentally started the liveness container at
			// create violates the no-VM pre-job contract; the long-lived payload
			// keeps it running, so the post-create state is not stopped.
			"liveness container started at create",
			func(rt *fakeRuntime) {
				rt.onInspect = func(_ string, rep InspectReport) (InspectReport, error) {
					rep.State = StateRunning
					return rep, nil
				}
			},
		},
		{
			"liveness command not realized",
			func(rt *fakeRuntime) {
				rt.onInspect = func(_ string, rep InspectReport) (InspectReport, error) {
					rep.Command = []string{"sh", "-c", "true"}
					return rep, nil
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, rt := newSuiteTest(t)
			tc.script(rt)
			err := s.PreJob(context.Background())
			wantCheckFailure(t, err, CheckPreJobProbe)
			// The runtime-unreachable case never creates anything; the others
			// must not leave the liveness container behind.
			s.assertReaped(t, rt)
		})
	}
}

// TestSuitePreJobFailsClosedOnLyingDelete: the runtime reports the liveness
// delete as successful but leaves the container behind; PreJob must prove
// absence and fail closed rather than trusting the delete call.
func TestSuitePreJobFailsClosedOnLyingDelete(t *testing.T) {
	s, rt := newSuiteTest(t)
	rt.onDeleteContainer = func(string) (bool, error) {
		return true, nil // report success, skip removal
	}
	err := s.PreJob(context.Background())
	// The deferred absence proof catches the survivor and fails closed as a
	// teardown conformance failure.
	wantCheckFailure(t, err, CheckTeardown)
	if !strings.Contains(err.Error(), "survived cleanup") {
		t.Errorf("error = %q, want the survived-cleanup failure", err)
	}
}

// TestSuitePreJobCatchesListingThatOmitsSurvivor ensures the cleanup proof does
// not trust one filtered list when direct inspection still sees the object.
func TestSuitePreJobCatchesListingThatOmitsSurvivor(t *testing.T) {
	s, rt := newSuiteTest(t)
	rt.onDeleteContainer = func(string) (bool, error) { return true, nil }
	rt.onListContainers = func([]ContainerSummary) ([]ContainerSummary, error) {
		return nil, nil
	}
	err := s.PreJob(context.Background())
	wantCheckFailure(t, err, CheckTeardown)
	if !strings.Contains(err.Error(), "survived cleanup") {
		t.Errorf("error = %q, want directly inspected survivor", err)
	}
}

// TestSuitePreJobDoesNotDeleteForeignCollision proves registering the reap
// before create is safe even when the deterministic name already belongs to a
// foreign object.
func TestSuitePreJobDoesNotDeleteForeignCollision(t *testing.T) {
	s, rt := newSuiteTest(t)
	name := s.conformanceName("prejob")
	rt.ctrs[name] = &fakeCtr{
		spec:    ContainerSpec{Name: name, Labels: []Label{{Key: "foreign", Value: "true"}}},
		created: "foreign-created",
	}
	err := s.PreJob(context.Background())
	wantCheckFailure(t, err, CheckPreJobProbe)
	if _, ok := rt.ctrs[name]; !ok {
		t.Fatal("foreign same-name container was deleted")
	}
}

// TestSuiteFullDoesNotDeleteForeignVolumeCollision is the volume analogue of
// the container collision: an already-existing credential-volume name is not
// authority to delete it after CreateVolume fails.
func TestSuiteFullDoesNotDeleteForeignVolumeCollision(t *testing.T) {
	s, rt := newSuiteTest(t)
	name := s.conformanceName("cred")
	rt.vols[name] = &fakeVol{
		labels:  []Label{{Key: "foreign", Value: "true"}},
		created: "foreign-created",
	}
	if err := s.Full(context.Background()); err == nil {
		t.Fatal("Full = nil, want create collision failure")
	}
	if _, ok := rt.vols[name]; !ok {
		t.Fatal("foreign same-name volume was deleted")
	}
}

func TestNewSuiteValidation(t *testing.T) {
	b := newHandoffFixture(t).backend(t)
	valid := SuiteFixture{
		AgentImage:       "example.test/agent@sha256:" + strings.Repeat("1", 64),
		CredentialMarker: suiteMarker,
		RunID:            "conf-run",
	}
	if _, err := NewSuite(b, valid); err != nil {
		t.Fatalf("valid fixture: NewSuite = %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*SuiteFixture)
	}{
		{"unpinned agent image", func(fx *SuiteFixture) { fx.AgentImage = "example.test/agent:latest" }},
		{"empty marker", func(fx *SuiteFixture) { fx.CredentialMarker = "" }},
		{"marker with shell metacharacters", func(fx *SuiteFixture) { fx.CredentialMarker = "x; rm -rf /" }},
		{"relative credential target", func(fx *SuiteFixture) { fx.CredentialTarget = "credentials" }},
		{"credential target with CLI delimiter", func(fx *SuiteFixture) { fx.CredentialTarget = "/creds,rw" }},
		{"credential target nested under workspace", func(fx *SuiteFixture) { fx.CredentialTarget = "/workspace/creds" }},
		{"credential target equal to workspace", func(fx *SuiteFixture) { fx.CredentialTarget = "/workspace" }},
		{"marker collides with writer sentinel", func(fx *SuiteFixture) { fx.CredentialMarker = "writer" }},
		{"marker substring of run-specific sentinel", func(fx *SuiteFixture) { fx.CredentialMarker = "conf" }},
		{"marker collides with state payload", func(fx *SuiteFixture) { fx.CredentialMarker = "durable" }},
		{"marker collides with result path", func(fx *SuiteFixture) { fx.CredentialMarker = "result" }},
		{"marker collides with nested state path", func(fx *SuiteFixture) { fx.CredentialMarker = "nested" }},
		{"marker collides with manifest schema", func(fx *SuiteFixture) { fx.CredentialMarker = "version" }},
		{"marker collides with manifest kind", func(fx *SuiteFixture) { fx.CredentialMarker = "regular" }},
		{"marker collides with digest vocabulary", func(fx *SuiteFixture) { fx.CredentialMarker = "sha256" }},
		{"marker collides with manifest filename", func(fx *SuiteFixture) { fx.CredentialMarker = "json" }},
		{"marker collides with blob directory", func(fx *SuiteFixture) { fx.CredentialMarker = "blob" }},
		{"credential target shadows audit marker path", func(fx *SuiteFixture) { fx.CredentialTarget = auditMarkerPath("conf-run") }},
		{"bad run id", func(fx *SuiteFixture) { fx.RunID = "Conf/Run" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := valid
			tc.mutate(&fx)
			if _, err := NewSuite(b, fx); !errors.Is(err, ErrInvalidConfig) {
				t.Errorf("NewSuite = %v, want ErrInvalidConfig", err)
			}
		})
	}

	if _, err := NewSuite(nil, valid); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("NewSuite(nil) = %v, want ErrInvalidConfig", err)
	}
}

func TestIsAttachmentExclusion(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"spike message", errors.New("VZErrorDomain Code=2: The storage device attachment is invalid"), true},
		{"storage device only", errors.New("the storage device attachment failed"), false},
		{"vz domain code 2 no storage wording", errors.New("VZErrorDomain Code=2: attachment rejected"), false},
		{"vz domain code 20 not 2", errors.New("VZErrorDomain Code=20: attachment rejected"), false},
		{"vz domain code 21 not 2", errors.New("VZErrorDomain Code=21: attachment rejected"), false},
		{"vz code 2 nested under a different top-level code", errors.New("VZErrorDomain Code=7: attachment failed; underlying NSError Code=2"), false},
		{"vz code 2 in a nested vzerrordomain, not the first", errors.New("VZErrorDomain Code=7: attachment failed; underlying VZErrorDomain Code=2"), false},
		{"storage-device wording cannot rescue a non-2 vz code", errors.New("VZErrorDomain Code=20: storage device attachment failed"), false},
		{"vz domain attachment no code", errors.New("VZErrorDomain: attachment rejected"), false},
		{"unrelated vz attachment code", errors.New("VZErrorDomain Code=7: network attachment failed"), false},
		{"unrelated vz error", errors.New("VZErrorDomain Code=5: virtual machine failed to start"), false},
		{"lone attachment word", errors.New("email attachment too large"), false},
		{"out of memory", errors.New("container start failed: out of memory"), false},
		{"context canceled", context.Canceled, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAttachmentExclusion(tc.err); got != tc.want {
				t.Errorf("isAttachmentExclusion(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestSuiteFullExclusionUnrelatedStartFailure: the second attach fails, but
// not with the storage-attachment exclusion, so the probe must fail closed
// rather than pass on an unrelated error.
func TestSuiteFullExclusionUnrelatedStartFailure(t *testing.T) {
	s, rt := newSuiteTest(t)
	rt.onExport = passAudit(s)
	rt.onStart = func(id string) error {
		if id == s.conformanceName("excl-second") {
			return errors.New("container start failed: out of memory")
		}
		return nil
	}
	err := s.Full(context.Background())
	wantCheckFailure(t, err, CheckWriterVolumeExclusion)
	var cf *ConformanceFailure
	_ = errors.As(err, &cf)
	if !strings.Contains(cf.Reason, "not with the storage-device attachment") {
		t.Errorf("reason = %q, want the unrelated-failure fail-closed", cf.Reason)
	}
	s.assertReaped(t, rt)
}

func TestMarkerScanWriter(t *testing.T) {
	marker := []byte("SECRET")

	t.Run("split across writes", func(t *testing.T) {
		w := &markerScanWriter{marker: marker}
		mustWrite(t, w, "noise SEC")
		mustWrite(t, w, "RET more")
		if !w.found {
			t.Error("marker split across chunk boundary not found")
		}
	})

	t.Run("absent", func(t *testing.T) {
		w := &markerScanWriter{marker: marker}
		mustWrite(t, w, "nothing to see")
		mustWrite(t, w, " here at all")
		if w.found {
			t.Error("reported a marker that is not present")
		}
	})

	t.Run("whole in one write", func(t *testing.T) {
		w := &markerScanWriter{marker: marker}
		mustWrite(t, w, "xxSECRETyy")
		if !w.found {
			t.Error("marker in a single write not found")
		}
	})
}

func TestDirMetadataContainsMarker(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o750); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		marker string
		want   bool
	}{
		{"blob", true},
		{"sha256", true},
		{"not-present", false},
	} {
		got, err := dirMetadataContainsMarker(dir, tc.marker)
		if err != nil {
			t.Fatalf("dirMetadataContainsMarker(%q) = %v", tc.marker, err)
		}
		if got != tc.want {
			t.Errorf("dirMetadataContainsMarker(%q) = %v, want %v", tc.marker, got, tc.want)
		}
	}
}

func TestAuditArchiveHasSentinel(t *testing.T) {
	wantPath := auditMarkerPath("conf-run")
	wantBody := auditSentinel("conf-run") + "\n"
	cases := []struct {
		name    string
		entries []tarEntry
		found   bool
		wantErr bool
	}{
		{
			name:    "exact regular file",
			entries: []tarEntry{{name: strings.TrimPrefix(wantPath, "/"), body: []byte(wantBody)}},
			found:   true,
		},
		{
			name:    "sentinel at wrong path",
			entries: []tarEntry{{name: "tmp/unrelated", body: []byte(wantBody)}},
		},
		{
			name: "duplicate sentinel",
			entries: []tarEntry{
				{name: strings.TrimPrefix(wantPath, "/"), body: []byte(wantBody)},
				{name: strings.TrimPrefix(wantPath, "/"), body: []byte(wantBody)},
			},
			wantErr: true,
		},
		{
			name:    "sentinel is symlink",
			entries: []tarEntry{{name: strings.TrimPrefix(wantPath, "/"), typeflag: tar.TypeSymlink, linkname: "elsewhere"}},
			wantErr: true,
		},
		{
			name:    "wrong content",
			entries: []tarEntry{{name: strings.TrimPrefix(wantPath, "/"), body: []byte("forged\n")}},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := buildTar(t, tc.entries)
			f, err := os.Open(p) //nolint:gosec // test temp path
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close() //nolint:errcheck // test read handle
			found, err := auditArchiveHasSentinel(f, wantPath, wantBody)
			if (err != nil) != tc.wantErr || found != tc.found {
				t.Fatalf("auditArchiveHasSentinel = (%v, %v), want (%v, error=%v)", found, err, tc.found, tc.wantErr)
			}
		})
	}
}

func mustWrite(t *testing.T, w io.Writer, s string) {
	t.Helper()
	n, err := w.Write([]byte(s))
	if err != nil || n != len(s) {
		t.Fatalf("Write(%q) = (%d, %v), want (%d, nil)", s, n, err, len(s))
	}
}

func TestDirContainsMarker(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "clean.txt", "nothing here")
	found, err := dirContainsMarker(dir, suiteMarker)
	if err != nil || found {
		t.Fatalf("dirContainsMarker(clean) = (%v, %v), want (false, nil)", found, err)
	}
	writeFile(t, dir, "leak.txt", "prefix "+suiteMarker+" suffix")
	found, err = dirContainsMarker(dir, suiteMarker)
	if err != nil || !found {
		t.Fatalf("dirContainsMarker(leak) = (%v, %v), want (true, nil)", found, err)
	}
}

// markerArchive is a valid exported rootfs whose one released blob carries the
// marker, so an extraction of it leaves the marker in the output directory.
func markerArchive(t *testing.T, marker string) []tarEntry {
	t.Helper()
	blob := []byte(marker + "\n")
	manifest, hexDigest := fixtureManifest(t, blob)
	return []tarEntry{
		{name: "handoff-proof.txt", body: validProof()},
		{name: "handoff/", typeflag: tar.TypeDir},
		{name: "handoff/manifest.json", body: manifest},
		{name: "handoff/blobs/", typeflag: tar.TypeDir},
		{name: "handoff/blobs/sha256/", typeflag: tar.TypeDir},
		{name: "handoff/blobs/sha256/" + hexDigest, body: blob},
	}
}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
