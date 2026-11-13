package importer

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// evidenceNowFixture is the fixed stamp buildClaims tests pin as each claim's
// EvidenceMetadata.CreatedAt, so the persisted claim is byte-deterministic.
var evidenceNowFixture = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

// Evidence blob contents. Each carries a real image magic signature so
// validateEvidenceType accepts it; the trailing bytes are inert filler. The
// script content is a non-image used to prove a magic/type mismatch.
const (
	pngContent    = "\x89PNG\r\n\x1a\nfreeside-fake-png-body"
	jpegContent   = "\xff\xd8\xff\xe0freeside-fake-jpeg-body"
	gifContent    = "GIF89afreeside-fake-gif-body"
	webpContent   = "RIFF\x2c\x00\x00\x00WEBPfreeside-fake-webp-body"
	jsonlContent  = "{\"type\":\"assistant\"}\n{\"type\":\"result\",\"subtype\":\"success\"}\n"
	scriptContent = "#!/bin/sh\necho not-an-image\n"
)

// A fixed, agent-declared head for head-bound evidence. The importer passes it
// through verbatim (the publisher verifies head binding later, §5.15), so a
// literal keeps the golden deterministic.
const evidenceSourceHead = "0123456789abcdef0123456789abcdef01234567"

// evidenceEntry builds a valid head-independent entry bound to content by real
// digest and size, with agent provenance.
func evidenceEntry(label, mediaType, content string) export.EvidenceEntry {
	return export.EvidenceEntry{
		Label:     label,
		MediaType: mediaType,
		Size:      int64(len(content)),
		Digest:    sha256Digest(content),
		Provenance: export.EvidenceProvenance{
			ProducerClass:        export.EvidenceProducerAgent,
			ProducerInvocationID: "inv-" + label,
			HeadBinding:          export.EvidenceHeadIndependent,
			SensitivityClass:     export.EvidenceSensitivityNormal,
		},
	}
}

// headBoundEntry is evidenceEntry bound to a source head, carrying the
// sensitive class, so a golden exercises both head-binding modes and a
// non-default sensitivity.
func headBoundEntry(label, mediaType, content string) export.EvidenceEntry {
	e := evidenceEntry(label, mediaType, content)
	e.Provenance.HeadBinding = export.EvidenceHeadBound
	e.Provenance.SourceHeadSHA = evidenceSourceHead
	e.Provenance.SensitivityClass = export.EvidenceSensitivitySensitive
	return e
}

// evidenceManifest assembles a manifest with entries in canonical (bytewise by
// label) order, so Encode accepts it.
func evidenceManifest(entries ...export.EvidenceEntry) export.EvidenceManifest {
	sort.Slice(entries, func(i, j int) bool {
		return bytes.Compare([]byte(entries[i].Label), []byte(entries[j].Label)) < 0
	})
	return export.EvidenceManifest{Version: export.EvidenceManifestVersion, Entries: entries}
}

// rawEvidenceJSON marshals a manifest to the canonical byte form WITHOUT
// validating it, so a test can feed the importer a well-formed-looking but
// hostile manifest (unsorted entries, a bad version, a forged producer or
// provenance) that Encode would reject.
func rawEvidenceJSON(t *testing.T, em export.EvidenceManifest) []byte {
	t.Helper()
	body, err := json.MarshalIndent(em, "", "  ")
	if err != nil {
		t.Fatalf("marshal raw evidence: %v", err)
	}
	return append(body, '\n')
}

// evidenceBlob is one blob to plant under the evidence store, keyed by the
// digest name the manifest references.
type evidenceBlob struct {
	digest  export.Digest
	content string
}

func blobFor(content string) evidenceBlob {
	return evidenceBlob{digest: sha256Digest(content), content: content}
}

