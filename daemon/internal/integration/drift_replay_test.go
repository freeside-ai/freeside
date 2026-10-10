package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/advisory"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/inference/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// The drift replay (#1053) audits real over-hardened pull requests from this
// repository's history and compares the audit's reversal list with the owner's
// own judgment. Codex reviewed those pull requests on GitHub and no Freeside
// run did, so each fixture under testdata/drift_replay is a hand-built audit
// input: its manifest names the pull request, the commits, and the commands
// that produced the files.
//
// The replay audits the merged head as the round after the last round with
// findings, so every finding can be cited. Production audits only a round
// that has findings.

const (
	driftReplayRoot    = "testdata/drift_replay"
	driftReplayProject = "freeside-drift-replay"
)

// driftReplayManifest names what a fixture was built from. A rebased pull
// request has two bases, so the round-1 diff records its own pair.
type driftReplayManifest struct {
	PR                  int                           `json:"pr"`
	Title               string                        `json:"title"`
	SpecificationSource string                        `json:"specification_source"`
	InstructionsSource  string                        `json:"instructions_source"`
	BaseSHA             string                        `json:"base_sha"`
	HeadSHA             string                        `json:"head_sha"`
	RoundOneBaseSHA     string                        `json:"round_one_base_sha"`
	RoundOneHeadSHA     string                        `json:"round_one_head_sha"`
	LastFindingRound    int                           `json:"last_finding_round"`
	DeclaredPaths       []string                      `json:"declared_paths"`
	DiffMetrics         domain.ReviewRoundDiffMetrics `json:"diff_metrics"`
	Commands            []string                      `json:"commands"`
}

// driftReplayFinding is one Codex inline comment and its outcome, transcribed
// from the pull request's review thread. Title and the URLs are for the reader
// checking the transcription; the auditor is shown only the disposition.
type driftReplayFinding struct {
	ID          domain.FindingID         `json:"id"`
	Round       int                      `json:"round"`
	Severity    domain.FindingSeverity   `json:"severity"`
	Path        string                   `json:"path"`
	Title       string                   `json:"title"`
	CommentURL  string                   `json:"comment_url"`
	Disposition domain.ReviewDisposition `json:"disposition"`
	Reason      string                   `json:"reason"`
	ReplyURL    string                   `json:"reply_url"`
	DisposedAt  time.Time                `json:"disposed_at"`
}

type driftReplayHistory struct {
	Findings []driftReplayFinding `json:"findings"`
}

type driftReplayExpectedReversal struct {
	FindingID domain.FindingID `json:"finding_id"`
	Note      string           `json:"note"`
}

type driftReplayAssessment struct {
	FindingID  domain.FindingID                    `json:"finding_id"`
	Assessment domain.ClassifierAccuracyAssessment `json:"assessment"`
	Note       string                              `json:"note"`
}

// driftReplayOwnerJudgment holds the owner's side of the comparison. The
// expectation is written before the live run, so the audit's answer cannot
// anchor it; ExpectationSource says where that earlier record is. The
// assessments mark each reversal the audit proposed, in the shadow arm's words.
type driftReplayOwnerJudgment struct {
	ExpectationSource   string                        `json:"expectation_source"`
	ExpectedVerdict     domain.DriftVerdict           `json:"expected_verdict"`
	ExpectedReversals   []driftReplayExpectedReversal `json:"expected_reversals"`
	ReversalAssessments []driftReplayAssessment       `json:"reversal_assessments"`
}

// driftReplayRecording is one live run's raw answer.
type driftReplayRecording struct {
	Model        string          `json:"model"`
	RecordedAt   time.Time       `json:"recorded_at"`
	DaemonCommit string          `json:"daemon_commit"`
	ComputeUnits int64           `json:"compute_units"`
	Output       json.RawMessage `json:"output"`
}

