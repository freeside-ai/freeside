package domain

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Task attachments are operator-supplied input a submission carries: files a
// client uploaded first by digest (PUT /attachments/{digest}), each bound to a
// relative-path name. They are never evidence (plan §5.15): they are not
// artifacts, so nothing that lists artifacts sees them, and nothing here
// grants authority. The name later becomes a file path when an attachment is
// delivered to an agent, so the name rules below are the daemon's defense,
// whatever a client checked first.

// Task attachment limits (owner decision, 2026-11-04). An image is an
// attachment whose daemon-determined media type is an image type.
const (
	MaxTaskAttachmentImageBytes   int64 = 10 << 20
	MaxTaskAttachmentFileBytes    int64 = 32 << 20
	MaxTaskAttachmentsTotalBytes  int64 = 100 << 20
	MaxTaskAttachments                  = 1000
	MaxTaskAttachmentSegmentBytes       = 255
	// MaxTaskAttachmentNameBytes bounds a whole name. Without it a name of
	// many short segments could be most of a 1 MiB command body: it could
	// never be written out as a path, and checking a list of such names
	// costs time on every read.
	MaxTaskAttachmentNameBytes = 1024
	// MaxTaskAttachmentClaimBytes bounds the client's claimed media type,
	// which is kept only as a display hint.
	MaxTaskAttachmentClaimBytes = 255
)

var (
	ErrInvalidTaskAttachmentName      = errors.New("invalid task attachment name")
	ErrInvalidTaskAttachmentMediaType = errors.New("invalid task attachment media type")
	ErrInvalidTaskAttachmentClaim     = errors.New("claimed media type is not a lowercase type/subtype token")
	ErrInvalidTaskAttachmentDigest    = errors.New("task attachment digest is not sha256:<64 lowercase hex>")
	ErrTaskAttachmentTooLarge         = errors.New("task attachment exceeds its size limit")
	ErrTaskAttachmentNameConflict     = errors.New("task attachment names conflict")
	ErrTooManyTaskAttachments         = errors.New("task carries too many attachments")
	ErrTaskAttachmentsTooLarge        = errors.New("task attachments exceed the per-task size budget")
)

// TaskAttachmentName is an attachment's relative-path name, such as
// "screenshot.png" or "design_handoff/tokens/colors.json".
type TaskAttachmentName string

// Validate applies the name rules: at most MaxTaskAttachmentNameBytes bytes,
// valid UTF-8, no control character
// (unicode.IsControl, which covers NUL, the C0 and C1 ranges, and DEL), no
// leading "/", and "/"-separated segments that are non-empty, neither "." nor
// "..", and at most MaxTaskAttachmentSegmentBytes bytes each. A backslash or a
// percent-encoded separator is an ordinary character: only "/" separates.
//
// Refusals here and in the other attachment validators name the rule, never
// the refused text: a submit_task refusal reaches the client and the logs,
// and the text may be something pasted into the wrong field.
func (n TaskAttachmentName) Validate() error {
	s := string(n)
	if s == "" {
		return fmt.Errorf("name: %w", ErrEmptyField)
	}
	if len(s) > MaxTaskAttachmentNameBytes {
		return fmt.Errorf("name is over %d bytes: %w", MaxTaskAttachmentNameBytes, ErrInvalidTaskAttachmentName)
	}
	if !utf8.ValidString(s) {
		return fmt.Errorf("name is not valid UTF-8: %w", ErrInvalidTaskAttachmentName)
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return fmt.Errorf("name has a control character: %w", ErrInvalidTaskAttachmentName)
		}
	}
	if strings.HasPrefix(s, "/") {
		return fmt.Errorf("name is absolute: %w", ErrInvalidTaskAttachmentName)
	}
	for segment := range strings.SplitSeq(s, "/") {
		switch {
		case segment == "", segment == ".", segment == "..":
			return fmt.Errorf("name has an empty, \".\", or \"..\" segment: %w", ErrInvalidTaskAttachmentName)
		case len(segment) > MaxTaskAttachmentSegmentBytes:
			return fmt.Errorf("name has a segment over %d bytes: %w", MaxTaskAttachmentSegmentBytes, ErrInvalidTaskAttachmentName)
		}
	}
	return nil
}

