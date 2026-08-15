// Package elaborate owns the unit-local contract between the elaborator and
// the daemon. It deliberately stays outside domain: these bytes are one
// workflow's stage output, not shared persisted vocabulary.
package elaborate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const (
	MaxOutputBytes        = strictjson.Limit(1 << 20)
	MaxFetchRequests      = 16
	MaxURLBytes           = 8 << 10
	MaxPurposeBytes       = 4 << 10
	MaxSummaryBytes       = 8 << 10
	MaxAddressals         = 64
	MaxAddressalTextBytes = 8 << 10
)

var (
	ErrInvalidOutput = errors.New("invalid elaborator output")
	ErrPolicyMissing = errors.New("elaboration policy key is missing")
)

// FetchRequest asks the daemon to retrieve one research URL for a stated use.
// The URL is still untrusted here; Fetcher applies the security policy.
type FetchRequest struct {
	URL     string `json:"url"`
	Purpose string `json:"purpose"`
}

// Addressal maps one prior human comment to the revision's claimed response.
// It is agent-authored presentation, never proof that the comment was met.
type Addressal struct {
	Comment  string `json:"comment"`
	Response string `json:"response"`
}

// Specification is the terminal elaborator output.
type Specification struct {
	Summary    string      `json:"summary"`
	Body       string      `json:"body"`
	Addressals []Addressal `json:"addressals"`
}

// Output is exactly one loop decision: request research or return a spec.
type Output struct {
	FetchRequests []FetchRequest `json:"fetch_requests"`
	Specification *Specification `json:"specification"`
}

// DecodeOutput strictly reconstructs and validates one typed stage payload.
// One tolerated presentation defect: a single Markdown code fence around the
// whole object (issue #780). Fence-wrapping is the model class's dominant
// output-shape failure despite the prompt forbidding it, and each occurrence
// otherwise costs a full elaboration execution; anything beyond that exact
// shape (prose, truncation, a second fence) still fails strict decode, with a
// bounded prefix of the raw output preserved for diagnosis.
func DecodeOutput(data []byte) (Output, error) {
	var out Output
	if err := strictjson.Decode(stripMarkdownFence(data), &out,
		strictjson.RejectInvalidUTF8, MaxOutputBytes); err != nil {
		return Output{}, fmt.Errorf("decode elaborator output: %w (output begins %q)",
			err, outputPrefix(data))
	}
	if err := out.Validate(); err != nil {
		return Output{}, err
	}
	return out, nil
}

// fenceTagPattern admits the optional language tag on an opening fence line
// ("```json"): a bare alphanumeric token, nothing else, so an opening line
// carrying real content is never treated as presentation.
var fenceTagPattern = regexp.MustCompile(`^[A-Za-z0-9_-]*$`)

// stripMarkdownFence removes exactly one whole-payload fence pair and nothing
// else: the trimmed payload's first line must be a fence with at most a bare
// language tag, its last line exactly a closing fence. Any other shape
// returns the input unchanged for strict decode to reject.
func stripMarkdownFence(data []byte) []byte {
	trimmed := bytes.TrimSpace(data)
	if !bytes.HasPrefix(trimmed, []byte("```")) || !bytes.HasSuffix(trimmed, []byte("```")) {
		return data
	}
	firstBreak := bytes.IndexByte(trimmed, '\n')
	if firstBreak < 0 {
		return data
	}
	openTag := bytes.TrimSpace(trimmed[len("```"):firstBreak])
	body := trimmed[firstBreak+1 : len(trimmed)-len("```")]
	lastBreak := bytes.LastIndexByte(body, '\n')
	if !fenceTagPattern.Match(openTag) || lastBreak < 0 ||
		len(bytes.TrimSpace(body[lastBreak:])) != 0 {
		return data
	}
	return body[:lastBreak]
}

