package domain

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

var (
	// ErrImageUpkeepRefusalInvalid marks a malformed upkeep refusal record.
	ErrImageUpkeepRefusalInvalid = errors.New("image upkeep refusal is invalid")
	// ErrImageUpkeepRefusalInconsistent marks a record whose ID is not the
	// content address of its facts.
	ErrImageUpkeepRefusalInconsistent = errors.New("image upkeep refusal id does not match its content")
	// ErrInvalidImageUpkeepRefusalClass marks an unknown refusal class.
	ErrInvalidImageUpkeepRefusalClass = errors.New("invalid image upkeep refusal class")
	// ErrRepositoryTipObservationInvalid marks a malformed tip observation.
	ErrRepositoryTipObservationInvalid = errors.New("repository tip observation is invalid")
)

// imageUpkeepRefusalEncodingVersion addresses an upkeep refusal record. It
// must never change once a record is stored: every stored ID validates
// against this preimage.
const imageUpkeepRefusalEncodingVersion = "freeside.image-upkeep-refusal/v1"

// MaxImageUpkeepDetailBytes bounds one clause finding's detail. It leaves
// room for the engine's quoted 1000-byte excerpt of build output
// (boundedRebuildText), whose escaping can quadruple the length.
const MaxImageUpkeepDetailBytes = 4096

// maxRepositoryBranchBytes bounds an observed default-branch name, which the
// forge reports and the store keeps beside the observation.
const maxRepositoryBranchBytes = 255

// ImageUpkeepRefusalClass sorts an upkeep refusal by what may answer it
// (plan §5.7, Project-image upkeep). Which gate clauses yield which class is
// the upkeep job's rule; the record only names the class. The zero value ""
// is invalid by design.
type ImageUpkeepRefusalClass string

const (
	// ImageUpkeepRefusalPolicy: only trusted-policy clauses refused (the
	// recipe digest, an authority outside the registry set, or no registry
	// set), so an adoption card may answer it.
	ImageUpkeepRefusalPolicy ImageUpkeepRefusalClass = "policy"
	// ImageUpkeepRefusalRepair: the fix is a repository change, so a
	// daemon-origin maintenance task may answer it.
	ImageUpkeepRefusalRepair ImageUpkeepRefusalClass = "repair"
	// ImageUpkeepRefusalHostFault: the builder, runtime, or rebuild
	// configuration failed for a reason outside the project. It never opens
	// a task, and the record may name no clause.
	ImageUpkeepRefusalHostFault ImageUpkeepRefusalClass = "host_fault"
	// ImageUpkeepRefusalGoModuleDelta: a Go input no image supports, or a
	// go.mod or go.sum the gate cannot read. Records written before Go module
	// changes were bounded also hold it for any go.mod or go.sum change. It
	// opens neither a proposal nor a task.
	ImageUpkeepRefusalGoModuleDelta ImageUpkeepRefusalClass = "go_module_delta"
	// ImageUpkeepRefusalUnsupportedNPMInput: an npm input no project image
	// supports, usually a deliberate change a repair could only revert. It
	// opens neither a proposal nor a task.
	ImageUpkeepRefusalUnsupportedNPMInput ImageUpkeepRefusalClass = "unsupported_npm_input"
)

// AllImageUpkeepRefusalClasses lists every valid ImageUpkeepRefusalClass.
var AllImageUpkeepRefusalClasses = []ImageUpkeepRefusalClass{
	ImageUpkeepRefusalPolicy, ImageUpkeepRefusalRepair, ImageUpkeepRefusalHostFault,
	ImageUpkeepRefusalGoModuleDelta, ImageUpkeepRefusalUnsupportedNPMInput,
}

func (c ImageUpkeepRefusalClass) valid() bool {
	switch c {
	case ImageUpkeepRefusalPolicy, ImageUpkeepRefusalRepair, ImageUpkeepRefusalHostFault,
		ImageUpkeepRefusalGoModuleDelta, ImageUpkeepRefusalUnsupportedNPMInput:
		return true
	default:
		return false
	}
}

// ImageUpkeepClauseFinding is one gate clause that refused, with its bounded
// detail.
type ImageUpkeepClauseFinding struct {
	Clause RebuildRefusal `json:"clause"`
	Detail string         `json:"detail"`
}

func (f ImageUpkeepClauseFinding) validate() error {
	if !f.Clause.valid() {
		return fmt.Errorf("image upkeep finding clause %q: %w", f.Clause, ErrImageUpkeepRefusalInvalid)
	}
	// JSON rewrites invalid UTF-8, so a detail that is not valid UTF-8 would
	// decode to other bytes than its ID addresses.
	if !utf8.ValidString(f.Detail) {
		return fmt.Errorf("image upkeep finding detail is not UTF-8: %w", ErrImageUpkeepRefusalInvalid)
	}
	if len(f.Detail) > MaxImageUpkeepDetailBytes {
		return fmt.Errorf("image upkeep finding detail is %d bytes, limit %d: %w",
			len(f.Detail), MaxImageUpkeepDetailBytes, ErrImageUpkeepRefusalInvalid)
	}
	return nil
}

