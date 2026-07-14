#!/usr/bin/env bash
# Regression tests for pr-integrity-check.sh. Case 1 reproduces the #47
# clobber (a PR declaring daemon/ that reverted api/ files and deleted a
# merged devlog entry) that motivated the gate. Run: bash this file.
set -u
here="$(cd "$(dirname "$0")" && pwd)"
S="$here/pr-integrity-check.sh"
pass=0; fail=0
run() { # name expected_exit BODY name_status_text [numstat_text]
  # Specs are the readable tab/newline form; convert to the -z NUL framing the
  # checker consumes (name-status: NUL after every field; numstat: NUL after
  # every record/line), so the tests exercise the real input format.
  local name="$1" exp="$2" body="$3" ns="$4" numstat="${5-}" out rc nsfile
  nsfile="$(mktemp)"
  printf '%s' "$numstat" | awk 'NF{printf "%s%c",$0,0}' > "$nsfile"
  out="$(printf '%s' "$ns" | awk -F'\t' 'NF{for(i=1;i<=NF;i++)printf "%s%c",$i,0}' \
         | BODY="$body" NUMSTAT_FILE="$nsfile" bash "$S" 2>&1)"; rc=$?
  rm -f "$nsfile"
  if [ "$rc" -eq "$exp" ]; then pass=$((pass+1)); echo "ok   $name (exit $rc)"
  else fail=$((fail+1)); echo "FAIL $name: expected $exp got $rc"; printf '%s\n' "$out" | sed 's/^/      /'; fi
}

run "#47 clobber (api out-of-scope + devlog delete)" 1 $'## Scope\nScope: daemon/\n\n## What' \
  $'M\t.github/workflows/api-ci.yml\nM\tapi/README.md\nD\tdevlog/2026-07-14-1240-api-ci-vacuum-binary.md\nM\tdaemon/internal/exec/capability.go\n'

run "clean daemon PR" 0 $'## Scope\nScope: daemon/\n' \
  $'M\tdaemon/internal/exec/capability.go\nA\tdevlog/2026-07-14-1300-thing.md\n'

run "infra-only PR (no component)" 0 $'## Scope\nScope: .github/, scripts/ (repo infra)\n' \
  $'A\t.github/workflows/pr-integrity.yml\nA\tscripts/pr-integrity-check.sh\nM\tAGENTS.md\nA\tdevlog/x.md\n'

run "repo-wide scaffold" 0 $'## Scope\nScope: repo-wide scaffold\n' \
  $'M\tapi/openapi.yaml\nM\tdaemon/main.go\n'

run "incidental/negated 'repo-wide' does NOT opt out" 1 $'## Scope\nScope: daemon/ (not repo-wide)\n' \
  $'M\tapi/openapi.yaml\n'

run "negated component in parens is NOT declared" 1 $'## Scope\nScope: daemon/ (not api/)\n' \
  $'M\tapi/openapi.yaml\n'

run "per-dir parenthetical annotations keep real dirs declared" 0 $'## Scope\nScope: api/ (spec), daemon/ (consumer)\n' \
  $'M\tapi/openapi.yaml\nM\tdaemon/x.go\n'

# Non-parenthetical negated prose must not flip the guard (round-7 class):
run "non-paren negated 'repo-wide scaffold' does NOT opt out" 1 $'## Scope\nScope: daemon/ -- not repo-wide scaffold\n' \
  $'M\tapi/openapi.yaml\n'
run "non-paren negated component is NOT declared" 1 $'## Scope\nScope: daemon/ -- not api/\n' \
  $'M\tapi/openapi.yaml\n'
run "trailing prose after a dir token still declares it" 0 $'## Scope\nScope: api/ its CI workflow and README\n' \
  $'M\tapi/openapi.yaml\n'

# --- adversarial enumeration of the Scope-declaration input space -----------
# Run once as tests so the class is closed by enumeration, not by widening the
# cited pattern one review round at a time.
run "later 'scope:' in prose does not become the declaration" 1 $'## Scope\nScope: daemon/ -- out of scope: api/\n' \
  $'M\tapi/openapi.yaml\n'
run "multiple 'Scope:' markers: first wins" 1 $'## Scope\nScope: daemon/ (see Scope: api/ below)\n' \
  $'M\tapi/openapi.yaml\n'
