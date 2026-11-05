package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// inputFilesFixture is an agent-admission fixture with the task the store
// bound its run to.
type inputFilesFixture struct {
	*agentAdmissionFixture
	taskID domain.TaskID
}

func newInputFilesFixture(t *testing.T) inputFilesFixture {
	t.Helper()
	f := newAgentAdmissionFixture(t)
	return inputFilesFixture{agentAdmissionFixture: f, taskID: f.binding.run.TaskID}
}

func (f inputFilesFixture) putTaskAttachments(t *testing.T, list []domain.TaskAttachment) {
	t.Helper()
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.PutTaskAttachments(t.Context(), f.taskID, list)
	}); err != nil {
		t.Fatal(err)
	}
}

func (f inputFilesFixture) putBlob(t *testing.T, body []byte) domain.Digest {
	t.Helper()
	digest := digestBytes(body)
	if _, err := f.engine.specification.blobs.Put(digest, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	return digest
}

// bindConversation binds the fixture's invocations to a conversation holding
// messages, read through the first through of them.
func (f *inputFilesFixture) bindConversation(t *testing.T, through int, messages ...domain.Message) {
	t.Helper()
	conversation := domain.Conversation{ID: "conversation-1", Status: domain.ConversationIdle}
	for _, message := range messages {
		conversation, _ = conversation.Append(message)
	}
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.PutConversation(t.Context(), conversation)
	}); err != nil {
		t.Fatal(err)
	}
	f.binding.conversation = conversation
	f.binding.invocation.ConversationID = &conversation.ID
	f.binding.invocation.ThroughSequence = through
}