// writeEvidence plants an evidence channel onto an existing handoff directory:
// the raw evidence.json plus one file per blob under evidence/sha256/. It
// bypasses Encode so hostile manifests can be written, and writes exactly the
// blobs given so orphan and missing-blob layouts can be constructed.
func writeEvidence(t *testing.T, handoffDir string, manifestBytes []byte, blobs ...evidenceBlob) {
	t.Helper()
	sha256Dir := filepath.Join(handoffDir, export.EvidenceBlobsDirname, "sha256")
	if err := os.MkdirAll(sha256Dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, b := range blobs {
		name := filepath.Join(sha256Dir, strings.TrimPrefix(string(b.digest), "sha256:"))
		if err := os.WriteFile(name, []byte(b.content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(handoffDir, export.EvidenceFilename), manifestBytes, 0o600); err != nil {
		t.Fatal(err)
	}
}

// evidenceFixture builds a base repo, an exported repo-change handoff over the
// given workspace files, and a fresh clone to import onto. Callers add an
// evidence channel to the returned handoff before importing.
func evidenceFixture(t *testing.T, wsFiles map[string]string) (clone, base, handoff string) {
	t.Helper()
	checkout, base := initBaseRepo(t, map[string]string{"a.txt": "old\n"})
	ws := t.TempDir()
	for path, content := range wsFiles {
		full := filepath.Join(ws, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	handoff = exportWorkspace(t, ws)
	clone = cloneAtBase(t, checkout)
	return clone, base, handoff
}

// writeEvidenceFile writes just evidence.json into a bare temp dir, for
// unit-testing loadEvidenceManifest without a full handoff.
func writeEvidenceFile(t *testing.T, body []byte) string {
	t.Helper()
	dir := t.TempDir()
	if body != nil {
		if err := os.WriteFile(filepath.Join(dir, export.EvidenceFilename), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestLoadEvidenceManifest covers the manifest-intake boundary: an absent
// channel is not an error, a valid manifest decodes, and every framing forgery
// or intake-cap breach fails closed. media/type and blob checks are exercised
// through Import below; this table isolates the decode boundary.
func TestLoadEvidenceManifest(t *testing.T) {
	t.Parallel()
	valid := evidenceManifest(evidenceEntry("shot", "image/png", pngContent))
	validBytes, err := valid.Encode()
	if err != nil {
		t.Fatalf("encode valid manifest: %v", err)
	}

	// A two-entry manifest for the entry-count cap.
	twoEntries := evidenceManifest(
		evidenceEntry("a", "image/png", pngContent),
		evidenceEntry("b", "image/jpeg", jpegContent),
	)

	// Forged producer class: agent is the only valid class at this seam.
	forgedProducer := valid
	forgedProducer.Entries = []export.EvidenceEntry{cloneEntry(valid.Entries[0], func(e *export.EvidenceEntry) {
		e.Provenance.ProducerClass = "verifier"
	})}

	// Head-shape forgeries.
	headBoundNoHead := valid
	headBoundNoHead.Entries = []export.EvidenceEntry{cloneEntry(valid.Entries[0], func(e *export.EvidenceEntry) {
		e.Provenance.HeadBinding = export.EvidenceHeadBound // leaves source_head_sha empty
	})}
	headIndepWithHead := valid
	headIndepWithHead.Entries = []export.EvidenceEntry{cloneEntry(valid.Entries[0], func(e *export.EvidenceEntry) {
		e.Provenance.SourceHeadSHA = evidenceSourceHead // head_independent must not carry one
	})}

	// Non-canonical entry order.
	unsorted := export.EvidenceManifest{Version: export.EvidenceManifestVersion, Entries: []export.EvidenceEntry{
		evidenceEntry("b", "image/jpeg", jpegContent),
		evidenceEntry("a", "image/png", pngContent),
	}}

	badVersion := valid
	badVersion.Version = "freeside.export.evidence/v99"

	cases := []struct {
		name        string
		body        []byte
		pol         Policy
		wantPresent bool
		wantErr     error
	}{
		{name: "absent", body: nil, wantPresent: false, wantErr: nil},
		{name: "valid", body: validBytes, wantPresent: true, wantErr: nil},
		{name: "forged producer class", body: rawEvidenceJSON(t, forgedProducer), wantErr: export.ErrInvalidEvidenceProducer},
		{name: "injected publish_eligible", body: bytes.Replace(validBytes, []byte(`"producer_class": "agent"`), []byte(`"publish_eligible": false,
        "producer_class": "agent"`), 1), wantErr: ErrEvidenceInvalid},
		{name: "injected recipe digest", body: bytes.Replace(validBytes, []byte(`"producer_class": "agent"`), []byte(`"verification_recipe_digest": "sha256:00",
        "producer_class": "agent"`), 1), wantErr: ErrEvidenceInvalid},
		{name: "head_bound without source", body: rawEvidenceJSON(t, headBoundNoHead), wantErr: export.ErrProvenanceInconsistent},
		{name: "head_independent with source", body: rawEvidenceJSON(t, headIndepWithHead), wantErr: export.ErrProvenanceInconsistent},
		{name: "non-canonical order", body: rawEvidenceJSON(t, unsorted), wantErr: export.ErrEvidenceNotCanonical},
		{name: "duplicate key", body: []byte(strings.Replace(string(validBytes), `"version": "`+export.EvidenceManifestVersion+`",`, `"version": "`+export.EvidenceManifestVersion+`",
  "version": "`+export.EvidenceManifestVersion+`",`, 1)), wantErr: strictjson.ErrDuplicateName},
		{name: "trailing content", body: append(append([]byte{}, validBytes...), []byte("{}\n")...), wantErr: export.ErrTrailingContent},
		{name: "invalid utf8", body: append(append([]byte{}, validBytes...), 0xff), wantErr: export.ErrInvalidUTF8},
		{name: "compact non-canonical", body: mustMarshalCompact(t, valid), wantErr: export.ErrNotCanonicalEncoding},
		{name: "unknown version", body: rawEvidenceJSON(t, badVersion), wantErr: export.ErrUnknownEvidenceVersion},
		{name: "oversized manifest", body: validBytes, pol: Policy{MaxManifestBytes: 8}, wantErr: ErrManifestTooLarge},
		{name: "entry count over cap", body: rawEvidenceJSON(t, twoEntries), pol: Policy{MaxEntries: 1}, wantErr: ErrManifestTooLarge},
		// The count cap takes precedence over a per-entry content error: an
		// over-count manifest with a forged entry fails as too-large before the
		// canonical decode ever validates that entry.
		{name: "over-cap precedes content error", body: rawEvidenceJSON(t, evidenceManifest(
			cloneEntry(evidenceEntry("a", "image/png", pngContent), func(e *export.EvidenceEntry) { e.Provenance.ProducerClass = "verifier" }),
			evidenceEntry("b", "image/jpeg", jpegContent),
		)), pol: Policy{MaxEntries: 1}, wantErr: ErrManifestTooLarge},
		// The count pass reads only the first JSON value, so appending a trailing
		// value cannot hide an over-cap entries array behind it.
		{name: "over-cap with trailing content", body: append(rawEvidenceJSON(t, twoEntries), []byte("{}")...), pol: Policy{MaxEntries: 1}, wantErr: ErrManifestTooLarge},
		// The count pass sums every "entries" key, so a duplicate key cannot hide
		// a large first array behind a small last one (which json.Unmarshal's
		// last-wins would undercount).
		{name: "over-cap behind duplicate entries key", body: dupEntriesKeyJSON(t, evidenceEntry("a", "image/png", pngContent), evidenceEntry("b", "image/jpeg", jpegContent)), pol: Policy{MaxEntries: 1}, wantErr: ErrManifestTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeEvidenceFile(t, tc.body)
			em, present, err := loadEvidenceManifest(dir, tc.pol.withDefaults())
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if present != tc.wantPresent {
				t.Fatalf("present = %v, want %v", present, tc.wantPresent)
			}
			if present && len(em.Entries) != 1 {
				t.Fatalf("entries = %d, want 1", len(em.Entries))
			}
		})
	}
}

func cloneEntry(e export.EvidenceEntry, mutate func(*export.EvidenceEntry)) export.EvidenceEntry {
	mutate(&e)
	return e
}

func mustMarshalCompact(t *testing.T, em export.EvidenceManifest) []byte {
	t.Helper()
	body, err := json.Marshal(em)
	if err != nil {
		t.Fatalf("marshal compact: %v", err)
	}
	return body
}

// dupEntriesKeyJSON builds a hostile manifest with two "entries" keys, the shape
// json.Unmarshal resolves last-wins: the structural count must sum both, not
// trust the last.
func dupEntriesKeyJSON(t *testing.T, a, b export.EvidenceEntry) []byte {
	t.Helper()
	ab, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal entry a: %v", err)
	}
	bb, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal entry b: %v", err)
	}
	return []byte(`{"version":"` + export.EvidenceManifestVersion + `","entries":[` + string(ab) + `],"entries":[` + string(bb) + `]}`)
}

// TestValidateEvidenceType pins the evidence media allow-set: each supported
// image validates against its magic, JSON Lines validates record by record,
// and mismatched, malformed, unlisted, scriptable, or truncated content fails
// closed.
func TestValidateEvidenceType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		media   string
		content string
		wantErr bool
	}{
		{"png", "image/png", pngContent, false},
		{"jpeg", "image/jpeg", jpegContent, false},
		{"gif", "image/gif", gifContent, false},
		{"webp", "image/webp", webpContent, false},
		{"json lines transcript", "application/jsonl", jsonlContent, false},
		{"json lines final newline optional", "application/jsonl", `{"type":"result"}`, false},
		{"json lines malformed record", "application/jsonl", "{\"type\":\"assistant\"}\nnot-json\n", true},
		{"json lines blank record", "application/jsonl", "{\"type\":\"assistant\"}\n\n", true},
		{"json lines empty", "application/jsonl", "", true},
		{"json lines invalid utf8", "application/jsonl", "{\"text\":\"\xff\"}\n", true},
		{"markdown", "text/markdown", "Changed **one** path.\n", false},
		{"markdown oversized valid", "text/markdown", strings.Repeat("x", 1<<16+1), false},
		{"markdown empty", "text/markdown", "", true},
		{"markdown invalid utf8", "text/markdown", "summary \xff", true},
		{"markdown invalid utf8 after stream chunk", "text/markdown", strings.Repeat("x", 64<<10) + "\xff", true},
		{"png declared but script content", "image/png", scriptContent, true},
		{"json lines declared but png content", "application/jsonl", pngContent, true},
		{"unlisted media type", "application/zip", pngContent, true},
		{"svg excluded", "image/svg+xml", "<svg/>", true},
		{"truncated below magic", "image/png", "\x89PN", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "blob")
			if err := os.WriteFile(p, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			entry := export.EvidenceEntry{Label: "x", MediaType: tc.media, Size: int64(len(tc.content)), Digest: sha256Digest(tc.content)}
			err := validateEvidenceType(entry, p)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, ErrEvidenceMediaMismatch) {
				t.Fatalf("err = %v, want ErrEvidenceMediaMismatch", err)
			}
		})
	}
}

func TestBuildClaimsInlinesConformingMarkdown(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		content     string
		sensitivity export.EvidenceSensitivityClass
		scanBytes   int64
		wantText    bool
		wantSecret  bool
	}{
		{name: "normal", content: "Summary with **context**.", sensitivity: export.EvidenceSensitivityNormal, wantText: true},
		{name: "sensitive", content: "Summary with **context**.", sensitivity: export.EvidenceSensitivitySensitive, wantText: true},
		{name: "secret stays referenced", content: "token: ghp_" + strings.Repeat("A", 36), sensitivity: export.EvidenceSensitivitySensitive, wantSecret: true},
		{name: "over secret scan cap stays referenced", content: strings.Repeat("x", 2048), sensitivity: export.EvidenceSensitivitySensitive, scanBytes: 1024},
		{name: "high sensitivity stays referenced", content: "Summary with **context**.", sensitivity: export.EvidenceSensitivityHigh},
		{name: "oversized stays referenced", content: strings.Repeat("x", 1<<16+1), sensitivity: export.EvidenceSensitivitySensitive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "blob")
			if err := os.WriteFile(p, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			entry := evidenceEntry(export.SummaryEvidenceLabel, "text/markdown", tc.content)
			entry.Provenance.SensitivityClass = tc.sensitivity
			claims, findings, _, err := buildClaims(evidenceManifest(entry), map[export.Digest]blobInfo{
				entry.Digest: {size: entry.Size, verifiedPath: p},
			}, Options{Policy: Policy{SecretMaxScanBytes: tc.scanBytes}, Now: evidenceNowFixture})
			if err != nil {
				t.Fatalf("buildClaims: %v", err)
			}
			if got := len(findings) == 1 && findings[0].Kind == FindingSecret &&
				findings[0].Path == export.SummaryEvidenceLabel; got != tc.wantSecret {
				t.Fatalf("secret findings = %+v, want secret = %v", findings, tc.wantSecret)
			}
			if len(claims) != 1 {
				t.Fatalf("claims = %d, want 1", len(claims))
			}
			if (claims[0].Text != nil) != tc.wantText {
				t.Fatalf("claim text = %#v, want present = %v", claims[0].Text, tc.wantText)
			}
			if claims[0].Text != nil {
				if claims[0].Text.MediaType != "text/markdown" || claims[0].Text.Content != tc.content {
					t.Fatalf("claim text = %#v", claims[0].Text)
				}
				if claims[0].Digest != claims[0].Text.ComputeDigest() {
					t.Fatalf("claim digest %q does not bind inline text", claims[0].Digest)
				}
			}
		})
	}
}

