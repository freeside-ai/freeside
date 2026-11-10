// Package daemoncompose builds the parts of a freesided start that package
// main composes:
//
//   - The Claude composition (-driver claude). ComposeClaudeWorkflow builds
//     the Claude driver, its GitHub wiring, the judgment roles and their
//     credentials, the agent selection and its conformance record, and the
//     engine around them. ComposeClaudeLoops builds the loops that
//     composition runs: the durable scheduler and the jobs it arms, the
//     intake and active-resource reconcilers, and the image-upkeep
//     trigger. ClaudeStartupConfig carries what both read from main's
//     configuration.
//   - The scripted scenario lane (ComposeScenario, -driver scenario).
//   - The walking-skeleton fake lane (ComposeFakeWorkflow and
//     SeedWalkingSkeleton, -driver fake).
//   - The startup storage phases (SeedStartupFixture and
//     MaintainLocalBackups).
//   - ProductionEngine, the one builder that turns a production
//     composition's parts into engine options for both the Claude workflow
//     and the scenario.
//
// The functions here build and return; they start no loop. Package main
// still owns run() and its flags, the attention service, usage collection
// (ComposeClaudeLoops takes the collector's pass as a parameter),
// newDoctorPass, the loop starters (startServing, startClaudeLoops, and
// startScenarioLoops, including the one that calls ServeForge), the
// control routes, the preflight, deputy, rig, and auth commands, and every
// cleanup of a start that fails after a composition returns, such as
// closing the scenario forge's listener.
//
// The scenario lane runs only under -environment ephemeral, against an
// in-process fake forge; run refuses any other environment before the
// store opens. ScenarioParts carries seven values the scenario does not
// build itself, and main still builds and passes each one: the admission
// floor, the two delivery validators, the export recorder, and the agent
// expiry margin its lineup admits with, from the Tier A claude_driver.go;
// the publication watch registrations, from scheduler.go; and the import
// authority, from internal/admissionauth. Main builds each but the margin
// exactly as the scenario built it there, and passes the Claude
// composition's own margin: the import authority and export recorder are
// bare, not the Claude composition's configured values.
//
// Some exports exist only for package main's tests. Those tests need
// fixtures main owns (the auth adopt command, preflight, run, main's
// configuration, or the seeded template store), so they stay in main and
// reach this package through exported names. Main's production code uses
// none of these: ActivateAgentSelection, ActiveResourceReconciler,
// AdapterConformanceLeaser, AgentSelectionGate, AgentSelectionItemPrefix,
// AttendedAdmissionFloor, BindRigRuntimeResources,
// CheckJudgmentRolesAtStartup, CheckWriterRoles,
// ClaudeReviewConfigurationDigest, ComposeHostVerificationExecutor,
// ErrJudgmentCredentialUnavailable, ErrRetiredIdentityOwnsWork,
// ExactBaseFetcher, HeldJudgmentCredential, ImageRepairProject,
// InferenceLedgerItemPrefix, InvocationStallNotice, JudgmentCredentials,
// JudgmentRoleHealth, JudgmentRoleItemPrefix, JudgmentRoleOfCause,
// LedgerHealthSubject, LogEffectiveReviewConfiguration,
// NativeReviewObserver, NewJudgmentRoleHealth, ObserveCompositionBaseInputs,
// PendingImageRepairTask, PromptPackages, ProveSelectedClaudeAdapter,
// RecordObservedAdapterConformance, ReportRoleFailure,
// ResolveReviewSelection, ReviewAdmission, ReviewAdmitter, RoleCausePrefix,
// ShadowReviewConfigurationDigests, ShadowReviewProvider,
// StoreConformanceRecorder, TaskRuntimeCoverageFile, UnprovableTaskStop,
// and WriterLineages, with the exported fields of ActiveResourceReconciler,
// the active-resource reconcile's result, AdapterConformanceLeaser,
// HeldJudgmentCredential, JudgmentCredentials, JudgmentRoleHealth,
// PromptPackages, the shadow review digests, and StoreConformanceRecorder.
//
// Test helpers that tests on both sides use are copied into both packages,
// identical apart from package qualifiers, so a change to one copy belongs
// in the other. In this
// package's test files they are: active_resource_native_test.go
// (testReviewerLogin, testReviewers, staticReview, execStoreSQL),
// active_resource_test.go (activeResourceTestTime, armActiveTestSchedules,
// engineScheduleID, readActiveItem, exactPull, activePullFacts,
// assertNoActiveCompletion), agent_selection_test.go
// (testObservedClaudeCapabilities, recordTestClaudeAdapterConformance),
// completion_milestone_test.go (workUnitCompletedMilestones,
// assertCompletionMilestone), external_review_reentry_test.go
// (externalTestMaintainerID, externalTestMaintainerLogin,
// externalTestMaintainer, activateExternalTestProfile,
// externalReentryActivity, externalReentryReconciler, reconcileClean),
// inference_test.go (judgmentFixture), judgment_credential_test.go
// (judgmentSentinelToken, judgmentSecondToken, judgmentTestToken),
// judgment_roles_test.go (judgmentTestSites), preflight_test.go
// (mustRoutedReviewProvider), prompt_packages_test.go
// (priorArtifactsTestDirective, promptPackagesFixture),
// readiness_reentry_test.go (reentryTestBase, reentryTestHead,
// reentryTestReviewID, reentryReadyItem, readReentry), rig_test.go
// (rigCommandRoots), scheduler_test.go (schedTestStore,
// schedTestResolvedPolicy, seedCaptureRun, readyBindingAuthority,
// seedReadyBindingAuthority, capturedRun, capturedRunNamed,
// capturedRunWithCriterion, capturedRunAtHead), setup_test.go
// (sharedSetupTestKey, setupTestKey), startup_test.go (testEvidenceTime,
// testRunEvidenceMetadata, lockedBuffer), and submission_egress_test.go
// (submissionEgressCase, submissionEgressCases, withSubmissionEgress,
// checkSubmissionEgressError, assertEgressPolicyAbsent). Main's copies sit
// in the same file names, except completion_reconcile_test.go and
// main_test.go for completion_milestone_test.go and startup_test.go.
package daemoncompose
