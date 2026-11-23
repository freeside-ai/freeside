# A Cordoned Writer Gets Its Token on Its Own Descriptor

`internal/cordon` now takes the enrolled identity's Claude setup token on
`LaunchRequest.Token` and hands it to the agent CLI alone: the bytes on a
pipe at fd 3, named by `CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR=3`, with the
write end closed before the CLI starts. The canary gets nothing. A request
with no token is refused before the launch claims its directory. Issue
#3469, tracker #3462, under the plan comment on #3469 and the evidence in
`docs/spikes/cordoned-writer.md` (Token and Config). Nothing calls the
launch yet; #3472 composes the daemon's read with it.

## Chose a Descriptor Over the Other Handoffs

The CLI reads the descriptor to end of file, and a tool it runs finds the
token in no environment of its own and on no inherited descriptor (#3463).

Rejected:

- **`CLAUDE_CODE_OAUTH_TOKEN` in the CLI's environment, the ward's way.**
  The CLI strips it from its tools' environment, but its own environment
  is the one it started with, and a same-user tool reads that with
  `sysctl(KERN_PROCARGS2)`, the call `ps eww` makes. No Seatbelt rule
  hides it without breaking the CLI or `go version` (#3463).
- **A file under `RUN_ROOT`.** The rules reopen `RUN_ROOT` to the launch,
  so every tool could read it.
- **The CLI's stdin.** It carries the prompt, and `-p` appends stdin to the
  prompt, so the transcript would record the token; the spike's one run
  that did this never authenticated and leaked it into its transcript.
- **`CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1`.** It drops
  `--dangerously-skip-permissions` to `default` mode, so every tool call
  waits for approval, which a writer can't run under.
- **A proxy that substitutes the token.** That is `subscription_injected`,
  which stays held (plan §5.4, revision 103); a cordoned writer runs under
  `subscription_contained` and holds its own token (revision 134).

The pipe is written before the CLI starts. A token is at most
`ward.MaxSetupTokenBytes` (4096) and a macOS pipe buffers at least 16
KiB, so the write can't block, and closing the write end first means
nothing but the CLI's read ever holds the pipe open. `os.Pipe` sets
close-on-exec, so only the agent inherits the read end. The daemon clears
its revealed copy once the agent starts; the request's own
`ward.SetupToken` renders as a placeholder under every `fmt` verb.

## Chose to Hold the Store Only for the Read

The daemon reads the token with the existing `wardstore.CredentialReader`,
under the identity's shared read hold, for the generation admission named.
The hold ends when the read does. The run keeps its own copy, in the CLI's
memory, so a renewal can replace the store while a run is under way; the
run keeps the old generation's token until it ends, as a ward run keeps
its mounted copy. Holding the store for the run's length would block
renewal for hours for no protection the copy doesn't already lose. If a
recovered run is relaunched rather than adopted (#3468), the relaunch reads
again.

## The Pinned-CLI Behaviors This Path Relies On

Observed on `@anthropic-ai/claude-code@2.1.319` (`images/agent-claude/`),
not documented by the vendor. The cordon's credential live suite,
`TestLiveCredentialLaunchAuthenticates`, re-proves each one on every CLI
version bump (`daemon/README.md`, Cordoned Launches):

- **It reads the token from the descriptor
  `CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR` names** and authenticates with
  it, with the owner's Claude config and the keychain denied, so no other
  credential is in reach.
- **It keeps the token out of its tools' environment and descriptors:** a
  tool the agent runs finds it in no environment of its own process chain
  or of any same-user process, and on none of fds 3 to 9. The tool runs
  after the CLI has read the descriptor to end of file, so this shows what
  a tool started then can reach; a child the CLI starts before its read is
  outside what the suite tests, and stays under the same rules.
- **It caches no token in `CLAUDE_CONFIG_DIR`,** nor writes the token
  anywhere under the launch's directory, nor adds, removes, or rewrites a
  login keychain item whose service starts with `Claude Code`.
- **Its hardened runtime, without `get-task-allow`, stops a same-user tool
  attaching to it** and reading its memory. The live suite doesn't probe
  this; it is the binary's signature (`codesign -dv` shows
  `flags=0x10000(runtime)`), checked by hand on a bump.

If a bump breaks one, the suite fails; the fix is to stop and replan, never
to fall back to the environment.

## Residuals

- **The CLI process holds the token in memory** for the run, as in a ward.
- **Other processes' environments stay readable.** A tool reads the
  environment of any process on the account, so a secret another session
  keeps in its environment, such as another agent's token, is exposed to
  the run. The spike found another Claude session's token that way. No
  rule closes it; #3550 moves the daemon's own children off environment
  secrets, and plan §14 records the residual the owner accepted
  (revision 134).

## Refute-First Findings

One fresh-context reviewer tried to prove the change wrong.

- **A concurrent fork inherits an end of the pipe: disproved.** On darwin
  `os.Pipe` sets close-on-exec under `syscall.ForkLock`, and the daemon
  closes both of its ends around `cmd.Start`.
- **Clearing the revealed copy wipes the request's token: disproved.**
  `Reveal` returns a clone, and the pipe is written before `start`
  returns.
- **A live-suite failure prints the token: disproved.** Every print of
  the transcript or the probe's output follows an exact search for the
  token, and the probe sees only its length and digest.
- **The keychain check missed a derived or rewritten item: confirmed,
  fixed.** It now compares every `Claude Code` item's service name and
  modification time.
- **The probe can't tell a closed fd 3 from a drained one: confirmed,
  allowed.** The claim above is narrowed to what a tool started after
  the read can reach, and the probe logs which of fds 3 to 9 are open.

Revisit when the vendor documents a credential handoff, the pinned CLI
stops honoring the descriptor, or macOS gains a rule that hides another
process's environment without breaking the CLI.