// TaskAttachmentMediaType is the daemon's determination of an attachment's
// type from its bytes. The set is closed: formats with no reliable signature
// (SVG among them) are application/octet-stream by design.
type TaskAttachmentMediaType string

const (
	TaskAttachmentMediaPNG         TaskAttachmentMediaType = "image/png"
	TaskAttachmentMediaJPEG        TaskAttachmentMediaType = "image/jpeg"
	TaskAttachmentMediaGIF         TaskAttachmentMediaType = "image/gif"
	TaskAttachmentMediaWebP        TaskAttachmentMediaType = "image/webp"
	TaskAttachmentMediaOctetStream TaskAttachmentMediaType = "application/octet-stream"
)

// AllTaskAttachmentMediaTypes lists every valid TaskAttachmentMediaType; it is
// the single registration point.
var AllTaskAttachmentMediaTypes = []TaskAttachmentMediaType{
	TaskAttachmentMediaPNG, TaskAttachmentMediaJPEG, TaskAttachmentMediaGIF,
	TaskAttachmentMediaWebP, TaskAttachmentMediaOctetStream,
}

func (m TaskAttachmentMediaType) valid() bool {
	switch m {
	case TaskAttachmentMediaPNG, TaskAttachmentMediaJPEG, TaskAttachmentMediaGIF,
		TaskAttachmentMediaWebP, TaskAttachmentMediaOctetStream:
		return true
	default:
		return false
	}
}

// IsImage reports whether the type is one of the image types, which carry
// the smaller per-image size limit.
func (m TaskAttachmentMediaType) IsImage() bool {
	switch m {
	case TaskAttachmentMediaPNG, TaskAttachmentMediaJPEG, TaskAttachmentMediaGIF, TaskAttachmentMediaWebP:
		return true
	case TaskAttachmentMediaOctetStream:
		return false
	}
	return false
}

// SniffTaskAttachmentHeadBytes is how many leading bytes
// SniffTaskAttachmentMediaType reads; more are ignored.
const SniffTaskAttachmentHeadBytes = 512

// SniffTaskAttachmentMediaType determines an attachment's media type from its
// leading bytes. It maps net/http's signature table onto the closed set, so a
// change in that table can only move a blob between an image type and
// octet-stream.
func SniffTaskAttachmentMediaType(head []byte) TaskAttachmentMediaType {
	if len(head) > SniffTaskAttachmentHeadBytes {
		head = head[:SniffTaskAttachmentHeadBytes]
	}
	switch got := TaskAttachmentMediaType(http.DetectContentType(head)); got {
	case TaskAttachmentMediaPNG, TaskAttachmentMediaJPEG, TaskAttachmentMediaGIF, TaskAttachmentMediaWebP:
		return got
	default:
		return TaskAttachmentMediaOctetStream
	}
}

// ValidateTaskAttachmentClaim checks a client's claimed media type: a
// lowercase type/subtype pair of RFC 2045 token characters, with no
// parameters or whitespace, at most MaxTaskAttachmentClaimBytes bytes. The
// claim decides nothing; the shape rule keeps a display hint from carrying
// arbitrary text.
func ValidateTaskAttachmentClaim(claim string) error {
	if len(claim) > MaxTaskAttachmentClaimBytes {
		return fmt.Errorf("claimed media type is over %d bytes: %w", MaxTaskAttachmentClaimBytes, ErrInvalidTaskAttachmentClaim)
	}
	kind, sub, ok := strings.Cut(claim, "/")
	if !ok || !isLowerMediaToken(kind) || !isLowerMediaToken(sub) {
		return fmt.Errorf("claimed media type: %w", ErrInvalidTaskAttachmentClaim)
	}
	return nil
}

// isLowerMediaToken reports whether s is a non-empty RFC 2045 token (printable
// ASCII other than space and the tspecials) with no uppercase letter.
func isLowerMediaToken(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if c <= ' ' || c >= 0x7f || ('A' <= c && c <= 'Z') || strings.IndexByte(`()<>@,;:\"/[]?=`, c) >= 0 {
			return false
		}
	}
	return true
}

