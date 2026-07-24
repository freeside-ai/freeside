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
	CredentialFindingLegacyLayout       CredentialFindingCode = "legacy_singleton_layout"
	CredentialFindingReusedMachineKey   CredentialFindingCode = "reused_machine_key"
)

// AllCredentialFindingCodes is the single registration point for doctor
// findings.
var AllCredentialFindingCodes = []CredentialFindingCode{
	CredentialFindingBadPermissions,
	CredentialFindingMissingKey,
	CredentialFindingVisibilityMismatch,
	CredentialFindingMetadataMismatch,
	CredentialFindingJanitorInactive,
	CredentialFindingLegacyLayout,
	CredentialFindingReusedMachineKey,
}

func (c CredentialFindingCode) valid() bool {
	switch c {
	case CredentialFindingBadPermissions,
		CredentialFindingMissingKey,
		CredentialFindingVisibilityMismatch,
		CredentialFindingMetadataMismatch,
		CredentialFindingJanitorInactive,
		CredentialFindingLegacyLayout,
		CredentialFindingReusedMachineKey:
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

	apps, err := d.onboarder.keystore.ListApps()
	if err != nil {
		switch {
		case errors.Is(err, ErrCredentialPermissions):
			return []CredentialFinding{credentialFinding(CredentialFindingBadPermissions, 0, 0, "")}, nil
		case errors.Is(err, ErrNoAppCredentials), errors.Is(err, ErrNoAppRegistration):
			return []CredentialFinding{credentialFinding(CredentialFindingMissingKey, 0, 0, "")}, nil
		default:
			return nil, fmt.Errorf("credential doctor: inspect keystore: %w", err)
		}
	}

	localByOwner := make(map[int64]AppCredentials, len(apps))
	for _, app := range apps {
		localByOwner[app.OwnerID] = app
	}
	var findings []CredentialFinding
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
			CredentialFindingJanitorInactive,
			app.AppID,
			app.OwnerID,
			"",
		)}, nil
	}
	return nil, nil
}
