package ward

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Codex model discovery (issue #2595) asks an enrollment's pinned Codex CLI
// which models and reasoning efforts it lists, through the app-server's
// model/list method: the list a picker shows (includeHidden unset), paged by
// nextCursor. It runs as a Codex initialize lineup probe, under the same
// containment and access-only credential as the Codex prompt probe, and the
// answer feeds a picker and the selection check only.

// ErrModelReportIncomplete is a model list with a page missing, or whose last
// page still points at another.
var ErrModelReportIncomplete = errors.New("the CLI's model list is incomplete")

// DiscoverCodexModels runs one Codex initialize probe through image under
// credential and decodes its answer.
func DiscoverCodexModels(
	ctx context.Context, rt Runtime, image string, credential LineupProbeCredential, authorize RuntimeResourceAuthorizer,
) (ModelReport, error) {
	out, err := ProbeLineupImage(ctx, rt, LineupProbe{
		Harness: domain.HarnessClientCodexCLI, Image: image, Initialize: true, Credential: credential,
	}, authorize)
	if err != nil {
		return ModelReport{}, err
	}
	return DecodeCodexModelReport(out, credential)
}

// codexRPCMessage is one app-server stdout line. An answer carries an id and
// no method; a notification or a server-sent request carries a method.
type codexRPCMessage struct {
	ID     json.RawMessage `json:"id"`
	Method *string         `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

type codexModelPage struct {
	Data       *[]codexReportedModel `json:"data"`
	NextCursor *string               `json:"nextCursor"`
}

type codexReportedModel struct {
	Model                     string `json:"model"`
	DisplayName               string `json:"displayName"`
	SupportedReasoningEfforts []struct {
		ReasoningEffort string `json:"reasoningEffort"`
	} `json:"supportedReasoningEfforts"`
}

// DecodeCodexModelReport reads a Codex initialize probe's output, run under
// credential. It refuses output that carries a run of any of the
// credential's secrets, in the raw bytes or a decoded string, as
// DecodeClaudeModelReport does. It then requires a successful
// answer to initialize (id 1) and model/list answers numbered from 2 with no
// gap, where every page but the last carries a cursor and the last carries
// none; an error answer, a repeated id, an id the probe never sent, or a
// nonzero exit is refused. Each model's id is its model field, the value a
// launch passes as -m, and its effort levels are its supported reasoning
// efforts in the CLI's order. Ids and efforts must be values a launch can
// pass as an option, and the models must form a valid catalog. No error
// quotes the CLI's output.
func DecodeCodexModelReport(out LineupProbeOutput, credential LineupProbeCredential) (ModelReport, error) {
	secrets, err := codexProbeSecrets(credential)
	if err != nil {
		return ModelReport{}, err
	}
	if carriesAnySecretRun(out.Stdout, secrets) || carriesAnySecretRun(out.Version, secrets) {
		return ModelReport{}, ErrModelReportCarriesCredential
	}
	answers := map[int]codexRPCMessage{}
	lineNumber := 0
	for line := range bytes.SplitSeq(out.Stdout, []byte("\n")) {
		lineNumber++
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		// The parser's own errors can quote the input, so they are replaced.
		if RejectDuplicateJSONKeys(line) != nil {
			return ModelReport{}, fmt.Errorf("%w: line %d is not one JSON value with unique keys",
				ErrModelReportMalformed, lineNumber)
		}
		var message codexRPCMessage
		if json.Unmarshal(line, &message) != nil {
			return ModelReport{}, fmt.Errorf("%w: line %d does not decode", ErrModelReportMalformed, lineNumber)
		}
		if message.Method != nil || len(message.ID) == 0 {
			continue
		}
		var id int
		if json.Unmarshal(message.ID, &id) != nil || id < 1 || id > lineupProbeCodexMaxPages+1 {
			return ModelReport{}, fmt.Errorf("%w: line %d answers a request the probe did not send",
				ErrModelReportMalformed, lineNumber)
		}
		if _, ok := answers[id]; ok {
			return ModelReport{}, fmt.Errorf("%w: request %d answered twice", ErrModelReportMalformed, id)
		}
		answers[id] = message
	}
	initialize, ok := answers[1]
	if !ok {
		return ModelReport{}, ErrNoModelReport
	}
	for id := 1; id <= lineupProbeCodexMaxPages+1; id++ {
		if answer, ok := answers[id]; ok && isJSONPresent(answer.Error) {
			return ModelReport{}, fmt.Errorf("%w: request %d's answer is an error", ErrModelReportRefused, id)
		}
	}
	switch {
	case !isJSONPresent(initialize.Result):
		return ModelReport{}, fmt.Errorf("%w: the initialize answer carries no result", ErrModelReportMalformed)
	case out.ExitStatus != 0:
		return ModelReport{}, fmt.Errorf("%w: the CLI exited %d", ErrModelReportRefused, out.ExitStatus)
	}
	pages := len(answers) - 1
	if pages == 0 {
		return ModelReport{}, fmt.Errorf("%w: page 1 is missing", ErrModelReportIncomplete)
	}
	report := ModelReport{CLIBuild: firstLine(out.Version), Models: []domain.DiscoveredModel{}}
	for id := 2; id <= pages+1; id++ {
		answer, ok := answers[id]
		if !ok {
			return ModelReport{}, fmt.Errorf("%w: page %d is missing", ErrModelReportIncomplete, id-1)
		}
		var page codexModelPage
		if json.Unmarshal(answer.Result, &page) != nil || page.Data == nil {
			return ModelReport{}, fmt.Errorf("%w: page %d is not a model list", ErrModelReportMalformed, id-1)
		}
		more := page.NextCursor != nil && *page.NextCursor != ""
		switch {
		case id <= pages && !more:
			return ModelReport{}, fmt.Errorf("%w: page %d ends the list before page %d", ErrModelReportMalformed, id-1, id)
		case id > pages && more:
			return ModelReport{}, fmt.Errorf("%w: page %d still carries a cursor", ErrModelReportIncomplete, id-1)
		}
		for _, reported := range *page.Data {
			model, ok := codexDiscoveredModel(reported)
			if !ok {
				return ModelReport{}, fmt.Errorf("%w: model %d is not a launchable value",
					ErrModelReportMalformed, len(report.Models))
			}
			report.Models = append(report.Models, model)
		}
	}
	if reportCarriesSecret(report, secrets) {
		return ModelReport{}, ErrModelReportCarriesCredential
	}
	if err := domain.ValidateDiscoveredModels(report.Models); err != nil {
		return ModelReport{}, err
	}
	return report, nil
}

// codexDiscoveredModel maps one listed model, refusing one whose strings are
// not safe to record as selectable. Codex names no resolved model.
func codexDiscoveredModel(reported codexReportedModel) (domain.DiscoveredModel, bool) {
	efforts := make([]string, 0, len(reported.SupportedReasoningEfforts))
	for _, effort := range reported.SupportedReasoningEfforts {
		efforts = append(efforts, effort.ReasoningEffort)
	}
	if !launchableModelStrings(reported.Model, "", reported.DisplayName, efforts) {
		return domain.DiscoveredModel{}, false
	}
	return domain.DiscoveredModel{ID: reported.Model, DisplayName: reported.DisplayName, EffortLevels: efforts}, true
}

// codexProbeSecrets are the secrets a Codex probe credential carries, the
// bearer first: the access token of a subscription credential, with the id
// token and any exchanged API key beside it, or the API key. A credential
// without its bearer is refused.
func codexProbeSecrets(credential LineupProbeCredential) ([][]byte, error) {
	var auth codexAuthFile
	if json.Unmarshal(credential.body, &auth) != nil {
		return nil, errors.New("codex model discovery: the credential is not an auth.json")
	}
	var secrets [][]byte
	switch {
	case credential.mode == CodexAuthSubscription && auth.Tokens != nil && auth.Tokens.AccessToken != "":
		secrets = append(secrets, []byte(auth.Tokens.AccessToken))
		if auth.Tokens.IDToken != "" {
			secrets = append(secrets, []byte(auth.Tokens.IDToken))
		}
	case credential.mode == CodexAuthAPIKey && auth.OpenAIAPIKey != nil && *auth.OpenAIAPIKey != "":
	default:
		return nil, errors.New("codex model discovery: the credential carries no Codex secret")
	}
	if auth.OpenAIAPIKey != nil && *auth.OpenAIAPIKey != "" {
		secrets = append(secrets, []byte(*auth.OpenAIAPIKey))
	}
	return secrets, nil
}

// isJSONPresent reports whether a raw member was present and not null.
func isJSONPresent(raw json.RawMessage) bool {
	return len(raw) != 0 && !bytes.Equal(raw, []byte("null"))
}
