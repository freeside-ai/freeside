package collector

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func WriteUnitArtifacts(outputDir string, snapshot UnitSnapshot) error {
	if outputDir == "" {
		return fmt.Errorf("output directory is required")
	}
	jsonBytes, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("encode unit snapshot: %w", err)
	}
	jsonBytes = append(jsonBytes, '\n')
	reportBytes := []byte(RenderUnitReport(snapshot))
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	if err := writeAtomic(filepath.Join(outputDir, "unit.json"), jsonBytes); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(outputDir, "unit.md"), reportBytes)
}

// RenderUnitReport prints the evidence with the UNKNOWN entries first. It
// states what was read and never whether work may start. An empty list is
// called empty only over complete evidence: with any UNKNOWN entry the
// members, reverse declarations, and trackers are retained evidence, and an
// absence among them establishes nothing.
func RenderUnitReport(snapshot UnitSnapshot) string {
	var report bytes.Buffer
	stamp := "2006-01-02T15:04:05.999999999Z07:00"
	fmt.Fprintf(&report, "# Unit Coordination Evidence: #%d\n\n", snapshot.Issue)
	fmt.Fprintf(&report, "Repository: `%s`  \n", snapshot.Repository)
	fmt.Fprintf(&report, "Issue: #%d %s  \n", snapshot.Issue, snapshot.Title)
	fmt.Fprintf(&report, "Collected: `%s` to `%s`\n\n", snapshot.StartedAt.Format(stamp), snapshot.CompletedAt.Format(stamp))
	fmt.Fprintln(&report, "This is evidence for the Claiming reads in `docs/coordination.md`. It decides nothing; the session applies the Coordination Gates in `AGENTS.md`.")
	fmt.Fprintln(&report)

	fmt.Fprintln(&report, "## UNKNOWN")
	fmt.Fprintln(&report)
	if snapshot.Complete {
		fmt.Fprintln(&report, "None.")
	} else {
		fmt.Fprintln(&report, "The evidence is incomplete. A state these entries leave unread is not established, and the sections below come from retained evidence only.")
		fmt.Fprintln(&report)
	}
	for _, unknown := range snapshot.Unknowns {
		fmt.Fprintf(&report, "- **%s** (%s): %s\n", unknown.Code, unknown.Subject, unknown.Detail)
	}
	fmt.Fprintln(&report)

	fmt.Fprintln(&report, "## UNKNOWN Relationships")
	fmt.Fprintln(&report)
	if len(snapshot.UnknownRelationships) == 0 {
		fmt.Fprintln(&report, emptyList(snapshot.Complete))
	}
	for _, unknown := range snapshot.UnknownRelationships {
		fmt.Fprintf(&report, "- **UNKNOWN relationship** on #%d: %s.\n", unknown.Issue, unknown.Reason)
		writeRawLines(&report, unknown.Line)
	}
	fmt.Fprintln(&report)

	fmt.Fprintf(&report, "## Dependencies of #%d\n\n", snapshot.Issue)
	if len(snapshot.Dependencies) == 0 {
		fmt.Fprintln(&report, "No line leads with a typed relationship or `none`.")
	}
	for _, dependency := range snapshot.Dependencies {
		switch {
		case dependency.Kind == DependencyNone && dependency.Relationship == "":
			fmt.Fprintln(&report, "- `none`")
		case dependency.Kind == DependencyNone:
			fmt.Fprintf(&report, "- `%s`: `none`\n", dependency.Relationship)
		default:
			fmt.Fprintf(&report, "- `%s` %s\n", dependency.Relationship, dependencyTargetList(dependency.Targets))
		}
		writeRawLines(&report, dependency.Line)
	}
	fmt.Fprintln(&report)

	fmt.Fprintln(&report, "## Direct Exclusivity Set")
	fmt.Fprintln(&report)
	if !snapshot.Complete {
		fmt.Fprintln(&report, "Members found in retained evidence; the set may be larger.")
		fmt.Fprintln(&report)
	}
	for _, member := range snapshot.Members {
		writeUnitMember(&report, snapshot.Issue, member)
	}
	fmt.Fprintln(&report)

	fmt.Fprintf(&report, "## Open Trackers Listing #%d\n\n", snapshot.Issue)
	if len(snapshot.Trackers) == 0 {
		fmt.Fprintln(&report, emptyList(snapshot.Complete))
	}
	for _, tracker := range snapshot.Trackers {
		milestone := tracker.Milestone
		if milestone == "" {
			milestone = "none"
		}
		fmt.Fprintf(&report, "- #%d %s, milestone: %s\n", tracker.Number, tracker.Title, milestone)
	}
	return report.String()
}

