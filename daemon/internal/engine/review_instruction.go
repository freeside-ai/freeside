package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

// ReviewHostInstructions is the exact operator-host source admitted for new
// Codex review requests. Present=false is explicit source absence.
type ReviewHostInstructions struct {
	Present bool
	Digest  domain.Digest
	Body    []byte
}

func (h ReviewHostInstructions) validate() error {
	if !h.Present {
		if h.Digest != "" || len(h.Body) != 0 {
			return errors.New("absent review host instructions carry content")
		}
		return nil
	}
	if !contentaddr.Valid(string(h.Digest)) || reviewInstructionDigest(h.Body) != h.Digest {
		return errors.New("review host instructions do not match their digest")
	}
	return nil
}

// SnapshotReviewHostInstructions applies the same stable regular-file read
// and explicit-absence contract used by execution admission.
func SnapshotReviewHostInstructions(
	ctx context.Context, path string, forbiddenHostPaths ...string,
) (ReviewHostInstructions, error) {
	snapshot, body, err := snapshotVendorInstructions(ctx, VendorInstructionConfig{
		Vendor:             domain.AgentVendorCodex,
		Delivery:           domain.VendorInstructionDeliveryAppendFile,
		HostPath:           path,
		ForbiddenHostPaths: forbiddenHostPaths,
	})
	if err != nil {
		return ReviewHostInstructions{}, err
	}
	host := ReviewHostInstructions{Present: snapshot.Digest != nil, Body: bytes.Clone(body)}
	if snapshot.Digest != nil {
		host.Digest = *snapshot.Digest
	}
	return host, host.validate()
}

func (w *productionPublicationWorkflow) composeReviewInstructions(
	exactBase string,
) (exec.ReviewInstructionBinding, error) {
	if err := w.reviewHostInstructions.validate(); err != nil {
		return exec.ReviewInstructionBinding{}, err
	}
	sources, err := discoverCodexReviewInstructions(exactBase)
	if err != nil {
		return exec.ReviewInstructionBinding{}, err
	}
	host := exec.ReviewHostInstructionInput{Present: w.reviewHostInstructions.Present}
	if w.reviewHostInstructions.Present {
		host.Body = bytes.Clone(w.reviewHostInstructions.Body)
		if _, err := w.artifacts.Put(w.reviewHostInstructions.Digest, bytes.NewReader(host.Body)); err != nil {
			return exec.ReviewInstructionBinding{}, fmt.Errorf("store review host instructions: %w", err)
		}
	}
	for _, source := range sources {
		digest := reviewInstructionDigest(source.Body)
		if _, err := w.artifacts.Put(digest, bytes.NewReader(source.Body)); err != nil {
			return exec.ReviewInstructionBinding{}, fmt.Errorf(
				"store review repository instructions %q: %w", source.Path, err)
		}
	}
	bundle, binding, err := exec.ComposeCodexReviewInstructions(host, sources)
	if err != nil {
		return exec.ReviewInstructionBinding{}, err
	}
	if w.reviewHostInstructions.Present &&
		(binding.HostDigest == nil || *binding.HostDigest != w.reviewHostInstructions.Digest) {
		return exec.ReviewInstructionBinding{}, errors.New("composed review host binding diverged from admission")
	}
	if _, err := w.artifacts.Put(binding.ResultDigest, bytes.NewReader(bundle)); err != nil {
		return exec.ReviewInstructionBinding{}, fmt.Errorf("store composed review instructions: %w", err)
	}
	return binding, nil
}

type reviewInstructionCandidates struct {
	override string
	agents   string
}

func discoverCodexReviewInstructions(root string) ([]exec.ReviewInstructionSourceInput, error) {
	candidates := make(map[string]reviewInstructionCandidates)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() != "AGENTS.override.md" && entry.Name() != "AGENTS.md" {
			return nil
		}
		dir := filepath.Dir(path)
		found := candidates[dir]
		if entry.Name() == "AGENTS.override.md" {
			found.override = path
		} else {
			found.agents = path
		}
		candidates[dir] = found
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("discover exact-base Codex review instructions: %w", err)
	}
	dirs := make([]string, 0, len(candidates))
	for dir := range candidates {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	sources := make([]exec.ReviewInstructionSourceInput, 0, len(dirs))
	remaining := int64(domain.MaxVendorInstructionBytes)
	for _, dir := range dirs {
		for _, path := range []string{candidates[dir].override, candidates[dir].agents} {
			if path == "" {
				continue
			}
			body, err := readExactBaseReviewInstruction(path, remaining)
			if err != nil {
				return nil, err
			}
			remaining -= int64(len(body))
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return nil, err
			}
			sources = append(sources, exec.ReviewInstructionSourceInput{
				Path: filepath.ToSlash(rel), Body: body,
			})
			break
		}
	}
	return sources, nil
}

func readExactBaseReviewInstruction(path string, remaining int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect exact-base review instruction: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("exact-base review instruction is not a regular file")
	}
	file, err := os.Open(path) //nolint:gosec // daemon-fetched exact-base checkout
	if err != nil {
		return nil, fmt.Errorf("open exact-base review instruction: %w", err)
	}
	body, readErr := io.ReadAll(io.LimitReader(file, remaining+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	if int64(len(body)) > remaining {
		return nil, fmt.Errorf("exact-base review instructions exceed %d bytes",
			domain.MaxVendorInstructionBytes)
	}
	return body, nil
}

func reviewInstructionDigest(body []byte) domain.Digest {
	return domain.Digest(fmt.Sprintf("sha256:%x", sha256.Sum256(body)))
}
