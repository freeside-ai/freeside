package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func scheduleConfig(output string) ScheduleConfig {
	return ScheduleConfig{
		Repository: Repository{Host: "github.com", Owner: "freeside-ai", Name: "freeside"},
		OutputDir:  output,
		PageSize:   100,
		PageCap:    10,
	}
}

// waveTracker adds a tracker carrying a milestone, the shape only the wave
// tracker takes (docs/coordination.md, Tracking Issues).
func waveTracker(f *contractFixture, number int, milestone, body string) {
	f.tracker(number, body)
	f.issues[len(f.issues)-1]["milestone"] = map[string]any{"title": milestone}
}

func collectSchedule(t *testing.T, fixture *contractFixture) ScheduleSnapshot {
	t.Helper()
	snapshot, err := CollectSchedule(context.Background(), scheduleConfig(""), fixture.runner(t), fixedClock)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Complete {
		t.Fatalf("snapshot is incomplete: %#v", snapshot.Ambiguities)
	}
	return snapshot
}

func scheduleEntries(snapshot ScheduleSnapshot) string {
	parts := make([]string, 0, len(snapshot.Entries))
	for _, entry := range snapshot.Entries {
		parts = append(parts, fmt.Sprintf("%s#%d", entry.Kind, entry.Number))
	}
	return strings.Join(parts, ",")
}

func TestScheduleMilestoneWithoutAListing(t *testing.T) {
	fixture := newContractFixture()
	// The wave tracker carries its milestone without listing itself, and is
	// never an entry.
	waveTracker(fixture, 100, "1B", "## Units\n### lane:spine\n- [ ] #10")
	fixture.issue(10, "1B", "kind:feature")
	fixture.issue(11, "1B", "kind:feature")
	// A milestone no open tracker carries is unlisted wherever the issue
	// appears.
	fixture.issue(12, "1A", "kind:fix")
	snapshot := collectSchedule(t, fixture)

	if got := scheduleEntries(snapshot); got != "milestone-unlisted#11,milestone-unlisted#12" {
		t.Fatalf("entries = %s", got)
	}
	if snapshot.Entries[0].Milestone != "1B" || len(snapshot.Entries[0].Trackers) != 0 {
		t.Fatalf("entry = %#v", snapshot.Entries[0])
	}
}

func TestScheduleListingWithoutTheMilestone(t *testing.T) {
	fixture := newContractFixture()
	waveTracker(fixture, 100, "1B", "## Units\n### lane:spine\n- [ ] #10\n- [x] #11\n- [ ] #12\n- [ ] #13")
	fixture.issue(10, "", deferralLabel)
	fixture.issue(11, "1B", "kind:feature")
	fixture.issue(12, "1A", "kind:fix")
	// #13 is closed: a merged unit leaves the open inventory and is no entry.
	snapshot := collectSchedule(t, fixture)

	// #12 carries another wave's milestone: listed here without this one, and
	// no open tracker carries its own.
	if got := scheduleEntries(snapshot); got != "milestone-unlisted#12,listed-unmilestoned#10,listed-unmilestoned#12" {
		t.Fatalf("entries = %s", got)
	}
	unlisted := snapshot.Entries[1]
	if unlisted.Detail != "listed on #100, whose milestone is 1B, but carries no milestone" || len(unlisted.Trackers) != 1 || unlisted.Trackers[0] != 100 {
		t.Fatalf("entry = %#v", unlisted)
	}
}

