package exec

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// InputSource is the read seam over the daemon's content-addressed artifact
// store. Open resolves by digest only; paths and current workspace state never
// enter the materialization contract.
type InputSource interface {
	OpenContext(context.Context, domain.Digest) (io.ReadCloser, error)
}

// MaterializerOptions bounds one input and the complete admitted bundle.
// Callers must choose limits deliberately for their execution class.
type MaterializerOptions struct {
	MaxInputBytes int64
	MaxTotalBytes int64
}

// Materializer resolves and verifies an admitted input snapshot before a real
// driver starts its process.
type Materializer struct {
	source InputSource
	opts   MaterializerOptions
}

// StageInputLoader resolves and verifies the immutable inputs for one start.
type StageInputLoader func(context.Context) (StageInputs, error)

// MaterializedStageDriver is the process-facing half of a real driver.
// StartWithInputs owns duplicate arbitration and must serialize concurrent
// calls for the same id. It returns ErrDuplicateStart without calling load
// when an intent is already committed. When no intent exists, it calls load
// exactly once for the winning caller and commits no intent if load fails.
// A contender waiting on a failed load may then become the winner; a contender
// waiting on a committed start returns ErrDuplicateStart without calling load.
type MaterializedStageDriver interface {
	StartWithInputs(
		context.Context, domain.InvocationID, StartSpec, StageInputLoader,
	) error
	Inspect(context.Context, domain.InvocationID) (Inspection, error)
	Stream(context.Context, domain.InvocationID) (io.ReadCloser, error)
	Cancel(context.Context, domain.InvocationID) error
	Collect(context.Context, domain.InvocationID) (StageResult, error)
}

// MaterializingStageDriver adapts a process-facing driver to StageDriver. It
// is the one production start seam: digest verification completes before the
// process-facing driver can commit an invocation intent.
type MaterializingStageDriver struct {
	materializer *Materializer
	driver       MaterializedStageDriver
}

var _ StageDriver = (*MaterializingStageDriver)(nil)

// NewMaterializer constructs a production materializer. Zero or negative
// limits are rejected rather than interpreted as unlimited.
func NewMaterializer(source InputSource, opts MaterializerOptions) (*Materializer, error) {
	switch {
	case source == nil:
		return nil, ErrInputSourceMissing
	case opts.MaxInputBytes <= 0:
		return nil, fmt.Errorf("materializer max input bytes %d: %w",
			opts.MaxInputBytes, ErrInputLimitInvalid)
	case opts.MaxTotalBytes <= 0:
		return nil, fmt.Errorf("materializer max total bytes %d: %w",
			opts.MaxTotalBytes, ErrInputLimitInvalid)
	}
	return &Materializer{source: source, opts: opts}, nil
}

// NewMaterializingStageDriver builds the production StageDriver adapter.
func NewMaterializingStageDriver(
	materializer *Materializer, driver MaterializedStageDriver,
) (*MaterializingStageDriver, error) {
	if materializer == nil {
		return nil, ErrMaterializerMissing
	}
	if driver == nil {
		return nil, ErrMaterializedDriverMissing
	}
	return &MaterializingStageDriver{materializer: materializer, driver: driver}, nil
}

func (d *MaterializingStageDriver) Start(
	ctx context.Context, id domain.InvocationID, spec StartSpec,
) error {
	err := d.driver.StartWithInputs(ctx, id, spec, func(ctx context.Context) (StageInputs, error) {
		inputs, err := d.materializer.Materialize(ctx, spec)
		if err != nil {
			return StageInputs{}, err
		}
		if err := ctx.Err(); err != nil {
			return StageInputs{}, fmt.Errorf("materialized inputs canceled: %w",
				errors.Join(ErrInputUnavailable, err))
		}
		return inputs, nil
	})
	if err != nil {
		return fmt.Errorf("start invocation %s: %w", id, err)
	}
	return nil
}

func (d *MaterializingStageDriver) Inspect(
	ctx context.Context, id domain.InvocationID,
) (Inspection, error) {
	return d.driver.Inspect(ctx, id)
}

func (d *MaterializingStageDriver) Stream(
	ctx context.Context, id domain.InvocationID,
) (io.ReadCloser, error) {
	return d.driver.Stream(ctx, id)
}

func (d *MaterializingStageDriver) Cancel(
	ctx context.Context, id domain.InvocationID,
) error {
	return d.driver.Cancel(ctx, id)
}

func (d *MaterializingStageDriver) Collect(
	ctx context.Context, id domain.InvocationID,
) (StageResult, error) {
	return d.driver.Collect(ctx, id)
}

