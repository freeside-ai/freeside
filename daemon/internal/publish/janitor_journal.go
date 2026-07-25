package publish

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"
	"unicode/utf8"
)

// installationJanitorJournalVersion is the only journal version this daemon
// interprets. The journal is daemon-owned, but it is still read back across a
// process boundary, so an unrecognized version fails closed like any other
// reconstructed value.
const installationJanitorJournalVersion = 1

// janitorAction records which recorder method committed an audit entry. It is
// written by the caller rather than derived from the reason, so the durable
// quarantine set never depends on the janitor's internal mapping from removal
// reason to destructive action.
type janitorAction string

const (
	janitorActionRemoval    janitorAction = "removal"
	janitorActionQuarantine janitorAction = "quarantine"
)

// allJanitorActions is the single registration point for the vocabulary. It is
// unexported because the type is: an exported slice of a package-private type
// could not be used by a caller outside this package. valid derives from it
// rather than restating it as a switch, so a member added here cannot drift
// from the predicate at all.
var allJanitorActions = []janitorAction{
	janitorActionRemoval,
	janitorActionQuarantine,
}

func (a janitorAction) valid() bool {
	return slices.Contains(allJanitorActions, a)
}

// janitorJournal is the daemon-owned companion to the operator's authority
// snapshot. It holds two things that must commit together: the audit entries
// the janitor writes before every destructive request, and the quarantine set
// that keeps a quarantined installation from being re-trusted after a restart.
//
// The quarantine set is stored rather than derived from the entries. It is
// bounded by the number of installations ever quarantined, while the audit log
// only grows, so an operator who rotates the log for size must be able to do so
// without silently restoring trust.
type janitorJournal struct {
	Version     int                       `json:"version"`
	Quarantined []quarantinedInstallation `json:"quarantined"`
	Entries     []janitorAuditEntry       `json:"entries"`
}

// quarantinedInstallation is one installation whose trust this daemon has
// durably withdrawn. It carries no account login: the janitor records the
// numeric account only, since the returned login is untrusted text.
type quarantinedInstallation struct {
	RegistrationID int64     `json:"registration_id"`
	InstallationID int64     `json:"installation_id"`
	RecordedAt     time.Time `json:"recorded_at"`
}

// janitorAuditEntry is the durable pre-effect record of one removal request.
type janitorAuditEntry struct {
	Action                janitorAction             `json:"action"`
	RequestedAt           time.Time                 `json:"requested_at"`
	RegistrationID        int64                     `json:"registration_id"`
	InstallationID        int64                     `json:"installation_id"`
	AccountID             int64                     `json:"account_id"`
	Reason                InstallationRemovalReason `json:"reason"`
	ObservedRepositoryIDs []int64                   `json:"observed_repository_ids"`
}

func (j janitorJournal) validate() error {
	if j.Version != installationJanitorJournalVersion {
		return fmt.Errorf(
			"installation janitor journal: version %d is not %d",
			j.Version, installationJanitorJournalVersion,
		)
	}
	seen := make(map[quarantineKey]struct{}, len(j.Quarantined))
	for _, quarantined := range j.Quarantined {
		if quarantined.RegistrationID <= 0 || quarantined.InstallationID <= 0 {
			return errors.New("installation janitor journal: quarantine entry has a non-positive id")
		}
		if quarantined.RecordedAt.IsZero() {
			return errors.New("installation janitor journal: quarantine entry has no timestamp")
		}
		key := quarantineKey{
			registrationID: quarantined.RegistrationID,
			installationID: quarantined.InstallationID,
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf(
				"installation janitor journal: installation %d is quarantined twice",
				quarantined.InstallationID,
			)
		}
		seen[key] = struct{}{}
	}
	for _, entry := range j.Entries {
		if err := entry.validate(); err != nil {
			return err
		}
		// A quarantine entry whose set member is missing means the withdrawal
		// was rolled back while its evidence survived. Trust cannot be restored
		// by deletion, so the journal fails closed instead of serving the
		// binding again.
		if entry.Action != janitorActionQuarantine {
			continue
		}
		key := quarantineKey{
			registrationID: entry.RegistrationID,
			installationID: entry.InstallationID,
		}
		if _, ok := seen[key]; !ok {
			return fmt.Errorf(
				"installation janitor journal: installation %d has a quarantine record but no quarantine entry",
				entry.InstallationID,
			)
		}
	}
	return nil
}

func (e janitorAuditEntry) validate() error {
	if !e.Action.valid() {
		return fmt.Errorf("installation janitor journal: unknown action %q", e.Action)
	}
	if !e.Reason.valid() {
		return fmt.Errorf("installation janitor journal: unknown removal reason %q", e.Reason)
	}
	if e.RegistrationID <= 0 || e.InstallationID <= 0 || e.AccountID <= 0 {
		return errors.New("installation janitor journal: audit entry has a non-positive id")
	}
	if e.RequestedAt.IsZero() {
		return errors.New("installation janitor journal: audit entry has no timestamp")
	}
	if len(e.ObservedRepositoryIDs) == 0 {
		return nil
	}
	if err := validateAuthoredRepositoryIDs(e.ObservedRepositoryIDs, false); err != nil {
		return fmt.Errorf("installation janitor journal: audit entry: %w", err)
	}
	return nil
}

type quarantineKey struct {
	registrationID int64
	installationID int64
}

func (j janitorJournal) encode() ([]byte, error) {
	if err := j.validate(); err != nil {
		return nil, err
	}
	payload, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("installation janitor journal: encode: %w", err)
	}
	return append(payload, '\n'), nil
}

