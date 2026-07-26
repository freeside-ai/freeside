package publish

import (
	"context"
	"errors"
	"fmt"
)

// CredentialFindingCode is the closed vocabulary for publish-credential
// doctor findings.
type CredentialFindingCode string

const (
	CredentialFindingBadPermissions     CredentialFindingCode = "bad_keystore_permissions" //nolint:gosec // diagnostic vocabulary, not a credential
	CredentialFindingMissingKey         CredentialFindingCode = "missing_machine_key"
	CredentialFindingVisibilityMismatch CredentialFindingCode = "visibility_mismatch"
	CredentialFindingMetadataMismatch   CredentialFindingCode = "registration_metadata_mismatch"
	CredentialFindingJanitorInactive    CredentialFindingCode = "janitor_inactive"
	CredentialFindingJanitorFailed      CredentialFindingCode = "janitor_registration_failed"
	CredentialFindingJanitorChurning    CredentialFindingCode = "janitor_removal_churn"
	CredentialFindingLegacyLayout       CredentialFindingCode = "legacy_singleton_layout"
	CredentialFindingReusedMachineKey   CredentialFindingCode = "reused_machine_key"
	CredentialFindingUnreadableRecord   CredentialFindingCode = "unreadable_registration" //nolint:gosec // diagnostic vocabulary, not a credential
	CredentialFindingUnexpectedEntry    CredentialFindingCode = "unexpected_keystore_entry"
)

// AllCredentialFindingCodes is the single registration point for doctor
// findings.
var AllCredentialFindingCodes = []CredentialFindingCode{
	CredentialFindingBadPermissions,
	CredentialFindingMissingKey,
	CredentialFindingVisibilityMismatch,
	CredentialFindingMetadataMismatch,
	CredentialFindingJanitorInactive,
	CredentialFindingJanitorFailed,
	CredentialFindingJanitorChurning,
	CredentialFindingLegacyLayout,
	CredentialFindingReusedMachineKey,
	CredentialFindingUnreadableRecord,
	CredentialFindingUnexpectedEntry,
}

func (c CredentialFindingCode) valid() bool {
	switch c {
	case CredentialFindingBadPermissions,
		CredentialFindingMissingKey,
		CredentialFindingVisibilityMismatch,
		CredentialFindingMetadataMismatch,
		CredentialFindingJanitorInactive,
		CredentialFindingJanitorFailed,
		CredentialFindingJanitorChurning,
		CredentialFindingLegacyLayout,
		CredentialFindingReusedMachineKey,
		CredentialFindingUnreadableRecord,
		CredentialFindingUnexpectedEntry:
		return true
	default:
		return false
	}
}

func credentialFinding(
	code CredentialFindingCode,
	registrationID, ownerID int64,
	keyID string,
) CredentialFinding {
	if !code.valid() {
		panic("publish: invalid credential finding code")
	}
	return CredentialFinding{
		Code: code, RegistrationID: registrationID, OwnerID: ownerID, KeyID: keyID,
	}
}

// CredentialFinding carries safe numeric coordinates. KeyID is populated only
// for detectable key reuse and is the public SHA-256 fingerprint GitHub shows.
type CredentialFinding struct {
	Code           CredentialFindingCode
	RegistrationID int64
	OwnerID        int64
	KeyID          string
}

// janitorFaultSource is the reporting half of the always-on janitor. The
// doctor discovers it on the JanitorStatus it already holds rather than taking
// it as a second dependency, so the fault explaining a shut gate always comes
// from the janitor that shut it. A JanitorStatus that does not implement it
// degrades to the generic inactive finding.
type janitorFaultSource interface {
	RegistrationFaults() []JanitorRegistrationFault
}

type janitorChurnSource interface {
	ChurningRegistrations() []JanitorRegistrationChurn
}

// CredentialDoctor checks the local keystore and canonical App metadata. It
// reports conformance findings but returns operational/API failures as errors,
// so an unreachable forge cannot look like a clean bill of health.
type CredentialDoctor struct {
	onboarder *CredentialOnboarder
	janitor   JanitorStatus
}

func NewCredentialDoctor(onboarder *CredentialOnboarder, janitor JanitorStatus) *CredentialDoctor {
	return &CredentialDoctor{onboarder: onboarder, janitor: janitor}
}

