package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// A selection is what admission resolves, hashes, and records for an agent
// (plan §5.4, revision 111; devlog/2026-11-05-1035-select-by-enrollment-
// model-effort.md): an enrollment, a model the enrollment's harness offers,
// an effort that model reports, and the adapter the launch shape picks. The
// route is not part of it, because the enrollment fixes the route; no name
// is part of it either. The agent document stays beside it until the tree
// format drops it (#2599): a tree agent reduces to the selection it stands
// for (ReduceAgentSelection), and admission refuses an agent whose
// reduction is not the selection resolution produces.
//
// This is unrelated to engine.AgentSelection, the engine's lineup-admission
// configuration, and to AgentSelectionSource, which records which record
// chose the agent.

// AgentSelectionEncodingVersion tags the selection's canonical serialization.
const AgentSelectionEncodingVersion = 1

// maxLaunchOptionBytes bounds a model or effort a launch passes as one
// option value. It matches the bound inference.ValidLaunchValue holds an
// observed identifier to, since a model comes back as one.
const maxLaunchOptionBytes = 256

// AgentSelection is the resolved, digest-addressed tuple.
type AgentSelection struct {
	EncodingVersion int                `json:"encoding_version"`
	EnrollmentID    ClientEnrollmentID `json:"enrollment_id"`
	// ModelID is a model id from the enrollment's discovered catalog (an
	// entry's id or the model it resolves to), or
	// ClaudeCodeNativeDefaultRouteModelID, which a claude_code launch passes
	// as no model.
	ModelID string `json:"model_id"`
	// Effort is the harness's own effort string, not an EffortLevel: a
	// model's catalog entry may report levels that enum lacks (xhigh), and
	// the set is per model. EffortHarnessDefault keeps its name and means
	// the launch passes no effort.
	Effort        string `json:"effort"`
	AdapterDigest Digest `json:"adapter_digest"`
	Digest        Digest `json:"digest"`
}

// canonicalAgentSelection is the body the selection digest addresses, in
// contract order. A distinct type so adding a field without deciding whether
// it enters the identity is a compile-visible choice.
type canonicalAgentSelection struct {
	EncodingVersion int                `json:"encoding_version"`
	EnrollmentID    ClientEnrollmentID `json:"enrollment_id"`
	ModelID         string             `json:"model_id"`
	Effort          string             `json:"effort"`
	AdapterDigest   Digest             `json:"adapter_digest"`
}

// ComputeDigest hashes the canonical body. Field order is part of the
// contract and is pinned by a golden.
func (s AgentSelection) ComputeDigest() (Digest, error) {
	body, err := json.Marshal(canonicalAgentSelection{
		EncodingVersion: s.EncodingVersion, EnrollmentID: s.EnrollmentID,
		ModelID: s.ModelID, Effort: s.Effort, AdapterDigest: s.AdapterDigest,
	})
	if err != nil {
		return "", fmt.Errorf("agent selection canonical encoding: %w", err)
	}
	return Digest(contentaddr.Sum(body)), nil
}

// Validate reports whether the selection is well-formed and its digest
// authentic. Whether its model and effort are ones the enrollment offers is
// ResolveAgentSelection's question.
func (s AgentSelection) Validate() error {
	if s.EncodingVersion != AgentSelectionEncodingVersion {
		return fmt.Errorf("agent selection encoding_version %d: %w", s.EncodingVersion, ErrAgentEncodingVersion)
	}
	if s.EnrollmentID == "" {
		return fmt.Errorf("agent selection enrollment_id: %w", ErrEmptyID)
	}
	if s.ModelID == "" {
		return fmt.Errorf("agent selection model_id: %w", ErrEmptyField)
	}
	if s.Effort == "" {
		return fmt.Errorf("agent selection effort: %w", ErrEmptyField)
	}
	if !contentaddr.Valid(string(s.AdapterDigest)) {
		return fmt.Errorf("agent selection adapter_digest %q: %w", s.AdapterDigest, ErrInvalidDigest)
	}
	if !contentaddr.Valid(string(s.Digest)) {
		return fmt.Errorf("agent selection digest %q: %w", s.Digest, ErrInvalidDigest)
	}
	computed, err := s.ComputeDigest()
	if err != nil {
		return err
	}
	if s.Digest != computed {
		return fmt.Errorf("agent selection digest %q, content resolves to %q: %w",
			s.Digest, computed, ErrAgentDigestMismatch)
	}
	return nil
}