func decodeJanitorJournal(payload []byte) (janitorJournal, error) {
	if !utf8.Valid(payload) {
		return janitorJournal{}, errors.New("installation janitor journal: decode: payload is not valid UTF-8")
	}
	if err := rejectDuplicateJSONKeys(payload); err != nil {
		return janitorJournal{}, fmt.Errorf("installation janitor journal: decode: %w", err)
	}
	if err := requireJournalKeys(payload); err != nil {
		return janitorJournal{}, fmt.Errorf("installation janitor journal: decode: %w", err)
	}
	var journal janitorJournal
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&journal); err != nil {
		return janitorJournal{}, fmt.Errorf("installation janitor journal: decode: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return janitorJournal{}, errors.New("installation janitor journal: decode: trailing data after the journal")
	}
	if err := journal.validate(); err != nil {
		return janitorJournal{}, err
	}
	return journal, nil
}

// record appends one audit entry, adding the installation to the quarantine set
// when the action withdraws trust. Re-quarantining an installation is
// idempotent in the set and still appends its own audit entry: each destructive
// request is its own event.
func (j janitorJournal) record(entry janitorAuditEntry) janitorJournal {
	updated := janitorJournal{
		Version:     installationJanitorJournalVersion,
		Quarantined: slices.Clone(j.Quarantined),
		Entries:     append(slices.Clone(j.Entries), entry),
	}
	if entry.Action != janitorActionQuarantine {
		return updated
	}
	for _, quarantined := range updated.Quarantined {
		if quarantined.RegistrationID == entry.RegistrationID &&
			quarantined.InstallationID == entry.InstallationID {
			return updated
		}
	}
	updated.Quarantined = append(updated.Quarantined, quarantinedInstallation{
		RegistrationID: entry.RegistrationID,
		InstallationID: entry.InstallationID,
		RecordedAt:     entry.RequestedAt,
	})
	return updated
}

// applyQuarantine withdraws every binding and pending envelope the journal has
// quarantined for the entry's registration.
//
// The refusal in the middle is the anti-laundering rule. Subtraction only
// removes bindings, and several of the snapshot's cross-binding invariants can
// be *satisfied* by removal: a repository or account bound twice, or a pending
// envelope that shadows a known installation, would become well-formed once the
// conflicting binding disappears. Rather than decide which of those a stale
// binding plus a live exception meant, a registration that still binds a
// quarantined installation while carrying an unrelated pending envelope fails
// the pass until the operator reconciles the file.
//
// A withdrawal reaches the pending envelope by installation ID, which leaves one
// case open (#283). Once the operator has removed the stale binding, a *later*
// envelope authored with no installation ID matches by account, so a quarantined
// installation that outlived its deletion can satisfy it and escape cleanup.
// Closing it needs the withdrawn set to reach the reconciliation gate, which is
// #263's pending-envelope semantics rather than this store's. What it cannot do
// is regain credentials: a zero-ID envelope carries an empty current set, and
// only that set enters janitor coverage, so the escape grants no repository and
// ends when the envelope expires.
func applyQuarantine(
	entry InstallationAuthorityEntry,
	quarantined []quarantinedInstallation,
) (InstallationAuthorityEntry, error) {
	withdrawn := make(map[int64]struct{}, len(quarantined))
	for _, record := range quarantined {
		if record.RegistrationID == entry.RegistrationID {
			withdrawn[record.InstallationID] = struct{}{}
		}
	}
	if len(withdrawn) == 0 {
		return entry, nil
	}

	if entry.Pending != nil && entry.Pending.installationID() > 0 {
		if _, ok := withdrawn[entry.Pending.installationID()]; ok {
			entry.Pending = nil
		}
	}

	bindings := make([]TrustedInstallationRecord, 0, len(entry.TrustedInstallations))
	dropped := false
	for _, binding := range entry.TrustedInstallations {
		if _, ok := withdrawn[binding.InstallationID]; ok {
			dropped = true
			continue
		}
		bindings = append(bindings, binding)
	}
	if !dropped {
		return entry, nil
	}
	if entry.Pending != nil {
		return InstallationAuthorityEntry{}, fmt.Errorf(
			"installation authority: registration %d still binds a quarantined installation "+
				"while carrying a pending envelope: %w",
			entry.RegistrationID, ErrInstallationAuthoritySnapshot,
		)
	}
	entry.TrustedInstallations = bindings

	// A cheap assertion, not the guarantee: the refusal above is what keeps a
	// removal from widening what the janitor accepts. This only pins that the
	// served entry still satisfies the rules the authored one did, so a later
	// change to either cannot quietly diverge.
	if err := entry.validate(); err != nil {
		return InstallationAuthorityEntry{}, err
	}
	return entry, nil
}

// requireJournalKeys holds the journal to the same rule as the operator's
// document: an omitted key must not read as an authored empty value. It matters
// most for the quarantine set, where absent would mean "nothing was ever
// withdrawn", so a hand-edit that drops the key while rotating the audit log
// would restore trust in every installation this daemon destroyed.
func requireJournalKeys(payload []byte) error {
	journal, err := jsonObject(payload)
	if err != nil || journal == nil {
		return nil
	}
	if err := requireKeys(journal, "journal", "version", "quarantined", "entries"); err != nil {
		return err
	}
	if err := requireElementKeys(journal["quarantined"], "quarantine record",
		"registration_id", "installation_id", "recorded_at"); err != nil {
		return err
	}
	return requireElementKeys(journal["entries"], "audit entry", "action", "requested_at",
		"registration_id", "installation_id", "account_id", "reason", "observed_repository_ids")
}
