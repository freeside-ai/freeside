#!/usr/bin/env bash
# test-exit-root.sh — regression suite for exit-root.sh.
#
# Builds a synthetic repository under a temp directory holding exit-root.sh,
# its templates, and two commits whose implementer prompt differs, so a
# rerun on the second commit is a prompt-package change. A stand-in `go`
# writes a fake freesided whose subcommands agree with each other: `deputy`
# emits a tree patch derived from the settings' prompt package and second
# writer, and reports a mismatch until agent_tree.commit holds that tree;
# `prelaunch` derives first_missing from the same tree, a trust marker that
# `onboard -approve` writes, and the image and recipe that approval prints;
# `routing`, `set-pool-limit`, and `probe-lineup` record their arguments.
# A stand-in `lsof` reports the ports a case lists as listening. The
# synthetic repository's dev-instance.sh is a stand-in too, since
# test-dev-instance.sh covers the real one.
#
# The cases cover the settings rendering from the fixture template (every
# fixture-table value and the machine file, nothing else), the tree commit on
# its own branch, the stop at the onboarding approval and the resume after
# it, the installation-intent stop, the rerun on a new build, port selection,
# the seeded-routing overlay, the scenario pass, and the refusals that create
# nothing.
#
# Exit code: 0 when every assertion passes, 1 otherwise.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
CASE=''
CASE_DIR=''
OUT=''
RC=0
case_n=0

begin_case() {
  CASE=$1
  case_n=$((case_n + 1))
  CASE_DIR=$TMP/case-$case_n
  mkdir -p "$CASE_DIR"
  echo "case: $CASE"
}

report_failure() {
  fail=$((fail + 1))
  echo "FAIL [$CASE]: $*"
  printf '%s\n' "$OUT" | sed 's/^/    | /'
}

ok() { pass=$((pass + 1)); }

assert_rc() {
  if [ "$RC" -eq "$1" ]; then ok; else report_failure "expected rc=$1, got rc=$RC"; fi
}

assert_contains() {
  case "$OUT" in
  *"$1"*) ok ;;
  *) report_failure "output does not contain: $1" ;;
  esac
}

assert_not_contains() {
  case "$OUT" in
  *"$1"*) report_failure "output contains: $1" ;;
  *) ok ;;
  esac
}

assert_line() { # <file> <exact line>
  if grep -qxF -- "$2" "$1" 2>/dev/null; then ok; else report_failure "$(basename "$1") lacks line: $2"; fi
}

assert_eq() { # <actual> <expected> <what>
  if [[ "$1" == "$2" ]]; then ok; else report_failure "$3: expected '$2', got '$1'"; fi
}

assert_not_exists() {
  if [ -e "$1" ]; then report_failure "unexpected path exists: $1"; else ok; fi
}

# ---------------------------------------------------------------- stand-ins
STUB_BIN=$TMP/bin
mkdir -p "$STUB_BIN"
FAKE_IMAGE="registry.test/fixture@sha256:$(printf 'b%.0s' $(seq 1 64))"
FAKE_RECIPE="sha256:$(printf 'c%.0s' $(seq 1 64))"
REVIEW_DIGEST="sha256:$(printf 'd%.0s' $(seq 1 64))"
APPROVAL_DIGEST="sha256:$(printf 'a%.0s' $(seq 1 64))"
export FAKE_IMAGE FAKE_RECIPE REVIEW_DIGEST APPROVAL_DIGEST