// compareImageUpkeepFindings orders findings by clause, then detail: the
// canonical order a refusal record keeps them in.
func compareImageUpkeepFindings(a, b ImageUpkeepClauseFinding) int {
	return cmp.Or(cmp.Compare(a.Clause, b.Clause), cmp.Compare(a.Detail, b.Detail))
}

// ImageUpkeepRefusal is the immutable record of one upkeep run the gate
// refused at one default-branch tip (plan §5.7, Project-image upkeep). Its ID
// is the content address of every fact but RecordedAt, so the same refusal
// at the same tip, measured from the same image, converges on one record.
// Findings are kept in canonical order without exact duplicates, so the order
// the gate reported them in does not change the ID. The
// upkeep hold, the adoption card, and the maintenance task each reference it
// by ID.
type ImageUpkeepRefusal struct {
	ID                  Digest                     `json:"id"`
	RepositoryID        int64                      `json:"repository_id"`
	Repository          string                     `json:"repository"`
	Harness             HarnessClientKind          `json:"harness"`
	TipSHA              string                     `json:"tip_sha"`
	MeasuredFromImageID Digest                     `json:"measured_from_image_id"`
	Class               ImageUpkeepRefusalClass    `json:"class"`
	Findings            []ImageUpkeepClauseFinding `json:"findings"`
	RecordedAt          time.Time                  `json:"recorded_at"`
}

// ImageUpkeepRefusalInput carries the facts NewImageUpkeepRefusal addresses.
type ImageUpkeepRefusalInput struct {
	RepositoryID        int64
	Repository          string
	Harness             HarnessClientKind
	TipSHA              string
	MeasuredFromImageID Digest
	Class               ImageUpkeepRefusalClass
	Findings            []ImageUpkeepClauseFinding
	RecordedAt          time.Time
}

// canonicalImageUpkeepRefusal is the preimage the ID addresses. Its field
// order and names are part of the v1 encoding.
type canonicalImageUpkeepRefusal struct {
	Version             string                     `json:"version"`
	RepositoryID        int64                      `json:"repository_id"`
	Repository          string                     `json:"repository"`
	Harness             HarnessClientKind          `json:"harness"`
	TipSHA              string                     `json:"tip_sha"`
	MeasuredFromImageID Digest                     `json:"measured_from_image_id"`
	Class               ImageUpkeepRefusalClass    `json:"class"`
	Findings            []ImageUpkeepClauseFinding `json:"findings"`
}

// NewImageUpkeepRefusal builds a validated record and computes its ID.
func NewImageUpkeepRefusal(in ImageUpkeepRefusalInput) (ImageUpkeepRefusal, error) {
	refusal := ImageUpkeepRefusal{
		RepositoryID: in.RepositoryID, Repository: in.Repository, Harness: in.Harness,
		TipSHA: in.TipSHA, MeasuredFromImageID: in.MeasuredFromImageID, Class: in.Class,
		Findings:   append([]ImageUpkeepClauseFinding{}, in.Findings...),
		RecordedAt: in.RecordedAt,
	}
	slices.SortFunc(refusal.Findings, compareImageUpkeepFindings)
	refusal.Findings = slices.Compact(refusal.Findings)
	id, err := refusal.ComputeID()
	if err != nil {
		return ImageUpkeepRefusal{}, err
	}
	refusal.ID = id
	if err := refusal.Validate(); err != nil {
		return ImageUpkeepRefusal{}, err
	}
	return refusal, nil
}

// Preimage returns the canonical v1 bytes the ID addresses: every fact but
// RecordedAt. A nil and an empty Findings list encode alike, so a record
// decoded from either form keeps its ID. A caller that stores these bytes in
// the content-addressed blob store gets them back under the refusal's ID.
func (r ImageUpkeepRefusal) Preimage() ([]byte, error) {
	findings := r.Findings
	if findings == nil {
		findings = []ImageUpkeepClauseFinding{}
	}
	body, err := json.Marshal(canonicalImageUpkeepRefusal{
		Version:      imageUpkeepRefusalEncodingVersion,
		RepositoryID: r.RepositoryID, Repository: r.Repository, Harness: r.Harness,
		TipSHA: r.TipSHA, MeasuredFromImageID: r.MeasuredFromImageID, Class: r.Class,
		Findings: findings,
	})
	if err != nil {
		return nil, fmt.Errorf("image upkeep refusal preimage: %w", err)
	}
	return body, nil
}

// ComputeID returns the versioned content address of Preimage, so the
// exported bytes and the ID cannot drift apart.
func (r ImageUpkeepRefusal) ComputeID() (Digest, error) {
	body, err := r.Preimage()
	if err != nil {
		return "", fmt.Errorf("image upkeep refusal id: %w", err)
	}
	return Digest(contentaddr.Sum(body)), nil
}

