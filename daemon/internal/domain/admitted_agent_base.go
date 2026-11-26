package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// ErrAdmittedAgentBaseInvalid marks a malformed admitted agent base or
// pointer.
var ErrAdmittedAgentBaseInvalid = errors.New("admitted agent base is invalid")

// AgentBaseSourceDir is the directory of the source repository whose tree
// defines the Claude agent base: its Containerfile and the files it copies.
// The upkeep job builds a base only from this directory at the source
// repository's merged tip.
const AgentBaseSourceDir = "images/agent-claude"

// AdmittedAgentBase is one agent base the daemon built, probed, and admitted
// (plan §5.7, Agent-base upkeep). It is write-once and content-addressed:
// ID is the digest of every fact but AdmittedAt, so the same build of the
// same source tree records once.
//
// The base is built from AgentBaseSourceDir at SourceCommit of the one
// repository the agent_base instance setting names; SourceTreeSHA is that
// directory's git tree, so a tip that changes nothing under it needs no
// build. HarnessBuild is the harness build the probe read from the image,
// in the form agentbaseline pins (for Claude, "claude-code <version>"), and
// the record exists only because it equaled the daemon's own pin. ImageRef
// is the published, digest-pinned base that project images build FROM.
type AdmittedAgentBase struct {
	ID               Digest            `json:"id"`
	Harness          HarnessClientKind `json:"harness"`
	ImageRef         ImageRef          `json:"image_ref"`
	HarnessBuild     string            `json:"harness_build"`
	SourceRepoID     int64             `json:"source_repository_id"`
	SourceRepository string            `json:"source_repository"`
	SourceCommit     string            `json:"source_commit"`
	SourceTreeSHA    string            `json:"source_tree_sha"`
	AdmittedAt       time.Time         `json:"admitted_at"`
}

// AdmittedAgentBaseInput is everything but the derived ID.
type AdmittedAgentBaseInput struct {
	Harness          HarnessClientKind
	ImageRef         ImageRef
	HarnessBuild     string
	SourceRepoID     int64
	SourceRepository string
	SourceCommit     string
	SourceTreeSHA    string
	AdmittedAt       time.Time
}

// NewAdmittedAgentBase builds and validates a record, deriving its ID.
func NewAdmittedAgentBase(in AdmittedAgentBaseInput) (AdmittedAgentBase, error) {
	base := AdmittedAgentBase{
		Harness: in.Harness, ImageRef: in.ImageRef, HarnessBuild: in.HarnessBuild,
		SourceRepoID: in.SourceRepoID, SourceRepository: in.SourceRepository,
		SourceCommit: in.SourceCommit, SourceTreeSHA: in.SourceTreeSHA,
		AdmittedAt: in.AdmittedAt,
	}
	id, err := base.ComputeID()
	if err != nil {
		return AdmittedAgentBase{}, err
	}
	base.ID = id
	if err := base.Validate(); err != nil {
		return AdmittedAgentBase{}, err
	}
	return base, nil
}

const admittedAgentBaseEncodingVersion = 1

type canonicalAdmittedAgentBase struct {
	Version          int               `json:"v"`
	Harness          HarnessClientKind `json:"harness"`
	ImageRef         ImageRef          `json:"image_ref"`
	HarnessBuild     string            `json:"harness_build"`
	SourceRepoID     int64             `json:"source_repository_id"`
	SourceRepository string            `json:"source_repository"`
	SourceCommit     string            `json:"source_commit"`
	SourceTreeSHA    string            `json:"source_tree_sha"`
}

// ComputeID returns the versioned content address of every fact but
// AdmittedAt. The store rebuilds it on decode, so a row whose columns were
// changed no longer matches its key.
func (b AdmittedAgentBase) ComputeID() (Digest, error) {
	body, err := json.Marshal(canonicalAdmittedAgentBase{
		Version: admittedAgentBaseEncodingVersion,
		Harness: b.Harness, ImageRef: b.ImageRef, HarnessBuild: b.HarnessBuild,
		SourceRepoID: b.SourceRepoID, SourceRepository: b.SourceRepository,
		SourceCommit: b.SourceCommit, SourceTreeSHA: b.SourceTreeSHA,
	})
	if err != nil {
		return "", fmt.Errorf("admitted agent base id: %w", err)
	}
	return Digest(contentaddr.Sum(body)), nil
}