// TestImportEvidenceCleanEndToEnd is the happy path: a repo change plus a valid
// two-entry evidence channel (both head-binding modes, two image types) imports
// to a clean commit with two labeled agent claims and no findings.
func TestImportEvidenceCleanEndToEnd(t *testing.T) {
	t.Parallel()
	clone, base, handoff := evidenceFixture(t, map[string]string{"a.txt": "new\n"})
	em := evidenceManifest(
		evidenceEntry("after-shot", "image/png", pngContent),
		headBoundEntry("before-shot", "image/jpeg", jpegContent),
	)
	body, err := em.Encode()
	if err != nil {
		t.Fatalf("encode evidence: %v", err)
	}
	writeEvidence(t, handoff, body, blobFor(pngContent), blobFor(jpegContent))

	res, err := Import(t.Context(), handoff, clone, testImportOptions(base))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.CommitSHA == "" {
		t.Fatal("clean evidence import produced no commit")
	}
	if len(res.Findings) != 0 {
		t.Fatalf("clean evidence import produced findings: %+v", res.Findings)
	}
	if len(res.Claims) != 2 {
		t.Fatalf("claims = %d, want 2", len(res.Claims))
	}
	for _, c := range res.Claims {
		want := agentArtifactID(c.Provenance, export.Digest(c.Digest))
		if c.Artifact != want {
			t.Errorf("claim %q artifact id %q != %q", c.Label, c.Artifact, want)
		}
	}
	goldenResult(t, "import_evidence_result", res)
}