cat >"$STUB_BIN/go" <<'GO_STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s | %s\n' "$PWD" "$*" >>"${STUB_DIR:?}/go.log"
out=''
while [[ $# -gt 0 ]]; do
  [[ "$1" != -o ]] || out=$2
  shift
done
cp "${FAKE_FREESIDED:?}" "$out"
chmod +x "$out"
GO_STUB

cat >"$STUB_BIN/lsof" <<'LSOF_STUB'
#!/usr/bin/env bash
port=''
for arg in "$@"; do
  case $arg in -iTCP:*) port=${arg#-iTCP:} ;; esac
done
for listening in ${FAKE_LISTENING:-}; do
  [[ "$listening" != "$port" ]] || exit 0
done
exit 1
LSOF_STUB
chmod +x "$STUB_BIN/go" "$STUB_BIN/lsof"

FAKE_FREESIDED=$TMP/fake-freesided
export FAKE_FREESIDED
cat >"$FAKE_FREESIDED" <<'FAKE'
#!/usr/bin/env bash
# A fake freesided: just enough of each subcommand exit-root.sh drives.
set -euo pipefail
cmd=${1:-}
shift || true
printf '%s\n' "$cmd" >>"${STUB_DIR:?}/calls.log"

flag() { # <name> <args...>: the value after -name
  local want=$1
  shift
  while [[ $# -gt 0 ]]; do
    [[ "$1" != "$want" ]] || { printf '%s\n' "$2"; return; }
    shift
  done
}

# expected_lineup <settings>: the tree content adoption would emit.
expected_lineup() {
  local prompt second
  prompt=$(jq -r '.prompts.prompt_package' "$1")
  second=$(jq -r '.agent_tree.second_writer.auth_identity // "none"' "$1")
  printf 'lineup %s %s\n' "$(cksum <"$prompt" | cut -d' ' -f1)" "$second"
}

tree_at_commit() { # <settings>
  git -C "$(jq -r '.agent_tree.checkout' "$1")" show "$(jq -r '.agent_tree.commit' "$1"):policy/lineup" 2>/dev/null || true
}

tree_matches() { # <settings>
  [[ "$(tree_at_commit "$1")" == "$(expected_lineup "$1")" ]]
}

# prelaunch_tree_ok <root>: what the real prelaunch checks, the commit
# against the patch the last deputy start wrote, not against the settings,
# so a prompt change since that start goes unseen.
prelaunch_tree_ok() {
  local have patch=$1/daemon/agent-tree.patch
  have=$(tree_at_commit "$1/daemon/settings.json")
  [[ -n "$have" ]] || return 1
  [[ ! -f "$patch" ]] || [[ "$(sed -n 's/^+lineup/lineup/p' "$patch")" == "$have" ]]
}

case $cmd in
prelaunch)
  root=$(flag -root "$@")
  settings=$root/daemon/settings.json
  printf '%s\n' "$@" >"$STUB_DIR/prelaunch.args"
  missing=null
  if ! prelaunch_tree_ok "$root" || [[ ! -f "$root/daemon/freeside.db" ]]; then
    missing=agent_tree
  elif [[ "$(cat "$root/daemon/fake-trust" 2>/dev/null)" != "$REVIEW_DIGEST" ]]; then
    missing=trust_profile
  elif [[ "$(jq -r '.images.agent' "$settings")" != "$FAKE_IMAGE" ]]; then
    missing=project_image
  elif [[ "$(jq -r '.approved_recipe' "$settings")" != "$FAKE_RECIPE" ]]; then
    missing=approved_recipe
  fi
  passed=false
  [[ "$missing" != null || "${FAKE_PRELAUNCH_MODE:-}" == fail ]] || passed=true
  jq -n --arg m "$missing" --argjson passed "$passed" '{
    passed: $passed,
    first_missing: (if $m == "null" then null else {prerequisite: $m, evidence: "fake", fix: "fake"} end),
    checks: (if $passed then [] else [{group: "rig", name: "rig", status: "failed", evidence: "fake failure"}] end)
  }' | tee "$root/daemon/prelaunch.json"
  [[ "$passed" == true ]] || exit 1
  ;;
deputy)
  root=$(flag -root "$@")
  settings=$root/daemon/settings.json
  [[ "$(stat -c %a "$settings" 2>/dev/null || stat -f %Lp "$settings")" == 600 ]] || {
    echo "freesided deputy: settings mode is not 0600" >&2
    exit 2
  }
  echo "freesided deputy: build fake" >&2
  rm -f "$root/daemon/composition-manifest.json"
  : >"$root/daemon/freeside.db"
  if [[ "${FAKE_DEPUTY_MODE:-}" == exporter ]]; then
    echo "freesided deputy: exporter_image failed; execution stays disabled (fake)" >&2
    trap 'exit 0' TERM
    echo '{"api_url":"http://x"}'
    for _ in $(seq 1 300); do sleep 0.1; done
    exit 0
  fi
  printf 'diff --git a/policy/lineup b/policy/lineup\nnew file mode 100644\n--- /dev/null\n+++ b/policy/lineup\n@@ -0,0 +1 @@\n+%s\n' \
    "$(expected_lineup "$settings")" >"$root/daemon/agent-tree.patch"
  if ! tree_matches "$settings"; then
    echo "freesided deputy: agent_tree failed; execution stays disabled (fake mismatch)" >&2
  else
    jq -n --arg d "$REVIEW_DIGEST" '{review_configuration_digest: $d}' >"$root/daemon/composition-manifest.json"
    if [[ "$(cat "$root/daemon/fake-trust" 2>/dev/null)" != "$REVIEW_DIGEST" ]]; then
      echo "freesided deputy: trust_profile failed; execution stays disabled (fake)" >&2
    else
      echo "freesided deputy: composition passed; manifest x" >&2
    fi
  fi
  if [[ "${FAKE_DEPUTY_MODE:-}" == dirty-exit ]]; then
    trap 'exit 1' TERM
  else
    trap 'exit 0' TERM
  fi
  printf '{"api_url":"http://%s","pairing_code":"ABCD","environment":"ephemeral","run_id":"r1"}\n' \
    "$(jq -r '.listen' "$settings")"
  for _ in $(seq 1 300); do sleep 0.1; done
  ;;
onboard)
  n=$(find "$STUB_DIR" -name 'onboard-*.args' | wc -l | tr -d ' ')
  printf '%s\n' "$@" >"$STUB_DIR/onboard-$((n + 1)).args"
  db=$(flag -db "$@")
  if [[ -n "$(flag -approve "$@")" ]]; then
    printf '%s\n' "$(flag -review-config-digest "$@")" >"$(dirname "$db")/fake-trust"
    jq -n --arg i "$FAKE_IMAGE" --arg r "$FAKE_RECIPE" \
      '{status: "complete", project_image: {image_ref: $i, recipe_digest: $r}}'
  elif [[ "${FAKE_ONBOARD_MODE:-}" == pending ]] && ! printf '%s\n' "$@" | grep -qx -- -resume; then
    jq -n '{status: "installation_pending", installation_url: "https://github.com/apps/fake/installations/new"}'
  else
    jq -n --arg a "$APPROVAL_DIGEST" '{status: "review_required", approval_digest: $a}'
  fi
  ;;
