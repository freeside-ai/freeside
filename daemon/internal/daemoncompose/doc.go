// Package daemoncompose builds and starts a freesided daemon. Start is the
// whole start: it takes the database lock, opens the control socket, the
// listeners, and the store, builds the attention service and usage
// collection, composes one execution lane, runs the startup doctor pass
// and convergences, starts every loop, and publishes readiness. A start
// that fails releases everything it acquired, in reverse order. The lanes
// and phases it composes are:
//
//   - The Claude composition (-driver claude). ComposeClaudeWorkflow builds
//     the Claude driver, its GitHub wiring, the judgment roles and their
//     credentials, the agent selection and its conformance record, and the
//     engine around them. ComposeClaudeLoops builds the loops that
//     composition runs: the durable scheduler and the jobs it arms, the
//     intake and active-resource reconcilers, and the image-upkeep
//     trigger. ClaudeStartupConfig carries what both read from the start
//     options.
//   - The scripted scenario lane (ComposeScenario, -driver scenario).
//   - The walking-skeleton fake lane (ComposeFakeWorkflow and
//     SeedWalkingSkeleton, -driver fake).
//   - The startup storage phases (SeedStartupFixture and
//     MaintainLocalBackups).
//   - ProductionEngine, the one builder that turns a production
//     composition's parts into engine options for both the Claude workflow
//     and the scenario.
//
// Start is the one function here that starts loops; the Compose functions
// build and return. Package main keeps the flags and their translation
// into StartOptions, the control routes, and the CLI commands (preflight,
// deputy, rig, auth, and the rest). It supplies through StartOptions what
// only it has: the readiness environment name, the build version, the
// auth identity label, the fake driver's auto-scripting wrapper (beside
// the kill-recovery checkpoints), and OpenControl, which opens the control
// socket as a ControlSocket that serves main's routes. StartSeams holds the
// test seams; production leaves them unset.
//
// The scenario lane runs only under -environment ephemeral, against an
// in-process fake forge; run refuses any other environment before the
// store opens. ScenarioParts carries seven values the scenario does not
// build itself, and Start builds and passes each one: the admission
// floor, the two delivery validators, the export recorder, and the agent
// expiry margin its lineup admits with, from the Tier A claude_driver.go;
// the publication watch registrations, from scheduler.go; and the import
// authority, from internal/admissionauth. Start builds each but the margin
// exactly as the scenario built it there, and passes the Claude
// composition's own margin: the import authority and export recorder are
// bare, not the Claude composition's configured values.
//
// Some exports exist only for package main's tests. Those tests need
// fixtures main owns (the auth adopt command, preflight, run, main's
// configuration, the doctor command, or the seeded template store), so they
// stay in main and
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
// NativeReviewObserver, NewCredentialIntegrityProbe, NewJudgmentRoleHealth,
// ObserveCompositionBaseInputs, PendingImageRepairTask,
// ProductionCredentialIntegrityObservers, PromptPackages,
// ProveSelectedClaudeAdapter, RecordObservedAdapterConformance,
// ReportRoleFailure, RequireManualProjectsFit,
// ResolveReviewSelection, ReviewAdmission, ReviewAdmitter, RoleCausePrefix,
// ShadowReviewConfigurationDigests, ShadowReviewProvider,
// StoreConformanceRecorder, TaskRuntimeCoverageFile, UnprovableTaskStop,
// and WriterLineages, with the exported fields of ActiveResourceReconciler,
// the active-resource reconcile's result, AdapterConformanceLeaser,
// CredentialIntegrityObservers' SetupToken, Daemon (but its embedded
// shell), HeldJudgmentCredential, JudgmentCredentials, JudgmentRoleHealth,
// PromptPackages, the shadow review digests, StartSeams (but Now), and
// StoreConformanceRecorder.
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
// auth_add_test.go (authAddTestToken), auth_usage_test.go (usageManifest,
// seedUsageEnrollment, blockingUsageCollector),
// codex_model_discovery_test.go (codexModelDiscoveryTestImage,
// codexDiscoveredReport), completion_reconcile_test.go (storeRevision),
// external_review_reentry_test.go
// (externalTestMaintainerID, externalTestMaintainerLogin,
// externalTestMaintainer, activateExternalTestProfile,
// externalReentryActivity, externalReentryReconciler, reconcileClean),
// inference_test.go (judgmentFixture), judgment_credential_test.go
// (judgmentSentinelToken, judgmentSecondToken, judgmentTestToken),
// judgment_roles_test.go (judgmentTestSites), model_discovery_test.go
// (modelDiscoveryTestImage, discoveredReport), preflight_test.go
// (mustRoutedReviewProvider), prompt_packages_test.go
// (priorArtifactsTestDirective, promptPackagesFixture),
// readiness_reentry_test.go (reentryTestBase, reentryTestHead,
// reentryTestReviewID, reentryReadyItem, readReentry), rig_test.go
// (rigCommandRoots), scheduler_test.go (schedTestStore,
// schedTestResolvedPolicy, seedCaptureRun, readyBindingAuthority,
// seedReadyBindingAuthority, capturedRunNamed,
// capturedRunWithCriterion, capturedRunAtHead), setup_test.go
// (sharedSetupTestKey, setupTestKey), startup_test.go (testEvidenceTime,
// testRunEvidenceMetadata, lockedBuffer), and submission_egress_test.go
// (submissionEgressCase, submissionEgressCases, withSubmissionEgress,
// checkSubmissionEgressError, assertEgressPolicyAbsent). Main's copies sit
// in the same file names, except main_test.go for startup_test.go.
package daemoncompose