func emptyList(complete bool) string {
	if complete {
		return "None."
	}
	return "None in the retained evidence."
}

func writeUnitMember(report *bytes.Buffer, assigned int, member UnitMember) {
	var relations []string
	for _, relation := range member.Relations {
		switch relation {
		case RelationAssigned:
			relations = append(relations, "the assigned issue")
		case RelationForward:
			relations = append(relations, fmt.Sprintf("#%d declares `exclusive-with` it", assigned))
		case RelationReverse:
			relations = append(relations, fmt.Sprintf("it declares `exclusive-with` #%d", assigned))
		}
	}
	fmt.Fprintf(report, "- #%d", member.Number)
	if member.Title != "" {
		fmt.Fprintf(report, " %s", member.Title)
	}
	fmt.Fprintf(report, ": `%s` (%s)", member.State, strings.Join(relations, "; "))
	if member.Stamp != nil {
		milestone := member.Milestone
		if milestone == "" {
			milestone = "none"
		}
		fmt.Fprintf(report, ", milestone: %s", milestone)
		if member.Deferral {
			fmt.Fprint(report, ", deferral")
		}
	}
	fmt.Fprintln(report)
	switch member.State {
	case MemberUnknown:
		fmt.Fprintln(report, "  - Claim state not established; see **UNKNOWN**. Any claim, PR, or reservation below is retained evidence only.")
	case MemberNotOpen:
		fmt.Fprintln(report, "  - Not in the open-issue inventory: closed, or not an issue. Its comments were not read.")
	}
	if member.OrderingClaim != nil {
		fmt.Fprintf(report, "  - Ordering key: claim comment %d, created %s.\n", member.OrderingClaim.CommentID, member.OrderingClaim.CreatedAt)
	}
	for _, claim := range member.Claims {
		fmt.Fprintf(report, "  - Claim comment %d at %s on `%s`: %s", claim.CommentID, claim.CreatedAt, claim.Branch, claim.Status)
		if len(claim.ClosingPullRequests) > 0 {
			fmt.Fprintf(report, ", open PR closing the issue: %s", issueNumberList(claim.ClosingPullRequests))
		}
		if len(claim.OtherPullRequests) > 0 {
			fmt.Fprintf(report, ", open PR from the branch without the close keyword: %s", issueNumberList(claim.OtherPullRequests))
		}
		fmt.Fprintln(report, ".")
	}
	if len(member.LegacyPullRequests) > 0 {
		fmt.Fprintf(report, "  - Open PR closing the issue with no claim comment behind it: %s.\n", issueNumberList(member.LegacyPullRequests))
	}
	for _, reservation := range member.Reservations {
		status := "expired"
		if reservation.Active {
			status = "active"
		}
		fmt.Fprintf(report, "  - Planning reservation comment %d at %s: %s.\n", reservation.CommentID, reservation.CreatedAt, status)
	}
}

// writeRawLines prints issue text word for word, each line as a code span
// fenced by one backtick more than its longest run, so the text can neither
// close its span nor add structure to the report.
func writeRawLines(report *bytes.Buffer, text string) {
	if text == "" {
		return
	}
	for _, line := range strings.Split(text, "\n") {
		fence := strings.Repeat("`", longestBacktickRun(line)+1)
		fmt.Fprintf(report, "  - %s %s %s\n", fence, line, fence)
	}
}
