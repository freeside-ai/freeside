package domain_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const attachmentDigest = domain.Digest("sha256:1111111111111111111111111111111111111111111111111111111111111111")

func attachment(name string, media domain.TaskAttachmentMediaType, size int64) domain.TaskAttachment {
	return domain.TaskAttachment{
		Digest: attachmentDigest, Name: domain.TaskAttachmentName(name),
		MediaType: media, ClaimedMediaType: "application/octet-stream", SizeBytes: size,
	}
}

func TestTaskAttachmentNameValidate(t *testing.T) {
	t.Parallel()
	valid := []string{
		"screenshot.png",
		"design_handoff/tokens/colors.json",
		"a%2Fb",      // an encoded separator is ordinary text
		`dir\file`,   // only "/" separates
		"...",        // a dot run that is not "." or ".."
		"..hidden/x", // a segment that only starts with ".."
		"résumé.pdf", // non-ASCII UTF-8
		strings.Repeat("a", 255) + "/" + strings.Repeat("b", 255),
		strings.Repeat("a/", 511) + "aa", // 1,024 bytes
	}
	for _, name := range valid {
		if err := domain.TaskAttachmentName(name).Validate(); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", name, err)
		}
	}
	invalid := map[string]error{
		"":                              domain.ErrEmptyField,
		"/etc/passwd":                   domain.ErrInvalidTaskAttachmentName,
		"a//b":                          domain.ErrInvalidTaskAttachmentName,
		"a/":                            domain.ErrInvalidTaskAttachmentName,
		"./a":                           domain.ErrInvalidTaskAttachmentName,
		"a/../../b":                     domain.ErrInvalidTaskAttachmentName,
		"..":                            domain.ErrInvalidTaskAttachmentName,
		"a\x00b":                        domain.ErrInvalidTaskAttachmentName,
		"a\nb":                          domain.ErrInvalidTaskAttachmentName,
		"a\x7fb":                        domain.ErrInvalidTaskAttachmentName,
		"a\u0085b":                      domain.ErrInvalidTaskAttachmentName, // C1 control
		"bad\xffutf8":                   domain.ErrInvalidTaskAttachmentName,
		strings.Repeat("a", 256):        domain.ErrInvalidTaskAttachmentName,
		strings.Repeat("a/", 512) + "a": domain.ErrInvalidTaskAttachmentName, // 1,025 bytes
		"x/" + strings.Repeat("é", 128): domain.ErrInvalidTaskAttachmentName, // 256 bytes
	}
	for name, want := range invalid {
		if err := domain.TaskAttachmentName(name).Validate(); !errors.Is(err, want) {
			t.Errorf("Validate(%q) = %v, want %v", name, err, want)
		}
	}
}

func TestSniffTaskAttachmentMediaType(t *testing.T) {
	t.Parallel()
	cases := map[string]domain.TaskAttachmentMediaType{
		"\x89PNG\r\n\x1a\nrest":                     domain.TaskAttachmentMediaPNG,
		"\xff\xd8\xffrest":                          domain.TaskAttachmentMediaJPEG,
		"GIF89a rest":                               domain.TaskAttachmentMediaGIF,
		"RIFF\x00\x00\x00\x00WEBPVP":                domain.TaskAttachmentMediaWebP,
		`<svg xmlns="http://www.w3.org/2000/svg"/>`: domain.TaskAttachmentMediaOctetStream,
		"%PDF-1.7":                                  domain.TaskAttachmentMediaOctetStream,
		"plain text":                                domain.TaskAttachmentMediaOctetStream,
		"":                                          domain.TaskAttachmentMediaOctetStream,
	}
	for head, want := range cases {
		if got := domain.SniffTaskAttachmentMediaType([]byte(head)); got != want {
			t.Errorf("sniff(%q) = %q, want %q", head, got, want)
		}
	}
	// Only the leading bytes count: a signature past them is not seen.
	late := append(make([]byte, domain.SniffTaskAttachmentHeadBytes), "\x89PNG\r\n\x1a\n"...)
	if got := domain.SniffTaskAttachmentMediaType(late); got != domain.TaskAttachmentMediaOctetStream {
		t.Errorf("sniff of a late signature = %q", got)
	}
}

func TestTaskAttachmentMediaTypes(t *testing.T) {
	t.Parallel()
	for _, m := range domain.AllTaskAttachmentMediaTypes {
		a := attachment("f", m, 0)
		if err := a.Validate(); err != nil {
			t.Errorf("%s: %v", m, err)
		}
		if got, want := m.IsImage(), strings.HasPrefix(string(m), "image/"); got != want {
			t.Errorf("%s IsImage = %t", m, got)
		}
	}
	for _, m := range []domain.TaskAttachmentMediaType{"", "image/svg+xml", "text/plain"} {
		if err := attachment("f", m, 1).Validate(); !errors.Is(err, domain.ErrInvalidTaskAttachmentMediaType) {
			t.Errorf("%q: %v", m, err)
		}
	}
}