routing)
  printf '%s\n' "$@" >>"$STUB_DIR/routing.args"
  echo '{"recorded":true}'
  ;;
set-pool-limit)
  printf '%s\n' "$@" >>"$STUB_DIR/pool.args"
  echo '{"recorded":true}'
  ;;
probe-lineup)
  printf '%s\n' "$@" >"$STUB_DIR/probe.args"
  echo '{}'
  ;;
*)
  echo "fake freesided: no $cmd" >&2
  exit 2
  ;;
esac
FAKE
chmod +x "$FAKE_FREESIDED"

# ------------------------------------------------------- synthetic fixtures
export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.invalid
export GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.invalid
git_q() { git -c init.defaultBranch=main -c commit.gpgsign=false "$@" >/dev/null; }

REPO=$TMP/repo
mkdir -p "$REPO/scripts" "$REPO/daemon/cmd/freesided" "$REPO/prompts/phase-1a"
cp "$SCRIPT_DIR/exit-root.sh" "$SCRIPT_DIR/supervised-paths.sh" "$REPO/scripts/"
cp -R "$SCRIPT_DIR/exit-root" "$REPO/scripts/exit-root"
cat >"$REPO/scripts/dev-instance.sh" <<'DEV'
#!/usr/bin/env bash
printf '%s\n' "$@" >"$STUB_DIR/dev-instance.args"
root=''
while [[ $# -gt 0 ]]; do
  [[ "$1" != --root ]] || root=$2
  shift
done
mkdir "$root"
printf 'root=%s\napi_url=http://fake\ntask=task-scenario\n' "$root"
DEV
printf 'package main\n' >"$REPO/daemon/cmd/freesided/main.go"
for role in implementer specifier remediator; do printf '%s prompt v1\n' "$role" >"$REPO/prompts/phase-1a/$role.md"; done
git_q -C "$REPO" init
git_q -C "$REPO" add -A
git_q -C "$REPO" commit -m one
SHA1=$(git -C "$REPO" rev-parse HEAD)
printf 'implementer prompt v2\n' >"$REPO/prompts/phase-1a/implementer.md"
git_q -C "$REPO" commit -am two
SHA2=$(git -C "$REPO" rev-parse HEAD)

TREE=$TMP/agent-tree
git_q init "$TREE"
git_q -C "$TREE" commit --allow-empty -m 'Start the agent tree'
TREE_HEAD=$(git -C "$TREE" rev-parse HEAD)
printf 'operator work in progress\n' >"$TREE/uncommitted.txt"

CLONE=$TMP/fixture-clone
mkdir -p "$CLONE"
REPO_SLUG=freeasinbird/freeside-review-challenge

# make_machine <file> <roots_dir> [jq filter]: the example machine file with
# the paths a case needs, then the case's own edit.
make_machine() {
  jq --arg roots "$2" --arg tree "$TREE" --arg clone "$CLONE" --arg r "$REPO_SLUG" \
    --arg image "registry.test/placeholder@sha256:$(printf 'e%.0s' $(seq 1 64))" \
    --arg recipe "sha256:$(printf 'f%.0s' $(seq 1 64))" '
    .exit_root.roots_dir = $roots | .exit_root.listen_host = "100.64.0.9" | .exit_root.port_base = 9100
    | .exit_root.placeholder = {image: $image, recipe: $recipe}
    | .exit_root.repositories[$r].checkout = $clone
    | .exit_root.second_writer.auth_identity = "claude-second"
    | .settings.agent_tree.checkout = $tree | .settings.agent_tree.auth_identity = "claude-first"
    | '"${3:-.}" "$REPO/scripts/exit-root/machine.example.json" >"$1"
}

FAKE_HOME=$TMP/home
mkdir -p "$FAKE_HOME"

# run_exit_root [arg...]: runs the script to completion with the stand-ins.
run_exit_root() {
  RC=0
  STUB_DIR=$CASE_DIR PATH="$STUB_BIN:$PATH" HOME=$FAKE_HOME \
    bash "$REPO/scripts/exit-root.sh" "$@" >"$CASE_DIR/stdout" 2>"$CASE_DIR/stderr" || RC=$?
  OUT=$(cat "$CASE_DIR/stdout" "$CASE_DIR/stderr")
}

# approve: runs the approval= command the last run printed, as the operator.
approve() {
  local command
  command=$(sed -n 's/^approval=//p' "$CASE_DIR/stdout")
  STUB_DIR=$CASE_DIR PATH="$STUB_BIN:$PATH" bash -c "$command"
}

# The deputy settings keys (cmd/freesided/deputy_settings.go): the rendered
# fixture settings carry exactly these, so the template and the example
# machine file together leave none out.
SETTINGS_KEYS='agent_tree.auth_identity agent_tree.auth_volume agent_tree.checkout agent_tree.claude_account
agent_tree.commit agent_tree.cost_owner agent_tree.offer_not_after agent_tree.pricing_revision
agent_tree.review_auth_identity agent_tree.review_cost_owner agent_tree.review_harness agent_tree.terms_basis_date
approved_recipe build.base_build_ref build.dns build.proxy images.agent images.exporter images.review
judgment.claude_bin judgment.claude_sha256 judgment.effort judgment.model judgment.publication_author_prompt
listen manual_submission_config memory_budget_mib prod.app_credentials_dir prod.app_state_dir
prod.installed_daemon prompts.prompt_package prompts.remediation_prompt_package
prompts.specification_prompt_package prompts.vendor_instructions repository.allowed_paths repository.base_ref
repository.base_sha repository.checkout repository.project repository.repo repository.repository_id
review.auth_mode review.auth_snapshot review.effort review.input_root review.instructions review.model
seed_root timeouts.writer_stall timeouts.writer_stop version writers.implementer.effort
writers.implementer.model writers.remediator.effort writers.remediator.model writers.specifier.effort
writers.specifier.model'

# -------------------------------------------------------------------- cases
ROOTS=$TMP/roots
MACHINE=$TMP/machine.json
make_machine "$MACHINE" "$ROOTS"
FIX=$ROOTS/fix

begin_case "a fresh fixture root commits its tree and stops at the onboarding approval"
run_exit_root --pass fixture --build "$SHA1" --machine "$MACHINE" fix
assert_rc 3
assert_line "$CASE_DIR/stdout" "root=$FIX"
assert_line "$CASE_DIR/stdout" 'api_url=http://100.64.0.9:9100'
approval=$(sed -n 's/^approval=//p' "$CASE_DIR/stdout")
case $approval in
*" onboard $REPO_SLUG "*"-approve $APPROVAL_DIGEST > $FIX/daemon/onboard-approved.json") ok ;;
*) report_failure "approval is not the approve command: $approval" ;;
esac
if grep -qx -- -approve "$CASE_DIR"/onboard-*.args; then report_failure "the script itself passed -approve"; else ok; fi
assert_line "$CASE_DIR/onboard-1.args" "$REVIEW_DIGEST"
assert_line "$CASE_DIR/onboard-1.args" "$CLONE"
assert_line "$CASE_DIR/onboard-1.args" '584dba6120ee6e126460438032c3189762603306'
assert_line "$CASE_DIR/onboard-1.args" '-image-name'
assert_line "$CASE_DIR/onboard-1.args" '-registration-id'
if grep -q "main.version=$SHA1" "$CASE_DIR/go.log" && grep -q "^$FIX/builds/.partial.*/src/daemon |" "$CASE_DIR/go.log"; then
  ok
