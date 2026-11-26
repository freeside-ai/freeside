package daemoncompose

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// Agent-base upkeep's composition (plan §5.7): the stored source setting
// upkeep builds from, and the weekly bump schedule that opens a task to move
// the agent base's Claude CLI pin to the newest release.

const (
	agentBaseBumpScheduleID = domain.ScheduleID("schedule-agent-base-bump")
	agentBaseBumpInterval   = 7 * 24 * time.Hour
	// claudeCodeLatestURL is the npm registry's document for the release
	// the latest dist-tag names.
	claudeCodeLatestURL = "https://registry.npmjs.org/@anthropic-ai/claude-code/latest"
	// maxNPMDocumentBytes bounds the registry document read; a larger one
	// is refused.
	maxNPMDocumentBytes strictjson.Limit = 4 << 20
)

// agentBaseSourceSetting reads the agent_base instance setting at each call,
// nil when unset. The stored section is re-checked against the settings gate
// on every read, so a value current policy refuses never chooses what the
// daemon builds its agent base from.
func agentBaseSourceSetting(st *store.Store) func(context.Context) (*domain.AgentBaseSettings, error) {
	return func(ctx context.Context) (*domain.AgentBaseSettings, error) {
		var record domain.InstanceSettingsRecord
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			record, err = tx.InstanceSettingsRecord(ctx)
			return err
		}); err != nil {
			return nil, err
		}
		source, err := record.AgentBase()
		if err != nil || source == nil {
			return nil, err
		}
		if err := CheckInstanceSettingsSection(
			domain.InstanceSettings{AgentBase: source}, domain.InstanceSectionAgentBase,
		); err != nil {
			return nil, err
		}
		return source, nil
	}
}

// claudeRelease is a plain major.minor.patch release. A pre-release or any
// other shape is not one, so the bump never moves to it.
var claudeRelease = regexp.MustCompile(`^(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})$`)

// newerClaudeRelease reports whether candidate is a plain release later than
// current. Either one malformed is not newer.
func newerClaudeRelease(candidate, current string) bool {
	a, b := claudeRelease.FindStringSubmatch(candidate), claudeRelease.FindStringSubmatch(current)
	if a == nil || b == nil {
		return false
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(a[i])
		y, _ := strconv.Atoi(b[i])
		if x != y {
			return x > y
		}
	}
	return false
}

// readLatestClaudeCode reads the release npm's latest dist-tag names. The
// registry is untrusted: the answer only decides whether a bump task is
// opened, never what it asks, and a version that is not a plain release is
// refused here.
func readLatestClaudeCode(ctx context.Context, client *http.Client, url string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("read the latest Claude Code release: %w", err)
	}
	defer response.Body.Close() //nolint:errcheck // read-only body
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("read the latest Claude Code release: registry answered %s", response.Status)
	}
	var document struct {
		Version string `json:"version"`
	}
	if err := strictjson.DecodeReaderAllowingUnknownFields(
		response.Body, &document, strictjson.RejectInvalidUTF8, maxNPMDocumentBytes,
	); err != nil {
		return "", fmt.Errorf("read the latest Claude Code release: %w", err)
	}
	if !claudeRelease.MatchString(document.Version) {
		return "", fmt.Errorf("read the latest Claude Code release: the registry named %q, not a plain release",
			boundedVersionText(document.Version))
	}
	return document.Version, nil
}

func boundedVersionText(version string) string {
	const limit = 64
	if len(version) > limit {
		return version[:limit] + "…"
	}
	return version
}

// agentBaseBump opens the bump task an agent_base_bump occurrence asks for,
// when a Claude Code release newer than every pin the daemon knows exists.
type agentBaseBump struct {
	store        *store.Store
	attention    *signet.Service
	project      domain.ProjectID
	authority    engine.ProjectAuthoritySource
	source       func(context.Context) (*domain.AgentBaseSettings, error)
	repository   string
	repositoryID int64
	// pinned is the release this daemon's adapter pins, and tipPinned the
	// release the source pinned at the last tip upkeep read, empty when
	// none was read.
	pinned    string
	tipPinned func() string
	latest    func(context.Context) (string, error)
	logger    *slog.Logger
}