func TestScheduleOwnerRunEntriesAreNotListings(t *testing.T) {
	fixture := newContractFixture()
	waveTracker(fixture, 100, "1B", strings.Join([]string{
		"## Units",
		"### lane:spine",
		"- [ ] #10",
		"### Owner-run",
		"- [ ] #20",
		"- [ ] #21",
		"### lane:ward",
		"- [ ] #22",
		"## Exit",
		"- [ ] #23 proves the exit",
	}, "\n"))
	fixture.issue(10, "1B", "kind:feature")
	// An Owner-run item stays unmilestoned and fiat-only.
	fixture.issue(20, "", needsHumanLabel)
	// A milestone on an Owner-run item has no scheduling listing behind it.
	fixture.issue(21, "1B", needsHumanLabel)
	// The Owner-run group ends at the next heading of its level.
	fixture.issue(22, "", deferralLabel)
	// Exit may cite an issue without listing it.
	fixture.issue(23, "1B", "kind:feature")
	snapshot := collectSchedule(t, fixture)

	if got := scheduleEntries(snapshot); got != "milestone-unlisted#21,milestone-unlisted#23,listed-unmilestoned#22" {
		t.Fatalf("entries = %s", got)
	}
	if trackers := snapshot.Entries[0].Trackers; len(trackers) != 1 || trackers[0] != 100 {
		t.Fatalf("an Owner-run entry still names its tracker: %#v", snapshot.Entries[0])
	}
}

func TestScheduleAdHocTrackerListsWithoutAMilestone(t *testing.T) {
	fixture := newContractFixture()
	// An ad hoc tracker carries no milestone and its units start by fiat, so
	// its listing schedules nothing and needs no milestone.
	fixture.tracker(101, "## Units\n### lane:spine\n- [ ] #30\n- [ ] #31")
	fixture.issue(30, "", deferralLabel)
	fixture.issue(31, "", "kind:feature")
	snapshot := collectSchedule(t, fixture)

	if got := scheduleEntries(snapshot); got != "invisible#31" {
		t.Fatalf("entries = %s", got)
	}
	if trackers := snapshot.Entries[0].Trackers; len(trackers) != 1 || trackers[0] != 101 {
		t.Fatalf("entry = %#v", snapshot.Entries[0])
	}
}

func TestScheduleShellTrackerListsNothing(t *testing.T) {
	fixture := newContractFixture()
	// Wave planning creates the tracker with its milestone and no Units, then
	// sweeps. Its report must stay usable: the shell lists nothing.
	waveTracker(fixture, 100, "1B", "**Wave 9.**\n\n## Exit\n\n- [ ] #10 proves the exit")
	fixture.issue(10, "1B", "kind:feature")
	snapshot := collectSchedule(t, fixture)

	if got := scheduleEntries(snapshot); got != "milestone-unlisted#10" {
		t.Fatalf("entries = %s", got)
	}
}

func TestScheduleReadsACRLFTracker(t *testing.T) {
	fixture := newContractFixture()
	// GitHub stores web-editor text with CRLF endings. The last entry of
	// each group must still parse once the groups are split apart.
	waveTracker(fixture, 100, "1B", strings.Join([]string{
		"## Units", "### lane:spine", "- [ ] #10", "- [ ] #11",
		"### Owner-run", "- [ ] #20", "- [ ] #21", "### lane:ward", "- [ ] #12", "", "## Exit", "",
	}, "\r\n"))
	fixture.issue(10, "1B", "kind:feature")
	fixture.issue(11, "1B", "kind:feature")
	fixture.issue(12, "1B", "kind:feature")
	fixture.issue(20, "", needsHumanLabel)
	fixture.issue(21, "1B", needsHumanLabel)
	snapshot := collectSchedule(t, fixture)

	// #11 ends the lane:spine part and #21 ends the Owner-run part; both are
	// read, so #21 names its tracker.
	if got := scheduleEntries(snapshot); got != "milestone-unlisted#21" {
		t.Fatalf("entries = %s", got)
	}
	if trackers := snapshot.Entries[0].Trackers; len(trackers) != 1 || trackers[0] != 100 {
		t.Fatalf("entry = %#v", snapshot.Entries[0])
	}
}

func TestScheduleInvisibleToEverySweep(t *testing.T) {
	fixture := newContractFixture()
	waveTracker(fixture, 100, "1B", "## Units\n### lane:spine\n- [ ] #43")
	fixture.issue(40, "")
	fixture.issue(41, "", deferralLabel, "lane:spine")
	fixture.issue(42, "", needsHumanLabel)
	fixture.issue(43, "1B", "kind:feature")
	fixture.issue(44, "", "kind:fix", "lane:saddle")
	snapshot := collectSchedule(t, fixture)

	if got := scheduleEntries(snapshot); got != "invisible#40,invisible#44" {
		t.Fatalf("entries = %s", got)
	}
	if labels := snapshot.Entries[1].Labels; strings.Join(labels, ",") != "kind:fix,lane:saddle" {
		t.Fatalf("labels = %#v", labels)
	}
}

