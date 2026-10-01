package inference_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/inference/fake"
)

func driftAuditorInput() inference.DriftAuditorInput {
	return inference.DriftAuditorInput{
		RunID: "run-1", Round: 6, BaseSHA: "base", HeadSHA: "head-6",
		ApprovedSpecDigest:        "sha256:" + domain.Digest(strings.Repeat("a", 64)),
		ApprovedSpecification:     "approved token-value specification",
		InstructionSnapshotDigest: "sha256:" + domain.Digest(strings.Repeat("b", 64)),
		InstructionSnapshot:       "repository token-value instructions",
		ResolvedPolicyDigest:      "sha256:" + domain.Digest(strings.Repeat("c", 64)),
		DeclaredPaths:             []string{"daemon/**"},
		RoundOneDiff:              "diff --git a/main.go b/main.go\n+token-value round one\n",
		CurrentDiff:               "diff --git a/main.go b/main.go\n+token-value current\n",
		Dispositions: []domain.ReviewDispositionRecord{{
			FindingID: fake.DriftAuditFindingID, RunID: "run-1", Round: 2,
			Disposition: domain.ReviewDispositionFixed, Reason: "wrapped the read in a retry",
			CreatedAt: time.Unix(50, 0).UTC(),
		}},
	}
}

func scriptDriftAudit(driver *fake.Driver, bodies ...string) {
	scripts := make([]fake.Script, 0, len(bodies))
	for _, body := range bodies {
		scripts = append(scripts, fake.Script{Response: inference.Response{Output: []byte(body), ComputeUnits: 1}})
	}
	driver.Script(inference.DriftAuditorSiteID, scripts...)
}

func TestDriftAuditorContractDeclaresCeilingBoundedVerdictLattice(t *testing.T) {
	classifier := inference.ClassifierSite(testBudget(10)).Annotation
	adjudicator := inference.AdjudicatorSite(testBudget(10))
	site := inference.DriftAuditorSite(testBudget(10))
	contract := site.DriftAudit
	if site.Authority != inference.AuthorityAnnotate || site.Annotation != nil ||
		site.Adjudication != nil || contract == nil {
		t.Fatalf("site authority contract = %#v", site)
	}
	if !slices.Equal(contract.Verdicts, []string{"converged", "over_hardened", "stuck"}) ||
		!slices.Equal(contract.Confidence, []string{"low", "medium", "high"}) ||
		!slices.Equal(contract.ReducesWork, []string{"over_hardened"}) {
		t.Fatalf("verdict lattice = %#v", contract)
	}
	if !slices.Equal(contract.SeverityMappings, classifier.SeverityMappings) ||
		contract.UnknownSeverityFallback != classifier.UnknownSeverityFallback ||
		!slices.Equal(contract.NormalizedSeverityCeilings, classifier.NormalizedSeverityCeilings) ||
		!slices.Equal(contract.SecondAdjudicationRules, classifier.SecondAdjudicationRules) {
		t.Fatal("drift auditor severity ceilings diverge from classifier")
	}
	// The limits are the adjudicator's, which the composition's per-root
	// allowance is sized to.
	if site.Timeout != adjudicator.Timeout || site.MaxInputBytes != adjudicator.MaxInputBytes ||
		site.MaxComputeUnits != adjudicator.MaxComputeUnits || site.AuditEvery != adjudicator.AuditEvery ||
		site.Retention != adjudicator.Retention || site.MaxOutputBytes != domain.MaxDriftAuditBytes {
		t.Fatalf("site limits = %#v", site)
	}
	if err := site.ValidateOutput([]byte(site.FailSafe)); err == nil {
		t.Fatal("fail-safe bytes pass the output schema and could become a verdict")
	}
}

