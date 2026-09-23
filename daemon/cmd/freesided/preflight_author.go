package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
)

func compositionChecksPassed(manifest *compositionManifest, names ...string) bool {
	for _, name := range names {
		found := false
		for _, check := range manifest.Checks {
			if check.Name == name {
				found = check.Status == compositionPassed
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// checkPreflightAuthorInputs reads only objects named by the admitted base
// commit. It never consults the checkout's working tree or writes to its Git
// metadata, so candidate edits cannot alter repository or template sources.
// The admitted host snapshot remains a separate explicit input.
func checkPreflightAuthorInputs(ctx context.Context, cfg preflightConfig, host engine.ReviewHostInstructions) error {
	tree, err := preflightBaseTree(ctx, cfg.RepositoryCheckout, cfg.BaseSHA)
	if err != nil {
		return errors.New("trusted-base author sources could not be enumerated")
	}
	paths := make([]string, 0, len(tree))
	for path := range tree {
		paths = append(paths, path)
	}
	selected := engine.SelectCodexReviewInstructionPaths(paths)
	sources := make([]exec.ReviewInstructionSourceInput, 0, len(selected))
	for _, path := range selected {
		if mode := tree[path]; mode != "100644" && mode != "100755" {
			return errors.New("selected exact-base instruction source is not a regular file")
		}
		body, err := preflightBaseBlob(ctx, cfg.RepositoryCheckout, cfg.BaseSHA, path, tree[path])
		if err != nil {
			return fmt.Errorf("trusted-base instruction source: %w", err)
		}
		sources = append(sources, exec.ReviewInstructionSourceInput{Path: path, Body: body})
	}
	bundle, _, err := exec.ComposeCodexReviewInstructions(
		exec.ReviewHostInstructionInput{Present: host.Present, Body: host.Body}, sources,
	)
	if err != nil {
		return errors.New("trusted-base review instruction composition was refused")
	}
	validate := func(field string, content []byte) error {
		return inference.ValidateAuthorControlFile(field, inference.ControlFile{
			Content: string(content), Digest: contentaddr.Sum(content), TrustedBaseCommit: cfg.BaseSHA,
		})
	}
	if err := validate("instruction_snapshot", bundle); err != nil {
		return err
	}
	var template []byte
	for _, path := range engine.PublicationPRTemplatePaths() {
		mode, exists := tree[path]
		if !exists {
			continue
		}
		body, err := preflightBaseBlob(ctx, cfg.RepositoryCheckout, cfg.BaseSHA, path, mode)
		if err != nil {
			return fmt.Errorf("trusted-base PR template: %w", err)
		}
		if utf8.Valid(body) {
			template = body
			break
		}
	}
	return validate("pr_template", template)
}

func preflightBaseTree(ctx context.Context, checkout, baseSHA string) (map[string]string, error) {
	// Match the publication materializer's tree-listing budget so preflight
	// does not reject supported repositories before selecting author sources.
	data, err := runPreflightAuthorGit(ctx, checkout, 64<<20, "ls-tree", "-rz", "--full-tree", baseSHA)
	if err != nil {
		return nil, err
	}
	files := make(map[string]string)
	for _, record := range bytes.Split(data, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		meta, path, ok := bytes.Cut(record, []byte{'\t'})
		if !ok {
			return nil, errors.New("invalid base tree entry")
		}
		mode, objectType, ok := strings.Cut(string(meta), " ")
		if !ok || !strings.HasPrefix(objectType, "blob ") {
			continue
		}
		files[string(path)] = mode
	}
	return files, nil
}

func preflightBaseBlob(ctx context.Context, checkout, baseSHA, path, mode string) ([]byte, error) {
	if mode != "100644" && mode != "100755" && mode != "120000" {
		return nil, errors.New("selected exact-base source is not a blob file")
	}
	object := baseSHA + ":" + path
	sizeText, err := runPreflightAuthorGit(ctx, checkout, 64<<10, "cat-file", "-s", object)
	if err != nil {
		return nil, errors.New("exact-base source size is unavailable")
	}
	size, err := strconv.ParseInt(strings.TrimSpace(string(sizeText)), 10, 64)
	if err != nil || size < 0 {
		return nil, errors.New("exact-base source has invalid size")
	}
	if size > domain.MaxVendorInstructionBytes {
		return nil, fmt.Errorf("exact-base source is %d bytes, observation limit %d", size, domain.MaxVendorInstructionBytes)
	}
	body, err := runPreflightAuthorGit(ctx, checkout, int(size)+1, "cat-file", "blob", object)
	if err != nil || int64(len(body)) != size {
		return nil, errors.New("exact-base source could not be read consistently")
	}
	return body, nil
}
