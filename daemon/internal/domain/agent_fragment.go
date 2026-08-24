package domain

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// The admitted-agent configuration fragments (plan §5.4, revision 39):
// operator-authored, content-addressed documents in the control-plane tree.
// Fragments are configuration and carry authority; identities, enrollments,
// generations, and admissions are facts and records. A fragment's digest is
// computed over its versioned canonical body; names live in the tree's
// name-to-digest map and are never part of any digest, so the same content
// under two names is one fragment and a renamed fragment is unchanged.

const (
	AgentFragmentEncodingVersion = 1
	MaxAgentFragmentBytes        = 64 << 10
)

// RouteFragment is one credential route: hosts, protocol, and terms basis
// (§5.4). An endpoint or terms edit changes this digest and every agent
// naming the route, never the enrollments enrolled through it — enrollments
// reference the route's stable tree name, not this digest.
type RouteFragment struct {
	EncodingVersion int `json:"encoding_version"`
	// ServiceOperator names who operates the credentialed service the route
	// reaches (e.g. the subscription vendor), distinct from the model
	// developer and the harness.
	ServiceOperator string `json:"service_operator"`
	Protocol        string `json:"protocol"`
	// InferenceAuthorities is the ordered list of network authorities the
	// route's inference traffic is allowed to reach; the effective egress
	// allowlist derives from it.
	InferenceAuthorities []string `json:"inference_authorities"`
	BillingMode          string   `json:"billing_mode"`
	FallbackPolicy       string   `json:"fallback_policy"`
	// TermsBasisDate dates the operator's recorded basis for believing the
	// route's use is permitted (§14 subscription-terms drift watches it).
	TermsBasisDate string `json:"terms_basis_date"`
	// Digest is the content address of the canonical body; computed, never
	// caller-chosen, and re-verified at every boundary.
	Digest Digest `json:"digest"`
}

type canonicalRouteFragment struct {
	EncodingVersion      int      `json:"encoding_version"`
	ServiceOperator      string   `json:"service_operator"`
	Protocol             string   `json:"protocol"`
	InferenceAuthorities []string `json:"inference_authorities"`
	BillingMode          string   `json:"billing_mode"`
	FallbackPolicy       string   `json:"fallback_policy"`
	TermsBasisDate       string   `json:"terms_basis_date"`
}

func (f RouteFragment) canonical() canonicalRouteFragment {
	return canonicalRouteFragment{
		EncodingVersion: f.EncodingVersion, ServiceOperator: f.ServiceOperator,
		Protocol: f.Protocol, InferenceAuthorities: f.InferenceAuthorities,
		BillingMode: f.BillingMode, FallbackPolicy: f.FallbackPolicy,
		TermsBasisDate: f.TermsBasisDate,
	}
}

// ComputeDigest hashes the explicit-version canonical encoding. Struct field
// order is part of the contract and is pinned by a golden.
func (f RouteFragment) ComputeDigest() (Digest, error) {
	body, err := json.Marshal(f.canonical())
	if err != nil {
		return "", fmt.Errorf("route fragment canonical encoding: %w", err)
	}
	return Digest(contentaddr.Sum(body)), nil
}

// Validate reports whether the fragment is well-formed and its digest
// authentic.
func (f RouteFragment) Validate() error {
	if f.EncodingVersion != AgentFragmentEncodingVersion {
		return fmt.Errorf("route fragment encoding_version %d: %w", f.EncodingVersion, ErrAgentEncodingVersion)
	}
	if f.ServiceOperator == "" {
		return fmt.Errorf("route fragment service_operator: %w", ErrEmptyField)
	}
	if f.Protocol == "" {
		return fmt.Errorf("route fragment protocol: %w", ErrEmptyField)
	}
	if len(f.InferenceAuthorities) == 0 {
		return fmt.Errorf("route fragment inference_authorities: %w", ErrEmptyField)
	}
	for _, authority := range f.InferenceAuthorities {
		if authority == "" {
			return fmt.Errorf("route fragment inference authority: %w", ErrEmptyField)
		}
	}
	if f.BillingMode == "" {
		return fmt.Errorf("route fragment billing_mode: %w", ErrEmptyField)
	}
	if f.FallbackPolicy == "" {
		return fmt.Errorf("route fragment fallback_policy: %w", ErrEmptyField)
	}
	if _, err := time.Parse(time.DateOnly, f.TermsBasisDate); err != nil {
		return fmt.Errorf("route fragment terms_basis_date %q: %w", f.TermsBasisDate, ErrMissingTimestamp)
	}
	return f.validateDigest()
}

