#!/usr/bin/env bash
# test-tier-a-lookup.sh — fixtures for the Tier A path lookup.
#
# Builds a synthetic repo root holding only an AGENTS.md, since the lookup
# matches the entries' text and not the tree, plus one synthetic git repo
# for range mode. No network. Exit code: 0 when every assertion passes, 1
# otherwise.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
LOOKUP=$SCRIPT_DIR/tier-a-lookup.sh

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.invalid
export GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.invalid
export GIT_AUTHOR_DATE='2026-01-01T00:00:00Z'
export GIT_COMMITTER_DATE='2026-01-01T00:00:00Z'
unset GIT_DIR GIT_WORK_TREE

pass=0
fail=0
CASE=''
OUT=''
RC=0

ROOT=$TMP/root
mkdir -p "$ROOT"
cat >"$ROOT/AGENTS.md" <<'MD'
# Project

### Tier A Paths

All under `daemon/`:

- **Import and handoff:** `internal/importer/` and
  `internal/store/handoff_journal.go`.
- **Publication decisions in the engine:**
  `internal/engine/production_reentry*.go` (re-earn readiness).
- **Provider credentials:** in `cmd/freesided/` the files `auth*.go` and
  `enroll_codex.go`.
- `enroll_codex.go` has no directory before it in this bullet.
- **Migrations that rewrite data:** any `migrations/*.sql` file that
  rewrites rows.
- **Live writer transcript:** `internal/importer/` again.

### Worked Examples

- `internal/after_section/` is outside the section.
MD

run_lookup() {
  set +e
  OUT=$(bash "$LOOKUP" "$@" 2>&1)
  RC=$?
  set -e
}

