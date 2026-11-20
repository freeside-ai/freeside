package ward

import (
	"errors"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// HandoffCheckFailureOf is the typed, screened record of the handoff check an
// error carries, or nil when no ConformanceFailure is on its chain. Evidence
// is the failure's Reason only, screened by domain.ScreenFailureEvidence: the
// text of Err is withheld even where a failw caller also formatted it into the
// reason, because Err isn't held to the no-credential rule and may name host
// paths. The content-evidence mark carries over, so the record says whether a
// retry would be refused identically.
func HandoffCheckFailureOf(err error) *domain.HandoffCheckFailure {
	var cf *ConformanceFailure
	if !errors.As(err, &cf) {
		return nil
	}
	var withheld []string
	if cf.Err != nil {
		withheld = append(withheld, cf.Err.Error())
	}
	return &domain.HandoffCheckFailure{
		Check:           domain.WardCheck(cf.Check),
		Evidence:        domain.ScreenFailureEvidence(cf.Reason, withheld...),
		ContentEvidence: cf.ContentEvidence,
	}
}
