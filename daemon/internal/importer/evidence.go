package importer

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/export"
)

// loadEvidenceManifest reads and re-validates the evidence channel's manifest
// (the second §5.6 workspace-exit channel), the peer of loadManifest for the
// repo-change channel. Evidence is optional: an honest workspace may emit none,
// so an absent evidence.json returns (_, false, nil) rather than an error. A
// present file is untrusted like every handoff byte: the open is hardened
// (loadManifest's openRegular: no symlink or FIFO follow, regular-file only, so
// a planted inode is a hard error, not "absent"), the read is capped before any
// byte is parsed, and DecodeEvidenceManifest is the canonical-or-reject boundary
// that already strict-decodes (an injected trust field like publish_eligible is
// an unknown field, not an ignored extra), rejects non-canonical bytes, and
// re-runs the producer's full Validate (which pins producer_class == agent).
// Nothing downstream ever sees evidence the producer's rules would reject.
func loadEvidenceManifest(handoffDir string, pol Policy) (export.EvidenceManifest, bool, error) {
	name := filepath.Join(handoffDir, export.EvidenceFilename)
	f, err := openRegular(name, ErrEvidenceUnreadable)
	if err != nil {
		// Only a truly absent file means "no evidence channel". A symlink
		// (O_NOFOLLOW → ELOOP) or FIFO/device (rejected as non-regular) planted
		// at evidence.json is hostile, so it fails closed rather than being read
		// as absence.
		if errors.Is(err, os.ErrNotExist) {
			return export.EvidenceManifest{}, false, nil
		}
		return export.EvidenceManifest{}, false, fmt.Errorf("open evidence manifest: %w: %w", ErrEvidenceUnreadable, err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, pol.MaxManifestBytes+1))
	if err != nil {
		return export.EvidenceManifest{}, false, fmt.Errorf("read evidence manifest: %w: %w", ErrEvidenceUnreadable, err)
	}
	if int64(len(data)) > pol.MaxManifestBytes {
		return export.EvidenceManifest{}, false, fmt.Errorf("evidence manifest exceeds %d bytes: %w", pol.MaxManifestBytes, ErrManifestTooLarge)
	}
	// Enforce the entry-count cap BEFORE the full canonical decode, so an over-cap
	// manifest that is still under the byte cap never pays for the per-entry typed
	// decode, the per-entry validation, and the canonical re-encode that
	// DecodeEvidenceManifest runs. The count is a streaming token pass
	// (manifestEntryCountExceeds) that reads only the first JSON value and counts
	// every "entries" array's elements with an early exit at the cap: it does not
	// trust json.Unmarshal's last-wins on a duplicate key or its whole-input
	// requirement, both of which a hostile manifest can use to hide a large array
	// behind a trailing value or a second "entries" key. loadManifest applies the
	// same helper before its strict decode for the repo channel.
	if manifestEntryCountExceeds(data, pol.MaxEntries) {
		return export.EvidenceManifest{}, false, fmt.Errorf("evidence entries exceed the cap of %d: %w", pol.MaxEntries, ErrManifestTooLarge)
	}
	em, err := export.DecodeEvidenceManifest(data)
	if err != nil {
		return export.EvidenceManifest{}, false, fmt.Errorf("%w: %w", ErrEvidenceInvalid, err)
	}
	return em, true, nil
}

