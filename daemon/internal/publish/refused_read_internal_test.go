package publish

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

// TestRefusedReadOnlyForListReaderRefusals proves refusedRead matches only the
// refusal the list reader marks, not any 403: a token mint that fails with
// 403 before the reactions read is sent must still fail the observation
// (#2717).
func TestRefusedReadOnlyForListReaderRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"list reader refusal", fmt.Errorf("get issue reactions: %w", &APIError{Status: http.StatusForbidden, refused: true}), true},
		{"mint 403", fmt.Errorf("mint: %w", &APIError{Status: http.StatusForbidden, RequestPath: "/app/installations/1/access_tokens"}), false},
		{"other status", &APIError{Status: http.StatusNotFound}, false},
		{"transport", errors.New("connection reset"), false},
		{"nil", nil, false},
	} {
		if got := refusedRead(tc.err); got != tc.want {
			t.Errorf("%s: refusedRead = %v, want %v", tc.name, got, tc.want)
		}
	}
}
