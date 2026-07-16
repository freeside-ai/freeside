package signet_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/signet"
)

// secretValue is deliberately distinctive so any leak is unambiguous in
// a failure message.
const secretValue = "tk_SECRETSECRETSECRETSECRET"

// TestSecretRendersRedactedEverywhere drives every implicit rendering a
// Secret can reach — each fmt verb (including the non-string verbs that
// bypass Stringer), fmt.Stringer, fmt.GoStringer, JSON and text
// marshalling — and asserts the value never appears.
func TestSecretRendersRedactedEverywhere(t *testing.T) {
	s := signet.Secret(secretValue)

	for _, verb := range []string{"%s", "%q", "%v", "%+v", "%#v", "%x", "%X", "%d"} {
		got := fmt.Sprintf(verb, s)
		if strings.Contains(got, secretValue) {
			t.Errorf("fmt.Sprintf(%q) leaked the value: %s", verb, got)
		}
		if !strings.Contains(got, "[REDACTED]") {
			t.Errorf("fmt.Sprintf(%q) = %q, want it to contain [REDACTED]", verb, got)
		}
	}

	if got := s.String(); got != "[REDACTED]" {
		t.Errorf("String() = %q, want [REDACTED]", got)
	}
	if got := s.GoString(); got != "[REDACTED]" {
		t.Errorf("GoString() = %q, want [REDACTED]", got)
	}

	j, err := json.Marshal(struct {
		Token signet.Secret `json:"token"`
	}{Token: s})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if strings.Contains(string(j), secretValue) {
		t.Errorf("json.Marshal leaked the value: %s", j)
	}
	if want := `{"token":"[REDACTED]"}`; string(j) != want {
		t.Errorf("json.Marshal = %s, want %s", j, want)
	}

	txt, err := s.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText: %v", err)
	}
	if string(txt) != "[REDACTED]" {
		t.Errorf("MarshalText = %q, want [REDACTED]", txt)
	}
}

// TestSecretDecodesAndReveals covers the two deliberate paths through
// the redaction boundary: decoding a configuration value into a Secret
// and revealing it at a use site.
func TestSecretDecodesAndReveals(t *testing.T) {
	var got struct {
		Token signet.Secret `json:"token"`
	}
	if err := json.Unmarshal([]byte(`{"token":"`+secretValue+`"}`), &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if got.Token.Reveal() != secretValue {
		t.Errorf("Reveal() = %q, want the decoded value", got.Token.Reveal())
	}
}
