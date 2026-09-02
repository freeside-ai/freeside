package domain

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// An agent is one operator-authored document in the control-plane tree,
// reviewed as a diff, with four lines and no role (plan §5.4): who
// (enrollment), through (route), running (adapter), asking (offer, effort).
// The source uses names; the canonical body — what is hashed — holds the
// resolved enrollment id, the route, adapter, and offer digests, and the
// effort value. Names live in the tree's name-to-digest map and are never
// part of a digest, so a rename changes nothing and a line edit is a
// different agent.

// AgentEncodingVersion tags the agent's canonical serialization.
const AgentEncodingVersion = 1

// maxAgentNameBytes leaves room for the nine-byte ".attended" suffix in a
// 255-byte policy-tree filename component.
const maxAgentNameBytes = 246

// AgentSource is the operator-authored form: names, resolved against one
// control-plane revision at admission step 1.
type AgentSource struct {
	// Name is at most 246 bytes of lowercase ASCII alphanumeric, with '-' and
	// '_' allowed only as interior separators.
	Name       string      `json:"name"`
	Enrollment string      `json:"enrollment"`
	Route      string      `json:"route"`
	Adapter    string      `json:"adapter"`
	Offer      string      `json:"offer"`
	Effort     EffortLevel `json:"effort"`
}

// Validate reports whether the source lines are well-formed. Whether the
// names resolve is ResolveAgentDefinition's question, against a supplied
// tree revision.
func (s AgentSource) Validate() error {
	if s.Name == "" {
		return fmt.Errorf("agent source name: %w", ErrEmptyField)
	}
	if !validAgentName(s.Name) {
		return fmt.Errorf("agent source name %q: %w", s.Name, ErrInvalidAgentName)
	}
	for field, value := range map[string]string{
		"enrollment": s.Enrollment, "route": s.Route,
		"adapter": s.Adapter, "offer": s.Offer,
	} {
		if value == "" {
			return fmt.Errorf("agent %s %s: %w", s.Name, field, ErrEmptyField)
		}
	}
	if !s.Effort.valid() {
		return fmt.Errorf("agent %s effort %q: %w", s.Name, s.Effort, ErrInvalidEffortLevel)
	}
	return nil
}

// AgentDefinition is the resolved, digest-addressed form. Name rides outside
// the canonical body for rendering and the tree map; everything hashed is a
// resolved reference or the effort value.
type AgentDefinition struct {
	Name            string             `json:"name"`
	EncodingVersion int                `json:"encoding_version"`
	EnrollmentID    ClientEnrollmentID `json:"enrollment_id"`
	RouteDigest     Digest             `json:"route_digest"`
	AdapterDigest   Digest             `json:"adapter_digest"`
	OfferDigest     Digest             `json:"offer_digest"`
	Effort          EffortLevel        `json:"effort"`
	Digest          Digest             `json:"digest"`
}

// canonicalAgentDefinition is the versioned serialization the agent digest
// addresses: resolved references and the effort value, nothing else. A
// distinct type so adding a field without deciding whether it enters the
// identity is a compile-visible choice.
type canonicalAgentDefinition struct {
	EncodingVersion int                `json:"encoding_version"`
	EnrollmentID    ClientEnrollmentID `json:"enrollment_id"`
	RouteDigest     Digest             `json:"route_digest"`
	AdapterDigest   Digest             `json:"adapter_digest"`
	OfferDigest     Digest             `json:"offer_digest"`
	Effort          EffortLevel        `json:"effort"`
}

func (a AgentDefinition) canonical() canonicalAgentDefinition {
	return canonicalAgentDefinition{
		EncodingVersion: a.EncodingVersion, EnrollmentID: a.EnrollmentID,
		RouteDigest: a.RouteDigest, AdapterDigest: a.AdapterDigest,
		OfferDigest: a.OfferDigest, Effort: a.Effort,
	}
}

// ComputeDigest hashes the explicit-version canonical body. Struct field
// order is part of the contract and is pinned by a golden.
func (a AgentDefinition) ComputeDigest() (Digest, error) {
	body, err := json.Marshal(a.canonical())
	if err != nil {
		return "", fmt.Errorf("agent canonical encoding: %w", err)
	}
	return Digest(contentaddr.Sum(body)), nil
}

