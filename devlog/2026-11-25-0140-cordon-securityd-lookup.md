# Open securityd's Lookup to Cordoned Launches, Close the Keychain Preferences

Issue #3599. Tier A (runner containment). Measured on the owner's Mac,
macOS 27.2.1, mise 2026.6.1, from the desktop session.

## Decision

The cordon rule file no longer denies `mach-lookup` of
`com.apple.SecurityServer`, so mise can install tools inside a cordoned run.
It still denies `com.apple.securityd.xpc`. The `~/Library/Keychains` file
rule now keeps the login keychain closed on its own. A new rule denies
writes to `~/Library/Preferences/com.apple.security.plist*`, which holds the
account's default keychain and keychain search list, and a rule denies
`user-preference-write` of the `com.apple.security` domain, which cfprefsd
writes to the same file.

The owner chose, in session, to fold the preference rule into this unit
rather than file it as a separate unit first.

## Why

- **mise needs only `SecurityServer`.** Installing jq 1.7.1 from empty mise
  directories fails with `bad certificate format` when `SecurityServer` is
  denied, whether or not the xpc service is. It works with only the xpc
  service denied. So the xpc service stays denied: nothing measured needs
  it.
- **The file rule alone closes the keychain.** With the lookup open, a
  launch can't read an item planted in the login keychain whose access list
  trusts `/usr/bin/security`, though the same read works outside a launch.
  #3463 measured the same on an earlier macOS
  (`docs/spikes/cordoned-writer.md`, Keychain). `TestLiveLaunchRefusesSessionServices`
  keeps proving it.
- **Opening the lookup opened a credential path, which the preference rule
  closes.** With the lookup open, a launch can create an unlocked keychain in
  its own directory and make it the account's default. A
  `security add-generic-password` run outside the launch then saved into
  that keychain without a prompt, so the owner's next saved credential would
  go to a keychain whose password the launch chose. The setting lives in
  `com.apple.security.plist`, which two routes write. Security.framework
  writes it itself for `security default-keychain` and `list-keychains`, and
  only the file rule stops that; the domain rule alone changed nothing.
  cfprefsd writes it, outside the launch, for `defaults write
  com.apple.security`, and only the domain rule stops that: a test-only key
  landed under the file rule alone. The fresh-context review found the
  second route. `TestLiveLaunchCantChangeTheDefaultKeychain` covers both.

## Measurements

Each operation was run inside a launch under each rule file. Lock-all wasn't
run, because it would lock the shared login keychain that `gh` and other
sessions on the account read. Locking a named throwaway keychain stands in
for it.

| Operation inside a launch | Rules before #3599 | Lookup open, no preference rule | Rules after #3599 |
|---|---|---|---|
| Create a keychain outside `~/Library/Keychains` | refused | works | works; not made the default or added to the search list |
| Lock a named keychain | refused | works | works (not rerun; the preference rule doesn't affect it) |
| Set the user default keychain | **changes it** when the named file exists, though `security` reports an error | works | doesn't take effect |
| Set the user search list | **changes it** | works | doesn't take effect |
| The owner's next save lands in the launch's keychain | no: the launch can't create an unlocked one | **yes, without a prompt** | no |

Only the user domain changes: `security` calls `dynamic` invalid, and the
`common` and `system` settings stayed the same when a launch tried them.

The rules before this unit already let a launch change the default keychain
and search list. Pointing the default at a keychain file the launch wrote
would make the owner's next save prompt for a password the owner doesn't
know, and the search list could hide the login keychain. The preference rule
closes that too.

## Rejected

- **Opening both lookups.** That's what the shared-host spike (#3596) did,
  but mise doesn't need the xpc service.
- **Denying all of `com.apple.security.*`.** Other daemons write files under
  that prefix, and nothing measured needed a broader rule.
- **A live probe that adds an item to the login keychain.** Inside a launch,
  the add asks authd for a login-keychain prompt (`makeLoginAuthUI`) and
  waits for an answer, both before and after this unit, so each live run
  could put a dialog on the owner's screen. The owner chose to drop the
  probe. The rule that refuses the read refuses writes to the same
  directory, and the suite's full pass probes its reads.

## Accepted Residual

A launch can now ask securityd to lock keychains, and locking all of them
locks the owner's login keychain, so `gh`, the daemon, and the owner's apps
prompt or fail until the owner unlocks it. That costs availability, not a
credential, and the only rule that refuses it is the lookup denial mise
can't run under. This wasn't run on the shared account; locking a named
throwaway keychain worked.

A launch can also reach any unlocked keychain outside `~/Library/Keychains`
through securityd, the System keychain among them, and decrypt items whose
access list admits any app. On the measuring host, the only such System
keychain entries were two public keys; every password item names its
applications. Another host's admin-installed tools may differ. Denying
reads of `/Library/Keychains` would cost the root certificates TLS
clients trust.

The suite's full pass checks the preference file, not the domain rule;
only the live test shows the domain rule works (#3613).

Follow-ups: #3611, #3613 (the authd prompt a launch can raise).

## Revisit When

- A macOS release moves the default keychain or search list out of
  `com.apple.security.plist`, or makes Security.framework write it through
  cfprefsd. The live test then fails.
- A tool a cordoned run needs fails without `com.apple.securityd.xpc`.
