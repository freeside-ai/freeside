#!/usr/bin/env bash
# test-check-tier-a-paths.sh — fixtures for the Tier A path-list check.
#
# Builds synthetic repo roots (an AGENTS.md and a daemon/ tree) with no git
# and no network, then asserts the PR workflow runs the check on every pull
# request. Exit code: 0 when every assertion passes, 1 otherwise.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
CHECK=$SCRIPT_DIR/check-tier-a-paths.sh

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
case_number=0
CASE=''
ROOT=''
OUT=''
RC=0

# begin_case <name> starts a case with a fresh root holding a daemon/ tree.
begin_case() {
  case_number=$((case_number + 1))
  CASE=$(printf '%02d %s' "$case_number" "$1")
  echo "case: $CASE"
  ROOT=$TMP/case-$case_number
  mkdir -p "$ROOT/daemon/internal/engine" "$ROOT/daemon/internal/publish" \
    "$ROOT/daemon/cmd/freesided" "$ROOT/daemon/migrations"
  touch "$ROOT/daemon/internal/engine/admission.go" \
    "$ROOT/daemon/internal/engine/production_reentry.go" \
    "$ROOT/daemon/internal/engine/production_reentry_rebase.go" \
    "$ROOT/daemon/cmd/freesided/auth_login.go" \
    "$ROOT/daemon/cmd/freesided/enroll_codex.go" \
    "$ROOT/daemon/migrations/0001_first.sql"
  # A directory that matches a glob is not a file.
  mkdir -p "$ROOT/daemon/internal/engine/stale_dir.go"
}

# agents writes stdin as the case root's AGENTS.md.
agents() {
  cat >"$ROOT/AGENTS.md"
}

run_check() {
  set +e
  OUT=$(bash "$CHECK" "$@" 2>&1)
  RC=$?
  set -e
}

report_failure() {
  fail=$((fail + 1))
  echo "FAIL [$CASE]: $*"
  printf '%s\n' "$OUT" | sed 's/^/    | /'
}

assert_rc() {
  if [ "$RC" -eq "$1" ]; then
    pass=$((pass + 1))
  else
    report_failure "expected exit $1, got $RC"
  fi
}

assert_contains() {
  if printf '%s\n' "$OUT" | grep -qF -- "$1"; then
    pass=$((pass + 1))
  else
    report_failure "expected output to contain: $1"
  fi
}

assert_not_contains() {
  if printf '%s\n' "$OUT" | grep -qF -- "$1"; then
    report_failure "expected output not to contain: $1"
  else
    pass=$((pass + 1))
  fi
}

begin_case "every entry form resolves"
agents <<'MD'
# Project

### Tier A Paths

Prose naming `internal/prose_only.go` is not a bullet. All under `daemon/`:

- **Files and directories:** `internal/publish/`,
  `internal/engine/admission.go`, and `CredentialMode`, a field.
- **Globs:** `internal/engine/production_reentry*.go` and any
  `migrations/*.sql` file.

  `internal/engine/admission.go` again, in a paragraph after a blank line.
- **Bare names:** in `cmd/freesided/` the files `auth*.go` and
  `enroll_codex.go`, wired by `storeAdmissionAuthority`.

### Worked Examples

- `internal/after_section.go` is outside the section.
MD
run_check "$ROOT"
assert_rc 0
assert_contains 'all 8 Tier A path entries resolve'

begin_case "the default root is the working directory"
agents <<'MD'
### Tier A Paths

- `internal/engine/admission.go`
MD
set +e
OUT=$(cd "$ROOT" && bash "$CHECK" 2>&1)
RC=$?
set -e
assert_rc 0
assert_contains 'all 1 Tier A path entries resolve'

begin_case "a missing file is unresolved"
agents <<'MD'
### Tier A Paths

- `internal/engine/admission.go` and
  `internal/engine/production_unpublished_reentry.go` (removed).
MD
run_check "$ROOT"
assert_rc 1
assert_contains 'unresolved: internal/engine/production_unpublished_reentry.go (no such file)'
assert_contains '1 of 2 Tier A path entries are unresolved'
assert_not_contains 'unresolved: internal/engine/admission.go'

begin_case "a glob that matches no file is unresolved"
agents <<'MD'
### Tier A Paths

- `internal/engine/publication_closure*.go` and
  `internal/engine/stale_dir*.go`.
MD
run_check "$ROOT"
assert_rc 1
assert_contains 'unresolved: internal/engine/publication_closure*.go (pattern matches no file)'
assert_contains 'unresolved: internal/engine/stale_dir*.go (pattern matches no file)'

begin_case "a missing directory is unresolved"
agents <<'MD'
### Tier A Paths

