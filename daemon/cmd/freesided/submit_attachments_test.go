package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

const pngSignature = "\x89PNG\r\n\x1a\n"

// submittedTaskAttachments reads the attachments of the task a CLI
// submission created, opening and closing the store per read.
func submittedTaskAttachments(t *testing.T, dbPath string, result submitResult) []domain.TaskAttachment {
	t.Helper()
	ctx := t.Context()
	st := storetest.Open(t, dbPath, store.Options{})
	defer func() { _ = st.Close() }()
	var list []domain.TaskAttachment
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		run, err := tx.GetRun(ctx, result.SpecificationRunID)
		if err != nil {
			return err
		}
		list, err = tx.TaskAttachments(ctx, run.TaskID)
		return err
	}); err != nil {
		t.Fatalf("read task attachments: %v", err)
	}
	return list
}

func writeAttachment(t *testing.T, path string, body []byte) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func attachmentSubmitConfig(t *testing.T, root, id string) submitCommandConfig {
	t.Helper()
	taskPath, policyPath, publicationPath := writeSubmissionInputs(t, root)
	return submitCommandConfig{
		SubmissionID: id,
		DBPath:       filepath.Join(root, "freeside.db"),
		TaskPath:     taskPath, PolicyPath: policyPath, WorkUnitPath: declaredWorkUnit(t), PublicationPath: publicationPath,
		ProjectID: "proj-submit",
	}
}

// TestSubmitCommandRecordsAttachments covers --attach: the files are recorded
// in the given order with the daemon's sniffed type, the journal keeps only
// references, and a replay or a manual retry from the journal resubmits the
// same list without recording a second one.
func TestSubmitCommandRecordsAttachments(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	root := t.TempDir()
	cfg := attachmentSubmitConfig(t, root, "attach-test")
	screenshot := []byte(pngSignature + "pixels")
	notes := []byte(`{"note":"read me first"}`)
	cfg.AttachPaths = []string{
		writeAttachment(t, filepath.Join(root, "inputs", "screenshot.png"), screenshot),
		writeAttachment(t, filepath.Join(root, "inputs", "nested", "notes.json"), notes),
	}
	first, err := runSubmitCommand(ctx, cfg)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	want := []domain.TaskAttachment{
		{
			Digest: submissionBytes(screenshot).digest, Name: "screenshot.png",
			MediaType: domain.TaskAttachmentMediaPNG, ClaimedMediaType: "image/png", SizeBytes: int64(len(screenshot)),
		},
		{
			Digest: submissionBytes(notes).digest, Name: "notes.json",
			MediaType: domain.TaskAttachmentMediaOctetStream, ClaimedMediaType: claimedAttachmentType("notes.json"),
			SizeBytes: int64(len(notes)),
		},
	}
	if got := submittedTaskAttachments(t, cfg.DBPath, first); !reflect.DeepEqual(got, want) {
		t.Fatalf("attachments = %+v, want %+v", got, want)
	}
	// The bytes are in the daemon's blob store under their digests.
	blobs, err := signet.NewBlobStore(cfg.DBPath + ".blobs")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range want {
		if ok, err := blobs.Verify(a.Digest); err != nil || !ok {
			t.Fatalf("blob %s: verified %t, %v", a.Name, ok, err)
		}
	}
	// The journal names the references, never the bytes.
	journal, err := os.ReadFile(submissionJournalPath(cfg.DBPath, cfg.SubmissionID))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(journal), `"Attachments":[{"digest":"`+string(want[0].Digest)) ||
		bytes.Contains(journal, []byte("read me first")) {
		t.Fatalf("journal = %s, want attachment references only", journal)
	}
	// The fingerprint covers the references in order under their wire names.
	lineLess := lineLessFingerprint(t, cfg.ProjectID, first)
	pinned := string(lineLess[:len(lineLess)-1]) + `,"Attachments":[` +
		`{"digest":"` + string(want[0].Digest) + `","name":"screenshot.png","media_type":"image/png"},` +
		`{"digest":"` + string(want[1].Digest) + `","name":"notes.json","media_type":"` + want[1].ClaimedMediaType + `"}]}`
	if got, want := recordedRequestDigest(t, cfg.DBPath, cfg.SubmissionID), domain.Digest(contentaddr.Sum([]byte(pinned))); got != want {
		t.Fatalf("request digest = %s, want %s", got, want)
	}

	for name, repeat := range map[string]submitCommandConfig{
		"same request": cfg,
		"manual retry": {DBPath: cfg.DBPath, RetrySubmissionID: cfg.SubmissionID},
	} {
		replay, err := runSubmitCommand(ctx, repeat)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if replay != first {
			t.Fatalf("%s: result = %#v, want %#v", name, replay, first)
		}
		if got := submittedTaskAttachments(t, cfg.DBPath, first); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: attachments = %+v, want %+v", name, got, want)
		}
	}

	// The same identity with another list is refused, by the saved journal
	// and, once that is gone, by the recorded submission's fingerprint.
	reordered := cfg
	reordered.AttachPaths = []string{cfg.AttachPaths[1], cfg.AttachPaths[0]}
	none := cfg
	none.AttachPaths = nil
	for name, other := range map[string]submitCommandConfig{"reordered": reordered, "none": none} {
		if _, err := runSubmitCommand(ctx, other); !errors.Is(err, store.ErrImmutableConflict) {
			t.Fatalf("%s against the journal: error = %v, want ErrImmutableConflict", name, err)
		}
	}
	if err := os.Remove(submissionJournalPath(cfg.DBPath, cfg.SubmissionID)); err != nil {
		t.Fatal(err)
	}
	for name, other := range map[string]submitCommandConfig{"reordered": reordered, "none": none} {
		if _, err := runSubmitCommand(ctx, other); !errors.Is(err, store.ErrImmutableConflict) {
			t.Fatalf("%s against the record: error = %v, want ErrImmutableConflict", name, err)
		}
		if err := os.Remove(submissionJournalPath(cfg.DBPath, cfg.SubmissionID)); err != nil {
			t.Fatal(err)
		}
	}
	if got := submittedTaskAttachments(t, cfg.DBPath, first); !reflect.DeepEqual(got, want) {
		t.Fatalf("attachments after refused changes = %+v, want %+v", got, want)
	}
}

