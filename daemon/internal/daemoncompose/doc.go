// Package daemoncompose builds the parts of a freesided start that package
// main composes outside the Claude driver: the scripted scenario lane
// (ComposeScenario, -driver scenario), the walking-skeleton fake lane
// (ComposeFakeWorkflow and SeedWalkingSkeleton, -driver fake), the
// startup storage phases (SeedStartupFixture and MaintainLocalBackups),
// and ProductionEngine, the one builder that turns a production
// composition's parts into engine options for both the Claude workflow
// and the scenario.
//
// The functions here build and return; they start no loop. Package main
// still owns the attention service, the Claude composition, usage
// collection, the run loops (including the one that calls ServeForge),
// and the cleanup of a start that fails after a composition returns, such
// as closing the scenario forge's listener.
//
// The scenario lane runs only under -environment ephemeral, against an
// in-process fake forge; run refuses any other environment before the
// store opens. ScenarioParts carries seven values the scenario does not
// build here. Six have their helpers in package main: the admission floor,
// the two delivery validators, the export recorder, and the agent expiry
// margin its lineup admits with are in the Tier A claude_driver.go, and the
// publication watch registrations in scheduler.go. The seventh, the import
// authority, lives in internal/admissionauth, but main still builds and
// passes it. Main builds each but the margin exactly as the scenario
// built it there, and passes the Claude composition's own margin: the
// import authority and export recorder are bare, not the Claude
// composition's configured values. When the Claude composition moves out
// of main (#2863), those inputs can become direct calls that keep them
// bare.
package daemoncompose