- `internal/publish/` and `internal/ward/`.
MD
run_check "$ROOT"
assert_rc 1
assert_contains 'unresolved: internal/ward/ (no such directory)'
assert_not_contains 'unresolved: internal/publish/'

begin_case "a bare name resolves only against its own bullet's directory"
agents <<'MD'
### Tier A Paths

- In `cmd/freesided/`: `enroll_codex.go` and `renew_codex.go`.
- `enroll_codex.go` has no directory before it in this bullet.
- `internal/engine/admission.go` names a file, not a directory: `auth*.go`.
MD
run_check "$ROOT"
assert_rc 1
assert_contains 'unresolved: cmd/freesided/renew_codex.go (no such file)'
assert_contains 'unresolved: enroll_codex.go (bare name with no directory before it in its bullet)'
assert_contains 'unresolved: auth*.go (bare name with no directory before it in its bullet)'
assert_contains '3 of 6 Tier A path entries are unresolved'

begin_case "a subsection does not end the section"
agents <<'MD'
### Tier A Paths

- `internal/engine/admission.go`

#### More

- `internal/engine/gone_after_subheading.go`

## Next Section

- `internal/engine/outside.go`
MD
run_check "$ROOT"
assert_rc 1
assert_contains 'unresolved: internal/engine/gone_after_subheading.go (no such file)'
assert_not_contains 'outside.go'

begin_case "every Markdown list shape is read"
agents <<'MD'
### Tier A Paths

- `internal/engine/admission.go` and
`internal/engine/gone_lazy.go`, a lazy continuation line.
* `internal/engine/gone_star.go`
+ `internal/engine/gone_plus.go`
1. ``internal/engine/gone_double.go``, a double-backtick span.

Prose after a blank line: `internal/engine/prose.go`.
MD
run_check "$ROOT"
assert_rc 1
assert_contains 'unresolved: internal/engine/gone_lazy.go (no such file)'
assert_contains 'unresolved: internal/engine/gone_star.go (no such file)'
assert_contains 'unresolved: internal/engine/gone_plus.go (no such file)'
assert_contains 'unresolved: internal/engine/gone_double.go (no such file)'
assert_not_contains 'prose.go'
assert_contains '4 of 5 Tier A path entries are unresolved'

begin_case "a missing AGENTS.md is an environment error"
run_check "$ROOT"
assert_rc 2
assert_contains 'AGENTS.md not found'

begin_case "a missing section heading is an environment error"
agents <<'MD'
### Tier A Path List

- `internal/engine/admission.go`
MD
run_check "$ROOT"
assert_rc 2
assert_contains "has no '### Tier A Paths' heading"

begin_case "a section with no path entries is an environment error"
agents <<'MD'
### Tier A Paths

All under `daemon/`, now in a table:

| Path | Why |
| `internal/engine/missing.go` | prose, not a bullet |

- `CredentialMode` is an identifier, not a path.

### Worked Examples

- `internal/engine/admission.go`
MD
run_check "$ROOT"
assert_rc 2
assert_contains 'yields no path entries'

begin_case "a root that is not a directory is an environment error"
run_check "$TMP/no-such-root"
assert_rc 2
assert_contains 'is not a directory'

begin_case "extra arguments are a usage error"
run_check "$ROOT" extra
assert_rc 2
assert_contains 'usage:'

begin_case "the PR workflow runs the check unconditionally and requires it"
set +e
OUT=$(python3 - "$SCRIPT_DIR/.." 2>&1 <<'PY'
import re
import sys
from pathlib import Path

root = Path(sys.argv[1])
workflow = (root / '.github/workflows/tier-a-paths.yml').read_text()
assert '  workflow_call:' in workflow
assert '  contents: read' in workflow
assert 'group: tier-a-paths-${{' in workflow
assert '        run: bash scripts/check.sh scripts tier-a-paths\n' in workflow
assert not re.search(r'^\s+if:', workflow, re.M)
caller = (root / '.github/workflows/pull-request.yml').read_text()
jobs = dict(re.findall(r'^  ([a-z-]+):\n(.*?)(?=^  [a-z-]+:|\Z)', caller, re.M | re.S))
job_lines = [line.strip() for line in jobs['tier-a-paths'].splitlines()
             if line.strip() and not line.lstrip().startswith('#')]
assert job_lines == ['uses: ./.github/workflows/tier-a-paths.yml'], job_lines
assert '      - tier-a-paths\n' in jobs['required']
assert '    if: always()\n' in jobs['required']
assert '.["tier-a-paths"].result == "success"' in jobs['required']
print('PASS: tier-a-paths workflow is unconditional and required')
PY
)
RC=$?
set -e
assert_rc 0
assert_contains 'PASS: tier-a-paths workflow is unconditional and required'

echo
echo "passed $pass assertion(s), failed $fail"
[ "$fail" -eq 0 ]
