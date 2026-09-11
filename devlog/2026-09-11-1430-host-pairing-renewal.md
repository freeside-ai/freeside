# Renew Pairing Within The Running Daemon

The owner chose one host-authorized renewal command for #1304 after an expired
startup code blocked pairing during a running acceptance attempt. Restarting
would change the process and could disrupt the workflow. An offline database
writer would also lack the running daemon's process-local pairing key.

Use the existing mint service through a private Unix socket. Kernel peer
credentials bind both ends to the same OS user on macOS and Linux. The request
names the canonical state directory, so a stale or misdirected advertisement
cannot mint on a different daemon. This implements the existing host authority
used for private startup readiness; it adds no network mint route or authority
to a paired device. Codes remain single-use with a ten-minute lifetime, and
renewal leaves workflow commands, revisions and device revocation unchanged.

Chose a short, unique socket directory advertised under the state root over
binding directly there, because macOS's Unix address limit excludes valid long
state paths. An exclusive state-directory lock prevents competing daemons from
overwriting live discovery. A new process creates a fresh socket; it does not
guess whether an old process's socket is safe to delete. Normal cleanup checks
file identity, disables automatic socket unlinking, and never recursively
deletes a directory. Unknown crash leftovers remain untouched.

The command's production control and Signet paths are exercised against a
running daemon with a parked fixture workflow. Those tests establish renewal
behavior, privacy and lifecycle boundaries. They do not establish the later
live finding, remediation, client walkthrough or Wave 7 exit.

Independent refutation confirmed an ownership-ordering defect in the first
implementation: acquiring the state lock after composition allowed another
database's daemon to reach shared driver recovery before refusal. Ownership is
now acquired before opening the store or composing state consumers. A competing
daemon test verifies refusal before database creation and continued access to
the original endpoint. Refutation also caught the existing production startup
path's ability to create a fresh state root. Startup preserves that behavior
before taking the lock; the CLI still requires an existing directory and never
creates state. The original failed-start fixture continues to reach its
post-background-start failure without changes. The reviewer found no additional
peer-authority, credential-output or replacement-cleanup defect.

Revisit when another host operation needs a supported control surface. This
single operation does not justify a general administration API or automatic
periodic minting.
