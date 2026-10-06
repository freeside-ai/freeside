package publish

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Resolved-policy keys that bound follow-up filing (plan §5.17: "Rate, depth,
// and cost caps come from resolved policy"). They are publish-local constants
// on the gates.source_issue_closure pattern: the filer reads them from the
// origin run's stored policy, and the domain-change ban keeps them out of a
// shared package.
const (
	// policyFollowUpFilingSettleInterval is the wait after an unproven create
	// before the candidate listing.
	policyFollowUpFilingSettleInterval = "follow_up_filing.settle_interval"
	// policyFollowUpFilingMaxPerDay caps the filings per repository in the
	// trailing 24 hours that may have created an issue. Over it a filing
	// waits: no intent opens until the window frees.
	policyFollowUpFilingMaxPerDay = "follow_up_filing.max_per_day"
	// policyFollowUpFilingMaxDepth caps the new issue's depth in
	// Freeside-filed ancestry: one more than the number of filed follow-up
	// issues above the origin run's bound issue. Over it a filing is refused.
	policyFollowUpFilingMaxDepth = "follow_up_filing.max_depth"
	// policyFollowUpFilingMaxPerRun caps the filings per origin run that may
	// have created an issue. Over it a filing is refused.
	policyFollowUpFilingMaxPerRun = "follow_up_filing.max_per_run"
)

// errFollowUpFilingCapsInvalid reports a present but malformed cap value. It
// fails closed to a refused filing: a configuration typo never lifts a cap.
var errFollowUpFilingCapsInvalid = errors.New("follow-up filing cap policy value is malformed")

// followUpFilingCaps is the parsed cap set for one origin run.
type followUpFilingCaps struct {
	SettleInterval time.Duration
	MaxPerDay      int
	MaxDepth       int
	MaxPerRun      int
}

var defaultFollowUpFilingCaps = followUpFilingCaps{
	SettleInterval: 10 * time.Minute,
	MaxPerDay:      10,
	MaxDepth:       2,
	MaxPerRun:      5,
}

// parseFollowUpFilingCaps reads the cap keys from a run's resolved policy. A
// missing key takes its default; a present key must be a positive duration
// or a positive integer.
func parseFollowUpFilingCaps(policy domain.ResolvedPolicy) (followUpFilingCaps, error) {
	caps := defaultFollowUpFilingCaps
	for _, k := range policy.Keys {
		switch k.Key {
		case policyFollowUpFilingSettleInterval:
			interval, err := time.ParseDuration(k.Value)
			if err != nil || interval <= 0 {
				return followUpFilingCaps{}, fmt.Errorf("%s: %w", k.Key, errFollowUpFilingCapsInvalid)
			}
			caps.SettleInterval = interval
		case policyFollowUpFilingMaxPerDay, policyFollowUpFilingMaxDepth, policyFollowUpFilingMaxPerRun:
			limit, err := strconv.Atoi(k.Value)
			if err != nil || limit <= 0 {
				return followUpFilingCaps{}, fmt.Errorf("%s: %w", k.Key, errFollowUpFilingCapsInvalid)
			}
			switch k.Key {
			case policyFollowUpFilingMaxPerDay:
				caps.MaxPerDay = limit
			case policyFollowUpFilingMaxDepth:
				caps.MaxDepth = limit
			case policyFollowUpFilingMaxPerRun:
				caps.MaxPerRun = limit
			}
		}
	}
	return caps, nil
}