run "space before colon still parses marker" 0 $'## Scope\nScope : daemon/\n' \
  $'M\tdaemon/x.go\n'
run "comma without space splits items" 0 $'## Scope\nScope: api/,daemon/\n' \
  $'M\tapi/openapi.yaml\nM\tdaemon/x.go\n'
run "substring dir 'apidocs/' does not declare api" 1 $'## Scope\nScope: apidocs/\n' \
  $'M\tapi/openapi.yaml\n'
run "nested 'foo/api/' item does not declare api" 1 $'## Scope\nScope: foo/api/\n' \
  $'M\tapi/openapi.yaml\n'
run "bare list without 'Scope:' marker still parses (fallback)" 0 $'## Scope\napi/, daemon/\n' \
  $'M\tapi/openapi.yaml\nM\tdaemon/x.go\n'
run "leading indentation on the Scope line" 0 $'## Scope\n  Scope: daemon/\n' \
  $'M\tdaemon/x.go\n'
run "duplicate dir declaration is harmless" 0 $'## Scope\nScope: api/, api/\n' \
  $'M\tapi/openapi.yaml\n'

run "api+daemon both declared" 0 $'## Scope\nScope: api/, daemon/\n' \
  $'M\tapi/openapi.yaml\nM\tdaemon/internal/domain/x.go\n'

run "component change, no scope" 1 $'## What\nstuff\n' \
  $'M\tdaemon/main.go\n'

run "devlog marker-append (M, purely additive) allowed" 0 $'## Scope\nScope: daemon/\n' \
  $'M\tdevlog/2026-07-08-old.md\nM\tdaemon/main.go\n' \
  $'2\t0\tdevlog/2026-07-08-old.md\n10\t3\tdaemon/main.go\n'

run "devlog rewrite/truncate (M with deletions) blocked" 1 $'## Scope\nScope: daemon/\n' \
  $'M\tdevlog/2026-07-08-old.md\n' \
  $'4\t9\tdevlog/2026-07-08-old.md\n'

run "M devlog with no NUMSTAT -> not manufactured as violation" 0 $'## Scope\nScope: daemon/\n' \
  $'M\tdevlog/2026-07-08-old.md\n'

run "devlog type-change (T, file->symlink) blocked" 1 $'## Scope\nScope: daemon/\n' \
  $'T\tdevlog/2026-07-08-old.md\n' \
  $'0\t5\tdevlog/2026-07-08-old.md\n'

run "new devlog entry (A) always allowed" 0 $'## Scope\nScope: daemon/\n' \
  $'A\tdevlog/2026-07-14-new.md\n' \
  $'40\t0\tdevlog/2026-07-14-new.md\n'

run "devlog/README.md rewrite (protocol, not a frozen entry) allowed" 0 $'## Scope\nScope: daemon/\n' \
  $'M\tdevlog/README.md\n' \
  $'12\t7\tdevlog/README.md\n'

run "devlog/README.md delete (protocol, not this gate's concern) allowed" 0 $'## Scope\nScope: daemon/\n' \
  $'D\tdevlog/README.md\n'

# Non-ASCII path (the round-4 finding): under -z it's unquoted, so its top dir
# resolves to daemon/ and the scope check applies (would've read "daemon and
# slipped through in the quoted default form).
run "non-ASCII component path out-of-scope caught" 1 $'## Scope\nScope: api/\n' \
  $'A\tdaemon/caf\xc3\xa9.go\n'
run "non-ASCII component path in-scope allowed" 0 $'## Scope\nScope: daemon/\n' \
  $'A\tdaemon/caf\xc3\xa9.go\n'

run "devlog rename-away" 1 $'## Scope\nScope: daemon/\n' \
  $'R100\tdevlog/2026-07-08-old.md\tdevlog/renamed.md\n'

run "no false 'app' match on daemon/apply.go" 0 $'## Scope\nScope: daemon/\n' \
  $'M\tdaemon/apply.go\n'

run "html-comment scope not counted" 1 $'## Scope\n<!-- e.g. Scope: api/, daemon/ -->\nScope: daemon/\n' \
  $'M\tapi/openapi.yaml\n'

echo "----"; echo "pass=$pass fail=$fail"; [ "$fail" -eq 0 ]