else
  report_failure "freesided not built stamped from the extracted tree: $(cat "$CASE_DIR/go.log")"
fi
settings=$FIX/daemon/settings.json
mode=$(stat -c %a "$settings" 2>/dev/null || stat -f %Lp "$settings" 2>/dev/null || true)
assert_eq "$mode" 600 'settings mode'
tip=$(git -C "$TREE" rev-parse --verify --quiet refs/heads/exit-root/fixture || true)
assert_eq "$(jq -r .tree_commit "$FIX/exit-root.json")" "$tip" 'state tree commit is the branch tip'
assert_eq "$(git -C "$TREE" rev-parse "$tip^")" "$TREE_HEAD" 'tree commit parent'
assert_eq "$(git -C "$TREE" show "$tip:policy/lineup")" \
  "lineup $(cksum <"$FIX/builds/$SHA1/src/prompts/phase-1a/implementer.md" | cut -d' ' -f1) none" 'tree holds the patch'
assert_eq "$(git -C "$TREE" rev-parse HEAD)" "$TREE_HEAD" 'agent-tree HEAD unchanged'
assert_eq "$(git -C "$TREE" symbolic-ref --short HEAD)" main 'agent-tree branch unchanged'
assert_eq "$(git -C "$TREE" status --porcelain)" '?? uncommitted.txt' 'agent-tree working tree unchanged'
prompts=$FIX/builds/$SHA1/src/prompts/phase-1a
expected=$(jq -S -n --slurpfile m "$MACHINE" --slurpfile t "$REPO/scripts/exit-root/passes/fixture/settings.json" \
  --arg root "$FIX" --arg p "$prompts" --arg clone "$CLONE" --arg tip "$tip" '
  $m[0].settings * $t[0] * {
    version: "freeside-deputy-settings-v1", listen: "100.64.0.9:9100", seed_root: ($root + "/seed"),
    prompts: {prompt_package: ($p + "/implementer.md"), specification_prompt_package: ($p + "/specifier.md"),
      remediation_prompt_package: ($p + "/remediator.md")},
    repository: {checkout: $clone}, agent_tree: {commit: $tip},
    images: {agent: $m[0].exit_root.placeholder.image}, approved_recipe: $m[0].exit_root.placeholder.recipe}')
assert_eq "$(jq -S . "$settings")" "$expected" 'rendered settings'
assert_eq "$(jq -r '.repository.base_sha, .writers.implementer.model, .review.model, .repository.project' "$settings" | tr '\n' ' ')" \
  '584dba6120ee6e126460438032c3189762603306 claude-haiku-5-5 gpt-6-luna fixture ' 'fixture-table values'
keys=$(jq -r '[paths(type != "object") | select(all(.[]; type == "string")) | join(".")] | sort | join(" ")' "$settings")
assert_eq "$keys" "$(tr -s ' \n' '\n' <<<"$SETTINGS_KEYS" | sort | tr '\n' ' ' | sed 's/ $//')" 'settings key set'
assert_eq "$(jq -r .commit_author.app_slug "$FIX/inputs/publication.json")" '<prod App slug>' 'publication commit author'
assert_eq "$(cat "$FIX/inputs/task.md")" 'https://github.com/freeasinbird/freeside-review-challenge/issues/2' 'task input'
FIX_APPROVAL_DIR=$CASE_DIR

begin_case "after the approval, a rerun sets the image and recipe, probes, and is ready"
CASE_DIR_SAVED=$CASE_DIR
CASE_DIR=$FIX_APPROVAL_DIR approve
CASE_DIR=$CASE_DIR_SAVED
run_exit_root --pass fixture --build "$SHA1" --machine "$MACHINE" fix
assert_rc 0
assert_line "$CASE_DIR/stdout" "root=$FIX"
assert_line "$CASE_DIR/stdout" 'api_url=http://100.64.0.9:9100'
if grep -q '^approval=' "$CASE_DIR/stdout"; then report_failure "ready root printed an approval"; else ok; fi
assert_eq "$(jq -r '.images.agent, .approved_recipe' "$FIX/daemon/settings.json" | tr '\n' ' ')" \
  "$FAKE_IMAGE $FAKE_RECIPE " 'image and recipe from the approval'
assert_line "$CASE_DIR/probe.args" "$FIX"
assert_not_exists "$CASE_DIR/routing.args"
assert_not_exists "$CASE_DIR/go.log"
assert_not_exists "$CASE_DIR/onboard-1.args"
assert_line "$CASE_DIR/prelaunch.args" '-no-live'
assert_line "$CASE_DIR/prelaunch.args" "$FIX/inputs/policy.json"

begin_case "a rerun on a new build rebuilds, re-adopts, and keeps the root, database, and port"
old_tip=$(git -C "$TREE" rev-parse exit-root/fixture)
run_exit_root --pass fixture --build "$SHA2" --machine "$MACHINE" fix
assert_rc 0
if grep -q "main.version=$SHA2" "$CASE_DIR/go.log"; then ok; else report_failure "no rebuild at the new SHA"; fi
new_tip=$(git -C "$TREE" rev-parse exit-root/fixture)
if [[ "$new_tip" != "$old_tip" && "$(git -C "$TREE" rev-parse "$new_tip^")" == "$old_tip" ]]; then ok; else report_failure "tree not re-adopted on the branch"; fi
assert_eq "$(jq -r '.port, .build, .tree_commit' "$FIX/exit-root.json" | tr '\n' ' ')" "9100 $SHA2 $new_tip " 'state after the rebuild'
assert_eq "$(jq -r .prompts.prompt_package "$FIX/daemon/settings.json")" "$FIX/builds/$SHA2/src/prompts/phase-1a/implementer.md" 'new prompt package'
assert_eq "$(jq -r .images.agent "$FIX/daemon/settings.json")" "$FAKE_IMAGE" 'image kept'
if [[ -f "$FIX/daemon/freeside.db" ]]; then ok; else report_failure "database gone"; fi
assert_not_exists "$CASE_DIR/onboard-1.args"
assert_line "$CASE_DIR/stdout" 'api_url=http://100.64.0.9:9100'

begin_case "a rerun with nothing changed starts no deputy and commits nothing"
old_tip=$(git -C "$TREE" rev-parse exit-root/fixture)
run_exit_root --pass fixture --build "$SHA2" --machine "$MACHINE" fix
assert_rc 0
if grep -qx deputy "$CASE_DIR/calls.log"; then report_failure "an unchanged rerun started the deputy"; else ok; fi
assert_eq "$(git -C "$TREE" rev-parse exit-root/fixture)" "$old_tip" 'tree branch unchanged'
assert_not_exists "$CASE_DIR/go.log"

begin_case "a rerun is refused while something listens on the root's port"
FAKE_LISTENING=9100 run_exit_root --pass fixture --build "$SHA2" --machine "$MACHINE" fix
assert_rc 2
assert_contains "port 9100 of $FIX is in use"
assert_not_exists "$CASE_DIR/calls.log"

begin_case "a root made for another pass is refused"
run_exit_root --pass seeded-routing --build "$SHA2" --machine "$MACHINE" fix
assert_rc 2
assert_contains 'was made for the "fixture" pass'

begin_case "a new root skips a listening port and a port another root claims"
FAKE_LISTENING=9101 run_exit_root --pass fixture --build "$SHA1" --machine "$MACHINE" fix-ports
assert_rc 3
assert_eq "$(jq -r .port "$ROOTS/fix-ports/exit-root.json")" 9102 'chosen port'
assert_line "$CASE_DIR/stdout" 'api_url=http://100.64.0.9:9102'

begin_case "a key the template and the machine file both set is refused before the root is created"
M=$CASE_DIR/machine.json
make_machine "$M" "$CASE_DIR/roots" '.settings.repository = {base_ref: "other"}'
run_exit_root --pass fixture --build "$SHA1" --machine "$M" clash
assert_rc 2
assert_contains 'both set: repository.base_ref'
assert_not_exists "$CASE_DIR/roots"
assert_not_exists "$CASE_DIR/go.log"

begin_case "a computed key in the machine file is refused before the root is created"
M=$CASE_DIR/machine.json
make_machine "$M" "$CASE_DIR/roots" '.settings.listen = "127.0.0.1:1"'
run_exit_root --pass fixture --build "$SHA1" --machine "$M" computed
assert_rc 2
assert_contains 'sets keys this script computes: listen'
assert_not_exists "$CASE_DIR/roots"

begin_case "an onboard flag the script sets is refused in the machine file"
M=$CASE_DIR/machine.json
make_machine "$M" "$CASE_DIR/roots" '.exit_root.onboard_args += ["-approve", "x"]'
run_exit_root --pass fixture --build "$SHA1" --machine "$M" approve
assert_rc 2
assert_contains 'without a flag the script sets or -approve'
assert_not_exists "$CASE_DIR/roots"

begin_case "a root under a supervised state root is refused and nothing is created"
M=$CASE_DIR/machine.json
SUPERVISED="$FAKE_HOME/Library/Application Support/Freeside Dev/roots"
make_machine "$M" "$SUPERVISED"
run_exit_root --pass fixture --build "$SHA1" --machine "$M" sup
assert_rc 2
assert_contains 'refusing exit root'
assert_contains 'resolves under the dev state root'
assert_not_exists "$FAKE_HOME/Library"
assert_not_exists "$CASE_DIR/go.log"

begin_case "an installation intent stops for the installation, then resumes with -resume"
FAKE_ONBOARD_MODE=pending run_exit_root --pass fixture --build "$SHA1" --machine "$MACHINE" pending
assert_rc 3
assert_contains 'approval=complete the GitHub App installation for freeasinbird/freeside-review-challenge at https://github.com/apps/fake/installations/new'
FAKE_ONBOARD_MODE=pending run_exit_root --pass fixture --build "$SHA1" --machine "$MACHINE" pending
assert_rc 3
assert_line "$CASE_DIR/onboard-2.args" '-resume'
case $(sed -n 's/^approval=//p' "$CASE_DIR/stdout") in
*" -resume -approve $APPROVAL_DIGEST "*) ok ;;
*) report_failure "the approve command lacks -resume" ;;
esac

