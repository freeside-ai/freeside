package collector

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func WriteScheduleArtifacts(outputDir string, snapshot ScheduleSnapshot) error {
	if outputDir == "" {
		return fmt.Errorf("output directory is required")
	}
	jsonBytes, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("encode schedule snapshot: %w", err)
	}
	jsonBytes = append(jsonBytes, '\n')
	reportBytes := []byte(RenderScheduleReport(snapshot))
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	if err := writeAtomic(filepath.Join(outputDir, "schedule.json"), jsonBytes); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(outputDir, "schedule.md"), reportBytes)
}

// RenderScheduleReport prints the scheduling-state evidence. With any
// ambiguity it states no entry count or clean verdict: the entries it lists
// then cover retained evidence only.
func RenderScheduleReport(snapshot ScheduleSnapshot) string {
	var report bytes.Buffer
	stamp := "2006-01-02T15:04:05.999999999Z07:00"
	fmt.Fprintln(&report, "# Scheduling State Evidence")
	fmt.Fprintln(&report)
	fmt.Fprintf(&report, "Repository: `%s`  \n", snapshot.Repository)
	fmt.Fprintf(&report, "Collected: `%s` to `%s`\n", snapshot.StartedAt.Format(stamp), snapshot.CompletedAt.Format(stamp))
	fmt.Fprintln(&report)
	fmt.Fprintln(&report, "This is evidence for the half-scheduled check and the deferral sweep in `docs/coordination.md`. It repairs nothing.")
	fmt.Fprintln(&report)

	fmt.Fprintln(&report, "## Open Trackers")
	fmt.Fprintln(&report)
	if len(snapshot.Trackers) == 0 && snapshot.Complete {
		fmt.Fprintln(&report, "None.")
	}
	for _, tracker := range snapshot.Trackers {
		milestone := tracker.Milestone
		if milestone == "" {
			milestone = "none"
		}
		fmt.Fprintf(&report, "- #%d %s, milestone: %s\n", tracker.Number, tracker.Title, milestone)
	}
	fmt.Fprintln(&report)

	fmt.Fprintln(&report, "## Summary")
	fmt.Fprintln(&report)
	switch {
	case !snapshot.Complete:
		fmt.Fprintln(&report, "No entry count or verdict: the evidence is incomplete; see **AMBIGUOUS**. The entries below come from retained evidence only.")
	case len(snapshot.Entries) == 0:
		fmt.Fprintln(&report, "Entries: **0**. Every milestone matches a tracker listing, and every open issue is milestoned or carries a label a sweep selects.")
	default:
		fmt.Fprintf(&report, "Entries: **%d**.\n", len(snapshot.Entries))
	}
	fmt.Fprintln(&report)

	sections := []struct{ kind, heading string }{
		{ScheduleMilestoneUnlisted, "Milestone Without a Listing"},
		{ScheduleListedUnmilestoned, "Listing Without the Milestone"},
		{ScheduleInvisible, "Invisible to Every Sweep"},
	}
	for _, section := range sections {
		fmt.Fprintf(&report, "## %s (`%s`)\n\n", section.heading, section.kind)
		listed := 0
		for _, entry := range snapshot.Entries {
			if entry.Kind != section.kind {
				continue
			}
			listed++
			labels := "none"
			if len(entry.Labels) > 0 {
				labels = strings.Join(entry.Labels, ", ")
			}
			fmt.Fprintf(&report, "- #%d %s: %s. Labels: %s; trackers: %s.\n", entry.Number, entry.Title, entry.Detail, labels, issueNumberList(entry.Trackers))
		}
		if listed == 0 && snapshot.Complete {
			fmt.Fprintln(&report, "None.")
		}
		fmt.Fprintln(&report)
	}

	fmt.Fprintln(&report, "## AMBIGUOUS")
	fmt.Fprintln(&report)
	if len(snapshot.Ambiguities) == 0 {
		fmt.Fprintln(&report, "None.")
	}
	for _, ambiguity := range snapshot.Ambiguities {
		fmt.Fprintf(&report, "- **%s** (%s): %s\n", ambiguity.Code, ambiguity.Subject, ambiguity.Detail)
	}
	return report.String()
}
