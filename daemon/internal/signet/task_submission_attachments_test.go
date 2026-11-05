package signet_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

var pngSignature = []byte("\x89PNG\r\n\x1a\n")

// newAttachmentSubmitService is newSubmitTaskService that also hands back the
// blob store, so a test can upload what its commands reference.
func newAttachmentSubmitService(t *testing.T, submitter signet.TaskSubmitter) (*signet.Service, *store.Store, *signet.BlobStore) {
	t.Helper()
	ctx := context.Background()
	s := storetest.Open(t, t.TempDir()+"/signet.db", store.Options{})
	t.Cleanup(func() { _ = s.Close() })
	blobs, err := signet.NewBlobStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewBlobStore: %v", err)
	}
	device := domain.Device{
		ID: "device-1", DisplayName: "Test Mac", Status: domain.DeviceActive,
		PairedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutDevice(ctx, device) }); err != nil {
		t.Fatalf("seed device: %v", err)
	}
	return signet.NewService(s, signet.WithBlobStore(blobs), signet.WithTaskSubmitter(submitter)), s, blobs
}

// putBlob stores body and returns its digest.
func putBlob(t *testing.T, blobs *signet.BlobStore, body []byte) domain.Digest {
	t.Helper()
	digest := domain.Digest(contentaddr.Sum(body))
	if _, err := blobs.Put(digest, bytes.NewReader(body)); err != nil {
		t.Fatalf("put blob: %v", err)
	}
	return digest
}

