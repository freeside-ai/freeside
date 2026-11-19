package importer

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/export"
)

// fakeSecrets are structurally valid but non-live tokens: the scanner
// keys on structure, so these exercise every rule without being real
// credentials.
var fakeSecrets = []struct {
	name  string
	rule  string
	token string
}{
	{"anthropic API key", "anthropic_token", "sk-ant-api03-" + strings.Repeat("A", 40)},
	{"anthropic setup token", "anthropic_token", "sk-ant-oat01-" + strings.Repeat("B", 40)},
	{"GitHub token", "github_token", "ghp_" + strings.Repeat("A", 36)},
	{"GitHub fine-grained token", "github_pat", "github_pat_" + strings.Repeat("B", 30)},
	{"AWS access key id", "aws_access_key_id", "AKIA" + strings.Repeat("Q", 16)},
	{"Slack token", "slack_token", "xoxb-" + strings.Repeat("1", 20)},
	{"PEM private key", "pem_private_key", "-----BEGIN " + "OPENSSH PRIVATE KEY-----"},
	{"GCP service account key", "gcp_service_account_key", `"private_key_id": "` + strings.Repeat("a", 40) + `"`},
}

func TestScanTextRules(t *testing.T) {
	t.Parallel()
	for _, fixture := range fakeSecrets {
		t.Run(fixture.name, func(t *testing.T) {
			content := []byte("prefix line\nvalue = " + fixture.token + "\ntrailer\n")
			findings := scanText("config.txt", content)
			if len(findings) != 1 {
				t.Fatalf("rule %s: got %d findings, want 1: %+v", fixture.rule, len(findings), findings)
			}
			f := findings[0]
			if f.Rule != fixture.rule || f.Line != 2 || f.Kind != FindingSecret {
				t.Errorf("rule %s finding = %+v, want rule=%s line=2", fixture.rule, f, fixture.rule)
			}
		})
	}
}

func TestScanTextAnthropicTokenWithoutWordBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
	}{
		{"preceded by a word byte", "prefixsk-ant-oat01-" + strings.Repeat("C", 40)},
		{"ending in a hyphen", "sk-ant-api03-" + strings.Repeat("D", 9) + "-"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			findings := scanText("config.txt", []byte(tc.content))
			if len(findings) != 1 || findings[0].Rule != "anthropic_token" {
				t.Fatalf("boundary-adjacent Anthropic token must be found: %+v", findings)
			}
		})
	}
}

// TestGitHubTokenWithoutWordBoundaries pins the shared GitHub token rule on
// the importer's screens: the importer's own rule once required word
// boundaries and 36 characters, so these three shapes passed
// ContainsSecret while the export scan refused them.
func TestGitHubTokenWithoutWordBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
	}{
		{"preceded by a word byte", "GH_TOKEN_ghp_" + strings.Repeat("E", 36)},
		{"followed by an underscore", "ghs_" + strings.Repeat("F", 36) + "_x"},
		{"sixteen characters", "gho_" + strings.Repeat("G", 16)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !ContainsSecret([]byte(tc.content)) {
				t.Error("ContainsSecret must report a boundary-adjacent or short GitHub token")
			}
			findings := scanText("config.txt", []byte(tc.content))
			if len(findings) != 1 || findings[0].Rule != "github_token" {
				t.Fatalf("boundary-adjacent or short GitHub token must be found: %+v", findings)
			}
		})
	}
}

// TestShortGitHubTokenPrefixPasses keeps the rule's floor: a bare prefix
// or a run under 16 characters is a mention, not a token.
func TestShortGitHubTokenPrefixPasses(t *testing.T) {
	t.Parallel()
	for _, content := range []string{"ghp_", "ghp_" + strings.Repeat("H", 15)} {
		if ContainsSecret([]byte(content)) {
			t.Errorf("ContainsSecret(%q) = true, want false", content)
		}
	}
}

// TestScanTextLongLine is the Codex P2 regression: a token on a line
// far longer than any scanner buffer must still be found, not silently
// dropped at ErrTooLong.
func TestScanTextLongLine(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	var got []string
	for i, l := range splitLines([]byte("a\r\nb\nc")) {
		got = append(got, fmt.Sprintf("%d:%s", i, l))
	}
	if strings.Join(got, "|") != "0:a|1:b|2:c" {
		t.Fatalf("splitLines = %v, want [0:a 1:b 2:c]", got)
	}
}

func TestScanTextNoFalsePositive(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
	}{
		{"credential vocabulary", "The API returns a token; set GITHUB_TOKEN in CI.\n"},
		{"short GitHub token", "ghp_short\n"},
		{"redacted Anthropic token", "sk-ant-oat01-...\n"},
		{"short Anthropic token", "sk-ant-api03-" + strings.Repeat("A", 9) + "\n"},
		{"wrong Anthropic separator", "sk_ant_oat01_" + strings.Repeat("C", 40) + "\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if findings := scanText("readme.md", []byte(tc.content)); len(findings) != 0 {
				t.Errorf("mention or malformed token must not match: %+v", findings)
			}
		})
	}
}

func TestImportSecretScan(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// TestImportSecretScanCoversLargeFiles: a changed file over the evidence
// scan cap is scanned whole, not skipped (#3396). A clean one imports with
// no finding, and a token past the first MiB is still found on its line.
// It supersedes TestImportSecretScanOverCapSurfaced, which asserted the
// skip finding the importer no longer emits.
func TestImportSecretScanCoversLargeFiles(t *testing.T) {
	t.Parallel()
	checkout, base := initBaseRepo(t, map[string]string{"a.txt": "old\n"})
	filler := strings.Repeat(strings.Repeat("x", 1023)+"\n", 1100) // 1100 KiB of lines
	token := "ghp_" + strings.Repeat("D", 36)
	clean, leaked := filler, filler+"token = "+token+"\n"
	handoff := handoffFromEntries(t, []export.Entry{
		regularEntryFor("clean.swift", clean, false),
		regularEntryFor("leaked.swift", leaked, false),
	}, clean, leaked)
	opts := testImportOptions(base)
	opts.Policy.SecretMaxScanBytes = 1024 // the evidence cap no longer bounds repository content
	res, err := Import(t.Context(), handoff, cloneAtBase(t, checkout), opts)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(res.Findings) != 1 || res.Findings[0].Kind != FindingSecret ||
		res.Findings[0].Path != "leaked.swift" || res.Findings[0].Line != 1101 {
		t.Fatalf("findings = %+v, want one secret on leaked.swift line 1101", res.Findings)
	}
}

func TestImportSecretScanSkipsUnchanged(t *testing.T) {
	t.Parallel()
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