// handle submits the occurrence's bump task, or nothing when the agent_base
// setting does not name the daemon's repository, the project's writers run
// warded, upkeep has read no release pinned at the tip, or no newer release
// exists. Only a cordoned writer can reach the
// npm registry the task's measurements need.
func (b *agentBaseBump) handle(ctx context.Context, occurrence domain.ScheduleEvent) error {
	source, err := b.source(ctx)
	if err != nil {
		return fmt.Errorf("read the agent_base setting: %w", err)
	}
	if source == nil || source.RepositoryID != b.repositoryID || source.Repository != b.repository {
		b.logger.Info("agent base bump: the agent_base setting does not name this daemon's repository; opened no task")
		return nil
	}
	var authority engine.ProjectAuthority
	if err := b.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		authority, err = b.authority(ctx, tx, b.project)
		return err
	}); err != nil {
		return fmt.Errorf("read project %s's authority: %w", b.project, err)
	}
	if authority.WriterClass != nil && *authority.WriterClass != domain.BackendCordonedHostProcess {
		b.logger.Warn("agent base bump: the project's writers do not run cordoned, so a bump writer could not reach the npm registry; opened no task",
			"project", b.project, "writer_class", *authority.WriterClass)
		return nil
	}
	// Until upkeep has read a plain release at the tip, a bump that already
	// merged is indistinguishable from none, so the fire opens nothing
	// rather than a second bump.
	tip := b.tipPinned()
	if !claudeRelease.MatchString(tip) {
		b.logger.Info("agent base bump: upkeep has read no release pinned at the tip; opened no task", "tip", tip)
		return nil
	}
	latest, err := b.latest(ctx)
	if err != nil {
		return err
	}
	current := b.pinned
	if newerClaudeRelease(tip, current) {
		current = tip
	}
	if !newerClaudeRelease(latest, current) {
		b.logger.Info("agent base bump: the pinned Claude Code release is current", "release", current)
		return nil
	}
	submission, err := b.attention.SubmitAgentBaseBumpTask(ctx, b.project, occurrence)
	if err != nil {
		return fmt.Errorf("submit the agent base bump task: %w", err)
	}
	b.logger.Info("agent base bump: a newer Claude Code release exists",
		"release", latest, "pinned", current, "task", submission.TaskID)
	return nil
}

// composeAgentBaseBump composes the bump schedule's handler, or nil when the
// daemon's configured repository has no upkeep or no project on it is
// configured for manual submission to take the task (the project its
// maintenance tasks go to, ImageRepairProject). Only that repository's
// upkeep keeps the agent base, so its tip read is the one the bump compares.
func composeAgentBaseBump(
	st *store.Store, attention *signet.Service, cfg ClaudeStartupConfig, wiring *ClaudeComposition,
	upkeeps imageUpkeeps,
) *agentBaseBump {
	var configured *repositoryUpkeep
	for i := range upkeeps {
		if upkeeps[i].loop.RepositoryID == cfg.Claude.RepositoryID {
			configured = &upkeeps[i]
			break
		}
	}
	if configured == nil {
		return nil
	}
	project, ok := ImageRepairProject(configured.loop.Manual)
	if !ok {
		return nil
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	client := &http.Client{Timeout: time.Minute}
	return &agentBaseBump{
		store: st, attention: attention, project: project, authority: wiring.projectAuthority,
		source: agentBaseSourceSetting(st), repository: cfg.Claude.Repo, repositoryID: cfg.Claude.RepositoryID,
		pinned: harnessBuildRelease(agentbaseline.ClaudeCodeHarnessBuild), tipPinned: configured.upkeep.TipAgentBaseVersion,
		latest: func(ctx context.Context) (string, error) {
			return readLatestClaudeCode(ctx, client, claudeCodeLatestURL)
		},
		logger: logger.With("subsystem", "agent_base_bump"),
	}
}

// harnessBuildRelease is the release a harness build pin names
// (agentbaseline.ClaudeCodeHarnessBuild).
func harnessBuildRelease(harnessBuild string) string {
	release, _ := strings.CutPrefix(harnessBuild, "claude-code ")
	return release
}
