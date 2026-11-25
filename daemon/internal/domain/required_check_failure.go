package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const (
	// RequiredCheckFailureKind is the outbox kind of a RequiredCheckFailure
	// record.
	RequiredCheckFailureKind    = "required_check_failure"
	RequiredCheckFailureVersion = "freeside.required-check-failure/v1"
	// MaxCheckFailureJobs bounds the failed jobs a record carries, and so
	// the logs a check_failure cycle reads and hands its remediator.
	MaxCheckFailureJobs = 3
	// MaxCheckFailureExcerptBytes bounds one job's log excerpt: the tail of
	// the log, where a test runner prints its failures.
	MaxCheckFailureExcerptBytes = 8 << 10
)

// RequiredCheckFailure is the daemon's record of a published head's failed
// required check, the failure a check_failure authority answers (issue
// #3598). The daemon writes it from the check's jobs in the transaction that
// seals the authority; nothing a client sends reaches it.
type RequiredCheckFailure struct {
	Version  string `json:"version"`
	RunID    RunID  `json:"run_id"`
	Repo     string `json:"repo"`
	PRNumber int    `json:"pr_number"`
	HeadSHA  string `json:"head_sha"`
	// Jobs are the failed jobs the card named, earliest first, at most
	// MaxCheckFailureJobs.
	Jobs []CheckFailureJob `json:"jobs"`
}

// CheckFailureJob is one failed job of a RequiredCheckFailure. Name is
// screened (ScreenRequiredCheckJobName). ExcerptDigest is the content
// address of the job's screened log excerpt (ScreenCheckFailureExcerpt) in
// the artifact store.
type CheckFailureJob struct {
	Name          string `json:"name"`
	JobID         int64  `json:"job_id"`
	WorkflowRunID int64  `json:"workflow_run_id"`
	Attempt       int    `json:"attempt"`
	ExcerptDigest Digest `json:"excerpt_digest"`
}

// Validate reports whether the record is structurally sound. A job name
// that still matches a credential rule is refused, as HandoffCheckFailure
// refuses such evidence, so a record that reached the store is screened
// whoever wrote it.
func (f RequiredCheckFailure) Validate() error {
	if f.Version != RequiredCheckFailureVersion || f.RunID == "" || f.PRNumber < 1 || f.HeadSHA == "" {
		return fmt.Errorf("required check failure: incomplete record: %w", ErrParentKeyMismatch)
	}
	if owner, name, ok := strings.Cut(f.Repo, "/"); !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return fmt.Errorf("required check failure repository %q: %w", f.Repo, ErrParentKeyMismatch)
	}
	if len(f.Jobs) == 0 || len(f.Jobs) > MaxCheckFailureJobs {
		return fmt.Errorf("required check failure names %d jobs, want 1..%d: %w",
			len(f.Jobs), MaxCheckFailureJobs, ErrParentKeyMismatch)
	}
	for _, job := range f.Jobs {
		if err := validateRequiredCheckJobName(job.Name); err != nil {
			return err
		}
		if job.JobID < 1 || job.WorkflowRunID < 1 || job.Attempt < 1 || !contentaddr.Valid(string(job.ExcerptDigest)) {
			return fmt.Errorf("required check failure job %d: %w", job.JobID, ErrParentKeyMismatch)
		}
	}
	return nil
}

// Key is the record's outbox key: its run, its head, and the workflow run
// attempts its jobs failed in. One failure of one head is recorded once.
func (f RequiredCheckFailure) Key() string {
	var attempts []string
	for _, job := range f.Jobs {
		attempt := strconv.FormatInt(job.WorkflowRunID, 10) + "." + strconv.Itoa(job.Attempt)
		if !slices.Contains(attempts, attempt) {
			attempts = append(attempts, attempt)
		}
	}
	return RequiredCheckFailureKeyPrefix(f.RunID) + url.PathEscape(f.HeadSHA) + "/" + strings.Join(attempts, ",")
}

// RequiredCheckFailureKeyPrefix is the key prefix of every failure record
// of one run.
func RequiredCheckFailureKeyPrefix(run RunID) string {
	return "required-check-failure/" + url.PathEscape(string(run)) + "/"
}

// Encode returns the record's canonical bytes and their content address,
// which a check_failure authority names.
func (f RequiredCheckFailure) Encode() ([]byte, Digest, error) {
	if err := f.Validate(); err != nil {
		return nil, "", err
	}
	body, err := json.Marshal(f)
	if err != nil {
		return nil, "", err
	}
	return body, Digest(contentaddr.Sum(body)), nil
}

// ExcerptDigests are the content addresses of the record's log excerpts.
func (f RequiredCheckFailure) ExcerptDigests() []Digest {
	digests := make([]Digest, 0, len(f.Jobs))
	for _, job := range f.Jobs {
		digests = append(digests, job.ExcerptDigest)
	}
	return digests
}

// DecodeRequiredCheckFailure strictly decodes a stored record and refuses
// any encoding but the canonical one.
func DecodeRequiredCheckFailure(body []byte) (RequiredCheckFailure, error) {
	var f RequiredCheckFailure
	if err := strictjson.Decode(body, &f, strictjson.RejectInvalidUTF8, strictjson.Limit(1<<16)); err != nil {
		return RequiredCheckFailure{}, err
	}
	canonical, _, err := f.Encode()
	if err != nil {
		return RequiredCheckFailure{}, err
	}
	if !bytes.Equal(canonical, body) {
		return RequiredCheckFailure{}, ErrParentKeyMismatch
	}
	return f, nil
}

// ScreenCheckFailureExcerpt makes the tail of a job's log safe to store and
// to hand a remediator: valid UTF-8, the last MaxCheckFailureExcerptBytes of
// it cut on a rune boundary, and every credential-rule match replaced by the
// redaction token, as ScreenFailureEvidence does with a larger cap. The log
// is the candidate's output, so it is data; a log that still matches a rule
// after screening becomes the bare token.
//
// The whole log is screened before the tail is cut, so a credential that
// straddles the cut is replaced whole rather than left as a fragment no rule
// matches. The caller bounds the log it reads.
func ScreenCheckFailureExcerpt(log []byte) string {
	s := redactCredentials(strings.ToValidUTF8(string(log), "�"))
	if len(s) > MaxCheckFailureExcerptBytes {
		cut := len(s) - MaxCheckFailureExcerptBytes
		for cut < len(s) && !utf8.RuneStart(s[cut]) {
			cut++
		}
		// Screen again after the cut: the cut can leave a prefix of a
		// replaced match that a rule anchored on a boundary now matches.
		s = redactCredentials(s[cut:])
	}
	if strings.TrimSpace(s) == "" || len(s) > MaxCheckFailureExcerptBytes || matchesCredentialRule(s) {
		return redactedEvidence
	}
	return s
}

// ValidateCheckFailureExcerpt reports whether excerpt is one
// ScreenCheckFailureExcerpt could have produced: bounded, valid UTF-8, and
// matching no credential rule.
func ValidateCheckFailureExcerpt(excerpt string) error {
	if excerpt == "" || len(excerpt) > MaxCheckFailureExcerptBytes || !utf8.ValidString(excerpt) ||
		matchesCredentialRule(excerpt) {
		return fmt.Errorf("required check failure excerpt: %w", ErrParentKeyMismatch)
	}
	return nil
}
