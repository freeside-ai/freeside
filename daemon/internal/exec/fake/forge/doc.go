// Package forge is a loopback double of the GitHub surface the publisher
// uses, with the local-git publication transport that pushes to it. The
// integration tests script it to drive publication through every recovery
// path; the ephemeral scenario daemon serves it in-process so a scripted run
// publishes a pull request without reaching GitHub.
//
// The doubles' state is exported on purpose. Tests set faults and inspect
// effects directly, so a field like Forge.PRs is read and written under
// Forge.Mu by the caller, the way the handler itself does. The double is
// not a contract: it answers exactly the requests the publisher makes today
// and reports anything else through Config.Report.
//
// Layout:
//
//   - forge.go      Forge, its pull requests, the token source and auditor
//   - transport.go  Transport, the checkout it seals, and its re-entry fetches
package forge