// Validate checks the record's shape and that ID addresses its facts.
func (b AdmittedAgentBase) Validate() error {
	if !b.Harness.valid() {
		return fmt.Errorf("admitted agent base harness %q: %w", b.Harness, ErrInvalidHarnessClientKind)
	}
	if err := b.ImageRef.Validate(); err != nil {
		return fmt.Errorf("admitted agent base image_ref: %w", err)
	}
	if !validHarnessBuild(b.HarnessBuild) {
		return fmt.Errorf("admitted agent base harness_build %q: %w", b.HarnessBuild, ErrAdmittedAgentBaseInvalid)
	}
	if err := validAgentBaseSource(b.SourceRepoID, b.SourceRepository); err != nil {
		return fmt.Errorf("admitted agent base: %w", err)
	}
	if !projectCommitPattern.MatchString(b.SourceCommit) {
		return fmt.Errorf("admitted agent base source_commit %q: %w", b.SourceCommit, ErrAdmittedAgentBaseInvalid)
	}
	if !projectCommitPattern.MatchString(b.SourceTreeSHA) {
		return fmt.Errorf("admitted agent base source_tree_sha %q: %w", b.SourceTreeSHA, ErrAdmittedAgentBaseInvalid)
	}
	if b.AdmittedAt.IsZero() {
		return fmt.Errorf("admitted agent base admitted_at: %w", ErrMissingTimestamp)
	}
	if b.AdmittedAt.Location() != time.UTC {
		return fmt.Errorf("admitted agent base admitted_at: %w", ErrTimestampNotUTC)
	}
	id, err := b.ComputeID()
	if err != nil {
		return err
	}
	if b.ID != id {
		return fmt.Errorf("admitted agent base id %q does not address its facts: %w", b.ID, ErrAdmittedAgentBaseInvalid)
	}
	return nil
}

// validHarnessBuild accepts a short printable build string such as
// "claude-code 2.1.319".
func validHarnessBuild(build string) bool {
	if build == "" || len(build) > 128 || strings.TrimSpace(build) != build {
		return false
	}
	for _, r := range build {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

func validAgentBaseSource(repositoryID int64, repository string) error {
	if repositoryID <= 0 {
		return fmt.Errorf("source repository_id %d: %w", repositoryID, ErrNonPositive)
	}
	if !projectRepositoryPattern.MatchString(repository) || strings.Contains(repository, "..") {
		return fmt.Errorf("source repository %q: %w", repository, ErrAdmittedAgentBaseInvalid)
	}
	return nil
}

// AdmittedAgentBasePointer names the agent base project images for one
// harness build FROM. Like AdmittedImagePointer it selects a record and
// never vouches for one: the store advances it only to a recorded base of
// the same harness and re-reads that record whenever it decodes it.
type AdmittedAgentBasePointer struct {
	Harness    HarnessClientKind `json:"harness"`
	BaseID     Digest            `json:"base_id"`
	AdvancedAt time.Time         `json:"advanced_at"`
}

// Validate checks the pointer's own shape.
func (p AdmittedAgentBasePointer) Validate() error {
	if !p.Harness.valid() {
		return fmt.Errorf("admitted agent base pointer harness %q: %w", p.Harness, ErrInvalidHarnessClientKind)
	}
	if !contentaddr.Valid(string(p.BaseID)) {
		return fmt.Errorf("admitted agent base pointer base_id %q: %w", p.BaseID, ErrAdmittedAgentBaseInvalid)
	}
	if p.AdvancedAt.IsZero() {
		return fmt.Errorf("admitted agent base pointer advanced_at: %w", ErrMissingTimestamp)
	}
	if p.AdvancedAt.Location() != time.UTC {
		return fmt.Errorf("admitted agent base pointer advanced_at: %w", ErrTimestampNotUTC)
	}
	return nil
}
