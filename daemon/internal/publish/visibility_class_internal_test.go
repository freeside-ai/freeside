package publish

import (
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestClassForRepositoryVisibility proves the visibility->class mapping fails
// closed on an untrusted returned object: a partial or empty body, or a
// contradictory pair, is an error rather than the least restrictive class.
func TestClassForRepositoryVisibility(t *testing.T) {
	boolp := func(b bool) *bool { return &b }
	valid := map[string]struct {
		resp repoVisibilityResponse
		want domain.SensitivityClass
	}{
		"public":                 {repoVisibilityResponse{Visibility: "public"}, domain.SensitivityNormal},
		"public + private=false": {repoVisibilityResponse{Visibility: "public", Private: boolp(false)}, domain.SensitivityNormal},
		"private":                {repoVisibilityResponse{Visibility: "private"}, domain.SensitivitySensitive},
		"internal":               {repoVisibilityResponse{Visibility: "internal"}, domain.SensitivitySensitive},
		"private + private=true": {repoVisibilityResponse{Visibility: "private", Private: boolp(true)}, domain.SensitivitySensitive},
		// GitHub internal repositories report private=false; that pair is valid.
		"internal + private=false": {repoVisibilityResponse{Visibility: "internal", Private: boolp(false)}, domain.SensitivitySensitive},
		// A sensitive-mapped visibility is safe regardless of the private bool.
		"private + private=false": {repoVisibilityResponse{Visibility: "private", Private: boolp(false)}, domain.SensitivitySensitive},
		"legacy private=true":     {repoVisibilityResponse{Private: boolp(true)}, domain.SensitivitySensitive},
		"legacy private=false":    {repoVisibilityResponse{Private: boolp(false)}, domain.SensitivityNormal},
	}
	for name, tc := range valid {
		got, err := classForRepositoryVisibility(tc.resp)
		if err != nil || got != tc.want {
			t.Fatalf("%s: got %q err %v, want %q", name, got, err, tc.want)
		}
	}

	failClosed := map[string]repoVisibilityResponse{
		"empty response":          {},
		"no signal, private nil":  {Visibility: ""},
		"public but private=true": {Visibility: "public", Private: boolp(true)},
		"unrecognized visibility": {Visibility: "secret"},
	}
	for name, resp := range failClosed {
		if got, err := classForRepositoryVisibility(resp); err == nil {
			t.Fatalf("%s: expected fail-closed error, got class %q", name, got)
		}
	}
}
