# LaunchAgent Arg-Placeholder Templating Fix (#762)

## Decisions

**Chose `plutil -replace ProgramArguments -json '[…]'` (whole-array
rewrite) over per-index `-replace ProgramArguments.<N>`.** Local repro
confirmed `plutil -replace ProgramArguments.<N>` *inserts* at the index
instead of overwriting it, shifting each placeholder down and shipping a
doubled 9-element vector with `__FREESIDE_STATE_DIR__` still literal, so
nothing bound the fixed 127.0.0.1:7331 listener. A position-independent
whole-array replace has no index bookkeeping to get wrong. Rejected:
per-index `-remove`+`-insert` (also works, keeps the fragile index
math); distinct tokens + `sed` text substitution (brittle against paths
containing the delimiter). Tradeoff accepted: the arg structure now
lives in the script, coupled to the bundle plist template, held honest
by the install-time placeholder guard.

**JSON-encoded the dynamic state paths before the `-json` fragment.**
`plutil -json` parses its argument as JSON, so a raw pathname bearing a
backslash followed by a JSON escape letter (a legal `…\troot`) decodes
to a different byte string (a tab) and binds a path that is not the
created directory, and an embedded double quote could inject arguments;
raw interpolation does not fail closed on those bytes. A `json_string`
helper is a complete JSON string encoder (backslash, double quote, and
the control characters `-json` rejects raw, which the prior per-index
`-string` op tolerated) so the bound path stays byte-identical to the
created directory; it iterates byte-wise so multibyte UTF-8 passes
through. Verified byte-exact through real plutil for newline, tab, CR,
0x1F, backslash, quote, `&`/`<`/`>`, and UTF-8 inputs. The install-time guard reads
the bound arguments back with `plutil -extract ProgramArguments.N raw`
and compares each byte-for-byte (plus a trailing-index bound so a
doubled vector is still caught), rather than an `__FREESIDE_` prefix
grep or an xml1 re-extraction: a prefix grep aborts a legal home path
containing that substring, and xml1 hands `&`/`<`/`>` back escaped, so
both false-abort a valid install; raw comparison is metacharacter-
agnostic. The test stub models raw indexed extraction, and cases pin
backslash, `__FREESIDE_`, and XML-metacharacter state paths.

**Kept the daemon plist token unchanged and did not touch
`app/Apps/macOS/LaunchAgents/ai.freeside.daemon.plist`.** The `-json`
rewrite replaces the whole array, so the db token's exact spelling no
longer matters; no shipped-plist edit was needed.

**Chose a faithful test stub over the plan's preferred "drop the stub,
run real plutil" — because real plutil is not viable in this harness.**
`scripts/test-install-mac-app.sh` writes synthetic `Info.plist` files
containing the literal `fixture\n`, which are not valid plists; the
install script reads `CFBundleIdentifier` from them via `plutil
-extract`, so real plutil would fail those extractions and break
unrelated recovery/guard cases. Instead the stub now models the exact
operations the fixed script performs (`-replace ProgramArguments -json`,
the guard's `-extract`) and rejects the index `-replace` form, so a
regression to it fails loudly (installer `die`) rather than passing
green-but-blind. The old stub's index-based token substitution mis-
modeled real plutil's insert semantics, which is why the harness stayed
green while real installs shipped placeholders; that gap is what this
closes. The install-script guard is the production backstop that runs
against real plutil on every install.

## Verification Findings

- Local repro reproduced the issue's exact 9-element vector, then
  confirmed the `-json` fix yields a clean 7-element vector (paths with
  spaces handled).
- Negative test: a mutated installer that reintroduces the index form
  reddens the harness; the install-script guard fires on a surviving
  `__FREESIDE_` token.
- CI-blind: the real install (SMAppService registration, listener bind,
  menu health) was not exercised in this session; left for an operator
  smoke check.

Revisit when: the daemon needs additional launch arguments — update both
the shipped plist template and the `-json` literal in the script, and
extend the exact-vector assertion.