// Validate reports whether the definition is well-formed, resolved, and its
// digest authentic. A canonical body that carries a name where a resolved
// reference belongs fails here: a fragment name is not a valid content
// address, so the digest-shape checks refuse it with a typed error before
// the content address is even compared.
func (a AgentDefinition) Validate() error {
	if a.Name == "" {
		return fmt.Errorf("agent name: %w", ErrEmptyField)
	}
	if !validAgentName(a.Name) {
		return fmt.Errorf("agent name %q: %w", a.Name, ErrInvalidAgentName)
	}
	if a.EncodingVersion != AgentEncodingVersion {
		return fmt.Errorf("agent %s encoding_version %d: %w", a.Name, a.EncodingVersion, ErrAgentEncodingVersion)
	}
	if a.EnrollmentID == "" {
		return fmt.Errorf("agent %s enrollment_id: %w", a.Name, ErrEmptyID)
	}
	for field, digest := range map[string]Digest{
		"route_digest": a.RouteDigest, "adapter_digest": a.AdapterDigest,
		"offer_digest": a.OfferDigest,
	} {
		if !contentaddr.Valid(string(digest)) {
			return fmt.Errorf("agent %s %s %q: %w", a.Name, field, digest, ErrAgentBodyUnresolved)
		}
	}
	if !a.Effort.valid() {
		return fmt.Errorf("agent %s effort %q: %w", a.Name, a.Effort, ErrInvalidEffortLevel)
	}
	if !contentaddr.Valid(string(a.Digest)) {
		return fmt.Errorf("agent %s digest %q: %w", a.Name, a.Digest, ErrInvalidDigest)
	}
	computed, err := a.ComputeDigest()
	if err != nil {
		return err
	}
	if a.Digest != computed {
		return fmt.Errorf("agent %s digest %q, content resolves to %q: %w",
			a.Name, a.Digest, computed, ErrAgentDigestMismatch)
	}
	return nil
}

// Encode emits the validated canonical persisted form.
func (a AgentDefinition) Encode() ([]byte, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(a)
	if err != nil {
		return nil, fmt.Errorf("agent encode: %w", err)
	}
	return body, nil
}

// DecodeAgentDefinition rejects oversized, unknown-field, invalid-UTF8, and
// trailing-data payloads before revalidating the content address.
func DecodeAgentDefinition(body []byte) (AgentDefinition, error) {
	var a AgentDefinition
	if err := strictjson.Decode(body, &a, strictjson.RejectInvalidUTF8, MaxAgentFragmentBytes); err != nil {
		return AgentDefinition{}, fmt.Errorf("agent decode: %w", err)
	}
	if err := a.Validate(); err != nil {
		return AgentDefinition{}, err
	}
	return a, nil
}

// AgentResolutionInput is everything admission step 1 resolves an agent
// against: the source lines, the tree's fragments for the named route,
// adapter, and offer, the tree fact of which route the offer is authored
// under, and the enrollment and identity records the `who` line names. The
// caller (the store, at a trust boundary) supplies records it has already
// reconstructed through their own gates.
type AgentResolutionInput struct {
	Source     AgentSource
	Identity   AuthIdentity
	Enrollment ClientEnrollment
	Route      RouteFragment
	Adapter    AdapterFragment
	Offer      OfferFragment
	// OfferRoute is the route name the offer is authored under in the tree —
	// resolution context, deliberately outside every digest.
	OfferRoute string
}

