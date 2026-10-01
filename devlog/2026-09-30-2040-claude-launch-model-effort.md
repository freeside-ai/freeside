# Pass the Agent's Model and Effort to the Claude Ward Launch

Work unit #1618. `claudeProvider.HandoffSpec` now turns
`StartSpec.RouteModelID` and `StartSpec.NativeEffort` (#1615) into `--model`
and `--effort` on the `claude -p` invocation. This is returned-object
trust-boundary work: both values are decoded from a stored admission and
become argv of a shell that starts as root.

## Chose a Private Copy of the CLI-Safety Rule

`launchFlagValueSafe` in `daemon/internal/exec/claude` repeats ward's
`cliSafe` rule (no comma, no control character) for a non-empty value.

- **Rejected: exporting `ward.cliSafe`.** The issue contract keeps
  `daemon/internal/ward` and `daemon/internal/exec/stage` out of scope, and
  the stage driver already keeps its own copy (`cliSafeMountField`).
- **Cost: three copies.** `ward.cliSafe`, `stage.cliSafeMountField`, and
  `launchFlagValueSafe` must change together.
- **Empty is not refused.** It means the flag isn't passed, which is how the
  native-default offer at `harness_default` launches.

## Chose to Pass the Spec's Values Without a Recheck

The launcher passes what the spec carries. It doesn't translate the effort,
special-case `claude-code-native-default`, or recompute the values from the
agent: `domain.DeriveAgentLaunchSelection` does the first two before a
binding is written, and #1648 owns the recheck. Until #1648 lands, a stored
binding that disagrees with its agent reaches argv with only the CLI-safety
rule and shell quoting in the way.

## Chose the Flag Position by Byte Identity

The flags sit directly before the transcript redirect, so an empty selection
leaves the command string unchanged. Four golden files captured from the
code before the change pin that for both prompt deliveries, with and without
the preparation command. Recovery rebuilds a running launch's command and
ward compares it with the recorded spec, so a drifted command strands the
launch as retryable forever.

## Refute-First Findings

An independent reviewer tried to break the change, with mutants, about 180
hostile values run through four shells, and two million fuzz cases.

- **Confirmed, accepted: a non-empty selection journaled by an older build
  can't be recovered by this one.** The fields exist on `main` since #1615
  and the launch ignored them, so such a launch recorded a flagless command;
  this build rebuilds it with flags and the comparison fails. The same holds
  for a running launch whose value the new refusal rejects. Unreachable
  today: no production code writes an agent binding, so every spec carries
  neither value. It becomes real only if a binding writer (#867) ships
  before this change.
- **Confirmed, accepted: no length bound on the model.** The rule in the
  contract is `cliSafe` only. A model id of tens of kilobytes would exceed
  the Linux single-argument limit and fail at exec, after the intent is
  saved. The id is operator-authored in an offer.
- **Confirmed, fixed: the control-character tests were thin.** A rule that
  refused only comma, newline, and `0x7f` passed. The refusal table now
  covers `0x00`, tab, newline, carriage return, `0x1f`, and `0x7f` for both
  fields.
- **Accepted: a leading `-` passes the rule.** On the local Claude Code
  2.1.286, `claude -p --model --version` and `claude -p --effort --version`
  both consumed the dash-led word as the flag's value, not as a flag. The
  pinned 2.1.220 was not run.
- **Disproved:** a value can leave its quoted word or alter the redirect
  (argv tail was always exactly the two flags and their values); `%` in a
  value is read as a format verb (the value is a `Sprintf` argument); an
  invalid UTF-8 byte hides a control character from the rune loop; the
  private rule differs from `cliSafe` for some non-empty input; a refusal
  leaves a record (`handoffSpec` returns before `saveIntent`); the tests
  pass when the flags are dropped, misplaced, unquoted, swapped, or emitted
  when empty.
- **Not checked:** the pinned image's CLI, `setpriv`, and a real container.

## Revisit When

- #867 is about to merge before this change: a selection journaled in
  between can't be recovered (first finding).
- A new start can use argument delivery again. The argument protocol's
  worst-case prompt already leaves one byte under the 128 KiB limit with a
  preparation command, so any flag would overflow it. Today only
  reconstructed legacy starts use it, and they carry no selection.
- The rule changes in `ward.cliSafe` or `stage.cliSafeMountField`.
