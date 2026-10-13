// Package specify owns the unit-local contract between the specifier and
// the daemon. It deliberately stays outside domain: these bytes are one
// workflow's stage output, not shared persisted vocabulary.
package specify

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
	MaxReplyBytes         = 8 << 10
	MaxAddressals         = 64
	MaxAddressalTextBytes = 8 << 10
)

var (
	ErrInvalidOutput = errors.New("invalid specifier output")
	ErrPolicyMissing = errors.New("specification policy key is missing")
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
	CommentID string `json:"comment_id"`
	Response  string `json:"response"`
}

type legacyAddressal struct {
	Comment  string `json:"comment"`
	Response string `json:"response"`
}

type legacySpecification struct {
	Summary    string            `json:"summary"`
	Body       string            `json:"body"`
	Addressals []legacyAddressal `json:"addressals"`
}

type legacyOutput struct {
	FetchRequests []FetchRequest       `json:"fetch_requests"`
	Specification *legacySpecification `json:"specification"`
	Reply         *string              `json:"reply"`
}

// Specification is the terminal specifier output.
type Specification struct {
	Title      *string     `json:"title"`
	Summary    string      `json:"summary"`
	Body       string      `json:"body"`
	Addressals []Addressal `json:"addressals"`
}

// Output is exactly one loop decision: request research, return a spec,
// answer a discussion turn without advancing the specification, or hand the
// human the decisions that block an implementation-ready specification.
type Output struct {
	FetchRequests []FetchRequest    `json:"fetch_requests"`
	Specification *Specification    `json:"specification"`
	Reply         *string           `json:"reply"`
	Decisions     []domain.Decision `json:"decisions"`
}

// DecodeOutput strictly reconstructs and validates one typed stage payload.
// Three tolerated presentation defects: a single Markdown code fence around
// the whole object (issue #780), surrounding whitespace on prose fields
// (issue #1123), and a backslash before a backtick inside a JSON string
// (issue #1984). These model output defects otherwise cost a full specification
// execution. Every other invalid escape still fails. Identifiers (fetch URLs,
// comment IDs) stay strict.
// Anything beyond those exact shapes (prose, truncation, a second fence)
// still fails strict decode, with a bounded prefix of the raw output
// preserved for diagnosis.
func DecodeOutput(data []byte) (Output, error) {
	var out Output
	payload, repaired := dropBacktickEscapes(stripMarkdownFence(data))
	if err := strictjson.Decode(payload, &out,
		strictjson.RejectInvalidUTF8, MaxOutputBytes); err != nil {
		return Output{}, fmt.Errorf("decode specifier output: %w (output begins %q)",
			err, outputPrefix(data))
	}
	// A repaired identifier must never acquire authority as a different URL
	// or comment ID. Bare backticks plus a repair elsewhere fail closed too.
	if repaired {
		for _, request := range out.FetchRequests {
			if strings.ContainsRune(request.URL, '`') {
				return Output{}, fmt.Errorf("%w: backtick escape tolerance cannot repair fetch URLs", ErrInvalidOutput)
			}
		}
		if out.Specification != nil {
			for _, addressal := range out.Specification.Addressals {
				if strings.ContainsRune(addressal.CommentID, '`') {
					return Output{}, fmt.Errorf("%w: backtick escape tolerance cannot repair comment IDs", ErrInvalidOutput)
				}
			}
		}
	}
	out.trimProse()
	if err := out.Validate(); err != nil {
		return Output{}, err
	}
	return out, nil
}

// DecodeLegacyAddressalOutput reconstructs output admitted under the prompt
// contract immediately preceding comment_id addressals. The caller supplies
// the authenticated feedback-body-to-command-id binding from that invocation;
// no current-contract output passes through this compatibility path.
func DecodeLegacyAddressalOutput(data []byte, commentIDs map[string]string) (Output, error) {
	var legacy legacyOutput
	payload, repaired := dropBacktickEscapes(stripMarkdownFence(data))
	if err := strictjson.Decode(payload, &legacy,
		strictjson.RejectInvalidUTF8, MaxOutputBytes); err != nil {
		return Output{}, fmt.Errorf("decode legacy specifier output: %w (output begins %q)",
			err, outputPrefix(data))
	}
	if repaired {
		for _, request := range legacy.FetchRequests {
			if strings.ContainsRune(request.URL, '`') {
				return Output{}, fmt.Errorf("%w: backtick escape tolerance cannot repair fetch URLs", ErrInvalidOutput)
			}
		}
	}
	out := Output{
		FetchRequests: legacy.FetchRequests,
		Reply:         legacy.Reply,
	}
	if legacy.Specification != nil {
		out.Specification = &Specification{
			Summary:    legacy.Specification.Summary,
			Body:       legacy.Specification.Body,
			Addressals: make([]Addressal, len(legacy.Specification.Addressals)),
		}
		for i, addressal := range legacy.Specification.Addressals {
			commentID, ok := commentIDs[addressal.Comment]
			if !ok {
				return Output{}, fmt.Errorf("%w: legacy addressals[%d] does not match authenticated feedback",
					ErrInvalidOutput, i)
			}
			out.Specification.Addressals[i] = Addressal{
				CommentID: commentID,
				Response:  addressal.Response,
			}
		}
	}
	out.trimProse()
	if err := out.Validate(); err != nil {
		return Output{}, err
	}
	return out, nil
}

