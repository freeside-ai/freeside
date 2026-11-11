#!/usr/bin/env bash
# exit-root.sh — stand up an exit-run root for one pass with one command.
#
# Usage: exit-root.sh --pass PASS --build SHA [--machine FILE] [--template DIR] NAME
#
# Creates <roots_dir>/NAME (roots_dir from the machine file) for a deputy
# pass, such as `fixture` or `seeded-routing`, or a scenario pass,
# `scenario:<name>`, on a freesided built from a clean tree of SHA. It is
# resumable: each run reads the root's state file, <root>/exit-root.json, does
# every step it can, and stops at the first approval with the exact command
# or client action to take. Running it again afterwards continues from there.
# docs/production-walkthrough.md (Rehearse Before The Real Run) is the
# procedure this automates; issue #3061 is its contract.
#
#   --pass PASS      a pass under scripts/exit-root/passes/, the label of an
#                    operator-written --template, or scenario:<name>
#   --build SHA      the commit to build; reruns on a new SHA rebuild and
#                    re-adopt the tree, keeping the root, database, and port
#   --machine FILE   machine-bound values (default
#                    $HOME/.config/freeside/exit-root-machine.json; see
#                    scripts/exit-root/machine.example.json). Never written.
#   --template DIR   a pass directory of the operator's own, for the real run
#
# A pass directory holds settings.json (deputy settings keys), the submit
# inputs (task.md, policy.json, publication.json, work-unit.json), and an
# optional pass.json: `base` (a bundled pass whose settings and inputs it
# extends), `second_writer` (true adds the machine's second writer),
# `routing` and `pool_limits` (by the symbols `writer` and `second_writer`),
# and `seed_exhausted` (the symbol whose pool the pass seeds before submit).
#
# A deputy pass, in order, each step skipped once done:
#   1. Refuse a root under a supervised state root (supervised-paths.sh), a
#      root another pass made, and settings that a template and the machine
#      file both set, or that set a key this script computes, all before
#      anything is created.
#   2. Pick the lowest free port from port_base that no other root under
#      roots_dir claims, and keep it on every rerun.
#   3. Build <root>/builds/<sha>/freesided from `git archive`, stamped with
#      -X main.version=<sha> (preflight's daemon_build check needs the stamp).
#   4. Render <root>/daemon/settings.json, mode 0600, from the machine file,
#      the template, and the state file, on every run.
#   5. When the settings differ from the last start that composed (a new
#      root, a new build's prompt packages, a changed machine file, a new
#      image), start the deputy once and stop it; on a tree mismatch, commit
#      its patch on branch exit-root/<pass> of the agent-tree checkout, never
#      touching the checkout's current branch or working tree, and start
#      again. Prelaunch can't see such a change, since it compares the tree
#      only with the last start's patch. A start that composes enables
#      execution until it is stopped, so on a root that already ran tasks a
#      rerun may resume one; reruns are for iterating, and a counted pass
#      uses a fresh root.
#   6. Run `freesided prelaunch -no-live` and act on its first_missing:
#      trust_profile: run the onboarding review (never -approve) and stop
#        with the approval command;
#      project_image, approved_recipe: take both from the approving run's
#        output, <root>/daemon/onboard-approved.json.
#   7. Apply the pass's routing order and pool limits, run
#      `freesided probe-lineup` (one short call per role), and require
#      `prelaunch -no-live` to pass.
#
# A scenario pass extracts the build's source tree and execs its
# scripts/dev-instance.sh --daemon-only --scenario <name> --root
# <root>/instance --listen <listen_host>:<port>, which stays in the
# foreground; a scenario instance is single-use, so a rerun is refused.
#
# Stdout: root=<root>, api_url=<url> once known, one approval=<step> per
# approval still needed, and next=<command> for a step the operator runs
# just before submit. A scenario pass prints its approval= line, then
# dev-instance.sh's own root= (the instance), api_url=, and task= lines.
# Progress goes to stderr; logs go to <root>/logs.
#
# Exit codes: 0 when the root is ready, 3 when it stopped for an approval,
# 2 on a usage error or refusal, 1 on any other failure. A scenario pass
# exits with dev-instance.sh's code.
#
# Bash 3.2 compatible (macOS /bin/bash). Needs jq, git, go, and lsof.
set -euo pipefail

