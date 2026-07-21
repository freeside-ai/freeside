package main

const (
	killTestCheckpointEnv = "FREESIDE_KILL_TEST_CHECKPOINT"
	killTestMarkerEnv     = "FREESIDE_KILL_TEST_MARKER"
)

const (
	killCheckpointBeforeIntentDispatch = "before_intent_dispatch"
	killCheckpointAfterIntentAccepted  = "after_intent_accepted"
	killCheckpointAfterResultCommitted = "after_result_committed"
)
