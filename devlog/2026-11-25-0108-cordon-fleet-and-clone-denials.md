# The Cordon Denies the Fleet's Signing Files and the Agent Prompt Clones

Issue #3603 closes two holes the shared-host spike (#3596,
`docs/spikes/cordoned-shared-host.md`) found on the Mac mini: a cordoned
launch could open the fleet's code-signing key, keychain, keychain password,
and `sign` script under `~/.config/fleet`, and could write in place to the
prompt and skill clones that `~/.claude` and `~/.codex` link into. The owner
handed over the issue's contract and its implementation plan with
`Handle #3603`; the choices below are the plan's, kept unchanged.

## Chose Fixed Home-Relative Rules Over a Configurable List

`rules.sb` denies `~/.config/fleet` reading and writing, in the credential
block beside `~/.config/gcloud`, and denies writes under
`~/proj/freeasinbird/free-prompts` and `~/proj/freeasinbird/free-skills`.
All three are `(param "HOME")` paths, so `renameGuards` guards the
directories above them and `TestSecretPathsAreTheRuleFilesHomePaths` makes
the suite list each one, with no new `Config` field, `-D` parameter, or
daemon flag.

Rejected:

- **A configurable list of extra denied paths.** Two fixed entries don't
  justify a new field and flag, and a fixed rule can't be left unset on a
  host that needs it. Revisit when a third host-specific path appears.
- **Writing the clone rule against `PROJECTS_RE`.** It would tie the rule to
  `-cordon-projects`, so a host whose projects directory is elsewhere would
  lose it, and the pin test and rename guards read only home paths.

## Chose the Key's Whole Directory, Read and Write

The one subpath covers the key, the keychain and its password, and the
`sign` script. With the key a run can sign any binary as the fleet
identity, and under #3602 that identity carries the host's Screen
Recording and Accessibility approvals, so a run could mint a program that
inherits them. Revisit when the fleet moves its signing files out of
`~/.config/fleet`.

## Left the Clones Readable and Denied Writes Unconditionally

The clones are public and hold no secret, so reads stay open. Writes are
denied whether or not the fleet repository has made the clones read-only to
the account: that can't be checked from here, and a write denial on a clone
that's already read-only costs nothing. The existing unlink denial under
the projects directory already keeps them from being moved or removed.
Making the clones read-only stays the fleet repository's own fix.

## The Suite Probes Only What the Owner Has

The suite read-probes `~/.config/fleet` as it does every secret path, and
create-probes a new file in the fleet directory and in each clone only when
it exists and the account can write it. Creating the owner's fleet
directory or a clone isn't the suite's business, and the read probe already
covers an absent fleet directory. The create probe follows a symbolic link
(`os.Stat`, where the protected roots use `os.Lstat`): Seatbelt matches the
resolved path, so a clone that links elsewhere escapes its rule, and probing
through the link fails the pass instead of leaving the clone unprobed.

## The Rule-File Change Invalidates Every Host's Recorded Pass

Any change to `rules.sb` changes `RuleDigest`, so each host's recorded
conformance pass stops counting and cordoned work waits until the suite
passes again there (#3470). That is the intended cost of a rule change.

## Refute-First Findings

- **Confirmed and fixed: a symlinked clone went unprobed.** The first draft
  checked each place with `os.Lstat`, so a clone that is itself a link was
  skipped while its rule matched nothing. A scratch run that made the
  fixture's clone a link failed the real-launch test once the probe used
  `os.Stat`.
- **Disproved by a check: a new probe passes without its rule.** Removing the
  fleet line, or renaming the skill clone's rule, made
  `TestFullProvesEveryDenialInARealLaunch` fail on that path's read or
  create check.
- **Disproved by a check: a hard link writes through the clone rule.** The
  reviewer's scratch launch showed the write denial refuses `ln` from a
  clone file.
- **Allowed: a failed pass can leave its probe file in a clone** under the
  projects directory, since the unlink denial there refuses the cleanup. It
  happens only on a pass that already failed and names the path.
- **Allowed: no test lists the new create targets.** The real-launch test
  can't tell a dropped probe from a passing one; no probe target has such a
  list test, and the removal runs above show today's probes run.
- **Allowed: a host whose clones live elsewhere gets no write denial**, and
  its pass still conforms. That is the fixed-path trade-off above.
