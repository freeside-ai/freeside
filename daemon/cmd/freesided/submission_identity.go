package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// prepareSubmission freezes inputs in memory or restores a manual retry.
// Validation uses this snapshot before a new recovery journal is published.
func prepareSubmission(cfg submitCommandConfig) (submitCommandConfig, error) {
	if cfg.DBPath == "" {
		return cfg, errors.New("submit: -db is required")
	}
	if cfg.RunID != "" {
		if cfg.SubmissionID != "" || cfg.RetrySubmissionID != "" {
			return cfg, errors.New("submit: --run-id is lookup-only and cannot name a new submission")
		}
		return cfg, nil
	}
	if cfg.RetrySubmissionID != "" {
		if cfg.SubmissionID != "" || cfg.TaskPath != "" || cfg.PolicyPath != "" || cfg.PublicationPath != "" ||
			cfg.WorkUnitPath != "" || cfg.CompositionPath != "" || cfg.ProjectID != "" || cfg.RequireComposition {
			return cfg, errors.New("submit: manual retry takes only --db and --retry-submission-id; it uses the saved inputs")
		}
		if !validSubmissionID(cfg.RetrySubmissionID) {
			return cfg, errors.New("submit: invalid submission identity")
		}
		body, err := os.ReadFile(submissionJournalPath(cfg.DBPath, cfg.RetrySubmissionID))
		if err != nil {
			return cfg, fmt.Errorf("submit: read saved submission: %w", err)
		}
		var saved submitCommandConfig
		if err := json.Unmarshal(body, &saved); err != nil {
			return cfg, err
		}
		if saved.SubmissionID != cfg.RetrySubmissionID || saved.DBPath != cfg.DBPath || saved.SavedInputs == nil || saved.RunID != "" {
			return cfg, errors.New("submit: saved submission ownership disagrees")
		}
		return saved, nil
	}
	if cfg.SubmissionID == "" {
		cfg.SubmissionID = rand.Text()
	}
	if !validSubmissionID(cfg.SubmissionID) {
		return cfg, errors.New("submit: submission identity must contain 1–128 ASCII letters, digits, underscores or hyphens")
	}
	cfg.SavedInputs = map[string][]byte{}
	for role, path := range map[string]string{
		"task": cfg.TaskPath, "policy": cfg.PolicyPath,
		"publication": cfg.PublicationPath, "work-unit": cfg.WorkUnitPath, "composition": cfg.CompositionPath,
	} {
		if path == "" {
			continue
		}
		input, err := readSubmissionFile(path)
		if err != nil {
			return cfg, err
		}
		cfg.SavedInputs[role] = input.body
	}
	saved, err := matchingRetainedSubmission(cfg)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	return saved, err
}

// retainSubmission publishes the validated snapshot before database acceptance.
// A manual retry always keeps the original bytes, even when a repeated request
// supplies equivalent JSON with different formatting.
func retainSubmission(cfg submitCommandConfig) (submitCommandConfig, error) {
	if cfg.RunID != "" {
		return cfg, nil
	}
	path := submissionJournalPath(cfg.DBPath, cfg.SubmissionID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return cfg, err
	}
	body, err := json.Marshal(cfg)
	if err != nil {
		return cfg, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".submission-*")
	if err != nil {
		return cfg, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, writeErr := tmp.Write(body)
	err = errors.Join(writeErr, tmp.Sync(), tmp.Close())
	if err != nil {
		return cfg, err
	}
	// Linking publishes a complete journal atomically without replacing the
	// original when an identity is delivered again.
	if err := os.Link(tmp.Name(), path); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return cfg, err
		}
		return matchingRetainedSubmission(cfg)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return cfg, err
	}
	if err := errors.Join(dir.Sync(), dir.Close()); err != nil {
		return cfg, err
	}
	_, _ = fmt.Fprintf(os.Stderr, "Saved submission %s. Manual retry: freesided submit --db %q --retry-submission-id %s\n", cfg.SubmissionID, cfg.DBPath, cfg.SubmissionID)
	return cfg, nil
}

func matchingRetainedSubmission(cfg submitCommandConfig) (submitCommandConfig, error) {
	body, err := os.ReadFile(submissionJournalPath(cfg.DBPath, cfg.SubmissionID))
	if err != nil {
		return cfg, err
	}
	var saved submitCommandConfig
	if err := json.Unmarshal(body, &saved); err != nil {
		return cfg, err
	}
	if saved.DBPath != cfg.DBPath || saved.SubmissionID != cfg.SubmissionID || saved.ProjectID != cfg.ProjectID ||
		saved.RequireComposition != cfg.RequireComposition || !sameSubmissionInputs(saved.SavedInputs, cfg.SavedInputs) {
		return cfg, fmt.Errorf("submit: saved submission inputs changed: %w", store.ErrImmutableConflict)
	}
	return saved, nil
}

func sameSubmissionInputs(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for role, original := range a {
		incoming, ok := b[role]
		if !ok {
			return false
		}
		if role == "task" {
			if string(original) != string(incoming) {
				return false
			}
			continue
		}
		x, xErr := decodedSubmissionInput(role, original)
		y, yErr := decodedSubmissionInput(role, incoming)
		if xErr != nil || yErr != nil || !reflect.DeepEqual(x, y) {
			return false
		}
	}
	return true
}

// Compare the same decoded contract types acceptance uses. Formatting and key
// order carry no identity, while typed integers retain their full precision.
func decodedSubmissionInput(role string, body []byte) (any, error) {
	var value any
	utf8Policy := strictjson.TolerateInvalidUTF8
	switch role {
	case "policy":
		value = new([]domain.PolicyKey)
	case "publication":
		value = new(engine.ProductionPublication)
	case "work-unit":
		value = new(submittedWorkUnit)
	case "composition":
		value = new(compositionManifest)
		utf8Policy = strictjson.RejectInvalidUTF8
	default:
		return nil, errors.New("unknown submission input")
	}
	if err := ward.RejectDuplicateJSONKeys(body); err != nil {
		return nil, err
	}
	err := strictjson.Decode(body, value, utf8Policy, maxSubmissionFileBytes)
	return value, err
}

func validSubmissionID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func submissionJournalPath(db, id string) string {
	return filepath.Join(db+".submissions", id+".json")
}

func (cfg submitCommandConfig) input(role, path string) (submissionFile, error) {
	if cfg.SavedInputs != nil {
		body, ok := cfg.SavedInputs[role]
		if !ok || len(body) == 0 || len(body) > maxSubmissionFileBytes {
			return submissionFile{}, fmt.Errorf("submit: saved %s input is missing or invalid", role)
		}
		return submissionBytes(body), nil
	}
	return readSubmissionFile(path)
}
