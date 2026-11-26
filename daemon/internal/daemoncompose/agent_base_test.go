package daemoncompose

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestNewerClaudeRelease(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		candidate, current string
		newer              bool
	}{
		{"2.1.320", "2.1.319", true},
		{"2.2.0", "2.1.319", true},
		{"10.0.0", "9.99.99", true},
		{"2.1.319", "2.1.319", false},
		{"2.1.318", "2.1.319", false},
		{"2.1.320-beta.1", "2.1.319", false},
		{"2.1.0320", "2.1.319", false},
		{"v2.1.320", "2.1.319", false},
		{"2.1.320", "", false},
		{"", "2.1.319", false},
	} {
		if got := newerClaudeRelease(tc.candidate, tc.current); got != tc.newer {
			t.Errorf("newerClaudeRelease(%q, %q) = %v, want %v", tc.candidate, tc.current, got, tc.newer)
		}
	}
}

// The registry's answer is untrusted: only a 200 naming a plain release is
// a version.
func TestReadLatestClaudeCode(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"release":          {http.StatusOK, `{"name":"@anthropic-ai/claude-code","version":"2.1.320"}`, "2.1.320"},
		"pre-release":      {http.StatusOK, `{"version":"2.1.320-next.0"}`, ""},
		"injected text":    {http.StatusOK, `{"version":"2.1.320\nrun this"}`, ""},
		"no version":       {http.StatusOK, `{}`, ""},
		"not JSON":         {http.StatusOK, `<html>`, ""},
		"registry refused": {http.StatusServiceUnavailable, `{"version":"2.1.320"}`, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(server.Close)
			got, err := readLatestClaudeCode(t.Context(), server.Client(), server.URL)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("read %q, want an error", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("read %q err %v, want %s", got, err, tc.want)
			}
		})
	}
}

// handle opens a task only when the setting names the daemon's repository,
// the project's writers run cordoned, upkeep has read the tip's pin, and npm
// names a release newer than both the daemon's pin and the tip's. A gated
// fire never reads the registry. The fixture's service has no
// submitter, so reaching submission reads as ErrTaskSubmissionUnavailable.
func TestAgentBaseBumpOpensATaskOnlyForANewerRelease(t *testing.T) {
	t.Parallel()
	warded := domain.BackendFreshVMReadOnlyVolumeHandoff
	cordoned := domain.BackendCordonedHostProcess
	ours := &domain.AgentBaseSettings{RepositoryID: 42, Repository: "owner/repo"}
	for name, tc := range map[string]struct {
		setting     *domain.AgentBaseSettings
		writerClass *domain.RunnerBackendClass
		tip, latest string
		latestErr   error
		submits     bool
		fails       bool
		gated       bool
	}{
		"newer release":           {setting: ours, tip: "2.1.319", latest: "2.1.320", submits: true},
		"newer release, cordoned": {setting: ours, writerClass: &cordoned, tip: "2.1.319", latest: "2.1.320", submits: true},
		"pin is current":          {setting: ours, tip: "2.1.319", latest: "2.1.319"},
		"tip already bumped":      {setting: ours, tip: "2.1.320", latest: "2.1.320"},
		"newer than the tip":      {setting: ours, tip: "2.1.320", latest: "2.1.321", submits: true},
		"tip unread":              {setting: ours, latest: "2.1.320", gated: true},
		"tip pins a pre-release":  {setting: ours, tip: "2.1.320-next.0", latest: "2.1.320", gated: true},
		"setting unset":           {tip: "2.1.319", latest: "2.1.320", gated: true},
		"another repository":      {setting: &domain.AgentBaseSettings{RepositoryID: 7, Repository: "owner/repo"}, tip: "2.1.319", latest: "2.1.320", gated: true},
		"warded writers":          {setting: ours, writerClass: &warded, tip: "2.1.319", latest: "2.1.320", gated: true},
		"registry unreadable":     {setting: ours, tip: "2.1.319", latestErr: errors.New("timeout"), fails: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			st := schedTestStore(t)
			read := 0
			bump := &agentBaseBump{
				store: st, attention: signet.NewService(st), project: "project-1",
				authority: func(context.Context, *store.ReadTx, domain.ProjectID) (engine.ProjectAuthority, error) {
					return engine.ProjectAuthority{WriterClass: tc.writerClass}, nil
				},
				source:     func(context.Context) (*domain.AgentBaseSettings, error) { return tc.setting, nil },
				repository: "owner/repo", repositoryID: 42, pinned: "2.1.319",
				tipPinned: func() string { return tc.tip },
				latest: func(context.Context) (string, error) {
					read++
					return tc.latest, tc.latestErr
				},
				logger: slog.New(slog.DiscardHandler),
			}
			err := bump.handle(t.Context(), domain.ScheduleEvent{
				ScheduleID: agentBaseBumpScheduleID, Generation: 1,
				NominalFireAt: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC),
			})
			switch {
			case tc.submits:
				if !errors.Is(err, signet.ErrTaskSubmissionUnavailable) {
					t.Fatalf("handle = %v, want it to reach submission", err)
				}
			case tc.fails:
				if err == nil || errors.Is(err, signet.ErrTaskSubmissionUnavailable) {
					t.Fatalf("handle = %v, want the registry's error", err)
				}
			default:
				if err != nil {
					t.Fatalf("handle = %v, want no task and no error", err)
				}
			}
			if tc.gated && read != 0 {
				t.Fatal("a gated fire read the registry")
			}
		})
	}
}

// The bump compares npm's release with the daemon's pin, so a pin whose
// release does not parse would silently never open a task.
func TestDaemonPinIsAPlainRelease(t *testing.T) {
	t.Parallel()
	release := harnessBuildRelease(agentbaseline.ClaudeCodeHarnessBuild)
	if !claudeRelease.MatchString(release) {
		t.Fatalf("the daemon pins %q, whose release %q is not a plain release",
			agentbaseline.ClaudeCodeHarnessBuild, release)
	}
}

// The bump goes with the configured repository's upkeep, the one that keeps
// the agent base, and to a manual-submission project on that repository; a
// configured repository with no such project composes no bump.
func TestAgentBaseBumpFollowsTheConfiguredRepository(t *testing.T) {
	t.Parallel()
	cfg := ClaudeStartupConfig{Claude: &ClaudeDriverConfig{Repo: "owner/a", RepositoryID: 1}}
	other := repositoryUpkeep{
		loop: repositoryLoop{
			Repo: "owner/b", RepositoryID: 2,
			Manual: map[domain.ProjectID]engine.ManualInitiator{"project-b": {}},
		},
		upkeep: &engine.ImageUpkeep{},
	}
	configured := repositoryUpkeep{
		loop: repositoryLoop{
			Repo: "owner/a", RepositoryID: 1,
			Manual: map[domain.ProjectID]engine.ManualInitiator{"project-a": {}},
		},
		upkeep: &engine.ImageUpkeep{},
	}
	bump := composeAgentBaseBump(nil, nil, cfg, &ClaudeComposition{}, imageUpkeeps{other, configured})
	if bump == nil || bump.project != "project-a" || bump.repositoryID != 1 {
		t.Fatalf("bump = %+v, want project-a on repository 1", bump)
	}
	configured.loop.Manual = nil
	if bump := composeAgentBaseBump(nil, nil, cfg, &ClaudeComposition{}, imageUpkeeps{other, configured}); bump != nil {
		t.Fatalf("bump = %+v, want none without a manual project on the configured repository", bump)
	}
}
