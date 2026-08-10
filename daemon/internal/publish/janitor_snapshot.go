package publish

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// installationAuthoritySnapshotVersion is the only document version this
// daemon interprets. A future version changes what a field means, so an
// unrecognized one must fail closed rather than be read under today's
// meaning: DisallowUnknownFields alone would accept a version that only
// reinterprets fields this version already has.
const installationAuthoritySnapshotVersion = 1

// InstallationAuthorityDocument is the operator-authored canonical snapshot
// backing InstallationAuthoritySource on a standalone host. One document
// describes every registration the host's keystore holds; the janitor reads
// exactly one entry per registration and treats a missing entry as an error
// rather than as an empty authority (see InstallationAuthorityStore).
//
// The wire types are deliberately separate from the port types
// (InstallationAuthority and friends), which carry no persistence concerns.
// The mapping is one-way and total: every field the janitor validates is
// either authored here or derived from the entry that contains it.
type InstallationAuthorityDocument struct {
	Version       int                          `json:"version"`
	Registrations []InstallationAuthorityEntry `json:"registrations"`
}

// InstallationAuthorityEntry is one registration's complete authority.
// RegistrationID is carried once, by the entry, and copied into each binding
// and the pending envelope on load: the janitor rejects a binding whose
// registration differs from the App it is reconciling, and a per-binding
// field could disagree with the entry that contains it.
//
// An entry with no trusted installations is the most destructive declaration
// this format can express: the janitor deletes every installation it observes
// for a registration it has no binding for. Authoring one is a deliberate act,
// not a default.
type InstallationAuthorityEntry struct {
	RegistrationID        int64                       `json:"registration_id"`
	ActiveEpoch           int64                       `json:"active_epoch"`
	DurableIntentRevision int64                       `json:"durable_intent_revision"`
	TrustedOwners         []TrustedOwnerRecord        `json:"trusted_owners"`
	TrustedInstallations  []TrustedInstallationRecord `json:"trusted_installations"`
	Pending               *PendingEnvelopeRecord      `json:"pending"`
}

// TrustedOwnerRecord is one owner account approved for a public registration.
type TrustedOwnerRecord struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
}

// TrustedInstallationRecord binds one installation to the exact repository IDs
// it may serve. RepositoryIDs must be authored in canonical ascending order, so
// a reader and the author agree on the set without normalizing it first.
type TrustedInstallationRecord struct {
	InstallationID int64   `json:"installation_id"`
	Account        string  `json:"account"`
	AccountID      int64   `json:"account_id"`
	RepositoryIDs  []int64 `json:"repository_ids"`
}

// PendingEnvelopeRecord is the bounded reconciliation exception. It receives
// that exception only while its epoch and revision equal the entry's frontier
// and it has not expired; otherwise it is structurally valid history.
//
// InstallationID is a pointer because zero is its widest value, not its
// narrowest: an envelope without an installation matches any installation on
// the expected account, which is what a native install needs before GitHub
// assigns an ID. An omitted field must therefore be an error rather than that
// value, which is the one place in this format where absence would widen
// authority instead of narrowing it.
type PendingEnvelopeRecord struct {
	ActiveEpoch            int64     `json:"active_epoch"`
	DurableIntentRevision  int64     `json:"durable_intent_revision"`
	ExpectedAccount        string    `json:"expected_account"`
	ExpectedAccountID      int64     `json:"expected_account_id"`
	InstallationID         *int64    `json:"installation_id"`
	CurrentRepositoryIDs   []int64   `json:"current_repository_ids"`
	ExpectedRepositoryIDs  []int64   `json:"expected_repository_ids"`
	RequiredRepositoryMode string    `json:"required_repository_mode"`
	ExpiresAt              time.Time `json:"expires_at"`
}

// installationID reports the envelope's authored installation. Callers must
// have validated the entry first; validatePending rejects a nil pointer.
func (p PendingEnvelopeRecord) installationID() int64 {
	if p.InstallationID == nil {
		return 0
	}
	return *p.InstallationID
}

