package importer

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/export"
)

// fakeSecrets are structurally valid but non-live tokens: the scanner
// keys on structure, so these exercise every rule without being real
// credentials.
var fakeSecrets = map[string]string{
	"github_token":            "ghp_" + strings.Repeat("A", 36),
	"github_pat":              "github_pat_" + strings.Repeat("B", 30),
	"aws_access_key_id":       "AKIA" + strings.Repeat("Q", 16),
	"slack_token":             "xoxb-" + strings.Repeat("1", 20),
	"pem_private_key":         "-----BEGIN OPENSSH PRIVATE KEY-----",
	"gcp_service_account_key": `"private_key_id": "` + strings.Repeat("a", 40) + `"`,
}

func TestScanTextRules(t *testing.T) {
	for rule, token := range fakeSecrets {
		content := []byte("prefix line\nvalue = " + token + "\ntrailer\n")
		findings := scanText("config.txt", content)
		if len(findings) != 1 {
			t.Fatalf("rule %s: got %d findings, want 1: %+v", rule, len(findings), findings)
		}
		f := findings[0]
		if f.Rule != rule || f.Line != 2 || f.Kind != FindingSecret {
			t.Errorf("rule %s finding = %+v, want rule=%s line=2", rule, f, rule)
		}
	}
}

// TestScanTextLongLine is the Codex P2 regression: a token on a line
// far longer than any scanner buffer must still be found, not silently
// dropped at ErrTooLong.
func TestScanTextLongLine(t *testing.T) {
	token := "ghp_" + strings.Repeat("A", 36)
	// A single line well over a megabyte with the token at its end,
	// separated by a non-word byte as it would be in a real env or
	// minified line (var TOKEN="...").
	line := strings.Repeat("x", 2<<20) + `TOKEN="` + token + `"`
	findings := scanText("min.js", []byte(line+"\n"))
	if len(findings) != 1 || findings[0].Rule != "github_token" || findings[0].Line != 1 {
		t.Fatalf("long-line token must be found: %+v", findings)
	}
}

// TestSplitLinesNoLimit sanity-checks the newline splitter's line
// numbering and CRLF handling with no length cap.
func TestSplitLinesNoLimit(t *testing.T) {
	lines := splitLines([]byte("a\r\nb\nc"))
	got := make([]string, len(lines))
	for i, l := range lines {
		got[i] = string(l)
	}
	if strings.Join(got, "|") != "a|b|c" {
		t.Fatalf("splitLines = %v, want [a b c]", got)
	}
}

func TestScanTextNoFalsePositive(t *testing.T) {
	content := []byte("The API returns a token; set GITHUB_TOKEN in CI.\nghp_short\n")
	if findings := scanText("readme.md", content); len(findings) != 0 {
		t.Errorf("mentions must not match: %+v", findings)
	}
}

func TestImportSecretScan(t *testing.T) {
	token := "ghp_" + strings.Repeat("Z", 36)
	fileContent := "aws := \"AKIA" + strings.Repeat("Q", 16) + "\"\ntoken := \"" + token + "\"\n"
	checkout, base := initBaseRepo(t, map[string]string{"a.txt": "old\n"})
	handoff := handoffFromEntries(t, []export.Entry{
		regularEntryFor("secrets.go", fileContent, false),
	}, fileContent)
	clone := cloneAtBase(t, checkout)
	res, err := Import(t.Context(), handoff, clone, testImportOptions(base))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.CommitSHA == "" {
		t.Fatal("a secret finding is publish-blocking, not construction-blocking")
	}
	rules := map[string]bool{}
	for _, f := range res.Findings {
		if f.Kind == FindingSecret {
			rules[f.Rule] = true
		}
	}
	if !rules["aws_access_key_id"] || !rules["github_token"] {
		t.Fatalf("expected aws and github findings, got %+v", res.Findings)
	}

	// Leak-safety: neither the whole Result nor any finding field may
	// carry the matched secret bytes.
	blob, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), token) || strings.Contains(string(blob), "AKIA"+strings.Repeat("Q", 16)) {
		t.Fatal("Result JSON leaked a matched secret")
	}
	for _, f := range res.Findings {
		if strings.Contains(f.Detail, token) || strings.Contains(f.Path, token) {
			t.Fatalf("finding leaked the secret: %+v", f)
		}
	}
}

