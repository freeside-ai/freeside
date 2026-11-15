package publish

import (
	"testing"
	"time"
)

func TestPendingReadyNeverGrantsRuntimeAuthority(t *testing.T) {
	t.Parallel()
	janitor := &InstallationJanitor{
		covered: map[int64]registrationCoverage{
			11: {
				registrationID: 11,
				repositories:   map[int64]map[int64]struct{}{},
				pendingReady: &pendingCoverage{
					activeEpoch: 7, durableIntentRevision: 9,
					installationID: 22, repositories: map[int64]struct{}{33: {}},
				},
			},
		},
	}
	envelope := PendingInstallationEnvelope{
		ActiveEpoch: 7, DurableIntentRevision: 9,
		RegistrationID: 11, InstallationID: 22,
		ExpectedRepositoryIDs: []int64{33},
	}
	if installationID, ready := janitor.PendingReady(envelope); !ready || installationID != 22 {
		t.Fatal("PendingReady did not expose the exact transition signal")
	}
	unassigned := envelope
	unassigned.InstallationID = 0
	if installationID, ready := janitor.PendingReady(unassigned); !ready || installationID != 22 {
		t.Fatal("PendingReady did not resolve GitHub's assigned installation ID")
	}
	if janitor.AllowsRepository(11, 22, 33) {
		t.Fatal("pending transition signal leaked into runtime authority")
	}
	for _, changed := range []PendingInstallationEnvelope{
		func() PendingInstallationEnvelope { v := envelope; v.RegistrationID = 10; return v }(),
		func() PendingInstallationEnvelope { v := envelope; v.InstallationID = 21; return v }(),
		func() PendingInstallationEnvelope { v := envelope; v.ActiveEpoch = 8; return v }(),
		func() PendingInstallationEnvelope {
			v := envelope
			v.DurableIntentRevision = 10
			return v
		}(),
		func() PendingInstallationEnvelope {
			v := envelope
			v.ExpectedRepositoryIDs = []int64{32}
			return v
		}(),
	} {
		if _, ready := janitor.PendingReady(changed); ready {
			t.Fatalf("PendingReady accepted changed coordinates %+v", changed)
		}
	}
}

// TestCoverageGatesServeTheLastPassWhileOneRuns: while a pass runs, a grant
// the last complete pass covered is answered at once, with no wait on the
// pass, and a grant it lacks waits for the running pass and reads its result
// (#3215).
func TestCoverageGatesServeTheLastPassWhileOneRuns(t *testing.T) {
	t.Parallel()
	janitor := &InstallationJanitor{
		covered: map[int64]registrationCoverage{
			11: {
				registrationID: 11,
				repositories:   map[int64]map[int64]struct{}{22: {33: {}}},
				pendingReady: &pendingCoverage{
					activeEpoch: 7, durableIntentRevision: 9,
					installationID: 22, repositories: map[int64]struct{}{33: {}},
				},
			},
		},
	}
	envelope := PendingInstallationEnvelope{
		ActiveEpoch: 7, DurableIntentRevision: 9,
		RegistrationID: 11, InstallationID: 22,
		ExpectedRepositoryIDs: []int64{33},
	}
	done := janitor.beginPublishedPass()
	if !janitor.AwaitActiveFor(11) || !janitor.AwaitAllowsRepository(11, 22, 33) {
		t.Fatal("covered grant was not served while a pass runs")
	}
	if installationID, ready := janitor.AwaitPendingReady(envelope); !ready || installationID != 22 {
		t.Fatal("ready envelope was not served while a pass runs")
	}
	added := make(chan bool, 1)
	go func() { added <- janitor.AwaitAllowsRepository(11, 22, 34) }()
	unknown := make(chan bool, 1)
	go func() { unknown <- janitor.AwaitActiveFor(12) }()
	select {
	case <-added:
		t.Fatal("uncovered grant answered before the running pass published")
	case <-unknown:
		t.Fatal("uncovered registration answered before the running pass published")
	case <-time.After(20 * time.Millisecond):
	}
	janitor.publishPass(janitorPass{
		registrations: []int64{11},
		covered: []registrationCoverage{{
			registrationID: 11,
			repositories:   map[int64]map[int64]struct{}{22: {33: {}, 34: {}}},
		}},
	})
	janitor.endPublishedPass(done)
	if !<-added {
		t.Fatal("grant the running pass covered was denied after it published")
	}
	if <-unknown {
		t.Fatal("registration no pass covered was allowed")
	}
	if _, ready := janitor.AwaitPendingReady(envelope); ready {
		t.Fatal("pending readiness outlived the pass that replaced it")
	}
}
