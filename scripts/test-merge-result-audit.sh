#!/usr/bin/env bash
# test-merge-result-audit.sh — regression suite for merge-result-audit.sh.
#
# Builds isolated synthetic git repositories under a temp directory
# (hermetic git config/identity, no network) and checks every audit
# guarantee: conflict detection, exact-base binding, complete
# prospective-diff visibility, path-boundary enforcement, and the
# malformed-input rejections. Case 11 reproduces the incident class this
# tool defends against: a branch that inherits a sibling's commits and
# later carries the inverse change, which a clean 3-way merge would
# silently apply to already-merged work (#47 reverting #48).
#
# Exit code: 0 when every assertion passes, 1 otherwise.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
AUDIT=$SCRIPT_DIR/merge-result-audit.sh

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# Hermetic git environment: no user/system config (rename or diff
# settings there would change behavior), fixed identity and dates.
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_SYSTEM=/dev/null
export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.invalid
export GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.invalid
export GIT_AUTHOR_DATE='2026-01-01T00:00:00Z'
export GIT_COMMITTER_DATE='2026-01-01T00:00:00Z'
unset GIT_DIR GIT_WORK_TREE

pass=0
fail=0
CASE=''

begin_case() {
  CASE=$1
  echo "case: $CASE"
}

report_failure() {
  fail=$((fail + 1))
  echo "FAIL [$CASE]: $*"
  printf '%s\n' "$OUT" | sed 's/^/    | /'
}

run_audit() { # <dir> <audit args>... ; sets OUT and RC
  local dir=$1
  shift
  set +e
  OUT=$( (cd "$dir" && "$AUDIT" "$@") 2>&1 )
  RC=$?
  set -e
}

assert_rc() {
  if [ "$RC" -eq "$1" ]; then
    pass=$((pass + 1))
  else
    report_failure "expected rc=$1, got rc=$RC"
  fi
}

assert_contains() {
  case $OUT in
    *"$1"*) pass=$((pass + 1)) ;;
    *) report_failure "output does not contain: $1" ;;
  esac
}

assert_not_contains() {
  case $OUT in
    *"$1"*) report_failure "output unexpectedly contains: $1" ;;
    *) pass=$((pass + 1)) ;;
  esac
}

new_repo() { # <name> ; prints repo path
  local r=$TMP/$1
  git init -q -b main "$r"
  printf '%s' "$r"
}

commit_file() { # <repo> <path> <content> <message>
  local r=$1 p=$2 c=$3 m=$4
  mkdir -p "$r/$(dirname "$p")"
  printf '%s\n' "$c" >"$r/$p"
  git -C "$r" add -- "$p"
  git -C "$r" commit -qm "$m"
}

# ---------------------------------------------------------------- case 1
# Ordinary independent branch: main advances with unrelated sibling work
# after the branch forks; the in-scope change passes, and the resolved
# base/head SHAs are printed.
begin_case "01 ordinary independent branch"
r=$(new_repo case01)
commit_file "$r" README.md "readme v1" "init"
commit_file "$r" docs/a.md "doc v1" "add doc"
git -C "$r" checkout -qb feature main
commit_file "$r" docs/a.md "doc v2 from feature" "feature: edit doc"
git -C "$r" checkout -q main
commit_file "$r" README.md "readme v2" "main: sibling advance"
run_audit "$r" main feature docs
assert_rc 0
assert_contains $'M\tdocs/a.md'
assert_contains "base: main = $(git -C "$r" rev-parse main)"
assert_contains "head: feature = $(git -C "$r" rev-parse feature)"
assert_contains "PASS"