type driftReplayFixture struct {
	Dir           string
	Manifest      driftReplayManifest
	Specification string
	Instructions  string
	RoundOneDiff  string
	CurrentDiff   string
	History       driftReplayHistory
	// Judgment and Recording are nil until the owner and a live run supply them.
	Judgment  *driftReplayOwnerJudgment
	Recording *driftReplayRecording
}

func loadDriftReplayFixtures(t *testing.T) []driftReplayFixture {
	t.Helper()
	entries, err := os.ReadDir(driftReplayRoot)
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []driftReplayFixture
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(driftReplayRoot, entry.Name())
		fixture := driftReplayFixture{
			Dir:           dir,
			Specification: readDriftReplayFile(t, dir, "spec.md"),
			Instructions:  readDriftReplayFile(t, dir, "instructions.md"),
			RoundOneDiff:  readDriftReplayFile(t, dir, "round_one.patch"),
			CurrentDiff:   readDriftReplayFile(t, dir, "final.patch"),
		}
		readDriftReplayJSON(t, dir, "manifest.json", &fixture.Manifest)
		readDriftReplayJSON(t, dir, "history.json", &fixture.History)
		if driftReplayFileExists(t, dir, "owner_judgment.json") {
			fixture.Judgment = new(driftReplayOwnerJudgment)
			readDriftReplayJSON(t, dir, "owner_judgment.json", fixture.Judgment)
		}
		if driftReplayFileExists(t, dir, "recorded_response.json") {
			fixture.Recording = new(driftReplayRecording)
			readDriftReplayJSON(t, dir, "recorded_response.json", fixture.Recording)
		}
		if want := fmt.Sprintf("pr-%d", fixture.Manifest.PR); entry.Name() != want {
			t.Fatalf("%s: manifest names pull request %d", dir, fixture.Manifest.PR)
		}
		fixtures = append(fixtures, fixture)
	}
	slices.SortFunc(fixtures, func(a, b driftReplayFixture) int { return a.Manifest.PR - b.Manifest.PR })
	return fixtures
}

func readDriftReplayFile(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // G304: committed fixture under testdata, named by the test.
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func readDriftReplayJSON(t *testing.T, dir, name string, dst any) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader([]byte(readDriftReplayFile(t, dir, name))))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		t.Fatalf("%s: %v", filepath.Join(dir, name), err)
	}
}

func driftReplayFileExists(t *testing.T, dir, name string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, name))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

func (f driftReplayFixture) runID() domain.RunID {
	return domain.RunID(fmt.Sprintf("replay-pr-%d", f.Manifest.PR))
}

// auditedRound is the round after the last one with findings, so the audit
// judges the merged head and may cite every finding (issue #1053). Production
// never audits such a round: it audits only a round with findings, and the
// gate refuses a reversal of that round's own batch. The gate answer pinned
// here is therefore the store gate's on a round no run reaches, and says
// nothing of the engine's further conditions.
func (f driftReplayFixture) auditedRound() int { return f.Manifest.LastFindingRound + 1 }

// dispositions renders the transcribed outcomes as the daemon's records. The
// remediation and adjudication bindings are synthetic: GitHub review had no
// remediation invocation and no adjudicator, and a valid record needs one or
// the other.
func (f driftReplayFixture) dispositions(t *testing.T) []domain.ReviewDispositionRecord {
	t.Helper()
	seen := make(map[domain.FindingID]bool, len(f.History.Findings))
	records := make([]domain.ReviewDispositionRecord, 0, len(f.History.Findings))
	for _, finding := range f.History.Findings {
		record := domain.ReviewDispositionRecord{
			FindingID: finding.ID, RunID: f.runID(), Round: finding.Round,
			Disposition: finding.Disposition, Reason: finding.Reason, CreatedAt: finding.DisposedAt,
		}
		if finding.Disposition == domain.ReviewDispositionFixed {
			record.RemediationInvocationID = domain.InvocationID(
				fmt.Sprintf("%s-remediation-%d", f.runID(), finding.Round))
		} else {
			record.AdjudicationDigest = domain.Digest(contentaddr.Sum([]byte(finding.Reason)))
		}
		if err := record.Validate(); err != nil {
			t.Fatalf("%s: finding %s: %v", f.Dir, finding.ID, err)
		}
		if seen[finding.ID] || finding.Round > f.Manifest.LastFindingRound {
			t.Fatalf("%s: finding %s is repeated or outside the reviewed rounds", f.Dir, finding.ID)
		}
		seen[finding.ID] = true
		records = append(records, record)
	}
	return records
}

