package wardstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// DefaultIntegrityProbeHold bounds one identity's read hold and the
// observations under it. It is short because the hold delays a refresh or
// enrollment on that identity, and a crashed pass leaves it until it expires.
const DefaultIntegrityProbeHold = 2 * time.Minute

// IntegritySkipReason is why a probe pass did not check one enrollment's
// store. It is a fixed category, never a cause's text. The zero value is
// invalid by design.
type IntegritySkipReason string

const (
	// IntegritySkipMutationLeaseLive: the identity's mutation lease was live,
	// so its store may have been changing (plan §5.4).
	IntegritySkipMutationLeaseLive IntegritySkipReason = "mutation_lease_live"
	// IntegritySkipReadHoldUnavailable: the read hold could not be taken for
	// any other reason.
	IntegritySkipReadHoldUnavailable IntegritySkipReason = "read_hold_unavailable"
	// IntegritySkipStoreAbsent: no volume, token file, or host file to read.
	IntegritySkipStoreAbsent IntegritySkipReason = "store_absent"
	// IntegritySkipObservationFailed: the observation ended in any other
	// error.
	IntegritySkipObservationFailed IntegritySkipReason = "observation_failed"
	// IntegritySkipObservationUnstable: two observations under one hold
	// disagreed on the store's length verdict.
	IntegritySkipObservationUnstable IntegritySkipReason = "observation_unstable"
	// IntegritySkipReadHoldEnded: the hold was no longer live when the
	// observation finished, so the store may have changed under it.
	IntegritySkipReadHoldEnded IntegritySkipReason = "read_hold_ended"
	// IntegritySkipGenerationChanged: the enrollment's current generation
	// moved during the observation.
	IntegritySkipGenerationChanged IntegritySkipReason = "generation_changed"
)

// AllIntegritySkipReasons is the single registration point for skip reasons.
var AllIntegritySkipReasons = []IntegritySkipReason{
	IntegritySkipMutationLeaseLive,
	IntegritySkipReadHoldUnavailable,
	IntegritySkipStoreAbsent,
	IntegritySkipObservationFailed,
	IntegritySkipObservationUnstable,
	IntegritySkipReadHoldEnded,
	IntegritySkipGenerationChanged,
}

func (r IntegritySkipReason) valid() bool {
	switch r {
	case IntegritySkipMutationLeaseLive, IntegritySkipReadHoldUnavailable,
		IntegritySkipStoreAbsent, IntegritySkipObservationFailed,
		IntegritySkipObservationUnstable,
		IntegritySkipReadHoldEnded, IntegritySkipGenerationChanged:
		return true
	default:
		return false
	}
}

// IntegrityProbeResult is one enrollment's outcome from a probe pass.
type IntegrityProbeResult struct {
	AuthIdentityID domain.AuthIdentityID
	EnrollmentID   domain.ClientEnrollmentID
	// Ordinal is the generation the pass read as current.
	Ordinal int
	// Skipped is empty when the store was observed to completion, and
	// otherwise why it was not checked. A skipped enrollment is never
	// marked.
	Skipped IntegritySkipReason
	// Err is the cause behind Skipped, for the caller's log. It comes from
	// the store or from a ward observation, neither of which carries
	// credential bytes; it never enters a mark, an item, or a report.
	Err error
	// CorruptionChecked reports whether the recorded digest was compared
	// with the store. It is false for a store the daemon refreshes: a
	// refresh rewrites the store without appending a generation, so the
	// recorded digest no longer describes a healthy store.
	CorruptionChecked bool
	// CorruptionUnconfirmed reports a corruption finding that a second
	// observation did not reproduce, so it recorded no mark. The length
	// verdict both observations agreed on still stands.
	CorruptionUnconfirmed bool
	// Marks are the marks this pass recorded, each as the store holds it:
	// the first observation of that finding, which may predate this pass.
	Marks []domain.GenerationIntegrityMark
}