func TestImportEvidenceSecretBecomesNonInliningFinding(t *testing.T) {
	t.Parallel()
	clone, base, handoff := evidenceFixture(t, map[string]string{"a.txt": "new\n"})
	content := "Token: ghp_" + strings.Repeat("A", 36)
	entry := evidenceEntry(export.SummaryEvidenceLabel, "text/markdown", content)
	entry.Provenance.SensitivityClass = export.EvidenceSensitivitySensitive
	body, err := evidenceManifest(entry).Encode()
	if err != nil {
		t.Fatalf("encode evidence: %v", err)
	}
	writeEvidence(t, handoff, body, blobFor(content))

	res, err := Import(t.Context(), handoff, clone, testImportOptions(base))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(res.Claims) != 1 || res.Claims[0].Text != nil {
		t.Fatalf("secret-bearing claims = %+v", res.Claims)
	}
	if len(res.Findings) != 1 || res.Findings[0].Kind != FindingSecret ||
		res.Findings[0].Path != export.SummaryEvidenceLabel || res.Findings[0].Rule != "github_token" {
		t.Fatalf("secret-bearing evidence findings = %+v", res.Findings)
	}
	encoded, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("ghp_")) {
		t.Fatal("import result leaked the matched credential")
	}
}

func TestAgentArtifactIdentityIncludesCompleteProvenance(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "same.png")
	if err := os.WriteFile(path, []byte(pngContent), 0o600); err != nil {
		t.Fatalf("write evidence fixture: %v", err)
	}
	entry := evidenceEntry("shot", "image/png", pngContent)
	blobs := map[export.Digest]blobInfo{
		entry.Digest: {size: entry.Size, verifiedPath: path},
	}
	first, findings, _, err := buildClaims(
		evidenceManifest(entry), blobs, Options{Now: evidenceNowFixture},
	)
	if err != nil {
		t.Fatalf("build first claim: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("build first claim findings: %+v", findings)
	}
	// #922: the importer carries the daemon-validated media type and size onto
	// the claim's evidence metadata instead of discarding them; the entry is an
	// image/png, and the metadata rides the claim channel.
	if len(first) != 1 {
		t.Fatalf("expected one claim, got %d", len(first))
	}
	if first[0].Metadata.MediaType != domain.EvidenceMediaImagePNG {
		t.Fatalf("claim media_type = %q, want image/png", first[0].Metadata.MediaType)
	}
	if first[0].Metadata.SizeBytes != entry.Size {
		t.Fatalf("claim size_bytes = %d, want %d", first[0].Metadata.SizeBytes, entry.Size)
	}
	if first[0].Metadata.Source != domain.EvidenceSourceClaim {
		t.Fatalf("claim metadata source = %q, want claim", first[0].Metadata.Source)
	}
	if first[0].Metadata.CreatedAt != evidenceNowFixture {
		t.Fatalf("claim created_at = %s, want %s", first[0].Metadata.CreatedAt, evidenceNowFixture)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*export.EvidenceProvenance)
	}{
		{"producer invocation", func(p *export.EvidenceProvenance) {
			p.ProducerInvocationID = "inv-other"
		}},
		{"head binding and source", func(p *export.EvidenceProvenance) {
			p.HeadBinding = export.EvidenceHeadBound
			p.SourceHeadSHA = evidenceSourceHead
		}},
		{"sensitivity", func(p *export.EvidenceProvenance) {
			p.SensitivityClass = export.EvidenceSensitivitySensitive
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := entry
			tc.mutate(&changed.Provenance)
			second, findings, _, err := buildClaims(evidenceManifest(changed), blobs, Options{Now: evidenceNowFixture})
			if err != nil {
				t.Fatalf("build changed claim: %v", err)
			}
			if len(findings) != 0 {
				t.Fatalf("build changed claim findings: %+v", findings)
			}
			if first[0].Digest != second[0].Digest {
				t.Fatal("identical evidence bytes produced different content digests")
			}
			if first[0].Artifact == second[0].Artifact {
				t.Fatal("different artifact provenance reused one immutable artifact id")
			}
		})
	}
}