// manifestEntryCountExceeds reports whether the manifest structurally declares
// more than max entries, counted by a streaming token pass that early-exits at
// the cap so a hostile manifest cannot force a byte-cap-sized typed decode before
// the cap rejects it. It reads only the first JSON value (trailing content cannot
// smuggle a second) and counts the elements of every top-level "entries" array,
// duplicate keys included, since the typed decoder would build each in turn. Any
// shape it cannot count structurally (a non-object root, a malformed stream)
// yields false: the channel's typed decoder (DecodeEvidenceManifest for the
// evidence channel, loadManifest's strict decode for the repo channel) is the
// authoritative rejection, and those shapes cannot force a large typed decode (a
// non-object root fails the struct decode immediately, and total work stays
// bounded by the byte cap). Shared by both channels' manifest intake.
func manifestEntryCountExceeds(data []byte, max int) bool {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return false
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return false
	}
	total := 0
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return false
		}
		key, ok := keyTok.(string)
		if !ok {
			return false
		}
		// Match the key the way encoding/json matches the struct field:
		// case-insensitively (EqualFold, which covers every fold Go's field
		// matcher does for an ASCII name, verified in TestManifestEntryCount
		// CaseInsensitive). Counting only the exact lowercase "entries" would
		// let a hostile "Entries"/"ENTRIES" array route into the typed decode
		// uncounted, defeating the cap.
		if !strings.EqualFold(key, "entries") {
			if err := skipJSONValue(dec); err != nil {
				return false
			}
			continue
		}
		open, err := dec.Token()
		if err != nil {
			return false
		}
		d, ok := open.(json.Delim)
		if !ok || d != '[' {
			// "entries" is not an array; DecodeEvidenceManifest will reject it.
			// Consume the value so the key/value walk stays aligned.
			if ok {
				if err := skipUntilJSONClose(dec); err != nil {
					return false
				}
			}
			continue
		}
		for dec.More() {
			if err := skipJSONValue(dec); err != nil {
				return false
			}
			total++
			if total > max {
				return true
			}
		}
		if _, err := dec.Token(); err != nil { // consume the closing ']'
			return false
		}
	}
	return total > max
}

// skipJSONValue reads and discards exactly one complete JSON value (scalar,
// object, or array) from dec.
func skipJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); ok && (d == '{' || d == '[') {
		return skipUntilJSONClose(dec)
	}
	return nil
}

// skipUntilJSONClose consumes tokens until the container whose opening delim was
// just read is closed, tracking nesting so inner containers do not end it early.
func skipUntilJSONClose(dec *json.Decoder) error {
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return nil
}

// buildClaims routes each verified evidence entry into a labeled domain
// AgentClaim (plan §5.15 rule 2): agent-produced artifacts appear only as
// claims, never in an item's evidence snapshot and never auto-uploaded. It runs
// after verifyBlobs has bytes-verified every entry's blob, so it validates the
// declared media type against the blob's magic bytes (§5.15 rule 3: the daemon
// validates magic/type/size and treats images as opaque), maps the typed
// provenance into the domain shape, and derives an invocation-scoped artifact
// id while retaining the content address in the claim's digest. Credential
// matches return metadata-only findings and omit inline text; any integrity
// failure returns a typed error and no claims. Bad evidence fails the whole
// import closed, exactly as the repo channel's integrity violations do.
func buildClaims(
	em export.EvidenceManifest, blobs map[export.Digest]blobInfo, opts Options,
) ([]domain.AgentClaim, []Finding, error) {
	pol := opts.Policy.withDefaults()
	createdAt := opts.evidenceCreatedAt()
	claims := make([]domain.AgentClaim, 0, len(em.Entries))
	var findings []Finding
	for _, e := range em.Entries {
		info, ok := blobs[e.Digest]
		if !ok {
			// verifyBlobs built the verified set from these same entries, so a
			// miss is an internal invariant break, not hostile input.
			return nil, nil, fmt.Errorf("evidence entry %q digest %s has no verified blob: %w", e.Label, e.Digest, ErrMissingBlob)
		}
		if err := validateEvidenceType(e, info.verifiedPath); err != nil {
			return nil, nil, err
		}
		prov, err := mapEvidenceProvenance(e.Provenance)
		if err != nil {
			return nil, nil, err
		}
		// Carry the daemon-validated media type and size the importer already
		// checked (§5.15 rule 3) instead of dropping them: an unrecognized
		// media type fails the import closed, exactly like a magic-byte
		// mismatch. Availability is a placeholder here (the sync projection
		// recomputes it from the blob store per synced item).
		mediaType, err := mapEvidenceMediaType(e.MediaType)
		if err != nil {
			return nil, nil, fmt.Errorf("evidence entry %q: %w", e.Label, err)
		}
		claim := domain.AgentClaim{
			Label:      e.Label,
			Artifact:   agentArtifactID(prov, e.Digest),
			Digest:     domain.Digest(string(e.Digest)),
			Provenance: prov,
			Metadata: domain.EvidenceMetadata{
				MediaType: mediaType, SizeBytes: e.Size, CreatedAt: createdAt,
				Source: domain.EvidenceSourceClaim, Availability: domain.EvidenceAvailable,
			},
		}
		if e.MediaType == "text/markdown" && e.Size <= pol.SecretMaxScanBytes {
			body, readErr := readScanBlob(info, e.Digest)
			if readErr != nil {
				return nil, nil, fmt.Errorf("read evidence entry %q text: %w", e.Label, readErr)
			}
			matches := scanText(e.Label, body)
			findings = append(findings, matches...)
			// Secret-bearing evidence stays out of both inline attention state
			// and durable artifact storage: the finding makes the production
			// profile reject the import before persistEvidence runs. The
			// scanner reports only rule and line metadata, never matched bytes.
			if len(matches) == 0 && e.Size <= domain.MaxClaimTextBytes &&
				prov.SensitivityClass != domain.SensitivityHigh {
				claim.Text = &domain.ClaimText{
					MediaType: domain.MediaTypeTextMarkdown,
					Content:   string(body),
				}
			}
		}
		// Defense in depth: decode already guarantees agent provenance, but
		// re-pinning it here fails a mapping bug closed rather than open.
		if err := claim.Validate(); err != nil {
			return nil, nil, fmt.Errorf("evidence entry %q: %w", e.Label, err)
		}
		claims = append(claims, claim)
	}
	return claims, findings, nil
}

