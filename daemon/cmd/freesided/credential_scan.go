package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

// The §5.4 export scan (ward's check-7 hook): the last gate between an
// agent's verified output and the gauntlet. It answers one question — does
// this output carry credential material — and fails the handoff closed when
// it does, so no output reaches the importer unscanned.
//
// Scope, stated plainly: this is a high-signal pattern scan over the
// extracted export, not a proof of absence. It catches a credential copied
// or echoed into the workspace, which is the realistic Phase 1A.2 leak
// (the CLI's own auth store lives on a separate leased volume that never
// enters the export). It cannot catch material the agent encoded or split.

// credentialPatterns are shapes that are credential material wherever they
// appear. Each is anchored on a vendor-issued prefix or an armored header
// rather than a generic word, so ordinary prose and code do not trip it.
var credentialPatterns = []*regexp.Regexp{
	// Anthropic API keys and OAuth/setup tokens.
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{16,}`),
	// GitHub tokens: personal, OAuth, user-to-server, server-to-server, refresh.
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{16,}`),
	// GitHub fine-grained personal access tokens.
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,}\b`),
	// Slack app, bot, user, refresh, and service tokens.
	regexp.MustCompile(`\bxox[abprs]-[0-9A-Za-z-]{10,}\b`),
	// PEM private keys of any flavour.
	regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY-----`),
	// AWS access key ids.
	regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),
	// GCP service-account private key identifiers.
	regexp.MustCompile(`"private_key_id"\s*:\s*"[0-9a-f]{40}"`),
}

// scanChunkBytes is how much of a file is held in memory at once. Files are
// streamed in overlapping chunks rather than truncated: the export's largest
// member is the agent's own transcript, which is both the most likely to
// exceed any fixed cap and the most likely to echo auth material, so a cap
// that returned "clean" for the rest of a large file would be a silent false
// negative in exactly the worst place.
const scanChunkBytes = 1 << 20

// scanOverlapBytes is carried between chunks so a pattern straddling a chunk
// boundary is still matched. It exceeds the longest pattern this scanner can
// match by a wide margin.
const scanOverlapBytes = 4 << 10

// credentialScanner implements ward.OutputScanner.
type credentialScanner struct{}

func (credentialScanner) Scan(ctx context.Context, dir string) error {
	return filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		// Only regular files are read. The export is already digest-verified
		// and extracted by the gate, but a scanner that followed a symlink
		// would read outside the directory it was asked to inspect.
		if !entry.Type().IsRegular() {
			return nil
		}
		matched, err := scanFile(ctx, path)
		if err != nil {
			return err
		}
		if matched != nil {
			// The match itself is never reported: naming the pattern locates
			// the leak without copying the secret into a log, an attention
			// item, or an error a client can read.
			rel, relErr := filepath.Rel(dir, path)
			if relErr != nil {
				rel = filepath.Base(path)
			}
			return fmt.Errorf("credential material matching %s found in exported %s",
				patternName(matched), rel)
		}
		return nil
	})
}

// scanFile streams one file through the pattern set, returning the first
// pattern that matched or nil. The whole file is examined regardless of its
// size; only the working set is bounded.
func scanFile(ctx context.Context, path string) (*regexp.Regexp, error) {
	f, err := os.Open(path) //nolint:gosec // G304: gate-extracted export path handed to this scanner
	if err != nil {
		return nil, fmt.Errorf("scan export file: %w", err)
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, 0, scanChunkBytes+scanOverlapBytes)
	chunk := make([]byte, scanChunkBytes)
	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		n, readErr := f.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			for _, pattern := range credentialPatterns {
				if pattern.Match(buf) {
					return pattern, nil
				}
			}
			// Carry the tail forward so a pattern split across this boundary
			// is matched on the next pass.
			if len(buf) > scanOverlapBytes {
				buf = append(buf[:0], buf[len(buf)-scanOverlapBytes:]...)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil, nil
			}
			return nil, fmt.Errorf("scan export file: %w", readErr)
		}
	}
}

// patternName describes a matched pattern without echoing the match.
func patternName(pattern *regexp.Regexp) string {
	switch {
	case bytes.Contains([]byte(pattern.String()), []byte("sk-ant")):
		return "an Anthropic key or token"
	case bytes.Contains([]byte(pattern.String()), []byte("gh[pousr]")) ||
		bytes.Contains([]byte(pattern.String()), []byte("github_pat")):
		return "a GitHub token"
	case bytes.Contains([]byte(pattern.String()), []byte("xox")):
		return "a Slack token"
	case bytes.Contains([]byte(pattern.String()), []byte("PRIVATE KEY")):
		return "a PEM private key"
	case bytes.Contains([]byte(pattern.String()), []byte("private_key_id")):
		return "a GCP service-account key id"
	default:
		return "an AWS access key id"
	}
}
