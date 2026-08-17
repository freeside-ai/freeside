package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReattemptArgumentsFailBeforeOpeningStore(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	for _, tc := range []struct {
		name string
		cfg  reattemptCommandConfig
		want string
	}{
		{"no selector", reattemptCommandConfig{DBPath: dbPath, Reason: "repair"}, "exactly one"},
		{"both selectors", reattemptCommandConfig{
			DBPath: dbPath, ParentRunID: "run-1", CampaignID: "campaign-1", Reason: "repair",
		}, "exactly one"},
		{"untrimmed reason", reattemptCommandConfig{
			DBPath: dbPath, ParentRunID: "run-1", Reason: " repair ",
		}, "non-empty and trimmed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := runReattemptCommand(t.Context(), tc.cfg); err == nil ||
				!strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runReattemptCommand() = %v, want %q", err, tc.want)
			}
			if _, err := os.Stat(dbPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid command created database: %v", err)
			}
		})
	}
}

func TestParseReattemptRejectsPositionalArguments(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	_, err := parseReattemptCommand([]string{
		"-db", "state.db", "-parent-run", "run-1", "-reason", "repair", "ignored",
	}, &stderr)
	if err == nil || !strings.Contains(err.Error(), "unexpected positional arguments") {
		t.Fatalf("parseReattemptCommand() = %v, want positional-argument refusal", err)
	}
}
