package collector

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func WriteContractsArtifacts(outputDir string, snapshot ContractsSnapshot) error {
	if outputDir == "" {
		return fmt.Errorf("output directory is required")
	}
	jsonBytes, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("encode contracts snapshot: %w", err)
	}
	jsonBytes = append(jsonBytes, '\n')
	reportBytes := []byte(RenderContractsReport(snapshot))
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	if err := writeAtomic(filepath.Join(outputDir, "contracts.json"), jsonBytes); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(outputDir, "contracts.md"), reportBytes)
}

// RenderContractsReport prints the occupancy evidence. With any ambiguity it
// states no active count, finding count, or clean verdict: the units and
// findings it lists then cover retained evidence only.
func RenderContractsReport(snapshot ContractsSnapshot) string {
	var report bytes.Buffer
	stamp := "2006-01-02T15:04:05.999999999Z07:00"
	fmt.Fprintln(&report, "# Contract Occupancy Evidence")
	fmt.Fprintln(&report)
	fmt.Fprintf(&report, "Repository: `%s`  \n", snapshot.Repository)
	fmt.Fprintf(&report, "Collected: `%s` to `%s`\n", snapshot.StartedAt.Format(stamp), snapshot.CompletedAt.Format(stamp))
	fmt.Fprintln(&report)

	fmt.Fprintln(&report, "## Occupancy")
	fmt.Fprintln(&report)
	if snapshot.Complete {
		fmt.Fprintf(&report, "Active contract implementations: **%d** against a cap of **%d**.\n", *snapshot.ActiveCount, snapshot.Cap)
		fmt.Fprintf(&report, "Reserved for planning: **%d** (not counted as active).\n", *snapshot.ReservedCount)
		if *snapshot.ActiveCount > snapshot.Cap {
			fmt.Fprintln(&report, "The active count exceeds the cap.")
		}
	} else {
		fmt.Fprintf(&report, "Active contract implementations: not established against a cap of **%d**. The evidence is incomplete; see **AMBIGUOUS**. The units below come from retained evidence only.\n", snapshot.Cap)
	}
	fmt.Fprintln(&report)
	listed := 0
	for _, unit := range snapshot.Contracts {
		if contractStateIsActive(unit.State) || unit.State == ContractReserved {
			writeContractUnit(&report, unit)
			listed++
		}
	}
	if listed == 0 && snapshot.Complete {
		fmt.Fprintln(&report, "No contract unit holds an active claim or planning reservation.")
	}
	fmt.Fprintln(&report)

	fmt.Fprintln(&report, "## Findings")
	fmt.Fprintln(&report)
	switch {
	case !snapshot.Complete:
		fmt.Fprintln(&report, "No finding count or verdict: the evidence is incomplete. Findings from retained evidence:")
	case len(snapshot.Findings) == 0:
		fmt.Fprintln(&report, "Findings: **0**. The occupancy label matches claim state on every open contract unit.")
	default:
		fmt.Fprintf(&report, "Findings: **%d**.\n", len(snapshot.Findings))
	}
	for _, finding := range snapshot.Findings {
		fmt.Fprintf(&report, "- **%s** #%d (`%s`): %s\n", finding.Code, finding.Number, finding.State, finding.Detail)
	}
	fmt.Fprintln(&report)

	fmt.Fprintln(&report, "## Other Contract Units")
	fmt.Fprintln(&report)
	for _, state := range []string{ContractExpired, ContractIdle, ContractDormant} {
		for _, unit := range snapshot.Contracts {
			if unit.State == state {
				writeContractUnit(&report, unit)
			}
		}
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

func writeContractUnit(report *bytes.Buffer, unit ContractUnit) {
	label := "label absent"
	if unit.Labeled {
		label = "label present"
	}
	milestone := unit.Milestone
	if milestone == "" {
		milestone = "none"
	}
	fmt.Fprintf(report, "- #%d %s: `%s`, %s, trackers: %s, milestone: %s", unit.Number, unit.Title, unit.State, label, issueNumberList(unit.Trackers), milestone)
	if unit.Deferral {
		fmt.Fprint(report, ", deferral")
	}
	fmt.Fprintln(report)
	if unit.OrderingClaim != nil {
		fmt.Fprintf(report, "  - Ordering key: claim comment %d, created %s.\n", unit.OrderingClaim.CommentID, unit.OrderingClaim.CreatedAt)
	}
	for _, claim := range unit.Claims {
		fmt.Fprintf(report, "  - Claim comment %d at %s on `%s`: %s", claim.CommentID, claim.CreatedAt, claim.Branch, claim.Status)
		if len(claim.ClosingPullRequests) > 0 {
			fmt.Fprintf(report, ", open PR closing the issue: %s", issueNumberList(claim.ClosingPullRequests))
		}
		if len(claim.OtherPullRequests) > 0 {
			fmt.Fprintf(report, ", open PR from the branch without the close keyword: %s", issueNumberList(claim.OtherPullRequests))
		}
		fmt.Fprintln(report, ".")
	}
	if len(unit.LegacyPullRequests) > 0 {
		fmt.Fprintf(report, "  - Open PR closing the issue with no claim comment behind it: %s.\n", issueNumberList(unit.LegacyPullRequests))
	}
	for _, reservation := range unit.Reservations {
		status := "expired"
		if reservation.Active {
			status = "active"
		}
		fmt.Fprintf(report, "  - Planning reservation comment %d at %s: %s.\n", reservation.CommentID, reservation.CreatedAt, status)
	}
}

func issueNumberList(numbers []int) string {
	if len(numbers) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(numbers))
	for _, number := range numbers {
		parts = append(parts, fmt.Sprintf("#%d", number))
	}
	return strings.Join(parts, ", ")
}