// putZeroBlob stores size bytes that start with head and are zero after it,
// streamed so a large blob never sits in memory twice.
func putZeroBlob(t *testing.T, blobs *signet.BlobStore, head []byte, size int64) domain.Digest {
	t.Helper()
	body := func() io.Reader {
		return io.MultiReader(bytes.NewReader(head), io.LimitReader(zeroReader{}, size-int64(len(head))))
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, body()); err != nil {
		t.Fatal(err)
	}
	digest := domain.Digest(contentaddr.Format(hasher.Sum(nil)))
	if _, err := blobs.Put(digest, body()); err != nil {
		t.Fatalf("put blob: %v", err)
	}
	return digest
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func submitTaskCommandWithAttachments(commandID string, attachments ...signet.TaskAttachmentInput) signet.ClientCommand {
	cmd := submitTaskCommand(commandID, "Add a health endpoint.", "Health endpoint")
	cmd.SubmitTask.Attachments = attachments
	return cmd
}

// TestSubmitTaskCommandResolvesAttachmentsInOrder: the submitter receives the
// attachments in submitted order, each with the stored size and the media
// type the daemon sniffs, whatever the client claimed.
func TestSubmitTaskCommandResolvesAttachmentsInOrder(t *testing.T) {
	t.Parallel()
	submitter := &fakeTaskSubmitter{}
	service, _, blobs := newAttachmentSubmitService(t, submitter)
	png := append(append([]byte(nil), pngSignature...), "image body"...)
	text := []byte(`{"color":"#fff"}`)
	cmd := submitTaskCommandWithAttachments("cmd-1",
		signet.TaskAttachmentInput{Digest: putBlob(t, blobs, text), Name: "tokens/colors.json", MediaType: "application/json"},
		// The claim says text; the bytes say PNG, and the bytes decide.
		signet.TaskAttachmentInput{Digest: putBlob(t, blobs, png), Name: "a.png", MediaType: "text/plain"},
	)
	if _, err := service.Submit(context.Background(), cmd); err != nil {
		t.Fatalf("submit: %v", err)
	}
	want := []domain.TaskAttachment{
		{
			Digest: cmd.SubmitTask.Attachments[0].Digest, Name: "tokens/colors.json",
			MediaType: domain.TaskAttachmentMediaOctetStream, ClaimedMediaType: "application/json", SizeBytes: int64(len(text)),
		},
		{
			Digest: cmd.SubmitTask.Attachments[1].Digest, Name: "a.png",
			MediaType: domain.TaskAttachmentMediaPNG, ClaimedMediaType: "text/plain", SizeBytes: int64(len(png)),
		},
	}
	if !reflect.DeepEqual(submitter.last.Attachments, want) {
		t.Fatalf("submitter attachments = %+v, want %+v", submitter.last.Attachments, want)
	}
}

// TestSubmitTaskCommandBindsAttachmentsToItsIdentity covers the replay rule:
// the same list under the same command_id returns the recorded result, and
// any other list, including the same entries reordered, is refused.
func TestSubmitTaskCommandBindsAttachmentsToItsIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	submitter := &fakeTaskSubmitter{}
	service, _, blobs := newAttachmentSubmitService(t, submitter)
	first := signet.TaskAttachmentInput{Digest: putBlob(t, blobs, []byte("one")), Name: "one.txt", MediaType: "text/plain"}
	second := signet.TaskAttachmentInput{Digest: putBlob(t, blobs, []byte("two")), Name: "two.txt", MediaType: "text/plain"}
	third := signet.TaskAttachmentInput{Digest: putBlob(t, blobs, []byte("three")), Name: "three.txt", MediaType: "text/plain"}

	result, err := service.Submit(ctx, submitTaskCommandWithAttachments("cmd-1", first, second))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	replay, err := service.Submit(ctx, submitTaskCommandWithAttachments("cmd-1", first, second))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replay.Submission == nil || !reflect.DeepEqual(*replay.Submission, *result.Submission) || replay.Revision != result.Revision {
		t.Fatalf("replay result = %+v, want %+v", replay, result)
	}
	renamed := second
	renamed.Name = "renamed.txt"
	reclaimed := second
	reclaimed.MediaType = "application/octet-stream"
	for name, cmd := range map[string]signet.ClientCommand{
		"reordered":      submitTaskCommandWithAttachments("cmd-1", second, first),
		"removed":        submitTaskCommandWithAttachments("cmd-1", first),
		"added":          submitTaskCommandWithAttachments("cmd-1", first, second, third),
		"renamed":        submitTaskCommandWithAttachments("cmd-1", first, renamed),
		"claim changed":  submitTaskCommandWithAttachments("cmd-1", first, reclaimed),
		"other content":  submitTaskCommandWithAttachments("cmd-1", first, signet.TaskAttachmentInput{Digest: third.Digest, Name: second.Name, MediaType: second.MediaType}),
		"no attachments": submitTaskCommandWithAttachments("cmd-1"),
	} {
		if _, err := service.Submit(ctx, cmd); !errors.Is(err, store.ErrImmutableConflict) {
			t.Fatalf("%s: error = %v, want ErrImmutableConflict", name, err)
		}
	}
	// The reverse: a submission recorded without attachments does not
	// replay for a retry that adds one.
	if _, err := service.Submit(ctx, submitTaskCommandWithAttachments("cmd-2")); err != nil {
		t.Fatalf("submit without attachments: %v", err)
	}
	if _, err := service.Submit(ctx, submitTaskCommandWithAttachments("cmd-2", first)); !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("attachment added to a plain submission: error = %v, want ErrImmutableConflict", err)
	}
	if submitter.calls != 2 {
		t.Fatalf("submitter calls = %d, want 2 (one per accepted command)", submitter.calls)
	}
}

// TestSubmitTaskRequestDigestWithAttachmentsIsPinned pins the bytes a
// submission with attachments is digested over: the references as sent, in
// order, under their wire names. A change here strands every recorded
// submission that carries one.
func TestSubmitTaskRequestDigestWithAttachmentsIsPinned(t *testing.T) {
	t.Parallel()
	submitter := &fakeTaskSubmitter{}
	service, _, blobs := newAttachmentSubmitService(t, submitter)
	digest := putBlob(t, blobs, []byte("hello"))
	cmd := submitTaskCommandWithAttachments("cmd-1",
		signet.TaskAttachmentInput{Digest: digest, Name: "notes/hello.txt", MediaType: "text/plain"})
	if _, err := service.Submit(context.Background(), cmd); err != nil {
		t.Fatalf("submit: %v", err)
	}
	pinned := `{"Device":"device-1","Payload":{"ProjectID":"project-1","Source":"Add a health endpoint.","Name":"Health endpoint",` +
		`"Attachments":[{"digest":"` + string(digest) + `","name":"notes/hello.txt","media_type":"text/plain"}]}}`
	if want := domain.Digest(contentaddr.Sum([]byte(pinned))); submitter.last.RequestDigest != want {
		t.Fatalf("request digest = %s, want %s", submitter.last.RequestDigest, want)
	}
}

