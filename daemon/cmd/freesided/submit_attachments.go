package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
)

// attachFlag collects repeated --attach PATH values.
type attachFlag []string

func (f *attachFlag) String() string { return "" }

func (f *attachFlag) Set(value string) error {
	if value == "" {
		return errors.New("want a file path")
	}
	*f = append(*f, value)
	return nil
}

// attachmentFile is one --attach file read for a submission: the reference
// the recovery journal saves, and the bytes stored before the journal.
type attachmentFile struct {
	input signet.TaskAttachmentInput
	body  []byte
}

// readAttachmentFiles reads the --attach files in order and checks every rule
// the daemon checks again when it records them: each is a regular file within
// its size limit, named by its base name, and the list meets the count, the
// per-task budget, and the name rules. It runs before anything is saved, so a
// refused list leaves no journal and no blob. The per-task budget is checked
// while reading, which bounds the bytes held in memory.
func readAttachmentFiles(paths []string) ([]attachmentFile, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	if len(paths) > domain.MaxTaskAttachments {
		return nil, fmt.Errorf("%d files, limit %d: %w", len(paths), domain.MaxTaskAttachments, domain.ErrTooManyTaskAttachments)
	}
	files := make([]attachmentFile, len(paths))
	list := make([]domain.TaskAttachment, len(paths))
	var total int64
	for i, path := range paths {
		body, err := readAttachmentFile(path)
		if err != nil {
			return nil, err
		}
		total += int64(len(body))
		if total > domain.MaxTaskAttachmentsTotalBytes {
			return nil, fmt.Errorf("files exceed %d bytes in total: %w", domain.MaxTaskAttachmentsTotalBytes, domain.ErrTaskAttachmentsTooLarge)
		}
		input := signet.TaskAttachmentInput{
			Digest:    submissionBytes(body).digest,
			Name:      domain.TaskAttachmentName(filepath.Base(path)),
			MediaType: claimedAttachmentType(path),
		}
		files[i] = attachmentFile{input: input, body: body}
		list[i] = domain.TaskAttachment{
			Digest: input.Digest, Name: input.Name, MediaType: domain.SniffTaskAttachmentMediaType(body),
			ClaimedMediaType: input.MediaType, SizeBytes: int64(len(body)),
		}
	}
	if err := domain.ValidateTaskAttachments(list); err != nil {
		return nil, err
	}
	return files, nil
}

// readAttachmentFile reads one regular file under the per-file limit. A
// directory is refused: --attach takes files, and a folder upload is not
// part of the CLI. The kind is checked before the open, which would block
// on a FIFO with no writer.
func readAttachmentFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory; --attach takes files", path)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	f, err := os.Open(path) //nolint:gosec // G304: operator-supplied attachment path is this flag's whole purpose; bytes are hashed, size-capped, and stored by digest
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	body, err := io.ReadAll(io.LimitReader(f, domain.MaxTaskAttachmentFileBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > domain.MaxTaskAttachmentFileBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes: %w", path, domain.MaxTaskAttachmentFileBytes, domain.ErrTaskAttachmentTooLarge)
	}
	return body, nil
}

// claimedAttachmentType is the media type the file's extension suggests,
// without parameters, or application/octet-stream when the extension names
// none or the system's table gives a malformed one. It is only the claim the
// daemon keeps as a display hint; the daemon sniffs the type it records.
func claimedAttachmentType(path string) string {
	claim, _, err := mime.ParseMediaType(mime.TypeByExtension(filepath.Ext(path)))
	if err != nil || domain.ValidateTaskAttachmentClaim(claim) != nil {
		return string(domain.TaskAttachmentMediaOctetStream)
	}
	return claim
}

func attachmentInputs(files []attachmentFile) []signet.TaskAttachmentInput {
	if len(files) == 0 {
		return nil
	}
	inputs := make([]signet.TaskAttachmentInput, len(files))
	for i, file := range files {
		inputs[i] = file.input
	}
	return inputs
}

// storeAttachmentFiles puts the files' bytes in the blob store the daemon on
// this database opens, so the daemon finds every reference the journal names.
// Put is atomic and converges per digest, so a daemon reading the same
// directory sees a complete blob or none.
func storeAttachmentFiles(dbPath string, files []attachmentFile) error {
	if len(files) == 0 {
		return nil
	}
	blobs, err := signet.NewBlobStore(dbPath + ".blobs")
	if err != nil {
		return err
	}
	for _, file := range files {
		if _, err := blobs.Put(file.input.Digest, bytes.NewReader(file.body)); err != nil {
			return err
		}
	}
	return nil
}
