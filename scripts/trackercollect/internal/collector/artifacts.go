package collector

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func WriteArtifacts(outputDir string, snapshot Snapshot) error {
	if outputDir == "" {
		return fmt.Errorf("output directory is required")
	}
	jsonBytes, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("encode snapshot: %w", err)
	}
	jsonBytes = append(jsonBytes, '\n')
	reportBytes := []byte(RenderReport(snapshot))
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	if err := writeAtomic(filepath.Join(outputDir, "snapshot.json"), jsonBytes); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(outputDir, "report.md"), reportBytes); err != nil {
		return err
	}
	return nil
}

func writeAtomic(path string, contents []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".trackercollect-*")
	if err != nil {
		return fmt.Errorf("create temporary artifact for %s: %w", filepath.Base(path), err)
	}
	temporaryPath := temporary.Name()
	ok := false
	defer func() {
		_ = temporary.Close()
		if !ok {
			_ = os.Remove(temporaryPath)
		}
	}()
	if _, err := temporary.Write(contents); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", filepath.Base(path), err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close %s: %w", filepath.Base(path), err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish %s: %w", filepath.Base(path), err)
	}
	ok = true
	return nil
}

func RenderReport(snapshot Snapshot) string {
	var report bytes.Buffer
	canonicalRepository := canonicalNameWithOwner(snapshot.Repository)
	fmt.Fprintln(&report, "# Tracker Reconciliation Evidence")
	fmt.Fprintln(&report)
	fmt.Fprintf(&report, "Repository: `%s`  \n", snapshot.Repository)
	fmt.Fprintf(&report, "Default-branch head: `%s`  \n", snapshot.Collection.DefaultBranchHeadSHA)
	fmt.Fprintf(&report, "Collected: `%s` to `%s`\n", snapshot.Collection.StartedAt.Format("2006-01-02T15:04:05.999999999Z07:00"), snapshot.Collection.CompletedAt.Format("2006-01-02T15:04:05.999999999Z07:00"))
	fmt.Fprintln(&report)

	fmt.Fprintln(&report, "## Merged Unit")
	fmt.Fprintln(&report)
	fmt.Fprintf(&report, "PR #%d, %s, merged as `%s` (`%s` → `%s`).\n", snapshot.MergedPullRequest.Number, snapshot.MergedPullRequest.Title, snapshot.MergedPullRequest.MergeCommitSHA, snapshot.MergedPullRequest.HeadRef, snapshot.MergedPullRequest.BaseRef)
	closingSubject := fmt.Sprintf("pull request #%d closing issues", snapshot.MergedPullRequest.Number)
	closingIssuesTruncated := snapshotHasAmbiguity(snapshot, "page-cap-truncation", closingSubject)
	if closingIssuesTruncated {
		fmt.Fprintln(&report, "Closing issues: incomplete because the connection reached its page cap; see **AMBIGUOUS**.")
	} else if snapshot.MergedPullRequest.DirectUnit {
		fmt.Fprintln(&report, "Closing issues: none (direct session-contained unit; no containing tracker work).")
	} else if len(snapshot.MergedPullRequest.ClosingIssues) == 0 {
		fmt.Fprintln(&report, "Closing issues: no verified current closing issue retained; provenance or reconciliation is unresolved. See **AMBIGUOUS**.")
	} else {
		fmt.Fprintln(&report, "Closing issues:")
		for _, issue := range snapshot.MergedPullRequest.ClosingIssues {
			fmt.Fprintf(&report, "- #%d %s (%s)\n", issue.Number, issue.Title, issue.State)
		}
	}
	fmt.Fprintln(&report)

	fmt.Fprintln(&report, "## Wave-Title Matches")
	fmt.Fprintln(&report)
	if snapshotHasAmbiguity(snapshot, "page-cap-truncation", "pinned issues") {
		fmt.Fprintln(&report, "Pinned-issue inventory is incomplete; counts below cover retained pages only.")
	}
	fmt.Fprintf(&report, "Open canonical-title match count: **%d**. This is evidence, not a wave-state verdict.\n", snapshot.OpenWaveTitleMatchCount)
	for _, issue := range snapshot.PinnedIssues {
		fmt.Fprintf(&report, "- #%d %s (%s), title match: %t\n", issue.Number, issue.Title, issue.State, issue.TitleMatchesWavePattern)
	}
	fmt.Fprintln(&report)

	fmt.Fprintln(&report, "## Containing Trackers")
	fmt.Fprintln(&report)
	openIssuesTruncated := snapshotHasAmbiguity(snapshot, "page-cap-truncation", "open issues")
	zeroWorkEstablished := !closingIssuesTruncated &&
		!snapshotHasAmbiguityCode(snapshot, "closing-issue-state") &&
		!snapshotHasAmbiguityCode(snapshot, "tracker-structure") &&
		!snapshotHasAmbiguityCode(snapshot, "malformed-tracker-entry") &&
		(snapshot.MergedPullRequest.DirectUnit || len(snapshot.MergedPullRequest.ClosingIssues) > 0)
	if openIssuesTruncated {
		fmt.Fprintln(&report, "Containing-tracker selection is incomplete because the open-issue inventory reached its page cap; see **AMBIGUOUS**.")
	}
	if len(snapshot.ContainingTrackers) == 0 && !openIssuesTruncated && zeroWorkEstablished {
		fmt.Fprintln(&report, "None (valid zero-work result).")
	} else if len(snapshot.ContainingTrackers) == 0 {
		fmt.Fprintln(&report, "No containing tracker appears in the retained pages; this is not a zero-work verdict.")
	}
	for _, tracker := range snapshot.ContainingTrackers {
		fmt.Fprintf(&report, "### #%d %s\n\n", tracker.Number, tracker.Title)
		fmt.Fprintln(&report, "Listed units:")
		for _, entry := range tracker.Entries {
			mark := " "
			if entry.Checked {
				mark = "x"
			}
			fmt.Fprintf(&report, "- [%s] #%d\n", mark, entry.UnitNumber)
		}
		for _, unit := range tracker.Units {
			fmt.Fprintf(&report, "\n#### Unit #%d, %s (%s)\n\n", unit.Number, unit.Title, unit.State)
			writeRawSection(&report, "Dependencies", unit.DependenciesSection)
			closingPRsTruncated := snapshotHasAmbiguity(snapshot, "page-cap-truncation", fmt.Sprintf("issue #%d closing pull requests", unit.Number))
			if closingPRsTruncated {
				fmt.Fprintln(&report, "Closing-PR evidence is incomplete because the connection reached its page cap.")
			}
			if len(unit.ClosingPullRequests) == 0 && !closingPRsTruncated {
				fmt.Fprintln(&report, "Closing PRs: none retained.")
			} else if len(unit.ClosingPullRequests) == 0 {
				fmt.Fprintln(&report, "Closing PRs: none in retained pages; this is not a completeness claim.")
			} else {
				fmt.Fprintln(&report, "Closing PRs:")
				for _, pr := range unit.ClosingPullRequests {
					fmt.Fprintf(&report, "- #%d %s (%s, merged: %t, `%s` → `%s`, %s)\n", pr.Number, pr.Title, pr.State, pr.Merged, pr.HeadRef, pr.BaseRef, repositoryIdentityEvidence(pr.HeadRepository, canonicalRepository))
				}
			}
			for _, stacked := range unit.StackedOn {
				fmt.Fprintf(&report, "Stacked-on %s #%d:\n", strings.ReplaceAll(stacked.ReferenceKind, "_", " "), stacked.ReferenceNumber)
				if stacked.ReferenceKind == "issue" && snapshotHasAmbiguity(snapshot, "page-cap-truncation", fmt.Sprintf("issue #%d closing pull requests", stacked.ReferenceNumber)) {
					fmt.Fprintln(&report, "- Base closing-PR evidence is incomplete because the connection reached its page cap.")
				}
				for _, pr := range stacked.BasePullRequests {
					fmt.Fprintf(&report, "- Base PR #%d: %s, merged: %t, `%s` → `%s`, %s\n", pr.Number, pr.State, pr.Merged, pr.HeadRef, pr.BaseRef, repositoryIdentityEvidence(pr.HeadRepository, canonicalRepository))
				}
				for _, pr := range stacked.ChildPullRequests {
					fmt.Fprintf(&report, "- Child PR #%d: %s, merged: %t, `%s` → `%s`, %s\n", pr.Number, pr.State, pr.Merged, pr.HeadRef, pr.BaseRef, repositoryIdentityEvidence(pr.HeadRepository, canonicalRepository))
				}
			}
		}
		fmt.Fprintln(&report)
	}

	fmt.Fprintln(&report, "## Open Pull Requests")
	fmt.Fprintln(&report)
	if snapshotHasAmbiguity(snapshot, "page-cap-truncation", "open pull requests") {
		fmt.Fprintln(&report, "Open-PR inventory is incomplete; see **AMBIGUOUS**.")
	}
	if len(snapshot.OpenPullRequests) == 0 {
		fmt.Fprintln(&report, "None.")
	}
	for _, pr := range snapshot.OpenPullRequests {
		fmt.Fprintf(&report, "### #%d %s\n\n", pr.Number, pr.Title)
		fmt.Fprintf(&report, "Refs: `%s` → `%s`; %s; draft: %t.\n\n", pr.HeadRef, pr.BaseRef, repositoryIdentityEvidence(pr.HeadRepository, canonicalRepository), pr.Draft)
		if snapshotHasAmbiguity(snapshot, "page-cap-truncation", fmt.Sprintf("pull request #%d linked issues", pr.Number)) {
			fmt.Fprintln(&report, "Linked-issue evidence is incomplete because the connection reached its page cap.")
		}
		if pr.ScopeLine == "" {
			fmt.Fprintln(&report, "Scope line: not present.")
		} else {
			fmt.Fprintf(&report, "Scope line: `%s`\n", strings.ReplaceAll(pr.ScopeLine, "`", "'"))
		}
		for _, issue := range pr.LinkedIssues {
			fmt.Fprintf(&report, "\nLinked issue #%d, %s (%s)\n\n", issue.Number, issue.Title, issue.State)
			writeRawSection(&report, "Scope / declared paths", issue.ScopeSection)
			if len(issue.DeclaredPaths) > 0 {
				fmt.Fprintf(&report, "Parsed paths: `%s`\n", strings.Join(issue.DeclaredPaths, "`, `"))
			}
		}
		fmt.Fprintln(&report)
	}

	fmt.Fprintln(&report, "## Claims And Reservations")
	fmt.Fprintln(&report)
	commentsTruncated := snapshotHasAmbiguitySuffix(snapshot, "page-cap-truncation", " comments")
	if commentsTruncated {
		fmt.Fprintln(&report, "Marker-comment evidence is incomplete because at least one comments connection reached its page cap.")
	}
	if len(snapshot.MarkerComments) == 0 && !commentsTruncated {
		fmt.Fprintln(&report, "None retained.")
	} else if len(snapshot.MarkerComments) == 0 {
		fmt.Fprintln(&report, "None in retained pages; this is not a completeness claim.")
	}
	for _, marker := range snapshot.MarkerComments {
		fmt.Fprintf(&report, "- Issue #%d comment %d, %s at %s", marker.IssueNumber, marker.Stamp.DatabaseID, marker.Kind, marker.CreatedAt)
		if marker.Branch != "" {
			fmt.Fprintf(&report, ", branch `%s`", marker.Branch)
		}
		if marker.ReleasesClaimID != 0 {
			fmt.Fprintf(&report, ", releases comment %d", marker.ReleasesClaimID)
		}
		if marker.PlanIssueNumber != 0 {
			fmt.Fprintf(&report, ", reserves plan for issue #%d", marker.PlanIssueNumber)
		}
		if len(marker.MatchedOpenPullRequests) > 0 {
			fmt.Fprintf(&report, ", canonical-repository open PR matches: %v", marker.MatchedOpenPullRequests)
		}
		fmt.Fprintln(&report, ".")
	}
	fmt.Fprintln(&report)

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

func writeRawSection(report *bytes.Buffer, name, section string) {
	if section == "" {
		fmt.Fprintf(report, "%s section: not present.\n\n", name)
		return
	}
	fence := strings.Repeat("`", maximum(3, longestBacktickRun(section)+1))
	fmt.Fprintf(report, "%s section (raw):\n\n%smarkdown\n%s\n%s\n\n", name, fence, section, fence)
}

func longestBacktickRun(value string) int {
	longest, current := 0, 0
	for _, character := range value {
		if character == '`' {
			current++
			if current > longest {
				longest = current
			}
		} else {
			current = 0
		}
	}
	return longest
}

func maximum(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func snapshotHasAmbiguity(snapshot Snapshot, code, subject string) bool {
	for _, ambiguity := range snapshot.Ambiguities {
		if ambiguity.Code == code && ambiguity.Subject == subject {
			return true
		}
	}
	return false
}

func snapshotHasAmbiguityCode(snapshot Snapshot, code string) bool {
	for _, ambiguity := range snapshot.Ambiguities {
		if ambiguity.Code == code {
			return true
		}
	}
	return false
}

func snapshotHasAmbiguitySuffix(snapshot Snapshot, code, subjectSuffix string) bool {
	for _, ambiguity := range snapshot.Ambiguities {
		if ambiguity.Code == code && strings.HasSuffix(ambiguity.Subject, subjectSuffix) {
			return true
		}
	}
	return false
}

func canonicalNameWithOwner(repository string) string {
	parts := strings.SplitN(repository, "/", 2)
	if len(parts) != 2 {
		return ""
	}
	return parts[1]
}

func repositoryIdentityEvidence(identity RepositoryIdentity, canonical string) string {
	if identity.NameWithOwner == nil {
		return fmt.Sprintf("head repository: %s, canonical repository: false", identity.State)
	}
	return fmt.Sprintf("head repository: `%s`, canonical repository: %t", *identity.NameWithOwner, strings.EqualFold(*identity.NameWithOwner, canonical))
}
