// Package cordonhost serves cordoned launches from a host launcher, a
// freesided LaunchAgent in a run account's desktop session, to a client on
// the same machine (docs/plan.md §5.7 Execution Hosts). The Client has
// *cordon.Backend's operations and its suite's two passes, so the daemon's
// gate can drive a launch behind the launcher as it drives one in process;
// the Server runs each on a real backend.
//
// # Protocol
//
// Each frame is a four-byte big-endian length and a strict-JSON body of at
// most maxFrame bytes, which carries only its kind's fields. Each
// operation has its own connection: the client's hello, the launcher's,
// the request, then the reply. A launch's tar seed follows its request,
// and record frames from the launcher's suite, each answered by a recorded
// frame, may come before the reply.
//
//   - The protocol's version is the build's. Each side decodes the
//     other's hello leniently, so it can name a different build, and
//     refuses it with ErrBuildMismatch before anything else crosses. No
//     token crosses before the launcher's hello names the client's build.
//   - The launcher's hello carries its configuration digest, its run
//     account, and its host facts: the desktop session and whether the
//     serving program holds the screen-recording or accessibility
//     approval.
//   - A launch's token crosses once, in the launch frame, and both sides
//     clear their copy of its bytes once it is wrapped or written. The
//     launcher hands it to the backend, which gives it to the agent alone
//     on its own pipe.
//   - A refusal crosses as a code, and the client rebuilds the backend's
//     error from it: ErrDetached, ErrRunEndUnproven, ErrNoRun, ErrCanceled,
//     a *BlockedError for the run requested, and a *ConformanceFailure
//     with its check and reason, joined with ErrConformanceRecord when its
//     record was not written.
//   - An end report crosses checked: the client refuses an end no backend
//     reports, and a failed end carries the transcript and stderr, each
//     bounded at ward.MaxFailureTranscriptBytes, which the client keeps
//     under its scratch root for the gate to screen. A clean exit's
//     transcript stays on the launcher.
//
// # Seeds
//
// A host holds no forge credential, so the launcher's backend has no forge
// seeder (NoForgeSeeder refuses). The client seeds each launch's base on
// the daemon's host with the daemon's own seeder, .git included, and
// streams it as a tar of directories, regular files, and symlinks, bounded
// at 1 GiB and 1<<20 entries. The launcher stages it under its seed root
// through an os.Root and seeds the run from it with the launch's own
// Seeder.
//
// # Disconnects and Records
//
// A client that leaves mid-launch, as a daemon shutting down does, leaves
// the run running: only a cancel frame cancels a launch, and a restarted
// daemon recovers the run. Every other operation ends when its client
// leaves. The suite's records cross on the connection whose operation runs
// the pass (ConnectionRecorder), found through the context the suite keeps
// values from, so a pre-job probe's withdrawal crosses on its launch's
// connection. The client checks a record against the hello's digest
// before its recorder keeps it, and the launcher fails the record's step
// when no answer comes within recordTimeout, which leaves the class
// undeclared there.
//
// # Install
//
// CheckInstall refuses to serve from an install the run account could
// change, or from a socket a run could reach. While any work runs, the
// launcher holds a power assertion that keeps the host from idle sleep.
//
// Tier A: the launcher carries a launch's token and reports the end the
// daemon's gate trusts. No export returns from a launcher yet (#3624),
// and nothing reaches one over a network (#3625).
//
// Rationale: devlog/2026-11-26-0409-cordon-host-launcher.md.
package cordonhost