# ---------------------------------------------------------------- case 2
# Intentional stack: B forks from unmerged A. Audited with the union
# allowlist it passes; with only its own paths, A's inherited change is
# (correctly) out of scope.
begin_case "02 intentional stack"
r=$(new_repo case02)
commit_file "$r" api/x.txt "api v1" "init api"
commit_file "$r" docs/y.md "doc v1" "init docs"
git -C "$r" checkout -qb stack-a main
commit_file "$r" api/x.txt "api v2" "a: api change"
git -C "$r" checkout -qb stack-b stack-a
commit_file "$r" docs/y.md "doc v2" "b: docs change"
run_audit "$r" main stack-b api docs
assert_rc 0
run_audit "$r" main stack-b docs
assert_rc 1
assert_contains "api/x.txt"

# ---------------------------------------------------------------- case 3
# Clean current-base merge: head branched from the exact base tip.
begin_case "03 clean current-base merge"
r=$(new_repo case03)
commit_file "$r" docs/a.md "doc v1" "init"
git -C "$r" checkout -qb fresh main
commit_file "$r" docs/a.md "doc v2" "edit doc"
run_audit "$r" main fresh docs
assert_rc 0
assert_contains $'M\tdocs/a.md'

# ---------------------------------------------------------------- case 4
# Real conflict: both sides edit the same file.
begin_case "04 merge conflict"
r=$(new_repo case04)
commit_file "$r" docs/a.md "original" "init"
git -C "$r" checkout -qb conflicting main
commit_file "$r" docs/a.md "branch version" "branch edit"
git -C "$r" checkout -q main
commit_file "$r" docs/a.md "main version" "main edit"
run_audit "$r" main conflicting docs
assert_rc 2
assert_contains "conflict"
assert_contains "docs/a.md"

# ---------------------------------------------------------------- case 5
# Allowed-path matching: directory entry, exact-file entry, and a
# trailing-slash directory entry all admit the change.
begin_case "05 allowed-path change"
r=$(new_repo case05)
commit_file "$r" api/x.txt "api v1" "init"
git -C "$r" checkout -qb apichange main
commit_file "$r" api/x.txt "api v2" "api edit"
run_audit "$r" main apichange api
assert_rc 0
run_audit "$r" main apichange api/x.txt
assert_rc 0
run_audit "$r" main apichange api/
assert_rc 0

# ---------------------------------------------------------------- case 6
# Out-of-scope change, including the ambiguous-prefix boundary: allowed
# "api" must not admit "api2/...".
begin_case "06 out-of-scope change and prefix boundary"
r=$(new_repo case06)
commit_file "$r" api/x.txt "api v1" "init api"
commit_file "$r" api2/foo.txt "api2 v1" "init api2"
commit_file "$r" docs/y.md "doc v1" "init docs"
git -C "$r" checkout -qb overreach main
commit_file "$r" api/x.txt "api v2" "api edit"
commit_file "$r" docs/y.md "doc v2" "docs edit"
run_audit "$r" main overreach docs
assert_rc 1
assert_contains "api/x.txt"
assert_contains "outside the allowed paths"
git -C "$r" checkout -qb prefix main
commit_file "$r" api2/foo.txt "api2 v2" "api2 edit"
run_audit "$r" main prefix api
assert_rc 1
assert_contains "api2/foo.txt"

# ---------------------------------------------------------------- case 7
# Paths with spaces and non-ASCII bytes, in the change set and in the
# allowlist, are matched and displayed without mangling.
begin_case "07 spaces and non-ASCII paths"
r=$(new_repo case07)
commit_file "$r" "docs dir/my file.txt" "v1" "init spaced"
commit_file "$r" "docs/naïve.md" "v1" "init non-ascii"
git -C "$r" checkout -qb spaced main
commit_file "$r" "docs dir/my file.txt" "v2" "edit spaced"
commit_file "$r" "docs/naïve.md" "v2" "edit non-ascii"
run_audit "$r" main spaced "docs dir" docs
assert_rc 0
assert_contains "docs dir/my file.txt"
assert_contains "docs/naïve.md"
run_audit "$r" main spaced docs
assert_rc 1
assert_contains "docs dir/my file.txt"

