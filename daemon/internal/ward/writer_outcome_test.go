package ward

import (
	"errors"
	"strings"
	"testing"
)

func TestVerifyWriterOutcomeProof(t *testing.T) {
	t.Parallel()
	nonce := strings.Repeat("a", 32)
	tests := []struct {
		name   string
		proof  string
		status int
		ok     bool
	}{
		{"success", nonce + " 0\n", 0, true},
		{"failure", nonce + " 86\n", 86, true},
		{"missing newline", nonce + " 0", 0, false},
		{"extra line", nonce + " 0\nignored\n", 0, false},
		{"foreign nonce", strings.Repeat("b", 32) + " 0\n", 0, false},
		{"negative", nonce + " -1\n", 0, false},
		{"overflow", nonce + " 256\n", 0, false},
		{"leading zero", nonce + " 01\n", 0, false},
		{"extra field", nonce + " 0 extra\n", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := verifyWriterOutcomeProof([]byte(tc.proof), nonce)
			if tc.ok {
				if err != nil || got != tc.status {
					t.Fatalf("verify = (%d, %v), want (%d, nil)", got, err, tc.status)
				}
				return
			}
			if !errors.Is(err, ErrConformance) {
				t.Fatalf("verify error = %v, want ErrConformance", err)
			}
		})
	}
}

// TestObserveWriterOutcomeWrapsCleanupFailures pins how the writer-outcome
// observer reports a failure after its proof read: a failed delete or
// absence proof carries ErrWriterFailed only when the status was nonzero,
// and a failed journal write carries it once, never twice.
func TestObserveWriterOutcomeWrapsCleanupFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		status        int
		fail          string
		writerFailed  bool
		conformance   bool
		journalLeft   bool
		failureStatus bool
	}{
		{"status 0 delete", 0, "delete", false, true, false, false},
		{"status 0 absence", 0, "absent", false, true, false, false},
		{"status 7 delete", 7, "delete", true, true, false, true},
		{"status 7 absence", 7, "absent", true, true, false, true},
		{"status 7 journal", 7, "journal", true, false, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newHandoffFixture(t)
			j := fx.journalled()
			hs := testHandoffSpec()
			hs.Agent.OutcomeMarkerPath = fx.cfg.WorkspaceTarget + "/.freeside/evidence/writer-outcome"
			hs.Agent.Command = []string{"sh", "-c", "printf '%s 0\\n' " + WriterNoncePlaceholder + " > " + hs.Agent.OutcomeMarkerPath}
			fx.rt.writerStatus = tc.status
			if err := j.Begin(t.Context(), HandoffJournalRecord{RunID: hs.RunID}); err != nil {
				t.Fatal(err)
			}
			names := namesFor(hs.RunID)
			injected := errors.New("injected failure")
			switch tc.fail {
			case "delete":
				fx.rt.onDeleteContainer = func(id string) (bool, error) {
					if id == names.WriterObserver {
						return true, injected
					}
					return false, nil
				}
			case "absent":
				fx.rt.onDeleteContainer = func(id string) (bool, error) {
					return id == names.WriterObserver, nil
				}
			case "journal":
				j.failMark = injected
			}
			st := &runState{ownershipLabel: testOwnershipLabel(), journalOpen: true}
			status, err := fx.backend(t).observeWriterOutcome(t.Context(), hs, names, st)
			if err == nil || status != 0 {
				t.Fatalf("observeWriterOutcome = %d, %v; want 0 and an error", status, err)
			}
			if got := errors.Is(err, ErrWriterFailed); got != tc.writerFailed {
				t.Errorf("errors.Is(ErrWriterFailed) = %v, want %v: %v", got, tc.writerFailed, err)
			}
			if n := strings.Count(err.Error(), ErrWriterFailed.Error()); n > 1 {
				t.Errorf("ErrWriterFailed joined %d times: %v", n, err)
			}
			var cf *ConformanceFailure
			if got := errors.As(err, &cf) && cf.Check == CheckWriterTermination; got != tc.conformance {
				t.Errorf("writer_termination conformance failure = %v, want %v: %v", got, tc.conformance, err)
			}
			if st.preserveForRecovery != tc.journalLeft || st.leaveJournalOpen != tc.journalLeft {
				t.Errorf("preserveForRecovery, leaveJournalOpen = %v, %v; want %v",
					st.preserveForRecovery, st.leaveJournalOpen, tc.journalLeft)
			}
			if got := st.writerFailureStatus != nil; got != tc.failureStatus {
				t.Errorf("writerFailureStatus set = %v, want %v", got, tc.failureStatus)
			}
			if !st.writerObserver.attempted || !st.writerObserver.owned {
				t.Errorf("writer-outcome claim = %+v, want it kept for teardown", st.writerObserver)
			}
		})
	}
}