// LaunchValues is what a launch under the selection passes for a client: the
// model, empty for a claude_code launch at the reserved native-default id,
// and the native effort, empty at harness_default. They are the binding's
// route_model_id and native_effort.
func (s AgentSelection) LaunchValues(client HarnessClientKind) (model, nativeEffort string) {
	model = s.ModelID
	if client == HarnessClientClaudeCode {
		model = ClaudeCodeLaunchModel(s.ModelID)
	}
	if s.Effort != string(EffortHarnessDefault) {
		nativeEffort = s.Effort
	}
	return model, nativeEffort
}

func sealAgentSelection(s AgentSelection) (AgentSelection, error) {
	digest, err := s.ComputeDigest()
	if err != nil {
		return AgentSelection{}, err
	}
	s.Digest = digest
	if err := s.Validate(); err != nil {
		return AgentSelection{}, err
	}
	return s, nil
}

// AgentSelectionNames is the operator-facing form a selection resolves
// from: the enrollment's id, the model id, and the effort string.
type AgentSelectionNames struct {
	Enrollment string
	Model      string
	Effort     string
}

// Validate reports whether every name is present.
func (n AgentSelectionNames) Validate() error {
	for field, value := range map[string]string{
		"enrollment": n.Enrollment, "model": n.Model, "effort": n.Effort,
	} {
		if value == "" {
			return fmt.Errorf("agent selection %s: %w", field, ErrEmptyField)
		}
	}
	return nil
}

// ValidLaunchOptionValue reports whether a launch can pass value as one
// option: graphic characters with no whitespace, no leading dash a CLI would
// parse as another option, and at most maxLaunchOptionBytes. It is the rule
// inference.ValidLaunchValue applies to the call launch, which keeps its own
// copy because that package's drivers check requests the domain never sees.
func ValidLaunchOptionValue(value string) bool {
	return len(value) <= maxLaunchOptionBytes && !strings.HasPrefix(value, "-") && utf8.ValidString(value) &&
		!strings.ContainsFunc(value, func(r rune) bool { return unicode.IsSpace(r) || !unicode.IsGraphic(r) })
}

// AgentSelectionInput is what a selection resolves against: the names, the
// enrollment they name and its identity as the store holds them now, the
// adapter the launch shape picked (SelectAdapterForLaunch), and the
// enrollment's discovered catalog, nil when none is recorded.
type AgentSelectionInput struct {
	Names      AgentSelectionNames
	Identity   AuthIdentity
	Enrollment ClientEnrollment
	Adapter    AdapterFragment
	Catalog    *DiscoveredModelCatalog
}

