package inference_test

import (
	"encoding/json"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
)

func allSites() []inference.Site {
	budget := inference.Budget{}
	return []inference.Site{
		inference.DiagnosticSite(budget), inference.TaskNamerSite(budget),
		inference.PublicationAuthorExplainSite(budget), inference.PublicationAuthorProposeSite(budget),
		inference.ClassifierSite(budget), inference.AdjudicatorSite(budget),
		inference.DriftAuditorSite(budget), inference.DiscussionSite(budget),
	}
}

// The golden pins each site's contract digest, so an edit to a site's
// instruction or a bump of its output contract shows up as a reviewed diff
// here and never as a silent change to what calls are compared under.
func TestSiteContractDigestsGolden(t *testing.T) {
	type entry struct {
		SiteID         string        `json:"site_id"`
		OutputContract string        `json:"output_contract"`
		HasInstruction bool          `json:"has_instruction"`
		ContractDigest domain.Digest `json:"contract_digest"`
	}
	var entries []entry
	seen := map[domain.Digest]string{}
	for _, site := range allSites() {
		digest := site.ContractDigest()
		if other, dup := seen[digest]; dup {
			t.Fatalf("sites %s and %s share contract digest %s", other, site.ID, digest)
		}
		seen[digest] = site.ID
		entries = append(entries, entry{site.ID, site.OutputContract, site.Instruction != "", digest})
	}
	body, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "site_contracts", append(body, '\n'))
}

// The digest covers each part of the site contract and nothing a budget or a
// deployment sets.
func TestSiteContractDigestCoversTheContractOnly(t *testing.T) {
	site := inference.ClassifierSite(inference.Budget{})
	base := site.ContractDigest()

	reworded := site
	reworded.Instruction += " Answer briefly."
	if reworded.ContractDigest() == base {
		t.Fatal("an instruction change did not move the contract digest")
	}
	reshaped := site
	reshaped.OutputContract = "finding_classifier_output_v2"
	if reshaped.ContractDigest() == base {
		t.Fatal("an output-contract change did not move the contract digest")
	}
	if inference.ClassifierSite(testBudget(3)).ContractDigest() != base {
		t.Fatal("a budget change moved the contract digest")
	}
}
