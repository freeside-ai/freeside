package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/credentialpattern"
)

// WardCheck names the ward handoff check that refused a run's workspace or
// export. It mirrors ward.Check member for member, because domain cannot
// import ward; a ward test keeps the two lists equal. It is unrelated to
// HandoffCheck, the publication's handoff proof.
type WardCheck string

const (
	WardCheckCredentialSeparation   WardCheck = "credential_separation" //nolint:gosec // G101 false positive: a check name, not a credential.
	WardCheckControlPlaneIsolation  WardCheck = "control_plane_isolation"
	WardCheckWriterTermination      WardCheck = "writer_termination"
	WardCheckExporterAllowlist      WardCheck = "exporter_allowlist"
	WardCheckInExporterVerification WardCheck = "in_exporter_verification"
	WardCheckExportVerification     WardCheck = "export_verification"
	WardCheckTeardown               WardCheck = "teardown"
	WardCheckWriterVolumeExclusion  WardCheck = "writer_volume_exclusion"
	WardCheckCredentialContainment  WardCheck = "credential_containment" //nolint:gosec // G101 false positive: a check name, not a credential.
	WardCheckSameVMRefutation       WardCheck = "same_vm_refutation"
	WardCheckPreJobProbe            WardCheck = "pre_job_probe"
	WardCheckNetworklessExport      WardCheck = "networkless_export"
	WardCheckAgentEgress            WardCheck = "agent_egress"
	WardCheckWorkspaceSeeding       WardCheck = "workspace_seeding"
	WardCheckObservedBaseIdentity   WardCheck = "observed_base_identity"
	WardCheckAuthStoreMutationLease WardCheck = "auth_store_mutation_lease"
	WardCheckRecovery               WardCheck = "recovery"
)

// AllWardChecks lists every valid WardCheck; it is the single
// registration point.
var AllWardChecks = []WardCheck{
	WardCheckCredentialSeparation,
	WardCheckControlPlaneIsolation,
	WardCheckWriterTermination,
	WardCheckExporterAllowlist,
	WardCheckInExporterVerification,
	WardCheckExportVerification,
	WardCheckTeardown,
	WardCheckWriterVolumeExclusion,
	WardCheckCredentialContainment,
	WardCheckSameVMRefutation,
	WardCheckPreJobProbe,
	WardCheckNetworklessExport,
	WardCheckAgentEgress,
	WardCheckWorkspaceSeeding,
	WardCheckObservedBaseIdentity,
	WardCheckAuthStoreMutationLease,
	WardCheckRecovery,
}

func (c WardCheck) valid() bool {
	switch c {
	case WardCheckCredentialSeparation, WardCheckControlPlaneIsolation,
		WardCheckWriterTermination, WardCheckExporterAllowlist,
		WardCheckInExporterVerification, WardCheckExportVerification, WardCheckTeardown,
		WardCheckWriterVolumeExclusion, WardCheckCredentialContainment,
		WardCheckSameVMRefutation, WardCheckPreJobProbe, WardCheckNetworklessExport,
		WardCheckAgentEgress, WardCheckWorkspaceSeeding, WardCheckObservedBaseIdentity,
		WardCheckAuthStoreMutationLease, WardCheckRecovery:
		return true
	default:
		return false
	}
}

// MaxHandoffEvidenceBytes bounds a handoff check's evidence. The reasons ward
// writes are one line; the cap keeps an unexpectedly long one (a path list,
// a wrapped error) from growing a card or a task record.
const MaxHandoffEvidenceBytes = 512

// HandoffCheckFailure is the typed record of the ward handoff check that
// refused a run: which check, its screened evidence, and whether the refusal
// derives from the content itself (ward's content-evidence mark), the one
// class a retry would refuse identically.
//
// Evidence is ward's ConformanceFailure reason after ScreenFailureEvidence: it
// never carries the failure's underlying error text, and any credential-rule
// match is replaced by a fixed token. Validate refuses evidence that still
// matches a credential rule, so a record that reached the store is screened
// whoever wrote it.
type HandoffCheckFailure struct {
	Check           WardCheck `json:"check"`
	Evidence        string    `json:"evidence"`
	ContentEvidence bool      `json:"content_evidence"`
}

