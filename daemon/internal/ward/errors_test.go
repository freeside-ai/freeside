package ward

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"syscall"
	"testing"
)

func TestCheckValid(t *testing.T) {
	t.Parallel()
	for _, c := range AllChecks {
		if !c.valid() {
			t.Errorf("Check %q: valid() = false, want true", c)
		}
	}
	for _, c := range []Check{"", "credential-separation", "check_6"} {
		if c.valid() {
			t.Errorf("Check %q: valid() = true, want false", c)
		}
	}
}

func TestAllChecksDistinct(t *testing.T) {
	t.Parallel()
	seen := make(map[Check]bool, len(AllChecks))
	for _, c := range AllChecks {
		if seen[c] {
			t.Errorf("check identifier %q listed twice", c)
		}
		seen[c] = true
	}
}

func TestConformanceFailureClassAndDetails(t *testing.T) {
	t.Parallel()
	var err error = &ConformanceFailure{
		Backend: "some_backend",
		Check:   CheckWriterTermination,
		Reason:  "state running after stop deadline",
	}

	if !errors.Is(err, ErrConformance) {
		t.Fatal("errors.Is(err, ErrConformance) = false, want true")
	}
	var cf *ConformanceFailure
	if !errors.As(err, &cf) {
		t.Fatal("errors.As(err, *ConformanceFailure) = false, want true")
	}
	if cf.Check != CheckWriterTermination {
		t.Errorf("Check = %q, want %q", cf.Check, CheckWriterTermination)
	}
	for _, want := range []string{"some_backend", string(CheckWriterTermination), "stop deadline"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Error() = %q, missing %q", err.Error(), want)
		}
	}
}

func TestFailwReachesCauseOnlyThroughErr(t *testing.T) {
	t.Parallel()
	pathErr := &fs.PathError{Op: "open", Path: "/run/ward/x", Err: syscall.EACCES}
	cause := fmt.Errorf("read manifest: %w", pathErr)
	err := failw(CheckExportVerification, cause, "read manifest: %v", cause)

	if !errors.Is(err, ErrConformance) {
		t.Fatal("errors.Is(err, ErrConformance) = false, want true")
	}
	var cf *ConformanceFailure
	if !errors.As(err, &cf) {
		t.Fatal("errors.As(err, *ConformanceFailure) = false, want true")
	}
	var got *fs.PathError
	if !errors.As(cf.Err, &got) {
		t.Fatal("errors.As(cf.Err, *fs.PathError) = false, want true")
	}
	if got != pathErr {
		t.Errorf("errors.As(cf.Err) reached %v, want %v", got, pathErr)
	}
}

func TestFailwTextMatchesFailf(t *testing.T) {
	t.Parallel()
	cause := errors.New("disk full")
	format, args := "write %s: %v", []any{"journal", cause}
	withCause := failw(CheckRecovery, cause, format, args...)
	without := failf(CheckRecovery, format, args...)
	if withCause.Error() != without.Error() {
		t.Errorf("failw Error() = %q, failf Error() = %q, want equal",
			withCause.Error(), without.Error())
	}
	var cf *ConformanceFailure
	if !errors.As(withCause, &cf) || cf.Reason != "write journal: disk full" {
		t.Errorf("failw Reason = %q, want %q", cf.Reason, "write journal: disk full")
	}
}

// TestFailwCauseDoesNotReclassify pins the decision to keep Err off the
// error chain (devlog/2026-11-04-0926-ward-conformance-cause.md): a cause
// carrying a classifier's sentinel or *fs.PathError must leave every
// chain-walking classifier's answer where a cause-free failure puts it.
func TestFailwCauseDoesNotReclassify(t *testing.T) {
	t.Parallel()
	causes := map[string]error{
		"operational":        fmt.Errorf("%w: journal", ErrCodexReviewOperational),
		"content evidence":   evidencef(CheckExportVerification, "digest mismatch"),
		"canceled":           context.Canceled,
		"memory limit":       ErrMemoryLimit,
		"path error":         &fs.PathError{Op: "open", Path: "/run/ward/x", Err: syscall.EIO},
		"wrapped path error": fmt.Errorf("stat: %w", &fs.PathError{Op: "stat", Path: "/x", Err: syscall.EIO}),
	}
	sentinels := map[string]error{
		"ErrCodexReviewOperational": ErrCodexReviewOperational,
		"errContentEvidence":        errContentEvidence,
		"context.Canceled":          context.Canceled,
		"ErrMemoryLimit":            ErrMemoryLimit,
	}
	for name, cause := range causes {
		t.Run(name, func(t *testing.T) {
			const format = "observe: %v"
			withCause := failw(CheckRecovery, cause, format, cause)
			without := failf(CheckRecovery, format, cause)

			for sentinelName, sentinel := range sentinels {
				if errors.Is(withCause, sentinel) {
					t.Errorf("errors.Is(failure, %s) = true, want false", sentinelName)
				}
			}
			var pathErr *fs.PathError
			if errors.As(withCause, &pathErr) {
				t.Error("errors.As(failure, *fs.PathError) = true, want false")
			}

			if got, want := classifyCodexLaunchFailure(withCause), classifyCodexLaunchFailure(without); got != want {
				t.Errorf("classifyCodexLaunchFailure = %q, want %q", got, want)
			}
			if got, want := classifyCodexObservationFailure(withCause), classifyCodexObservationFailure(without); got != want {
				t.Errorf("classifyCodexObservationFailure = %q, want %q", got, want)
			}
			if got, want := classifyCodexInstructionMaterializationFailure(withCause),
				classifyCodexInstructionMaterializationFailure(without); got != want {
				t.Errorf("classifyCodexInstructionMaterializationFailure = %q, want %q", got, want)
			}
			if got, want := codexReviewInstructionOpenIsOperational(withCause),
				codexReviewInstructionOpenIsOperational(without); got != want {
				t.Errorf("codexReviewInstructionOpenIsOperational = %t, want %t", got, want)
			}
		})
	}
}
