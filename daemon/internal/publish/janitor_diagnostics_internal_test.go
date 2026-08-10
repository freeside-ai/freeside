package publish

import (
	"errors"
	"testing"
)

func TestPublishPassAttributesEveryCoverageWithdrawal(t *testing.T) {
	t.Parallel()
	fault := errors.New("authority unavailable")
	tests := []struct {
		name       string
		faults     []JanitorRegistrationFault
		churn      []int64
		incomplete []int64
		assert     func(*testing.T, *InstallationJanitor)
	}{
		{
			name:   "fault",
			faults: []JanitorRegistrationFault{{RegistrationID: 501, Err: fault}},
			assert: func(t *testing.T, janitor *InstallationJanitor) {
				t.Helper()
				got := janitor.RegistrationFaults()
				if len(got) != 1 || got[0].RegistrationID != 501 || !errors.Is(got[0].Err, fault) {
					t.Fatalf("faults = %+v", got)
				}
			},
		},
		{
			name:  "churn",
			churn: []int64{501},
			assert: func(t *testing.T, janitor *InstallationJanitor) {
				t.Helper()
				got := janitor.ChurningRegistrations()
				if len(got) != 1 || got[0] != (JanitorRegistrationChurn{
					RegistrationID: 501, ConsecutivePasses: 1,
				}) {
					t.Fatalf("churn = %+v", got)
				}
			},
		},
		{
			name:       "incomplete",
			incomplete: []int64{501},
			assert: func(t *testing.T, janitor *InstallationJanitor) {
				t.Helper()
				got := janitor.IncompleteRegistrations()
				if len(got) != 1 || got[0].RegistrationID != 501 {
					t.Fatalf("incomplete = %+v", got)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			covered := []registrationCoverage{{
				registrationID: 501,
				repositories: map[int64]map[int64]struct{}{
					701: {9001: {}},
				},
			}}
			pass := janitorPass{
				registrations: []int64{501},
				faults:        test.faults, churn: test.churn, incomplete: test.incomplete,
			}
			pass.covered = withdrawUnreconciled(
				covered, pass.faults, pass.churn, pass.incomplete,
			)
			janitor := &InstallationJanitor{covered: map[int64]registrationCoverage{}}
			janitor.publishPass(pass)
			if janitor.ActiveFor(501) {
				t.Fatal("withdrawn registration remained covered")
			}
			test.assert(t, janitor)
			diagnostics := len(janitor.RegistrationFaults()) +
				len(janitor.ChurningRegistrations()) + len(janitor.IncompleteRegistrations())
			if diagnostics != 1 {
				t.Fatalf("published %d diagnostics, want exactly one", diagnostics)
			}
		})
	}
}

func TestWithdrawUnreconciledUsesUnionOfAllCauses(t *testing.T) {
	t.Parallel()
	for mask := 0; mask < 8; mask++ {
		var (
			faults     []JanitorRegistrationFault
			churn      []int64
			incomplete []int64
		)
		if mask&1 != 0 {
			faults = []JanitorRegistrationFault{{RegistrationID: 501, Err: errors.New("fault")}}
		}
		if mask&2 != 0 {
			churn = []int64{501}
		}
		if mask&4 != 0 {
			incomplete = []int64{501}
		}
		got := withdrawUnreconciled(
			[]registrationCoverage{{registrationID: 501}}, faults, churn, incomplete,
		)
		if (len(got) == 0) != (mask != 0) {
			t.Errorf("mask %03b coverage = %+v", mask, got)
		}
	}
	got := withdrawUnreconciled(
		[]registrationCoverage{{registrationID: 501}},
		[]JanitorRegistrationFault{{RegistrationID: 601, Err: errors.New("sibling fault")}},
		[]int64{601},
		[]int64{601},
	)
	if len(got) != 1 || got[0].registrationID != 501 {
		t.Fatalf("sibling causes withdrew registration 501: %+v", got)
	}
}