// TestSubmitTaskCommandRejectsMalformedAttachments: every rule refuses the
// command before the submitter runs, so nothing is written.
func TestSubmitTaskCommandRejectsMalformedAttachments(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	submitter := &fakeTaskSubmitter{}
	service, s, blobs := newAttachmentSubmitService(t, submitter)
	stored := putBlob(t, blobs, []byte("stored"))
	ok := func(name string) signet.TaskAttachmentInput {
		return signet.TaskAttachmentInput{Digest: stored, Name: domain.TaskAttachmentName(name), MediaType: "text/plain"}
	}
	unstored := domain.Digest(contentaddr.Sum([]byte("never uploaded")))
	// A PNG one byte over the image limit, and a non-image one byte over
	// the per-file limit.
	bigImage := putZeroBlob(t, blobs, pngSignature, domain.MaxTaskAttachmentImageBytes+1)
	bigFile := putZeroBlob(t, blobs, []byte("not an image"), domain.MaxTaskAttachmentFileBytes+1)
	// A non-image at the image limit's size plus one is fine: the smaller
	// limit is for images only.
	fileOverImageLimit := putZeroBlob(t, blobs, []byte("not an image"), domain.MaxTaskAttachmentImageBytes+1)
	if _, err := service.Submit(ctx, submitTaskCommandWithAttachments("cmd-file-ok",
		signet.TaskAttachmentInput{Digest: fileOverImageLimit, Name: "big.bin", MediaType: "application/octet-stream"})); err != nil {
		t.Fatalf("a non-image over the image limit: %v", err)
	}
	tooMany := make([]signet.TaskAttachmentInput, domain.MaxTaskAttachments+1)
	for i := range tooMany {
		tooMany[i] = ok(fmt.Sprintf("f%d.txt", i))
	}
	cases := map[string]struct {
		list []signet.TaskAttachmentInput
		want error
	}{
		"empty list":       {[]signet.TaskAttachmentInput{}, signet.ErrInvalidSubmitTaskPayload},
		"unstored digest":  {[]signet.TaskAttachmentInput{{Digest: unstored, Name: "a.txt", MediaType: "text/plain"}}, signet.ErrAttachmentNotStored},
		"uppercase digest": {[]signet.TaskAttachmentInput{{Digest: domain.Digest(strings.ToUpper(string(stored))), Name: "a.txt", MediaType: "text/plain"}}, domain.ErrInvalidTaskAttachmentDigest},
		"traversal":        {[]signet.TaskAttachmentInput{ok("a/../../etc/passwd")}, domain.ErrInvalidTaskAttachmentName},
		"absolute":         {[]signet.TaskAttachmentInput{ok("/etc/passwd")}, domain.ErrInvalidTaskAttachmentName},
		"control char":     {[]signet.TaskAttachmentInput{ok("a\x00b")}, domain.ErrInvalidTaskAttachmentName},
		"long segment":     {[]signet.TaskAttachmentInput{ok(strings.Repeat("a", 256))}, domain.ErrInvalidTaskAttachmentName},
		"missing name":     {[]signet.TaskAttachmentInput{ok("")}, domain.ErrEmptyField},
		"duplicate name":   {[]signet.TaskAttachmentInput{ok("a.txt"), ok("a.txt")}, domain.ErrTaskAttachmentNameConflict},
		"name is a dir":    {[]signet.TaskAttachmentInput{ok("docs"), ok("docs/a.txt")}, domain.ErrTaskAttachmentNameConflict},
		"claim params":     {[]signet.TaskAttachmentInput{{Digest: stored, Name: "a.txt", MediaType: "text/plain; charset=utf-8"}}, domain.ErrInvalidTaskAttachmentClaim},
		"missing claim":    {[]signet.TaskAttachmentInput{{Digest: stored, Name: "a.txt"}}, domain.ErrInvalidTaskAttachmentClaim},
		"too many":         {tooMany, domain.ErrTooManyTaskAttachments},
		"image too large":  {[]signet.TaskAttachmentInput{{Digest: bigImage, Name: "big.png", MediaType: "image/png"}}, domain.ErrTaskAttachmentTooLarge},
		"file too large":   {[]signet.TaskAttachmentInput{{Digest: bigFile, Name: "big.bin", MediaType: "application/octet-stream"}}, domain.ErrTaskAttachmentTooLarge},
	}
	for name, tc := range cases {
		commandID := "cmd-" + strings.ReplaceAll(name, " ", "-")
		_, err := service.Submit(ctx, submitTaskCommandWithAttachments(commandID, tc.list...))
		if !errors.Is(err, tc.want) {
			t.Fatalf("%s: error = %v, want %v", name, err, tc.want)
		}
		// Each refusal is a request error the HTTP boundary answers 400.
		if !errors.Is(err, signet.ErrInvalidSubmitTaskPayload) && !errors.Is(err, signet.ErrAttachmentNotStored) {
			t.Fatalf("%s: error = %v is not a submit_task request error", name, err)
		}
		if err := s.Read(ctx, func(tx *store.ReadTx) error {
			_, _, err := tx.GetTaskSubmissionSnapshot(ctx, commandID)
			return err
		}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%s: submission lookup = %v, want ErrNotFound (nothing written)", name, err)
		}
	}
	if submitter.calls != 1 {
		t.Fatalf("submitter calls = %d, want 1 (only the accepted command)", submitter.calls)
	}
}

