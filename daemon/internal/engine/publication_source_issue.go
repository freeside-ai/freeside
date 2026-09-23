package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const productionSourceIssueCheckpointKind = "production_source_issue_checkpoint"

var errSourceIssueCheckpoint = errors.New("source issue checkpoint unavailable")

// The two author sites share one durable observation, including a failed read.
// This engine-private inbox row binds advisory prose to the candidate and source;
// it never grants closure authority or changes the source's sensitivity class.
type productionSourceIssueCheckpoint struct {
	Version      string             `json:"version"`
	RunID        domain.RunID       `json:"run_id"`
	HeadSHA      string             `json:"head_sha"`
	BaseSHA      string             `json:"base_sha"`
	Repo         string             `json:"repo"`
	RepositoryID int64              `json:"repository_id"`
	IssueNumber  int                `json:"issue_number"`
	Text         *publish.IssueText `json:"text"`
}

func (w *productionPublicationWorkflow) publicationSourceIssueText(
	ctx context.Context, task productionPublicationTask, binding productionBinding, number int,
) (publish.IssueText, error) {
	key := "production-source-issue/" + string(task.RunID) + "/" + task.HeadSHA + "/" + binding.admission.Base.BaseSHA
	want := productionSourceIssueCheckpoint{
		Version: "1", RunID: task.RunID, HeadSHA: task.HeadSHA,
		BaseSHA: binding.admission.Base.BaseSHA, Repo: binding.admission.Base.Repo,
		RepositoryID: binding.admission.Base.RepositoryID, IssueNumber: number,
	}
	var saved productionSourceIssueCheckpoint
	found := false
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		entry, err := tx.GetInbox(ctx, key)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.Kind != productionSourceIssueCheckpointKind {
			return domain.ErrParentKeyMismatch
		}
		if err := strictjson.Decode(entry.Payload, &saved, strictjson.RejectInvalidUTF8, strictjson.NoLimit); err != nil {
			return err
		}
		identity := saved
		identity.Text = nil
		if identity != want {
			return domain.ErrParentKeyMismatch
		}
		if saved.Text != nil && strings.TrimSpace(saved.Text.Title) == "" {
			return fmt.Errorf("source issue checkpoint carries no title: %w", domain.ErrParentKeyMismatch)
		}
		found = true
		return nil
	})
	if err != nil {
		return publish.IssueText{}, fmt.Errorf("%w: %w", errSourceIssueCheckpoint, err)
	}
	if !found {
		saved = want
		text, readErr := w.publisher.SourceIssueText(ctx, publish.Candidate{Repo: want.Repo}, number)
		if readErr == nil {
			saved.Text = &text
		}
		payload, err := json.Marshal(saved)
		if err != nil {
			return publish.IssueText{}, fmt.Errorf("%w: %w", errSourceIssueCheckpoint, err)
		}
		if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
			entry, _, err := tx.RecordInbox(ctx, key, productionSourceIssueCheckpointKind, payload)
			if err != nil {
				return err
			}
			if entry.Kind != productionSourceIssueCheckpointKind || !bytes.Equal(entry.Payload, payload) {
				return domain.ErrImmutableTransition
			}
			return nil
		}); err != nil {
			return publish.IssueText{}, fmt.Errorf("%w: %w", errSourceIssueCheckpoint, err)
		}
	}
	if saved.Text == nil {
		return publish.IssueText{}, errors.New("source issue read failed for this candidate")
	}
	return *saved.Text, nil
}