// TestImportNoEvidenceChannel proves the channel is optional: a handoff with no
// evidence.json imports exactly as before, with an empty claim list.
func TestImportNoEvidenceChannel(t *testing.T) {
	t.Parallel()
	clone, base, handoff := evidenceFixture(t, map[string]string{"a.txt": "new\n"})
	res, err := Import(t.Context(), handoff, clone, testImportOptions(base))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.CommitSHA == "" {
		t.Fatal("import produced no commit")
	}
	if len(res.Claims) != 0 {
		t.Fatalf("claims = %+v, want none", res.Claims)
	}
}

// The evidence channel fails the whole import closed on any hostile blob or
// forged type. Each case asserts a typed error and no Result.
func TestImportEvidenceHostileBlobs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		build   func(t *testing.T, handoff string)
		pol     func(Options) Options
		wantErr error
	}{
		{
			name: "media type magic mismatch",
			build: func(t *testing.T, handoff string) {
				e := evidenceEntry("shot", "image/png", scriptContent) // declares png, content is a script
				writeEvidence(t, handoff, rawEvidenceJSON(t, evidenceManifest(e)), blobFor(scriptContent))
			},
			wantErr: ErrEvidenceMediaMismatch,
		},
		{
			name: "unlisted media type",
			build: func(t *testing.T, handoff string) {
				e := evidenceEntry("shot", "application/zip", pngContent)
				writeEvidence(t, handoff, rawEvidenceJSON(t, evidenceManifest(e)), blobFor(pngContent))
			},
			wantErr: ErrEvidenceMediaMismatch,
		},
		{
			name: "digest mismatch",
			build: func(t *testing.T, handoff string) {
				e := evidenceEntry("shot", "image/png", pngContent)
				// Blob at the referenced digest name holds different content of
				// the SAME length, so the size check passes and the digest check
				// is what fails closed.
				tampered := pngContent[:len(pngContent)-1] + "X"
				writeEvidence(t, handoff, rawEvidenceJSON(t, evidenceManifest(e)), evidenceBlob{digest: e.Digest, content: tampered})
			},
			wantErr: ErrDigestMismatch,
		},
		{
			name: "size mismatch",
			build: func(t *testing.T, handoff string) {
				e := evidenceEntry("shot", "image/png", pngContent)
				e.Size = int64(len(pngContent)) + 5 // declared size overstates the blob
				writeEvidence(t, handoff, rawEvidenceJSON(t, evidenceManifest(e)), blobFor(pngContent))
			},
			wantErr: ErrSizeMismatch,
		},
		{
			name: "oversized evidence blob",
			build: func(t *testing.T, handoff string) {
				e := evidenceEntry("shot", "image/png", pngContent)
				writeEvidence(t, handoff, rawEvidenceJSON(t, evidenceManifest(e)), blobFor(pngContent))
			},
			pol:     func(o Options) Options { o.Policy.MaxEvidenceBlobBytes = 4; return o },
			wantErr: ErrBlobTooLarge,
		},
		{
			name: "orphan evidence blob",
			build: func(t *testing.T, handoff string) {
				e := evidenceEntry("shot", "image/png", pngContent)
				// An extra unreferenced blob beside the referenced one.
				writeEvidence(t, handoff, rawEvidenceJSON(t, evidenceManifest(e)), blobFor(pngContent), blobFor("unreferenced junk"))
			},
			wantErr: ErrOrphanBlob,
		},
		{
			name: "missing evidence blob",
			build: func(t *testing.T, handoff string) {
				e := evidenceEntry("shot", "image/png", pngContent)
				writeEvidence(t, handoff, rawEvidenceJSON(t, evidenceManifest(e))) // no blob written
			},
			wantErr: ErrMissingBlob,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clone, base, handoff := evidenceFixture(t, map[string]string{"a.txt": "new\n"})
			tc.build(t, handoff)
			opts := testImportOptions(base)
			if tc.pol != nil {
				opts = tc.pol(opts)
			}
			res, err := Import(t.Context(), handoff, clone, opts)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if res.CommitSHA != "" || res.Claims != nil {
				t.Fatalf("hostile evidence still produced a result: %+v", res)
			}
		})
	}
}

