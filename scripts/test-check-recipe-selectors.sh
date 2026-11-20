#!/usr/bin/env bash
# test-check-recipe-selectors.sh — fixtures for the recipe selector check.
#
# Writes a synthetic recipe and pull-request workflow per case, with no git
# and no network, runs scripts/check-recipe-selectors.py against them, and
# asserts the PR workflow runs the check on every pull request. Exit code:
# 0 when every assertion passes, 1 otherwise.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
CHECK=$SCRIPT_DIR/check-recipe-selectors.py

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
case_number=0
CASE=''
DIR=''
OUT=''
RC=0

# begin_case <name> starts a case with a fresh directory holding the
# baseline workflow. Its selectors are a cut-down copy of the real ones,
# including a continuation line and a shell line shlex can't split.
begin_case() {
  case_number=$((case_number + 1))
  CASE=$(printf '%02d %s' "$case_number" "$1")
  echo "case: $CASE"
  DIR=$TMP/case-$case_number
  mkdir -p "$DIR"
  cat >"$DIR/workflow.yml" <<'YML'
        run: |
          # It's a comment with one apostrophe.
          select_component() {
            local component=$1 selected=false
          }
          select_component daemon daemon/ scripts/check.sh \
            images/agent-claude/
          select_component app app/ api/openapi.yaml
          select_component scripts scripts/ .githooks/
          select_component vocabulary daemon/ app/ scripts/
YML
}

# recipe writes stdin as the case's recipe.
recipe() {
  cat >"$DIR/recipe.json"
}

# agreeing writes a recipe whose paths match the baseline workflow.
agreeing() {
  recipe <<'JSON'
{
  "commands": [
    ["bash", "scripts/check.sh", "scripts", "tier-a-paths", "recipe-selectors"],
    {"argv": ["bash", "scripts/check.sh", "daemon", "build"],
     "paths": ["images/agent-claude/", "daemon/", "scripts/check.sh"]},
    {"argv": ["env", "X=1", "bash", "scripts/check.sh", "app", "test"], "room": "macos_host",
     "paths": ["app/", "api/openapi.yaml"]},
    {"argv": ["bash", "scripts/check.sh", "scripts", "syntax", "suites"],
     "paths": ["scripts/", ".githooks/"]},
    {"argv": ["bash", "scripts/check.sh", "scripts", "vocabulary"],
     "paths": ["daemon/", "app/", "scripts/"]},
    {"argv": ["bash", "scripts/verify-range-check.sh"], "history": true}
  ],
  "run_all_on": [".freeside/verify.json", "scripts/check.sh", ".github/workflows/"]
}
JSON
}

# edit_recipe <python expression> rewrites the recipe through `r`.
edit_recipe() {
  python3 - "$DIR/recipe.json" "$1" <<'PY'
import json, sys
path, expr = sys.argv[1], sys.argv[2]
with open(path) as f:
    r = json.load(f)
exec(expr)
with open(path, "w") as f:
    json.dump(r, f)
PY
}

