package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/macverify"
)

func TestGoCacheCommandNamesItsVerb(t *testing.T) {
	t.Parallel()
	populate := func(context.Context, macverify.PopulateOptions) (macverify.PopulateResult, error) {
		return macverify.PopulateResult{Key: "abc", Files: []string{"daemon/go.mod", "daemon/go.sum"}}, nil
	}
	var stdout, stderr bytes.Buffer
	err := runCacheCommand(t.Context(), "go-cache", nil, &stdout, &stderr, populate)
	if err == nil || !strings.Contains(err.Error(), "freesided go-cache populate") {
		t.Fatalf("usage error = %v, want the go-cache usage", err)
	}
	err = runCacheCommand(t.Context(), "go-cache",
		[]string{"populate", "--executor-root", "/Library/Freeside/macverify", "--source", "/src"},
		&stdout, &stderr, populate)
	if err != nil {
		t.Fatalf("command: %v", err)
	}
	if want := "already cached abc for daemon/go.mod, daemon/go.sum\n"; stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}
