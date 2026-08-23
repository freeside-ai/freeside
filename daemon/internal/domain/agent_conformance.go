package domain

import (
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// AdapterConformance is the durable record of what one adapter build's stage
// contract suite proved (plan §5.4, admission step 3): one record per
// completed pass over an adapter build, with the proved launch capabilities
// in the closed vocabulary. The store contract is proved the same way —
// LaunchCapRouteStoreContract attests the sanitized single-route store,
// read-only to the agent, daemon-owned refresh under the identity lease,
// refresh hosts reachable by the daemon only, and harness update and
// telemetry hosts absent from the proxy allowlist. Runner conformance
// (BackendConformance, §5.7) is untouched: that vocabulary isolates the
// runner backend, this one proves what the driven harness honours.
//
// Like the backend record, this is a ceiling, never a grant: admission
// checks a launch's required capabilities against the newest passed record
// for the agent's adapter build and fails closed with a typed error when the
// launch requires more than the suite proved.
type AdapterConformance struct {
	// AdapterDigest names the exact adapter fragment (build plus pinned
	// harness build) the suite exercised; a different build is a different
	// record.
	AdapterDigest Digest             `json:"adapter_digest"`
	Outcome       ConformanceOutcome `json:"outcome"`
	// ProvedCapabilities is the proven declaration of a passed pass, and nil
	// exactly when the pass did not pass: a failed pass proves nothing.
	ProvedCapabilities LaunchCapabilitySet `json:"proved_capabilities"`
	// Generation is the store-assigned, per-adapter-monotonic proof
	// generation: zero on a record that has not been persisted yet, the row
	// identity once it has (the BackendConformance posture).
	Generation uint64 `json:"generation"`
	// ProvedAt is the UTC instant the pass completed; supersession is decided
	// by Generation, never by comparing timestamps.
	ProvedAt time.Time `json:"proved_at"`
}

// AdapterConformanceInput carries the caller-supplied fields; it has no
// Generation field, so no input path can set the append identity.
type AdapterConformanceInput struct {
	AdapterDigest      Digest
	Outcome            ConformanceOutcome
	ProvedCapabilities LaunchCapabilitySet
	ProvedAt           time.Time
}

// NewAdapterConformance builds a validated record in canonical form: the
// capability set canonicalized and detached, the timestamp normalized to UTC.
func NewAdapterConformance(in AdapterConformanceInput) (AdapterConformance, error) {
	c := AdapterConformance{
		AdapterDigest:      in.AdapterDigest,
		Outcome:            in.Outcome,
		ProvedCapabilities: NewLaunchCapabilitySet(in.ProvedCapabilities...),
		ProvedAt:           in.ProvedAt.UTC(),
	}
	if err := c.Validate(); err != nil {
		return AdapterConformance{}, err
	}
	return c, nil
}

// Validate reports whether the record is well-formed. Generation is
// deliberately unconstrained: zero means not yet persisted, and the store
// stamps and range-checks the persisted value itself.
func (c AdapterConformance) Validate() error {
	if !contentaddr.Valid(string(c.AdapterDigest)) {
		return fmt.Errorf("adapter conformance adapter_digest %q: %w", c.AdapterDigest, ErrInvalidDigest)
	}
	if !c.Outcome.valid() {
		return fmt.Errorf("adapter conformance outcome %q: %w", c.Outcome, ErrInvalidConformanceOutcome)
	}
	if err := c.ProvedCapabilities.Validate(); err != nil {
		return fmt.Errorf("adapter conformance: %w", err)
	}
	if c.Outcome != ConformancePassed && c.ProvedCapabilities != nil {
		return fmt.Errorf("adapter conformance for %q: %w", c.AdapterDigest, ErrConformanceCapabilitiesWithoutPass)
	}
	if c.ProvedAt.IsZero() {
		return fmt.Errorf("adapter conformance proved_at: %w", ErrMissingTimestamp)
	}
	return nil
}

// ValidateAdapterLaunchCoverage is admission step 3's adapter leg: the
// record must be a passed pass for the agent's exact adapter build, and its
// proved capabilities must cover everything the stage's launch requires. A
// launch requiring capabilities beyond the proved set fails closed with a
// typed error naming the missing members.
func ValidateAdapterLaunchCoverage(record AdapterConformance, adapterDigest Digest, launch LaunchSpec) error {
	if err := record.Validate(); err != nil {
		return err
	}
	if err := launch.Validate(); err != nil {
		return err
	}
	if record.AdapterDigest != adapterDigest {
		return fmt.Errorf("adapter conformance proves build %q, agent runs %q: %w",
			record.AdapterDigest, adapterDigest, ErrLaunchCapabilityUnproved)
	}
	if record.Outcome != ConformancePassed {
		return fmt.Errorf("adapter conformance for %q records outcome %q: %w",
			record.AdapterDigest, record.Outcome, ErrLaunchCapabilityUnproved)
	}
	if missing := MissingLaunchCapabilities(record.ProvedCapabilities, launch.RequiredCapabilities()); len(missing) > 0 {
		return fmt.Errorf("launch for stage %q requires %v beyond the proved set: %w",
			launch.Stage, missing, ErrLaunchCapabilityUnproved)
	}
	return nil
}