func (f driftReplayFixture) auditInput(t *testing.T) inference.DriftAuditorInput {
	t.Helper()
	metrics := f.Manifest.DiffMetrics
	if err := metrics.Validate(); err != nil {
		t.Fatalf("%s: %v", f.Dir, err)
	}
	if metrics.Cumulative.BaseSHA != f.Manifest.BaseSHA || metrics.Cumulative.HeadSHA != f.Manifest.HeadSHA {
		t.Fatalf("%s: diff metrics name other commits than the manifest", f.Dir)
	}
	return inference.DriftAuditorInput{
		RunID: f.runID(), Round: f.auditedRound(),
		BaseSHA: f.Manifest.BaseSHA, HeadSHA: f.Manifest.HeadSHA,
		ApprovedSpecDigest:        domain.Digest(contentaddr.Sum([]byte(f.Specification))),
		ApprovedSpecification:     f.Specification,
		InstructionSnapshotDigest: domain.Digest(contentaddr.Sum([]byte(f.Instructions))),
		InstructionSnapshot:       f.Instructions,
		ResolvedPolicyDigest:      driftReplayPolicy(f.auditedRound()).Digest,
		DeclaredPaths:             f.Manifest.DeclaredPaths,
		RoundOneDiff:              f.RoundOneDiff, CurrentDiff: f.CurrentDiff,
		Dispositions: f.dispositions(t),
		DiffMetrics:  &metrics,
	}
}

// driftReplayPolicy is the policy the replay asks the route gate about:
// drift_audit_route auto at the recorded medium threshold. These pull requests
// never ran under a round limit, so the limit sits one round past the audited
// one and is never what parks a verdict.
func driftReplayPolicy(auditedRound int) store.ReviewConvergencePolicy {
	return store.ReviewConvergencePolicy{
		Digest:                          domain.Digest(contentaddr.Sum([]byte("drift replay: drift_audit_route=auto"))),
		HardRoundLimit:                  auditedRound + 1,
		DriftAuditRoute:                 domain.DriftAuditRouteAuto,
		AdjudicationConfidenceThreshold: domain.DispatchThresholdMedium,
	}
}

func (f driftReplayFixture) routeInput(t *testing.T, audit domain.DriftAudit) store.DriftRouteInput {
	t.Helper()
	input := store.DriftRouteInput{
		Audit: audit,
		// The audited round reported nothing, so its batch is empty.
		Record: domain.ReviewRecord{
			RunID: f.runID(), Round: f.auditedRound(),
			BaseSHA: f.Manifest.BaseSHA, HeadSHA: f.Manifest.HeadSHA,
		},
		Policy:   driftReplayPolicy(f.auditedRound()),
		Findings: make(map[domain.FindingID]domain.Finding, len(f.History.Findings)),
	}
	for index, record := range f.dispositions(t) {
		finding := f.History.Findings[index]
		input.Findings[finding.ID] = domain.Finding{
			ID: finding.ID, RunID: f.runID(), Source: "codex", Severity: finding.Severity,
		}
		input.Effective = append(input.Effective, store.EffectiveFindingDisposition{
			Stored: record, Effective: record.Disposition,
		})
	}
	return input
}

