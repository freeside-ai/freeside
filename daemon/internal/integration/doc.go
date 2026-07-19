// Package integration holds cross-package composition tests that exercise
// seams no single package owns. The first is the candidate path feeding the
// publication gate (#168, plan §5.6): the gauntlet importer detects
// publish-blocking findings, those lift into the daemon-authored candidate
// authorization, and the publisher must refuse a candidate that authorization
// does not permit — before any branch or pull request exists.
//
// It carries no production code; the tests live in integration_test, so the
// daemon's real packages never depend on this one.
package integration
