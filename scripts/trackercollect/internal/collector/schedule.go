package collector

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	ScheduleSchemaVersion = 1

	needsHumanLabel = "needs-human"
	ownerRunHeading = "Owner-run"
)

// Schedule entry kinds. Each names one way an issue escapes scheduling
// (docs/coordination.md, Work-Unit Issues and Tracking Issues).
const (
	// ScheduleMilestoneUnlisted is an open issue carrying a milestone that no
	// open tracker with that milestone lists outside its Owner-run group.
	ScheduleMilestoneUnlisted = "milestone-unlisted"
	// ScheduleListedUnmilestoned is an open issue a milestoned tracker lists
	// outside its Owner-run group without carrying that tracker's milestone.
	ScheduleListedUnmilestoned = "listed-unmilestoned"
	// ScheduleInvisible is an open issue with no milestone and none of the
	// deferral, needs-human, or tracker labels, which no sweep selects.
	ScheduleInvisible = "invisible"
)

type ScheduleConfig struct {
	Repository Repository
	OutputDir  string
	PageSize   int
	PageCap    int
}

type ScheduleSnapshot struct {
	SchemaVersion int       `json:"schema_version"`
	Repository    string    `json:"repository"`
	StartedAt     time.Time `json:"started_at"`
	CompletedAt   time.Time `json:"completed_at"`
	// Complete is false when any ambiguity exists. The entries then cover
	// retained evidence only and support no verdict.
	Complete    bool              `json:"complete"`
	Trackers    []ScheduleTracker `json:"trackers"`
	Entries     []ScheduleEntry   `json:"entries"`
	Ambiguities []Ambiguity       `json:"ambiguities"`
}

type ScheduleTracker struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	Milestone string `json:"milestone"`
}

type ScheduleEntry struct {
	Kind      string   `json:"kind"`
	Number    int      `json:"number"`
	Title     string   `json:"title"`
	Milestone string   `json:"milestone"`
	Labels    []string `json:"labels"`
	// Trackers are the open trackers whose Units section lists the issue,
	// in any group, so a reader sees where an entry already appears.
	Trackers []int  `json:"trackers"`
	Detail   string `json:"detail"`
}

// RunSchedule collects the scheduling-state report and writes its artifacts.
// It returns 0 for complete evidence with no entries, 3 for complete evidence
// with entries, 2 when any ambiguity exists (artifacts are still written), and
// 1 with an error for a hard failure.
func RunSchedule(ctx context.Context, config ScheduleConfig, runner QueryRunner, clock func() time.Time) (int, error) {
	snapshot, err := CollectSchedule(ctx, config, runner, clock)
	if err != nil {
		return 1, err
	}
	if err := WriteScheduleArtifacts(config.OutputDir, snapshot); err != nil {
		return 1, err
	}
	if len(snapshot.Ambiguities) > 0 {
		return 2, nil
	}
	if len(snapshot.Entries) > 0 {
		return 3, nil
	}
	return 0, nil
}

// trackerListing is one open tracker's Units section, split into the
// scheduling listing and the Owner-run group, which is not one
// (docs/coordination.md, Tracking Issues).
type trackerListing struct {
	tracker    ScheduleTracker
	scheduling map[int]bool
	all        map[int]bool
}

// CollectSchedule reads every open issue and reports the issues whose
// milestone and tracker listing disagree, and the issues no deferral sweep
// selects. It never writes to the forge and repairs nothing.
//
// A listing counts toward scheduling only on a tracker that carries a
// milestone: only the wave tracker does, and an ad hoc tracker's units start
// by fiat without one (docs/coordination.md, Tracking Issues). Trackers
// themselves are never entries; the wave tracker carries its milestone
// without being a unit.
func CollectSchedule(ctx context.Context, config ScheduleConfig, runner QueryRunner, clock func() time.Time) (ScheduleSnapshot, error) {
	if config.PageSize <= 0 {
		config.PageSize = defaultPageSize
	}
	if config.PageCap <= 0 {
		config.PageCap = defaultPageCap
	}
	c := &collector{
		config: Config{Repository: config.Repository, PageSize: config.PageSize, PageCap: config.PageCap},
		runner: runner,
	}
	startedAt := clock().UTC()

	openIssues, err := c.fetchOpenIssues(ctx)
	if err != nil {
		return ScheduleSnapshot{}, err
	}
	for _, issue := range openIssues {
		if *issue.Labels.PageInfo.HasNextPage {
			c.ambiguous("label-truncation", fmt.Sprintf("issue #%d labels", issue.Number), "more labels exist than one page holds; its classification is unknown")
		}
	}
	listings := c.trackerListings(openIssues)

	snapshot := ScheduleSnapshot{
		SchemaVersion: ScheduleSchemaVersion,
		Repository:    config.Repository.String(),
		StartedAt:     startedAt,
		Trackers:      []ScheduleTracker{},
		Entries:       []ScheduleEntry{},
	}
	for _, listing := range listings {
		snapshot.Trackers = append(snapshot.Trackers, listing.tracker)
	}
	for _, issue := range openIssues {
		if issue.hasLabel(trackerLabel) {
			continue
		}
		milestone := ""
		if issue.Milestone != nil {
			milestone = issue.Milestone.Title
		}
		var listedBy, scheduledBy, mismatched []int
		for _, listing := range listings {
			if listing.all[issue.Number] {
				listedBy = append(listedBy, listing.tracker.Number)
			}
			if listing.tracker.Milestone == "" || !listing.scheduling[issue.Number] {
				continue
			}
			if listing.tracker.Milestone == milestone {
				scheduledBy = append(scheduledBy, listing.tracker.Number)
			} else {
				mismatched = append(mismatched, listing.tracker.Number)
			}
		}
		entry := func(kind, detail string) {
			snapshot.Entries = append(snapshot.Entries, ScheduleEntry{
				Kind: kind, Number: issue.Number, Title: issue.Title, Milestone: milestone,
				Labels: issueLabels(issue), Trackers: append([]int{}, listedBy...), Detail: detail,
			})
		}
		if milestone != "" && len(scheduledBy) == 0 {
			entry(ScheduleMilestoneUnlisted, fmt.Sprintf("carries milestone %s, and no open tracker with that milestone lists it outside Owner-run", milestone))
		}
		for _, number := range mismatched {
			have := "no milestone"
			if milestone != "" {
				have = "milestone " + milestone
			}
			entry(ScheduleListedUnmilestoned, fmt.Sprintf("listed on #%d, whose milestone is %s, but carries %s", number, trackerMilestone(listings, number), have))
		}
		if milestone == "" && !issue.hasLabel(deferralLabel) && !issue.hasLabel(needsHumanLabel) {
			entry(ScheduleInvisible, "no milestone and none of the deferral, needs-human, or tracker labels")
		}
	}
	sort.SliceStable(snapshot.Entries, func(i, j int) bool {
		if snapshot.Entries[i].Kind != snapshot.Entries[j].Kind {
			return scheduleKindOrder(snapshot.Entries[i].Kind) < scheduleKindOrder(snapshot.Entries[j].Kind)
		}
		return snapshot.Entries[i].Number < snapshot.Entries[j].Number
	})
	snapshot.Ambiguities = c.sortedAmbiguities()
	snapshot.Complete = len(snapshot.Ambiguities) == 0
	snapshot.CompletedAt = clock().UTC()
	return snapshot, nil
}