// agentArtifactID is stable for one complete immutable artifact row. Labels
// may share an artifact, but any provenance field that changes the row gets a
// distinct identity even when the producer and content bytes are identical.
func agentArtifactID(prov domain.Provenance, digest export.Digest) domain.ArtifactID {
	recipe := ""
	if prov.VerificationRecipeDigest != nil {
		recipe = string(*prov.VerificationRecipeDigest)
	}
	h := sha256.New()
	for _, field := range []string{
		string(digest),
		string(prov.ProducerClass),
		string(prov.ProducerInvocationID),
		string(prov.HeadBinding),
		prov.SourceHeadSHA,
		recipe,
		string(prov.SensitivityClass),
	} {
		_, _ = fmt.Fprintf(h, "%d:", len(field))
		_, _ = h.Write([]byte(field))
	}
	return domain.ArtifactID(fmt.Sprintf("agent:%x", h.Sum(nil)))
}

// mapEvidenceProvenance converts the export channel's typed provenance into the
// domain shape. The producer, head-binding, and sensitivity tokens map through
// explicit default-less switches (helpers below), never a blind string() cast,
// so an unmapped token fails loud; the recipe digest is always nil, because
// agent output is never produced under a verification recipe (domain
// Provenance.Validate enforces this).
func mapEvidenceProvenance(p export.EvidenceProvenance) (domain.Provenance, error) {
	producer, err := mapProducerClass(p.ProducerClass)
	if err != nil {
		return domain.Provenance{}, err
	}
	binding, err := mapHeadBinding(p.HeadBinding)
	if err != nil {
		return domain.Provenance{}, err
	}
	sensitivity, err := mapSensitivityClass(p.SensitivityClass)
	if err != nil {
		return domain.Provenance{}, err
	}
	return domain.Provenance{
		ProducerClass:            producer,
		ProducerInvocationID:     domain.InvocationID(p.ProducerInvocationID),
		HeadBinding:              binding,
		SourceHeadSHA:            p.SourceHeadSHA,
		VerificationRecipeDigest: nil,
		SensitivityClass:         sensitivity,
	}, nil
}

// The three mappers omit default so a new export enum member forces a mapping
// decision (the exhaustive linter), with a trailing typed error for the invalid
// zero value. DecodeEvidenceManifest has already validated these tokens, so the
// trailing returns are defensive; the export "agent" channel carries only the
// agent producer class by contract.
func mapProducerClass(c export.EvidenceProducerClass) (domain.ProducerClass, error) {
	switch c {
	case export.EvidenceProducerAgent:
		return domain.ProducerAgent, nil
	}
	return "", fmt.Errorf("evidence producer_class %q: %w", c, ErrEvidenceInvalid)
}

func mapHeadBinding(b export.EvidenceHeadBinding) (domain.HeadBinding, error) {
	switch b {
	case export.EvidenceHeadBound:
		return domain.HeadBound, nil
	case export.EvidenceHeadIndependent:
		return domain.HeadIndependent, nil
	}
	return "", fmt.Errorf("evidence head_binding %q: %w", b, ErrEvidenceInvalid)
}