// driftReplayClient builds an inference client around the real drift-auditor
// site, with the advisory sizing the daemon composes.
func driftReplayClient(t *testing.T, roles fake.Roles, now func() time.Time) *inference.Client {
	t.Helper()
	dir := t.TempDir()
	claims, err := advisory.Open(filepath.Join(dir, "advisory.json"), 2_000, 64<<10, advisory.WithClock(now))
	if err != nil {
		t.Fatal(err)
	}
	limits := inference.Limits{Calls: 1, ComputeUnits: 10_000, AttentionItems: 1, Starvation: time.Hour}
	client, err := inference.New(inference.Config{
		StatePath: filepath.Join(dir, "ledger.json"),
		Roles:     roles,
		Sites: []inference.Site{inference.DriftAuditorSite(inference.Budget{
			Window: time.Hour, Site: limits, Project: limits, Global: limits,
			MaxCallsPerRoot: 1, MaxStarvationPerRoot: time.Hour,
		})},
		Advisory: claims, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

type driftReplayReversalSummary struct {
	FindingID  domain.FindingID                    `json:"finding_id"`
	Severity   domain.FindingSeverity              `json:"severity"`
	Assessment domain.ClassifierAccuracyAssessment `json:"assessment"`
}

type driftReplayPRSummary struct {
	PR              int                           `json:"pr"`
	InputBytes      int                           `json:"input_bytes"`
	Verdict         domain.DriftVerdict           `json:"verdict"`
	Confidence      domain.AdjudicationConfidence `json:"confidence"`
	ExpectedVerdict domain.DriftVerdict           `json:"expected_verdict"`
	VerdictAgrees   bool                          `json:"verdict_agrees"`
	Reversals       []driftReplayReversalSummary  `json:"reversals"`
	Missed          []domain.FindingID            `json:"missed"`
	ListValid       bool                          `json:"list_valid"`
	Auto            bool                          `json:"auto"`
}

// driftReplayAgreement pools the owner's marks across the replayed pull
// requests. The agreement rate is Accurate over Accurate plus Inaccurate.
type driftReplayAgreement struct {
	Accurate      int `json:"accurate"`
	Inaccurate    int `json:"inaccurate"`
	Indeterminate int `json:"indeterminate"`
	Missed        int `json:"missed"`
}

type driftReplaySummary struct {
	InputLimitBytes int                    `json:"input_limit_bytes"`
	PullRequests    []driftReplayPRSummary `json:"pull_requests"`
	Agreement       driftReplayAgreement   `json:"agreement"`
}

// summarize joins the audit, the gate's answer, and the owner's judgment. It
// fails unless the owner marked exactly the reversals the audit proposed.
func (f driftReplayFixture) summarize(
	t *testing.T, audit domain.DriftAudit, gate store.DriftRouteGate, inputBytes int,
) driftReplayPRSummary {
	t.Helper()
	judgment := f.Judgment
	severities := make(map[domain.FindingID]domain.FindingSeverity, len(f.History.Findings))
	for _, finding := range f.History.Findings {
		severities[finding.ID] = finding.Severity
	}
	marks := make(map[domain.FindingID]domain.ClassifierAccuracyAssessment, len(judgment.ReversalAssessments))
	for _, mark := range judgment.ReversalAssessments {
		if !slices.Contains(domain.AllClassifierAccuracyAssessments, mark.Assessment) {
			t.Fatalf("%s: assessment %q for %s is not one of the shadow arm's", f.Dir, mark.Assessment, mark.FindingID)
		}
		marks[mark.FindingID] = mark.Assessment
	}
	if len(marks) != len(judgment.ReversalAssessments) || len(marks) != len(audit.Reversals) {
		t.Fatalf("%s: the owner marked %d reversals and the audit proposed %d",
			f.Dir, len(judgment.ReversalAssessments), len(audit.Reversals))
	}
	summary := driftReplayPRSummary{
		PR: f.Manifest.PR, InputBytes: inputBytes,
		Verdict: audit.Verdict, Confidence: audit.Confidence,
		ExpectedVerdict: judgment.ExpectedVerdict,
		VerdictAgrees:   audit.Verdict == judgment.ExpectedVerdict,
		Reversals:       []driftReplayReversalSummary{},
		Missed:          []domain.FindingID{},
		ListValid:       gate.ListValid, Auto: gate.Auto,
	}
	proposed := make(map[domain.FindingID]bool, len(audit.Reversals))
	for _, reversal := range audit.Reversals {
		mark, marked := marks[reversal.FindingID]
		if !marked {
			t.Fatalf("%s: the owner did not mark proposed reversal %s", f.Dir, reversal.FindingID)
		}
		proposed[reversal.FindingID] = true
		summary.Reversals = append(summary.Reversals, driftReplayReversalSummary{
			FindingID: reversal.FindingID, Severity: severities[reversal.FindingID], Assessment: mark,
		})
	}
	for _, expected := range judgment.ExpectedReversals {
		if _, known := severities[expected.FindingID]; !known {
			t.Fatalf("%s: expected reversal %s is not in the history", f.Dir, expected.FindingID)
		}
		if !proposed[expected.FindingID] {
			summary.Missed = append(summary.Missed, expected.FindingID)
		}
	}
	return summary
}

func (a *driftReplayAgreement) add(summary driftReplayPRSummary) {
	a.Missed += len(summary.Missed)
	for _, reversal := range summary.Reversals {
		// The switch dispatches on the assessment, so it omits default; summarize
		// already refused a value outside the set.
		switch reversal.Assessment {
		case domain.ClassifierAssessmentAccurate:
			a.Accurate++
		case domain.ClassifierAssessmentInaccurate:
			a.Inaccurate++
		case domain.ClassifierAssessmentIndeterminate:
			a.Indeterminate++
		}
	}
}

// TestDriftReplay replays each recorded live answer through the real site and
// the real route gate. The site re-runs its size limit, schema check, and
// citation check on the real input, and the golden pins the verdicts, the
// gate's answers under drift_audit_route auto, and the owner's marks.
func TestDriftReplay(t *testing.T) {
	fixtures := loadDriftReplayFixtures(t)
	if len(fixtures) == 0 {
		t.Fatal("no drift replay fixtures")
	}
	now := func() time.Time { return time.Unix(100, 0).UTC() }
	summary := driftReplaySummary{
		InputLimitBytes: inference.DriftAuditorSite(inference.Budget{}).MaxInputBytes,
	}
	for _, fixture := range fixtures {
		if fixture.Judgment == nil || fixture.Recording == nil {
			t.Fatalf("%s: the owner's judgment and a recorded response are both required", fixture.Dir)
		}
		driver := fake.New()
		driver.Script(inference.DriftAuditorSiteID, fake.Script{Response: inference.Response{
			Output: fixture.Recording.Output, ComputeUnits: fixture.Recording.ComputeUnits,
		}})
		client := driftReplayClient(t, fake.Roles{
			Provider: "fake", Model: fixture.Recording.Model,
			Credential: "token-value", Driver: driver,
		}, now)
		input := fixture.auditInput(t)
		audit, err := client.AuditDrift(context.Background(), driftReplayProject, string(input.RunID), input)
		if err != nil {
			t.Fatalf("%s: %v", fixture.Dir, err)
		}
		sent, err := json.Marshal(driver.Requests()[0].Fields)
		if err != nil {
			t.Fatal(err)
		}
		gate := store.EvaluateDriftAuditRoute(fixture.routeInput(t, audit))
		prSummary := fixture.summarize(t, audit, gate, len(sent))
		summary.PullRequests = append(summary.PullRequests, prSummary)
		summary.Agreement.add(prSummary)
	}
	got, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "drift_replay/summary", append(got, '\n'))
}
