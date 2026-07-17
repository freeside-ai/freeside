package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Artifact types the verifier emits. The verifier's evidence channel is
// its own account of the verification, never files gathered from the
// candidate workspace: under capture "none" these two artifacts are the
// entire channel, so a candidate-planted file has no path into
// evidence (§5.15: only verifier/daemon artifacts under an approved
// recipe enter evidence_snapshot).
const (
	// ArtifactTypeVerificationReport is the canonical JSON account:
	// head, recipe digest, per-step argv and exit, outcome, findings.
	ArtifactTypeVerificationReport = "verification_report"
	// ArtifactTypeCommandTranscript is the bounded combined output of
	// the recipe's commands.
	ArtifactTypeCommandTranscript = "command_transcript"
)

// Evidence is one emitted artifact with its content bytes. The caller
// persists the content (no blob store exists yet); the artifact's
// digest binds the two.
type Evidence struct {
	Artifact domain.Artifact
	Content  []byte
}

// Outcome is a verification's overall result. The zero value "" is
// invalid by design.
type Outcome string

const (
	// OutcomePassed: every recipe command exited zero.
	OutcomePassed Outcome = "passed"
	// OutcomeFailed: a recipe command exited non-zero (or was killed,
	// e.g. by the per-command timeout); later commands did not run.
	OutcomeFailed Outcome = "failed"
)

// AllOutcomes lists every valid Outcome.
var AllOutcomes = []Outcome{OutcomePassed, OutcomeFailed}

// valid is the validity predicate; as a predicate it uses default.
func (o Outcome) valid() bool {
	switch o {
	case OutcomePassed, OutcomeFailed:
		return true
	default:
		return false
	}
}

// Step is one executed recipe command's account.
type Step struct {
	Argv     []string `json:"argv"`
	ExitCode int      `json:"exit_code"`
	// OutputTruncated reports that this step's output was cut at a
	// byte cap, so the transcript is honest about being partial.
	OutputTruncated bool `json:"output_truncated"`
}

// report is the verification report's canonical JSON shape. It carries
// no timestamps, so the report bytes (and therefore the artifact
// digest) are a deterministic function of what was verified and what
// happened.
type report struct {
	HeadSHA      string        `json:"head_sha"`
	BaseSHA      string        `json:"base_sha"`
	RecipePath   string        `json:"recipe_path"`
	RecipeDigest domain.Digest `json:"recipe_digest"`
	Outcome      Outcome       `json:"outcome"`
	Steps        []Step        `json:"steps"`
	Findings     []Finding     `json:"findings"`
}

// buildEvidence stamps the verifier's account as §5.15 evidence: both
// artifacts carry verifier provenance (producer class, invocation,
// head-bound head, recipe digest), and publish eligibility originates
// only in domain.NewArtifact against the approved-recipe set. Under an
// unapproved recipe the artifacts are emitted publish-ineligible, the
// fail-closed direction; forging the bit is structurally impossible
// from here.
func buildEvidence(opts Options, recipeDigest domain.Digest, rep report, transcript []byte) ([]Evidence, error) {
	reportBytes, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal verification report: %w", err)
	}
	reportBytes = append(reportBytes, '\n')
	var out []Evidence
	for _, e := range []struct {
		artifactType string
		content      []byte
	}{
		{ArtifactTypeVerificationReport, reportBytes},
		{ArtifactTypeCommandTranscript, transcript},
	} {
		digest := contentDigest(e.content)
		artifact, err := domain.NewArtifact(domain.ArtifactInput{
			// Identity = type + invocation + content digest (§5.15 rule
			// 4's effectively-once discipline). The invocation must be
			// part of the name: two runs can emit byte-identical content
			// (a quiet transcript) while their provenance differs, and
			// the store persists immutably by ID, so a purely
			// content-derived name would make the later run's evidence
			// unstorable. Within one invocation the name stays
			// deterministic, so a replayed put is idempotent.
			ID:     domain.ArtifactID(e.artifactType + ":" + string(opts.InvocationID) + ":" + string(digest)),
			Type:   e.artifactType,
			Digest: digest,
			Provenance: domain.Provenance{
				ProducerClass:            domain.ProducerVerifier,
				ProducerInvocationID:     opts.InvocationID,
				HeadBinding:              domain.HeadBound,
				SourceHeadSHA:            opts.HeadSHA,
				VerificationRecipeDigest: &recipeDigest,
				SensitivityClass:         domain.SensitivityNormal,
			},
		}, opts.ApprovedRecipes)
		if err != nil {
			return nil, err
		}
		out = append(out, Evidence{Artifact: artifact, Content: e.content})
	}
	return out, nil
}

// contentDigest is the sha256 content address of evidence bytes.
func contentDigest(content []byte) domain.Digest {
	sum := sha256.Sum256(content)
	return domain.Digest("sha256:" + hex.EncodeToString(sum[:]))
}