// TestLegacyTaskSubmissionRefusesAttachments: a historical record holds no
// request digest to compare and predates attachments, so a retry that
// carries one is a different request.
func TestLegacyTaskSubmissionRefusesAttachments(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fake := &fakeTaskSubmitter{}
	service, s, blobs := newAttachmentSubmitService(t, fake)
	const source = "Historical source"
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		outcome, err := fake.SubmitTask(ctx, tx, signet.TaskSubmissionInput{ProjectID: "project-1"})
		if err != nil {
			return err
		}
		return tx.PutTaskSubmission(ctx, domain.TaskSubmission{
			CommandID: "old-command", Origin: domain.TaskSubmissionOriginDevice, DeviceID: new(domain.DeviceID("device-1")),
			ProjectID: "project-1", SourceDigest: domain.Digest(contentaddr.Sum([]byte(source))), TaskID: outcome.TaskID,
			SpecificationRunID: outcome.SpecificationRunID, Name: outcome.Name,
		})
	}); err != nil {
		t.Fatal(err)
	}
	retry := submitTaskCommand("old-command", source, "")
	if _, err := service.Submit(ctx, retry); err != nil {
		t.Fatalf("plain legacy replay: %v", err)
	}
	retry.SubmitTask.Attachments = []signet.TaskAttachmentInput{
		{Digest: putBlob(t, blobs, []byte("x")), Name: "x.txt", MediaType: "text/plain"},
	}
	if _, err := service.Submit(ctx, retry); !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("legacy replay with an attachment: error = %v, want ErrImmutableConflict", err)
	}
}

