package ward

import "github.com/freeside-ai/freeside/daemon/internal/wardname"

// handoffRoleKind is the runtime class of a handoff role's object. The zero
// value is invalid by design.
type handoffRoleKind string

const (
	handoffContainer handoffRoleKind = "container"
	handoffVolume    handoffRoleKind = "volume"
	handoffNetwork   handoffRoleKind = "network"
)

// AllHandoffRoleKinds is the single registration point for handoffRoleKind.
var AllHandoffRoleKinds = []handoffRoleKind{handoffContainer, handoffVolume, handoffNetwork}

func (k handoffRoleKind) valid() bool {
	switch k {
	case handoffContainer, handoffVolume, handoffNetwork:
		return true
	default:
		return false
	}
}

// handoffFeature is the spec feature whose presence makes a handoff create
// a role's object. The zero value is invalid by design.
type handoffFeature string

const (
	// featureAlways roles exist in every handoff, the conformance suite's
	// synthetic handoffs included.
	featureAlways handoffFeature = "always"
	// featureAuthStoreLease roles exist when the spec leases a writable
	// credential store (HandoffSpec.AuthStoreLease).
	featureAuthStoreLease handoffFeature = "auth_store_lease"
	// featureClaudeClean roles exist for the Claude clean launch state.
	featureClaudeClean handoffFeature = "claude_clean_launch_state"
	// featurePromptFile roles exist when the prompt rides as a file.
	featurePromptFile handoffFeature = "prompt_file"
	// featureOperatorInputs roles exist when the spec carries operator
	// input files.
	featureOperatorInputs handoffFeature = "operator_inputs"
)

// AllHandoffFeatures is the single registration point for handoffFeature.
var AllHandoffFeatures = []handoffFeature{
	featureAlways, featureAuthStoreLease, featureClaudeClean, featurePromptFile, featureOperatorInputs,
}

func (f handoffFeature) valid() bool {
	switch f {
	case featureAlways, featureAuthStoreLease, featureClaudeClean, featurePromptFile, featureOperatorInputs:
		return true
	default:
		return false
	}
}

// handoffRole is one row of handoffRoles: a deterministic runtime object one
// handoff run may create.
type handoffRole struct {
	// role is the wardname suffix that names the object.
	role    string
	kind    handoffRoleKind
	feature handoffFeature
	// recoveryReap is the role's 1-based place in recovery's adoption reap
	// order, or 0 when adoption does not reap it.
	recoveryReap int
	name         func(*handoffNames) *string
	claim        func(*runState) *objectClaim
}