usage() {
  echo 'usage: exit-root.sh --pass PASS --build SHA [--machine FILE] [--template DIR] NAME' >&2
  exit 2
}
say() { echo "exit-root: $*" >&2; }
refuse() {
  say "$*"
  exit 2
}
die() {
  say "$*"
  exit 1
}

pass=''
build=''
machine="${HOME:-}/.config/freeside/exit-root-machine.json"
template=''
name=''
while [[ $# -gt 0 ]]; do
  case $1 in
  --pass | --build | --machine | --template)
    [[ $# -ge 2 && -n "$2" ]] || usage
    case $1 in
    --pass) pass=$2 ;;
    --build) build=$2 ;;
    --machine) machine=$2 ;;
    --template) template=$2 ;;
    esac
    shift
    ;;
  -*) usage ;;
  *)
    [[ -z "$name" ]] || usage
    name=$1
    ;;
  esac
  shift
done
[[ -n "$pass" && -n "$build" && -n "$name" ]] || usage

repo_root="$(cd -P "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
passes_dir="$repo_root/scripts/exit-root/passes"
# shellcheck source=scripts/supervised-paths.sh
source "$repo_root/scripts/supervised-paths.sh"
for tool in jq git go lsof; do
  # Without lsof every port would look free.
  command -v "$tool" >/dev/null || refuse "$tool is required"
done

[[ "$name" =~ ^[A-Za-z0-9._-]+$ && "$name" != . && "$name" != .. ]] ||
  refuse "root name \"$name\" must be letters, digits, '.', '_', or '-'"
scenario=''
case $pass in
scenario:*)
  scenario=${pass#scenario:}
  [[ "$scenario" =~ ^[a-z0-9-]+$ ]] || refuse "scenario name \"$scenario\" is not a scenario name"
  [[ -z "$template" ]] || refuse '--template does not apply to a scenario pass'
  ;;
*)
  # The pass names the agent-tree branch, so it must be a plain ref segment.
  [[ "$pass" =~ ^[a-z0-9][a-z0-9-]*$ ]] || refuse "pass name \"$pass\" must be lowercase letters, digits, and '-'"
  ;;
esac

[[ -f "$machine" ]] || refuse "no machine file at $machine (copy scripts/exit-root/machine.example.json there and fill it in)"
jq -e 'type == "object" and (.exit_root | type == "object") and ((.settings // {}) | type == "object")' \
  "$machine" >/dev/null 2>&1 || refuse "machine file $machine must be a JSON object with an exit_root object"
mval() { jq -r "$1 // empty" "$machine"; }
roots_dir=$(mval .exit_root.roots_dir)
listen_host=$(mval .exit_root.listen_host)
port_base=$(mval .exit_root.port_base)
# Settings paths must be absolute and clean, and every root path is built
# from roots_dir.
[[ "$roots_dir" == /* && "$roots_dir" != */ && "$roots_dir" != *//* && "$roots_dir" != */./* &&
  "$roots_dir" != */../* && "$roots_dir" != */. && "$roots_dir" != */.. ]] ||
  refuse "exit_root.roots_dir \"$roots_dir\" must be an absolute, clean path"
[[ -n "$listen_host" ]] || refuse 'exit_root.listen_host is required'
if ! [[ "$port_base" =~ ^[0-9]+$ ]] || ((port_base < 1024 || port_base > 65535)); then
  refuse 'exit_root.port_base must be a port from 1024 to 65535'
fi

build_sha=$(git -C "$repo_root" rev-parse --verify --quiet "$build^{commit}") ||
  refuse "--build $build is not a commit in $repo_root; fetch it first"

root="$roots_dir/$name"
state="$root/exit-root.json"
logs="$root/logs"

work=$(mktemp -d)
partial=''
cleanup() {
  rm -rf -- "$work"
  [[ -z "$partial" ]] || rm -rf -- "$partial"
}
trap cleanup EXIT

# ------------------------------------------------------------ the template
# Settings paths the script sets itself; a template or machine file that sets
# one, or a key inside one, is refused.
computed_paths='[["version"],["listen"],["seed_root"],["prompts","prompt_package"],
  ["prompts","specification_prompt_package"],["prompts","remediation_prompt_package"],
  ["repository","checkout"],["agent_tree","commit"],["agent_tree","second_writer"],
  ["images","agent"],["approved_recipe"]]'