// Check verifies every local registration plus every expected registration.
// Expected registrations let a restored or new machine diagnose the absence of
// its per-machine key before that key exists in the local keystore.
func (d *CredentialDoctor) Check(
	ctx context.Context,
	expected []AppRegistration,
) ([]CredentialFinding, error) {
	if d == nil || d.onboarder == nil || d.onboarder.keystore == nil {
		return nil, errors.New("credential doctor: nil dependency")
	}
	for _, registration := range expected {
		if err := registration.validate(); err != nil {
			return nil, fmt.Errorf("credential doctor: %w", err)
		}
	}

	legacy, err := d.onboarder.keystore.hasLegacyLayout()
	if err != nil {
		if errors.Is(err, ErrCredentialPermissions) {
			return []CredentialFinding{credentialFinding(CredentialFindingBadPermissions, 0, 0, "")}, nil
		}
		return nil, fmt.Errorf("credential doctor: inspect legacy layout: %w", err)
	}
	if legacy {
		return []CredentialFinding{credentialFinding(CredentialFindingLegacyLayout, 0, 0, "")}, nil
	}

	// Enumeration skips an entry that could never be a registration rather
	// than denying every one of them (#284), so the doctor is where a skipped
	// entry surfaces. It is gathered before enumeration and carried through
	// the failure returns below: a damaged record must not hide the artifact
	// sitting next to it.
	var findings []CredentialFinding
	unexpected, err := d.onboarder.keystore.UnexpectedEntries()
	if err != nil {
		if errors.Is(err, ErrCredentialPermissions) {
			return []CredentialFinding{credentialFinding(CredentialFindingBadPermissions, 0, 0, "")}, nil
		}
		return nil, fmt.Errorf("credential doctor: inspect keystore entries: %w", err)
	}
	if len(unexpected) > 0 {
		findings = append(findings, credentialFinding(CredentialFindingUnexpectedEntry, 0, 0, ""))
	}

	apps, err := d.onboarder.keystore.ListApps()
	if err != nil {
		// Enumeration fails closed on the whole keystore, so whenever the
		// failure belongs to one record its owner ID rides every finding
		// code, not only the unreadable one: an operator told "missing key"
		// or "bad permissions" with no owner cannot tell which of several
		// registrations is blocking every resolver and janitor cycle. A
		// keystore-wide failure carries no owner and reports zero.
		var unreadable *UnreadableRegistrationError
		var ownerID int64
		if errors.As(err, &unreadable) {
			ownerID = unreadable.OwnerID
		}
		switch {
		case errors.Is(err, ErrCredentialPermissions):
			return append(findings, credentialFinding(CredentialFindingBadPermissions, 0, ownerID, "")), nil
		case errors.Is(err, ErrNoAppCredentials), errors.Is(err, ErrNoAppRegistration):
			return append(findings, credentialFinding(CredentialFindingMissingKey, 0, ownerID, "")), nil
		case errors.Is(err, ErrUnreadableRegistration):
			return append(findings, credentialFinding(CredentialFindingUnreadableRecord, 0, ownerID, "")), nil
		default:
			return nil, fmt.Errorf("credential doctor: inspect keystore: %w", err)
		}
	}

	localByOwner := make(map[int64]AppCredentials, len(apps))
	for _, app := range apps {
		localByOwner[app.OwnerID] = app
	}
	firstByKey := make(map[string]AppCredentials, len(apps))
	reusedKeyIDs := make(map[string]struct{})
	for _, app := range apps {
		if first, ok := firstByKey[app.KeyID]; ok && first.AppID != app.AppID {
			reusedKeyIDs[app.KeyID] = struct{}{}
			findings = append(findings, credentialFinding(
				CredentialFindingReusedMachineKey,
				app.AppID,
				app.OwnerID,
				app.KeyID,
			))
			continue
		}
		firstByKey[app.KeyID] = app
	}

	checked := make(map[int64]struct{}, len(apps))
	for _, registration := range expected {
		app, ok := localByOwner[registration.OwnerID]
		if !ok {
			findings = append(findings, credentialFinding(
				CredentialFindingMissingKey,
				registration.AppID,
				registration.OwnerID,
				"",
			))
			continue
		}
		checked[app.OwnerID] = struct{}{}
		if !sameRegistration(app.Registration(), registration) {
			findings = append(findings, credentialFinding(
				CredentialFindingMetadataMismatch,
				registration.AppID,
				registration.OwnerID,
				"",
			))
			continue
		}
		if _, reused := reusedKeyIDs[app.KeyID]; reused {
			continue
		}
		appFindings, err := d.checkApp(ctx, app)
		if err != nil {
			return findings, err
		}
		findings = append(findings, appFindings...)
	}
	for _, app := range apps {
		if _, ok := checked[app.OwnerID]; ok {
			continue
		}
		if _, reused := reusedKeyIDs[app.KeyID]; reused {
			continue
		}
		appFindings, err := d.checkApp(ctx, app)
		if err != nil {
			return findings, err
		}
		findings = append(findings, appFindings...)
	}
	return findings, nil
}

// janitorCode separates a registration the janitor is actively failing or
// repeatedly removing from one that is merely waiting for its first pass. All
// shut the same gate, but #281 made failures survivable and #290 makes removal
// churn diagnosable, so neither state has another announcement path. A fault
// takes precedence; its reason stays in the janitor because it may quote a
// remote or operator-authored value, while CredentialFinding carries safe
// coordinates only.
func (d *CredentialDoctor) janitorCode(registrationID int64) CredentialFindingCode {
	if faults, ok := d.janitor.(janitorFaultSource); ok {
		for _, fault := range faults.RegistrationFaults() {
			if fault.RegistrationID == registrationID {
				return CredentialFindingJanitorFailed
			}
		}
	}
	churn, ok := d.janitor.(janitorChurnSource)
	if !ok {
		return CredentialFindingJanitorInactive
	}
	for _, registration := range churn.ChurningRegistrations() {
		if registration.RegistrationID == registrationID {
			return CredentialFindingJanitorChurning
		}
	}
	return CredentialFindingJanitorInactive
}

func (d *CredentialDoctor) checkApp(
	ctx context.Context,
	app AppCredentials,
) ([]CredentialFinding, error) {
	err := d.onboarder.verifyRegistration(ctx, app.Registration(), app.Key)
	switch {
	case errors.Is(err, ErrAppVisibilityMismatch):
		return []CredentialFinding{credentialFinding(
			CredentialFindingVisibilityMismatch,
			app.AppID,
			app.OwnerID,
			"",
		)}, nil
	case errors.Is(err, ErrAppRegistrationMismatch):
		return []CredentialFinding{credentialFinding(
			CredentialFindingMetadataMismatch,
			app.AppID,
			app.OwnerID,
			"",
		)}, nil
	case err != nil:
		return nil, fmt.Errorf("credential doctor: registration %d: %w", app.AppID, err)
	}
	if d.janitor == nil || !d.janitor.ActiveFor(app.AppID) {
		return []CredentialFinding{credentialFinding(
			d.janitorCode(app.AppID),
			app.AppID,
			app.OwnerID,
			"",
		)}, nil
	}
	return nil, nil
}