// trackerListings reads each open tracker's Units section. A tracker whose
// listing cannot be read is ambiguous, because any issue might be on it. A
// tracker with no Units section lists nothing: wave planning creates the
// wave tracker as a shell and adds Units only at Close Out, and its sweep
// runs this report in between.
func (c *collector) trackerListings(openIssues []graphIssue) []trackerListing {
	var listings []trackerListing
	for _, issue := range openIssues {
		if !issue.hasLabel(trackerLabel) {
			continue
		}
		subject := fmt.Sprintf("issue #%d", issue.Number)
		units := extractSections(issue.Body, "Units")
		if len(units) > 1 {
			c.ambiguous("tracker-structure", subject, fmt.Sprintf("expected at most one Units section, found %d", len(units)))
			continue
		}
		scheduling, ownerRun := "", ""
		if len(units) == 1 {
			scheduling, ownerRun = splitOwnerRun(units[0])
		}
		schedulingEntries, invalidScheduling := parseCheckboxEntries(scheduling)
		ownerRunEntries, invalidOwnerRun := parseCheckboxEntries(ownerRun)
		if invalid := append(invalidScheduling, invalidOwnerRun...); len(invalid) > 0 {
			c.ambiguous("malformed-tracker-entry", subject, fmt.Sprintf("invalid issue numbers: %s", strings.Join(invalid, ", ")))
			continue
		}
		listing := trackerListing{
			tracker:    ScheduleTracker{Number: issue.Number, Title: issue.Title},
			scheduling: map[int]bool{},
			all:        map[int]bool{},
		}
		if issue.Milestone != nil {
			listing.tracker.Milestone = issue.Milestone.Title
		}
		for _, entry := range schedulingEntries {
			listing.scheduling[entry.UnitNumber] = true
			listing.all[entry.UnitNumber] = true
		}
		for _, entry := range ownerRunEntries {
			listing.all[entry.UnitNumber] = true
		}
		listings = append(listings, listing)
	}
	sort.Slice(listings, func(i, j int) bool { return listings[i].tracker.Number < listings[j].tracker.Number })
	return listings
}

// splitOwnerRun separates a Units section's Owner-run groups from the rest.
// A group runs from its heading to the next heading at the same level or
// above. Headings inside code are not headings, as in extractSections. Each
// line keeps its own ending, so a CRLF body splits into CRLF parts whose
// last line still parses.
func splitOwnerRun(units string) (scheduling, ownerRun string) {
	rawLines := strings.SplitAfter(units, "\n")
	evidenceLines := strings.SplitAfter(markdownEvidence(units), "\n")
	var kept, owner []string
	groupLevel := 0
	for i, line := range evidenceLines {
		if match := headingPattern.FindStringSubmatch(strings.TrimSuffix(line, "\n")); match != nil {
			level := len(match[1])
			if groupLevel > 0 && level <= groupLevel {
				groupLevel = 0
			}
			if groupLevel == 0 && strings.EqualFold(strings.TrimSpace(match[2]), ownerRunHeading) {
				groupLevel = level
			}
		}
		if groupLevel > 0 {
			owner = append(owner, rawLines[i])
		} else {
			kept = append(kept, rawLines[i])
		}
	}
	return strings.Join(kept, ""), strings.Join(owner, "")
}

func trackerMilestone(listings []trackerListing, number int) string {
	for _, listing := range listings {
		if listing.tracker.Number == number {
			return listing.tracker.Milestone
		}
	}
	return ""
}

func issueLabels(issue graphIssue) []string {
	labels := []string{}
	if issue.Labels != nil && issue.Labels.Nodes != nil {
		for _, label := range *issue.Labels.Nodes {
			labels = append(labels, label.Name)
		}
	}
	sort.Strings(labels)
	return labels
}

func scheduleKindOrder(kind string) int {
	switch kind {
	case ScheduleMilestoneUnlisted:
		return 0
	case ScheduleListedUnmilestoned:
		return 1
	case ScheduleInvisible:
		return 2
	}
	return 3
}