func (f RouteFragment) validateDigest() error {
	if !contentaddr.Valid(string(f.Digest)) {
		return fmt.Errorf("route fragment digest %q: %w", f.Digest, ErrInvalidDigest)
	}
	computed, err := f.ComputeDigest()
	if err != nil {
		return err
	}
	if f.Digest != computed {
		return fmt.Errorf("route fragment digest %q, content resolves to %q: %w",
			f.Digest, computed, ErrAgentDigestMismatch)
	}
	return nil
}

// AdapterFragment names one Freeside adapter build: the harness client it
// drives, the exact harness build it pins, the launch capabilities it honours
// in the closed vocabulary, and the effort levels it can send. AgentVendor —
// the instruction mechanism — is derived from the adapter here and never
// selected by policy.
type AdapterFragment struct {
	EncodingVersion int `json:"encoding_version"`
	// AdapterBuild identifies the Freeside adapter implementation and build
	// (e.g. "pi_json_v1@sha256:…"); HarnessBuild pins the exact harness
	// version the conformance suite proved.
	AdapterBuild string            `json:"adapter_build"`
	HarnessBuild string            `json:"harness_build"`
	ClientKind   HarnessClientKind `json:"client_kind"`
	Vendor       AgentVendor       `json:"vendor"`
	// LaunchCapabilities is the canonical set of launch capabilities the
	// adapter declares it honours; the conformance record proves which it
	// actually does, and admission checks the launch against the proved set,
	// never this declaration alone.
	LaunchCapabilities LaunchCapabilitySet `json:"launch_capabilities"`
	SendableEfforts    []EffortLevel       `json:"sendable_efforts"`
	Digest             Digest              `json:"digest"`
}

type canonicalAdapterFragment struct {
	EncodingVersion    int                 `json:"encoding_version"`
	AdapterBuild       string              `json:"adapter_build"`
	HarnessBuild       string              `json:"harness_build"`
	ClientKind         HarnessClientKind   `json:"client_kind"`
	Vendor             AgentVendor         `json:"vendor"`
	LaunchCapabilities LaunchCapabilitySet `json:"launch_capabilities"`
	SendableEfforts    []EffortLevel       `json:"sendable_efforts"`
}

func (f AdapterFragment) canonical() canonicalAdapterFragment {
	return canonicalAdapterFragment{
		EncodingVersion: f.EncodingVersion, AdapterBuild: f.AdapterBuild,
		HarnessBuild: f.HarnessBuild, ClientKind: f.ClientKind, Vendor: f.Vendor,
		LaunchCapabilities: NewLaunchCapabilitySet(f.LaunchCapabilities...),
		SendableEfforts:    f.SendableEfforts,
	}
}

// ComputeDigest hashes the explicit-version canonical encoding, with the
// capability set canonicalized defensively so it is a true content address
// for any input.
func (f AdapterFragment) ComputeDigest() (Digest, error) {
	body, err := json.Marshal(f.canonical())
	if err != nil {
		return "", fmt.Errorf("adapter fragment canonical encoding: %w", err)
	}
	return Digest(contentaddr.Sum(body)), nil
}

// Validate reports whether the fragment is well-formed and its digest
// authentic.
func (f AdapterFragment) Validate() error {
	if f.EncodingVersion != AgentFragmentEncodingVersion {
		return fmt.Errorf("adapter fragment encoding_version %d: %w", f.EncodingVersion, ErrAgentEncodingVersion)
	}
	if f.AdapterBuild == "" {
		return fmt.Errorf("adapter fragment adapter_build: %w", ErrEmptyField)
	}
	if f.HarnessBuild == "" {
		return fmt.Errorf("adapter fragment harness_build: %w", ErrEmptyField)
	}
	if !f.ClientKind.valid() {
		return fmt.Errorf("adapter fragment client_kind %q: %w", f.ClientKind, ErrInvalidHarnessClientKind)
	}
	if !f.Vendor.valid() {
		return fmt.Errorf("adapter fragment vendor %q: %w", f.Vendor, ErrInvalidAgentVendor)
	}
	if err := f.LaunchCapabilities.Validate(); err != nil {
		return fmt.Errorf("adapter fragment: %w", err)
	}
	if len(f.SendableEfforts) == 0 {
		return fmt.Errorf("adapter fragment sendable_efforts: %w", ErrEmptyField)
	}
	if err := validateEffortList("adapter fragment sendable_efforts", f.SendableEfforts); err != nil {
		return err
	}
	return f.validateDigest()
}