func TestDriftAuditorAllowlistSendsExactlyTheDeclaredFields(t *testing.T) {
	driver := fake.New()
	scriptDriftAudit(driver, fake.DriftAuditConverged)
	client, _, _ := testClient(t, driver, 10)
	input := driftAuditorInput()
	input.DiffMetrics = &domain.ReviewRoundDiffMetrics{
		Cumulative: domain.DiffStats{
			FilesChanged: 4, Additions: 120, Deletions: 7, BaseSHA: "base", HeadSHA: "head-6",
		},
		Round: domain.DiffStats{
			FilesChanged: 1, Additions: 20, Deletions: 2, BaseSHA: "head-5", HeadSHA: "head-6",
		},
	}
	if _, err := client.AuditDrift(context.Background(), "project-1", "run-1", input); err != nil {
		t.Fatal(err)
	}
	requests := driver.Requests()
	if len(requests) != 1 {
		t.Fatalf("requests = %#v", requests)
	}
	fields := requests[0].Fields
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	slices.Sort(names)
	want := []string{
		"adjudication_entries", "approved_spec", "approved_spec_digest", "current_diff",
		"declared_paths", "diff_metrics", "disposition_history", "instruction_snapshot",
		"instruction_snapshot_digest", "resolved_policy_digest", "round", "round_one_diff", "run_id",
	}
	if !slices.Equal(names, want) {
		t.Fatalf("sent fields = %v, want %v", names, want)
	}
	if fields["run_id"] != "run-1" || fields["round"] != "6" ||
		fields["approved_spec"] != "approved [REDACTED] specification" ||
		fields["instruction_snapshot"] != "repository [REDACTED] instructions" ||
		!strings.Contains(fields["round_one_diff"], "[REDACTED] round one") ||
		!strings.Contains(fields["current_diff"], "[REDACTED] current") ||
		fields["disposition_history"] != string(mustJSON(t, input.Dispositions)) ||
		fields["adjudication_entries"] != "null" ||
		fields["diff_metrics"] != string(mustJSON(t, input.DiffMetrics)) {
		t.Fatalf("fields = %#v", fields)
	}
	if requests[0].InputDigest != contentaddr.Sum(mustJSON(t, fields)) {
		t.Fatal("input digest does not bind drift auditor fields")
	}
}

func TestDriftAuditorSendsNullForAMetricsGap(t *testing.T) {
	driver := fake.New()
	scriptDriftAudit(driver, fake.DriftAuditConverged)
	client, _, _ := testClient(t, driver, 10)
	if _, err := client.AuditDrift(context.Background(), "project-1", "run-1", driftAuditorInput()); err != nil {
		t.Fatal(err)
	}
	if got := driver.Requests()[0].Fields["diff_metrics"]; got != "null" {
		t.Fatalf("diff_metrics = %q", got)
	}
}

func TestDriftAuditorVerdictFixturesReturnBoundArtifacts(t *testing.T) {
	cases := map[string]struct {
		body       string
		verdict    domain.DriftVerdict
		confidence domain.AdjudicationConfidence
		reversals  []domain.FindingID
	}{
		"converged": {fake.DriftAuditConverged, domain.DriftVerdictConverged, domain.ConfidenceHigh, nil},
		"over hardened": {
			fake.DriftAuditOverHardened, domain.DriftVerdictOverHardened, domain.ConfidenceHigh,
			[]domain.FindingID{fake.DriftAuditFindingID},
		},
		"stuck": {fake.DriftAuditStuck, domain.DriftVerdictStuck, domain.ConfidenceMedium, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			driver := fake.New()
			scriptDriftAudit(driver, tc.body)
			client, _, _ := testClient(t, driver, 10)
			input := driftAuditorInput()
			audit, err := client.AuditDrift(context.Background(), "project-1", "run-1", input)
			if err != nil {
				t.Fatal(err)
			}
			if audit.RunID != input.RunID || audit.Round != input.Round ||
				audit.BaseSHA != input.BaseSHA || audit.HeadSHA != input.HeadSHA ||
				audit.ApprovedSpecDigest != input.ApprovedSpecDigest ||
				audit.ResolvedPolicyDigest != input.ResolvedPolicyDigest ||
				!audit.CreatedAt.Equal(time.Unix(100, 0)) {
				t.Fatalf("artifact binding = %#v", audit)
			}
			var cited []domain.FindingID
			for _, reversal := range audit.Reversals {
				cited = append(cited, reversal.FindingID)
			}
			if audit.Verdict != tc.verdict || audit.Confidence != tc.confidence ||
				!slices.Equal(cited, tc.reversals) {
				t.Fatalf("artifact verdict = %#v", audit)
			}
			if err := audit.Validate(); err != nil {
				t.Fatal(err)
			}
			encoded, err := audit.Encode()
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := domain.DecodeDriftAudit(encoded)
			if err != nil || !reflect.DeepEqual(decoded, audit) {
				t.Fatalf("round trip = %#v, %v, want %#v", decoded, err, audit)
			}
		})
	}
}