# ---------------------------------------------------------------- case 8
# Renames and deletions: a rename is gated on BOTH sides; a deletion of
# an out-of-scope file fails.
begin_case "08 rename and deletion"
r=$(new_repo case08)
commit_file "$r" api/old.txt "stable content for rename detection" "init"
git -C "$r" checkout -qb renamer main
mkdir -p "$r/docs"
git -C "$r" mv api/old.txt docs/new.txt
git -C "$r" commit -qm "rename out of api"
run_audit "$r" main renamer api
assert_rc 1
assert_contains "docs/new.txt"
run_audit "$r" main renamer api docs
assert_rc 0
assert_contains $'api/old.txt\tdocs/new.txt'
git -C "$r" checkout -qb deleter main
git -C "$r" rm -q -- api/old.txt
git -C "$r" commit -qm "delete api file"
run_audit "$r" main deleter docs
assert_rc 1
assert_contains $'D\tapi/old.txt'

# ---------------------------------------------------------------- case 9
# Empty and malformed allowlists are rejected before any git work.
begin_case "09 empty or malformed allowlist"
r=$(new_repo case09)
commit_file "$r" docs/a.md "v1" "init"
git -C "$r" checkout -qb any main
commit_file "$r" docs/a.md "v2" "edit"
run_audit "$r" main any
assert_rc 3
assert_contains "usage:"
for bad in "/abs" "a/../b" ".." "./x" "a//b" "." "docs//" "a	b"; do
  run_audit "$r" main any "$bad"
  assert_rc 3
done
run_audit "$r" main any ""
assert_rc 3
run_audit "$r" main any 'a\b'
assert_rc 3
run_audit "$r" main any docs docs
assert_rc 3
assert_contains "duplicate"

# --------------------------------------------------------------- case 10
# Base advance after earlier verification: the audit binds to the exact
# base SHA it prints, so evidence from the old tip names a SHA the new
# tip no longer matches.
begin_case "10 base advance invalidates earlier evidence"
r=$(new_repo case10)
commit_file "$r" api/x.txt "api v1" "init api"
commit_file "$r" docs/b.md "doc v1" "init docs"
git -C "$r" checkout -qb quiet main
commit_file "$r" docs/b.md "doc v2" "docs edit"
git -C "$r" checkout -q main
old_tip=$(git -C "$r" rev-parse main)
run_audit "$r" main quiet docs
assert_rc 0
assert_contains "base: main = $old_tip"
commit_file "$r" api/x.txt "api v2" "sibling advance"
new_tip=$(git -C "$r" rev-parse main)
run_audit "$r" main quiet docs
assert_rc 0
assert_contains "base: main = $new_tip"
assert_not_contains "base: main = $old_tip"

# --------------------------------------------------------------- case 11
# Regression, the #47/#48 class: branch B forks from sibling A (inherits
# its commits) and later reverts them. B's own pre-merge audit against
# the pre-A main is green — the inverse change is invisible there. After
# A merges, the prospective result of merging B reverts A's merged work,
# and the audit against the current tip exposes exactly that.
begin_case "11 inherited-then-reverted sibling work"
r=$(new_repo case11)
commit_file "$r" api/spec.yaml "spec v1" "init api"
commit_file "$r" docs/x.md "doc v1" "init docs"
git -C "$r" checkout -qb pr48 main
mkdir -p "$r/devlog"
printf '%s\n' "spec v2" >"$r/api/spec.yaml"
printf '%s\n' "pr48 devlog" >"$r/devlog/pr48.md"
git -C "$r" add -- api/spec.yaml devlog/pr48.md
git -C "$r" commit -qm "pr48: pin validator"
git -C "$r" checkout -qb pr47 pr48
git -C "$r" revert --no-edit HEAD >/dev/null
commit_file "$r" docs/x.md "doc v2" "pr47: real docs work"
# Branch-local verification is green: against pre-merge main, the net
# change is only the docs edit.
run_audit "$r" main pr47 docs
assert_rc 0
assert_not_contains "api/spec.yaml"
assert_not_contains "devlog/pr48.md"
# Sibling A merges first.
git -C "$r" checkout -q main
git -C "$r" merge -q --no-ff -m "merge pr48" pr48
# The audit against the current tip exposes the inverse change.
run_audit "$r" main pr47 docs
assert_rc 1
assert_contains $'M\tapi/spec.yaml'
assert_contains $'D\tdevlog/pr48.md'
assert_contains $'M\tdocs/x.md'
assert_contains "outside the allowed paths"