func (f AdapterFragment) validateDigest() error {
	if !contentaddr.Valid(string(f.Digest)) {
		return fmt.Errorf("adapter fragment digest %q: %w", f.Digest, ErrInvalidDigest)
	}
	computed, err := f.ComputeDigest()
	if err != nil {
		return err
	}
	if f.Digest != computed {
		return fmt.Errorf("adapter fragment digest %q, content resolves to %q: %w",
			f.Digest, computed, ErrAgentDigestMismatch)
	}
	return nil
}

// OfferFragment is one route's offer of one model (§5.4): the same model
// through two routes is two offers. Which route an offer is authored under is
// a tree fact supplied at resolution, not part of this body — no name enters
// any digest.
type OfferFragment struct {
	EncodingVersion int `json:"encoding_version"`
	// RouteModelID is the model identifier as the route's protocol names it.
	RouteModelID string `json:"route_model_id"`
	// LineageGroup is the vendor-family lineage the §7 review-independence
	// rule compares (curated conservatively; the same weights through any
	// route are one group; empty means unknown, which fails that rule
	// closed).
	LineageGroup      string            `json:"lineage_group"`
	IdentityStability IdentityStability `json:"identity_stability"`
	AllowedEfforts    []EffortLevel     `json:"allowed_efforts"`
	PricingRevision   string            `json:"pricing_revision"`
	// NotAfter is the authored expiry: an offer whose not_after precedes the
	// attempt deadline does not resolve (§5.4 admission step 1).
	NotAfter time.Time `json:"not_after"`
	Digest   Digest    `json:"digest"`
}

type canonicalOfferFragment struct {
	EncodingVersion   int               `json:"encoding_version"`
	RouteModelID      string            `json:"route_model_id"`
	LineageGroup      string            `json:"lineage_group"`
	IdentityStability IdentityStability `json:"identity_stability"`
	AllowedEfforts    []EffortLevel     `json:"allowed_efforts"`
	PricingRevision   string            `json:"pricing_revision"`
	NotAfter          time.Time         `json:"not_after"`
}

func (f OfferFragment) canonical() canonicalOfferFragment {
	return canonicalOfferFragment{
		EncodingVersion: f.EncodingVersion, RouteModelID: f.RouteModelID,
		LineageGroup: f.LineageGroup, IdentityStability: f.IdentityStability,
		AllowedEfforts: f.AllowedEfforts, PricingRevision: f.PricingRevision,
		NotAfter: f.NotAfter,
	}
}

// ComputeDigest hashes the explicit-version canonical encoding.
func (f OfferFragment) ComputeDigest() (Digest, error) {
	body, err := json.Marshal(f.canonical())
	if err != nil {
		return "", fmt.Errorf("offer fragment canonical encoding: %w", err)
	}
	return Digest(contentaddr.Sum(body)), nil
}

// Validate reports whether the fragment is well-formed and its digest
// authentic. LineageGroup may be empty — unknown lineage is a legal authored
// state that the review-independence rule fails closed on, not a malformed
// document.
func (f OfferFragment) Validate() error {
	if f.EncodingVersion != AgentFragmentEncodingVersion {
		return fmt.Errorf("offer fragment encoding_version %d: %w", f.EncodingVersion, ErrAgentEncodingVersion)
	}
	if f.RouteModelID == "" {
		return fmt.Errorf("offer fragment route_model_id: %w", ErrEmptyField)
	}
	if !f.IdentityStability.valid() {
		return fmt.Errorf("offer fragment identity_stability %q: %w", f.IdentityStability, ErrInvalidIdentityStability)
	}
	if len(f.AllowedEfforts) == 0 {
		return fmt.Errorf("offer fragment allowed_efforts: %w", ErrEmptyField)
	}
	if err := validateEffortList("offer fragment allowed_efforts", f.AllowedEfforts); err != nil {
		return err
	}
	if f.PricingRevision == "" {
		return fmt.Errorf("offer fragment pricing_revision: %w", ErrEmptyField)
	}
	if f.NotAfter.IsZero() {
		return fmt.Errorf("offer fragment not_after: %w", ErrMissingTimestamp)
	}
	if f.NotAfter.Location() != time.UTC {
		return fmt.Errorf("offer fragment not_after: %w", ErrTimestampNotUTC)
	}
	return f.validateDigest()
}