// ResolveAgentSelection builds and validates the selection the names stand
// for. Each refusal wraps ErrAgentJoinInvalid and names its leg: a disabled
// identity, an enrollment other than the one named, an adapter for another
// client kind, a model or effort a launch could not pass as one option, a
// model the catalog lacks (by id, or by the model an entry resolves to), or
// an effort the model does not report. Errors quote the operator's names,
// never the catalog's harness-reported strings.
//
// The reserved ClaudeCodeNativeDefaultRouteModelID names no catalog model:
// it is accepted only for a claude_code enrollment, and then with an effort
// the claude_code native table produces.
//
// Bridge: an enrollment with no recorded catalog (every Codex enrollment
// until #2595 records one, and a Claude enrollment before its first
// successful discovery) accepts the model as named and an effort the
// client's native table produces. A selection resolved under the bridge is
// only as checked as the tree agent it was reduced from. The bridge ends
// when a recorded catalog becomes mandatory for admission, which is a later
// unit's decision.
func ResolveAgentSelection(in AgentSelectionInput) (AgentSelection, error) {
	if err := in.Names.Validate(); err != nil {
		return AgentSelection{}, err
	}
	fail := func(format string, args ...any) (AgentSelection, error) {
		return AgentSelection{}, fmt.Errorf("agent selection %s/%s/%s: %s: %w",
			in.Names.Enrollment, in.Names.Model, in.Names.Effort, fmt.Sprintf(format, args...), ErrAgentJoinInvalid)
	}
	if err := in.Identity.Validate(); err != nil {
		return AgentSelection{}, fmt.Errorf("agent selection identity: %w", err)
	}
	if err := in.Enrollment.Validate(); err != nil {
		return AgentSelection{}, fmt.Errorf("agent selection enrollment: %w", err)
	}
	if err := ValidateEnrollmentIdentityBinding(in.Identity, in.Enrollment); err != nil {
		return AgentSelection{}, fmt.Errorf("agent selection: %w", err)
	}
	if err := in.Adapter.Validate(); err != nil {
		return AgentSelection{}, fmt.Errorf("agent selection adapter: %w", err)
	}
	if !in.Identity.Enabled {
		return fail("identity %s is disabled", in.Identity.ID)
	}
	if string(in.Enrollment.ID) != in.Names.Enrollment {
		return fail("was given enrollment %s", in.Enrollment.ID)
	}
	client := in.Enrollment.HarnessClient
	if client != in.Adapter.ClientKind {
		return fail("enrollment binds client %q, adapter drives %q", client, in.Adapter.ClientKind)
	}
	if !ValidLaunchOptionValue(in.Names.Model) {
		return fail("the model is not a value a launch can pass as one option")
	}
	if !ValidLaunchOptionValue(in.Names.Effort) {
		return fail("the effort is not a value a launch can pass as one option")
	}
	if in.Catalog != nil {
		if err := in.Catalog.Validate(); err != nil {
			return AgentSelection{}, fmt.Errorf("agent selection catalog: %w", err)
		}
		if in.Catalog.EnrollmentID != in.Enrollment.ID {
			return fail("the catalog is enrollment %s's", in.Catalog.EnrollmentID)
		}
	}
	switch {
	case in.Names.Model == ClaudeCodeNativeDefaultRouteModelID:
		if client != HarnessClientClaudeCode {
			return fail("the native-default model id is reserved for claude_code, the enrollment binds %q", client)
		}
		if !slices.Contains(nativeEfforts(client), in.Names.Effort) {
			return fail("the effort has no claude_code native value")
		}
	case in.Catalog == nil:
		// The bridge: the model as named, an effort the table produces.
		if !slices.Contains(nativeEfforts(client), in.Names.Effort) {
			return fail("enrollment %s records no catalog, and the effort has no %s native value",
				in.Enrollment.ID, client)
		}
	default:
		switch err := in.Catalog.Serves(in.Names.Model, in.Names.Effort); {
		case errors.Is(err, ErrDiscoveredModelUnserved):
			return fail("the model is not in enrollment %s's discovered catalog", in.Enrollment.ID)
		case err != nil:
			return fail("the effort is not one the model reports in enrollment %s's discovered catalog",
				in.Enrollment.ID)
		}
	}
	return sealAgentSelection(AgentSelection{
		EncodingVersion: AgentSelectionEncodingVersion, EnrollmentID: in.Enrollment.ID,
		ModelID: in.Names.Model, Effort: in.Names.Effort, AdapterDigest: in.Adapter.Digest,
	})
}

// catalogEntries is the catalog entry whose id is model or, when none is,
// every entry that resolves to model. The Claude CLI lists a current model
// under an alias ("opus", resolving to "claude-opus-5-5") and not under its
// full id, which its model option also takes and which offers name, so the
// full id must find the alias's entry or every such offer would refuse once
// a catalog is recorded.
func catalogEntries(catalog DiscoveredModelCatalog, model string) []DiscoveredModel {
	if index := slices.IndexFunc(catalog.Models, func(m DiscoveredModel) bool { return m.ID == model }); index >= 0 {
		return catalog.Models[index : index+1]
	}
	var resolved []DiscoveredModel
	for _, m := range catalog.Models {
		if m.ResolvedModel != nil && *m.ResolvedModel == model {
			resolved = append(resolved, m)
		}
	}
	return resolved
}

// nativeEfforts is every effort string the client's native table produces,
// harness_default included: the bridge's effort set.
func nativeEfforts(client HarnessClientKind) []string {
	var efforts []string
	for _, level := range AllEffortLevels {
		if translation, err := TranslateEffort(client, level); err == nil {
			efforts = append(efforts, translation.Effective())
		}
	}
	return efforts
}

// ReduceAgentSelection is the selection a resolved agent document stands
// for: its enrollment, its offer's route model id, the effort its adapter
// translates to (so a Codex max reduces to xhigh), and its adapter. The
// adapter and offer must be the ones the agent pins.
func ReduceAgentSelection(agent AgentDefinition, adapter AdapterFragment, offer OfferFragment) (AgentSelection, error) {
	if agent.AdapterDigest != adapter.Digest || agent.OfferDigest != offer.Digest {
		return AgentSelection{}, fmt.Errorf(
			"agent %s selection: adapter or offer is not the one the agent pins: %w", agent.Name, ErrAgentJoinInvalid)
	}
	effort, err := TranslateEffort(adapter.ClientKind, agent.Effort)
	if err != nil {
		return AgentSelection{}, fmt.Errorf("agent %s selection: %w", agent.Name, err)
	}
	return sealAgentSelection(AgentSelection{
		EncodingVersion: AgentSelectionEncodingVersion, EnrollmentID: agent.EnrollmentID,
		ModelID: offer.RouteModelID, Effort: effort.Effective(), AdapterDigest: adapter.Digest,
	})
}