begin_case "a deputy that does not exit 0 fails with the rig-recover hint"
FAKE_DEPUTY_MODE=dirty-exit run_exit_root --pass fixture --build "$SHA1" --machine "$MACHINE" dirty
assert_rc 1
assert_contains 'the deputy exited 1; it may have left the rig lease'
assert_contains "rig recover -state-root $ROOTS/dirty/daemon -confirm"

begin_case "a start that stops before the tree comparison names its failure"
FAKE_DEPUTY_MODE=exporter run_exit_root --pass fixture --build "$SHA1" --machine "$MACHINE" exporter
assert_rc 1
assert_contains 'stopped before its tree comparison: freesided deputy: exporter_image failed'
assert_not_exists "$CASE_DIR/onboard-1.args"

begin_case "prelaunch failures with nothing left to onboard fail before the probe"
FAKE_PRELAUNCH_MODE=fail run_exit_root --pass fixture --build "$SHA2" --machine "$MACHINE" fix
assert_rc 1
assert_contains 'prelaunch failed with nothing left to onboard'
assert_contains 'rig rig: fake failure'
assert_not_exists "$CASE_DIR/probe.args"

begin_case "the seeded-routing overlay adds the second writer, routing, and pool limits"
run_exit_root --pass seeded-routing --build "$SHA1" --machine "$MACHINE" seeded
assert_rc 3
SEEDED=$ROOTS/seeded
assert_eq "$(jq -c .agent_tree.second_writer "$SEEDED/daemon/settings.json")" "$(jq -c .exit_root.second_writer "$MACHINE")" 'second writer'
assert_eq "$(jq -r .repository.project "$SEEDED/daemon/settings.json")" fixture 'fixture settings inherited'
if git -C "$TREE" rev-parse --verify --quiet exit-root/seeded-routing >/dev/null; then ok; else report_failure "no seeded-routing tree branch"; fi
approve
run_exit_root --pass seeded-routing --build "$SHA1" --machine "$MACHINE" seeded
assert_rc 0
assert_line "$CASE_DIR/routing.args" 'specifier=claude-second/claude_code,claude-first/claude_code'
assert_line "$CASE_DIR/routing.args" 'implementer=claude-second/claude_code,claude-first/claude_code'
assert_line "$CASE_DIR/routing.args" "$(jq -r .tree_commit "$SEEDED/exit-root.json")"
assert_line "$CASE_DIR/pool.args" 'identity:claude-first'
assert_line "$CASE_DIR/pool.args" 'identity:claude-second'
next=$(sed -n 's/^next=//p' "$CASE_DIR/stdout")
case $next in
*"auth seed-exhausted -db $SEEDED/daemon/freeside.db -pool identity:claude-second -window five_hour -model claude-haiku-5-5 -resets-at"*) ok ;;
*) report_failure "next is not the seed command: $next" ;;
esac