// ResolveAgentDefinition builds and validates the resolved agent: fragment
// digests computed from the supplied fragments, the join validated, and the
// agent digest computed last. The §5.4 join rules, each failing closed with
// ErrAgentJoinInvalid:
//
//   - the enrollment is the agent's `who`: it belongs to an enabled
//     identity, carries that identity's account binding, and is enrolled
//     through the agent's route for the adapter's client kind;
//   - the offer is authored under the agent's route;
//   - the effort is one the offer allows and the adapter can send.
func ResolveAgentDefinition(in AgentResolutionInput) (AgentDefinition, error) {
	if err := in.Source.Validate(); err != nil {
		return AgentDefinition{}, err
	}
	fail := func(format string, args ...any) (AgentDefinition, error) {
		detail := fmt.Sprintf(format, args...)
		return AgentDefinition{}, fmt.Errorf("agent %s: %s: %w", in.Source.Name, detail, ErrAgentJoinInvalid)
	}
	if err := in.Identity.Validate(); err != nil {
		return AgentDefinition{}, fmt.Errorf("agent %s identity: %w", in.Source.Name, err)
	}
	if err := in.Enrollment.Validate(); err != nil {
		return AgentDefinition{}, fmt.Errorf("agent %s enrollment: %w", in.Source.Name, err)
	}
	if err := ValidateEnrollmentIdentityBinding(in.Identity, in.Enrollment); err != nil {
		return AgentDefinition{}, fmt.Errorf("agent %s: %w", in.Source.Name, err)
	}
	if err := in.Route.Validate(); err != nil {
		return AgentDefinition{}, fmt.Errorf("agent %s route: %w", in.Source.Name, err)
	}
	if err := in.Adapter.Validate(); err != nil {
		return AgentDefinition{}, fmt.Errorf("agent %s adapter: %w", in.Source.Name, err)
	}
	if err := in.Offer.Validate(); err != nil {
		return AgentDefinition{}, fmt.Errorf("agent %s offer: %w", in.Source.Name, err)
	}
	if !in.Identity.Enabled {
		return fail("identity %s is disabled", in.Identity.ID)
	}
	if in.Enrollment.Route != in.Source.Route {
		return fail("enrollment %s is enrolled through route %q, agent names %q",
			in.Enrollment.ID, in.Enrollment.Route, in.Source.Route)
	}
	if in.Enrollment.HarnessClient != in.Adapter.ClientKind {
		return fail("enrollment %s binds client %q, adapter drives %q",
			in.Enrollment.ID, in.Enrollment.HarnessClient, in.Adapter.ClientKind)
	}
	if in.OfferRoute != in.Source.Route {
		return fail("offer %q is authored under route %q, agent names %q",
			in.Source.Offer, in.OfferRoute, in.Source.Route)
	}
	if !slices.Contains(in.Offer.AllowedEfforts, in.Source.Effort) {
		return fail("effort %q is not allowed by offer %q", in.Source.Effort, in.Source.Offer)
	}
	if !slices.Contains(in.Adapter.SendableEfforts, in.Source.Effort) {
		return fail("effort %q is not sendable by adapter %q", in.Source.Effort, in.Source.Adapter)
	}
	routeDigest, err := in.Route.ComputeDigest()
	if err != nil {
		return AgentDefinition{}, err
	}
	adapterDigest, err := in.Adapter.ComputeDigest()
	if err != nil {
		return AgentDefinition{}, err
	}
	offerDigest, err := in.Offer.ComputeDigest()
	if err != nil {
		return AgentDefinition{}, err
	}
	agent := AgentDefinition{
		Name: in.Source.Name, EncodingVersion: AgentEncodingVersion,
		EnrollmentID: in.Enrollment.ID, RouteDigest: routeDigest,
		AdapterDigest: adapterDigest, OfferDigest: offerDigest,
		Effort: in.Source.Effort,
	}
	digest, err := agent.ComputeDigest()
	if err != nil {
		return AgentDefinition{}, err
	}
	agent.Digest = digest
	if err := agent.Validate(); err != nil {
		return AgentDefinition{}, err
	}
	return agent, nil
}

// ValidateOfferCoversDeadline is admission step 1's expiry leg: an offer
// whose authored not_after precedes the attempt deadline does not resolve.
func ValidateOfferCoversDeadline(offer OfferFragment, deadline time.Time) error {
	if deadline.IsZero() {
		return fmt.Errorf("offer deadline: %w", ErrMissingTimestamp)
	}
	if offer.NotAfter.Before(deadline) {
		return fmt.Errorf("offer not_after %s, attempt deadline %s: %w",
			offer.NotAfter.Format(time.RFC3339Nano), deadline.Format(time.RFC3339Nano), ErrOfferExpired)
	}
	return nil
}