func TestScheduleExitCodes(t *testing.T) {
	base := func() *contractFixture {
		fixture := newContractFixture()
		waveTracker(fixture, 100, "1B", "## Units\n### lane:spine\n- [ ] #10")
		fixture.issue(10, "1B", "kind:feature")
		return fixture
	}
	run := func(t *testing.T, runner *fixtureRunner, wantExit int, config ScheduleConfig) string {
		t.Helper()
		exit, err := RunSchedule(context.Background(), config, runner, fixedClock)
		if err != nil || exit != wantExit {
			t.Fatalf("exit=%d error=%v, want exit %d", exit, err, wantExit)
		}
		raw, err := os.ReadFile(filepath.Join(config.OutputDir, "schedule.md"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(config.OutputDir, "schedule.json")); err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	ambiguous := func(t *testing.T, runner *fixtureRunner, code, subject string, pageCap int) {
		t.Helper()
		config := scheduleConfig(t.TempDir())
		if pageCap > 0 {
			config.PageCap = pageCap
		}
		report := run(t, runner, 2, config)
		if want := fmt.Sprintf("- **%s** (%s)", code, subject); !strings.Contains(report, want) {
			t.Fatalf("report lacks %q:\n%s", want, report)
		}
		if strings.Contains(report, "Entries: **") || !strings.Contains(report, "No entry count or verdict") {
			t.Fatalf("an incomplete report states a verdict:\n%s", report)
		}
	}
	t.Run("no entries", func(t *testing.T) {
		report := run(t, base().runner(t), 0, scheduleConfig(t.TempDir()))
		if !strings.Contains(report, "Entries: **0**.") {
			t.Fatalf("report:\n%s", report)
		}
	})
	t.Run("entries", func(t *testing.T) {
		fixture := base()
		fixture.issue(11, "")
		report := run(t, fixture.runner(t), 3, scheduleConfig(t.TempDir()))
		if !strings.Contains(report, "Entries: **1**.") || !strings.Contains(report, "- #11 Unit 11: no milestone") {
			t.Fatalf("report:\n%s", report)
		}
	})
	t.Run("open-issue inventory page cap", func(t *testing.T) {
		runner := base().runner(t)
		setFixturePageInfo(t, runner, "OpenIssues::", []string{"data", "repository", "issues"}, morePages())
		ambiguous(t, runner, "page-cap-truncation", "open issues", 1)
	})
	t.Run("issue label page overflow", func(t *testing.T) {
		fixture := base()
		fixture.issue(11, "", deferralLabel)
		fixture.issues[len(fixture.issues)-1]["labels"].(map[string]any)["pageInfo"] = morePages()
		ambiguous(t, fixture.runner(t), "label-truncation", "issue #11 labels", 0)
	})
	t.Run("tracker entry with an invalid issue number", func(t *testing.T) {
		fixture := base()
		fixture.tracker(101, "## Units\n### Owner-run\n- [ ] #0")
		ambiguous(t, fixture.runner(t), "malformed-tracker-entry", "issue #101", 0)
	})
	t.Run("tracker with a repeated Units section", func(t *testing.T) {
		fixture := base()
		fixture.tracker(101, "## Units\n- [ ] #10\n\n## Units\n- [ ] #11")
		ambiguous(t, fixture.runner(t), "tracker-structure", "issue #101", 0)
	})
	t.Run("hard failure", func(t *testing.T) {
		runner := &fixtureRunner{responses: map[string]json.RawMessage{}}
		exit, err := RunSchedule(context.Background(), scheduleConfig(t.TempDir()), runner, fixedClock)
		if err == nil || exit != 1 {
			t.Fatalf("exit=%d error=%v", exit, err)
		}
	})
}