// outputPrefix bounds the diagnostic snippet a decode failure carries.
func outputPrefix(data []byte) string {
	const max = 80
	if len(data) > max {
		return string(data[:max]) + "…"
	}
	return string(data)
}

// EncodeOutput emits the canonical bytes the real path and fake authenticate.
func EncodeOutput(out Output) ([]byte, error) {
	if err := out.Validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("encode elaborator output: %w", err)
	}
	if strictjson.Limit(len(body)) > MaxOutputBytes {
		return nil, fmt.Errorf("%w: encoded output exceeds %d bytes", ErrInvalidOutput, MaxOutputBytes)
	}
	return body, nil
}

// Validate rejects ambiguous output and malformed presentation fields.
func (o Output) Validate() error {
	hasFetch := len(o.FetchRequests) > 0
	hasSpec := o.Specification != nil
	if hasFetch == hasSpec {
		return fmt.Errorf("%w: exactly one of fetch_requests or specification is required", ErrInvalidOutput)
	}
	if hasFetch {
		if len(o.FetchRequests) > MaxFetchRequests {
			return fmt.Errorf("%w: fetch_requests exceeds %d entries", ErrInvalidOutput, MaxFetchRequests)
		}
		seen := make(map[string]struct{}, len(o.FetchRequests))
		for i, request := range o.FetchRequests {
			if err := request.validate(); err != nil {
				return fmt.Errorf("%w: fetch_requests[%d]: %w", ErrInvalidOutput, i, err)
			}
			if _, duplicate := seen[request.URL]; duplicate {
				return fmt.Errorf("%w: duplicate fetch URL %q", ErrInvalidOutput, request.URL)
			}
			seen[request.URL] = struct{}{}
		}
		return nil
	}
	return o.Specification.validate()
}

func (r FetchRequest) validate() error {
	if r.URL == "" || len(r.URL) > MaxURLBytes || r.URL != strings.TrimSpace(r.URL) || r.Purpose == "" ||
		len(r.Purpose) > MaxPurposeBytes ||
		r.Purpose != strings.TrimSpace(r.Purpose) || !utf8.ValidString(r.URL) || !utf8.ValidString(r.Purpose) {
		return errors.New("URL and purpose must be non-empty trimmed UTF-8")
	}
	parsed, err := url.Parse(r.URL)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" {
		return errors.New("URL must be absolute")
	}
	return nil
}

func (s Specification) validate() error {
	if s.Summary == "" || len(s.Summary) > MaxSummaryBytes || s.Summary != strings.TrimSpace(s.Summary) ||
		s.Body == "" || s.Body != strings.TrimSpace(s.Body) ||
		!utf8.ValidString(s.Summary) || !utf8.ValidString(s.Body) {
		return fmt.Errorf("%w: specification summary and body must be non-empty trimmed UTF-8", ErrInvalidOutput)
	}
	if len(s.Body) > domain.MaxClaimTextBytes {
		return fmt.Errorf("%w: specification body exceeds %d bytes", ErrInvalidOutput, domain.MaxClaimTextBytes)
	}
	if s.Addressals == nil {
		return fmt.Errorf("%w: specification addressals must be an array", ErrInvalidOutput)
	}
	if len(s.Addressals) > MaxAddressals {
		return fmt.Errorf("%w: specification addressals exceeds %d entries", ErrInvalidOutput, MaxAddressals)
	}
	for i, addressal := range s.Addressals {
		if addressal.Comment == "" || addressal.Response == "" ||
			len(addressal.Comment) > MaxAddressalTextBytes || len(addressal.Response) > MaxAddressalTextBytes ||
			addressal.Comment != strings.TrimSpace(addressal.Comment) ||
			addressal.Response != strings.TrimSpace(addressal.Response) ||
			!utf8.ValidString(addressal.Comment) || !utf8.ValidString(addressal.Response) {
			return fmt.Errorf("%w: addressals[%d] requires trimmed UTF-8 comment and response", ErrInvalidOutput, i)
		}
	}
	return nil
}
