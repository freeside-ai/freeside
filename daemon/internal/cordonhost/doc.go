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
// the request, then the reply. A launch's or a check's seed follows its
// request, a clean run's export follows the reply that reports it, each a
// tree stream (Seeds, Exports), and record frames from the launcher's
// suite, each answered by a recorded frame, may come before the reply.
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
//   - A check crosses as a launch does, with no token. Its end carries the
//     tail of the command's output, the command's own untrusted bytes, at
//     most the request's bound, which is at most MaxCheckOutputBytes so it
//     fits the end's one frame. A check's recovery never exports.
//   - A refusal crosses as a code, and the client rebuilds the backend's
//     error from it: ErrDetached, ErrRunEndUnproven, ErrNoRun, ErrCanceled,
//     ErrWorktreeReplaced, export.ErrLinkedFile, ErrNotExportable,
//     ErrNoEndRecord, a *ScanRefusalError with a reason of the client's
//     own (the host's names an item of the export), a *BlockedError for
//     the run requested, and a *ConformanceFailure with its check and
//     reason, joined with ErrConformanceRecord when its record was not
//     written. So the daemon's gate classes a host's refusal as it
//     classes a local one.
//   - An end report crosses checked: the client refuses an end no backend
//     reports, and a failed end carries the transcript and stderr, each
//     bounded at ward.MaxFailureTranscriptBytes, which the client keeps
//     under its scratch root for the gate to screen. A clean exit's
//     transcript stays on the launcher, and reaches the daemon only as
//     the export's evidence.
//
// # Seeds
//
// A host holds no forge credential, so the launcher's backend has no forge
// seeder (NoForgeSeeder refuses). The client seeds each launch's base on
// the daemon's host with the daemon's own seeder, .git included, and
// streams it as a tar of directories, regular files, and symlinks, bounded
// at 1 GiB and 1<<20 entries. The launcher stages it under its seed root
// through an os.Root and seeds the run from it with the launch's own
// Seeder. A sparse entry, which would extract bytes the stream never
// carried, is refused in any tree.
//
// # Exports
//
// Export and a recovery that ends exported return the run's export over
// the connection. The launcher runs the backend's own Export, which
// refuses a run with no proven clean end before it reads the worktree,
// and streams the export directory after the reply. One run's export and
// recovery are serialized on the launcher, so a recovery never removes
// an export another connection is still writing.
//
// The client receives the stream as untrusted input (receipt.go), as the
// ward's daemon side receives an exporter VM's archive: under its bounds
// of 4 GiB streamed, 2 GiB extracted, and 10,000 entries, through an
// os.Root into
// <ExportRoot>/<runID>/.receiving, holding only directories and regular
// files. It then validates the manifests, bounded at 64 MiB, runs the
// export credential scan, makes the tree durable, renames it to export/,
// and writes a receipt record binding the run, the base commit, and the
// three member digests. A refused or cut receipt is removed. A refusal
// for what the export holds is ErrExportRefused, which the daemon's gate
// ends the run refused on, since asking again brings the same bytes; a
// cut stream is asked for again. A receipt with a record is returned
// without asking the launcher, so a restarted daemon finds it. AuthenticateReleasedExport
// checks the receipt record alone and never asks the launcher.
// ReleaseFinished removes the launcher's run directory and the receipt.
// The client removes only the names a receipt puts in a run's directory,
// and refuses a directory that holds any other.
//
// # Disconnects and Records
//
// A client that leaves mid-launch or mid-check, as a daemon shutting down
// does, leaves the run running: only a cancel frame cancels a launch or a
// check, and a restarted daemon recovers the run. Every other operation ends when its client
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
// daemon's gate trusts, and the client admits the export the importer
// reads. Nothing reaches a launcher over a network yet (#3625).
//
// Rationale: devlog/2026-11-26-0409-cordon-host-launcher.md and
// devlog/2026-11-26-0848-cordon-host-export.md.
package cordonhost