// mapEvidenceMediaType converts a validated evidence entry's declared media
// type into the domain enum. The cases are exactly the importer allow-set (the
// opaque image set plus application/jsonl, application/json, and
// text/markdown) that
// validateEvidenceType enforces; any other string fails closed with an
// ErrEvidenceMediaMismatch, so a mapping gap can never silently admit a claim
// with an unmapped type.
func mapEvidenceMediaType(mediaType string) (domain.EvidenceMediaType, error) {
	switch mediaType {
	case "image/png":
		return domain.EvidenceMediaImagePNG, nil
	case "image/jpeg":
		return domain.EvidenceMediaImageJPEG, nil
	case "image/gif":
		return domain.EvidenceMediaImageGIF, nil
	case "image/webp":
		return domain.EvidenceMediaImageWEBP, nil
	case "application/jsonl":
		return domain.EvidenceMediaApplicationJSONL, nil
	case "application/json":
		return domain.EvidenceMediaApplicationJSON, nil
	case "text/markdown":
		return domain.EvidenceMediaTextMarkdown, nil
	}
	return "", fmt.Errorf("evidence media_type %q is not an allowed type: %w", mediaType, ErrEvidenceMediaMismatch)
}

func mapSensitivityClass(c export.EvidenceSensitivityClass) (domain.SensitivityClass, error) {
	switch c {
	case export.EvidenceSensitivityNormal:
		return domain.SensitivityNormal, nil
	case export.EvidenceSensitivitySensitive:
		return domain.SensitivitySensitive, nil
	case export.EvidenceSensitivityHigh:
		return domain.SensitivityHigh, nil
	}
	return "", fmt.Errorf("evidence sensitivity_class %q: %w", c, ErrEvidenceInvalid)
}

// evidenceMagicPeek is the header length read for magic validation: 12 bytes,
// the longest signature checked (WEBP needs RIFF at 0 and WEBP at 8).
const evidenceMagicPeek = 12

