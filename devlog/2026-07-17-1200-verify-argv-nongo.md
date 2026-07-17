# Non-Go verification recipes: argv wire form and malformed-tree hardening

Work unit for #140 (gauntlet): extend the clean verifier (#137) so
verification recipes can run non-Go toolchains (the app lane's
`swift test` / `xcodebuild`), and close the malformed-tree /
path-cleaning items deferred from #137's Go-first scope.

## Decisions

- **Chose an explicit argv-array wire form over the whitespace-split
  string form** (`{"commands": [["go","test","./..."]]}`), removing
  `strings.Fields` and the shell-metacharacter rejection. Rationale: the
  documented iOS check needs `-destination 'generic/platform=iOS
  Simulator'` as one argv element, which a space-splitting form cannot
  express. Arguments are now opaque: the runner passes each element to
  execve verbatim, never to a shell, so spaces and metacharacters carry
  no chaining/substitution/redirection meaning and need no rejection.
  (Decider: #140 objective, owner-filed.)

- **Kept two fail-closed parser guards despite "opaque"**, because a
  trusted recipe can still be malformed or adversarial:
  - **NUL byte** in any token is rejected at parse. A NUL cannot cross
    execve and would otherwise surface as an opaque runtime error; this
    is the one deliberate deviation from pure opacity.
  - **`null` token** is rejected at parse. Tokens decode as `*string`,
    not `string`, so the parser distinguishes a JSON `null` (nil) from an
    intentional empty string; `json` folds `null` into the zero string,
    which would let `["swift","test",null]` masquerade as a valid empty
    argument. (Codex P2, PR #147.)
  - **`..` path segment** in any token is rejected at parse (`ErrRecipeInvalid`),
    chosen over silently skipping it in `repoRelPath`. A skipped token
    would drop out of `CommandPaths` and bypass the symlink-entrypoint
    guard *while the OS still executed the path* (`./link/../verify.sh`
    with a symlinked `link` runs a different file than `path.Clean`
    computes) — a fail-open hole. Hard rejection is the fail-closed
    choice. The segment check is exactly `path.Clean`'s own `..`
    recognition, so it covers Clean's entire traversal domain and leaves
    real filenames (`a..b`, `..bar`) alone.

- **Recipe digest binds to raw bytes, so the wire-form change alters
  every recipe's digest.** Acceptable now: nothing consumes recipes yet
  (integration is a separate unit, #140 non-goal), so no approved digest
  is invalidated in practice. Pairs naturally with the still-open
  provisional-format revisit (JSON vs the plan's §5.12 YAML), unchanged
  by this unit.

- **Transcript quotes any argv token that is not a shell-safe literal**
  (`strconv.Quote` unless every rune is in the shlex-safe word set
  `[A-Za-z0-9@%+=:,./_-]`), since with opaque arguments a bare space-join
  is ambiguous and a raw metacharacter or control char could make an
  evidence line read as a pipeline or inject a terminal-control sequence.
  The transcript is a human-readable account, never re-parsed; clean
  tokens render unchanged (the transcript golden did not move). Widened
  from a whitespace-only check to the full safe-set after Codex P2
  (round 5, PR #147).

- **Added the full documented SwiftPM/Xcode verification-control surface
  to the mandatory `DefaultVerificationControlPatterns`** via the
  existing glob machinery, so an app-lane candidate rerouting what
  `swift test` / `xcodebuild` compiles, fetches, or runs is flagged
  exactly as a swapped `go.mod` or `Makefile` is: `Package.swift`,
  `Package@swift-*.swift` (version-specific manifest SwiftPM prefers),
  `Package.resolved`, `.swiftpm/configuration/mirrors.json` and
  `registries.json` (dependency-source redirects, the go.mod-replace
  analog), `*.xcconfig` (build settings), `*.pbxproj`, `*.xcscheme`,
  `*.xctestplan` (test selection), and `contents.xcworkspacedata`
  (workspace membership). Enumerated the whole class up front (research
  against Apple/swift.org docs) rather than adding one per Codex round;
  `Info.plist` and `*.xcfilelist` are deliberately excluded (the real
  wiring lives in the flagged pbxproj/xcconfig, and globbing plists would
  flag every target). Over-inclusive by design: a control flag fails
  closed by over-flagging, so the auto-generated `contents.xcworkspacedata`
  UI copies are flagged too. (Codex serialized two of these as P2s on
  PR #147: `Package@swift-*.swift`, then `*.xctestplan`.)

- **Malformed-tree hardening in `listTree`** (before any write): reject
  a tree path with an empty/absolute/`.`/`..` component, and reject a
  duplicate path across entries and gitlinks. `git write-tree` never
  emits these; a tree crafted with `hash-object -t tree --literally`
  can. This closes the P1 lexical-escape and the P2 exact-string
  duplicate-disagreement items from #140's comment.

## Refute-first verification ledger

Two independent adversarial lenses, each tasked to *disprove* the
guards, given only the diff + intent.

- **Recipe / entrypoint lens — no bypass; guards hold.** `path.Clean`
  removes a named component only via its `..` rules, which are
  pre-rejected, so the symlink guard checks exactly the components the OS
  traverses. `validateCommand` runs on every token in the one mandatory
  `ParseRecipe` that precedes `CommandPaths` and the runner; no unparsed
  path reaches exec. No-shell exec makes metacharacters inert. Two
  out-of-scope latent notes, neither introduced here and both requiring a
  *trusted* recipe author: (a) a raw control char (ESC) in a token
  renders into the transcript, but candidate output is already written
  raw, so argv rendering is not a new injection surface; (b) the
  symlink-entrypoint guard's case-sensitivity vs a case-insensitive FS
  is pre-existing and orthogonal to the `..` fix.

- **Malformed-tree lens — one CONFIRMED bypass, pre-existing and out of
  scope; deferred to #145.** On a **case-insensitive** workspace FS
  (macOS APFS default), a crafted tree pairing symlink `link` (120000,
  out-of-tree target) with blob `LINK/pwned` (100644) escapes the
  workspace: the exact-string `seen` and `rejectPrefixConflicts`
  compare case-sensitively, but `os.MkdirAll(dest/LINK)` traverses the
  materialized `dest/link` symlink and `os.WriteFile` lands bytes
  outside `dest` (no `O_NOFOLLOW`). Reproduced. **Not introduced by this
  unit** — this diff's lexical `filepath.Join` claim holds and its
  exact-string duplicate check closes the case-*sensitive* variant; the
  case-folded collision is a distinct, deeper class (FS normalization,
  not lexical). A partial ASCII-case fold would give false assurance
  (APFS also Unicode-normalizes); the robust fix is no-symlink-traversal
  writes. Accepted-by-decision: **deferred to #145**, filed with the PoC
  and fix direction, rather than ballooning this security-sensitive PR
  into cross-platform materialization hardening. The verifier still
  fails the run closed via `verifyMaterialized`/`ErrWorkspaceMismatch`
  afterward, but only after the host write lands — hence #145 is real.

## Codex review (PR #147)

- **Null argv tokens (P2): fixed** in the argv-wire-form commit (see the
  parser-guards decision above).
- **Version-specific Swift manifest evasion (P2, round 2) and Xcode
  test-plan evasion (P2, round 3): fixed** — rather than add one pattern
  per round, enumerated and added the full SwiftPM/Xcode control surface
  (see the Swift/Xcode decision above), closing the class in one push.
- **Transcript renders unsafe tokens raw (P2, round 5): fixed** — widened
  the quoting predicate from whitespace-only to the full shell-safe set
  (see the transcript decision above).
- **`CommandPaths` lexical-extraction imprecision (two P2s, rounds 1 and
  4): deferred to #149.** Both are the same root cause — `CommandPaths`
  records any `/`-bearing token verbatim as a repo path even when the
  token is not a plain path:
  - *Non-file operand*: `xcodebuild -destination
    generic/platform=iOS Simulator` yields command path
    `generic/platform=iOS Simulator`, so a repo with a top-level
    `generic` symlink would fail `rejectSymlinkEntrypoints` closed (an
    over-flag, safe direction).
  - *Shell-runner string*: `sh -c "./scripts/verify.sh --fast"` records
    `scripts/verify.sh --fast`, so a candidate edit to the real
    `scripts/verify.sh` is not flagged (an under-flag) and the guard
    checks a nonexistent path.
  Declined-in-PR because: (a) the operand case is pre-existing since
  #137 (`go build ./cmd/foo` already classifies `cmd/foo`), not a #140
  regression; (b) neither is reachable for any current recipe (first
  repo `go test`/`go vet`; app lane `swift test`/`xcodebuild` — none use
  a `sh -c` wrapper), so both are latent; and (c) the correct fix is
  architectural — precise file-vs-operand classification must not reopen
  the symlink-*prefix* hole (`TestVerifyRejectsSymlinkPrefixEntrypoint`),
  and the shell case additionally needs a design choice (reject `sh -c`
  forms vs. shell-parse). That is its own unit with a refute pass, not a
  heuristic folded mid-review into a security PR.

## Revisit when

- The control-plane config format initializes (`policy/`, plan §5.12):
  the JSON wire form and its raw-byte digest revisit together.
- #145 lands: the case-folding/symlink-traversal escape is the residual
  of the malformed-tree class this unit only partly closed.

Follow-up: #145, #149