begin_case() {
  CASE=$1
  echo "case: $CASE"
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

NO_MATCH='no Tier A entry matches; decide the tier by category (AGENTS.md, Assurance Tiers, Classification)'

begin_case "a directory entry lists what is under it, once per bullet"
run_lookup --root "$ROOT" daemon/internal/importer/sub/x.go daemon/internal/importerx/y.go
assert_rc 0
assert_contains 'daemon/internal/importer/sub/x.go: Tier A: entry internal/importer/ (Import and handoff)'
assert_contains 'daemon/internal/importer/sub/x.go: Tier A: entry internal/importer/ (Live writer transcript)'
assert_contains "daemon/internal/importerx/y.go: $NO_MATCH"

begin_case "a file entry lists only itself"
run_lookup --root "$ROOT" daemon/internal/store/handoff_journal.go daemon/internal/store/other.go
assert_contains 'daemon/internal/store/handoff_journal.go: Tier A: entry internal/store/handoff_journal.go (Import and handoff)'
assert_contains "daemon/internal/store/other.go: $NO_MATCH"

begin_case "a bare name resolves against its bullet's directory, which lists nothing itself"
run_lookup --root "$ROOT" daemon/cmd/freesided/enroll_codex.go \
  daemon/cmd/freesided/auth_login.go daemon/cmd/freesided/main.go
assert_contains 'daemon/cmd/freesided/enroll_codex.go: Tier A: entry cmd/freesided/enroll_codex.go (Provider credentials)'
assert_contains 'daemon/cmd/freesided/auth_login.go: Tier A: entry cmd/freesided/auth*.go (Provider credentials)'
assert_contains "daemon/cmd/freesided/main.go: $NO_MATCH"
assert_not_contains 'entry cmd/freesided/ ('

begin_case "a bare name with no directory before it lists nothing"
run_lookup --root "$ROOT" daemon/enroll_codex.go
assert_contains "daemon/enroll_codex.go: $NO_MATCH"

begin_case "a glob's * stays within one path segment of the entry's directory"
run_lookup --root "$ROOT" daemon/internal/engine/production_reentry_rebase.go \
  daemon/internal/engine/production_reentry/x.go \
  daemon/internal/other/production_reentry_rebase.go
assert_contains 'daemon/internal/engine/production_reentry_rebase.go: Tier A: entry internal/engine/production_reentry*.go (Publication decisions in the engine)'
assert_contains "daemon/internal/engine/production_reentry/x.go: $NO_MATCH"
assert_contains "daemon/internal/other/production_reentry_rebase.go: $NO_MATCH"

begin_case "a conditional entry shows its bullet"
run_lookup --root "$ROOT" daemon/migrations/0100_x.sql
assert_contains 'daemon/migrations/0100_x.sql: Tier A: entry migrations/*.sql (Migrations that rewrite data)'

begin_case "a test-only file under an entry is not Tier A by path"
run_lookup --root "$ROOT" daemon/internal/importer/x_test.go \
  daemon/internal/importer/testdata/f.json
assert_contains 'daemon/internal/importer/x_test.go: not Tier A by path: test-only file under entry internal/importer/ (Import and handoff); a gate test or live-credential change stays Tier A (AGENTS.md, Assurance Tiers, Classification)'
assert_contains 'daemon/internal/importer/testdata/f.json: not Tier A by path: test-only file under entry internal/importer/ (Import and handoff)'
assert_not_contains 'x_test.go: Tier A'

begin_case "an unlisted path, and one outside the section, match nothing"
run_lookup --root "$ROOT" app/x.swift daemon/internal/other/x.go daemon/internal/after_section/x.go
assert_rc 0
assert_contains "app/x.swift: $NO_MATCH"
assert_contains "daemon/internal/other/x.go: $NO_MATCH"
assert_contains "daemon/internal/after_section/x.go: $NO_MATCH"

begin_case "the default root is the working directory"
set +e
OUT=$(cd "$ROOT" && bash "$LOOKUP" daemon/internal/importer/x.go 2>&1)
RC=$?
set -e
assert_rc 0
assert_contains 'daemon/internal/importer/x.go: Tier A: entry internal/importer/'

begin_case "range mode reports both sides of a rename and an added file"
REPO=$TMP/repo
git init -q -b main "$REPO"
cp "$ROOT/AGENTS.md" "$REPO/AGENTS.md"
mkdir -p "$REPO/daemon/internal/importer" "$REPO/daemon/internal/other"
echo one >"$REPO/daemon/internal/importer/old.go"
git -C "$REPO" add -A
git -C "$REPO" commit -q -m base
git -C "$REPO" mv daemon/internal/importer/old.go daemon/internal/other/new.go
echo two >"$REPO/app.txt"
git -C "$REPO" add -A
git -C "$REPO" commit -q -m change
run_lookup --root "$REPO" --range HEAD~1 HEAD
assert_rc 0
assert_contains 'daemon/internal/importer/old.go: Tier A: entry internal/importer/ (Import and handoff)'
assert_contains "daemon/internal/other/new.go: $NO_MATCH"
assert_contains "app.txt: $NO_MATCH"

begin_case "an empty range reports no files"
run_lookup --root "$REPO" --range HEAD HEAD
assert_rc 0
assert_contains 'no files differ in HEAD...HEAD'

begin_case "a bad ref is an environment error"
run_lookup --root "$REPO" --range no-such-ref HEAD
assert_rc 2
assert_contains 'git diff no-such-ref...HEAD failed'
assert_not_contains 'Tier A'

begin_case "no paths is a usage error"
run_lookup --root "$ROOT"
assert_rc 2
assert_contains 'usage:'

begin_case "a path not in repo-relative form is a usage error"
for bad in ./daemon/internal/importer/x.go daemon//internal/importer/x.go \
  /daemon/internal/importer/x.go daemon/internal/../internal/importer/x.go \
  daemon/internal/importer/; do
  run_lookup --root "$ROOT" daemon/internal/importer/ok.go "$bad"
  assert_rc 2
  assert_contains "$bad is not a repo-relative path"
  assert_not_contains 'Tier A: entry'
done

begin_case "--range with one ref is a usage error"
run_lookup --root "$ROOT" --range HEAD
assert_rc 2
assert_contains 'usage:'

begin_case "a missing AGENTS.md is an environment error"
mkdir -p "$TMP/empty"
run_lookup --root "$TMP/empty" daemon/x.go
assert_rc 2
assert_contains 'AGENTS.md not found'

begin_case "a section with no path entries is an environment error"
mkdir -p "$TMP/no-entries"
cat >"$TMP/no-entries/AGENTS.md" <<'MD'
### Tier A Paths

- `CredentialMode` only.
MD
run_lookup --root "$TMP/no-entries" daemon/x.go
assert_rc 2
assert_contains 'yields no path entries'

echo
echo "passed $pass assertion(s), failed $fail"
[ "$fail" -eq 0 ]