// LineupRoleKeyPrefix namespaces the lineup's ResolvedPolicy keys: one key
// per role naming an agent, one provenance entry per role riding the
// PolicyKey it lands on (§5.4: a lineup is a policy's map of roles to
// agents, and the only standing selection).
const LineupRoleKeyPrefix = "lineup.role."

// LineupSelection is one parsed lineup value: the agent's tree name and the
// exact digest the lineup binds for the role. The digest is what admission
// step 2 matches; the name is how a human reads the diff.
type LineupSelection struct {
	AgentName   string
	AgentDigest Digest
}

// LineupRoleKey returns the policy key for a role. Only canonical stage
// names mint keys: newly authored lineups never use a legacy engine spelling.
func LineupRoleKey(role StageName) (string, error) {
	if !role.valid() {
		return "", fmt.Errorf("lineup role %q: %w", role, ErrInvalidStageName)
	}
	return LineupRoleKeyPrefix + string(role), nil
}

// ParseLineupSelection parses a lineup key's value, "<agent-name>@<digest>".
// The digest half must be a real content address — a value naming only an
// agent, or carrying a name where the digest belongs, is refused, because a
// lineup that does not pin the digest could follow a tree edit nobody
// approved for the role.
func ParseLineupSelection(value string) (LineupSelection, error) {
	name, digest, found := strings.Cut(value, "@")
	if !found || name == "" {
		return LineupSelection{}, fmt.Errorf("lineup selection %q: %w", value, ErrInvalidLineupKey)
	}
	if !validAgentName(name) {
		return LineupSelection{}, fmt.Errorf("lineup selection %q name %q: %w", value, name, ErrInvalidAgentName)
	}
	if !contentaddr.Valid(digest) {
		return LineupSelection{}, fmt.Errorf("lineup selection %q digest: %w", value, ErrInvalidLineupKey)
	}
	return LineupSelection{AgentName: name, AgentDigest: Digest(digest)}, nil
}

func validAgentName(name string) bool {
	if name == "" || len(name) > maxAgentNameBytes {
		return false
	}
	for i := range len(name) {
		char := name[i]
		if ('a' <= char && char <= 'z') || ('0' <= char && char <= '9') {
			continue
		}
		if 0 < i && i < len(name)-1 && (char == '-' || char == '_') {
			continue
		}
		return false
	}
	return true
}

// ValidateLineupPolicyKeys is the namespaced key validator applied at policy
// resolution and approval, scoped to the keys this contract adds (no global
// policy-key registry): every key under lineup.role. must name a canonical
// stage role — legacy engine spellings are read through the stage-role
// resolver, never authored anew — and carry a parseable "<name>@<digest>"
// selection. Keys outside the namespace pass through untouched, so submit's
// free-form policy keys keep working.
func ValidateLineupPolicyKeys(keys []PolicyKey) error {
	seen := map[StageName]string{}
	for _, key := range keys {
		if !strings.HasPrefix(key.Key, LineupRoleKeyPrefix) {
			continue
		}
		role := canonicalStageName(strings.TrimPrefix(key.Key, LineupRoleKeyPrefix))
		if !role.valid() {
			return fmt.Errorf("lineup key %q role: %w", key.Key, ErrInvalidStageName)
		}
		if prior, dup := seen[role]; dup {
			return fmt.Errorf("lineup key %q duplicates %q: %w", key.Key, prior, ErrDuplicate)
		}
		seen[role] = key.Key
		if _, err := ParseLineupSelection(key.Value); err != nil {
			return fmt.Errorf("lineup key %q: %w", key.Key, err)
		}
	}
	return nil
}

