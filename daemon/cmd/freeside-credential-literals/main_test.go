package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixtures below are split so this file passes the check it tests.
const (
	anthropicFixture = "sk-ant-" + "oat01-fixture-not-a-token-0123"
	pemHeader        = "-----BEGIN " + "RSA PRIVATE KEY-----"
	gcpKeyIDSplit    = `"private_key_id":` + "\n  " + `"0123456789abcdef0123456789abcdef01234567"`
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runWith(t *testing.T, paths ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(strings.NewReader(strings.Join(paths, "\x00")+"\x00"), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestReportsEachMatchByFileLineAndRuleWithoutTheMatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	clean := writeFile(t, dir, "clean.go", "package x\n\nconst prefix = \"sk-ant-\"\n")
	dirty := writeFile(t, dir, "dirty.txt", "line one\ntoken="+anthropicFixture+"\n\n"+pemHeader+"\n")

	code, stdout, stderr := runWith(t, clean, dirty)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (stderr %q)", code, stderr)
	}
	want := dirty + ":2: anthropic_token\n" + dirty + ":4: pem_private_key\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	if strings.Contains(stdout, anthropicFixture) || strings.Contains(stdout, pemHeader) {
		t.Fatal("output carries the matched text")
	}
}

// The GCP key-id rule's \s* spans a newline. Matching line by line would miss
// what the export scan, which matches whole chunks, refuses.
func TestMatchesARuleThatSpansLinesAtItsFirstLine(t *testing.T) {
	t.Parallel()
	path := writeFile(t, t.TempDir(), "key.json", "{\n  "+gcpKeyIDSplit+"\n}\n")

	code, stdout, _ := runWith(t, path)
	if code != 1 || stdout != path+":2: gcp_service_account_key\n" {
		t.Fatalf("exit %d, stdout %q; want 1 and the match at line 2", code, stdout)
	}
}

func TestPassesCleanFilesAndAnEmptyList(t *testing.T) {
	t.Parallel()
	path := writeFile(t, t.TempDir(), "clean.md", "Mentions sk-ant- and ghs_ prefixes, never a token.\n")
	if code, stdout, stderr := runWith(t, path); code != 0 || stdout != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q; want a clean pass", code, stdout, stderr)
	}
	var out, errOut bytes.Buffer
	if code := run(strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("empty list exit = %d, want 0", code)
	}
}

// A file that can't be read fails the check rather than passing it unscanned.
func TestFailsOnAnUnreadableFile(t *testing.T) {
	t.Parallel()
	code, stdout, stderr := runWith(t, filepath.Join(t.TempDir(), "missing.go"))
	if code != 2 || stdout != "" || !strings.Contains(stderr, "missing.go") {
		t.Fatalf("exit %d, stdout %q, stderr %q; want 2 naming the file", code, stdout, stderr)
	}
}