// MaterializedContent is one verified immutable input. Its bytes stay private;
// Bytes returns a detached copy so a driver cannot rewrite the bundle another
// consumer or replay observes.
type MaterializedContent struct {
	digest domain.Digest
	body   []byte
}

// Digest returns the admitted content address verified for this body.
func (c MaterializedContent) Digest() domain.Digest { return c.digest }

// Bytes returns a detached copy of the verified content.
func (c MaterializedContent) Bytes() []byte { return slices.Clone(c.body) }

// Reader returns a new reader over the verified immutable content.
func (c MaterializedContent) Reader() io.Reader { return bytes.NewReader(c.body) }

// StageInputs is the fully materialized input bundle a real driver consumes.
// Accessors return values or detached slices; no mutable content is exposed.
type StageInputs struct {
	digest             domain.Digest
	specification      MaterializedContent
	promptPackage      MaterializedContent
	policy             MaterializedContent
	vendorInstructions *MaterializedVendorInstructions
	conversation       *MaterializedContent
	priorArtifacts     []MaterializedContent
	imageInputs        []MaterializedContent
}

// MaterializedVendorInstructions is the verified vendor-native instruction
// input for one admitted execution. A nil content value is the explicit
// snapshot of a missing host file, not a lookup to perform later.
type MaterializedVendorInstructions struct {
	vendor  domain.AgentVendor
	content *MaterializedContent
}

// Vendor identifies the native instruction mechanism this input targets.
func (i MaterializedVendorInstructions) Vendor() domain.AgentVendor { return i.vendor }

// Content returns the verified instruction bytes, or false for admitted
// absence.
func (i MaterializedVendorInstructions) Content() (MaterializedContent, bool) {
	if i.content == nil {
		return MaterializedContent{}, false
	}
	return *i.content, true
}

// Digest returns the admitted identity of the complete role-to-content map.
func (s StageInputs) Digest() domain.Digest { return s.digest }

// Specification returns the verified approved specification.
func (s StageInputs) Specification() MaterializedContent { return s.specification }

// PromptPackage returns the verified control-plane prompt package.
func (s StageInputs) PromptPackage() MaterializedContent { return s.promptPackage }

// Policy returns the verified resolved policy snapshot.
func (s StageInputs) Policy() MaterializedContent { return s.policy }

// VendorInstructions returns the admitted vendor-instruction role. False
// means a historical pre-v2 snapshot that carried no such role; an admitted
// missing host file returns a value whose Content reports false.
func (s StageInputs) VendorInstructions() (MaterializedVendorInstructions, bool) {
	if s.vendorInstructions == nil {
		return MaterializedVendorInstructions{}, false
	}
	return *s.vendorInstructions, true
}

// ConversationPrefix returns the verified immutable conversation prefix when
// the invocation was conversation-bound.
func (s StageInputs) ConversationPrefix() (MaterializedContent, bool) {
	if s.conversation == nil {
		return MaterializedContent{}, false
	}
	return *s.conversation, true
}

// PriorArtifacts returns the verified prior artifacts in admitted order.
func (s StageInputs) PriorArtifacts() []MaterializedContent {
	return slices.Clone(s.priorArtifacts)
}

// ImageInputs returns the verified image inputs in admitted order.
func (s StageInputs) ImageInputs() []MaterializedContent {
	return slices.Clone(s.imageInputs)
}