// evidenceMediaMagic is the allow-set of opaque image media types and each
// type's magic-byte matcher. Plan §5.15 rule 3 has the daemon validate
// magic/type/size and treat agent images as opaque blobs. SVG (scriptable XML,
// an exfiltration/stored-script vector when a claim is rendered) and text/plain
// (no reliable magic, so any bytes would "match") are excluded by design.
// text/markdown is handled separately as bounded, non-empty UTF-8 prose.
var evidenceMediaMagic = map[string]func([]byte) bool{
	"image/png":  hasPrefix([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}),
	"image/jpeg": hasPrefix([]byte{0xFF, 0xD8, 0xFF}),
	"image/gif": func(b []byte) bool {
		return bytes.HasPrefix(b, []byte("GIF87a")) || bytes.HasPrefix(b, []byte("GIF89a"))
	},
	"image/webp": func(b []byte) bool {
		return len(b) >= 12 && bytes.Equal(b[0:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP"))
	},
}

func hasPrefix(prefix []byte) func([]byte) bool {
	return func(b []byte) bool { return bytes.HasPrefix(b, prefix) }
}

// validateEvidenceType enforces the evidence media allow-set. Images use their
// magic signatures under §5.15 rule 3; a JSON Lines transcript must consist
// entirely of non-empty, individually valid JSON records. media_type is
// agent-declared and untrusted, so an unlisted type or content that does not
// match its declared type fails the import closed (ErrEvidenceMediaMismatch).
// Validation reads the daemon-private verified snapshot, never the handoff
// path, so no handoff inode is re-resolved after the blob audit.
func validateEvidenceType(entry export.EvidenceEntry, verifiedPath string) error {
	if entry.MediaType == "application/jsonl" {
		if err := validateJSONLines(verifiedPath, entry.Size); err != nil {
			if !errors.Is(err, ErrEvidenceMediaMismatch) {
				return fmt.Errorf("validate evidence entry %q as %q: %w",
					entry.Label, entry.MediaType, err)
			}
			return fmt.Errorf("evidence entry %q content does not match declared media_type %q: %w",
				entry.Label, entry.MediaType, ErrEvidenceMediaMismatch)
		}
		return nil
	}
	if entry.MediaType == "application/json" {
		if err := validateJSONDocument(verifiedPath, entry.Size); err != nil {
			if !errors.Is(err, ErrEvidenceMediaMismatch) {
				return fmt.Errorf("validate evidence entry %q as %q: %w",
					entry.Label, entry.MediaType, err)
			}
			return fmt.Errorf("evidence entry %q content does not match declared media_type %q: %w",
				entry.Label, entry.MediaType, ErrEvidenceMediaMismatch)
		}
		return nil
	}
	if entry.MediaType == "text/markdown" {
		if err := validateUTF8Markdown(verifiedPath, entry.Size); err != nil {
			if !errors.Is(err, ErrEvidenceMediaMismatch) {
				return fmt.Errorf("validate evidence entry %q as %q: %w",
					entry.Label, entry.MediaType, err)
			}
			return fmt.Errorf("evidence entry %q content does not match declared media_type %q: %w",
				entry.Label, entry.MediaType, ErrEvidenceMediaMismatch)
		}
		return nil
	}
	matcher, ok := evidenceMediaMagic[entry.MediaType]
	if !ok {
		return fmt.Errorf("evidence entry %q media_type %q is not an allowed type: %w", entry.Label, entry.MediaType, ErrEvidenceMediaMismatch)
	}
	header, err := readEvidenceHeader(verifiedPath)
	if err != nil {
		return err
	}
	if !matcher(header) {
		return fmt.Errorf("evidence entry %q content does not match declared media_type %q: %w", entry.Label, entry.MediaType, ErrEvidenceMediaMismatch)
	}
	return nil
}

// validateJSONDocument checks exactly one valid UTF-8 JSON value. The blob
// has already passed the evidence per-blob cap, which bounds the read.
func validateJSONDocument(verifiedPath string, size int64) error {
	if size <= 0 {
		return ErrEvidenceMediaMismatch
	}
	body, err := os.ReadFile(verifiedPath) //nolint:gosec // G304: daemon-private verified-snapshot path
	if err != nil {
		return err
	}
	if int64(len(body)) != size || !utf8.Valid(body) || !json.Valid(body) {
		return ErrEvidenceMediaMismatch
	}
	return nil
}

// validateJSONLines checks one complete record per line. Scanner memory is
// bounded by the already-verified manifest size for this blob; that size has
// already passed the evidence per-blob cap before this function runs.
func validateJSONLines(verifiedPath string, size int64) error {
	f, err := os.Open(verifiedPath) //nolint:gosec // G304: daemon-private verified-snapshot path
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	maxInt := int(^uint(0) >> 1)
	if size <= 0 || size > int64(maxInt-1) {
		return ErrEvidenceMediaMismatch
	}
	maxToken := int(size) + 1
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, min(maxToken, 64<<10)), maxToken)
	records := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 || !utf8.Valid(line) || !json.Valid(line) {
			return ErrEvidenceMediaMismatch
		}
		records++
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if records == 0 {
		return ErrEvidenceMediaMismatch
	}
	return nil
}

// validateUTF8Markdown checks the complete verified blob without allocating a
// second buffer proportional to its size. Oversized summaries deliberately
// stay artifact-only, so validating one must not first read the whole evidence
// cap into memory merely to decide that its bytes are well-formed.
func validateUTF8Markdown(verifiedPath string, size int64) error {
	if size <= 0 {
		return ErrEvidenceMediaMismatch
	}
	f, err := os.Open(verifiedPath) //nolint:gosec // G304: daemon-private verified-snapshot path
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	reader := bufio.NewReader(f)
	for {
		r, width, readErr := reader.ReadRune()
		switch {
		case errors.Is(readErr, io.EOF):
			return nil
		case readErr != nil:
			return readErr
		case r == utf8.RuneError && width == 1:
			return ErrEvidenceMediaMismatch
		}
	}
}

// readEvidenceHeader returns up to evidenceMagicPeek leading bytes of the
// verified snapshot. A file shorter than a type's signature returns a short
// header, which no matcher accepts, so an undersized blob fails type validation
// rather than passing on absent bytes.
func readEvidenceHeader(verifiedPath string) ([]byte, error) {
	f, err := os.Open(verifiedPath) //nolint:gosec // G304: daemon-private verified-snapshot path, written by this import from a validated digest
	if err != nil {
		return nil, fmt.Errorf("open verified evidence snapshot: %w: %w", ErrEvidenceUnreadable, err)
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, evidenceMagicPeek)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, fmt.Errorf("read verified evidence snapshot: %w: %w", ErrEvidenceUnreadable, err)
	}
	return buf[:n], nil
}