// TestImportEvidenceDirWithoutManifest proves an absent evidence channel is
// exactly the pre-evidence layout: a stray or planted evidence/ store with no
// evidence.json is an orphan, not a silently accepted empty second channel.
func TestImportEvidenceDirWithoutManifest(t *testing.T) {
	t.Parallel()
	clone, base, handoff := evidenceFixture(t, map[string]string{"a.txt": "new\n"})
	// Plant an evidence store but write no evidence.json (emPresent stays false).
	if err := os.MkdirAll(filepath.Join(handoff, export.EvidenceBlobsDirname, "sha256"), 0o750); err != nil {
		t.Fatal(err)
	}
	res, err := Import(t.Context(), handoff, clone, testImportOptions(base))
	if !errors.Is(err, ErrOrphanBlob) {
		t.Fatalf("err = %v, want ErrOrphanBlob (stray evidence dir without a manifest)", err)
	}
	if res.CommitSHA != "" {
		t.Fatalf("stray evidence dir still produced a commit: %+v", res)
	}
}

// TestImportEvidenceCrossChannelSubstitution proves the channels never share a
// blob store: an evidence entry whose content exists only under the repo blobs/
// store (because a repo change carries the same bytes) cannot resolve, and the
// evidence audit fails closed rather than borrowing the repo blob.
func TestImportEvidenceCrossChannelSubstitution(t *testing.T) {
	t.Parallel()
	// The workspace adds a repo file whose content equals the evidence content,
	// so sha256(pngContent) lands in the repo blobs/ store.
	clone, base, handoff := evidenceFixture(t, map[string]string{"a.txt": "new\n", "shared.dat": pngContent})
	e := evidenceEntry("shot", "image/png", pngContent)
	// Write the manifest referencing that digest but NO evidence blob for it.
	writeEvidence(t, handoff, rawEvidenceJSON(t, evidenceManifest(e)))

	res, err := Import(t.Context(), handoff, clone, testImportOptions(base))
	if !errors.Is(err, ErrMissingBlob) {
		t.Fatalf("err = %v, want ErrMissingBlob (evidence must not borrow a repo blob)", err)
	}
	if res.CommitSHA != "" {
		t.Fatalf("cross-channel substitution still produced a commit: %+v", res)
	}
}

// reviewGuideFixture returns a valid agent-written review guide as a JSON
// value, which a tamper case mutates before marshaling.
func reviewGuideFixture() map[string]any {
	return map[string]any{
		"stops": []any{map[string]any{
			"title": "Start at the gate", "summary": "The import rejects a malformed guide.",
			"attention": "risk",
			"excerpts": []any{map[string]any{
				"path": "a.txt", "side": "head", "start_line": 1, "end_line": 1,
				"caption": "The changed line.",
			}},
		}},
		"questions": []any{"Is failing closed right here?"},
		"notes":     nil,
	}
}

// guideStop and guideExcerpt reach into a fixture for mutation.
func guideStop(g map[string]any, i int) map[string]any {
	return g["stops"].([]any)[i].(map[string]any)
}

func guideExcerpt(g map[string]any) map[string]any {
	return guideStop(g, 0)["excerpts"].([]any)[0].(map[string]any)
}

func marshalGuide(t *testing.T, g map[string]any) string {
	t.Helper()
	body, err := json.Marshal(g)
	if err != nil {
		t.Fatalf("marshal review guide: %v", err)
	}
	return string(body)
}

// reviewGuideEntry is the entry the launcher declares for the review guide.
func reviewGuideEntry(content string) export.EvidenceEntry {
	e := evidenceEntry(export.ReviewGuideEvidenceLabel, "application/json", content)
	e.Provenance.SensitivityClass = export.EvidenceSensitivitySensitive
	return e
}

// importReviewGuide imports a one-change handoff whose evidence channel
// carries entry over content.
func importReviewGuide(t *testing.T, entry export.EvidenceEntry, content string, expectNoChanges bool) (Result, error) {
	t.Helper()
	workspace := map[string]string{"a.txt": "new\n"}
	if expectNoChanges {
		workspace["a.txt"] = "old\n"
	}
	clone, base, handoff := evidenceFixture(t, workspace)
	writeEvidence(t, handoff, rawEvidenceJSON(t, evidenceManifest(entry)), blobFor(content))
	opts := testImportOptions(base)
	opts.ExpectNoChanges = expectNoChanges
	return Import(t.Context(), handoff, clone, opts)
}