// Materialize resolves exactly the snapshot admitted in spec. Every body is
// hashed while read and compared with its frozen digest. Missing content,
// corruption, cancellation, or size overflow returns before any StageInputs
// value is exposed.
func (m *Materializer) Materialize(ctx context.Context, spec StartSpec) (StageInputs, error) {
	if spec.StageInputs == nil {
		return StageInputs{}, ErrStageInputsMissing
	}
	snapshot := *spec.StageInputs
	if spec.StageInputs.VendorInstructions != nil {
		vendor := *spec.StageInputs.VendorInstructions
		if spec.StageInputs.VendorInstructions.Digest != nil {
			digest := *spec.StageInputs.VendorInstructions.Digest
			vendor.Digest = &digest
		}
		snapshot.VendorInstructions = &vendor
	}
	if spec.StageInputs.ConversationDigest != nil {
		digest := *spec.StageInputs.ConversationDigest
		snapshot.ConversationDigest = &digest
	}
	snapshot.PriorArtifactDigests = slices.Clone(spec.StageInputs.PriorArtifactDigests)
	snapshot.ImageInputDigests = slices.Clone(spec.StageInputs.ImageInputDigests)
	if err := snapshot.Validate(); err != nil {
		return StageInputs{}, fmt.Errorf("materialize stage inputs: %w", err)
	}
	if snapshot.InputDigest != spec.InputDigest ||
		snapshot.SpecificationDigest != spec.SpecDigest ||
		snapshot.PolicyDigest != spec.PolicyDigest {
		return StageInputs{}, fmt.Errorf("materialize stage inputs: start spec disagrees with snapshot: %w",
			domain.ErrParentKeyMismatch)
	}

	remaining := m.opts.MaxTotalBytes
	load := func(role string, digest domain.Digest) (MaterializedContent, error) {
		content, err := m.load(ctx, role, digest, remaining)
		if err != nil {
			return MaterializedContent{}, err
		}
		remaining -= int64(len(content.body))
		return content, nil
	}

	specification, err := load("specification", snapshot.SpecificationDigest)
	if err != nil {
		return StageInputs{}, err
	}
	prompt, err := load("prompt package", snapshot.PromptPackageDigest)
	if err != nil {
		return StageInputs{}, err
	}
	policy, err := load("policy", snapshot.PolicyDigest)
	if err != nil {
		return StageInputs{}, err
	}
	var vendorInstructions *MaterializedVendorInstructions
	if snapshot.VendorInstructions != nil {
		vendorInstructions = &MaterializedVendorInstructions{
			vendor: snapshot.VendorInstructions.Vendor,
		}
		if snapshot.VendorInstructions.Digest != nil {
			content, err := load(
				"vendor instructions", *snapshot.VendorInstructions.Digest,
			)
			if err != nil {
				return StageInputs{}, err
			}
			vendorInstructions.content = &content
		}
	}
	var conversation *MaterializedContent
	if snapshot.ConversationDigest != nil {
		content, err := load("conversation prefix", *snapshot.ConversationDigest)
		if err != nil {
			return StageInputs{}, err
		}
		conversation = &content
	}
	prior, err := materializeMany(load, "prior artifact", snapshot.PriorArtifactDigests)
	if err != nil {
		return StageInputs{}, err
	}
	images, err := materializeMany(load, "image input", snapshot.ImageInputDigests)
	if err != nil {
		return StageInputs{}, err
	}
	return StageInputs{
		digest: snapshot.ID, specification: specification, promptPackage: prompt,
		policy: policy, vendorInstructions: vendorInstructions, conversation: conversation,
		priorArtifacts: prior, imageInputs: images,
	}, nil
}

func materializeMany(
	load func(string, domain.Digest) (MaterializedContent, error),
	role string,
	digests []domain.Digest,
) ([]MaterializedContent, error) {
	out := make([]MaterializedContent, 0, len(digests))
	for i, digest := range digests {
		content, err := load(fmt.Sprintf("%s[%d]", role, i), digest)
		if err != nil {
			return nil, err
		}
		out = append(out, content)
	}
	return out, nil
}

func (m *Materializer) load(
	ctx context.Context, role string, digest domain.Digest, totalRemaining int64,
) (MaterializedContent, error) {
	if !contentaddr.Valid(string(digest)) {
		return MaterializedContent{}, fmt.Errorf("materialize %s digest %q: %w",
			role, digest, ErrInputDigestInvalid)
	}
	body, err := m.source.OpenContext(ctx, digest)
	if err != nil {
		return MaterializedContent{}, fmt.Errorf("materialize %s %s: %w", role, digest, err)
	}
	if body == nil {
		return MaterializedContent{}, fmt.Errorf("materialize %s %s: %w",
			role, digest, ErrInputBodyMissing)
	}
	if err := ctx.Err(); err != nil {
		closeErr := body.Close()
		return MaterializedContent{}, fmt.Errorf("materialize %s %s: %w",
			role, digest, errors.Join(ErrInputUnavailable, err, closeErr))
	}
	limit := min(m.opts.MaxInputBytes, totalRemaining)
	content, readErr := io.ReadAll(io.LimitReader(contextReader{ctx: ctx, reader: body}, limit+1))
	closeErr := body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return MaterializedContent{}, fmt.Errorf("materialize %s %s: %w",
			role, digest, errors.Join(ErrInputUnavailable, err))
	}
	if int64(len(content)) > limit {
		return MaterializedContent{}, fmt.Errorf("materialize %s %s exceeds byte limit: %w",
			role, digest, ErrInputTooLarge)
	}
	got := domain.Digest(fmt.Sprintf("sha256:%x", sha256.Sum256(content)))
	if got != digest {
		return MaterializedContent{}, fmt.Errorf("materialize %s: body hashes to %s, want %s: %w",
			role, got, digest, ErrInputDigestMismatch)
	}
	return MaterializedContent{digest: digest, body: content}, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		return n, errors.Join(err, ctxErr)
	}
	return n, err
}