// TestSubmitTaskCommandHTTPRecordsAttachments runs the engine-backed
// submitter behind the HTTP handler: the attachments land with the task, the
// bootstrap shows them in order, a retry writes no second set, and a task
// without any shows an empty list.
func TestSubmitTaskCommandHTTPRecordsAttachments(t *testing.T) {
	t.Parallel()
	handler, _ := newRealSubmitTaskHandlerAndStore(t)
	upload := func(body []byte) domain.Digest {
		t.Helper()
		digest := domain.Digest(contentaddr.Sum(body))
		response := authenticatedRequest(t, handler, http.MethodPut, "/attachments/"+string(digest), bytes.NewReader(body))
		if response.Code != http.StatusCreated && response.Code != http.StatusOK {
			t.Fatalf("upload status = %d body=%s", response.Code, response.Body.String())
		}
		return digest
	}
	post := func(commandID, attachments string) int {
		t.Helper()
		body := `{"command_id":"` + commandID + `","device_id":"device-1","payload":{"kind":"submit_task",` +
			`"project_id":"project-1","source":"Add a health endpoint."` + attachments + `}}`
		response := authenticatedRequest(t, handler, http.MethodPost, "/commands", bytes.NewReader([]byte(body)))
		return response.Code
	}
	png := append(append([]byte(nil), pngSignature...), "pixels"...)
	pngDigest, textDigest := upload(png), upload([]byte("notes"))
	list := `,"attachments":[` +
		`{"digest":"` + string(textDigest) + `","name":"docs/notes.md","media_type":"text/markdown"},` +
		`{"digest":"` + string(pngDigest) + `","name":"screenshot.png","media_type":"image/png"}]`
	if code := post("cmd-attach", list); code != http.StatusOK {
		t.Fatalf("submit status = %d, want 200", code)
	}
	if code := post("cmd-attach", list); code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200", code)
	}
	if code := post("cmd-plain", ""); code != http.StatusOK {
		t.Fatalf("plain submit status = %d, want 200", code)
	}

	response := authenticatedRequest(t, handler, http.MethodGet, "/sync/bootstrap", bytes.NewReader(nil))
	if response.Code != http.StatusOK {
		t.Fatalf("bootstrap status = %d", response.Code)
	}
	var bootstrap struct {
		Tasks []struct {
			Task struct {
				Attachments json.RawMessage `json:"attachments"`
			} `json:"task"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &bootstrap); err != nil {
		t.Fatal(err)
	}
	want := `[{"digest":"` + string(textDigest) + `","name":"docs/notes.md","media_type":"application/octet-stream",` +
		`"claimed_media_type":"text/markdown","size_bytes":5},` +
		`{"digest":"` + string(pngDigest) + `","name":"screenshot.png","media_type":"image/png",` +
		`"claimed_media_type":"image/png","size_bytes":` + fmt.Sprint(len(png)) + `}]`
	var got []string
	for _, task := range bootstrap.Tasks {
		got = append(got, string(task.Task.Attachments))
	}
	if len(got) != 2 || !slices.Contains(got, want) || !slices.Contains(got, `[]`) {
		t.Fatalf("bootstrap attachments = %v, want %s and []", got, want)
	}
}

// TestSubmitTaskCommandHTTPRejectsMalformedAttachments covers the wire shapes
// the contract forbids. Each answers 400, writes nothing, and never echoes
// the refused text, which may be something pasted into the wrong field.
func TestSubmitTaskCommandHTTPRejectsMalformedAttachments(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	handler, s := newRealSubmitTaskHandlerAndStore(t)
	const secret = "ghp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	stored := domain.Digest(contentaddr.Sum([]byte("stored")))
	if response := authenticatedRequest(t, handler, http.MethodPut, "/attachments/"+string(stored),
		bytes.NewReader([]byte("stored"))); response.Code != http.StatusCreated {
		t.Fatalf("upload status = %d", response.Code)
	}
	entry := func(digest, name, claim string) string {
		return `{"digest":"` + digest + `","name":"` + name + `","media_type":"` + claim + `"}`
	}
	for name, list := range map[string]string{
		"empty list":     `[]`,
		"not a list":     entry(string(stored), "a.txt", "text/plain"),
		"unknown field":  `[{"digest":"` + string(stored) + `","name":"a.txt","media_type":"text/plain","size_bytes":1}]`,
		"secret digest":  `[` + entry(secret, "a.txt", "text/plain") + `]`,
		"secret name":    `[` + entry(string(stored), "../"+secret, "text/plain") + `]`,
		"secret claim":   `[` + entry(string(stored), "a.txt", secret) + `]`,
		"unstored":       `[` + entry(string(contentaddr.Sum([]byte("missing"))), "a.txt", "text/plain") + `]`,
		"duplicate name": `[` + entry(string(stored), secret, "text/plain") + `,` + entry(string(stored), secret, "text/plain") + `]`,
	} {
		commandID := "cmd-" + strings.ReplaceAll(name, " ", "-")
		body := `{"command_id":"` + commandID + `","device_id":"device-1","payload":{"kind":"submit_task",` +
			`"project_id":"project-1","source":"Body.","attachments":` + list + `}}`
		response := authenticatedRequest(t, handler, http.MethodPost, "/commands", bytes.NewReader([]byte(body)))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d body=%s, want 400", name, response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("%s: 400 body echoes the refused text: %s", name, response.Body.String())
		}
		if err := s.Read(ctx, func(tx *store.ReadTx) error {
			_, _, err := tx.GetTaskSubmissionSnapshot(ctx, commandID)
			return err
		}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%s: submission lookup = %v, want ErrNotFound (nothing written)", name, err)
		}
	}
}