func inputMessage(t *testing.T, sequence int, author domain.Author, attachments ...domain.Digest) domain.Message {
	t.Helper()
	message, err := domain.NewMessage(
		domain.MessageID(fmt.Sprintf("message-%d", sequence)), "conversation-1", author, "see attached",
		attachments, agentTestAt.Add(time.Duration(sequence)*time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func taskAttachment(name string, media domain.TaskAttachmentMediaType, body []byte) domain.TaskAttachment {
	return domain.TaskAttachment{
		Digest: digestBytes(body), Name: domain.TaskAttachmentName(name), MediaType: media,
		ClaimedMediaType: string(media), SizeBytes: int64(len(body)),
	}
}

// admitAs admits the fixture's binding as a writer stage of the given role,
// with the invocation and stage IDs that role's workflow uses.
func (f inputFilesFixture) admitAs(t *testing.T, role domain.RoleName) (domain.ExecutionAdmission, bool, error) {
	t.Helper()
	binding := f.binding
	runID := binding.run.ID
	var stage domain.Stage
	switch role {
	case domain.RoleSpecifier:
		stage = domain.Stage{ID: specificationStageID(runID), RunID: runID, Name: specificationStageName}
		// A specifier reads only typed specification inputs, none here.
		invocation, err := domain.NewAgentInvocation(specificationInvocationID(runID, 1), nil,
			binding.invocation.ConversationID, binding.invocation.ThroughSequence)
		if err != nil {
			t.Fatal(err)
		}
		binding.invocation = invocation
	case domain.RoleImplementer:
		stage = f.stage
	case domain.RoleRemediator:
		stage = domain.Stage{ID: remediationStageID(runID, 1), RunID: runID, Name: productionStageName}
		binding.invocation.ID = remediationInvocationID(runID, 1)
	default:
		t.Fatalf("role %s is not a writer", role)
	}
	return f.engine.admitAttempt(context.Background(), binding, stage, binding.invocation.ID)
}

// Every Ward writer receives the task's attachments, in submitted order and
// under task/, then the attachments of user-authored discussion messages the
// invocation reads, named by sequence and position with the type sniffed from
// the stored bytes. An agent's attachment and a message past the bound are
// not delivered, and names differing only in case both arrive.
func TestWardWritersReceiveOperatorInputFiles(t *testing.T) {
	t.Parallel()
	f := newInputFilesFixture(t)
	screenshot := []byte("\x89PNG\r\n\x1a\nhome page")
	tokens := []byte(`{"color":"teal"}`)
	upper, lower := []byte("\x89PNG\r\n\x1a\nupper"), []byte("\x89PNG\r\n\x1a\nlower")
	task := []domain.TaskAttachment{
		taskAttachment("screens/home.png", domain.TaskAttachmentMediaPNG, screenshot),
		taskAttachment("design/tokens/colors.json", domain.TaskAttachmentMediaOctetStream, tokens),
		taskAttachment("A.png", domain.TaskAttachmentMediaPNG, upper),
		taskAttachment("a.png", domain.TaskAttachmentMediaPNG, lower),
	}
	f.putTaskAttachments(t, task)
	discussShot := []byte("\x89PNG\r\n\x1a\ndiscuss")
	notes, gif := []byte("plain notes"), []byte("GIF89a frames")
	agentShot := []byte("\x89PNG\r\n\x1a\nagent made this")
	late := []byte("\x89PNG\r\n\x1a\nafter the bound")
	f.bindConversation(t, 3,
		inputMessage(t, 1, domain.AuthorUser, f.putBlob(t, discussShot)),
		inputMessage(t, 2, domain.AuthorAgent, f.putBlob(t, agentShot)),
		inputMessage(t, 3, domain.AuthorUser, f.putBlob(t, notes), f.putBlob(t, gif)),
		inputMessage(t, 4, domain.AuthorUser, f.putBlob(t, late)),
	)
	want := []domain.StageInputAttachment{
		{Digest: task[0].Digest, Name: "task/screens/home.png", MediaType: domain.TaskAttachmentMediaPNG, SizeBytes: task[0].SizeBytes},
		{Digest: task[1].Digest, Name: "task/design/tokens/colors.json", MediaType: domain.TaskAttachmentMediaOctetStream, SizeBytes: task[1].SizeBytes},
		{Digest: task[2].Digest, Name: "task/A.png", MediaType: domain.TaskAttachmentMediaPNG, SizeBytes: task[2].SizeBytes},
		{Digest: task[3].Digest, Name: "task/a.png", MediaType: domain.TaskAttachmentMediaPNG, SizeBytes: task[3].SizeBytes},
		{Digest: digestBytes(discussShot), Name: "discussion/1/1.png", MediaType: domain.TaskAttachmentMediaPNG, SizeBytes: int64(len(discussShot))},
		{Digest: digestBytes(notes), Name: "discussion/3/1.bin", MediaType: domain.TaskAttachmentMediaOctetStream, SizeBytes: int64(len(notes))},
		{Digest: digestBytes(gif), Name: "discussion/3/2.gif", MediaType: domain.TaskAttachmentMediaGIF, SizeBytes: int64(len(gif))},
	}
	for _, role := range []domain.RoleName{domain.RoleSpecifier, domain.RoleImplementer, domain.RoleRemediator} {
		t.Run(string(role), func(t *testing.T) {
			admission, admitted, err := f.admitAs(t, role)
			if err != nil || !admitted {
				t.Fatalf("admitted %t: %v", admitted, err)
			}
			if got := admission.StageInputs.Attachments; !slices.Equal(got, want) {
				t.Fatalf("input files =\n%+v\nwant\n%+v", got, want)
			}
			// Delivered files are operator input, never opaque prior artifacts.
			for _, digest := range admission.StageInputs.PriorArtifactDigests {
				if digest == digestBytes(agentShot) || digest == digestBytes(discussShot) {
					t.Fatalf("message attachment %s admitted as a prior artifact", digest)
				}
			}
		})
	}
}

// Admission fails closed, before any start, when a discussion blob is
// missing, an image is over its limit, the combined list breaks the
// snapshot's limits, or the task/ prefix makes a valid task name too long.
// A list refusal is permanent, so every writer stage and the prospective
// specification check classify it as their stage's undeliverable input and
// record a terminal outcome rather than retrying forever; a missing blob is an
// operational fault and stays retryable.
func TestOperatorInputFilesFailClosed(t *testing.T) {
	t.Parallel()
	missing := digestBytes([]byte("never stored"))
	for name, tc := range map[string]struct {
		task    func(t *testing.T, f inputFilesFixture) []domain.TaskAttachment
		message func(t *testing.T, f inputFilesFixture) domain.Digest
		want    error
	}{
		"missing blob": {
			message: func(*testing.T, inputFilesFixture) domain.Digest { return missing },
		},
		"discussion image over its limit": {
			message: func(t *testing.T, f inputFilesFixture) domain.Digest {
				body := make([]byte, domain.MaxTaskAttachmentImageBytes+1)
				copy(body, "\x89PNG\r\n\x1a\n")
				return f.putBlob(t, body)
			},
			want: domain.ErrTaskAttachmentTooLarge,
		},
		"over the count": {
			task: func(*testing.T, inputFilesFixture) []domain.TaskAttachment {
				list := make([]domain.TaskAttachment, domain.MaxTaskAttachments)
				for i := range list {
					list[i] = taskAttachment(fmt.Sprintf("f%04d.bin", i), domain.TaskAttachmentMediaOctetStream, []byte{byte(i)})
				}
				return list
			},
			message: func(t *testing.T, f inputFilesFixture) domain.Digest {
				return f.putBlob(t, []byte("\x89PNG\r\n\x1a\none too many"))
			},
			want: domain.ErrTooManyTaskAttachments,
		},
		"prefixed name too long": {
			task: func(*testing.T, inputFilesFixture) []domain.TaskAttachment {
				long := strings.Repeat("d/", (domain.MaxTaskAttachmentNameBytes-4)/2) + "x.md"
				return []domain.TaskAttachment{taskAttachment(long, domain.TaskAttachmentMediaOctetStream, []byte("x"))}
			},
			want: domain.ErrInvalidTaskAttachmentName,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newInputFilesFixture(t)
			if tc.task != nil {
				list := tc.task(t, f)
				if err := domain.ValidateTaskAttachments(list); err != nil {
					t.Fatalf("fixture task list is not a valid submission: %v", err)
				}
				f.putTaskAttachments(t, list)
			}
			// Every case reads a conversation, which a specifier with no
			// typed inputs needs.
			var attachments []domain.Digest
			if tc.message != nil {
				attachments = append(attachments, tc.message(t, f))
			}
			f.bindConversation(t, 1, inputMessage(t, 1, domain.AuthorUser, attachments...))
			undeliverable := map[domain.RoleName][]error{
				domain.RoleSpecifier:   {ErrSpecificationInputUndeliverable},
				domain.RoleImplementer: {ErrProductionInputUndeliverable},
				domain.RoleRemediator:  {ErrRemediationInputUndeliverable, ErrProductionInputUndeliverable},
			}
			for _, role := range []domain.RoleName{domain.RoleSpecifier, domain.RoleImplementer, domain.RoleRemediator} {
				_, admitted, err := f.admitAs(t, role)
				if admitted || err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
					t.Fatalf("%s: admitted %t, err %v, want %v", role, admitted, err, tc.want)
				}
				for _, sentinel := range undeliverable[role] {
					if got := errors.Is(err, sentinel); got != (tc.want != nil) {
						t.Fatalf("%s: undeliverable %v = %t: %v", role, sentinel, got, err)
					}
				}
			}
			f.engine.specification.validateDelivery = func(context.Context, exec.StartSpec) error { return nil }
			err := f.engine.validateProspectiveDelivery(t.Context(), f.binding.run, f.binding.invocation,
				f.engine.admission.environment.PromptPackageDigest, false, nil)
			if err == nil || errors.Is(err, ErrSpecificationInputUndeliverable) != (tc.want != nil) {
				t.Fatalf("prospective delivery check: %v", err)
			}
		})
	}
}