// TestSubmitCommandRefusesBadAttachments covers the refusals --attach makes
// before anything is saved: no journal, no database, and no blob store.
func TestSubmitCommandRefusesBadAttachments(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	root := t.TempDir()
	base := attachmentSubmitConfig(t, root, "bad-attach")
	files := filepath.Join(root, "files")
	plain := writeAttachment(t, filepath.Join(files, "a", "notes.txt"), []byte("one"))
	sameName := writeAttachment(t, filepath.Join(files, "b", "notes.txt"), []byte("two"))
	bigFile := filepath.Join(files, "big.bin")
	writeAttachment(t, bigFile, nil)
	if err := os.Truncate(bigFile, domain.MaxTaskAttachmentFileBytes+1); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(files, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	bigImage := writeAttachment(t, filepath.Join(files, "big.png"),
		append([]byte(pngSignature), make([]byte, domain.MaxTaskAttachmentImageBytes)...))
	for name, tc := range map[string]struct {
		paths []string
		want  string
		is    error
	}{
		"directory":           {paths: []string{filepath.Join(files, "a")}, want: "is a directory"},
		"duplicate base name": {paths: []string{plain, sameName}, is: domain.ErrTaskAttachmentNameConflict},
		"over the file limit": {paths: []string{bigFile}, is: domain.ErrTaskAttachmentTooLarge},
		"over the image limit": {
			paths: []string{bigImage}, is: domain.ErrTaskAttachmentTooLarge,
		},
		"fifo":         {paths: []string{fifo}, want: "is not a regular file"},
		"missing file": {paths: []string{filepath.Join(files, "absent.txt")}, is: os.ErrNotExist},
	} {
		cfg := base
		cfg.AttachPaths = tc.paths
		_, err := runSubmitCommand(ctx, cfg)
		if err == nil || (tc.is != nil && !errors.Is(err, tc.is)) || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error = %v, want %v %q", name, err, tc.is, tc.want)
		}
	}
	for _, path := range []string{submissionJournalPath(base.DBPath, base.SubmissionID), base.DBPath, base.DBPath + ".blobs"} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s after refused attachments: %v, want absent", path, err)
		}
	}

	attach := []string{plain}
	if _, err := runSubmitCommand(ctx, submitCommandConfig{DBPath: base.DBPath, RunID: "run-legacy", AttachPaths: attach}); err == nil ||
		!strings.Contains(err.Error(), "takes no --attach") {
		t.Fatalf("lookup with attachments: error = %v, want the lookup-only refusal", err)
	}
	if _, err := runSubmitCommand(ctx, submitCommandConfig{DBPath: base.DBPath, RetrySubmissionID: "saved", AttachPaths: attach}); err == nil ||
		!strings.Contains(err.Error(), "manual retry takes only") {
		t.Fatalf("retry with attachments: error = %v, want the saved-inputs refusal", err)
	}
}

