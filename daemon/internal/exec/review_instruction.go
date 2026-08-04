package exec

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const ReviewInstructionCompositionVersion = "codex_explicit_bundle_v1"

// ReviewInstructionSource binds one path-scoped repository instruction file
// from the reviewed repository's exact trusted base.
type ReviewInstructionSource struct {
	Path   string        `json:"path"`
	Digest domain.Digest `json:"digest"`
}

// ReviewInstructionBinding is the content-addressed authority for the exact
// instruction bundle used by one review pass. A nil HostDigest is explicit
// admitted absence, not an omitted reconstruction input.
type ReviewInstructionBinding struct {
	CompositionVersion string                    `json:"composition_version"`
	HostDigest         *domain.Digest            `json:"host_digest"`
	RepositorySources  []ReviewInstructionSource `json:"repository_sources"`
	ResultDigest       domain.Digest             `json:"result_digest"`
}

// Validate requires the one canonical source order used by composition and
// persistence. That makes reordering an authority change rather than an
// alternative encoding of the same request.
func (b ReviewInstructionBinding) Validate() error {
	if b.CompositionVersion != ReviewInstructionCompositionVersion ||
		!contentaddr.Valid(string(b.ResultDigest)) {
		return fmt.Errorf("review instruction composition: %w", domain.ErrInvalidReviewCompletionEvidence)
	}
	if b.HostDigest != nil && !contentaddr.Valid(string(*b.HostDigest)) {
		return fmt.Errorf("review instruction host digest %q: %w",
			*b.HostDigest, domain.ErrInvalidReviewCompletionEvidence)
	}
	for i, source := range b.RepositorySources {
		if !fs.ValidPath(source.Path) || source.Path == "." ||
			!contentaddr.Valid(string(source.Digest)) {
			return fmt.Errorf("review instruction repository_sources[%d]: %w",
				i, domain.ErrInvalidReviewCompletionEvidence)
		}
		if i > 0 && source.Path <= b.RepositorySources[i-1].Path {
			return fmt.Errorf("review instruction repository source order: %w",
				domain.ErrDigestsNotCanonical)
		}
	}
	return nil
}

// ArtifactDigests returns every source and result blob the request needs for
// deterministic reconstruction after backup restore.
func (b ReviewInstructionBinding) ArtifactDigests() ([]domain.Digest, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	digests := make([]domain.Digest, 0, len(b.RepositorySources)+2)
	if b.HostDigest != nil {
		digests = append(digests, *b.HostDigest)
	}
	for _, source := range b.RepositorySources {
		digests = append(digests, source.Digest)
	}
	digests = append(digests, b.ResultDigest)
	slices.Sort(digests)
	return slices.Compact(digests), nil
}

// ReviewInstructionSourceInput carries trusted bytes only while composing or
// reconstructing a bundle. Durable authority is the digest-only binding.
type ReviewInstructionSourceInput struct {
	Path string
	Body []byte
}

// ReviewHostInstructionInput preserves the distinction between a present
// empty regular file and explicit admitted absence.
type ReviewHostInstructionInput struct {
	Present bool
	Body    []byte
}

// ComposeCodexReviewInstructions deterministically combines explicit host
// absence or bytes with path-scoped exact-base repository instructions. The
// host block is final and globally authoritative; deeper repository scopes
// override shallower repository scopes before that final operator boundary.
func ComposeCodexReviewInstructions(
	host ReviewHostInstructionInput,
	repository []ReviewInstructionSourceInput,
) ([]byte, ReviewInstructionBinding, error) {
	if !host.Present && len(host.Body) != 0 {
		return nil, ReviewInstructionBinding{}, errors.New("absent review host instructions carry content")
	}
	sources := make([]ReviewInstructionSourceInput, len(repository))
	for i, source := range repository {
		sources[i] = ReviewInstructionSourceInput{
			Path: source.Path,
			Body: bytes.Clone(source.Body),
		}
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Path < sources[j].Path })

	binding := ReviewInstructionBinding{
		CompositionVersion: ReviewInstructionCompositionVersion,
		RepositorySources:  make([]ReviewInstructionSource, len(sources)),
	}
	var bundle bytes.Buffer
	bundle.WriteString("# Freeside Explicit Codex Review Instruction Bundle\n\n")
	bundle.WriteString("Composition: " + ReviewInstructionCompositionVersion + "\n\n")
	bundle.WriteString("Apply each digest-delimited repository block only within its named path scope. " +
		"The deepest matching repository scope takes precedence among repository blocks. " +
		"Apply the final operator-host block globally; it takes precedence over every repository block.\n\n" +
		"## Trusted-Base Repository Instructions\n")
	for i, source := range sources {
		if !fs.ValidPath(source.Path) || source.Path == "." ||
			(i > 0 && source.Path == sources[i-1].Path) {
			return nil, ReviewInstructionBinding{}, fmt.Errorf(
				"review instruction source path %q is not canonical", source.Path)
		}
		digest := digestReviewInstruction(source.Body)
		binding.RepositorySources[i] = ReviewInstructionSource{Path: source.Path, Digest: digest}
		scope := "."
		if slash := strings.LastIndexByte(source.Path, '/'); slash >= 0 {
			scope = source.Path[:slash]
		}
		bundle.WriteString("\n### Scope ")
		bundle.WriteString(strconv.Quote(scope))
		bundle.WriteString("\n\n--- BEGIN REPOSITORY INSTRUCTION ")
		bundle.WriteString(string(digest))
		bundle.WriteString(" ---\n")
		bundle.Write(source.Body)
		if len(source.Body) == 0 || source.Body[len(source.Body)-1] != '\n' {
			bundle.WriteByte('\n')
		}
		bundle.WriteString("--- END REPOSITORY INSTRUCTION ")
		bundle.WriteString(string(digest))
		bundle.WriteString(" ---\n")
	}
	bundle.WriteString("\n## Operator-Host Instructions\n\n")
	if !host.Present {
		bundle.WriteString("(No operator-host instruction file was admitted.)\n")
	} else {
		digest := digestReviewInstruction(host.Body)
		binding.HostDigest = &digest
		bundle.WriteString("--- BEGIN OPERATOR-HOST INSTRUCTION ")
		bundle.WriteString(string(digest))
		bundle.WriteString(" ---\n")
		bundle.Write(host.Body)
		if len(host.Body) == 0 || host.Body[len(host.Body)-1] != '\n' {
			bundle.WriteByte('\n')
		}
		bundle.WriteString("--- END OPERATOR-HOST INSTRUCTION ")
		bundle.WriteString(string(digest))
		bundle.WriteString(" ---\n")
	}
	if int64(bundle.Len()) > domain.MaxVendorInstructionBytes {
		return nil, ReviewInstructionBinding{}, fmt.Errorf(
			"review instruction bundle is %d bytes, limit %d",
			bundle.Len(), domain.MaxVendorInstructionBytes)
	}
	body := bytes.Clone(bundle.Bytes())
	binding.ResultDigest = digestReviewInstruction(body)
	if err := binding.Validate(); err != nil {
		return nil, ReviewInstructionBinding{}, err
	}
	return body, binding, nil
}

func digestReviewInstruction(body []byte) domain.Digest {
	return domain.Digest(fmt.Sprintf("sha256:%x", sha256.Sum256(body)))
}