// IntegrityProbe is the scheduled credential-integrity probe (plan §10,
// issue #1630): one pass checks the current generation of every enrollment
// for a truncated or corrupted stored credential and records a mark per
// finding.
//
// A false mark has no way back until re-enrollment ships, so the probe marks
// only after it finishes observing a store under a read hold that is still
// live. Everything short of that is a skip.
type IntegrityProbe struct {
	Store *store.Store
	// ObserveSetupToken observes a Claude setup-token volume; production
	// binds ward.ObserveSetupTokenIntegrity to the daemon's runtime.
	ObserveSetupToken func(ctx context.Context, volume string) (ward.SetupTokenIntegrity, error)
	// ObserveCodexStore observes a Codex host auth store; production binds
	// ward.ObserveCodexStoreIntegrity to the daemon's private root.
	ObserveCodexStore func(path string) (ward.CodexStoreIntegrity, error)
	// Now returns UTC instants. The hold window is judged on this clock.
	Now func() time.Time
	// HoldDuration bounds each identity's read hold and the observations
	// under it; zero means DefaultIntegrityProbeHold.
	HoldDuration time.Duration
}

// storeObservation is what either store kind reports: the digests a healthy
// store could have been recorded under, and the length verdict.
type storeObservation struct {
	digests   []domain.Digest
	truncated bool
}

type probeTarget struct {
	identity    domain.AuthIdentityID
	enrollments []domain.ClientEnrollment
}

// Run executes one pass. Its error is a store failure that prevents listing
// the enrollments; a store the pass cannot observe is a skipped result, and
// one such store never stops the pass from checking the others.
func (p IntegrityProbe) Run(ctx context.Context) ([]IntegrityProbeResult, error) {
	if p.Store == nil || p.ObserveSetupToken == nil || p.ObserveCodexStore == nil || p.Now == nil {
		return nil, errors.New("credential integrity probe: nil dependency")
	}
	if p.HoldDuration == 0 {
		p.HoldDuration = DefaultIntegrityProbeHold
	}
	if p.HoldDuration < 0 {
		return nil, errors.New("credential integrity probe: negative hold duration")
	}
	targets, err := p.targets(ctx)
	if err != nil {
		return nil, fmt.Errorf("credential integrity probe: %w", err)
	}
	var results []IntegrityProbeResult
	for _, target := range targets {
		results = append(results, p.probeIdentity(ctx, target)...)
	}
	return results, nil
}

// targets lists every identity's enrollments that have a generation. An
// enrollment with none has no store to check.
func (p IntegrityProbe) targets(ctx context.Context) ([]probeTarget, error) {
	var targets []probeTarget
	err := p.Store.Read(ctx, func(tx *store.ReadTx) error {
		identities, err := tx.ListAuthIdentities(ctx)
		if err != nil {
			return err
		}
		for _, identity := range identities {
			enrollments, err := tx.ListClientEnrollments(ctx, identity.ID)
			if err != nil {
				return err
			}
			target := probeTarget{identity: identity.ID}
			for _, enrollment := range enrollments {
				_, err := tx.CurrentEnrollmentGeneration(ctx, enrollment.ID)
				if errors.Is(err, store.ErrNotFound) {
					continue
				}
				if err != nil {
					return err
				}
				target.enrollments = append(target.enrollments, enrollment)
			}
			if len(target.enrollments) != 0 {
				targets = append(targets, target)
			}
		}
		return nil
	})
	return targets, err
}