# --------------------------------------------------------------- case 12
# Head already contained in base: empty change set, pass.
begin_case "12 head ancestor of base, base==head"
r=$(new_repo case12)
commit_file "$r" docs/a.md "v1" "init"
old=$(git -C "$r" rev-parse main)
commit_file "$r" docs/a.md "v2" "advance"
run_audit "$r" main "$old" docs
assert_rc 0
assert_contains "empty change set"
run_audit "$r" main main docs
assert_rc 0
assert_contains "empty change set"

# --------------------------------------------------------------- case 13
# Unverifiable states fail with the git-failure code: unresolvable ref,
# corrupt (deleted) head commit object, and no repository at all.
begin_case "13 command failure and corrupt refs"
r=$(new_repo case13)
commit_file "$r" docs/a.md "v1" "init"
run_audit "$r" main nosuchref docs
assert_rc 4
assert_contains "cannot resolve"
git -C "$r" checkout -qb doomed main
commit_file "$r" docs/a.md "v2" "doomed edit"
doomed_sha=$(git -C "$r" rev-parse doomed)
rm "$r/.git/objects/${doomed_sha:0:2}/${doomed_sha:2}"
run_audit "$r" main doomed docs
assert_rc 4
mkdir -p "$TMP/not-a-repo"
run_audit "$TMP/not-a-repo" main other docs
assert_rc 4
assert_contains "not inside a git repository"

# --------------------------------------------------------------- case 14
# Ref ambiguity (refute-pass finding): a tag shadowing the head branch
# name resolves in the tag's favor, so the audit would examine a
# different commit than the forge merges. Ambiguous names are rejected;
# the full ref form still audits the real branch.
begin_case "14 ambiguous ref rejected"
r=$(new_repo case14)
commit_file "$r" docs/a.md "v1" "init"
git -C "$r" checkout -qb feature main
commit_file "$r" api/x.txt "out of scope" "api change"
git -C "$r" tag feature main
run_audit "$r" main feature docs
assert_rc 4
assert_contains "ambiguous"
run_audit "$r" main refs/heads/feature docs
assert_rc 1
assert_contains "api/x.txt"

# --------------------------------------------------------------- case 15
# Submodule cloak (refute-pass finding): submodule.<name>.ignore=all,
# whether planted in-tree via .gitmodules or set in repo config, must
# not hide a gitlink move from the audited change set.
begin_case "15 submodule ignore cannot cloak gitlink changes"
r=$(new_repo case15)
commit_file "$r" docs/a.md "v1" "init docs"
printf '[submodule "vendor"]\n\tpath = vendor\n\turl = ./vendor\n\tignore = all\n' \
  >"$r/.gitmodules"
git -C "$r" add -- .gitmodules
git -C "$r" update-index --add \
  --cacheinfo 160000,1111111111111111111111111111111111111111,vendor
git -C "$r" commit -qm "add cloaked gitlink"
git -C "$r" config submodule.vendor.ignore all
git -C "$r" checkout -qb mover main
git -C "$r" update-index --add \
  --cacheinfo 160000,2222222222222222222222222222222222222222,vendor
git -C "$r" commit -qm "move gitlink"
commit_file "$r" docs/a.md "v2" "docs edit"
run_audit "$r" main mover docs
assert_rc 1
assert_contains "vendor"

