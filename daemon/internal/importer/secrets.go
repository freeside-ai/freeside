package importer

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"iter"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/credentialpattern"
	"github.com/freeside-ai/freeside/daemon/internal/export"
)

// secretRules is the shared credential rule list (internal/credentialpattern),
// held once so the per-line loop in scanText doesn't copy it.
var secretRules = credentialpattern.All()

// ContainsSecret reports whether content matches one of the shared
// high-signal credential rules. It deliberately exposes only a boolean so
// callers can reject agent-written structured text without copying a matched
// credential into an error, finding, or durable attention record.
func ContainsSecret(content []byte) bool {
	return len(scanText("", content)) > 0
}

// scanSecrets runs the best-effort secret scan over added-or-modified
// regular content and returns publish-blocking findings. Only content
// the candidate introduced is scanned (an unchanged base file is not
// the candidate's doing), and every blob actually present is scanned
// whatever its size: intake already refused a stored blob over
// Policy.MaxBlobBytes, so that cap bounds one scan's memory and the
// total cap bounds the work. A size cap here would leave an in-scope
// generated file, such as the Swift API client (#3396), unscanned and
// its candidate unpublishable. The high-signal ASCII rules run over the
// raw bytes:
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
func scanSecrets(blobs map[export.Digest]blobInfo, changes []plannedChange) ([]Finding, error) {
	var findings []Finding
	// A manifest can reference one digest from many paths. Verification
	// deduplicates those bytes, and scanning must do the same so entry
	// count cannot multiply regex and hashing work for one stored blob.
	scans := make(map[export.Digest][]Finding)
	for _, c := range changes {
		if c.kind == ChangeDeleted || c.oid == "" || c.fromBase {
			continue // deletions, content-free (omitted-blob), and base-object changes have nothing new to scan
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
	// One exact-size buffer: io.ReadAll's doubling would hold up to twice
	// a blob at the cap.
	data := make([]byte, info.size)
	n, err := io.ReadFull(f, data)
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("verified snapshot %s holds %d bytes, not %d: %w", digest, n, info.size, ErrSizeMismatch)
	}
	if err != nil {
		return nil, fmt.Errorf("read blob %s for scanning: %w: %w", digest, ErrHandoffUnreadable, err)
	}
	if extra, _ := f.Read(make([]byte, 1)); extra != 0 {
		return nil, fmt.Errorf("verified snapshot %s holds more than %d bytes: %w", digest, info.size, ErrSizeMismatch)
	}
	sum := sha256.Sum256(data)
	if got := contentaddr.Format(sum[:]); got != string(digest) {
		return nil, fmt.Errorf("blob for scanning hashes to %s, expected %s: %w", got, digest, ErrDigestMismatch)
	}
	return data, nil
}

// scanText applies every rule line by line, so a finding can name the
// line without the bytes. It splits the in-memory buffer on newlines
// directly rather than through bufio.Scanner: the content is already
// bounded by the blob cap, and a bufio.Scanner silently stops at
// ErrTooLong on a line at or above its buffer cap, which would let a
// token padded onto one very long line (a minified or env line) evade
// the scan while the import still commits. Manual splitting has no line
// cap and cannot fail closed-open.
func scanText(path string, content []byte) []Finding {
	var findings []Finding
	for line, b := range splitLines(content) {
		for _, rule := range secretRules {
			if rule.Pattern.Match(b) {
				findings = append(findings, Finding{Path: path, Kind: FindingSecret, Rule: rule.ID, Line: line + 1})
			}
		}
	}
	return findings
}

// splitLines yields each newline-separated line of b with its 0-based
// index (the trailing "\r" of a CRLF line trimmed for matching), without
// copying and without any per-line length limit. It yields lazily: a
// slice of every line would cost a slice header per newline, gigabytes
// for a blob at the cap that is all newlines.
func splitLines(b []byte) iter.Seq2[int, []byte] {
	return func(yield func(int, []byte) bool) {
		for line := 0; len(b) > 0; line++ {
			i := bytes.IndexByte(b, '\n')
			if i < 0 {
				yield(line, bytes.TrimSuffix(b, []byte("\r")))
				return
			}
			if !yield(line, bytes.TrimSuffix(b[:i], []byte("\r"))) {
				return
			}
			b = b[i+1:]
		}
	}
}