// probeIdentity checks one identity's enrollments under a single shared read
// hold. It never takes the mutation lease: a live one is a skip.
func (p IntegrityProbe) probeIdentity(ctx context.Context, target probeTarget) []IntegrityProbeResult {
	skipAll := func(reason IntegritySkipReason, err error) []IntegrityProbeResult {
		results := make([]IntegrityProbeResult, 0, len(target.enrollments))
		for _, enrollment := range target.enrollments {
			results = append(results, IntegrityProbeResult{
				AuthIdentityID: target.identity, EnrollmentID: enrollment.ID, Skipped: reason, Err: err,
			})
		}
		return results
	}
	holder, err := newIntegrityProbeHolder()
	if err != nil {
		return skipAll(IntegritySkipReadHoldUnavailable, err)
	}
	// The deadline starts before the hold does, so on the daemon's wall
	// clock it ends no later than the hold: an observer that outlives the
	// window is cancelled instead of reading a store nothing holds.
	observeCtx, cancel := context.WithTimeout(ctx, p.HoldDuration)
	defer cancel()
	acquiredAt := p.Now()
	var hold domain.AuthStoreReadHold
	err = p.Store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		hold, err = tx.AcquireAuthStoreReadHold(ctx, target.identity, holder, acquiredAt, acquiredAt.Add(p.HoldDuration))
		return err
	})
	if errors.Is(err, store.ErrLeaseHeld) {
		return skipAll(IntegritySkipMutationLeaseLive, err)
	}
	if err != nil {
		return skipAll(IntegritySkipReadHoldUnavailable, err)
	}
	defer p.releaseHold(ctx, hold)

	results := make([]IntegrityProbeResult, 0, len(target.enrollments))
	for _, enrollment := range target.enrollments {
		results = append(results, p.probeEnrollment(observeCtx, enrollment, holder))
	}
	return results
}

// releaseHold ends the hold on a context that survives the pass's
// cancellation. Its error is dropped on purpose: a hold that is not released
// expires on its own, having delayed mutations for at most HoldDuration and
// blocked no reader, and a hold that already expired refuses the release.
func (p IntegrityProbe) releaseHold(ctx context.Context, hold domain.AuthStoreReadHold) {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_ = p.Store.WriteInternal(releaseCtx, func(tx *store.InternalTx) error {
		return tx.ReleaseAuthStoreReadHold(
			releaseCtx, hold.AuthIdentityID, hold.Holder, hold.AcquiredAt, p.Now())
	})
}

func (p IntegrityProbe) probeEnrollment(
	ctx context.Context, enrollment domain.ClientEnrollment, holder domain.InvocationID,
) IntegrityProbeResult {
	result := IntegrityProbeResult{AuthIdentityID: enrollment.AuthIdentityID, EnrollmentID: enrollment.ID}
	skip := func(reason IntegritySkipReason, err error) IntegrityProbeResult {
		result.Skipped, result.Err = reason, err
		return result
	}
	// Read the generation again under the hold: nothing can append one while
	// the hold is live, so this is the store the observation reads.
	var generation domain.EnrollmentGeneration
	if err := p.Store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		generation, err = tx.CurrentEnrollmentGeneration(ctx, enrollment.ID)
		return err
	}); err != nil {
		return skip(IntegritySkipObservationFailed, err)
	}
	result.Ordinal = generation.Ordinal

	observe := func() (storeObservation, IntegritySkipReason, error) {
		seen, err := p.observe(ctx, enrollment, generation)
		switch {
		case errors.Is(err, ward.ErrCredentialStoreAbsent):
			return seen, IntegritySkipStoreAbsent, err
		case err != nil:
			return seen, IntegritySkipObservationFailed, err
		}
		return seen, "", nil
	}
	seen, reason, err := observe()
	if reason != "" {
		return skip(reason, err)
	}
	findings, corruptionChecked := classifyIntegrity(enrollment, generation, seen)
	corruptionUnconfirmed := false
	if slices.Contains(findings, domain.CredentialIntegrityCorruption) {
		// A digest cannot prove it covers the whole store: a tool that
		// fails inside the setup-token observer moves the tree digest
		// without failing the proof. A corruption finding marks only when a
		// second observation, under the same hold, reports the same
		// digests. The length verdict is validated on its own in each
		// observation, so one both agree on stands either way.
		again, reason, err := observe()
		if reason != "" {
			return skip(reason, err)
		}
		if again.truncated != seen.truncated {
			return skip(IntegritySkipObservationUnstable, nil)
		}
		if !slices.Equal(again.digests, seen.digests) {
			findings = slices.DeleteFunc(findings, func(finding domain.CredentialIntegrityFinding) bool {
				return finding == domain.CredentialIntegrityCorruption
			})
			corruptionChecked, corruptionUnconfirmed = false, true
		}
	}

	// One transaction decides whether the observation counts and records
	// what it found: the hold must still be live and the generation must be
	// the one observed, or nothing is recorded.
	var marks []domain.GenerationIntegrityMark
	err = p.Store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		reason, marks = "", nil
		observedAt := p.Now().UTC()
		hold, err := tx.GetAuthStoreReadHold(ctx, enrollment.AuthIdentityID, holder)
		if err != nil {
			return err
		}
		if !hold.HeldAt(observedAt) {
			reason = IntegritySkipReadHoldEnded
			return nil
		}
		current, err := tx.CurrentEnrollmentGeneration(ctx, enrollment.ID)
		if err != nil {
			return err
		}
		if current.Ordinal != generation.Ordinal {
			reason = IntegritySkipGenerationChanged
			return nil
		}
		for _, finding := range findings {
			mark, err := tx.RecordGenerationIntegrityMark(ctx, domain.GenerationIntegrityMark{
				EnrollmentID: enrollment.ID, Ordinal: generation.Ordinal,
				Finding: finding, ObservedAt: observedAt,
			})
			if err != nil {
				return err
			}
			marks = append(marks, mark)
		}
		return nil
	})
	if err != nil {
		return skip(IntegritySkipObservationFailed, err)
	}
	if reason != "" {
		return skip(reason, nil)
	}
	result.CorruptionChecked = corruptionChecked
	result.CorruptionUnconfirmed = corruptionUnconfirmed
	result.Marks = marks
	return result
}