# --------------------------------------------------------------- case 16
# Replace-ref cloak (Codex review finding): a refs/replace/ entry makes
# merge-tree/diff-tree read a substitute object while rev-parse still
# prints the original SHA, so the audited content would not be the
# recorded SHA. GIT_NO_REPLACE_OBJECTS must keep the real head audited.
begin_case "16 replace ref cannot cloak the audited head"
r=$(new_repo case16)
commit_file "$r" docs/a.md "v1" "init docs"
commit_file "$r" api/x.txt "v1" "init api"
git -C "$r" checkout -qb sneaky main
commit_file "$r" api/x.txt "evil" "api change"
commit_file "$r" docs/a.md "v2" "docs change"
git -C "$r" checkout -qb harmless main
commit_file "$r" docs/a.md "v2" "docs only"
git -C "$r" replace "$(git -C "$r" rev-parse sneaky)" \
  "$(git -C "$r" rev-parse harmless)"
run_audit "$r" main sneaky docs
assert_rc 1
assert_contains "api/x.txt"

# --------------------------------------------------------------- case 17
# Shallow history: a truncated clone can compute a different merge base
# than the forge, so the audit refuses to run in one.
begin_case "17 shallow repository rejected"
r=$(new_repo case17)
commit_file "$r" docs/a.md "v1" "init"
commit_file "$r" docs/a.md "v2" "advance"
git clone -q --depth 1 "file://$r" "$TMP/case17-shallow" 2>/dev/null
run_audit "$TMP/case17-shallow" main main docs
assert_rc 4
assert_contains "shallow"

# --------------------------------------------------------------- case 18
# Merge-driver cloak (Codex review finding): an in-tree .gitattributes
# entry plus a local merge.<name>.driver config can make merge-tree
# resolve a both-sides edit to the base content, erasing the
# out-of-scope path from the change set the forge merge would conflict
# on. Locally configured drivers are rejected outright.
begin_case "18 local merge driver rejected"
r=$(new_repo case18)
commit_file "$r" docs/a.md "v1" "init docs"
commit_file "$r" api/x.txt "original" "init api"
printf 'api/x.txt merge=keepbase\n' >"$r/.gitattributes"
git -C "$r" add -- .gitattributes
git -C "$r" commit -qm "add attributes"
git -C "$r" checkout -qb driver main
commit_file "$r" api/x.txt "branch version" "branch api edit"
commit_file "$r" docs/a.md "v2" "docs edit"
git -C "$r" checkout -q main
commit_file "$r" api/x.txt "main version" "main api edit"
git -C "$r" config merge.keepbase.driver true
run_audit "$r" main driver docs
assert_rc 4
assert_contains "merge configuration"
# Without the local driver config, the same refs are an honest conflict.
git -C "$r" config --unset merge.keepbase.driver
run_audit "$r" main driver docs
assert_rc 2

# --------------------------------------------------------------- case 19
# Merge-config cloak, widened class (Codex review finding): any
# repo-local merge.* knob (e.g. merge.directoryRenames=false hiding a
# directory-rename conflict the forge would report) or the
# diff.renames/renameLimit fallbacks merge-ort reads can steer the
# merge result; the whole family is rejected.
begin_case "19 local merge config rejected"
r=$(new_repo case19)
commit_file "$r" docs/a.md "v1" "init"
git -C "$r" checkout -qb tweak main
commit_file "$r" docs/a.md "v2" "edit"
git -C "$r" config merge.directoryRenames false
run_audit "$r" main tweak docs
assert_rc 4
assert_contains "merge configuration"
git -C "$r" config --unset merge.directoryRenames
git -C "$r" config diff.renames false
run_audit "$r" main tweak docs
assert_rc 4
git -C "$r" config --unset diff.renames
run_audit "$r" main tweak docs
assert_rc 0

# --------------------------------------------------------------- case 20
# Graft cloak (Codex review finding): .git/info/grafts fakes commit
# parents (the deprecated precursor of replace refs), so a graft
# parenting the head onto a pre-sibling base moves the merge base and
# hides an inherited reversion. A present graft file is rejected.
begin_case "20 graft file rejected"
r=$(new_repo case20)
commit_file "$r" docs/a.md "v1" "init"
git -C "$r" checkout -qb grafty main
commit_file "$r" docs/a.md "v2" "edit"
mkdir -p "$r/.git/info"
git -C "$r" rev-parse grafty >"$r/.git/info/grafts"
run_audit "$r" main grafty docs
assert_rc 4
assert_contains "graft"
rm "$r/.git/info/grafts"
run_audit "$r" main grafty docs
assert_rc 0