func TestValidateTaskAttachmentClaim(t *testing.T) {
	t.Parallel()
	for _, claim := range []string{"image/png", "application/vnd.api+json", "text/x-go", "a/b"} {
		if err := domain.ValidateTaskAttachmentClaim(claim); err != nil {
			t.Errorf("%q: %v", claim, err)
		}
	}
	for _, claim := range []string{
		"", "image", "image/", "/png", "image/png/x", "Image/PNG", "text/plain; charset=utf-8",
		" image/png", "image/png ", "image /png", "text/\"x\"", "a/b\x00", "a/é",
		"a/" + strings.Repeat("b", 254),
	} {
		if err := domain.ValidateTaskAttachmentClaim(claim); !errors.Is(err, domain.ErrInvalidTaskAttachmentClaim) {
			t.Errorf("%q: %v", claim, err)
		}
	}
}

func TestTaskAttachmentValidate(t *testing.T) {
	t.Parallel()
	okImage := attachment("i.png", domain.TaskAttachmentMediaPNG, domain.MaxTaskAttachmentImageBytes)
	okFile := attachment("f.bin", domain.TaskAttachmentMediaOctetStream, domain.MaxTaskAttachmentFileBytes)
	for _, a := range []domain.TaskAttachment{okImage, okFile} {
		if err := a.Validate(); err != nil {
			t.Errorf("%s: %v", a.Name, err)
		}
	}
	bigImage := okImage
	bigImage.SizeBytes++
	bigFile := okFile
	bigFile.SizeBytes++
	negative := okFile
	negative.SizeBytes = -1
	upperDigest := okFile
	upperDigest.Digest = domain.Digest(strings.ToUpper(string(attachmentDigest)))
	badClaim := okFile
	badClaim.ClaimedMediaType = "text/plain; charset=utf-8"
	badName := okFile
	badName.Name = "../x"
	cases := []struct {
		a    domain.TaskAttachment
		want error
	}{
		{bigImage, domain.ErrTaskAttachmentTooLarge},
		{bigFile, domain.ErrTaskAttachmentTooLarge},
		{negative, domain.ErrTaskAttachmentTooLarge},
		{upperDigest, domain.ErrInvalidTaskAttachmentDigest},
		{badClaim, domain.ErrInvalidTaskAttachmentClaim},
		{badName, domain.ErrInvalidTaskAttachmentName},
	}
	for _, tc := range cases {
		if err := tc.a.Validate(); !errors.Is(err, tc.want) {
			t.Errorf("%+v: %v, want %v", tc.a, err, tc.want)
		}
	}
}

func TestValidateTaskAttachments(t *testing.T) {
	t.Parallel()
	if err := domain.ValidateTaskAttachments(nil); err != nil {
		t.Fatalf("empty list: %v", err)
	}
	ok := []domain.TaskAttachment{
		attachment("b.txt", domain.TaskAttachmentMediaOctetStream, 3),
		attachment("a/b.txt", domain.TaskAttachmentMediaOctetStream, 3),
		attachment("a/c/d.txt", domain.TaskAttachmentMediaOctetStream, 3),
		attachment("ab/c.txt", domain.TaskAttachmentMediaOctetStream, 3),
	}
	if err := domain.ValidateTaskAttachments(ok); err != nil {
		t.Fatalf("valid list: %v", err)
	}

	atCount := make([]domain.TaskAttachment, domain.MaxTaskAttachments)
	for i := range atCount {
		atCount[i] = attachment(fmt.Sprintf("f%d", i), domain.TaskAttachmentMediaOctetStream, 0)
	}
	if err := domain.ValidateTaskAttachments(atCount); err != nil {
		t.Fatalf("list at the count limit: %v", err)
	}
	overCount := append(atCount, attachment("one-more", domain.TaskAttachmentMediaOctetStream, 0))

	// Four 25 MiB files fill the budget exactly; one more byte breaches it.
	quarter := domain.MaxTaskAttachmentsTotalBytes / 4
	atBudget := []domain.TaskAttachment{
		attachment("1", domain.TaskAttachmentMediaOctetStream, quarter),
		attachment("2", domain.TaskAttachmentMediaOctetStream, quarter),
		attachment("3", domain.TaskAttachmentMediaOctetStream, quarter),
		attachment("4", domain.TaskAttachmentMediaOctetStream, quarter),
	}
	if err := domain.ValidateTaskAttachments(atBudget); err != nil {
		t.Fatalf("list at the byte budget: %v", err)
	}
	overBudget := append(append([]domain.TaskAttachment(nil), atBudget...),
		attachment("5", domain.TaskAttachmentMediaOctetStream, 1))

	cases := map[string]struct {
		list []domain.TaskAttachment
		want error
	}{
		"count":     {overCount, domain.ErrTooManyTaskAttachments},
		"budget":    {overBudget, domain.ErrTaskAttachmentsTooLarge},
		"duplicate": {[]domain.TaskAttachment{ok[0], ok[0]}, domain.ErrTaskAttachmentNameConflict},
		"file is a directory of another": {[]domain.TaskAttachment{
			attachment("a/c/d.txt", domain.TaskAttachmentMediaOctetStream, 1),
			attachment("a/c", domain.TaskAttachmentMediaOctetStream, 1),
		}, domain.ErrTaskAttachmentNameConflict},
		"bad entry": {
			[]domain.TaskAttachment{attachment("/x", domain.TaskAttachmentMediaOctetStream, 1)},
			domain.ErrInvalidTaskAttachmentName,
		},
	}
	for name, tc := range cases {
		if err := domain.ValidateTaskAttachments(tc.list); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
}