// TestImportSecretScanInvalidUTF8 is the round-17 regression: one
// invalid byte must not turn an otherwise textual credential file into
// a silent scan skip.
func TestImportSecretScanInvalidUTF8(t *testing.T) {
	token := "ghp_" + strings.Repeat("V", 36)
	content := string([]byte{0xff}) + "TOKEN=" + token + "\n"
	checkout, base := initBaseRepo(t, nil)
	handoff := handoffFromEntries(t, []export.Entry{
		regularEntryFor(".env", content, false),
	}, content)
	res, err := Import(t.Context(), handoff, cloneAtBase(t, checkout), testImportOptions(base))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(res.Findings) != 1 || res.Findings[0].Kind != FindingSecret || res.Findings[0].Rule != "github_token" {
		t.Fatalf("invalid UTF-8 prefixed token was not detected: %+v", res.Findings)
	}
}

func TestImportSecretScanReplaysDeduplicatedBlobFindings(t *testing.T) {
	token := "ghp_" + strings.Repeat("D", 36)
	content := "TOKEN=" + token + "\n"
	checkout, base := initBaseRepo(t, nil)
	handoff := handoffFromEntries(t, []export.Entry{
		regularEntryFor("a.env", content, false),
		regularEntryFor("b.env", content, false),
	}, content)
	res, err := Import(t.Context(), handoff, cloneAtBase(t, checkout), testImportOptions(base))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(res.Findings) != 2 || res.Findings[0].Path != "a.env" || res.Findings[1].Path != "b.env" {
		t.Fatalf("deduplicated blob findings were not replayed per path: %+v", res.Findings)
	}
}

// TestImportSecretScanOverCapSurfaced is the Codex round-4 regression:
// an added file over the scan cap but under the size policy is not
// silently skipped; it produces a non-blocking secret_scan_skipped
// finding so a clean import never hides an unscanned file.
func TestImportSecretScanOverCapSurfaced(t *testing.T) {
	checkout, base := initBaseRepo(t, map[string]string{"a.txt": "old\n"})
	big := strings.Repeat("x", 4096) + "\n"
	handoff := handoffFromEntries(t, []export.Entry{
		regularEntryFor("big.txt", big, false),
	}, big)
	clone := cloneAtBase(t, checkout)
	opts := testImportOptions(base)
	opts.Policy.SecretMaxScanBytes = 1024 // below the file, above nothing else
	res, err := Import(t.Context(), handoff, clone, opts)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	skipped := 0
	for _, f := range res.Findings {
		if f.Kind == FindingSecretScanSkipped && f.Path == "big.txt" {
			skipped++
		}
	}
	if skipped != 1 {
		t.Fatalf("expected one secret_scan_skipped finding for big.txt, got %+v", res.Findings)
	}
	if res.CommitSHA == "" {
		t.Fatal("an unscanned-file finding is non-blocking; the commit must still exist")
	}
}

func TestImportSecretScanSkipsUnchanged(t *testing.T) {
	token := "ghp_" + strings.Repeat("C", 36)
	// The secret sits in an unchanged base file; the candidate leaves
	// it byte-identical, so it is not the candidate's doing and is not
	// re-flagged.
	checkout, _ := initBaseRepo(t, map[string]string{
		"existing.txt": "token = " + token + "\n",
	})
	base := rungit(t, checkout, "rev-parse", "HEAD")
	handoff := handoffFromEntries(t, []export.Entry{
		regularEntryFor("existing.txt", "token = "+token+"\n", false),
		regularEntryFor("new.txt", "clean\n", false),
	}, "token = "+token+"\n", "clean\n")
	clone := cloneAtBase(t, checkout)
	res, err := Import(t.Context(), handoff, clone, testImportOptions(base))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	for _, f := range res.Findings {
		if f.Kind == FindingSecret {
			t.Fatalf("unchanged base secret must not be re-flagged: %+v", f)
		}
	}
}