# leaves lists every key path that holds a non-object or an empty object;
# arrays are leaves. clash names each path in one list that equals, or sits
# above or below, a path in the other.
# shellcheck disable=SC2016 # jq's variables, not the shell's
jq_lib='def leaves: [paths(type != "object" or length == 0) | select(all(.[]; type == "string"))];
def prefix($a; $b): ($a | length) <= ($b | length) and $b[0:($a | length)] == $a;
def clash($ps; $qs): [$ps[] as $p | $qs[] as $q | select(prefix($p; $q) or prefix($q; $p)) | $p | join(".")] | unique;'

# merge_refusing_clash <label> <a.json> <b.json> <out.json>
merge_refusing_clash() {
  local clashes
  clashes=$(jq -n -r --slurpfile a "$2" --slurpfile b "$3" "$jq_lib"'
    clash($a[0] | leaves; $b[0] | leaves) | join(", ")')
  [[ -z "$clashes" ]] || refuse "$1 both set: $clashes"
  jq -n --slurpfile a "$2" --slurpfile b "$3" '$a[0] * $b[0]' >"$4"
}

# pass_file <file>: the pass directory's file, else its base's.
pass_file() {
  if [[ -f "$template_dir/$1" ]]; then
    printf '%s\n' "$template_dir/$1"
  elif [[ -n "$base_dir" && -f "$base_dir/$1" ]]; then
    printf '%s\n' "$base_dir/$1"
  fi
}

inputs=(task.md policy.json publication.json work-unit.json)
if [[ -z "$scenario" ]]; then
  template_dir=${template:-$passes_dir/$pass}
  [[ -d "$template_dir" ]] || refuse "no pass directory at $template_dir"
  template_dir=$(cd -P "$template_dir" && pwd -P)
  pass_json='{}'
  if [[ -f "$template_dir/pass.json" ]]; then
    pass_json=$(jq -c . "$template_dir/pass.json") || refuse "$template_dir/pass.json is not JSON"
  fi
  base_dir=''
  base=$(jq -r '.base // empty' <<<"$pass_json")
  if [[ -n "$base" ]]; then
    base_dir="$passes_dir/$base"
    [[ -d "$base_dir" ]] || refuse "pass.json names base \"$base\", which is not a pass under $passes_dir"
    [[ ! -f "$base_dir/pass.json" ]] || ! jq -e 'has("base")' "$base_dir/pass.json" >/dev/null ||
      refuse "base pass \"$base\" has a base of its own"
  fi
  for input in settings.json "${inputs[@]}"; do
    [[ -n "$(pass_file "$input")" ]] || refuse "the $pass pass has no $input"
  done

  printf '{}\n' >"$work/template.json"
  [[ -z "$base_dir" || ! -f "$base_dir/settings.json" ]] || cp "$base_dir/settings.json" "$work/template.json"
  if [[ -f "$template_dir/settings.json" ]]; then
    merge_refusing_clash "the $pass pass and its base" "$work/template.json" "$template_dir/settings.json" "$work/t.json"
    mv "$work/t.json" "$work/template.json"
  fi
  jq '.settings // {}' "$machine" >"$work/machine-settings.json"
  merge_refusing_clash "the $pass template and the machine file" "$work/template.json" \
    "$work/machine-settings.json" "$work/sources.json"
  computed_clash=$(jq -r --argjson c "$computed_paths" "$jq_lib"'clash(leaves; $c) | join(", ")' "$work/sources.json")
  [[ -z "$computed_clash" ]] || refuse "the template or machine file sets keys this script computes: $computed_clash"

  repo=$(jq -r '.repository.repo // empty' "$work/sources.json")
  [[ -n "$repo" ]] || refuse 'neither the template nor the machine file sets repository.repo'
  repo_checkout=$(jq -r --arg r "$repo" '.exit_root.repositories[$r].checkout // empty' "$machine")
  [[ "$repo_checkout" == /* ]] || refuse "the machine file names no absolute exit_root.repositories[\"$repo\"].checkout"
  tree_checkout=$(jq -r '.agent_tree.checkout // empty' "$work/sources.json")
  [[ "$tree_checkout" == /* ]] || refuse 'agent_tree.checkout must be an absolute path'
  second_writer=$(jq -r '.second_writer // false' <<<"$pass_json")
  if [[ "$second_writer" == true ]]; then
    jq -e '.exit_root.second_writer | type == "object" and
      ([.auth_identity, .auth_volume, .claude_account, .cost_owner] | all(type == "string" and length > 0))' \
      "$machine" >/dev/null || refuse 'the pass needs exit_root.second_writer with auth_identity, auth_volume, claude_account, and cost_owner'
  fi
  # The pass names enrollments and pools only through these two symbols.
  jq -e '[(.routing // {} | .[][]), (.pool_limits // {} | keys[]), (.seed_exhausted // empty)]
    | all(. == "writer" or . == "second_writer")' <<<"$pass_json" >/dev/null ||
    refuse 'pass.json routing, pool_limits, and seed_exhausted take only "writer" and "second_writer"'
  jq -e '[(.routing // {} | .[][]), (.pool_limits // {} | keys[]), (.seed_exhausted // empty)]
    | all(. != "second_writer")' <<<"$pass_json" >/dev/null || [[ "$second_writer" == true ]] ||
    refuse 'pass.json names second_writer without "second_writer": true'
  # The script passes these itself; -approve stays the operator's.
  # shellcheck disable=SC2016 # $r is jq's
  for list in '.exit_root.onboard_args' '.exit_root.repositories[$r].onboard_args'; do
    jq -e --arg r "$repo" "($list // []) | type == \"array\" and all(type == \"string\") and
      all(. != \"-db\" and . != \"-repository-id\" and . != \"-commit\" and . != \"-base-ref\" and
        . != \"-source\" and . != \"-review-config-digest\" and . != \"-approve\" and . != \"-resume\" and
        . != \"-replace-intent\")" "$machine" >/dev/null ||
      refuse "$list must be an array of strings without a flag the script sets or -approve"
  done
fi

# --------------------------------------------------------- refuse and create
refuse_supervised_path "exit root" "$root" || exit 2

state_get() { jq -r "$1 // empty" "$state"; }
# state_set <key> <json value>
state_set() {
  jq --arg k "$1" --argjson v "$2" '.[$k] = $v' "$state" >"$state.tmp"
  mv "$state.tmp" "$state"
}

# port_listening <port>: whether anything listens on the port, any address.
port_listening() { lsof -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1; }

if [[ -e "$root" || -L "$root" ]]; then
  [[ -f "$state" ]] || refuse "$root exists but holds no exit-root.json; name a new root"
  recorded_pass=$(state_get .pass)
  [[ "$recorded_pass" == "$pass" ]] || refuse "$root was made for the \"$recorded_pass\" pass, not \"$pass\""
  port=$(state_get .port)
  ! port_listening "$port" ||
    refuse "port $port of $root is in use; stop that root's deputy (or whatever holds the port) and rerun"
  say "resuming $root (port $port)"
else
  mkdir -p "$roots_dir"
  claimed=$(find "$roots_dir" -mindepth 2 -maxdepth 2 -name exit-root.json -exec jq -r '.port // empty' {} \; 2>/dev/null || true)
  port=$port_base
  while port_listening "$port" || grep -qx -- "$port" <<<"$claimed"; do
    port=$((port + 1))
    ((port <= 65535)) || die "no free port from $port_base"
  done
  mkdir -m 0700 "$root"
  mkdir -m 0700 "$root/daemon" "$root/seed" "$logs"
  jq -n --arg pass "$pass" --argjson port "$port" '{version: "exit-root-v1", pass: $pass, port: $port}' >"$state"
  say "created $root (port $port)"
fi
mkdir -p "$logs" "$root/builds"
listen="$listen_host:$port"

# ------------------------------------------------------------------- build
build_dir="$root/builds/$build_sha"
src="$build_dir/src"
freesided="$build_dir/freesided"
if [[ ! -d "$build_dir" ]]; then
  partial=$(mktemp -d "$root/builds/.partial.XXXXXX")
  mkdir "$partial/src"
  say "extracting $build_sha"
  git -C "$repo_root" archive "$build_sha" | tar -x -C "$partial/src"
  if [[ -z "$scenario" ]]; then
    say "building freesided at $build_sha (log: $logs/build.log)"
    (cd "$partial/src/daemon" && go build -ldflags "-X main.version=$build_sha" -o "$partial/freesided" ./cmd/freesided) \
      >"$logs/build.log" 2>&1 || {
      tail -n 20 "$logs/build.log" >&2
      die "go build failed at $build_sha"
    }
  fi
  mv "$partial" "$build_dir"
  partial=''
fi
if [[ "$(state_get .build)" != "$build_sha" ]]; then
  state_set build "\"$build_sha\""
fi

# ---------------------------------------------------------------- scenario
if [[ -n "$scenario" ]]; then
  instance="$root/instance"
  [[ ! -e "$instance" ]] ||
    refuse "$instance already ran a scenario, which is single-use; name a new root for another instance"
  printf 'approval=approve the %s scenario'"'"'s specification in a paired client when its task waits for it\n' "$scenario"
  cleanup
  trap - EXIT
  exec bash "$src/scripts/dev-instance.sh" --daemon-only --scenario "$scenario" \
    --root "$instance" --listen "$listen"
fi

# -------------------------------------------------------- render settings
settings="$root/daemon/settings.json"
if [[ -z "$(state_get .tree_commit)" ]]; then
  # Start from the pass's last adopted tree, else the checkout's HEAD; the
  # deputy's self-check says whether it matches.
  tree_start=$(git -C "$tree_checkout" rev-parse --verify --quiet "refs/heads/exit-root/$pass") ||
    tree_start=$(git -C "$tree_checkout" rev-parse --verify --quiet HEAD) ||
    die "the agent-tree checkout $tree_checkout has no commit (docs/production-walkthrough.md, Where The Agent Tree Lives)"
  state_set tree_commit "\"$tree_start\""
fi

render_settings() {
  local second='null'
  [[ "$second_writer" != true ]] || second=$(jq -c '.exit_root.second_writer' "$machine")
  jq -n --slurpfile sources "$work/sources.json" --slurpfile s "$state" --slurpfile m "$machine" \
    --arg listen "$listen" --arg root "$root" --arg prompts "$src/prompts/phase-1a" \
    --arg checkout "$repo_checkout" --argjson second "$second" '
    $sources[0] * {
      version: "freeside-deputy-settings-v1",
      listen: $listen,
      seed_root: ($root + "/seed"),
      prompts: {
        prompt_package: ($prompts + "/implementer.md"),
        specification_prompt_package: ($prompts + "/specifier.md"),
        remediation_prompt_package: ($prompts + "/remediator.md")
      },
      repository: {checkout: $checkout},
      agent_tree: ({commit: $s[0].tree_commit} + (if $second == null then {} else {second_writer: $second} end)),
      images: {agent: ($s[0].image // $m[0].exit_root.placeholder.image)},
      approved_recipe: ($s[0].recipe // $m[0].exit_root.placeholder.recipe)
    }' >"$settings.tmp"
  chmod 0600 "$settings.tmp"
  mv "$settings.tmp" "$settings"
}

# The submit inputs prelaunch checks and the pass submits.
mkdir -p "$root/inputs"
for input in task.md policy.json work-unit.json; do
  cp "$(pass_file "$input")" "$root/inputs/$input"
done
jq --argjson author "$(jq -c '.exit_root.commit_author // null' "$machine")" '
  if $author == null then .
  elif has("commit_author") then error("the template and the machine file both set commit_author")
  else . + {commit_author: $author} end' "$(pass_file publication.json)" >"$root/inputs/publication.json" ||
  refuse 'could not render publication.json'

setting() { jq -r "$1" "$settings"; }

# ------------------------------------------------------------- the steps
missing=''
passed=''
run_prelaunch() {
  local rc=0
  rm -f "$root/daemon/prelaunch.json"
  "$freesided" prelaunch -root "$root" -no-live -task "$root/inputs/task.md" \
    -policy "$root/inputs/policy.json" -publication "$root/inputs/publication.json" \
    -work-unit "$root/inputs/work-unit.json" >"$logs/prelaunch.json" 2>"$logs/prelaunch.err" || rc=$?
  if [[ "$rc" -gt 1 || ! -f "$root/daemon/prelaunch.json" ]]; then
    tail -n 20 "$logs/prelaunch.err" >&2
    die "prelaunch did not run (status $rc)"
  fi
  missing=$(jq -r '.first_missing.prerequisite // ""' "$root/daemon/prelaunch.json")
  passed=$(jq -r '.passed' "$root/daemon/prelaunch.json")
}

prelaunch_failures() {
  jq -r '.checks[] | select(.status == "failed") | "  \(.group) \(.name): \(.evidence)"' "$root/daemon/prelaunch.json" >&2
}

# setup_start: one deputy start, stopped once it serves. It adopts the
# identities and the tree and, when the tree matches, writes the composition
# manifest. Sets start_result to tree_mismatch or composed (the manifest was
# written, whatever composition checks failed); a failure before the tree
# comparison, such as exporter_image, ends the script.
start_result=''
setup_start() {
  local out="$logs/deputy-setup.out" err="$logs/deputy-setup.err" manifest="$root/daemon/composition-manifest.json"
  local pid rc=0 _ line
  say "starting the deputy once on $root (log: $err)"
  # The deputy removes an earlier manifest itself; removing it here too
  # keeps a stale one from passing for this start's.
  rm -f "$manifest"
  "$freesided" deputy -root "$root" >"$out" 2>"$err" &
  pid=$!
  for _ in $(seq 1 4500); do
    grep -q '"api_url"' "$out" 2>/dev/null && break
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.2
  done
  if ! grep -q '"api_url"' "$out" 2>/dev/null; then
    kill -TERM "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || rc=$?
    tail -n 20 "$err" >&2
    die "the deputy exited or never served (status $rc)"
  fi
  # TERM ends the deputy as Ctrl-C does (its signal context takes both), and
  # unlike INT a background job can receive it.
  kill -TERM "$pid"
  for _ in $(seq 1 300); do
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.2
  done
  ! kill -0 "$pid" 2>/dev/null || kill -KILL "$pid" 2>/dev/null || true
  wait "$pid" || rc=$?
  ((rc == 0)) || {
    tail -n 20 "$err" >&2
    die "the deputy exited $rc; it may have left the rig lease: $freesided rig recover -state-root $root/daemon -confirm"
  }
  state_set api_url "$(head -n 1 "$out" | jq -c '.api_url')"
  if [[ -f "$manifest" ]]; then
    start_result=composed
  elif grep -q '^freesided deputy: agent_tree failed;' "$err"; then
    start_result=tree_mismatch
  else
    line=$(grep -m 1 '^freesided deputy: .* failed; execution stays disabled' "$err" || true)
    die "the deputy's start stopped before its tree comparison: ${line:-no self-check result}; see $err"
  fi
}

# commit_tree: commits the deputy's patch on exit-root/<pass> with a scratch
# index, so the checkout's HEAD, index, and working tree never change.
commit_tree() {
  local patch="$root/daemon/agent-tree.patch" ref="refs/heads/exit-root/$pass" old base index tree commit
  [[ -s "$patch" ]] || die "the deputy wrote no tree patch; see $root/daemon/auth-adopt.log"
  # Moving a branch some worktree has checked out would change its files
  # under it, so that is the operator's to resolve.
  [[ -z "$(git -C "$tree_checkout" for-each-ref --format='%(worktreepath)' "$ref")" ]] ||
    refuse "exit-root/$pass is checked out in a worktree of $tree_checkout; switch that worktree off it and rerun"
  old=$(git -C "$tree_checkout" rev-parse --verify --quiet "$ref" || true)
  base=${old:-$(state_get .tree_commit)}
  index="$work/tree-index"
  rm -f "$index"
  GIT_INDEX_FILE=$index git -C "$tree_checkout" read-tree "$base"
  GIT_INDEX_FILE=$index git -C "$tree_checkout" rm --cached -r -q --ignore-unmatch -- \
    policy/fragments policy/lineup policy/agents.lock
  GIT_INDEX_FILE=$index git -C "$tree_checkout" apply --cached "$patch"
  tree=$(GIT_INDEX_FILE=$index git -C "$tree_checkout" write-tree)
  commit=$(git -C "$tree_checkout" commit-tree "$tree" -p "$base" \
    -m "Adopt the $pass exit-root tree" -m "Emitted by freesided $build_sha on $root.")
  # An empty old value means the ref must not exist yet, in either hash.
  git -C "$tree_checkout" update-ref -m "exit-root: adopt the $pass tree" "$ref" "$commit" "$old"
  say "committed $patch as $commit on exit-root/$pass in $tree_checkout"
  state_set tree_commit "\"$commit\""
}

emit_head() {
  printf 'root=%s\n' "$root"
  local url
  url=$(state_get .api_url)
  [[ -z "$url" ]] || printf 'api_url=%s\n' "$url"
}

# onboard_review: the one-time review, never -approve; stops with exit 3.
onboard_review() {
  local digest args=() arg status review="$root/daemon/onboard-review.json" approved="$root/daemon/onboard-approved.json"
  digest=$(jq -r '.review_configuration_digest // empty' "$root/daemon/composition-manifest.json" 2>/dev/null || true)
  [[ -n "$digest" ]] || die "the composition manifest names no review_configuration_digest"
  args=(onboard "$(setting .repository.repo)" -db "$root/daemon/freeside.db"
    -repository-id "$(setting .repository.repository_id)" -commit "$(setting .repository.base_sha)"
    -base-ref "$(setting .repository.base_ref)" -source "$repo_checkout" -review-config-digest "$digest")
  while IFS= read -r arg; do args+=("$arg"); done < <(jq -r --arg r "$repo" \
    '(.exit_root.onboard_args // [])[], (.exit_root.repositories[$r].onboard_args // [])[]' "$machine")
  [[ "$(state_get .onboard_resume)" != true ]] || args+=(-resume)
  # An approval from before this review binds an older review; keep it, but
  # out of the way of the step that reads the new one.
  [[ ! -e "$approved" ]] || mv "$approved" "$root/daemon/onboard-approved.superseded-$(date +%s).json"
  say "running the onboarding review (log: $logs/onboard-review.err)"
  "$freesided" "${args[@]}" >"$review" 2>"$logs/onboard-review.err" || {
    tail -n 20 "$logs/onboard-review.err" >&2
    die 'the onboarding review failed'
  }
  status=$(jq -r '.status // empty' "$review")
  case $status in
  installation_pending)
    state_set onboard_resume true
    emit_head
    printf 'approval=complete the GitHub App installation for %s at %s, then rerun this command\n' \
      "$repo" "$(jq -r '.installation_url' "$review")"
    exit 3
    ;;
  review_required)
    local command
    command="$(printf '%q ' "$freesided" "${args[@]}" -approve "$(jq -r '.approval_digest' "$review")")> $(printf '%q' "$approved")"
    state_set approval_command "$(jq -n --arg c "$command" '$c')"
    emit_head
    say "read the review in $review before approving"
    printf 'approval=%s\n' "$command"
    exit 3
    ;;
  *) die "the onboarding review returned status \"$status\"; see $review" ;;
  esac
}

take_onboarding_image() {
  local approved="$root/daemon/onboard-approved.json" image recipe
  if [[ ! -s "$approved" ]] || [[ "$(jq -r '.status // empty' "$approved")" != complete ]]; then
    emit_head
    say "no completed onboarding approval at $approved; run the approval command, which writes it"
    printf 'approval=%s\n' "$(state_get .approval_command)"
    exit 3
  fi
  image=$(jq -r '.project_image.image_ref // empty' "$approved")
  recipe=$(jq -r '.project_image.recipe_digest // empty' "$approved")
  [[ -n "$image" && -n "$recipe" ]] || die "$approved names no project_image"
  if [[ "$image" == "$(state_get .image)" && "$recipe" == "$(state_get .recipe)" ]]; then
    die "settings already carry the approved image, but prelaunch still reports $missing: $(jq -r '.first_missing.evidence' "$root/daemon/prelaunch.json")"
  fi
  state_set image "\"$image\""
  state_set recipe "\"$recipe\""
  say "set images.agent to $image and approved_recipe to $recipe"
}

# Prelaunch compares agent_tree.commit only with the patch the last start
# wrote, and a new build's prompt packages or a changed machine file alter
# what adoption emits without changing that patch. So any settings change
# since the last start that composed gets a start of its own, before
# prelaunch is trusted; the state file keeps that start's settings digest.
committed=false
for _ in 1 2 3 4 5 6 7 8; do
  render_settings
  key=$(git -C "$repo_root" hash-object -- "$settings")
  if [[ "$key" != "$(state_get .composed_settings)" ]]; then
    setup_start
    if [[ "$start_result" == tree_mismatch ]]; then
      [[ "$committed" == false ]] || die "the deputy still reports a tree mismatch after the commit; see $logs/deputy-setup.err"
      commit_tree
      committed=true
      continue
    fi
    state_set composed_settings "\"$key\""
  fi
  run_prelaunch
  case $missing in
  agent_tree)
    die "prelaunch still reports agent_tree after a start whose tree matched: $(jq -r '.first_missing.evidence' "$root/daemon/prelaunch.json")"
    ;;
  trust_profile) onboard_review ;;
  project_image | approved_recipe) take_onboarding_image ;;
  '')
    [[ "$passed" == true ]] || {
      prelaunch_failures
      die "prelaunch failed with nothing left to onboard; see $root/daemon/prelaunch.json"
    }
    break
    ;;
  *) die "prelaunch reports an unknown first_missing \"$missing\"" ;;
  esac
done
[[ -z "$missing" && "$passed" == true ]] || die 'the root did not converge; see the steps above'

# ------------------------------------------------------- routing and pools
writer=$(setting .agent_tree.auth_identity)
second=$(setting '.agent_tree.second_writer.auth_identity // ""')
# shellcheck disable=SC2016 # jq's variables, not the shell's
symbols='def id: if . == "writer" then $w else $s end;'
orders=()
while IFS= read -r order; do orders+=(-order "$order"); done < <(jq -r --arg w "$writer" --arg s "$second" "$symbols"'
  (.routing // {}) | to_entries[] | "\(.key)=" + (.value | map(id + "/claude_code") | join(","))' <<<"$pass_json")
if ((${#orders[@]} > 0)); then
  say "setting the routing order"
  "$freesided" routing set -db "$root/daemon/freeside.db" -agent-tree "$tree_checkout" \
    -agent-tree-commit "$(state_get .tree_commit)" "${orders[@]}" >"$logs/routing-set.json" 2>"$logs/routing-set.err" || {
    tail -n 20 "$logs/routing-set.err" >&2
    die 'routing set failed'
  }
fi
while IFS=$'\t' read -r pool limit; do
  [[ -n "$pool" ]] || continue
  say "setting $pool's parallel limit to $limit"
  "$freesided" set-pool-limit -db "$root/daemon/freeside.db" -pool "$pool" -max-parallel-executions "$limit" \
    >>"$logs/set-pool-limit.json" 2>"$logs/set-pool-limit.err" || {
    tail -n 20 "$logs/set-pool-limit.err" >&2
    die "set-pool-limit failed for $pool"
  }
done < <(jq -r --arg w "$writer" --arg s "$second" "$symbols"'
  (.pool_limits // {}) | to_entries[] | "identity:\(.key | id)\t\(.value)"' <<<"$pass_json")

# --------------------------------------------------------- probe and check
say "probing the lineup (one short call per role; log: $logs/probe-lineup.err)"
"$freesided" probe-lineup -root "$root" >"$logs/probe-lineup.json" 2>"$logs/probe-lineup.err" || {
  tail -n 20 "$logs/probe-lineup.err" >&2
  die 'probe-lineup failed'
}
run_prelaunch
[[ "$passed" == true ]] || {
  prelaunch_failures
  die "the final prelaunch failed; see $root/daemon/prelaunch.json"
}

emit_head
seed=$(jq -r '.seed_exhausted // empty' <<<"$pass_json")
if [[ -n "$seed" ]]; then
  seed_identity=$writer
  [[ "$seed" == writer ]] || seed_identity=$second
  say "run the next= command, with the reset filled in, just before submit: the pool's next collection clears the seed"
  printf 'next=%s-resets-at <RFC 3339 reset, in the future>\n' \
    "$(printf '%q ' "$freesided" auth seed-exhausted -db "$root/daemon/freeside.db" -pool "identity:$seed_identity" \
      -window five_hour -model "$(setting .writers.specifier.model)")"
fi
say "ready: start the deputy with $freesided deputy -root $root; the submit inputs are in $root/inputs"
