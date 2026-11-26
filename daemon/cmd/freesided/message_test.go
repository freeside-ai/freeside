package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestMessageUsesRunningDaemon sends a message through the control socket
// to a daemon with no Claude driver: the daemon answers that writer messages
// aren't available, records nothing, and nothing opens the store directly.
func TestMessageUsesRunningDaemon(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	seedTemplateStore(t, dbPath)
	h, err := run(t.Context(), nil, config{
		Environment: environmentEphemeral,
		DBPath:      dbPath, ListenAddr: "127.0.0.1:0",
		FakeDriverEnabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx := context.WithValue(t.Context(), directStoreOpenerKey{}, directStoreOpener(func(context.Context, string, store.Options, storeOpenMode) (*store.Store, error) {
		t.Error("message opened the store directly")
		return nil, errors.New("unexpected direct store open")
	}))
	_, err = runMessageCommand(ctx, messageCommandConfig{DBPath: dbPath, RunID: "run-1", InvocationID: "writer-1", Text: "steer"})
	if err == nil || !strings.Contains(err.Error(), "writer messages are not available") {
		t.Fatalf("message error = %v, want the daemon's refusal", err)
	}
	if err := h.Store.Read(ctx, func(tx *store.ReadTx) error {
		unsettled, err := tx.UnsettledOperatorMessages(ctx)
		if len(unsettled) != 0 {
			t.Errorf("refused message recorded %+v", unsettled)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestParseMessageReadsTextFromFlagOrStdin(t *testing.T) {
	t.Parallel()
	base := []string{"-db", "state.db", "-run", "run-1", "-invocation", "writer-1"}
	for name, args := range map[string][]string{
		"no db":         {"-run", "run-1", "-invocation", "writer-1", "-text", "hi"},
		"no run":        {"-db", "state.db", "-invocation", "writer-1", "-text", "hi"},
		"no invocation": {"-db", "state.db", "-run", "run-1", "-text", "hi"},
		"blank text":    append(append([]string{}, base...), "-text", " \n"),
		"positional":    append(append([]string{}, base...), "-text", "hi", "extra"),
	} {
		if _, err := parseMessageCommand(args, strings.NewReader(""), &bytes.Buffer{}); err == nil {
			t.Errorf("%s: parse accepted %v", name, args)
		}
	}
	want := messageCommandConfig{DBPath: "state.db", RunID: "run-1", InvocationID: "writer-1", Text: "from the flag"}
	cfg, err := parseMessageCommand(append(append([]string{}, base...), "-text", "from the flag"), strings.NewReader("ignored"), &bytes.Buffer{})
	if err != nil || cfg != want {
		t.Fatalf("parse = %+v, %v; want %+v", cfg, err, want)
	}
	want.Text = "from stdin\n"
	if cfg, err = parseMessageCommand(base, strings.NewReader("from stdin\n"), &bytes.Buffer{}); err != nil || cfg != want {
		t.Fatalf("parse = %+v, %v; want %+v", cfg, err, want)
	}
	oversized := strings.NewReader(strings.Repeat("a", domain.MaxOperatorMessageBytes+1))
	if _, err := parseMessageCommand(base, oversized, &bytes.Buffer{}); !errors.Is(err, domain.ErrInvalidOperatorMessageText) {
		t.Fatalf("oversized stdin error = %v, want %v", err, domain.ErrInvalidOperatorMessageText)
	}
}
