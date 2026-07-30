package ward

import (
	"errors"
	"strings"
	"testing"
)

func TestVerifyWriterOutcomeProof(t *testing.T) {
	t.Parallel()
	nonce := strings.Repeat("a", 32)
	tests := []struct {
		name   string
		proof  string
		status int
		ok     bool
	}{
		{"success", nonce + " 0\n", 0, true},
		{"failure", nonce + " 86\n", 86, true},
		{"missing newline", nonce + " 0", 0, false},
		{"extra line", nonce + " 0\nignored\n", 0, false},
		{"foreign nonce", strings.Repeat("b", 32) + " 0\n", 0, false},
		{"negative", nonce + " -1\n", 0, false},
		{"overflow", nonce + " 256\n", 0, false},
		{"leading zero", nonce + " 01\n", 0, false},
		{"extra field", nonce + " 0 extra\n", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := verifyWriterOutcomeProof([]byte(tc.proof), nonce)
			if tc.ok {
				if err != nil || got != tc.status {
					t.Fatalf("verify = (%d, %v), want (%d, nil)", got, err, tc.status)
				}
				return
			}
			if !errors.Is(err, ErrConformance) {
				t.Fatalf("verify error = %v, want ErrConformance", err)
			}
		})
	}
}
