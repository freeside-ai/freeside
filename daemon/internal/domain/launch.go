package domain

import (
	"encoding/json"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// The stage owns the launch (plan §5.4): specification, implementation, and
// review each define what they hand the harness — writer or read-only, the
// output contract, severance, session mode, and an auxiliary-inference
// policy. The adapter maps the launch to harness-native controls or declares
// it cannot, so any stage runs on any adapter whose proved capabilities cover
// its launch, and an agent carries no role. An agent narrows behaviour inside
// a stage and never waives or widens the stage's floors.

// LaunchEncodingVersion tags the launch's canonical serialization.
const LaunchEncodingVersion = 1

// LaunchSpec is one stage's launch definition, digest-addressed so the
// admission snapshot and the treatment digest can bind to the exact launch a
// run was given.
type LaunchSpec struct {
	EncodingVersion int       `json:"encoding_version"`
	Stage           StageName `json:"stage"`
	// Writer grants the mutation toolset; false is a read-only workspace
	// (review's floor).
	Writer bool `json:"writer"`
	// OutputContract identifies the stage's output contract — what the
	// harness's final output is validated against. It is an identifier the
	// owning stage defines, not a digest, because the contract's enforcement
	// lives with the stage; the launch only pins which one applies.
	OutputContract string `json:"output_contract"`
	// Severance requires the harness to run severed from ambient context:
	// no user-level config, memory, or extensions (review's fresh-context
	// floor rides this).
	Severance          bool                     `json:"severance"`
	SessionMode        SessionMode              `json:"session_mode"`
	AuxiliaryInference AuxiliaryInferencePolicy `json:"auxiliary_inference"`
	Digest             Digest                   `json:"digest"`
}

type canonicalLaunchSpec struct {
	EncodingVersion    int                      `json:"encoding_version"`
	Stage              StageName                `json:"stage"`
	Writer             bool                     `json:"writer"`
	OutputContract     string                   `json:"output_contract"`
	Severance          bool                     `json:"severance"`
	SessionMode        SessionMode              `json:"session_mode"`
	AuxiliaryInference AuxiliaryInferencePolicy `json:"auxiliary_inference"`
}

func (l LaunchSpec) canonical() canonicalLaunchSpec {
	return canonicalLaunchSpec{
		EncodingVersion: l.EncodingVersion, Stage: l.Stage, Writer: l.Writer,
		OutputContract: l.OutputContract, Severance: l.Severance,
		SessionMode: l.SessionMode, AuxiliaryInference: l.AuxiliaryInference,
	}
}

// ComputeDigest hashes the explicit-version canonical encoding. Struct field
// order is part of the contract and is pinned by a golden.
func (l LaunchSpec) ComputeDigest() (Digest, error) {
	body, err := json.Marshal(l.canonical())
	if err != nil {
		return "", fmt.Errorf("launch spec canonical encoding: %w", err)
	}
	return Digest(contentaddr.Sum(body)), nil
}

// Validate reports whether the launch is well-formed and its digest
// authentic.
func (l LaunchSpec) Validate() error {
	if l.EncodingVersion != LaunchEncodingVersion {
		return fmt.Errorf("launch spec encoding_version %d: %w", l.EncodingVersion, ErrAgentEncodingVersion)
	}
	if !l.Stage.valid() {
		return fmt.Errorf("launch spec stage %q: %w", l.Stage, ErrInvalidStageName)
	}
	if l.OutputContract == "" {
		return fmt.Errorf("launch spec output_contract: %w", ErrEmptyField)
	}
	if !l.SessionMode.valid() {
		return fmt.Errorf("launch spec session_mode %q: %w", l.SessionMode, ErrInvalidSessionMode)
	}
	if !l.AuxiliaryInference.valid() {
		return fmt.Errorf("launch spec auxiliary_inference %q: %w",
			l.AuxiliaryInference, ErrInvalidAuxiliaryInference)
	}
	if !contentaddr.Valid(string(l.Digest)) {
		return fmt.Errorf("launch spec digest %q: %w", l.Digest, ErrInvalidDigest)
	}
	computed, err := l.ComputeDigest()
	if err != nil {
		return err
	}
	if l.Digest != computed {
		return fmt.Errorf("launch spec digest %q, content resolves to %q: %w",
			l.Digest, computed, ErrAgentDigestMismatch)
	}
	return nil
}

// Encode emits the validated canonical persisted form.
func (l LaunchSpec) Encode() ([]byte, error) {
	if err := l.Validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(l)
	if err != nil {
		return nil, fmt.Errorf("launch spec encode: %w", err)
	}
	return body, nil
}

// DecodeLaunchSpec rejects oversized, unknown-field, invalid-UTF8, and
// trailing-data payloads before revalidating the content address.
func DecodeLaunchSpec(body []byte) (LaunchSpec, error) {
	var l LaunchSpec
	if err := strictjson.Decode(body, &l, strictjson.RejectInvalidUTF8, MaxAgentFragmentBytes); err != nil {
		return LaunchSpec{}, fmt.Errorf("launch spec decode: %w", err)
	}
	if err := l.Validate(); err != nil {
		return LaunchSpec{}, err
	}
	return l, nil
}

// RequiredCapabilities derives the launch-capability floor this launch puts
// on an adapter: the capabilities admission step 3 checks against the
// adapter build's proved set. Instruction delivery and the per-route store
// contract are required of every launch — every stage delivers instructions
// and mounts a sanitized store; the rest follow the launch's own knobs. An
// observed auxiliary-inference policy requires no control, which is exactly
// how the Claude baseline runs while its harness cannot honour a stricter
// policy (§5.4).
func (l LaunchSpec) RequiredCapabilities() LaunchCapabilitySet {
	required := []LaunchCapability{
		LaunchCapInstructionDelivery,
		LaunchCapRouteStoreContract,
		LaunchCapStructuredOutput,
	}
	if l.Writer {
		required = append(required, LaunchCapMutationTools)
	} else {
		required = append(required, LaunchCapReadTools)
	}
	if l.Severance {
		required = append(required, LaunchCapContextSeverance)
	}
	switch l.SessionMode {
	case SessionOneShot:
		// A one-shot launch needs no resume fidelity.
	case SessionResumed:
		required = append(required, LaunchCapExactResume)
	}
	switch l.AuxiliaryInference {
	case AuxiliaryForbidden, AuxiliaryDeclared:
		required = append(required, LaunchCapAuxiliaryInferenceControl)
	case AuxiliaryObserved:
		// Observation asks nothing of the harness.
	}
	return NewLaunchCapabilitySet(required...)
}

// SessionMode is how the harness session is driven: one shot per invocation,
// or resumed across invocations (which demands exact resume fidelity from
// the adapter).
type SessionMode string

const (
	SessionOneShot SessionMode = "one_shot"
	SessionResumed SessionMode = "resumed"
)

// AllSessionModes lists every valid SessionMode.
var AllSessionModes = []SessionMode{SessionOneShot, SessionResumed}

func (m SessionMode) valid() bool {
	switch m {
	case SessionOneShot, SessionResumed:
		return true
	default:
		return false
	}
}

// AuxiliaryInferencePolicy is the launch's stance on the harness's own
// auxiliary inference (§5.4): forbidden, declared (permitted but announced),
// or observed (recorded from the outside). Review and experiment arms
// require forbidden; the Claude baseline runs observed.
type AuxiliaryInferencePolicy string

const (
	AuxiliaryForbidden AuxiliaryInferencePolicy = "forbidden"
	AuxiliaryDeclared  AuxiliaryInferencePolicy = "declared"
	AuxiliaryObserved  AuxiliaryInferencePolicy = "observed"
)

// AllAuxiliaryInferencePolicies lists every valid AuxiliaryInferencePolicy.
var AllAuxiliaryInferencePolicies = []AuxiliaryInferencePolicy{
	AuxiliaryForbidden, AuxiliaryDeclared, AuxiliaryObserved,
}

func (p AuxiliaryInferencePolicy) valid() bool {
	switch p {
	case AuxiliaryForbidden, AuxiliaryDeclared, AuxiliaryObserved:
		return true
	default:
		return false
	}
}
