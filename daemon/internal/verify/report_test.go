package verify_test

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

func TestParseReportCanonicalRoundTrip(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("testdata/verify_report.golden")
	if err != nil {
		t.Fatal(err)
	}
	rep, err := verify.ParseReport(raw)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.MarshalIndent(rep, "", "  ")
	if err != nil || !bytes.Equal(append(encoded, '\n'), raw) {
		t.Fatalf("round trip changed the verifier artifact: %v", err)
	}
	for name, invalid := range map[string][]byte{
		"unknown field":    bytes.Replace(raw, []byte(`"head_sha"`), []byte(`"unknown"`), 1),
		"duplicate field":  bytes.Replace(raw, []byte("{\n"), []byte("{\n  \"outcome\": \"passed\",\n"), 1),
		"invalid outcome":  bytes.Replace(raw, []byte(`"passed"`), []byte(`"pending"`), 1),
		"empty head":       bytes.Replace(raw, []byte(rep.HeadSHA), nil, 1),
		"empty recipe":     bytes.Replace(raw, []byte(rep.RecipeDigest), nil, 1),
		"empty executable": bytes.Replace(raw, []byte(`"go"`), []byte(`""`), 1),
		"trailing value":   append(append([]byte(nil), raw...), []byte("{}")...),
		"noncanonical":     bytes.TrimSpace(raw),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := verify.ParseReport(invalid); err == nil {
				t.Fatal("accepted invalid report")
			}
		})
	}
}