// Validate checks the record's shape and that its ID addresses its facts.
// Only a host fault may name no clause: every other class is a gate verdict,
// and a verdict with no clause would carry nothing to propose or repair.
func (r ImageUpkeepRefusal) Validate() error {
	if r.RepositoryID <= 0 {
		return fmt.Errorf("image upkeep refusal repository_id %d: %w", r.RepositoryID, ErrNonPositive)
	}
	if !projectRepositoryPattern.MatchString(r.Repository) || strings.Contains(r.Repository, "..") {
		return fmt.Errorf("image upkeep refusal repository %q: %w", r.Repository, ErrImageUpkeepRefusalInvalid)
	}
	if !r.Harness.valid() {
		return fmt.Errorf("image upkeep refusal harness %q: %w", r.Harness, ErrInvalidHarnessClientKind)
	}
	if !projectCommitPattern.MatchString(r.TipSHA) {
		return fmt.Errorf("image upkeep refusal tip_sha %q: %w", r.TipSHA, ErrImageUpkeepRefusalInvalid)
	}
	if !contentaddr.Valid(string(r.MeasuredFromImageID)) {
		return fmt.Errorf("image upkeep refusal measured_from_image_id %q: %w",
			r.MeasuredFromImageID, ErrImageUpkeepRefusalInvalid)
	}
	if !r.Class.valid() {
		return fmt.Errorf("image upkeep refusal class %q: %w", r.Class, ErrInvalidImageUpkeepRefusalClass)
	}
	if len(r.Findings) == 0 && r.Class != ImageUpkeepRefusalHostFault {
		return fmt.Errorf("image upkeep refusal class %q names no clause: %w",
			r.Class, ErrImageUpkeepRefusalInvalid)
	}
	for index, finding := range r.Findings {
		if err := finding.validate(); err != nil {
			return fmt.Errorf("image upkeep refusal findings[%d]: %w", index, err)
		}
		if index > 0 && compareImageUpkeepFindings(r.Findings[index-1], finding) >= 0 {
			return fmt.Errorf("image upkeep refusal findings[%d] is out of canonical order or repeated: %w",
				index, ErrImageUpkeepRefusalInvalid)
		}
	}
	if r.RecordedAt.IsZero() {
		return fmt.Errorf("image upkeep refusal recorded_at: %w", ErrMissingTimestamp)
	}
	if r.RecordedAt.Location() != time.UTC {
		return fmt.Errorf("image upkeep refusal recorded_at: %w", ErrTimestampNotUTC)
	}
	if !contentaddr.Valid(string(r.ID)) {
		return fmt.Errorf("image upkeep refusal id %q: %w", r.ID, ErrImageUpkeepRefusalInvalid)
	}
	computed, err := r.ComputeID()
	if err != nil {
		return err
	}
	if r.ID != computed {
		return fmt.Errorf("image upkeep refusal %s, content resolves to %s: %w",
			r.ID, computed, ErrImageUpkeepRefusalInconsistent)
	}
	return nil
}

// RepositoryTipObservation is the latest default-branch tip the daemon read
// for one repository, independent of any bound PR (plan §5.7, Project-image
// upkeep). UpkeepTipSHA is the tip the upkeep job last ran at, empty until it
// first runs; a tip that differs from it is what triggers the job.
type RepositoryTipObservation struct {
	RepositoryID int64     `json:"repository_id"`
	Repository   string    `json:"repository"`
	Branch       string    `json:"branch"`
	TipSHA       string    `json:"tip_sha"`
	ObservedAt   time.Time `json:"observed_at"`
	UpkeepTipSHA string    `json:"upkeep_tip_sha"`
}

// Validate checks the observation's shape.
func (o RepositoryTipObservation) Validate() error {
	if o.RepositoryID <= 0 {
		return fmt.Errorf("repository tip observation repository_id %d: %w", o.RepositoryID, ErrNonPositive)
	}
	if !projectRepositoryPattern.MatchString(o.Repository) || strings.Contains(o.Repository, "..") {
		return fmt.Errorf("repository tip observation repository %q: %w",
			o.Repository, ErrRepositoryTipObservationInvalid)
	}
	if o.Branch == "" || len(o.Branch) > maxRepositoryBranchBytes || !utf8.ValidString(o.Branch) ||
		strings.ContainsFunc(o.Branch, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return fmt.Errorf("repository tip observation branch %q: %w", o.Branch, ErrRepositoryTipObservationInvalid)
	}
	if !projectCommitPattern.MatchString(o.TipSHA) {
		return fmt.Errorf("repository tip observation tip_sha %q: %w", o.TipSHA, ErrRepositoryTipObservationInvalid)
	}
	if o.UpkeepTipSHA != "" && !projectCommitPattern.MatchString(o.UpkeepTipSHA) {
		return fmt.Errorf("repository tip observation upkeep_tip_sha %q: %w",
			o.UpkeepTipSHA, ErrRepositoryTipObservationInvalid)
	}
	if o.ObservedAt.IsZero() {
		return fmt.Errorf("repository tip observation observed_at: %w", ErrMissingTimestamp)
	}
	if o.ObservedAt.Location() != time.UTC {
		return fmt.Errorf("repository tip observation observed_at: %w", ErrTimestampNotUTC)
	}
	return nil
}