// A low-confidence verdict is a real verdict: the site returns the artifact
// and the routing unit decides to park it.
func TestDriftAuditorReturnsLowConfidenceVerdict(t *testing.T) {
	driver := fake.New()
	scriptDriftAudit(driver, strings.Replace(
		fake.DriftAuditOverHardened, `"confidence":"high"`, `"confidence":"low"`, 1))
	client, _, _ := testClient(t, driver, 10)
	audit, err := client.AuditDrift(context.Background(), "project-1", "run-1", driftAuditorInput())
	if err != nil || audit.Verdict != domain.DriftVerdictOverHardened ||
		audit.Confidence != domain.ConfidenceLow || audit.Validate() != nil {
		t.Fatalf("AuditDrift = %#v, %v", audit, err)
	}
}

// A finding from the round's own batch can have an adjudication entry and no
// disposition yet; the route gate parks that citation as a real verdict.
func TestDriftAuditorAcceptsCitationFoundOnlyInAdjudicationEntries(t *testing.T) {
	entry, err := domain.NewModelAdjudicationEntry(
		fake.DriftAuditFindingID, domain.GoalAdjacent, nil, domain.RouteDefer, domain.ConfidenceHigh,
		"outside the accepted outcome", nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	driver := fake.New()
	scriptDriftAudit(driver, fake.DriftAuditOverHardened)
	client, _, _ := testClient(t, driver, 10)
	input := driftAuditorInput()
	input.Dispositions = nil
	input.AdjudicationEntries = []domain.FindingAdjudicationEntry{entry}
	audit, err := client.AuditDrift(context.Background(), "project-1", "run-1", input)
	if err != nil || len(audit.Reversals) != 1 || audit.Reversals[0].FindingID != fake.DriftAuditFindingID {
		t.Fatalf("AuditDrift = %#v, %v", audit, err)
	}
}

func TestDriftAuditorExtremeOutputsReturnTypedUnavailable(t *testing.T) {
	reversal := func(id, undo, rationale string) string {
		return `{"finding_id":"` + id + `","undo":"` + undo + `","rationale":"` + rationale + `"}`
	}
	body := func(verdict, confidence, reversals, explanation string) []byte {
		return []byte(`{"verdict":` + verdict + `,"confidence":` + confidence +
			`,"reversals":` + reversals + `,"explanation":` + explanation + `}`)
	}
	one := "[" + reversal("finding-1", "remove it", "not needed") + "]"
	// Under the output bound as a response, over the artifact bound once the
	// binding fields are added.
	nearBound := body(`"converged"`, `"high"`, `[]`, `"`+strings.Repeat("x", domain.MaxDriftAuditBytes-80)+`"`)
	if len(nearBound) > domain.MaxDriftAuditBytes {
		t.Fatalf("near-bound response is %d bytes, over the output bound", len(nearBound))
	}
	cases := map[string][]byte{
		"malformed fixture":       []byte(fake.DriftAuditMalformed),
		"unknown finding fixture": []byte(fake.DriftAuditUnknownFinding),
		"fail-safe bytes":         []byte(inference.DriftAuditorSite(testBudget(10)).FailSafe),
		"unknown verdict":         body(`"simplify"`, `"high"`, `[]`, `"x"`),
		"empty verdict":           body(`""`, `"high"`, `[]`, `"x"`),
		"null verdict":            body(`null`, `"high"`, `[]`, `"x"`),
		"unknown confidence":      body(`"converged"`, `"certain"`, `[]`, `"x"`),
		"null confidence":         body(`"converged"`, `null`, `[]`, `"x"`),
		"missing confidence":      []byte(`{"verdict":"converged","reversals":[],"explanation":"x"}`),
		"missing verdict":         []byte(`{"confidence":"high","reversals":[],"explanation":"x"}`),
		"missing reversals":       []byte(`{"verdict":"converged","confidence":"high","explanation":"x"}`),
		"null reversals":          body(`"converged"`, `"high"`, `null`, `"x"`),
		"missing explanation":     []byte(`{"verdict":"converged","confidence":"high","reversals":[]}`),
		"empty explanation":       body(`"converged"`, `"high"`, `[]`, `" "`),
		"over_hardened with none": body(`"over_hardened"`, `"high"`, `[]`, `"x"`),
		"converged with reversal": body(`"converged"`, `"high"`, one, `"x"`),
		"stuck with reversal":     body(`"stuck"`, `"high"`, one, `"x"`),
		"repeated finding": body(`"over_hardened"`, `"high"`,
			"["+reversal("finding-1", "a", "b")+","+reversal("finding-1", "c", "d")+"]", `"x"`),
		"empty finding id": body(`"over_hardened"`, `"high"`, "["+reversal("", "a", "b")+"]", `"x"`),
		"empty undo":       body(`"over_hardened"`, `"high"`, "["+reversal("finding-1", " ", "b")+"]", `"x"`),
		"empty rationale":  body(`"over_hardened"`, `"high"`, "["+reversal("finding-1", "a", "")+"]", `"x"`),
		"missing undo": body(`"over_hardened"`, `"high"`,
			`[{"finding_id":"finding-1","rationale":"b"}]`, `"x"`),
		"unknown reversal field": body(`"over_hardened"`, `"high"`,
			`[{"finding_id":"finding-1","undo":"a","rationale":"b","surface":"main.go"}]`, `"x"`),
		// encoding/json folds key case, so these are neither unknown fields nor
		// byte-exact duplicates to the strict decoder.
		"case-folded duplicate": []byte(`{"verdict":"converged","Verdict":"stuck","confidence":"high","reversals":[],"explanation":"x"}`),
		"case-folded override":  []byte(`{"verdict":"converged","confidence":"high","reversals":[],"explanation":"x","VERDICT":"over_hardened","REVERSALS":` + one + `}`),
		"non-canonical key":     []byte(`{"VERDICT":"converged","confidence":"high","reversals":[],"explanation":"x"}`),
		"unicode-folded key":    []byte(`{"verdict":"converged","confidence":"high","reverſals":[],"explanation":"x"}`),
		"case-folded reversal key": body(`"over_hardened"`, `"high"`,
			`[{"finding_id":"finding-unknown","Finding_Id":"finding-1","undo":"a","rationale":"b"}]`, `"x"`),
		"null reversal":        body(`"over_hardened"`, `"high"`, `[null]`, `"x"`),
		"duplicate key":        []byte(`{"verdict":"stuck","verdict":"converged","confidence":"high","reversals":[],"explanation":"x"}`),
		"unknown field":        []byte(`{"verdict":"converged","confidence":"high","reversals":[],"explanation":"x","digest":"sha256:0"}`),
		"trailing data":        append(body(`"converged"`, `"high"`, `[]`, `"x"`), []byte(` {}`)...),
		"not an object":        []byte(`["converged"]`),
		"empty response":       nil,
		"invalid utf8":         append(body(`"converged"`, `"high"`, `[]`, `"x"`), 0xff),
		"invalid utf8 in text": body(`"converged"`, `"high"`, `[]`, "\"x\xff\""),
		"oversized response": body(`"converged"`, `"high"`, `[]`,
			`"`+strings.Repeat("x", domain.MaxDriftAuditBytes)+`"`),
		"oversized artifact": nearBound,
	}
	for name, output := range cases {
		t.Run(name, func(t *testing.T) {
			driver := fake.New()
			driver.Script(inference.DriftAuditorSiteID, fake.Script{Response: inference.Response{Output: output}})
			client, _, _ := testClient(t, driver, 10)
			audit, err := client.AuditDrift(context.Background(), "project-1", "run-1", driftAuditorInput())
			if !errors.Is(err, inference.ErrDriftAuditNotAvailable) || !reflect.DeepEqual(audit, domain.DriftAudit{}) {
				t.Fatalf("AuditDrift = %#v, %v", audit, err)
			}
		})
	}
}

func TestDriftAuditorCallFailuresReturnTypedUnavailable(t *testing.T) {
	overLimit := driftAuditorInput()
	overLimit.CurrentDiff = strings.Repeat("x", 2<<20)
	binaryDiff := driftAuditorInput()
	binaryDiff.RoundOneDiff = "GIT binary patch\n\xff\xfe"
	unboundRound := driftAuditorInput()
	unboundRound.Round = 0
	cases := map[string]struct {
		driver inference.Driver
		input  inference.DriftAuditorInput
		calls  int
	}{
		"inference down": {nil, driftAuditorInput(), 0},
		"driver error": {func() inference.Driver {
			driver := fake.New()
			driver.Script(inference.DriftAuditorSiteID, fake.Script{Err: errors.New("provider refused")})
			return driver
		}(), driftAuditorInput(), 1},
		"over compute bound": {func() inference.Driver {
			driver := fake.New()
			driver.Script(inference.DriftAuditorSiteID, fake.Script{Response: inference.Response{
				Output: []byte(fake.DriftAuditConverged), ComputeUnits: 10_001,
			}})
			return driver
		}(), driftAuditorInput(), 1},
		// An input over the limit is never shortened to fit.
		"input over limit": {fake.New(), overLimit, 0},
		"diff not utf8":    {fake.New(), binaryDiff, 0},
		// The artifact cannot bind a round the input does not name.
		"unbound round": {func() inference.Driver {
			driver := fake.New()
			scriptDriftAudit(driver, fake.DriftAuditConverged)
			return driver
		}(), unboundRound, 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			client, _, _ := testClient(t, tc.driver, 10)
			audit, err := client.AuditDrift(context.Background(), "project-1", "run-1", tc.input)
			if !errors.Is(err, inference.ErrDriftAuditNotAvailable) || !reflect.DeepEqual(audit, domain.DriftAudit{}) {
				t.Fatalf("AuditDrift = %#v, %v", audit, err)
			}
			if driver, ok := tc.driver.(*fake.Driver); ok && len(driver.Requests()) != tc.calls {
				t.Fatalf("provider calls = %d, want %d", len(driver.Requests()), tc.calls)
			}
		})
	}
}