run_check() {
  set +e
  OUT=$(python3 "$CHECK" --recipe "$DIR/recipe.json" --workflow "$DIR/workflow.yml" 2>&1)
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

begin_case "paths that equal their selectors as sets pass"
agreeing
run_check
assert_rc 0
assert_contains 'recipe paths agree with 4 CI selectors'

begin_case "a path the selector has and the command lacks fails"
agreeing
edit_recipe 'r["commands"][1]["paths"].remove("images/agent-claude/")'
run_check
assert_rc 1
assert_contains "differ from selector 'daemon'"

begin_case "a path the command has and the selector lacks fails"
agreeing
edit_recipe 'r["commands"][2]["paths"].append("docs/")'
run_check
assert_rc 1
assert_contains "differ from selector 'app'"

begin_case "a selector entry the recipe lacks fails"
agreeing
sed -i.bak 's|select_component app app/ api/openapi.yaml|& app/extra.json|' "$DIR/workflow.yml"
run_check
assert_rc 1
assert_contains "differ from selector 'app'"

begin_case "a mapped command with no paths fails"
agreeing
edit_recipe 'del r["commands"][3]["paths"]'
run_check
assert_rc 1
assert_contains "differ from selector 'scripts'"

begin_case "an always-run scripts step with paths fails"
agreeing
edit_recipe 'r["commands"][0] = {"argv": r["commands"][0], "paths": ["scripts/"]}'
run_check
assert_rc 1
assert_contains 'maps to no CI selector, so it must carry no paths'

begin_case "a command outside check.sh with paths fails"
agreeing
edit_recipe 'r["commands"][5]["paths"] = ["daemon/"]'
run_check
assert_rc 1
assert_contains 'maps to no CI selector, so it must carry no paths'

begin_case "scripts steps CI runs on different triggers can't share a command"
agreeing
edit_recipe 'r["commands"][3]["argv"].append("vocabulary")'
run_check
assert_rc 1
assert_contains 'mixes scripts steps CI runs on different triggers'

begin_case "every scripts step in one command fails"
agreeing
edit_recipe 'r["commands"][3]["argv"] = ["bash", "scripts/check.sh", "scripts"]'
run_check
assert_rc 1
assert_contains 'runs every scripts step'

begin_case "an unknown check.sh component fails"
agreeing
edit_recipe 'r["commands"][1]["argv"][2] = "nope"'
run_check
assert_rc 1
assert_contains "names unknown check.sh component 'nope'"

begin_case "a selector the workflow doesn't define fails"
agreeing
edit_recipe 'r["commands"].append({"argv": ["bash", "scripts/check.sh", "docs"], "paths": ["docs/plan.md"]})'
run_check
assert_rc 1
assert_contains "maps to selector 'docs', which pull-request.yml does not define"

begin_case "run_all_on without the workflows directory fails"
agreeing
edit_recipe 'r["run_all_on"].remove(".github/workflows/")'
run_check
assert_rc 1
assert_contains 'run_all_on lacks .github/workflows/'

begin_case "run_all_on without check.sh fails"
agreeing
edit_recipe 'del r["run_all_on"][1]'
run_check
assert_rc 1
assert_contains 'run_all_on lacks scripts/check.sh'

begin_case "a key in another case fails, since the daemon would read it"
agreeing
edit_recipe 'r["commands"][0] = {"argv": r["commands"][0], "room": "project_image", "Paths": ["app/"]}'
run_check
assert_rc 1
assert_contains "carries key 'Paths'"

begin_case "a recipe key in another case fails"
agreeing
edit_recipe 'r["Run_All_On"] = r.pop("run_all_on")'
run_check
assert_rc 1
assert_contains "recipe carries key 'Run_All_On'"

begin_case "a selector defined twice is an input error"
agreeing
echo '          select_component app app/' >>"$DIR/workflow.yml"
run_check
assert_rc 2
assert_contains 'selector app is defined twice'

begin_case "a workflow with no selectors is an input error"
agreeing
echo 'on: pull_request' >"$DIR/workflow.yml"
run_check
assert_rc 2
assert_contains 'no select_component calls'

begin_case "a missing recipe is an input error"
run_check
assert_rc 2
assert_contains 'recipe.json'

begin_case "the PR workflow runs the check unconditionally and requires it"
set +e
OUT=$(python3 - "$SCRIPT_DIR/.." 2>&1 <<'PY'
import re
import sys
from pathlib import Path

root = Path(sys.argv[1])
workflow = (root / '.github/workflows/recipe-selectors.yml').read_text()
assert '  workflow_call:' in workflow
assert '  contents: read' in workflow
assert 'group: recipe-selectors-${{' in workflow
assert '        run: bash scripts/check.sh scripts recipe-selectors\n' in workflow
assert not re.search(r'^\s+if:', workflow, re.M)
caller = (root / '.github/workflows/pull-request.yml').read_text()
jobs = dict(re.findall(r'^  ([a-z-]+):\n(.*?)(?=^  [a-z-]+:|\Z)', caller, re.M | re.S))
job_lines = [line.strip() for line in jobs['recipe-selectors'].splitlines()
             if line.strip() and not line.lstrip().startswith('#')]
assert job_lines == ['uses: ./.github/workflows/recipe-selectors.yml'], job_lines
assert '      - recipe-selectors\n' in jobs['required']
assert '    if: always()\n' in jobs['required']
assert '"recipe-selectors"' in jobs['required'].split('keys ==')[1].split(']')[0]
assert '.["recipe-selectors"].result == "success"' in jobs['required']
print('PASS: recipe-selectors workflow is unconditional and required')
PY
)
RC=$?
set -e
assert_rc 0
assert_contains 'PASS: recipe-selectors workflow is unconditional and required'

echo
echo "passed $pass assertion(s), failed $fail"
[ "$fail" -eq 0 ]