func (p IntegrityProbe) observe(
	ctx context.Context, enrollment domain.ClientEnrollment, generation domain.EnrollmentGeneration,
) (storeObservation, error) {
	switch enrollment.HarnessClient {
	case domain.HarnessClientClaudeCode:
		seen, err := p.ObserveSetupToken(ctx, generation.AuthStoreVolume)
		if err != nil {
			return storeObservation{}, err
		}
		// auth add records the token's hash and auth adopt the volume's
		// tree digest, so a healthy store matches either.
		return storeObservation{
			digests:   []domain.Digest{seen.TokenDigest, seen.TreeDigest},
			truncated: seen.Truncated,
		}, nil
	case domain.HarnessClientCodexCLI:
		seen, err := p.ObserveCodexStore(generation.AuthStoreVolume)
		if err != nil {
			return storeObservation{}, err
		}
		return storeObservation{digests: []domain.Digest{seen.ContentDigest}, truncated: seen.Truncated}, nil
	}
	return storeObservation{}, fmt.Errorf("harness client %q has no integrity observation", enrollment.HarnessClient)
}

// classifyIntegrity turns a finished observation into findings. Truncation
// is the length verdict alone. Corruption is checked only for a store the
// daemon never refreshes, where the recorded digest still describes a
// healthy store: it is a recorded digest that matches none of the observed
// ones.
func classifyIntegrity(
	enrollment domain.ClientEnrollment, generation domain.EnrollmentGeneration, seen storeObservation,
) (findings []domain.CredentialIntegrityFinding, corruptionChecked bool) {
	if seen.truncated {
		findings = append(findings, domain.CredentialIntegrityTruncation)
	}
	if enrollment.RefreshStrategy != domain.RefreshExternal {
		return findings, false
	}
	if !slices.Contains(seen.digests, generation.StoreManifestDigest) {
		findings = append(findings, domain.CredentialIntegrityCorruption)
	}
	return findings, true
}

// newIntegrityProbeHolder mints a holder unique to one identity's pass: the
// read hold is keyed by holder, so two passes sharing one would share a
// window.
func newIntegrityProbeHolder() (domain.InvocationID, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", errors.New("mint credential integrity probe holder")
	}
	return domain.InvocationID("credential-integrity-probe-" + hex.EncodeToString(token[:])), nil
}