func TestDriftAuditorUnscopedCallReturnsTypedUnavailable(t *testing.T) {
	driver := fake.New()
	scriptDriftAudit(driver, fake.DriftAuditConverged)
	client, _, _ := testClient(t, driver, 10)
	audit, err := client.AuditDrift(context.Background(), "project-1", "", driftAuditorInput())
	if !errors.Is(err, inference.ErrDriftAuditNotAvailable) || !reflect.DeepEqual(audit, domain.DriftAudit{}) ||
		len(driver.Requests()) != 0 {
		t.Fatalf("AuditDrift = %#v, %v", audit, err)
	}
}

// The site's deadline is a real context timer in Client.Call, so the test
// runs in a synctest bubble instead of waiting out 120 seconds.
func TestDriftAuditorTimeoutReturnsTypedUnavailable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		driver := fake.New()
		driver.Script(inference.DriftAuditorSiteID, fake.Script{Wait: make(chan struct{})})
		client, _, _ := testClient(t, driver, 10)
		started := time.Now()
		audit, err := client.AuditDrift(t.Context(), "project-1", "run-1", driftAuditorInput())
		if !errors.Is(err, inference.ErrDriftAuditNotAvailable) || !reflect.DeepEqual(audit, domain.DriftAudit{}) {
			t.Fatalf("AuditDrift = %#v, %v", audit, err)
		}
		if waited := time.Since(started); waited != 120*time.Second {
			t.Fatalf("waited %v, want the site's 120s deadline", waited)
		}
		// Let the abandoned driver call observe its cancellation and exit.
		synctest.Wait()
	})
}

