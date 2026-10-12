package domain

import "fmt"

// ClaudeCodeNativeDefaultRouteModelID is the route model id reserved for the
// baseline Claude offer (§5.4, "The baseline is honest"): a claude_code
// launch under it passes no model, so Claude Code runs its own default.
// Names never enter a digest, so the offer's route_model_id is the only
// place code can recognize the native default; any other id is an explicit
// model the launch passes as written.
const ClaudeCodeNativeDefaultRouteModelID = "claude-code-native-default"

// EffortTranslation is one requested effort and the harness-native value the
// adapter sends for it (§5.4: the run records both, and a clamp shows).
// Native is empty only for EffortHarnessDefault, which sends no effort.
type EffortTranslation struct {
	Requested EffortLevel
	Native    string
}

// Clamped reports whether the harness receives a different value than the
// one requested.
func (t EffortTranslation) Clamped() bool {
	return t.Native != "" && t.Native != string(t.Requested)
}

// Effective is the value the harness runs at, as the treatment digest
// records it: the native value, or harness_default when none is sent.
func (t EffortTranslation) Effective() string {
	if t.Native == "" {
		return string(EffortHarnessDefault)
	}
	return t.Native
}

// String renders a clamp explicitly, "max → xhigh", and an unclamped
// translation as its single value.
func (t EffortTranslation) String() string {
	if t.Clamped() {
		return fmt.Sprintf("%s → %s", t.Requested, t.Native)
	}
	return t.Effective()
}

// TranslateEffort maps a requested effort to the native value the client's
// pinned harness build accepts. A client with no table, or an effort its
// table cannot send, fails with ErrEffortUntranslatable.
func TranslateEffort(client HarnessClientKind, effort EffortLevel) (EffortTranslation, error) {
	switch client {
	case HarnessClientClaudeCode:
		native, ok := claudeCodeNativeEffort(effort)
		if !ok {
			return EffortTranslation{}, fmt.Errorf("claude_code effort %q: %w", effort, ErrEffortUntranslatable)
		}
		return EffortTranslation{Requested: effort, Native: native}, nil
	case HarnessClientCodexCLI:
		native, ok := codexCLINativeEffort(effort)
		if !ok {
			return EffortTranslation{}, fmt.Errorf("codex_cli effort %q: %w", effort, ErrEffortUntranslatable)
		}
		return EffortTranslation{Requested: effort, Native: native}, nil
	}
	return EffortTranslation{}, fmt.Errorf("harness client %q: %w", client, ErrInvalidHarnessClientKind)
}

// claudeCodeNativeEffort is the --effort table for the Claude Code build the
// ward image pins (images/agent-claude/Containerfile CLAUDE_CODE_VERSION),
// which accepts low, medium, high, xhigh, and max: every Freeside level maps
// to itself. Moving the pin to a build with a different value set changes
// this table in the same change.
func claudeCodeNativeEffort(effort EffortLevel) (string, bool) {
	switch effort {
	case EffortHarnessDefault:
		return "", true
	case EffortLow:
		return "low", true
	case EffortMedium:
		return "medium", true
	case EffortHigh:
		return "high", true
	case EffortMax:
		return "max", true
	}
	return "", false
}

// ClaudeCodeSendableEfforts lists the efforts the Claude adapter can send,
// in AllEffortLevels order: the sendable_efforts a baseline Claude adapter
// fragment declares.
func ClaudeCodeSendableEfforts() []EffortLevel {
	var sendable []EffortLevel
	for _, effort := range AllEffortLevels {
		if _, ok := claudeCodeNativeEffort(effort); ok {
			sendable = append(sendable, effort)
		}
	}
	return sendable
}

// codexCLINativeEffort is the model_reasoning_effort table for the Codex
// build the review image pins (images/agent-codex/Containerfile
// CODEX_VERSION). Codex names its levels minimal, low, medium, high, and
// xhigh: it has no max, so max is sent as xhigh, its highest, and the clamp
// is recorded. minimal has no Freeside level. The pinned build passes the
// value to the service unchecked, so a level the service refuses for a
// model fails that launch, not this table.
func codexCLINativeEffort(effort EffortLevel) (string, bool) {
	switch effort {
	case EffortHarnessDefault:
		return "", true
	case EffortLow:
		return "low", true
	case EffortMedium:
		return "medium", true
	case EffortHigh:
		return "high", true
	case EffortMax:
		return "xhigh", true
	}
	return "", false
}

// CodexCLISendableEfforts lists the efforts a Codex adapter can send, in
// AllEffortLevels order.
func CodexCLISendableEfforts() []EffortLevel {
	var sendable []EffortLevel
	for _, effort := range AllEffortLevels {
		if _, ok := codexCLINativeEffort(effort); ok {
			sendable = append(sendable, effort)
		}
	}
	return sendable
}

// ClaudeCodeLaunchModel is the model a claude_code launch passes for an
// offer's route model id: none for the reserved native default, the id
// itself otherwise.
func ClaudeCodeLaunchModel(routeModelID string) string {
	if routeModelID == ClaudeCodeNativeDefaultRouteModelID {
		return ""
	}
	return routeModelID
}

// AgentLaunchSelection is the model and effort an admitted agent's launch
// passes, as the admission's agent binding records them. Every field empty
// means the launch passes neither, which only the Claude native-default
// offer at harness_default derives. A codex_cli agent always names a model.
type AgentLaunchSelection struct {
	RouteModelID    string
	RequestedEffort EffortLevel
	NativeEffort    string
}

// DeriveAgentLaunchSelection is the one derivation of what an agent's launch
// passes, used by the admission resolver to fill the binding and by the
// reconstruction recheck to refuse a binding that disagrees. The adapter and
// offer must be the ones the agent pins. Codex has no native-default id, so
// a codex_cli agent passes its offer's model as written.
func DeriveAgentLaunchSelection(
	agent AgentDefinition, adapter AdapterFragment, offer OfferFragment,
) (AgentLaunchSelection, error) {
	if agent.AdapterDigest != adapter.Digest || agent.OfferDigest != offer.Digest {
		return AgentLaunchSelection{}, fmt.Errorf(
			"agent %s launch selection: adapter or offer is not the one the agent pins: %w",
			agent.Name, ErrAgentJoinInvalid)
	}
	var selection AgentLaunchSelection
	switch adapter.ClientKind {
	case HarnessClientClaudeCode:
		selection.RouteModelID = ClaudeCodeLaunchModel(offer.RouteModelID)
	case HarnessClientCodexCLI:
		selection.RouteModelID = offer.RouteModelID
	}
	effort, err := TranslateEffort(adapter.ClientKind, agent.Effort)
	if err != nil {
		return AgentLaunchSelection{}, fmt.Errorf("agent %s: %w", agent.Name, err)
	}
	if effort.Native != "" {
		selection.RequestedEffort = effort.Requested
		selection.NativeEffort = effort.Native
	}
	return selection, nil
}