// CanonicalStageRole maps a persisted stage name onto the canonical
// StageName vocabulary: canonical names map to themselves, and the exhaustive
// set of legacy engine spellings — exactly "implement", the single Phase 1A.2
// production stage — maps to its canonical member. Persisted rows are
// preserved byte-for-byte; this resolver changes how a row is read where the
// lineup keys resolve per role, never what is stored. An unknown name
// resolves to nothing and the caller fails closed.
func CanonicalStageRole(name string) (StageName, error) {
	if canonical := StageName(name); canonical.valid() {
		return canonical, nil
	}
	switch name {
	case legacySpecificationStageName:
		return StageNameSpecification, nil
	case "implement":
		return StageNameImplementation, nil
	}
	return "", fmt.Errorf("stage name %q: %w", name, ErrUnknownStageRole)
}

// TreatmentEncodingVersion tags the treatment digest's canonical
// serialization.
const TreatmentEncodingVersion = 1

// canonicalTreatment is the §5.4/§8 comparison grouping: route behaviour,
// adapter, launch, offer behaviour, and requested plus effective effort.
// Excluded by design: enrollment, generation, cost owner, pricing, terms
// (the route's billing mode and terms basis, the offer's pricing revision),
// deprecation (the offer's not_after), and labels. Two runs with one
// treatment digest differ only in things that should not change behaviour;
// the agent digest stays the audit key.
type canonicalTreatment struct {
	EncodingVersion int `json:"encoding_version"`
	Route           struct {
		ServiceOperator      string   `json:"service_operator"`
		Protocol             string   `json:"protocol"`
		InferenceAuthorities []string `json:"inference_authorities"`
		FallbackPolicy       string   `json:"fallback_policy"`
	} `json:"route"`
	AdapterDigest Digest `json:"adapter_digest"`
	LaunchDigest  Digest `json:"launch_digest"`
	Offer         struct {
		RouteModelID      string            `json:"route_model_id"`
		LineageGroup      string            `json:"lineage_group"`
		IdentityStability IdentityStability `json:"identity_stability"`
	} `json:"offer"`
	RequestedEffort EffortLevel `json:"requested_effort"`
	// EffectiveEffort is the harness-native value the adapter actually sent,
	// so a clamp (max → xhigh) lands in the grouping explicitly.
	EffectiveEffort string `json:"effective_effort"`
}

// ComputeTreatmentDigest returns the versioned treatment digest for one run's
// admitted configuration and observed effective effort.
func ComputeTreatmentDigest(
	route RouteFragment, adapterDigest, launchDigest Digest,
	offer OfferFragment, requestedEffort EffortLevel, effectiveEffort string,
) (Digest, error) {
	if err := route.Validate(); err != nil {
		return "", fmt.Errorf("treatment route: %w", err)
	}
	if err := offer.Validate(); err != nil {
		return "", fmt.Errorf("treatment offer: %w", err)
	}
	if !contentaddr.Valid(string(adapterDigest)) {
		return "", fmt.Errorf("treatment adapter_digest %q: %w", adapterDigest, ErrInvalidDigest)
	}
	if !contentaddr.Valid(string(launchDigest)) {
		return "", fmt.Errorf("treatment launch_digest %q: %w", launchDigest, ErrInvalidDigest)
	}
	if !requestedEffort.valid() {
		return "", fmt.Errorf("treatment requested_effort %q: %w", requestedEffort, ErrInvalidEffortLevel)
	}
	if effectiveEffort == "" {
		return "", fmt.Errorf("treatment effective_effort: %w", ErrEmptyField)
	}
	treatment := canonicalTreatment{
		EncodingVersion: TreatmentEncodingVersion,
		AdapterDigest:   adapterDigest, LaunchDigest: launchDigest,
		RequestedEffort: requestedEffort, EffectiveEffort: effectiveEffort,
	}
	treatment.Route.ServiceOperator = route.ServiceOperator
	treatment.Route.Protocol = route.Protocol
	treatment.Route.InferenceAuthorities = route.InferenceAuthorities
	treatment.Route.FallbackPolicy = route.FallbackPolicy
	treatment.Offer.RouteModelID = offer.RouteModelID
	treatment.Offer.LineageGroup = offer.LineageGroup
	treatment.Offer.IdentityStability = offer.IdentityStability
	body, err := json.Marshal(treatment)
	if err != nil {
		return "", fmt.Errorf("treatment canonical encoding: %w", err)
	}
	return Digest(contentaddr.Sum(body)), nil
}
