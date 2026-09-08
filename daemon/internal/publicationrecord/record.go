// Package publicationrecord owns the durable, store-independent publication
// intent and outcome contracts. Both the publisher and data migrations decode
// through this package so historical rows cannot acquire authority under a
// weaker reconstruction rule.
package publicationrecord

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const (
	IntentKindPublication = "publish.publication"
	IntentKindOutcome     = "publish.outcome"
	IntentFormatLegacy    = 1
	IntentFormatHistory   = 2
	IntentFormatCurrent   = 3
	branchPrefix          = "freeside/publish/"
	branchDigestHexLen    = 16
)

// Intent is the durable publication effect recorded before dispatch.
type Intent struct {
	FormatVersion            int                 `json:"format_version"`
	Identity                 domain.Digest       `json:"identity"`
	InvocationID             domain.InvocationID `json:"invocation_id"`
	Repo                     string              `json:"repo"`
	BaseRef                  string              `json:"base_ref"`
	Branch                   string              `json:"branch,omitempty"`
	SourceHeadSHA            string              `json:"source_head_sha"`
	AuthorizationID          domain.Digest       `json:"authorization_id"`
	DispositionHistoryDigest domain.Digest       `json:"disposition_history_digest,omitempty"`
	ProducingInvocationID    domain.InvocationID `json:"producing_invocation_id,omitempty"`
	ReservationRunID         domain.RunID        `json:"reservation_run_id,omitempty"`
}

func (i Intent) Validate() error {
	if i.FormatVersion != IntentFormatLegacy && i.FormatVersion != IntentFormatHistory && i.FormatVersion != IntentFormatCurrent {
		return fmt.Errorf("intent: unsupported format version %d", i.FormatVersion)
	}
	if i.FormatVersion == IntentFormatLegacy && i.DispositionHistoryDigest != "" {
		return errors.New("intent: legacy format carries disposition history")
	}
	if !contentaddr.Valid(string(i.Identity)) {
		return fmt.Errorf("intent identity %q is not a publication identity digest", i.Identity)
	}
	if i.FormatVersion == IntentFormatCurrent {
		if err := validateResolvedBranch(i.Identity, i.Branch, i.BaseRef); err != nil {
			return fmt.Errorf("intent: %w", err)
		}
	} else if i.Branch != "" {
		return errors.New("intent: old format carries branch")
	}
	if i.InvocationID == "" {
		return errors.New("intent: empty invocation id")
	}
	if i.Repo == "" {
		return errors.New("intent: empty repository")
	}
	if i.BaseRef == "" {
		return errors.New("intent: empty base ref")
	}
	if i.SourceHeadSHA == "" {
		return errors.New("intent: empty source head sha")
	}
	if !contentaddr.Valid(string(i.AuthorizationID)) {
		return fmt.Errorf("intent authorization_id %q is not a digest", i.AuthorizationID)
	}
	if i.DispositionHistoryDigest != "" && !contentaddr.Valid(string(i.DispositionHistoryDigest)) {
		return fmt.Errorf("intent disposition_history_digest %q is not a digest", i.DispositionHistoryDigest)
	}
	if (i.ProducingInvocationID == "") != (i.ReservationRunID == "") {
		return errors.New("intent: producing invocation and reservation run must be present together")
	}
	return nil
}