// TestImportReviewGuideBindsTheBuiltCommit: a valid guide becomes one claim
// without inline text, head-bound to the commit the import built, with the
// artifact id of that complete provenance.
func TestImportReviewGuideBindsTheBuiltCommit(t *testing.T) {
	t.Parallel()
	content := marshalGuide(t, reviewGuideFixture())
	res, err := importReviewGuide(t, reviewGuideEntry(content), content, false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.CommitSHA == "" || len(res.Findings) != 0 || len(res.Claims) != 1 {
		t.Fatalf("result = %+v, want a clean commit with one claim", res)
	}
	claim := res.Claims[0]
	if claim.Label != export.ReviewGuideEvidenceLabel || claim.Text != nil ||
		claim.Metadata.MediaType != domain.EvidenceMediaApplicationJSON {
		t.Fatalf("guide claim = %+v, want application/json without text", claim)
	}
	if claim.Provenance.HeadBinding != domain.HeadBound || claim.Provenance.SourceHeadSHA != res.CommitSHA {
		t.Fatalf("guide claim provenance = %+v, want head_bound at %s", claim.Provenance, res.CommitSHA)
	}
	if want := agentArtifactID(claim.Provenance, export.Digest(claim.Digest)); claim.Artifact != want {
		t.Fatalf("guide claim artifact = %q, want %q", claim.Artifact, want)
	}
	unbound := claim.Provenance
	unbound.HeadBinding, unbound.SourceHeadSHA = domain.HeadIndependent, ""
	if claim.Artifact == agentArtifactID(unbound, export.Digest(claim.Digest)) {
		t.Fatal("guide claim kept its head-independent artifact id")
	}
}

// TestImportReviewGuideWithoutCommitStaysUnbound: a blocked-shape import
// builds no commit, so the guide claim has no head to name.
func TestImportReviewGuideWithoutCommitStaysUnbound(t *testing.T) {
	t.Parallel()
	content := marshalGuide(t, reviewGuideFixture())
	res, err := importReviewGuide(t, reviewGuideEntry(content), content, true)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.CommitSHA != "" || len(res.Claims) != 1 ||
		res.Claims[0].Provenance.HeadBinding != domain.HeadIndependent ||
		res.Claims[0].Provenance.SourceHeadSHA != "" {
		t.Fatalf("result = %+v, want no commit and a head-independent guide claim", res)
	}
}

// TestImportReviewGuideSecretIsAFinding: a credential in the guide's prose,
// raw or spelled with JSON escapes, is a secret finding the production
// profile rejects, and the guide claim never carries inline text.
func TestImportReviewGuideSecretIsAFinding(t *testing.T) {
	t.Parallel()
	token := "ghp_" + strings.Repeat("A", 36)
	for name, place := range map[string]func(map[string]any, string){
		"stop title": func(g map[string]any, s string) { guideStop(g, 0)["title"] = s },
		"caption":    func(g map[string]any, s string) { guideExcerpt(g)["caption"] = s },
		"question":   func(g map[string]any, s string) { g["questions"] = []any{s} },
		"notes":      func(g map[string]any, s string) { g["notes"] = s },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := reviewGuideFixture()
			place(g, "Token: "+token)
			content := marshalGuide(t, g)
			res, err := importReviewGuide(t, reviewGuideEntry(content), content, false)
			if err != nil {
				t.Fatalf("Import: %v", err)
			}
			if len(res.Findings) != 1 || res.Findings[0].Kind != FindingSecret ||
				res.Findings[0].Path != export.ReviewGuideEvidenceLabel {
				t.Fatalf("findings = %+v, want one secret finding on the guide", res.Findings)
			}
			if len(res.Claims) != 1 || res.Claims[0].Text != nil {
				t.Fatalf("claims = %+v, want the guide claim without text", res.Claims)
			}
		})
	}
	t.Run("escaped", func(t *testing.T) {
		t.Parallel()
		// A JSON escape for "A" early in the token, so the raw bytes hold too
		// short a run for any token rule while the decoded summary holds a
		// whole token.
		jsonEscapedA := `\` + "u0041"
		g := reviewGuideFixture()
		guideStop(g, 0)["summary"] = "MARKER"
		content := strings.Replace(marshalGuide(t, g), "MARKER",
			"ghp_"+strings.Repeat("A", 8)+jsonEscapedA+strings.Repeat("A", 30), 1)
		res, err := importReviewGuide(t, reviewGuideEntry(content), content, false)
		if err != nil {
			t.Fatalf("Import: %v", err)
		}
		if len(res.Findings) != 1 || res.Findings[0].Kind != FindingSecret || res.Findings[0].Detail == "" {
			t.Fatalf("findings = %+v, want one decoded-string secret finding", res.Findings)
		}
	})
}

// TestImportReviewGuideTamperFailsClosed: every malformed guide or forged
// declaration fails the import with a typed error naming the entry, before
// any commit is built.
func TestImportReviewGuideTamperFailsClosed(t *testing.T) {
	t.Parallel()
	excerpts := func(n int) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = map[string]any{"path": "a.txt", "side": "head", "start_line": 1, "end_line": 1, "caption": "c"}
		}
		return out
	}
	stops := func(n int) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = map[string]any{"title": "t", "summary": "s", "attention": "risk", "excerpts": []any{}}
		}
		return out
	}
	guideCases := map[string]func(map[string]any){
		"dot-dot path":          func(g map[string]any) { guideExcerpt(g)["path"] = "../a.txt" },
		"absolute path":         func(g map[string]any) { guideExcerpt(g)["path"] = "/etc/passwd" },
		"non-canonical path":    func(g map[string]any) { guideExcerpt(g)["path"] = "dir//a.txt" },
		"end before start":      func(g map[string]any) { guideExcerpt(g)["start_line"] = 3; guideExcerpt(g)["end_line"] = 2 },
		"zero start line":       func(g map[string]any) { guideExcerpt(g)["start_line"] = 0 },
		"no stops":              func(g map[string]any) { g["stops"] = []any{} },
		"unknown field":         func(g map[string]any) { g["version"] = "v1" },
		"daemon text":           func(g map[string]any) { guideExcerpt(g)["text"] = "forged" },
		"daemon head":           func(g map[string]any) { g["head_sha"] = evidenceSourceHead },
		"daemon claim digest":   func(g map[string]any) { g["claim_digest"] = "sha256:forged" },
		"daemon unresolved":     func(g map[string]any) { guideExcerpt(g)["unresolved_reason"] = "binary" },
		"invalid attention":     func(g map[string]any) { guideStop(g, 0)["attention"] = "vibes" },
		"invalid side":          func(g map[string]any) { guideExcerpt(g)["side"] = "left" },
		"missing questions":     func(g map[string]any) { delete(g, "questions") },
		"missing notes":         func(g map[string]any) { delete(g, "notes") },
		"missing excerpts":      func(g map[string]any) { delete(guideStop(g, 0), "excerpts") },
		"empty caption":         func(g map[string]any) { guideExcerpt(g)["caption"] = "" },
		"fractional start line": func(g map[string]any) { guideExcerpt(g)["start_line"] = 1.5 },
		"path over 1024 chars":  func(g map[string]any) { guideExcerpt(g)["path"] = strings.Repeat("a", 1025) },
		// A size cap trims, but never past a shape fault: each of these is
		// over a cap and malformed, the fault inside what a trim would cut.
		"over-long malformed range": func(g map[string]any) {
			guideExcerpt(g)["start_line"] = 0
			guideExcerpt(g)["end_line"] = 200
		},
		"over-long range, invalid side": func(g map[string]any) {
			guideExcerpt(g)["end_line"] = 200
			guideExcerpt(g)["side"] = "left"
		},
		"dropped stop with invalid attention": func(g map[string]any) {
			all := stops(13)
			all[12].(map[string]any)["attention"] = "vibes"
			g["stops"] = all
		},
		"dropped excerpt with dot-dot path": func(g map[string]any) {
			all := excerpts(5)
			all[4].(map[string]any)["path"] = "../a.txt"
			guideStop(g, 0)["excerpts"] = all
		},
		"dropped question empty": func(g map[string]any) {
			g["questions"] = []any{"1", "2", "3", "4", "5", "6", "7", "8", ""}
		},
		"dropped stop with unknown field": func(g map[string]any) {
			all := stops(13)
			all[12].(map[string]any)["version"] = "v1"
			g["stops"] = all
		},
	}
	type tamper struct {
		content string
		entry   func(content string) export.EvidenceEntry
		want    error
	}
	cases := map[string]tamper{}
	for name, mutate := range guideCases {
		g := reviewGuideFixture()
		mutate(g)
		cases[name] = tamper{content: marshalGuide(t, g), entry: reviewGuideEntry, want: ErrEvidenceInvalid}
	}
	valid := marshalGuide(t, reviewGuideFixture())
	cases["non-UTF-8 string"] = tamper{
		content: strings.Replace(valid, "The changed line.", "The \xff line.", 1),
		entry:   reviewGuideEntry, want: ErrEvidenceMediaMismatch,
	}
	// An over-64-KiB guide is dropped rather than refused, but only after
	// the declaration check and the media-type check on its bytes.
	cases["over 64 KiB, not JSON"] = tamper{
		content: valid[:len(valid)-1] + strings.Repeat(" ", MaxReviewGuideBytes) + "}{",
		entry:   reviewGuideEntry, want: ErrEvidenceMediaMismatch,
	}
	for name, forge := range map[string]func(*export.EvidenceEntry){
		"head-bound declaration": func(e *export.EvidenceEntry) {
			e.Provenance.HeadBinding = export.EvidenceHeadBound
			e.Provenance.SourceHeadSHA = evidenceSourceHead
		},
		"high-sensitivity declaration": func(e *export.EvidenceEntry) {
			e.Provenance.SensitivityClass = export.EvidenceSensitivityHigh
		},
		"markdown declaration": func(e *export.EvidenceEntry) { e.MediaType = "text/markdown" },
	} {
		for size, content := range map[string]string{
			"":             valid,
			" over 64 KiB": valid[:len(valid)-1] + strings.Repeat(" ", MaxReviewGuideBytes) + "}",
		} {
			name := name + size
			cases[name] = tamper{
				content: content,
				entry: func(content string) export.EvidenceEntry {
					e := reviewGuideEntry(content)
					forge(&e)
					return e
				},
				want: ErrEvidenceInvalid,
			}
		}
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			res, err := importReviewGuide(t, tc.entry(tc.content), tc.content, false)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Import = %v, want %v", err, tc.want)
			}
			if !strings.Contains(err.Error(), export.ReviewGuideEvidenceLabel) {
				t.Fatalf("error %q does not name the evidence entry", err)
			}
			if res.CommitSHA != "" || res.Claims != nil {
				t.Fatalf("tampered import returned a result: %+v", res)
			}
		})
	}
}

// TestImportReviewGuideSharingADigestFailsClosed: a guide whose bytes equal
// another entry's would leave the ready item's digest naming two claims, so
// the import refuses it before anything publishes.
func TestImportReviewGuideSharingADigestFailsClosed(t *testing.T) {
	t.Parallel()
	content := marshalGuide(t, reviewGuideFixture())
	clone, base, handoff := evidenceFixture(t, map[string]string{"a.txt": "new\n"})
	summary := evidenceEntry(export.SummaryEvidenceLabel, "text/markdown", content)
	summary.Provenance.SensitivityClass = export.EvidenceSensitivitySensitive
	writeEvidence(t, handoff, rawEvidenceJSON(t, evidenceManifest(reviewGuideEntry(content), summary)), blobFor(content))
	res, err := Import(t.Context(), handoff, clone, testImportOptions(base))
	if !errors.Is(err, ErrEvidenceInvalid) || !strings.Contains(err.Error(), export.SummaryEvidenceLabel) ||
		len(res.Claims) != 0 {
		t.Fatalf("Import = %+v, %v; want ErrEvidenceInvalid naming the summary", res, err)
	}
}