// Validate reports whether the document is well-formed under every rule that
// does not depend on the registration's own credentials.
//
// It is deliberately a pre-gate, not a shortcut around
// validateInstallationAuthority, which remains the authority and re-runs on
// every value this package serves. Restating the janitor's cross-binding rules
// here is load-bearing: the store subtracts quarantined bindings between decode
// and service, and subtraction only ever *removes* bindings, so a rule stated
// across bindings (a repository or account bound twice, a pending envelope
// shadowing a known installation) can be satisfied by removal. Validating the
// document as authored closes that.
//
// The guarantee is bounded by what this type can see, which is not the
// registration's credentials. Whether a bound account is trusted, and whether a
// public registration's own owner appears in its trusted set, are decided only
// on the served value, so subtraction can turn a document those rules would
// have rejected into one they accept. It cannot widen what the served authority
// grants, since removal only ever grants less; what it changes is that the pass
// proceeds instead of freezing, against a file the operator has not reconciled.
func (d InstallationAuthorityDocument) Validate() error {
	if d.Version != installationAuthoritySnapshotVersion {
		return fmt.Errorf(
			"installation authority: document version %d is not %d: %w",
			d.Version, installationAuthoritySnapshotVersion, ErrInstallationAuthoritySnapshot,
		)
	}
	seen := make(map[int64]struct{}, len(d.Registrations))
	for _, entry := range d.Registrations {
		if _, duplicate := seen[entry.RegistrationID]; duplicate {
			return fmt.Errorf(
				"installation authority: registration %d appears twice: %w",
				entry.RegistrationID, ErrInstallationAuthoritySnapshot,
			)
		}
		seen[entry.RegistrationID] = struct{}{}
		if err := entry.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (e InstallationAuthorityEntry) validate() error {
	if e.RegistrationID <= 0 {
		return newSnapshotError("registration id %d is not positive", e.RegistrationID)
	}
	// The frontier is always meaningful: a zero epoch or revision would read as
	// "unset" while silently deciding whether a pending envelope is current.
	if e.ActiveEpoch <= 0 || e.DurableIntentRevision <= 0 {
		return newSnapshotError(
			"registration %d has a non-positive epoch or durable intent revision",
			e.RegistrationID,
		)
	}
	if err := e.validateOwners(); err != nil {
		return err
	}
	if err := e.validateInstallations(); err != nil {
		return err
	}
	return e.validatePending()
}

func (e InstallationAuthorityEntry) validateOwners() error {
	seen := make(map[int64]struct{}, len(e.TrustedOwners))
	for _, owner := range e.TrustedOwners {
		if owner.ID <= 0 {
			return newSnapshotError(
				"registration %d has a trusted owner with a non-positive id", e.RegistrationID,
			)
		}
		if err := validateOwnerLogin(owner.Login); err != nil {
			return newSnapshotError(
				"registration %d has an invalid trusted owner login: %v", e.RegistrationID, err,
			)
		}
		if _, duplicate := seen[owner.ID]; duplicate {
			return newSnapshotError(
				"registration %d lists trusted owner %d twice", e.RegistrationID, owner.ID,
			)
		}
		seen[owner.ID] = struct{}{}
	}
	return nil
}

func (e InstallationAuthorityEntry) validateInstallations() error {
	// Absent is not empty. An entry with no bindings tells the janitor to delete
	// every installation the registration has, so it must be authored as an
	// explicit empty array; a forgotten or null key would make the most
	// destructive declaration in the format the easiest one to write by mistake.
	if e.TrustedInstallations == nil {
		return newSnapshotError(
			"registration %d omits trusted_installations; author an empty array to trust none",
			e.RegistrationID,
		)
	}
	installations := make(map[int64]struct{}, len(e.TrustedInstallations))
	accounts := make(map[trustedOwnerKey]int64, len(e.TrustedInstallations))
	repositories := make(map[int64]int64)
	for _, binding := range e.TrustedInstallations {
		if binding.InstallationID <= 0 || binding.AccountID <= 0 {
			return newSnapshotError(
				"registration %d has a binding with a non-positive installation or account id",
				e.RegistrationID,
			)
		}
		if err := validateOwnerLogin(binding.Account); err != nil {
			return newSnapshotError(
				"registration %d has an invalid binding account: %v", e.RegistrationID, err,
			)
		}
		if _, duplicate := installations[binding.InstallationID]; duplicate {
			return newSnapshotError(
				"registration %d binds installation %d twice",
				e.RegistrationID, binding.InstallationID,
			)
		}
		installations[binding.InstallationID] = struct{}{}
		account := trustedOwnerKey{login: strings.ToLower(binding.Account), id: binding.AccountID}
		if prior, duplicate := accounts[account]; duplicate && prior != binding.InstallationID {
			return newSnapshotError(
				"registration %d binds account %d to installations %d and %d",
				e.RegistrationID, binding.AccountID, prior, binding.InstallationID,
			)
		}
		accounts[account] = binding.InstallationID
		if err := validateAuthoredRepositoryIDs(binding.RepositoryIDs, false); err != nil {
			return newSnapshotError(
				"registration %d installation %d: %v",
				e.RegistrationID, binding.InstallationID, err,
			)
		}
		for _, repositoryID := range binding.RepositoryIDs {
			if prior, duplicate := repositories[repositoryID]; duplicate && prior != binding.InstallationID {
				return newSnapshotError(
					"registration %d binds repository %d to installations %d and %d",
					e.RegistrationID, repositoryID, prior, binding.InstallationID,
				)
			}
			repositories[repositoryID] = binding.InstallationID
		}
	}
	return nil
}

func (e InstallationAuthorityEntry) validatePending() error {
	if e.Pending == nil {
		return nil
	}
	pending := *e.Pending
	if pending.InstallationID == nil {
		return newSnapshotError(
			"registration %d pending envelope omits installation_id; author 0 for an install "+
				"GitHub has not numbered yet", e.RegistrationID,
		)
	}
	if pending.ActiveEpoch <= 0 || pending.DurableIntentRevision <= 0 ||
		pending.ExpectedAccountID <= 0 || pending.installationID() < 0 ||
		pending.RequiredRepositoryMode != "selected" || pending.ExpiresAt.IsZero() {
		return newSnapshotError("registration %d has an invalid pending envelope", e.RegistrationID)
	}
	if err := validateOwnerLogin(pending.ExpectedAccount); err != nil {
		return newSnapshotError(
			"registration %d has an invalid pending expected account: %v", e.RegistrationID, err,
		)
	}
	if err := validateAuthoredRepositoryIDs(pending.CurrentRepositoryIDs, true); err != nil {
		return newSnapshotError(
			"registration %d pending current repository IDs: %v", e.RegistrationID, err,
		)
	}
	if err := validateAuthoredRepositoryIDs(pending.ExpectedRepositoryIDs, false); err != nil {
		return newSnapshotError(
			"registration %d pending expected repository IDs: %v", e.RegistrationID, err,
		)
	}
	if !isRepositorySubset(pending.CurrentRepositoryIDs, pending.ExpectedRepositoryIDs) {
		return newSnapshotError(
			"registration %d pending current repository IDs are not a subset of the expected set",
			e.RegistrationID,
		)
	}
	return e.validatePendingAgainstBindings(pending)
}

// validatePendingAgainstBindings mirrors the cross-checks
// validateInstallationAuthority runs between the envelope and the trusted
// bindings. They are restated here because the store may remove a binding
// before the janitor sees the snapshot, and every one of these rules is
// satisfiable by removal.
func (e InstallationAuthorityEntry) validatePendingAgainstBindings(pending PendingEnvelopeRecord) error {
	if pending.installationID() == 0 && len(pending.CurrentRepositoryIDs) != 0 {
		return newSnapshotError(
			"registration %d pending envelope without an installation carries a current repository set",
			e.RegistrationID,
		)
	}
	account := strings.ToLower(pending.ExpectedAccount)
	for _, binding := range e.TrustedInstallations {
		sameAccount := strings.ToLower(binding.Account) == account &&
			binding.AccountID == pending.ExpectedAccountID
		if !sameAccount {
			continue
		}
		if pending.installationID() == 0 {
			return newSnapshotError(
				"registration %d pending envelope omits the known installation identity",
				e.RegistrationID,
			)
		}
		if binding.InstallationID != pending.installationID() {
			return newSnapshotError(
				"registration %d pending envelope names a second installation for a bound account",
				e.RegistrationID,
			)
		}
	}
	if pending.installationID() <= 0 {
		return nil
	}
	for _, binding := range e.TrustedInstallations {
		if binding.InstallationID != pending.installationID() {
			continue
		}
		if strings.ToLower(binding.Account) != account ||
			binding.AccountID != pending.ExpectedAccountID ||
			!slices.Equal(binding.RepositoryIDs, pending.CurrentRepositoryIDs) {
			return newSnapshotError(
				"registration %d pending envelope does not bind the current trusted installation state",
				e.RegistrationID,
			)
		}
		return nil
	}
	if len(pending.CurrentRepositoryIDs) != 0 {
		return newSnapshotError(
			"registration %d pending envelope carries an unbound current repository set",
			e.RegistrationID,
		)
	}
	return nil
}

// Encode validates and serializes the document in its canonical indented form.
// Operators edit this file by hand, so the encoding is the one a generator
// (freesided onboard, #238) must produce to leave the file byte-stable.
func (d InstallationAuthorityDocument) Encode() ([]byte, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	payload, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("installation authority: encode: %w", err)
	}
	return append(payload, '\n'), nil
}

// DecodeInstallationAuthorityDocument deserializes and validates an
// operator-authored snapshot. Unknown fields, trailing data, and invalid UTF-8
// fail closed: the decoder would otherwise launder an invalid byte into
// U+FFFD, and a login this package cannot read exactly must not be compared
// against a GitHub account.
func DecodeInstallationAuthorityDocument(payload []byte) (InstallationAuthorityDocument, error) {
	// Preserve invalid UTF-8 as the first content error before the structural
	// key gates. strictjson independently rechecks the same posture at decode.
	if !utf8.Valid(payload) {
		return InstallationAuthorityDocument{}, fmt.Errorf(
			"installation authority: decode: payload is not valid UTF-8: %w",
			ErrInstallationAuthoritySnapshot,
		)
	}
	if err := rejectDuplicateJSONKeys(payload); err != nil {
		return InstallationAuthorityDocument{}, fmt.Errorf(
			"installation authority: decode: %w: %w", err, ErrInstallationAuthoritySnapshot,
		)
	}
	if err := requireAuthoredKeys(payload); err != nil {
		return InstallationAuthorityDocument{}, fmt.Errorf(
			"installation authority: decode: %w: %w", err, ErrInstallationAuthoritySnapshot,
		)
	}
	var document InstallationAuthorityDocument
	if err := strictjson.Decode(payload, &document, strictjson.RejectInvalidUTF8, strictjson.NoLimit); err != nil {
		if errors.Is(err, strictjson.ErrInvalidUTF8) {
			return InstallationAuthorityDocument{}, fmt.Errorf(
				"installation authority: decode: payload is not valid UTF-8: %w",
				ErrInstallationAuthoritySnapshot,
			)
		}
		if errors.Is(err, strictjson.ErrTrailingData) {
			return InstallationAuthorityDocument{}, fmt.Errorf(
				"installation authority: decode: trailing data after the document: %w",
				ErrInstallationAuthoritySnapshot,
			)
		}
		return InstallationAuthorityDocument{}, fmt.Errorf(
			"installation authority: decode: %w: %w", err, ErrInstallationAuthoritySnapshot,
		)
	}
	if err := document.Validate(); err != nil {
		return InstallationAuthorityDocument{}, err
	}
	return document, nil
}

// entry returns the registration's authored entry. A registration the document
// does not name is an error, never an empty entry: an empty authority instructs
// the janitor to delete every installation it observes, so silence must fail
// the pass instead of authorizing removals.
func (d InstallationAuthorityDocument) entry(registrationID int64) (InstallationAuthorityEntry, error) {
	for _, entry := range d.Registrations {
		if entry.RegistrationID == registrationID {
			return entry, nil
		}
	}
	return InstallationAuthorityEntry{}, fmt.Errorf(
		"installation authority: registration %d has no authority entry: %w",
		registrationID, ErrInstallationAuthoritySnapshot,
	)
}

// authority maps the entry onto the port type, copying the entry's
// registration ID into every binding and the pending envelope.
func (e InstallationAuthorityEntry) authority() InstallationAuthority {
	authority := InstallationAuthority{
		ActiveEpoch:           e.ActiveEpoch,
		DurableIntentRevision: e.DurableIntentRevision,
	}
	for _, owner := range e.TrustedOwners {
		// The conversion is deliberate: adding a field to one of these two
		// shapes must fail to compile rather than silently drop on the way out.
		authority.TrustedOwners = append(authority.TrustedOwners, TrustedOwner(owner))
	}
	for _, binding := range e.TrustedInstallations {
		authority.TrustedInstallations = append(authority.TrustedInstallations, TrustedInstallation{
			RegistrationID: e.RegistrationID,
			InstallationID: binding.InstallationID,
			Account:        binding.Account,
			AccountID:      binding.AccountID,
			RepositoryIDs:  slices.Clone(binding.RepositoryIDs),
		})
	}
	if e.Pending != nil {
		authority.Pending = &PendingInstallationEnvelope{
			ActiveEpoch:            e.Pending.ActiveEpoch,
			DurableIntentRevision:  e.Pending.DurableIntentRevision,
			RegistrationID:         e.RegistrationID,
			ExpectedAccount:        e.Pending.ExpectedAccount,
			ExpectedAccountID:      e.Pending.ExpectedAccountID,
			InstallationID:         e.Pending.installationID(),
			CurrentRepositoryIDs:   slices.Clone(e.Pending.CurrentRepositoryIDs),
			ExpectedRepositoryIDs:  slices.Clone(e.Pending.ExpectedRepositoryIDs),
			RequiredRepositoryMode: e.Pending.RequiredRepositoryMode,
			ExpiresAt:              e.Pending.ExpiresAt,
		}
	}
	return authority
}

// validateAuthoredRepositoryIDs requires the authored order to already be the
// canonical one. canonicalRepositoryIDs sorts a copy, so an unsorted authored
// set would otherwise validate and re-encode differently than it was written.
func validateAuthoredRepositoryIDs(ids []int64, emptyAllowed bool) error {
	canonical, err := canonicalRepositoryIDs(ids, emptyAllowed)
	if err != nil {
		return err
	}
	if !slices.Equal(ids, canonical) {
		return errors.New("repository IDs are not in canonical ascending order")
	}
	return nil
}

func newSnapshotError(format string, args ...any) error {
	return fmt.Errorf(
		"installation authority: %s: %w",
		fmt.Sprintf(format, args...),
		ErrInstallationAuthoritySnapshot,
	)
}

// rejectDuplicateJSONKeys refuses an object that names the same key twice.
// RFC 8259 leaves the meaning implementation-defined, and encoding/json resolves
// it silently: the last scalar or array wins, and nested objects are merged. A
// hand-edited file that repeats a key would therefore decode to something no
// stanza in it authored, and the value that survives is routinely the narrower
// one, which on this path means deletions. Neither DisallowUnknownFields nor
// field validation can see it, since the collapse happens first.
func rejectDuplicateJSONKeys(payload []byte) error {
	type frame struct {
		object    bool
		expectKey bool
		keys      map[string]struct{}
	}
	var stack []*frame
	valueComplete := func() {
		if depth := len(stack); depth > 0 && stack[depth-1].object {
			stack[depth-1].expectKey = true
		}
	}

	dec := json.NewDecoder(bytes.NewReader(payload))
	for {
		token, err := dec.Token()
		if err != nil {
			// Malformed input is the strict decoder's to report, with its own
			// message; this pass only answers the duplicate-key question.
			return nil
		}
		if depth := len(stack); depth > 0 && stack[depth-1].object && stack[depth-1].expectKey {
			if key, ok := token.(string); ok {
				if _, duplicate := stack[depth-1].keys[key]; duplicate {
					return fmt.Errorf("object names %q twice", key)
				}
				stack[depth-1].keys[key] = struct{}{}
				stack[depth-1].expectKey = false
				continue
			}
		}
		delim, ok := token.(json.Delim)
		if !ok {
			valueComplete()
			continue
		}
		switch delim {
		case '{':
			stack = append(stack, &frame{object: true, expectKey: true, keys: map[string]struct{}{}})
		case '[':
			stack = append(stack, &frame{})
		case '}', ']':
			stack = stack[:len(stack)-1]
			valueComplete()
		}
	}
}

// requireAuthoredKeys rejects a document that omits any field of the shapes it
// defines. Go decodes an absent key and an authored null into the same zero
// value, so for every field where the zero value is the *more permissive*
// reading, a typo would silently choose it. Most fields are safe by accident
// (an absent ID is zero and zero is rejected), but the exceptions are exactly
// the ones that drive deletions: an absent `pending` leaves a fresh native
// install unprotected, and an absent `trusted_installations` trusts nothing.
//
// Rather than enumerate which fields are dangerous and re-litigate that list
// every time the format grows, every key is required and the operator authors
// the whole shape, as the golden shows it. An authored `null` still says
// "deliberately absent"; only the missing key is refused.
func requireAuthoredKeys(payload []byte) error {
	document, err := jsonObject(payload)
	if err != nil || document == nil {
		// Malformed input is the strict decoder's to report, with its own
		// message; this pass only answers the presence question.
		return nil
	}
	if err := requireKeys(document, "document", "version", "registrations"); err != nil {
		return err
	}
	entries, err := jsonArray(document["registrations"])
	if err != nil {
		return nil
	}
	for _, raw := range entries {
		if err := requireEntryKeys(raw); err != nil {
			return err
		}
	}
	return nil
}

func requireEntryKeys(raw json.RawMessage) error {
	entry, err := jsonObject(raw)
	if err != nil || entry == nil {
		return nil
	}
	if err := requireKeys(entry, "registration", "registration_id", "active_epoch",
		"durable_intent_revision", "trusted_owners", "trusted_installations", "pending"); err != nil {
		return err
	}
	if err := requireElementKeys(entry["trusted_owners"], "trusted owner", "login", "id"); err != nil {
		return err
	}
	if err := requireElementKeys(entry["trusted_installations"], "trusted installation",
		"installation_id", "account", "account_id", "repository_ids"); err != nil {
		return err
	}
	pending, err := jsonObject(entry["pending"])
	if err != nil || pending == nil {
		return nil // an authored null is a deliberate absence
	}
	return requireKeys(pending, "pending envelope", "active_epoch", "durable_intent_revision",
		"expected_account", "expected_account_id", "installation_id", "current_repository_ids",
		"expected_repository_ids", "required_repository_mode", "expires_at")
}

func requireElementKeys(raw json.RawMessage, shape string, keys ...string) error {
	elements, err := jsonArray(raw)
	if err != nil {
		return nil
	}
	for _, element := range elements {
		object, err := jsonObject(element)
		if err != nil || object == nil {
			continue
		}
		if err := requireKeys(object, shape, keys...); err != nil {
			return err
		}
	}
	return nil
}

func requireKeys(object map[string]json.RawMessage, shape string, keys ...string) error {
	for _, key := range keys {
		if _, present := object[key]; !present {
			return fmt.Errorf("%s omits %q", shape, key)
		}
	}
	return nil
}

// jsonObject decodes one JSON object, reporting a nil map for a null or for
// anything that is not an object, so presence checks skip what they cannot read
// and leave the error to the strict decoder.
func jsonObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}
	return object, nil
}

func jsonArray(raw json.RawMessage) ([]json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var elements []json.RawMessage
	if err := json.Unmarshal(raw, &elements); err != nil {
		return nil, err
	}
	return elements, nil
}