func (i Intent) Encode() ([]byte, error) {
	if err := i.Validate(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(i)
	if err != nil {
		return nil, fmt.Errorf("intent: encode: %w", err)
	}
	return payload, nil
}

func DecodeIntent(payload []byte) (Intent, error) {
	var intent Intent
	if err := decode(payload, &intent); err != nil {
		return Intent{}, fmt.Errorf("intent: decode: %w", err)
	}
	if err := intent.Validate(); err != nil {
		return Intent{}, err
	}
	return intent, nil
}

func IntentKey(invocationID domain.InvocationID, kind string) (string, error) {
	if invocationID == "" {
		return "", errors.New("intent key: empty invocation id")
	}
	if kind == "" {
		return "", errors.New("intent key: empty kind")
	}
	return "publish/" + string(invocationID) + "/" + kind, nil
}

// Outcome is the durable converged publication result.
type Outcome struct {
	Identity         domain.Digest `json:"identity"`
	Repo             string        `json:"repo"`
	BaseRef          string        `json:"base_ref"`
	HeadSHA          string        `json:"head_sha"`
	Branch           string        `json:"branch"`
	PRNumber         int           `json:"pr_number"`
	EvidenceEligible bool          `json:"evidence_eligible"`
}

func (o Outcome) Validate() error {
	if !contentaddr.Valid(string(o.Identity)) {
		return fmt.Errorf("outcome identity %q is not a publication identity digest", o.Identity)
	}
	if o.Repo == "" {
		return errors.New("outcome: empty repository")
	}
	if o.BaseRef == "" {
		return errors.New("outcome: empty base ref")
	}
	if o.HeadSHA == "" {
		return errors.New("outcome: empty head sha")
	}
	if err := validateResolvedBranch(o.Identity, o.Branch, o.BaseRef); err != nil {
		return fmt.Errorf("outcome: %w", err)
	}
	if o.PRNumber <= 0 {
		return fmt.Errorf("outcome: non-positive pr number %d", o.PRNumber)
	}
	if !o.EvidenceEligible {
		return errors.New("outcome: evidence is not eligible")
	}
	return nil
}

func (o Outcome) Encode() ([]byte, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(o)
	if err != nil {
		return nil, fmt.Errorf("outcome: encode: %w", err)
	}
	return payload, nil
}

func DecodeOutcome(payload []byte) (Outcome, error) {
	var outcome Outcome
	if err := decode(payload, &outcome); err != nil {
		return Outcome{}, fmt.Errorf("outcome: decode: %w", err)
	}
	if err := outcome.Validate(); err != nil {
		return Outcome{}, err
	}
	return outcome, nil
}

func OutcomeKey(identity domain.Digest) string {
	return IntentKindOutcome + "/" + string(identity)
}

func BranchName(identity domain.Digest) string {
	if !contentaddr.Valid(string(identity)) {
		return ""
	}
	hexPart := contentaddr.Hex(string(identity))
	return branchPrefix + hexPart[:branchDigestHexLen]
}

// ExpectedBranch returns the branch bound by a validated durable intent.
// Old formats retain the identity-derived name without rewriting their rows.
func ExpectedBranch(intent Intent) string {
	if intent.FormatVersion == IntentFormatCurrent {
		return intent.Branch
	}
	return BranchName(intent.Identity)
}

func validateResolvedBranch(identity domain.Digest, branch, baseRef string) error {
	if branch == BranchName(identity) {
		return nil
	}
	return ValidateDeclaredBranch(branch, baseRef)
}

// ValidateDeclaredBranch applies operator-input policy in addition to the
// shared transport grammar. An empty baseRef omits only the base comparison.
func ValidateDeclaredBranch(branch, baseRef string) error {
	if !ValidBranchName(branch) || strings.HasPrefix(branch, "refs/") ||
		strings.HasPrefix(branch, "freeside/") || branch == baseRef {
		return fmt.Errorf("branch %q is invalid, reserved, or equals the base ref", branch)
	}
	return nil
}

// ValidBranchName is the transport's bounded git refname grammar. Component
// checks also reject names such as release/.candidate and release/a.lock.
func ValidBranchName(name string) bool {
	if name == "" || len(name) > 255 {
		return false
	}
	if strings.HasPrefix(name, "-") || strings.Contains(name, "@{") || strings.HasSuffix(name, ".") {
		return false
	}
	for _, c := range name {
		if c <= ' ' || c == 0x7f {
			return false
		}
		switch c {
		case ':', '?', '*', '[', '\\', '~', '^':
			return false
		}
	}
	for _, component := range strings.Split(name, "/") {
		if component == "" || strings.HasPrefix(component, ".") ||
			strings.HasSuffix(component, ".") || strings.HasSuffix(component, ".lock") ||
			strings.Contains(component, "..") {
			return false
		}
	}
	return true
}

func decode(payload []byte, value any) error {
	if err := strictjson.Decode(payload, value, strictjson.TolerateInvalidUTF8, strictjson.NoLimit); err != nil {
		if errors.Is(err, strictjson.ErrTrailingData) {
			return errors.New("trailing data")
		}
		return err
	}
	return nil
}