func (f HandoffCheckFailure) Validate() error {
	if !f.Check.valid() {
		return fmt.Errorf("handoff check %q: %w", f.Check, ErrOutcomeInconsistent)
	}
	if f.Evidence == "" || len(f.Evidence) > MaxHandoffEvidenceBytes || !utf8.ValidString(f.Evidence) {
		return fmt.Errorf("handoff check %q evidence of %d bytes: %w", f.Check, len(f.Evidence), ErrOutcomeInconsistent)
	}
	if matchesCredentialRule(f.Evidence) {
		return fmt.Errorf("handoff check %q evidence is unscreened: %w", f.Check, ErrOutcomeInconsistent)
	}
	return nil
}

// Equal reports whether two optional records are the same, nil included.
func (f *HandoffCheckFailure) Equal(other *HandoffCheckFailure) bool {
	if f == nil || other == nil {
		return f == other
	}
	return *f == *other
}

// redactedEvidence replaces a credential-rule match or a withheld error text
// in failure evidence. It matches no rule itself.
const redactedEvidence = "[redacted]"

// evidenceElision joins the head and tail of evidence cut to the cap.
const evidenceElision = " … "

// ScreenFailureEvidence makes a ConformanceFailure reason safe to show: it
// removes every occurrence of each withheld text (the failure's underlying
// error, which ward's failw callers also format into the reason and which may
// name host paths or quote content), replaces every credential-rule match,
// normalizes to valid UTF-8, and bounds the result to MaxHandoffEvidenceBytes
// by keeping its head and tail. Screening runs again after the cut, so a cut
// can't turn a non-matching run of text into a match. An empty result reads
// as the redaction token, so the record always says something.
func ScreenFailureEvidence(reason string, withheld ...string) string {
	s := strings.ToValidUTF8(reason, "�")
	for _, text := range withheld {
		if text != "" {
			s = strings.ReplaceAll(s, strings.ToValidUTF8(text, "�"), redactedEvidence)
		}
	}
	s = redactCredentials(s)
	if len(s) > MaxHandoffEvidenceBytes {
		s = redactCredentials(cutHeadAndTail(s, MaxHandoffEvidenceBytes))
	}
	// A token longer than a short match it replaced can push the result
	// back over the cap, and one rule's token beside text can complete
	// another rule. Validate refuses either, which would hold the outcome
	// write, so the evidence falls back to the bare token.
	if strings.TrimSpace(s) == "" || len(s) > MaxHandoffEvidenceBytes || matchesCredentialRule(s) {
		return redactedEvidence
	}
	return s
}

// cutHeadAndTail bounds s to limit bytes, keeping both ends joined by the
// elision marker; both cuts fall on rune boundaries. A wrapped reason carries
// its specific cause at the end, so a head-only cut would drop it.
func cutHeadAndTail(s string, limit int) string {
	budget := limit - len(evidenceElision)
	head := budget / 2
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	tail := len(s) - (budget - head)
	for tail < len(s) && !utf8.RuneStart(s[tail]) {
		tail++
	}
	return s[:head] + evidenceElision + s[tail:]
}

func redactCredentials(s string) string {
	for _, rule := range credentialpattern.All() {
		s = rule.Pattern.ReplaceAllLiteralString(s, redactedEvidence)
	}
	return s
}

func matchesCredentialRule(s string) bool {
	for _, rule := range credentialpattern.All() {
		if rule.Pattern.MatchString(s) {
			return true
		}
	}
	return false
}

// RedactedName is the stable token that stands in for a name that must not
// be shown: a short digest of it, which an operator holding the name can
// recompute. Ward's export verification uses the same token for paths it
// withholds from a reason.
func RedactedName(name string) string {
	sum := sha256.Sum256([]byte(name))
	return "path:" + hex.EncodeToString(sum[:])[:16]
}

// ScreenedName shows an agent-chosen name (a workspace path, an evidence
// label). A name any credential rule matches could itself be the secret, so it
// shows only as its RedactedName token; any other name shows as it is.
func ScreenedName(name string) string {
	if matchesCredentialRule(name) {
		return RedactedName(name)
	}
	return name
}
