package importer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"regexp"

	"github.com/freeside-ai/freeside/daemon/internal/export"
)

// secretRule is one high-signal credential pattern. The set is
// deliberately narrow: §5.4 promises best-effort scanning of supported
// textual formats, not universal detection, and a low false-positive
// set keeps the publish-blocking signal meaningful. re matches the
// token's own structure, so a match is the credential, not a mention.
type secretRule struct {
	id string
	re *regexp.Regexp
}

// secretRules are compiled once. Each id names the credential class; the
// pattern anchors on the vendor's own token structure.
var secretRules = []secretRule{
	{"github_token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}\b`)},
	{"github_pat", regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,}\b`)},
	{"aws_access_key_id", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"slack_token", regexp.MustCompile(`\bxox[abprs]-[0-9A-Za-z-]{10,}\b`)},
	{"pem_private_key", regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY-----`)},
	{"gcp_service_account_key", regexp.MustCompile(`"private_key_id"\s*:\s*"[0-9a-f]{40}"`)},
}

// scanSecrets runs the best-effort secret scan over added-or-modified
// regular content and returns publish-blocking findings. Only content
// the candidate introduced is scanned (an unchanged base file is not
// the candidate's doing); only blobs actually present and within the
// scan cap. The high-signal ASCII rules run over the bounded raw bytes:
// invalid UTF-8 cannot turn an otherwise textual credential file into
// an unreported scan skip, while arbitrary binary content remains
// eligible only for those same narrow token structures.
//
// A finding carries the path, the rule id, and the 1-based line, never
// the matched bytes: surfacing the secret in the Result, a log, or the
// PR would defeat the containment this boundary exists for.
//
// Content is read from the daemon-private snapshot written by the
// verification stream and re-hashed against the manifest digest: the
// scan sees exactly the bytes construction will commit, with no later
// handoff path resolution. A fromBase mode-only change carries no new
// content, so it is not re-scanned.
func scanSecrets(blobs map[export.Digest]blobInfo, changes []plannedChange, maxScanBytes int64) ([]Finding, error) {
	var findings []Finding
	// A manifest can reference one digest from many paths. Verification
	// deduplicates those bytes, and scanning must do the same so entry
	// count cannot multiply regex and hashing work for one stored blob.
	scans := make(map[export.Digest][]Finding)
	for _, c := range changes {
		if c.kind == ChangeDeleted || c.oid == "" || c.fromBase {
			continue // deletions, content-free (omitted-blob), and base-object changes have nothing new to scan
		}
		if maxScanBytes > 0 && c.size > maxScanBytes {
			// Too large to scan: surface the gap as a finding rather than
			// skip silently, so a findings-free import never hides an
			// unscanned file. Non-blocking, like every secret finding.
			findings = append(findings, Finding{
				Path: c.path, Kind: FindingSecretScanSkipped,
				Detail: fmt.Sprintf("%d bytes exceed the %d-byte scan cap; not scanned", c.size, maxScanBytes),
			})
			continue
		}
		matches, scanned := scans[c.digest]
		if !scanned {
			info, ok := blobs[c.digest]
			if !ok {
				return nil, fmt.Errorf("blob %s has no verified snapshot for scanning: %w", c.digest, ErrTreeMismatch)
			}
			content, err := readScanBlob(info, c.digest)
			if err != nil {
				return nil, err
			}
			matches = scanText("", content)
			scans[c.digest] = matches
		}
		for _, match := range matches {
			match.Path = c.path
			findings = append(findings, match)
		}
	}
	return findings, nil
}

// readScanBlob reads a blob's bytes by digest and confirms they hash to
// that digest, so the scan is bound to the verified content rather than
// to whatever the file happens to hold when the scan runs. size is the
// verified length; the read is bounded to it, and a mismatch fails
// closed.
func readScanBlob(info blobInfo, digest export.Digest) ([]byte, error) {
	f, err := openRegular(info.verifiedPath, ErrHandoffUnreadable)
	if err != nil {
		return nil, fmt.Errorf("open blob %s for scanning: %w: %w", digest, ErrHandoffUnreadable, err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, info.size+1))
	if err != nil {
		return nil, fmt.Errorf("read blob %s for scanning: %w: %w", digest, ErrHandoffUnreadable, err)
	}
	if int64(len(data)) != info.size {
		return nil, fmt.Errorf("verified snapshot %s does not hold exactly %d bytes: %w", digest, info.size, ErrSizeMismatch)
	}
	sum := sha256.Sum256(data)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != string(digest) {
		return nil, fmt.Errorf("blob for scanning hashes to %s, expected %s: %w", got, digest, ErrDigestMismatch)
	}
	return data, nil
}

// scanText applies every rule line by line, so a finding can name the
// line without the bytes. It splits the in-memory buffer on newlines
// directly rather than through bufio.Scanner: the content is already
// bounded by SecretMaxScanBytes, and a bufio.Scanner silently stops at
// ErrTooLong on a line at or above its buffer cap, which would let a
// token padded onto one very long line (a minified or env line) evade
// the scan while the import still commits. Manual splitting has no line
// cap and cannot fail closed-open.
func scanText(path string, content []byte) []Finding {
	var findings []Finding
	for line, b := range splitLines(content) {
		for _, rule := range secretRules {
			if rule.re.Match(b) {
				findings = append(findings, Finding{Path: path, Kind: FindingSecret, Rule: rule.id, Line: line + 1})
			}
		}
	}
	return findings
}

// splitLines yields the newline-separated lines of b (the trailing "\r"
// of a CRLF line trimmed for matching), without copying and without any
// per-line length limit.
func splitLines(b []byte) [][]byte {
	var lines [][]byte
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			lines = append(lines, bytes.TrimSuffix(b, []byte("\r")))
			break
		}
		lines = append(lines, bytes.TrimSuffix(b[:i], []byte("\r")))
		b = b[i+1:]
	}
	return lines
}