// handoffRoles is the one list of handoff roles: namesFor, the runtime
// namespace functions, teardown, and recovery all read it, so a role added
// here is named, bound, reaped, and recovered together.
//
// Row order is teardown's reap order: containers (prompt and inputs helpers
// first, the exporter last), then volumes, then the network.
//
// recoveryReap is a second order, recovery's adoption reap over the
// containers a dead process may have left standing. The exporter, the
// post-writer credential observer, and the writer observer come first: their
// pre-execution inspections died with the process, so they are reaped and
// rerun, never trusted. The pre-writer roles follow, and the pre-writer
// credential observer comes last. The agent has no place: adoption proves it
// absent instead of reaping it. Each reap is independent and the
// token-stray check runs only after all of them, so this order is kept as
// recovery has always run it, not because a different order is unsafe.
var handoffRoles = []handoffRole{
	{
		role: wardname.PromptSeed, kind: handoffContainer, feature: featurePromptFile, recoveryReap: 12,
		name:  func(n *handoffNames) *string { return &n.PromptSeeder },
		claim: func(s *runState) *objectClaim { return &s.promptSeeder },
	},
	{
		role: wardname.PromptCheck, kind: handoffContainer, feature: featurePromptFile, recoveryReap: 13,
		name:  func(n *handoffNames) *string { return &n.PromptObserver },
		claim: func(s *runState) *objectClaim { return &s.promptObserver },
	},
	{
		role: wardname.InputsSeed, kind: handoffContainer, feature: featureOperatorInputs, recoveryReap: 14,
		name:  func(n *handoffNames) *string { return &n.InputsSeeder },
		claim: func(s *runState) *objectClaim { return &s.inputsSeeder },
	},
	{
		role: wardname.InputsCheck, kind: handoffContainer, feature: featureOperatorInputs, recoveryReap: 15,
		name:  func(n *handoffNames) *string { return &n.InputsObserver },
		claim: func(s *runState) *objectClaim { return &s.inputsObserver },
	},
	{
		role: wardname.Seeder, kind: handoffContainer, feature: featureAlways, recoveryReap: 4,
		name:  func(n *handoffNames) *string { return &n.Seeder },
		claim: func(s *runState) *objectClaim { return &s.seeder },
	},
	{
		role: wardname.Observer, kind: handoffContainer, feature: featureAlways, recoveryReap: 5,
		name:  func(n *handoffNames) *string { return &n.Observer },
		claim: func(s *runState) *objectClaim { return &s.observer },
	},
	{
		role: wardname.InsSeed, kind: handoffContainer, feature: featureAlways, recoveryReap: 6,
		name:  func(n *handoffNames) *string { return &n.InstructionSeeder },
		claim: func(s *runState) *objectClaim { return &s.instructionSeeder },
	},
	{
		role: wardname.InsCheck, kind: handoffContainer, feature: featureAlways, recoveryReap: 7,
		name:  func(n *handoffNames) *string { return &n.InstructionObserver },
		claim: func(s *runState) *objectClaim { return &s.instructionObserver },
	},
	{
		role: wardname.CfgSeed, kind: handoffContainer, feature: featureClaudeClean, recoveryReap: 8,
		name:  func(n *handoffNames) *string { return &n.ConfigRootSeeder },
		claim: func(s *runState) *objectClaim { return &s.configRootSeeder },
	},
	{
		role: wardname.CfgCheck, kind: handoffContainer, feature: featureClaudeClean, recoveryReap: 9,
		name:  func(n *handoffNames) *string { return &n.ConfigRootObserver },
		claim: func(s *runState) *objectClaim { return &s.configRootObserver },
	},
	{
		role: wardname.ProjectsCheck, kind: handoffContainer, feature: featureClaudeClean, recoveryReap: 10,
		name:  func(n *handoffNames) *string { return &n.ContinuityObserver },
		claim: func(s *runState) *objectClaim { return &s.continuityObserver },
	},
	{
		role: wardname.SessCheck, kind: handoffContainer, feature: featureClaudeClean, recoveryReap: 11,
		name:  func(n *handoffNames) *string { return &n.ScratchObserver },
		claim: func(s *runState) *objectClaim { return &s.scratchObserver },
	},
	{
		role: wardname.WriterCheck, kind: handoffContainer, feature: featureAlways, recoveryReap: 3,
		name:  func(n *handoffNames) *string { return &n.WriterObserver },
		claim: func(s *runState) *objectClaim { return &s.writerObserver },
	},
	{
		role: wardname.CredPre, kind: handoffContainer, feature: featureAuthStoreLease, recoveryReap: 16,
		name:  func(n *handoffNames) *string { return &n.CredObsPre },
		claim: func(s *runState) *objectClaim { return &s.credObsPre },
	},
	{
		role: wardname.CredPost, kind: handoffContainer, feature: featureAuthStoreLease, recoveryReap: 2,
		name:  func(n *handoffNames) *string { return &n.CredObsPost },
		claim: func(s *runState) *objectClaim { return &s.credObsPost },
	},
	{
		role: wardname.Agent, kind: handoffContainer, feature: featureAlways,
		name:  func(n *handoffNames) *string { return &n.Agent },
		claim: func(s *runState) *objectClaim { return &s.agent },
	},
	{
		role: wardname.Exporter, kind: handoffContainer, feature: featureAlways, recoveryReap: 1,
		name:  func(n *handoffNames) *string { return &n.Exporter },
		claim: func(s *runState) *objectClaim { return &s.exporter },
	},
	{
		role: wardname.Prompt, kind: handoffVolume, feature: featurePromptFile,
		name:  func(n *handoffNames) *string { return &n.Prompt },
		claim: func(s *runState) *objectClaim { return &s.prompt },
	},
	{
		role: wardname.Inputs, kind: handoffVolume, feature: featureOperatorInputs,
		name:  func(n *handoffNames) *string { return &n.Inputs },
		claim: func(s *runState) *objectClaim { return &s.inputs },
	},
	{
		role: wardname.Workspace, kind: handoffVolume, feature: featureAlways,
		name:  func(n *handoffNames) *string { return &n.Workspace },
		claim: func(s *runState) *objectClaim { return &s.workspace },
	},
	{
		role: wardname.Ins, kind: handoffVolume, feature: featureAlways,
		name:  func(n *handoffNames) *string { return &n.Instructions },
		claim: func(s *runState) *objectClaim { return &s.instructions },
	},
	{
		role: wardname.Cfg, kind: handoffVolume, feature: featureClaudeClean,
		name:  func(n *handoffNames) *string { return &n.ConfigRoot },
		claim: func(s *runState) *objectClaim { return &s.configRoot },
	},
	{
		role: wardname.Projects, kind: handoffVolume, feature: featureClaudeClean,
		name:  func(n *handoffNames) *string { return &n.Continuity },
		claim: func(s *runState) *objectClaim { return &s.continuity },
	},
	{
		role: wardname.SessionEnv, kind: handoffVolume, feature: featureClaudeClean,
		name:  func(n *handoffNames) *string { return &n.SessionScratch },
		claim: func(s *runState) *objectClaim { return &s.sessionScratch },
	},
	{
		role: wardname.Egress, kind: handoffNetwork, feature: featureAlways,
		name:  func(n *handoffNames) *string { return &n.Network },
		claim: func(s *runState) *objectClaim { return &s.network },
	},
}

// appendRuntimeNames appends each row's name for runID to its class in
// resources.
func appendRuntimeNames(resources RuntimeResourceNames, names handoffNames, rows []handoffRole) RuntimeResourceNames {
	for _, r := range rows {
		name := *r.name(&names)
		switch r.kind {
		case handoffContainer:
			resources.Containers = append(resources.Containers, name)
		case handoffVolume:
			resources.Volumes = append(resources.Volumes, name)
		case handoffNetwork:
			resources.Networks = append(resources.Networks, name)
		}
	}
	return resources
}
