# Prove the Cordon's Preference-Domain Denials by Asking the Sandbox

Issue #3613. Tier A (runner containment). Measured on the owner's Mac,
macOS 27.2.1.

## Decision

The cordon's conformance suite now proves the rule file's two
`user-preference-write` denials, for `com.apple.security` and
`kCFPreferencesAnyApplication`, in both the full pass and the pre-job probe.
The probe asks `sandbox_check` with the preference-domain filter whether
its own rules deny writing each domain, and the check passes only on the
sandbox's "denied" answer. The daemon's own unconfined answer for each
domain is the positive control. Inside the launch, the probe also asks
about a domain no rule names (`com.freeside.cordon-probe.unrelated`) and
fails the check if that reads as denied too. A portable test pins the
suite's domain list to the `preference-domain` names in `rules.sb`.

This closes the gap #3599's note left under Accepted Residual
(`devlog/2026-11-25-0140-cordon-securityd-lookup.md`): before, dropping
either domain rule would still have recorded a pass that declares the
class, because the suite checked only the matching preference files.

## Why a Sandbox Question, Not a Live Write

- **cfprefsd writes outside the launch.** A `defaults write` from a launch
  reaches the file through cfprefsd, which runs unconfined, so only the
  domain rule stops it, and the probe can't watch for an EPERM of its own.
- **The real domains are the owner's.** The global domain and
  `com.apple.security` are shared with the owner's session; a write that
  got through would change the owner's appearance settings or default
  keychain. The opt-in live tests keep proving the writes themselves.
- **`sandbox_check` has no side effect.** With the no-report flag it asks
  the rules and logs no violation, the same way the probe already proves
  `job-creation`, `lsopen`, and `mach-priv-task-port`.

## Measurements

`sandbox_check(getpid(), "user-preference-write", 6 | 0x40000000, name)`,
where 6 is `SANDBOX_FILTER_PREFERENCE_DOMAIN` in the private `sandbox.h`:

| Domain name | Unconfined | Both domain rules | Only the `com.apple.security` rule |
|---|---|---|---|
| `com.apple.security` | 0 | 1 | 1 |
| `kCFPreferencesAnyApplication` | 0 | 1 | 0 |
| `.GlobalPreferences` | 0 | 0 | 0 |
| an unrelated domain | 0 | 0 | 0 |

So each answer tracks its own rule, and the rule's literal name is the one
to ask about: `.GlobalPreferences` doesn't match the global rule.
`TestProbeFailsWithoutEachPreferenceDomainRule` repeats this under the
rendered rule file with each domain's rule removed in turn.

## Why the Unrelated Domain

The fresh-context review found that this is the one probe check that
rests only on the sandbox matching a name it is given. If a macOS
release answered "denied" for every domain once any domain rule exists,
dropping one of the two rules would still pass, and the unconfined control
can't see it, because unconfined answers 0 for everything. The unrelated
domain's answer catches that inside the pass itself, on whatever macOS the
host runs, not only in the darwin tests. A rule file that denies
`user-preference-write` unfiltered shows it fails both checks.

## Limits

The answer shows the rule is present and matches the name; it doesn't
watch cfprefsd refuse a write. That is the strength the operation checks
already have. A macOS release that renumbers the filter fails the
unconfined and rule-removal tests rather than passing silently.

## Revisit When

- `rules.sb` gains, drops, or renames a `preference-domain` rule. The pin
  test fails until the suite follows.
- A macOS release changes the filter number or how the global domain's
  name matches.