begin_case "a tree branch checked out in a worktree is refused, not moved"
git -C "$TREE" worktree add -q "$TMP/seeded-worktree" exit-root/seeded-routing
held_tip=$(git -C "$TREE" rev-parse exit-root/seeded-routing)
run_exit_root --pass seeded-routing --build "$SHA2" --machine "$MACHINE" seeded
assert_rc 2
assert_contains 'exit-root/seeded-routing is checked out in a worktree'
assert_eq "$(git -C "$TREE" rev-parse exit-root/seeded-routing)" "$held_tip" 'checked-out branch kept'
git -C "$TREE" worktree remove "$TMP/seeded-worktree"

begin_case "a scenario pass runs the build's dev-instance.sh on a retained root and a picked port"
run_exit_root --pass scenario:quota-park --build "$SHA1" --machine "$MACHINE" park
assert_rc 0
SCEN=$ROOTS/park
port=$(jq -r .port "$SCEN/exit-root.json")
assert_line "$CASE_DIR/dev-instance.args" '--daemon-only'
assert_line "$CASE_DIR/dev-instance.args" 'quota-park'
assert_line "$CASE_DIR/dev-instance.args" "$SCEN/instance"
assert_line "$CASE_DIR/dev-instance.args" "100.64.0.9:$port"
assert_line "$CASE_DIR/stdout" "root=$SCEN/instance"
assert_contains "approval=approve the quota-park scenario's specification in a paired client"
assert_not_exists "$CASE_DIR/go.log"
if [[ "$port" != 9100 ]]; then ok; else report_failure "scenario took a claimed port"; fi
run_exit_root --pass scenario:quota-park --build "$SHA1" --machine "$MACHINE" park
assert_rc 2
assert_contains 'single-use'

begin_case "usage errors and an unknown pass create nothing"
run_exit_root --pass fixture --machine "$MACHINE" nobuild
assert_rc 2
assert_contains 'usage: exit-root.sh'
run_exit_root --pass nope --build "$SHA1" --machine "$MACHINE" nope
assert_rc 2
assert_contains 'no pass directory'
assert_not_exists "$ROOTS/nope"
run_exit_root --pass fixture --build deadbeef --machine "$MACHINE" badsha
assert_rc 2
assert_contains 'is not a commit'
assert_not_exists "$ROOTS/badsha"

echo
echo "passed $pass assertion(s), failed $fail"
if [ "$fail" -gt 0 ]; then
  exit 1
fi