// TaskAttachment is one attachment recorded on a task: the uploaded digest,
// its name, the daemon's media type, the client's claim, and the byte size.
type TaskAttachment struct {
	Digest    Digest                  `json:"digest"`
	Name      TaskAttachmentName      `json:"name"`
	MediaType TaskAttachmentMediaType `json:"media_type"`
	// ClaimedMediaType is the client's claim, a display hint only.
	ClaimedMediaType string `json:"claimed_media_type"`
	SizeBytes        int64  `json:"size_bytes"`
}

// Validate checks one attachment on its own: a sha256 digest, a valid name and
// claim, a media type from the closed set, and a size within the per-image or
// per-file limit its media type selects. An empty file is valid: folders
// routinely hold them.
func (a TaskAttachment) Validate() error {
	if !isSHA256Digest(string(a.Digest)) {
		return fmt.Errorf("digest: %w", ErrInvalidTaskAttachmentDigest)
	}
	if err := a.Name.Validate(); err != nil {
		return err
	}
	if !a.MediaType.valid() {
		return fmt.Errorf("media type: %w", ErrInvalidTaskAttachmentMediaType)
	}
	if err := ValidateTaskAttachmentClaim(a.ClaimedMediaType); err != nil {
		return err
	}
	return validateTaskAttachmentSize(a.MediaType, a.SizeBytes)
}

// validateTaskAttachmentSize checks a size against the per-image or per-file
// limit the media type selects.
func validateTaskAttachmentSize(m TaskAttachmentMediaType, size int64) error {
	limit := MaxTaskAttachmentFileBytes
	if m.IsImage() {
		limit = MaxTaskAttachmentImageBytes
	}
	if size < 0 || size > limit {
		return fmt.Errorf("%d bytes, limit %d: %w", size, limit, ErrTaskAttachmentTooLarge)
	}
	return nil
}

// ValidateTaskAttachments checks a task's whole list: each entry valid, at
// most MaxTaskAttachments entries and MaxTaskAttachmentsTotalBytes in total,
// and names that can coexist as files on a case-sensitive filesystem: no two
// byte-equal, and none a directory prefix of another ("docs" beside
// "docs/a.md" could not both be written out). Names that differ only in case
// or Unicode normalization pass; delivery decides how to write them where
// they would collide. The list's order is the submitted order and is kept as
// given.
func ValidateTaskAttachments(list []TaskAttachment) error {
	return validateAttachmentList(len(list), func(i int) (TaskAttachmentName, int64, error) {
		return list[i].Name, list[i].SizeBytes, list[i].Validate()
	})
}

// validateAttachmentList applies the list rules ValidateTaskAttachments
// documents to n entries, in that order; entry(i) validates entry i on its
// own and returns its name and size. A task's list and a stage's snapshot
// list both use it, so a list submit accepted is a list delivery can write.
// Errors name positions, never names.
func validateAttachmentList(n int, entry func(int) (TaskAttachmentName, int64, error)) error {
	if n > MaxTaskAttachments {
		return fmt.Errorf("%d attachments, limit %d: %w", n, MaxTaskAttachments, ErrTooManyTaskAttachments)
	}
	names := make(map[TaskAttachmentName]struct{}, n)
	ordered := make([]TaskAttachmentName, n)
	var total int64
	for i := range n {
		name, size, err := entry(i)
		if err != nil {
			return fmt.Errorf("attachment %d: %w", i, err)
		}
		if _, dup := names[name]; dup {
			return fmt.Errorf("attachment %d: name repeats an earlier one: %w", i, ErrTaskAttachmentNameConflict)
		}
		names[name] = struct{}{}
		ordered[i] = name
		total += size
	}
	if total > MaxTaskAttachmentsTotalBytes {
		return fmt.Errorf("attachments total %d bytes, limit %d: %w", total, MaxTaskAttachmentsTotalBytes, ErrTaskAttachmentsTooLarge)
	}
	for i, name := range ordered {
		for end := range len(name) {
			if name[end] != '/' {
				continue
			}
			if _, clash := names[name[:end]]; clash {
				return fmt.Errorf("attachment %d: another attachment's name is one of its directories: %w", i, ErrTaskAttachmentNameConflict)
			}
		}
	}
	return nil
}
