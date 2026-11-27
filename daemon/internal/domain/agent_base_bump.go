package domain

import (
	"strconv"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// Daemon-origin submissions share the daemon: command-ID namespace, and the
// kind of source each one names follows from its prefix: an image-upkeep
// submission names a repair-class refusal record, and an agent-base bump
// (AgentBaseBumpSubmissionPrefix) names the schedule occurrence that fired
// it. The store's daemon-origin gate dispatches on these prefixes.
const (
	imageUpkeepSubmissionPrefix   = "daemon:image-upkeep:"
	AgentBaseBumpSubmissionPrefix = "daemon:agent-base-bump:"
)

// IsImageUpkeepSubmission reports whether commandID is in the namespace of
// project-image upkeep's maintenance tasks (ImageUpkeepSubmissionIdentity).
func IsImageUpkeepSubmission(commandID string) bool {
	return strings.HasPrefix(commandID, imageUpkeepSubmissionPrefix)
}

// AgentBaseBumpSubmissionIdentity is the manual-submission identity of the
// bump task one agent_base_bump schedule occurrence opens. Keying it on the
// occurrence makes a redelivered occurrence converge on the first
// submission and gives each later occurrence its own.
func AgentBaseBumpSubmissionIdentity(scheduleID ScheduleID, generation int64, nominalFireAt time.Time) string {
	return AgentBaseBumpSubmissionPrefix + string(scheduleID) + ":" +
		strconv.FormatInt(generation, 10) + ":" + strconv.FormatInt(nominalFireAt.UTC().UnixNano(), 10)
}

// ParseAgentBaseBumpSubmissionIdentity returns the schedule occurrence an
// agent-base bump identity names. ok is false for any other command ID and
// for one that does not round-trip, so one occurrence has one identity.
func ParseAgentBaseBumpSubmissionIdentity(commandID string) (
	scheduleID ScheduleID, generation int64, nominalFireAt time.Time, ok bool,
) {
	rest, found := strings.CutPrefix(commandID, AgentBaseBumpSubmissionPrefix)
	if !found {
		return "", 0, time.Time{}, false
	}
	nominalAt := strings.LastIndex(rest, ":")
	if nominalAt < 0 {
		return "", 0, time.Time{}, false
	}
	generationAt := strings.LastIndex(rest[:nominalAt], ":")
	if generationAt <= 0 {
		return "", 0, time.Time{}, false
	}
	generation, err := strconv.ParseInt(rest[generationAt+1:nominalAt], 10, 64)
	if err != nil {
		return "", 0, time.Time{}, false
	}
	nanos, err := strconv.ParseInt(rest[nominalAt+1:], 10, 64)
	if err != nil {
		return "", 0, time.Time{}, false
	}
	scheduleID = ScheduleID(rest[:generationAt])
	nominalFireAt = time.Unix(0, nanos).UTC()
	if AgentBaseBumpSubmissionIdentity(scheduleID, generation, nominalFireAt) != commandID {
		return "", 0, time.Time{}, false
	}
	return scheduleID, generation, nominalFireAt, true
}

// AgentBaseBumpTaskName is the bump task's display name.
const AgentBaseBumpTaskName = "Move the Claude agent base to the newest Claude Code release"

// agentBaseBumpTaskText is the bump task's source, fixed in the daemon so
// no schedule, setting, or forge text can change what the task asks. The
// writer finds the newest release on the npm registry, which only a
// cordoned writer can reach.
const agentBaseBumpTaskText = `# Move the Claude agent base to the newest Claude Code release

Freeside's daemon opened this task on its agent_base_bump schedule (plan
§5.7, Agent-base upkeep). Once it merges and the daemon running the merged
code is installed, the daemon builds and admits the new agent base and
rebuilds the project image on it.

## Steps

1. Find the newest published release of ` + "`@anthropic-ai/claude-code`" + `
   on the npm registry (` + "`npm view @anthropic-ai/claude-code version`" + `).
2. If ` + "`CLAUDE_CODE_VERSION`" + ` in ` + "`images/agent-claude/Containerfile`" + `
   already names that release, change nothing and report the task blocked:
   the pin is current, so there is nothing to publish.
3. Otherwise move the pin to that release in every place that holds it:
   - ` + "`CLAUDE_CODE_VERSION`" + ` in ` + "`images/agent-claude/Containerfile`" + `.
   - ` + "`ClaudeCodeHarnessBuild`" + ` in ` + "`daemon/internal/agentbaseline/fragments.go`" + `.
   - The Claude CLI row of the pinned-inputs table in
     ` + "`images/agent-claude/README.md`" + `.
4. Measure the new release's built-in plugins and set every one to false
   under ` + "`enabledPlugins`" + ` in ` + "`images/agent-claude/managed-settings.json`" + `.
   ` + "`TestHarnessBuildsMatchTheImagePins`" + ` and
   ` + "`TestClaudeImageDisablesEveryBuiltinPlugin`" + ` in
   ` + "`daemon/internal/agentbaseline/baseline_test.go`" + ` hold the pins and
   the plugin list together.
5. Run ` + "`bash scripts/check.sh daemon`" + ` and fix what it reports.

Change nothing else.
`

// AgentBaseBumpTaskSource returns the bump task's source bytes.
func AgentBaseBumpTaskSource() []byte {
	return []byte(agentBaseBumpTaskText)
}

// AgentBaseBumpSourceDigest is the content address of the bump task's
// source, the SourceDigest every bump submission records.
func AgentBaseBumpSourceDigest() Digest {
	return Digest(contentaddr.Sum(AgentBaseBumpTaskSource()))
}
