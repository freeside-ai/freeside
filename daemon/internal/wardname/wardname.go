package wardname

import (
	"regexp"
	"strconv"
	"strings"
)

// Name prefixes, one per ward namespace the rig admits.
const (
	HandoffPrefix             = "freeside-handoff-"
	ReviewPrefix              = "freeside-review-"
	ConformancePrefix         = "freeside-ward-conf-"
	PreflightCredentialPrefix = "freeside-preflight-credential-" //nolint:gosec // G101 false positive: a container name prefix, not a credential.
	LineupProbePrefix         = "freeside-lineup-probe-"
)

// Handoff roles. A conformance handoff and a review's workspace preparation
// use a subset of them under their own run-ID shapes.
const (
	Seeder        = "seeder"
	Observer      = "observer"
	InsSeed       = "ins-seed"
	InsCheck      = "ins-check"
	PromptSeed    = "prompt-seed"
	PromptCheck   = "prompt-check"
	InputsSeed    = "inputs-seed"
	InputsCheck   = "inputs-check"
	CfgSeed       = "cfg-seed"
	CfgCheck      = "cfg-check"
	ProjectsCheck = "projects-check"
	SessCheck     = "sess-check"
	WriterCheck   = "writer-check"
	CredPre       = "cred-pre"
	CredPost      = "cred-post"
	Agent         = "agent"
	Exporter      = "exporter"

	Workspace  = "ws"
	Ins        = "ins"
	Prompt     = "prompt"
	Inputs     = "inputs"
	Cfg        = "cfg"
	Projects   = "projects"
	SessionEnv = "session-env"

	// Egress is every namespace's network role.
	Egress = "egress"
)

// Review roles. LegacyWorkspaceObserver and LegacyAgentsObserver are the
// pre-#587 names, admitted only so teardown can reap a persisted intent.
// Inputs is shared with the handoff volumes.
const (
	WorkspaceObs            = "ws-obs"
	LegacyWorkspaceObserver = "workspace-observer"
	AgentsInit              = "agents-init"
	AgentsObs               = "agents-obs"
	LegacyAgentsObserver    = "agents-observer"
	SnapInit                = "snap-init"
	SnapObs                 = "snap-obs"
	InputsInit              = "inputs-init"
	InputsObs               = "inputs-obs"
	Codex                   = "codex"
	Claude                  = "claude"

	Agents = "agents"
	Snap   = "snap"
)

// Conformance probe roles: the pre-job probe and the full suite's probes.
const (
	PreJob     = "prejob"
	Liveness   = "liveness"
	Seed       = "seed"
	Audit      = "audit"
	ExclWriter = "excl-writer"
	ExclSecond = "excl-second"
	NetLive    = "net-live"
	Net        = "net"
	InxLive    = "inx-live"
	Inx        = "inx"
	Cred       = "cred"
	LivenessWS = "liveness-ws"
	ExclWS     = "excl-ws"
	NetLiveWS  = "net-live-ws"
	InxWS      = "inx-ws"
)

// Name is a role's runtime name under prefix and id.
func Name(prefix, id, role string) string { return prefix + id + "-" + role }

// BareName is a runtime name with no role suffix, for a namespace that holds
// one container (the preflight credential observer and the lineup probe).
func BareName(prefix, id string) string { return prefix + id }

// IDShape is a run-ID or owner-slice shape: a literal lead followed by
// exactly HexDigits lowercase hex digits.
type IDShape struct {
	Lead      string
	HexDigits int
}

func (s IDShape) pattern() string {
	return regexp.QuoteMeta(s.Lead) + "[0-9a-f]{" + strconv.Itoa(s.HexDigits) + "}"
}

// The ID shapes the rig admits, by the producer that mints each.
var (
	// productionRunID is exec/claude.RunIDFor's handoff run ID.
	productionRunID = IDShape{Lead: "c", HexDigits: 31}
	// conformanceRunID is the full conformance suite's run ID, minted by the
	// daemon's conformance driver for the suite and its two handoffs.
	conformanceRunID = IDShape{Lead: "conf-", HexDigits: 16}
	// reviewRunID is the production review gate's review run ID.
	reviewRunID = IDShape{Lead: "review-", HexDigits: 24}
	// preJobRunID is ward.PreJobRunIDForInvocation's digest prefix.
	preJobRunID = IDShape{HexDigits: 16}
	// ownerSlice is the first 12 hex digits of a fresh ownership label.
	ownerSlice = IDShape{HexDigits: 12}
)