// TestSubmitCommandRefusesAnUnstoredAttachment covers the daemon-side
// resolution: a journal that names a blob the store does not hold is refused
// before the task is created, and writes nothing.
func TestSubmitCommandRefusesAnUnstoredAttachment(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	root := t.TempDir()
	cfg := attachmentSubmitConfig(t, root, "unstored-attach")
	cfg.AttachPaths = []string{writeAttachment(t, filepath.Join(root, "notes.txt"), []byte("notes"))}
	if _, err := runSubmitCommand(ctx, cfg); err != nil {
		t.Fatalf("submit: %v", err)
	}
	// Keep the journal; lose the database and the blobs it named.
	for _, path := range []string{cfg.DBPath, cfg.DBPath + "-wal", cfg.DBPath + "-shm", cfg.DBPath + ".blobs"} {
		if err := os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
	}
	_, err := runSubmitCommand(ctx, submitCommandConfig{DBPath: cfg.DBPath, RetrySubmissionID: cfg.SubmissionID})
	if !errors.Is(err, signet.ErrAttachmentNotStored) {
		t.Fatalf("retry without the blob: error = %v, want ErrAttachmentNotStored", err)
	}
	st := storetest.Open(t, cfg.DBPath, store.Options{})
	defer func() { _ = st.Close() }()
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetManualSubmission(ctx, "cli:"+cfg.SubmissionID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("submission after refusal = %v, want none", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAttachFlag(t *testing.T) {
	t.Parallel()
	var paths attachFlag
	for _, value := range []string{"a.png", "dir/b.txt"} {
		if err := paths.Set(value); err != nil {
			t.Fatalf("Set(%q): %v", value, err)
		}
	}
	if want := (attachFlag{"a.png", "dir/b.txt"}); !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	if err := new(attachFlag).Set(""); err == nil {
		t.Fatal("Set accepted an empty path")
	}
}

func TestClaimedAttachmentType(t *testing.T) {
	t.Parallel()
	for path, want := range map[string]string{
		"a.png": "image/png", "a.PNG": "image/png", "noext": "application/octet-stream",
		"a.unknown-extension": "application/octet-stream",
	} {
		if got := claimedAttachmentType(path); got != want {
			t.Errorf("claimedAttachmentType(%q) = %q, want %q", path, got, want)
		}
	}
	// A claim never carries parameters, whatever the extension table says.
	if got := claimedAttachmentType("a.html"); strings.Contains(got, ";") || domain.ValidateTaskAttachmentClaim(got) != nil {
		t.Errorf("claimedAttachmentType(a.html) = %q", got)
	}
}

// TestValidateSubmitApplyChecksAttachments covers the daemon side of the
// control socket: the references' shape is checked again, and a legacy
// lookup carries none.
func TestValidateSubmitApplyChecksAttachments(t *testing.T) {
	t.Parallel()
	good := signet.TaskAttachmentInput{
		Digest: submissionBytes([]byte("x")).digest, Name: "x.txt", MediaType: "text/plain",
	}
	for name, input := range map[string]signet.TaskAttachmentInput{
		"uppercase digest": {Digest: domain.Digest(strings.ToUpper(string(good.Digest))), Name: good.Name, MediaType: good.MediaType},
		"traversal name":   {Digest: good.Digest, Name: "../x", MediaType: good.MediaType},
		"claim parameters": {Digest: good.Digest, Name: good.Name, MediaType: "text/plain; charset=utf-8"},
	} {
		if err := validateSubmitApply(submitApplyRequest{
			Attachments: []signet.TaskAttachmentInput{input},
		}); !errors.Is(err, signet.ErrInvalidSubmitTaskPayload) {
			t.Fatalf("%s: error = %v, want ErrInvalidSubmitTaskPayload", name, err)
		}
	}
	if err := validateSubmitApply(submitApplyRequest{
		LegacyRunID: "run-legacy", Attachments: []signet.TaskAttachmentInput{good},
	}); err == nil || !strings.Contains(err.Error(), "legacy run lookup carries no attachments") {
		t.Fatalf("legacy lookup with attachments: error = %v, want the legacy refusal", err)
	}
}