// dropBacktickEscapes preserves JSON escape pairs except for the invalid
// backtick escape. Consuming each pair together preserves escaped backslashes
// and keeps escaped quotes from changing the string boundary.
func dropBacktickEscapes(data []byte) ([]byte, bool) {
	// Leave oversized input to strict decode; repair must not shrink it below
	// the input limit or allocate another unbounded buffer.
	if strictjson.Limit(len(data)) > MaxOutputBytes {
		return data, false
	}
	var repaired []byte
	inString := false
	start := 0
	for i := 0; i < len(data); i++ {
		switch data[i] {
		case '"':
			inString = !inString
		case '\\':
			if !inString || i+1 == len(data) {
				continue
			}
			if data[i+1] == '`' {
				if repaired == nil {
					repaired = make([]byte, 0, len(data))
				}
				repaired = append(repaired, data[start:i]...)
				start = i + 1
			}
			i++
		}
	}
	if repaired == nil {
		return data, false
	}
	return append(repaired, data[start:]...), true
}

// trimProse strips surrounding whitespace from the free-text fields before
// validation, which then still rejects a field that was whitespace only.
// Trimming happens here, at the decode boundary, so every stored
// specification, reply, and addressal is the trimmed text and the digest
// over it is stable.
func (o *Output) trimProse() {
	if o.Reply != nil {
		trimmed := strings.TrimSpace(*o.Reply)
		o.Reply = &trimmed
	}
	if o.Specification == nil {
		return
	}
	o.Specification.Summary = strings.TrimSpace(o.Specification.Summary)
	if o.Specification.Title != nil {
		title := strings.TrimSpace(*o.Specification.Title)
		o.Specification.Title = &title
	}
	o.Specification.Body = strings.TrimSpace(o.Specification.Body)
	for i := range o.Specification.Addressals {
		o.Specification.Addressals[i].Response = strings.TrimSpace(o.Specification.Addressals[i].Response)
	}
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
		return nil, fmt.Errorf("encode specifier output: %w", err)
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
	hasReply := o.Reply != nil
	hasDecisions := len(o.Decisions) > 0
	if boolCount(hasFetch)+boolCount(hasSpec)+boolCount(hasReply)+boolCount(hasDecisions) != 1 {
		return fmt.Errorf("%w: exactly one of fetch_requests, specification, reply, or decisions is required",
			ErrInvalidOutput)
	}
	if hasDecisions {
		if err := domain.ValidateDecisions(o.Decisions); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidOutput, err)
		}
		return nil
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
	if hasSpec {
		return o.Specification.validate()
	}
	if *o.Reply == "" || *o.Reply != strings.TrimSpace(*o.Reply) ||
		len(*o.Reply) > MaxReplyBytes || !utf8.ValidString(*o.Reply) {
		return fmt.Errorf("%w: reply must be non-empty trimmed UTF-8 within %d bytes", ErrInvalidOutput, MaxReplyBytes)
	}
	return nil
}

func boolCount(value bool) int {
	if value {
		return 1
	}
	return 0
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
	if s.Title != nil && (*s.Title == "" || *s.Title != strings.TrimSpace(*s.Title) ||
		strings.ContainsAny(*s.Title, "\r\n") || !utf8.ValidString(*s.Title) || len(*s.Title) > 4<<10) {
		return fmt.Errorf("%w: specification title must be single-line trimmed UTF-8 within 4 KiB", ErrInvalidOutput)
	}
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
		if addressal.CommentID == "" || addressal.Response == "" ||
			len(addressal.CommentID) > MaxAddressalTextBytes || len(addressal.Response) > domain.MaxSpecRevisionCommentBytes ||
			addressal.CommentID != strings.TrimSpace(addressal.CommentID) ||
			addressal.Response != strings.TrimSpace(addressal.Response) ||
			!utf8.ValidString(addressal.CommentID) || !utf8.ValidString(addressal.Response) {
			return fmt.Errorf("%w: addressals[%d] requires trimmed UTF-8 comment_id and response", ErrInvalidOutput, i)
		}
	}
	return nil
}
