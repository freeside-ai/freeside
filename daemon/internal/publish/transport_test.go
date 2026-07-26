package publish

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

// staticTokenSource is the internal-package counterpart of the
// external tests' fixedTokenSource: a canned token with no minting.
type staticTokenSource struct{ tok InstallationToken }

func (s staticTokenSource) Token(context.Context, string) (InstallationToken, error) {
	return s.tok, nil
}

// TestTransportArgvGolden pins the exact argument vectors the
// transport hands to git: the hardened config prefix and the fixed
// fetch and push shapes. Any drift here changes the lane's protocol
// policy or ref discipline and must be a reviewed change.
func TestTransportArgvGolden(t *testing.T) {
	fixture := struct {
		HardenedConfig []string `json:"hardened_config"`
		Fetch          []string `json:"fetch"`
		Push           []string `json:"push"`
	}{
		HardenedConfig: transportConfig("https"),
		Fetch:          fetchArgs("https://github.com/freeasinbird/example.git", "main"),
		Push: pushArgs(
			"https://github.com/freeasinbird/example.git",
			"0123456789abcdef0123456789abcdef01234567",
			"freeside/publish/0123456789abcdef",
		),
	}
	b, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "transport-argv", append(b, '\n'))
}

func TestNewTransportValidates(t *testing.T) {
	if _, err := NewTransport(nil, TransportOptions{}); err == nil {
		t.Error("nil token source accepted")
	}
	if _, err := NewTransport(staticTokenSource{}, TransportOptions{RemoteBase: "http://github.com"}); err == nil {
		t.Error("non-https remote base accepted")
	}
	if _, err := NewTransport(staticTokenSource{}, TransportOptions{RemoteBase: "file:///tmp/x"}); err == nil {
		t.Error("file remote base accepted by the production constructor")
	}
	tr, err := NewTransport(staticTokenSource{}, TransportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if tr.remoteBase != "https://github.com" || tr.gitPath != "git" || tr.scheme != "https" {
		t.Errorf("defaults = %q %q %q", tr.remoteBase, tr.gitPath, tr.scheme)
	}
}

// TestValidRemoteBase enumerates the remote-base input space rather
// than the one shape a finding cited: anything carrying a credential,
// a non-https scheme, no host, or uninterpreted trailing material
// must be refused, because the composed repository URL is argv and
// error material the transport promises is credential-free.
func TestValidRemoteBase(t *testing.T) {
	for _, base := range []string{
		"https://github.com",
		"https://github.com/",
		"https://ghe.example.test:8443",
		"https://ghe.example.test/prefix",
	} {
		if err := validRemoteBase(base); err != nil {
			t.Errorf("validRemoteBase(%q) = %v, want nil", base, err)
		}
	}
	for _, base := range []string{
		"",                                  // empty
		"github.com",                        // no scheme
		"http://github.com",                 // plaintext
		"HTTP://github.com",                 // plaintext, cased
		"ssh://git@github.com",              // wrong scheme
		"file:///tmp/x",                     // wrong scheme
		"git://github.com",                  // wrong scheme
		"https://",                          // no host
		"https:///path",                     // no host
		"https:github.com",                  // opaque, no host
		"https://user@github.com",           // userinfo
		"https://user:password@github.com",  // userinfo with secret
		"https://:password@github.com",      // userinfo, empty user
		"https://user:password@github.com/", // userinfo with trailing slash
		"https://github.com?a=b",            // query
		"https://github.com/?",              // forced query
		"https://github.com#frag",           // fragment
		"https://git\nhub.com",              // control character
	} {
		if err := validRemoteBase(base); err == nil {
			t.Errorf("validRemoteBase(%q) accepted", base)
		}
	}
}

// TestRemoteBaseRefusalNeverLeaksCredentials crosses every rejection
// branch with every position a credential can occupy in a URL. Two
// rounds of review found leaks by moving the secret to a component
// the last fix did not cover (first userinfo, then an opaque body,
// then a query), so the enumeration is the product of both axes
// rather than a list of the shapes previously cited: the property is
// "no rejected base is echoed", not "the known credential fields are
// stripped".
func TestRemoteBaseRefusalNeverLeaksCredentials(t *testing.T) {
	const secret = "hunter2supersecret"
	// Each template places the secret in a different URL position.
	positions := map[string]string{
		"userinfo password": "SCHEME://user:" + secret + "@HOST/p",
		"userinfo user":     "SCHEME://" + secret + "@HOST/p",
		"host":              "SCHEME://" + secret + ".HOST/p",
		"port-ish":          "SCHEME://HOST:" + secret + "/p",
		"path":              "SCHEME://HOST/" + secret,
		"query":             "SCHEME://HOST/p?token=" + secret,
		"fragment":          "SCHEME://HOST/p#" + secret,
		"opaque":            "SCHEME:" + secret + "@HOST",
		"unparseable":       "SCHEME://HOST/p%zz?token=" + secret,
		"control character": "SCHEME://HOST/p\n" + secret,
	}
	// Each variant steers the parsed value into a different branch.
	variants := map[string]struct{ scheme, host string }{
		"https":      {"https", "github.com"},
		"http":       {"http", "github.com"},
		"ssh":        {"ssh", "github.com"},
		"empty host": {"https", ""},
	}
	for pos, template := range positions {
		for variant, v := range variants {
			base := strings.NewReplacer("SCHEME", v.scheme, "HOST", v.host).Replace(template)
			if err := validRemoteBase(base); err == nil {
				// A few crossings are legitimately valid URLs (a secret
				// in the path of an https origin); those are accepted by
				// design and have nothing to leak.
				continue
			}
			t.Run(pos+"/"+variant, func(t *testing.T) {
				err := validRemoteBase(base)
				rendered := fmt.Sprintf("%v|%s|%q|%+v", err, err, err, err)
				if strings.Contains(rendered, secret) {
					t.Errorf("refusal leaked the credential: %s", rendered)
				}
				_, cErr := NewTransport(staticTokenSource{}, TransportOptions{RemoteBase: base})
				if cErr == nil {
					t.Fatal("NewTransport accepted a base validRemoteBase rejected")
				}
				if strings.Contains(fmt.Sprintf("%v|%+v", cErr, cErr), secret) {
					t.Errorf("NewTransport refusal leaked the credential: %v", cErr)
				}
			})
		}
	}
}

func TestValidBranchName(t *testing.T) {
	valid := []string{
		"main", "freeside/publish/0123abcd", "release-1.2", "a_b", "x.y/z",
	}
	for _, name := range valid {
		if !validBranchName(name) {
			t.Errorf("validBranchName(%q) = false, want true", name)
		}
	}
	invalid := []string{
		"",                                        // empty
		"-flag",                                   // option-shaped
		".hidden",                                 // leading dot
		"/abs",                                    // leading slash
		"trail/",                                  // trailing slash
		"trail.",                                  // trailing dot
		"a.lock",                                  // lock suffix
		"a..b",                                    // traversal
		"a//b",                                    // empty segment
		"a@{1}",                                   // reflog syntax
		"a:b",                                     // second refspec component
		"a b",                                     // whitespace
		"a\tb",                                    // whitespace
		"a\nb",                                    // newline
		"a*b", "a?b", "a[b", "a\\b", "a~b", "a^b", // git-forbidden metacharacters
		string(make([]byte, 256)), // over-long
	}
	for _, name := range invalid {
		if validBranchName(name) {
			t.Errorf("validBranchName(%q) = true, want false", name)
		}
	}
}

func TestValidCommitSHA(t *testing.T) {
	if !validCommitSHA("0123456789abcdef0123456789abcdef01234567") {
		t.Error("full lowercase sha rejected")
	}
	for _, s := range []string{
		"",
		"0123456789abcdef0123456789abcdef0123456",   // 39
		"0123456789abcdef0123456789abcdef012345678", // 41
		"0123456789ABCDEF0123456789ABCDEF01234567",  // uppercase
		"0123456789abcdef0123456789abcdef0123456g",  // non-hex
		"main", "HEAD",
	} {
		if validCommitSHA(s) {
			t.Errorf("validCommitSHA(%q) = true, want false", s)
		}
	}
}

func TestParseTransportRepo(t *testing.T) {
	if _, err := parseTransportRepo("freeasinbird/gh-imgup"); err != nil {
		t.Errorf("plain owner/name rejected: %v", err)
	}
	if _, err := parseTransportRepo("owner/repo.name_1-x"); err != nil {
		t.Errorf("dotted/underscored name rejected: %v", err)
	}
	for _, repo := range []string{
		"", "owner", "owner/", "/name", "owner/name/extra",
		"-owner/name", "owner/-name", // option-shaped
		".owner/name", "owner/.name", // dot-leading (traversal shapes)
		"owner/na me", "owner/na:me", "owner/na\tme", "owner/naïve",
	} {
		if _, err := parseTransportRepo(repo); err == nil {
			t.Errorf("parseTransportRepo(%q) accepted", repo)
		}
	}
}
