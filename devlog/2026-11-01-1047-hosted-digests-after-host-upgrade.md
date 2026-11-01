# Hosted Digests After the Host Upgrade

Owner decision, 2026-11-01, on #2456. The development Mac moved to macOS
27.2 on 2026-10-31, and the recording guard accepts only `macOS-26.7`, so
the unit could not record its digests. All 1,631 keys mismatch on 27.2.

## Decisions

- **Copied the digests from the hosted runner, for this PR only.** Chose to
  take the new values from the `macos-26` runner's mismatch output on the PR
  over recording on a 26.7 Mac, which would leave the PR red until the owner
  found one, and over re-pinning the baseline first, which would hold a
  string change behind a rewrite of every digest and a CI image change. The
  runner matched the manifest on every key the PR did not move, and each key
  that moved draws a changed string or the list, so its output was the same
  pixels a 26.7 recording would be expected to give. That is an inference:
  no 26.7 host confirmed it.
- **Drew Needs You with no head.** The owner chose to remove the head and its
  count over keeping them for parity with Working and Finished, because those
  heads exist to fold and Needs You never folds. The menu bar keeps the count.
  The name stays, capitalized, on the summary eyebrow and `Show Needs You`.
  With no head on the list, the guidance links name no destination and the
  Settings sentence says `needs you` in plain words. The digests those changes
  moved came from the runner too, under the same exception.
- **Left the guard alone.** This is an exception to `app/README.md`, which
  says baseline recording needs the designated local macOS version. It sets
  no precedent for the next unit, and the recording guard and the hosted
  refusal stay as they are.
- **Rejected a hosted override.** `ScreenshotDigestOverrides.json` would
  also have turned the check green, but it would describe a real text change
  as a host difference and leave the manifest stale.

Revisit when #2460 restores a recording host. A full recording there may
move some of these keys, which would disprove the inference above.

Follow-up: #2460
