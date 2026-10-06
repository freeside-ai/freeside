package publish

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestParseFollowUpFilingCaps(t *testing.T) {
	t.Parallel()
	policy := func(values map[string]string) domain.ResolvedPolicy {
		keys := []domain.PolicyKey{{Key: "paths", Value: "daemon/"}}
		for key, value := range values {
			keys = append(keys, domain.PolicyKey{Key: key, Value: value})
		}
		return domain.ResolvedPolicy{Keys: keys}
	}
	want := followUpFilingCaps{SettleInterval: 10 * time.Minute, MaxPerDay: 10, MaxDepth: 2, MaxPerRun: 5}
	if got, err := parseFollowUpFilingCaps(policy(nil)); err != nil || got != want {
		t.Fatalf("defaults = %+v, %v, want %+v", got, err, want)
	}
	set := followUpFilingCaps{SettleInterval: 90 * time.Second, MaxPerDay: 3, MaxDepth: 4, MaxPerRun: 1}
	got, err := parseFollowUpFilingCaps(policy(map[string]string{
		"follow_up_filing.settle_interval": "1m30s", "follow_up_filing.max_per_day": "3",
		"follow_up_filing.max_depth": "4", "follow_up_filing.max_per_run": "1",
	}))
	if err != nil || got != set {
		t.Fatalf("set values = %+v, %v, want %+v", got, err, set)
	}
	malformed := map[string][]string{
		"follow_up_filing.settle_interval": {"", "10", "0s", "-1m", "soon"},
		"follow_up_filing.max_per_day":     {"", "0", "-1", "1.5", "ten", "10m"},
		"follow_up_filing.max_depth":       {"", "0", "-1", "1.5", "deep"},
		"follow_up_filing.max_per_run":     {"", "0", "-1", "1.5", "few"},
	}
	for key, values := range malformed {
		for _, value := range values {
			got, err := parseFollowUpFilingCaps(policy(map[string]string{key: value}))
			if !errors.Is(err, errFollowUpFilingCapsInvalid) || !strings.Contains(err.Error(), key) ||
				got != (followUpFilingCaps{}) {
				t.Errorf("%s=%q: caps = %+v, err = %v, want the malformed-cap error naming the key", key, value, got, err)
			}
		}
	}
}