// Shape is one prefix and run-ID shape with the roles each resource class
// may carry under it. BareContainer admits the container named by prefix and
// ID alone.
type Shape struct {
	Prefix        string
	RunID         IDShape
	Containers    []string
	Volumes       []string
	Networks      []string
	BareContainer bool
}

// RigShapes returns the rig-admitted namespace as a fresh table each call, so
// no caller can widen it for another. Adding a ward role means adding it here.
func RigShapes() []Shape {
	return []Shape{
		{
			Prefix: HandoffPrefix, RunID: productionRunID,
			Containers: []string{
				Seeder, Observer, InsSeed, InsCheck, PromptSeed, PromptCheck,
				InputsSeed, InputsCheck, CfgSeed, CfgCheck, ProjectsCheck,
				SessCheck, WriterCheck, CredPre, CredPost, Agent, Exporter,
			},
			Volumes:  []string{Workspace, Ins, Prompt, Inputs, Cfg, Projects, SessionEnv},
			Networks: []string{Egress},
		},
		{
			Prefix: HandoffPrefix, RunID: conformanceRunID,
			Containers: []string{Seeder, Observer, InsSeed, InsCheck, WriterCheck, Agent, Exporter},
			Volumes:    []string{Workspace, Ins},
			Networks:   []string{Egress},
		},
		{
			Prefix: HandoffPrefix, RunID: reviewRunID,
			Containers: []string{Seeder, Observer},
			Volumes:    []string{Workspace},
		},
		{
			Prefix: ReviewPrefix, RunID: reviewRunID,
			Containers: []string{
				WorkspaceObs, LegacyWorkspaceObserver, AgentsInit, AgentsObs, LegacyAgentsObserver,
				SnapInit, SnapObs, InputsInit, InputsObs, Codex, Claude,
			},
			Volumes:  []string{Agents, Snap, Inputs},
			Networks: []string{Egress},
		},
		{
			Prefix: ConformancePrefix, RunID: preJobRunID,
			Containers: []string{PreJob},
		},
		{
			Prefix: ConformancePrefix, RunID: conformanceRunID,
			Containers: []string{
				Liveness, Seed, Audit, ExclWriter, ExclSecond, NetLive, Net, InxLive, Inx,
			},
			Volumes: []string{Cred, LivenessWS, ExclWS, NetLiveWS, InxWS},
		},
		{Prefix: PreflightCredentialPrefix, RunID: ownerSlice, BareContainer: true},
		{Prefix: LineupProbePrefix, RunID: ownerSlice, BareContainer: true, Networks: []string{Egress}},
	}
}

// RigContainerPattern is the anchored RE2 pattern for every container name
// RigShapes admits.
func RigContainerPattern() string {
	return rigPattern(func(s Shape) ([]string, bool) { return s.Containers, s.BareContainer })
}

// RigVolumePattern is the anchored RE2 pattern for every volume name
// RigShapes admits.
func RigVolumePattern() string {
	return rigPattern(func(s Shape) ([]string, bool) { return s.Volumes, false })
}

// RigNetworkPattern is the anchored RE2 pattern for every network name
// RigShapes admits.
func RigNetworkPattern() string {
	return rigPattern(func(s Shape) ([]string, bool) { return s.Networks, false })
}

// rigPattern renders one alternative per shape and form: prefix and ID with
// one of the class's roles, and prefix and ID alone where the shape is bare.
// Every literal is quoted, so a role can't widen the pattern.
func rigPattern(class func(Shape) (roles []string, bare bool)) string {
	var alternatives []string
	for _, shape := range RigShapes() {
		roles, bare := class(shape)
		stem := regexp.QuoteMeta(shape.Prefix) + shape.RunID.pattern()
		if len(roles) > 0 {
			quoted := make([]string, len(roles))
			for i, role := range roles {
				quoted[i] = regexp.QuoteMeta(role)
			}
			alternatives = append(alternatives, stem+"-("+strings.Join(quoted, "|")+")")
		}
		if bare {
			alternatives = append(alternatives, stem)
		}
	}
	return "^(" + strings.Join(alternatives, "|") + ")$"
}