func (f OfferFragment) validateDigest() error {
	if !contentaddr.Valid(string(f.Digest)) {
		return fmt.Errorf("offer fragment digest %q: %w", f.Digest, ErrInvalidDigest)
	}
	computed, err := f.ComputeDigest()
	if err != nil {
		return err
	}
	if f.Digest != computed {
		return fmt.Errorf("offer fragment digest %q, content resolves to %q: %w",
			f.Digest, computed, ErrAgentDigestMismatch)
	}
	return nil
}

func validateEffortList(field string, efforts []EffortLevel) error {
	seen := map[EffortLevel]struct{}{}
	for _, effort := range efforts {
		if !effort.valid() {
			return fmt.Errorf("%s %q: %w", field, effort, ErrInvalidEffortLevel)
		}
		if _, dup := seen[effort]; dup {
			return fmt.Errorf("%s %q: %w", field, effort, ErrDuplicate)
		}
		seen[effort] = struct{}{}
	}
	return nil
}

// EncodeRouteFragment, EncodeAdapterFragment, and EncodeOfferFragment emit
// the validated canonical persisted forms; the Decode functions reject
// oversized, unknown-field, invalid-UTF8, and trailing-data payloads before
// revalidating the content address.

func (f RouteFragment) Encode() ([]byte, error)   { return encodeFragment(f) }
func (f AdapterFragment) Encode() ([]byte, error) { return encodeFragment(f) }
func (f OfferFragment) Encode() ([]byte, error)   { return encodeFragment(f) }

type fragmentValidator interface{ Validate() error }

func encodeFragment[T fragmentValidator](f T) ([]byte, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(f)
	if err != nil {
		return nil, fmt.Errorf("fragment encode: %w", err)
	}
	return body, nil
}

func decodeFragment[T fragmentValidator](body []byte, name string) (T, error) {
	var f T
	if err := strictjson.Decode(body, &f, strictjson.RejectInvalidUTF8, MaxAgentFragmentBytes); err != nil {
		return f, fmt.Errorf("%s decode: %w", name, err)
	}
	if err := f.Validate(); err != nil {
		return f, err
	}
	return f, nil
}

func DecodeRouteFragment(body []byte) (RouteFragment, error) {
	return decodeFragment[RouteFragment](body, "route fragment")
}

func DecodeAdapterFragment(body []byte) (AdapterFragment, error) {
	return decodeFragment[AdapterFragment](body, "adapter fragment")
}

func DecodeOfferFragment(body []byte) (OfferFragment, error) {
	return decodeFragment[OfferFragment](body, "offer fragment")
}

// LaunchCapability is the adapter's closed launch-capability vocabulary
// (§5.4): what a stage's launch can require of a harness, proved per adapter
// build by the stage contract suite. It is deliberately distinct from
// RunnerCapability — that vocabulary isolates the runner backend, this one
// describes what the driven harness honours.
type LaunchCapability string

const (
	LaunchCapReadTools           LaunchCapability = "read_tools"
	LaunchCapMutationTools       LaunchCapability = "mutation_tools"
	LaunchCapExactResume         LaunchCapability = "exact_resume"
	LaunchCapInstructionDelivery LaunchCapability = "instruction_delivery"
	LaunchCapStructuredOutput    LaunchCapability = "structured_output"
	LaunchCapContextSeverance    LaunchCapability = "context_severance"
	// LaunchCapAuxiliaryInferenceControl is the ability to forbid or declare
	// the harness's own auxiliary inference (compaction, summarization,
	// subagents); a harness that cannot control it can only run launches
	// whose policy is observed.
	LaunchCapAuxiliaryInferenceControl LaunchCapability = "auxiliary_inference_control"
	// LaunchCapRouteStoreContract is the proved store contract per route:
	// sanitized single-route store, read-only to the agent, daemon-owned
	// refresh under the identity lease, refresh hosts reachable by the
	// daemon only, harness update and telemetry hosts absent from the proxy
	// allowlist.
	LaunchCapRouteStoreContract LaunchCapability = "route_store_contract"
)

