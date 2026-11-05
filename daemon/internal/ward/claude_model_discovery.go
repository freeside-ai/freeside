package ward

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Model discovery (issue #2594) asks an enrollment's pinned Claude CLI which
// models and effort levels it can run. The CLI lists them in its answer to the
// control protocol's initialize request, which it gives without running a
// turn; the stream's system/init record carries no model list. The probe runs
// as an initialize lineup probe, under the same containment and credential
// handling, and the answer feeds a picker and the selection check only.

// The ways a model report is refused. Each keeps the enrollment's last
// catalog; none quotes the CLI's output, which is logged.
var (
	// ErrNoModelReport is a transcript with no answer to the request.
	ErrNoModelReport = errors.New("the CLI did not answer the initialize request")
	// ErrModelReportRefused is an answer that is an error, or a CLI that
	// exited nonzero.
	ErrModelReportRefused = errors.New("the CLI refused the initialize request")
	// ErrModelReportMalformed is a transcript that is not one JSON value per
	// line, carries a duplicate key, or answers the request twice.
	ErrModelReportMalformed = errors.New("the CLI's initialize answer is malformed")
	// ErrModelReportCarriesCredential is output that holds a run of the
	// enrollment's token. Nothing from it is decoded, stored, or logged.
	ErrModelReportCarriesCredential = errors.New("the CLI's initialize output carries the credential")
)

// claudeDefaultModelValue is the CLI's pointer at its own default model. It
// names no model of its own, so it is not recorded.
const claudeDefaultModelValue = "default"

// ClaudeModelReport is what the CLI reported: its build, from --version, and
// the models it offers, in its order.
type ClaudeModelReport struct {
	CLIBuild string
	Models   []domain.DiscoveredModel
}

// Catalog is the report as an enrollment's validated catalog.
func (r ClaudeModelReport) Catalog(
	enrollment domain.ClientEnrollmentID, observedAt time.Time,
) (domain.DiscoveredModelCatalog, error) {
	catalog := domain.DiscoveredModelCatalog{
		EnrollmentID: enrollment, HarnessBuild: r.CLIBuild, ObservedAt: observedAt.UTC(), Models: r.Models,
	}
	if err := catalog.Validate(); err != nil {
		return domain.DiscoveredModelCatalog{}, err
	}
	return catalog, nil
}

// DiscoverClaudeModels runs one initialize probe through image under token
// and decodes its answer.
func DiscoverClaudeModels(
	ctx context.Context, rt Runtime, image string, token SetupToken, authorize RuntimeResourceAuthorizer,
) (ClaudeModelReport, error) {
	out, err := ProbeLineupImage(ctx, rt, LineupProbe{
		Harness: domain.HarnessClientClaudeCode, Image: image, Initialize: true,
		Credential: SetupTokenProbeCredential(token),
	}, authorize)
	if err != nil {
		return ClaudeModelReport{}, err
	}
	return DecodeClaudeModelReport(out, token)
}

type claudeControlResponse struct {
	Type     string `json:"type"`
	Response struct {
		Subtype   string `json:"subtype"`
		RequestID string `json:"request_id"`
		Response  struct {
			Models []claudeReportedModel `json:"models"`
		} `json:"response"`
	} `json:"response"`
}

type claudeReportedModel struct {
	Value                 string   `json:"value"`
	ResolvedModel         string   `json:"resolvedModel"`
	DisplayName           string   `json:"displayName"`
	SupportsEffort        bool     `json:"supportsEffort"`
	SupportedEffortLevels []string `json:"supportedEffortLevels"`
}

// DecodeClaudeModelReport reads an initialize probe's output, run under
// token. It refuses output that carries a run of the token, as the enrollment
// probe withholds such a refusal, because the CLI holds the token and what it
// prints is stored and shown. It takes the one successful control_response to
// the probe's request, drops the default pointer, and gives a model without
// effort support no levels; a CLI that omits supportsEffort reports none. Ids
// and effort levels must be values a launch can pass as an option, and the
// models must form a valid catalog: at least one, unique ids, unique levels.
// No error quotes the CLI's output.
func DecodeClaudeModelReport(out LineupProbeOutput, token SetupToken) (ClaudeModelReport, error) {
	if refusalCarriesToken(string(out.Stdout), token) || refusalCarriesToken(string(out.Version), token) {
		return ClaudeModelReport{}, ErrModelReportCarriesCredential
	}
	var (
		answer claudeControlResponse
		found  bool
	)
	lineNumber := 0
	for line := range bytes.SplitSeq(out.Stdout, []byte("\n")) {
		lineNumber++
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		// The parser's own errors can quote the input, so they are replaced.
		if RejectDuplicateJSONKeys(line) != nil {
			return ClaudeModelReport{}, fmt.Errorf("%w: line %d is not one JSON value with unique keys",
				ErrModelReportMalformed, lineNumber)
		}
		var candidate claudeControlResponse
		if json.Unmarshal(line, &candidate) != nil {
			return ClaudeModelReport{}, fmt.Errorf("%w: line %d does not decode", ErrModelReportMalformed, lineNumber)
		}
		if candidate.Type != "control_response" || candidate.Response.RequestID != lineupProbeInitializeRequestID {
			continue
		}
		if found {
			return ClaudeModelReport{}, fmt.Errorf("%w: answered twice", ErrModelReportMalformed)
		}
		answer, found = candidate, true
	}
	switch {
	case !found:
		return ClaudeModelReport{}, ErrNoModelReport
	case answer.Response.Subtype != "success":
		return ClaudeModelReport{}, fmt.Errorf("%w: the answer is not a success", ErrModelReportRefused)
	case out.ExitStatus != 0:
		return ClaudeModelReport{}, fmt.Errorf("%w: the CLI exited %d", ErrModelReportRefused, out.ExitStatus)
	}
	report := ClaudeModelReport{CLIBuild: firstLine(out.Version)}
	for i, reported := range answer.Response.Response.Models {
		if reported.Value == claudeDefaultModelValue {
			continue
		}
		if !launchableModelReport(reported) {
			return ClaudeModelReport{}, fmt.Errorf("%w: model %d is not a launchable value", ErrModelReportMalformed, i)
		}
		model := domain.DiscoveredModel{ID: reported.Value, DisplayName: reported.DisplayName, EffortLevels: []string{}}
		if reported.ResolvedModel != "" {
			model.ResolvedModel = new(reported.ResolvedModel)
		}
		if reported.SupportsEffort && reported.SupportedEffortLevels != nil {
			model.EffortLevels = reported.SupportedEffortLevels
		}
		report.Models = append(report.Models, model)
	}
	if err := domain.ValidateDiscoveredModels(report.Models); err != nil {
		return ClaudeModelReport{}, err
	}
	return report, nil
}

// launchableModelReport reports whether a model's strings are safe to record
// as selectable: the id, resolved model, and effort levels pass the check a
// launch applies to an option value, and the display name is bounded text.
func launchableModelReport(m claudeReportedModel) bool {
	if !lineupProbeValue(m.Value) || !lineupProbeValue(m.ResolvedModel) {
		return false
	}
	for _, level := range m.SupportedEffortLevels {
		if !lineupProbeValue(level) {
			return false
		}
	}
	return len(m.DisplayName) <= 256 && !strings.ContainsFunc(m.DisplayName, func(r rune) bool { return !unicode.IsGraphic(r) })
}
