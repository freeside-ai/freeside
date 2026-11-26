# Deny Cordoned Launches an Authorization Right, Not authd Itself

Issue #3611. Tier A (runner containment). Measured on the owner's Mac,
macOS 27.2.1 (26B131), from the desktop session: Xcode 27.2, Go 1.27.4,
uv 0.13.9, cargo 1.100.0, mise 2026.11.10.

## Decision

The cordon rule file denies `authorization-right-obtain`, with no filter.
A keychain add that the `~/Library/Keychains` file rule refuses now fails
at once with `The authorization was denied` (`errAuthorizationDenied`,
-60005) instead of asking authd for a login-keychain prompt. The launch
can still look up `com.apple.authd`. The suite's full pass and pre-job
probe check the operation alongside `job-creation`, `lsopen`, and
`mach-priv-task-port`.

Chose the operation over the `com.apple.authd` lookup denial because both
stop the prompt, and the operation is the narrower rule: it refuses only
the act of getting a right, while the lookup denial cuts every
Authorization Services call.

## Why

- **The prompt runs outside the cordon.** Under the rules on `main`, the
  add waited until a 10-second timeout killed it, and SecurityAgent
  started. The window server listed no SecurityAgent window while it ran,
  so whether a dialog reached the screen still isn't confirmed. A
  login-keychain prompt can offer "Reset To Defaults", which replaces the
  login keychain, and a launch that waits on it stalls.
- **authd honors the operation.** Under the operation denial, the add
  fails in 0.1 seconds with `errAuthorizationDenied`, the code authd
  returns, and SecurityAgent doesn't start. A non-interactive
  `security authorize system.preferences` changes from
  `errAuthorizationInteractionNotAllowed` (-60007) to the same denial,
  while `security authorizationdb read` still succeeds. So authd is reached
  and refuses on the client's sandbox. This is inferred from the result
  codes: the account isn't an admin, so `log stream` and `log show` were
  unavailable and the authd and sandbox logs weren't read.
- **The lookup denial works too, but costs more.** With
  `(deny mach-lookup (global-name "com.apple.authd"))` the add fails in
  0.1 seconds with `Unable to obtain authorization for this operation`,
  and `authorizationdb read` fails as well.

## Measurements

Each row ran inside a launch under each rule file, the rendered blocks
left out (they hold only file and port rules). A 10-second timeout bounded
every add.

| Inside a launch | Rules on `main` | Operation denied (kept) | authd lookup denied |
|---|---|---|---|
| `security add-generic-password` on the login keychain, named or default | **waits; SecurityAgent starts** | fails at once, authorization denied | fails at once, unable to obtain authorization |
| `security add-internet-password` on the login keychain | not run | fails at once, authorization denied | not run |
| `security authorize` without interaction | -60007 | -60005 | fails |
| `security authorizationdb read` | works | works | fails |
| Create, unlock, add to, and read a keychain in the launch's directory | works | works | not run |
| `codesign -f -s -` and `codesign --verify` on a scratch binary | works | works | works |
| `security list-keychains`, `find-certificate -a`, `find-identity` | works | works | works |
| `xcrun --find clang`, `xcrun clang`, `xcrun swiftc`, `xcodebuild -version` | works | works | works |
| `go build`, and an HTTPS GET from the built program | works | works | works |
| `uv venv` and `uv pip install six` | works | works | works |
| `cargo new` and `cargo build` | works | works | works |
| `sudo -n true`, `sudo -k; sudo true` | exec refused | exec refused | exec refused |
| mise install (`TestLiveLaunchInstallsAMiseTool`) | passes | passes | not run |

`sudo` can't run in any launch, because Seatbelt refuses to exec a setuid
program, so the denial costs it nothing. Signing with a real identity
wasn't measured: the account holds none.

## Rejected

- **Denying the authd lookup.** It stops the prompt as well, but also
  every Authorization Services read, and nothing measured needed that.
- **A live probe that adds an item to the login keychain.** The owner
  dropped it in #3599 because a regression would put a dialog on the
  owner's screen, and that still holds. The suite's sandbox check fails
  when the rule leaves the file; deleting the rule makes
  `TestFullProvesEveryDenialInARealLaunch` fail with
  `denied authorization-right-obtain unproven: the rules allow it`.

## Accepted Residual

The suite proves the rule file denies the operation, not that authd still
checks it. If a macOS release stops authd checking, the add would wait on
a prompt again and no check would notice; only a manual rerun of the add
shows it.

Only the two `security add-*` commands were measured. `security import`
into the login keychain, and `create-keychain` with a bare name, which
resolves under `~/Library/Keychains`, likely take the same path but weren't
run.

A launch may still make securityd raise a prompt of its own; this is
unmeasured. It can create a keychain in its directory, lock it, and read
an item, or add an item whose access list trusts no app and read it back.
securityd, which a launch reaches (#3599), may then ask SecurityAgent for
an unlock or access dialog. If authd checks the operation against securityd rather
than the launch, this rule doesn't refuse it. The dialog stalls the launch
and puts a prompt on the owner's screen, and the launch may name its
keychain `login`, but such a dialog offers no "Reset To Defaults" and
hands the launch nothing the owner types. It wasn't measured, because a
measurement would raise the dialog (#3649).

A tool that needs an authorization right inside a launch, such as an
installer or a signing identity whose access asks for one, now fails
instead of prompting the owner.

## Revisit When

- A macOS release changes how a refused keychain add reaches authd, or
  authd stops checking `authorization-right-obtain` against its client.
- A tool a cordoned run needs fails for want of an authorization right.