func TestDriftAuditorAndAdjudicatorShareRootBudget(t *testing.T) {
	driver := fake.New()
	driver.Script(inference.AdjudicatorSiteID, fake.Script{Response: inference.Response{
		Output: []byte(acceptedAdjudicatorOutput), ComputeUnits: 1,
	}})
	scriptDriftAudit(driver, fake.DriftAuditConverged)
	client, _, _ := testClient(t, driver, 1)
	if _, err := client.AdjudicateFindings(
		context.Background(), "project-1", "run-1", adjudicatorInput()); err != nil {
		t.Fatal(err)
	}
	audit, err := client.AuditDrift(context.Background(), "project-1", "run-1", driftAuditorInput())
	if !errors.Is(err, inference.ErrDriftAuditNotAvailable) || !reflect.DeepEqual(audit, domain.DriftAudit{}) {
		t.Fatalf("second site call = %#v, %v", audit, err)
	}
	if got := len(driver.Requests()); got != 1 {
		t.Fatalf("provider calls = %d, want one cumulative-root-bounded call", got)
	}
}

func TestDriftAuditorRepeatedCallsStopAtCumulativeBound(t *testing.T) {
	driver := fake.New()
	scriptDriftAudit(driver, fake.DriftAuditStuck, fake.DriftAuditStuck, fake.DriftAuditStuck)
	client, _, _ := testClient(t, driver, 2)
	for call := 1; call <= 3; call++ {
		audit, err := client.AuditDrift(context.Background(), "project-1", "run-1", driftAuditorInput())
		if call <= 2 && (err != nil || audit.Verdict != domain.DriftVerdictStuck) {
			t.Fatalf("call %d = %#v, %v", call, audit, err)
		}
		if call == 3 && (!errors.Is(err, inference.ErrDriftAuditNotAvailable) ||
			!reflect.DeepEqual(audit, domain.DriftAudit{})) {
			t.Fatalf("bounded call = %#v, %v", audit, err)
		}
	}
	if got := len(driver.Requests()); got != 2 {
		t.Fatalf("provider calls = %d, want two bounded calls", got)
	}
}
