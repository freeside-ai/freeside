package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

func TestNewLoggerAcceptsTheDocumentedLevels(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error", "DEBUG", "Info"} {
		if _, err := newLogger(&bytes.Buffer{}, level); err != nil {
			t.Errorf("newLogger(%q): %v", level, err)
		}
	}
	if _, err := newLogger(&bytes.Buffer{}, "verbose"); err == nil {
		t.Error("newLogger accepted an unknown level; an unparsed flag must not fall back silently")
	}
	if _, err := newLogger(&bytes.Buffer{}, defaultLogLevel); err != nil {
		t.Fatalf("the documented default %q does not parse: %v", defaultLogLevel, err)
	}
}

// TestReconcilerLogsAPassFailureAtError is the operability case #543 names:
// a recurring GitHub failure in the quarter-hour loop has to arrive with a
// severity an operator can filter on, not as a bare stderr line.
func TestReconcilerLogsAPassFailureAtError(t *testing.T) {
	var out bytes.Buffer
	logger, err := newLogger(&out, defaultLogLevel)
	if err != nil {
		t.Fatalf("newLogger: %v", err)
	}
	// An unconfigured reconciler fails its first pass, which is the loop
	// boundary under test; what the pass failed at is Reconcile's business.
	if err := (activeResourceReconciler{}).Run(t.Context(), time.Minute, logger); err == nil {
		t.Fatal("Run returned nil for an unconfigured reconciler")
	}
	records := logRecords(t, out.String())
	failures := recordsWhere(records, "level", "ERROR")
	if len(failures) != 1 {
		t.Fatalf("got %d ERROR records, want exactly the failed pass:\n%s", len(failures), out.String())
	}
	if got := failures[0]["subsystem"]; got != "active-resource" {
		t.Errorf("subsystem = %q, want active-resource so an operator can filter the loop", got)
	}
	if failures[0]["error"] == "" {
		t.Errorf("ERROR record carries no error key: %v", failures[0])
	}
}

// TestNoCredentialReachesALogRecord covers the leak that a logging change
// introduces if nothing pins it: publish.Secret redacts every fmt verb and
// text marshalling, and this asserts slog's rendering goes through those
// rather than around them. Both shapes are checked, since slog treats an
// error value and an attribute value by different paths.
func TestNoCredentialReachesALogRecord(t *testing.T) {
	const token = "ghs_liveinstallationtokenvalue"
	secret := publish.Secret(token)

	var out bytes.Buffer
	logger, err := newLogger(&out, defaultLogLevel)
	if err != nil {
		t.Fatalf("newLogger: %v", err)
	}
	wrapped := fmt.Errorf("refresh installation token %s: %w", secret, errors.New("401 unauthorized"))
	logger.Error("active resource observation failed", "error", wrapped)
	logger.Error("credential attribute", "token", secret)
	logger.Error("credential struct", "auth", struct {
		Token publish.Secret
	}{Token: secret})

	rendered := out.String()
	if strings.Contains(rendered, token) {
		t.Fatalf("a credential value reached the log output:\n%s", rendered)
	}
	if strings.Count(rendered, "[REDACTED]") != 3 {
		t.Fatalf("want a redaction marker in each of the three records, got:\n%s", rendered)
	}
}

// logRecords parses the text handler's key=value output into one map per
// record, which is enough to assert on severity and stable keys without
// pinning the exact rendering.
func logRecords(t *testing.T, output string) []map[string]string {
	t.Helper()
	var records []map[string]string
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		record := map[string]string{}
		for _, field := range splitLogFields(line) {
			key, value, ok := strings.Cut(field, "=")
			if !ok {
				continue
			}
			record[key] = strings.Trim(value, `"`)
		}
		records = append(records, record)
	}
	return records
}

// splitLogFields splits on spaces outside quoted values, since the text
// handler quotes any value containing a space.
func splitLogFields(line string) []string {
	var fields []string
	var current strings.Builder
	quoted := false
	for _, r := range line {
		switch {
		case r == '"':
			quoted = !quoted
			current.WriteRune(r)
		case r == ' ' && !quoted:
			fields = append(fields, current.String())
			current.Reset()
		default:
			current.WriteRune(r)
		}
	}
	return append(fields, current.String())
}

func recordsWhere(records []map[string]string, key, value string) []map[string]string {
	var matched []map[string]string
	for _, record := range records {
		if record[key] == value {
			matched = append(matched, record)
		}
	}
	return matched
}