// SelectAdapterForLaunch picks, among adapters, the one for the client kind
// that can send the selection's native effort and whose declared launch
// capabilities cover what the stage launch requires, less the waived
// capabilities. A review launch waives auxiliary-inference control, which no
// review adapter declares yet (#1980), as review admission does for
// coverage. No covering adapter fails with ErrNoAdapterForLaunch and more
// than one with ErrAmbiguousAdapterForLaunch; the choice is never left to
// tree order. The effort is part of the choice because a tree may keep a
// retired adapter beside its successor (codex_review_v1, which sends only
// harness_default, beside codex_review_v2); the selection's effort tells them
// apart. This is stage shape only: the call launch and the judgment roles
// keep their agent's adapter.
func SelectAdapterForLaunch(
	adapters []AdapterFragment, client HarnessClientKind, launch LaunchSpec, effort string,
	waived ...LaunchCapability,
) (AdapterFragment, error) {
	if err := launch.Validate(); err != nil {
		return AdapterFragment{}, err
	}
	var required []LaunchCapability
	for _, capability := range launch.RequiredCapabilities() {
		if !slices.Contains(waived, capability) {
			required = append(required, capability)
		}
	}
	var covering []AdapterFragment
	for _, adapter := range adapters {
		if err := adapter.Validate(); err != nil {
			return AdapterFragment{}, err
		}
		if adapter.ClientKind == client && sendsEffort(adapter, effort) &&
			len(MissingLaunchCapabilities(adapter.LaunchCapabilities, required)) == 0 {
			covering = append(covering, adapter)
		}
	}
	switch len(covering) {
	case 0:
		return AdapterFragment{}, fmt.Errorf("client %s, stage %s, effort %s: %w",
			client, launch.Stage, effort, ErrNoAdapterForLaunch)
	case 1:
		return covering[0], nil
	}
	return AdapterFragment{}, fmt.Errorf("client %s, stage %s, effort %s: %d adapters: %w",
		client, launch.Stage, effort, len(covering), ErrAmbiguousAdapterForLaunch)
}

// sendsEffort reports whether the adapter can send the native effort: some
// level it declares sendable translates to it.
func sendsEffort(adapter AdapterFragment, effort string) bool {
	return slices.ContainsFunc(adapter.SendableEfforts, func(level EffortLevel) bool {
		translation, err := TranslateEffort(adapter.ClientKind, level)
		return err == nil && translation.Effective() == effort
	})
}

// Lineage groups LineageGroupOf derives. They are the values the tree's
// offers carry, so a binding admitted under a selection records the same
// group one admitted under an agent did.
const (
	LineageGroupAnthropic = "anthropic"
	LineageGroupOpenAI    = "openai"
)

// LineageGroupOf derives a selection's lineage group from the client that
// launches it and the model it names (§5.4: lineage is a recorded fact,
// never a gate). It is conservative: a group is named only when the
// client's vendor owns the model id by its naming (a claude_code launch of a
// "claude-" model, which covers ClaudeCodeNativeDefaultRouteModelID, or a
// codex_cli launch of a "gpt-" or "codex-" model), and anything else, an
// alias included, returns the empty group,
// which CompareLineage reads as unknown, never as a difference.
func LineageGroupOf(client HarnessClientKind, model string) string {
	switch client {
	case HarnessClientClaudeCode:
		if strings.HasPrefix(model, "claude-") {
			return LineageGroupAnthropic
		}
	case HarnessClientCodexCLI:
		if strings.HasPrefix(model, "gpt-") || strings.HasPrefix(model, "codex-") {
			return LineageGroupOpenAI
		}
	}
	return ""
}

// SelectionTreatmentDigest is the treatment digest of a launch under the
// selection: the selection's model, its effort as requested, and the native
// effort the adapter sends as effective, harness_default when it sends
// none. Admission and the closure recheck both compute it here, so the two
// cannot drift.
func SelectionTreatmentDigest(
	route RouteFragment, adapter AdapterFragment, launchDigest Digest, selection AgentSelection,
) (Digest, error) {
	_, nativeEffort := selection.LaunchValues(adapter.ClientKind)
	effective := nativeEffort
	if effective == "" {
		effective = string(EffortHarnessDefault)
	}
	return ComputeTreatmentDigest(route, adapter.Digest, launchDigest, selection.ModelID, selection.Effort, effective)
}
