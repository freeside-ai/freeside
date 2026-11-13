package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/macverify"
)

func TestSwiftCacheCommandPassesFlagsToPopulate(t *testing.T) {
	t.Parallel()
	var got macverify.PopulateOptions
	populate := func(_ context.Context, opts macverify.PopulateOptions) (macverify.PopulateResult, error) {
		got = opts
		return macverify.PopulateResult{Key: "abc", Files: []string{"app/Package.resolved"}, Populated: true}, nil
	}
	var stdout, stderr bytes.Buffer
	err := runCacheCommand(t.Context(), "swift-cache",
		[]string{"populate", "--executor-root", "/Library/Freeside/macverify", "--source", "checkout"},
		&stdout, &stderr, populate)
	if err != nil {
		t.Fatalf("command: %v", err)
	}
	wantSource, err := filepath.Abs("checkout")
	if err != nil {
		t.Fatal(err)
	}
	if got.ExecutorRoot != "/Library/Freeside/macverify" || got.Source != wantSource || got.Progress == nil {
		t.Fatalf("options = %+v", got)
	}
	if want := "populated abc for app/Package.resolved\n"; stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestSwiftCacheCommandRefusesBadUsage(t *testing.T) {
	t.Parallel()
	populate := func(context.Context, macverify.PopulateOptions) (macverify.PopulateResult, error) {
		t.Fatal("populate ran")
		return macverify.PopulateResult{}, nil
	}
	for name, args := range map[string][]string{
		"no subcommand":    nil,
		"unknown":          {"evict"},
		"no root":          {"populate", "--source", "x"},
		"no source":        {"populate", "--executor-root", "/r"},
		"extra positional": {"populate", "--executor-root", "/r", "--source", "x", "y"},
	} {
		var stdout, stderr bytes.Buffer
		if err := runCacheCommand(t.Context(), "swift-cache", args, &stdout, &stderr, populate); err == nil {
			t.Errorf("%s: command accepted %v", name, args)
		} else if name == "no subcommand" && !strings.Contains(err.Error(), "usage") {
			t.Errorf("%s: error %v does not show usage", name, err)
		}
	}
}