# --------------------------------------------------------------- case 21
# Local-attributes cloak (Codex review finding): $GIT_DIR/info/attributes
# (or a core.attributesFile) marking a file merge=union turns a real
# conflict into a clean result the forge would not produce. The local
# file is rejected; a configured attributes file is neutralized so the
# conflict stays visible.
begin_case "21 local attributes cannot cloak a conflict"
r=$(new_repo case21)
commit_file "$r" docs/a.txt "original" "init"
git -C "$r" checkout -qb unioned main
commit_file "$r" docs/a.txt "branch version" "branch edit"
git -C "$r" checkout -q main
commit_file "$r" docs/a.txt "main version" "main edit"
mkdir -p "$r/.git/info"
printf 'docs/a.txt merge=union\n' >"$r/.git/info/attributes"
run_audit "$r" main unioned docs
assert_rc 4
assert_contains "attributes"
rm "$r/.git/info/attributes"
printf 'docs/a.txt merge=union\n' >"$TMP/case21-extattrs"
git -C "$r" config core.attributesFile "$TMP/case21-extattrs"
run_audit "$r" main unioned docs
assert_rc 2
git -C "$r" config --unset core.attributesFile
run_audit "$r" main unioned docs
assert_rc 2

# --------------------------------------------------------------- case 22
# Head-committed attributes cloak (Codex review finding): merge-tree
# reads attributes from the current checkout by default, so a
# .gitattributes committed on the head branch (merge=union) makes the
# audit pass from the head checkout while a base-side merge conflicts.
# --attr-source pins attributes to the base tree: same verdict from any
# checkout, and head-side attributes take effect only once merged.
begin_case "22 head-committed attributes cannot cloak a conflict"
r=$(new_repo case22)
commit_file "$r" docs/a.txt "original" "init"
git -C "$r" checkout -qb attrhead main
commit_file "$r" docs/a.txt "branch version" "branch edit"
printf 'docs/a.txt merge=union\n' >"$r/.gitattributes"
git -C "$r" add -- .gitattributes
git -C "$r" commit -qm "add union attribute on head"
git -C "$r" checkout -q main
commit_file "$r" docs/a.txt "main version" "main edit"
# Audit from the head checkout (the PR author's usual working state),
# with the head's .gitattributes allowlisted: checkout attributes made
# the pre-fix audit return a clean PASS here.
git -C "$r" checkout -q attrhead
run_audit "$r" main attrhead docs .gitattributes
assert_rc 2
# Same verdict from the base checkout: checkout-independent.
git -C "$r" checkout -q main
run_audit "$r" main attrhead docs .gitattributes
assert_rc 2

# --------------------------------------------------------------- case 23
# Partial-clone lazy fetch (Codex review finding): a promisor clone
# fetches missing blobs over the network during merge-tree/diff-tree,
# breaking the no-network guarantee. Promisor-configured clones are
# rejected (GIT_NO_LAZY_FETCH=1 backs this up on gits that honor it).
begin_case "23 partial clone rejected"
r=$(new_repo case23)
commit_file "$r" docs/a.md "v1" "init"
commit_file "$r" docs/a.md "v2" "advance"
git -C "$r" config uploadpack.allowFilter true
git clone -q --filter=blob:none "file://$r" "$TMP/case23-partial" 2>/dev/null
run_audit "$TMP/case23-partial" main main docs
assert_rc 4
assert_contains "partial clone"

# ---------------------------------------------------------------- summary
echo
echo "passed $pass assertion(s), failed $fail"
if [ "$fail" -gt 0 ]; then
  exit 1
fi
