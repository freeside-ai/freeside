package domain_test

import (
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestExternalCommentLabeledBlocking(t *testing.T) {
	for message, want := range map[string]bool{
		"[P3, blocking] Declare 401":                 true,
		"  **[Blocking]** nil handle":                true,
		"[nit,BLOCKING , P2] close it":               true,
		"[P3 blocking] spaced":                       true,
		"[P3 | blocking] piped":                      true,
		"[P3/blocking] slashed":                      true,
		"[P3, blocking_soon] joined":                 false,
		"[P3][blocking] second badge":                false,
		"[P3] blocking: the handle leaks":            false,
		"Declare 401 [blocking]":                     false,
		"[P3, non-blocking] style":                   false,
		"[P3, blocking":                              false,
		"(blocking) parenthesized":                   false,
		"[" + strings.Repeat("x,", 40) + "blocking]": false,
	} {
		if got := domain.ExternalCommentLabeledBlocking(message); got != want {
			t.Errorf("%q = %t, want %t", message, got, want)
		}
	}
}