// AllLaunchCapabilities lists every valid LaunchCapability; it drives
// table-driven tests and is the single place a new capability is registered.
var AllLaunchCapabilities = []LaunchCapability{
	LaunchCapReadTools,
	LaunchCapMutationTools,
	LaunchCapExactResume,
	LaunchCapInstructionDelivery,
	LaunchCapStructuredOutput,
	LaunchCapContextSeverance,
	LaunchCapAuxiliaryInferenceControl,
	LaunchCapRouteStoreContract,
}

func (c LaunchCapability) valid() bool {
	switch c {
	case LaunchCapReadTools, LaunchCapMutationTools, LaunchCapExactResume,
		LaunchCapInstructionDelivery, LaunchCapStructuredOutput,
		LaunchCapContextSeverance, LaunchCapAuxiliaryInferenceControl,
		LaunchCapRouteStoreContract:
		return true
	default:
		return false
	}
}

// LaunchCapabilitySet is the canonical (sorted, deduplicated) persisted form
// of a launch-capability declaration, mirroring CapabilitySnapshot so one
// declaration has exactly one byte form.
type LaunchCapabilitySet []LaunchCapability

// NewLaunchCapabilitySet returns the canonical set: sorted, deduplicated,
// detached from the caller's backing array; empty collapses to nil. Unknown
// members are kept for Validate to reject at the boundary.
func NewLaunchCapabilitySet(caps ...LaunchCapability) LaunchCapabilitySet {
	if len(caps) == 0 {
		return nil
	}
	out := slices.Clone(caps)
	slices.Sort(out)
	return slices.Compact(out)
}

// Validate reports whether the set is canonical and every member registered.
func (s LaunchCapabilitySet) Validate() error {
	for i, c := range s {
		if !c.valid() {
			return fmt.Errorf("launch capability %q: %w", c, ErrInvalidLaunchCapability)
		}
		if i > 0 && s[i-1] >= c {
			return fmt.Errorf("launch capability set at %q: %w", c, ErrKeysNotCanonical)
		}
	}
	return nil
}

// Has reports whether the set contains c.
func (s LaunchCapabilitySet) Has(c LaunchCapability) bool {
	_, found := slices.BinarySearch(s, c)
	return found
}

// MissingLaunchCapabilities returns the members of required that declared
// does not cover, canonicalized for deterministic rendering, or nil when
// covered.
func MissingLaunchCapabilities(declared LaunchCapabilitySet, required []LaunchCapability) []LaunchCapability {
	var missing []LaunchCapability
	for _, c := range required {
		if !declared.Has(c) {
			missing = append(missing, c)
		}
	}
	return NewLaunchCapabilitySet(missing...)
}

// EffortLevel is Freeside's requested-effort vocabulary. The adapter
// translates it to the harness's native value and the run records both; a
// clamp is rendered explicitly (max → xhigh), never silently (§5.4).
type EffortLevel string

const (
	// EffortHarnessDefault requests whatever the pinned harness build does
	// when no effort is passed — the honest name for today's Claude baseline,
	// which passes no effort flag at all.
	EffortHarnessDefault EffortLevel = "harness_default"
	EffortLow            EffortLevel = "low"
	EffortMedium         EffortLevel = "medium"
	EffortHigh           EffortLevel = "high"
	EffortMax            EffortLevel = "max"
)

// AllEffortLevels lists every valid EffortLevel.
var AllEffortLevels = []EffortLevel{
	EffortHarnessDefault, EffortLow, EffortMedium, EffortHigh, EffortMax,
}

func (e EffortLevel) valid() bool {
	switch e {
	case EffortHarnessDefault, EffortLow, EffortMedium, EffortHigh, EffortMax:
		return true
	default:
		return false
	}
}

// IdentityStability is an offer's declared upstream stability (§5.4): whether
// the served model identity is pinned, rolls forward under one id, or is
// opaque. Records claim only what the route exposed; pre-proving a rolling
// upstream would be fiction.
type IdentityStability string

const (
	IdentityPinned  IdentityStability = "pinned"
	IdentityRolling IdentityStability = "rolling"
	IdentityOpaque  IdentityStability = "opaque"
)

// AllIdentityStabilities lists every valid IdentityStability.
var AllIdentityStabilities = []IdentityStability{
	IdentityPinned, IdentityRolling, IdentityOpaque,
}

func (s IdentityStability) valid() bool {
	switch s {
	case IdentityPinned, IdentityRolling, IdentityOpaque:
		return true
	default:
		return false
	}
}
