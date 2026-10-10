#!/usr/bin/env bash
# Hermetic fixtures for the harness's adoption step. A stand-in freesided
# stands for `auth adopt`: it emits a fixed tree patch and reports each
# identity adopted on a state root's first run and reused after it. No daemon,
# store, container, or network.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
# shellcheck source=scripts/real-work-agent-tree.sh
source "$root/scripts/real-work-agent-tree.sh"

fail() {
	echo "test-real-work-agent-tree: $*" >&2
	exit 1
}

git_fixture() {
	GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null \
		GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.test \
		GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.test git "$@"
}

# The emitted tree: one file under each compared path, as agenttree.Patch
# writes them (whole new files, mode 100644). The lock line ends in a space so
# that a whitespace-fixing apply would change the bytes.
cat >"$tmp/emitted.patch" <<'PATCH'
diff --git a/policy/agents.lock b/policy/agents.lock
new file mode 100644
--- /dev/null
+++ b/policy/agents.lock
@@ -0,0 +1,1 @@
+claude-code-default sha256:aa 
diff --git a/policy/agents/claude-code-default.json b/policy/agents/claude-code-default.json
new file mode 100644
--- /dev/null
+++ b/policy/agents/claude-code-default.json
@@ -0,0 +1,1 @@
+{"enrollment":"<identity>/claude_code"}
diff --git a/policy/fragments/routes/route.json b/policy/fragments/routes/route.json
new file mode 100644
--- /dev/null
+++ b/policy/fragments/routes/route.json
@@ -0,0 +1,1 @@
+{"terms_basis_date":"2026-01-02"}
diff --git a/policy/lineup b/policy/lineup
new file mode 100644
--- /dev/null
+++ b/policy/lineup
@@ -0,0 +1,1 @@
+implementer claude-code-default
PATCH

# The stand-in records its arguments, fails when told to (quoting a cost owner
# as the real command's mismatch error does), and otherwise writes the patch
# and a report whose status depends on whether the root was adopted before.
cat >"$tmp/freesided" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1 $2" == "auth adopt" ]] || { echo "unexpected command: $*" >&2; exit 64; }
shift 2
printf '%s\n' "$@" >"$STUB_DIR/args"
db="" patch="" owner=""
while [[ $# -gt 0 ]]; do
	case "$1" in
	-db) db=$2 ;;
	-patch) patch=$2 ;;
	-cost-owner) owner=$2 ;;
	esac
	shift 2
done
if [[ -e "$STUB_DIR/fail" ]]; then
	echo "auth identity writer has cost owner \"other\", not \"$owner\"" >&2
	exit 1
fi
status=adopted
[[ ! -e "$db.adopted" ]] || status=reused
: >"$db.adopted"
[[ -e "$STUB_DIR/no-patch" ]] || cp "$STUB_DIR/emitted.patch" "$patch"
printf '{"identities":[{"auth_identity_id":"writer-id","harness_client":"claude_code","status":"%s"},{"auth_identity_id":"reviewer-id","harness_client":"codex_cli","status":"%s"}],"patch":"%s"}\n' \
	"$status" "$status" "$patch"
STUB
chmod +x "$tmp/freesided"
export STUB_DIR=$tmp

# The operator's agent-tree checkout: the emitted tree committed beside an
# unrelated policy file, then a later commit that moves one tree file.
tree=$tmp/agent-tree
git_fixture init -q "$tree"
git_fixture -C "$tree" apply --whitespace=nowarn --index "$tmp/emitted.patch"
mkdir -p "$tree/policy/other"
echo unrelated >"$tree/policy/other/keys.json"
git_fixture -C "$tree" add policy/other
git_fixture -C "$tree" commit -q -m adopt
matching=$(git -C "$tree" rev-parse HEAD)
echo 'implementer codex-review-default' >"$tree/policy/lineup"
git_fixture -C "$tree" commit -q -am edit
edited=$(git -C "$tree" rev-parse HEAD)
git_fixture -C "$tree" checkout -q "$matching" -- policy/lineup
echo '{}' >"$tree/policy/agents/extra.json"
git_fixture -C "$tree" add policy
git_fixture -C "$tree" commit -q -m extra
extra=$(git -C "$tree" rev-parse HEAD)
git_fixture -C "$tree" rm -q policy/agents.lock policy/agents/extra.json
git_fixture -C "$tree" commit -q -m unlock
unlocked=$(git -C "$tree" rev-parse HEAD)
git_fixture -C "$tree" checkout -q "$matching" -- policy/agents.lock
git_fixture -C "$tree" update-index --chmod=+x policy/lineup
git_fixture -C "$tree" commit -q -m mode
executable=$(git -C "$tree" rev-parse HEAD)
git_fixture -C "$tree" update-index --chmod=-x policy/lineup
git_fixture -C "$tree" commit -q -m restore
restored=$(git -C "$tree" rev-parse HEAD)

tree_state() {
	git -C "$tree" rev-parse HEAD
	git -C "$tree" status --porcelain --untracked-files=all
	git -C "$tree" for-each-ref
	git -C "$tree" count-objects -v
	find "$tree" -type f ! -path '*/.git/*' -exec shasum {} + | sort
}

export FREESIDE_REAL_RUN_APPROVED_RECIPE=sha256:recipe
export FREESIDE_REAL_RUN_AUTH_IDENTITY=writer-id
export FREESIDE_REAL_RUN_REVIEW_AUTH_IDENTITY=reviewer-id
export FREESIDE_REAL_RUN_COST_OWNER=cost-owner-sentinel
export FREESIDE_REAL_RUN_REVIEW_COST_OWNER=review-cost-owner-sentinel
export FREESIDE_REAL_RUN_CLAUDE_ACCOUNT=account-sentinel@example.test
export FREESIDE_WARD_EXPORTER_IMAGE=example.test/exporter@sha256:00
export FREESIDE_REAL_RUN_REVIEW_INPUT_ROOT=$tmp/review-inputs
export FREESIDE_REAL_RUN_REVIEW_MODEL=review-model
export FREESIDE_REAL_RUN_TERMS_BASIS_DATE=2026-01-02
export FREESIDE_REAL_RUN_PRICING_REVISION=2026-01
export FREESIDE_REAL_RUN_OFFER_NOT_AFTER=2027-01-02T00:00:00Z
export FREESIDE_REAL_RUN_PROMPT_PACKAGE=$tmp/implementer
export FREESIDE_REAL_RUN_SPECIFICATION_PROMPT_PACKAGE=$tmp/specifier
export FREESIDE_REAL_RUN_REMEDIATION_PROMPT_PACKAGE=$tmp/remediator
export FREESIDE_REAL_RUN_AGENT_TREE=$tree
# Judgments are off unless a case switches them on, whatever the caller's
# environment holds.
unset FREESIDE_REAL_RUN_JUDGMENT_CLAUDE_BIN FREESIDE_REAL_RUN_JUDGMENT_CLAUDE_SHA256 \
	FREESIDE_REAL_RUN_JUDGMENT_MODEL FREESIDE_REAL_RUN_JUDGMENT_AUTH_SNAPSHOT

# adopt <session> <state-root> <commit> runs the step and leaves its status in
# $rc and its stderr in <session>/stderr.
adopt() {
	local session=$tmp/$1 state_root=$tmp/$2
	mkdir -p "$session" "$state_root"
	rc=0
	FREESIDE_REAL_RUN_AGENT_TREE_COMMIT=$3 real_work_adopt_agent_tree \
		"$tmp/freesided" "$state_root/freeside.db" "$session" 2>"$session/stderr" || rc=$?
}

# Every adoption input is required, and a missing one is named, not valued.
[[ -z "$(real_work_missing_adoption_inputs)" ]] || fail 'a complete environment reported a missing adoption input'
missing=$(
	unset FREESIDE_REAL_RUN_CLAUDE_ACCOUNT FREESIDE_REAL_RUN_PRICING_REVISION
	real_work_missing_adoption_inputs
)
[[ "$missing" == $'FREESIDE_REAL_RUN_CLAUDE_ACCOUNT\nFREESIDE_REAL_RUN_PRICING_REVISION' ]] || fail "missing adoption inputs reported as: $missing"

before=$(tree_state)

# A fresh state root with a matching tree commit goes on to preflight in one
# pass, and adoption got every pinned input. A later commit holding the same
# tree matches too.
adopt restored-session root-restored "$restored"
[[ "$rc" == 0 ]] || fail "a later commit of the same tree returned $rc"
adopt fresh-session root-a "$matching"
[[ "$rc" == 0 ]] || fail "fresh root with a matching commit returned $rc: $(cat "$tmp/fresh-session/stderr")"
grep -qx 'auth adopt: claude_code adopted' "$tmp/fresh-session/stderr" || fail 'fresh root did not report the writer adopted'
grep -qx 'auth adopt: codex_cli adopted' "$tmp/fresh-session/stderr" || fail 'fresh root did not report the reviewer adopted'
expected_args=(
	-db "$tmp/root-a/freeside.db" -approved-recipe sha256:recipe
	-auth-identity writer-id -review-auth-identity reviewer-id
	-cost-owner cost-owner-sentinel -review-cost-owner review-cost-owner-sentinel
	-claude-account account-sentinel@example.test
	-exporter-image example.test/exporter@sha256:00
	-auth-store-root "$tmp/review-inputs" -review-model review-model
	-terms-basis-date 2026-01-02 -pricing-revision 2026-01
	-offer-not-after 2027-01-02T00:00:00Z
	-prompt-package "$tmp/implementer"
	-specification-prompt-package "$tmp/specifier"
	-remediation-prompt-package "$tmp/remediator"
	-patch "$tmp/fresh-session/agent-tree.patch"
)
[[ "$(cat "$tmp/args")" == "$(printf '%s\n' "${expected_args[@]}")" ]] || fail "auth adopt got unexpected arguments: $(tr '\n' ' ' <"$tmp/args")"

# With subscription judgments on, adoption also gets the publication author's
# prompt file, the one the harness gives preflight and the daemon: the role is
# on for both, so the adopted lineup needs its line. Any one of the four
# variables counts, as it does where the harness builds judgment_args.
author_prompt_args=(-judgment-publication-author-prompt "$root/prompts/publication-author.md")
patch_at=$((${#expected_args[@]} - 2))
judgment_expected_args=("${expected_args[@]:0:patch_at}" "${author_prompt_args[@]}"
	-patch "$tmp/judgment-session/agent-tree.patch")
FREESIDE_REAL_RUN_JUDGMENT_CLAUDE_BIN=/opt/claude FREESIDE_REAL_RUN_JUDGMENT_CLAUDE_SHA256=sha256:11 \
	FREESIDE_REAL_RUN_JUDGMENT_MODEL=judgment-model FREESIDE_REAL_RUN_JUDGMENT_AUTH_SNAPSHOT=token \
	adopt judgment-session root-a "$matching"
[[ "$rc" == 0 ]] || fail "adoption with judgments on returned $rc"
[[ "$(cat "$tmp/args")" == "$(printf '%s\n' "${judgment_expected_args[@]}")" ]] || fail "auth adopt with judgments on got unexpected arguments: $(tr '\n' ' ' <"$tmp/args")"
FREESIDE_REAL_RUN_JUDGMENT_MODEL=judgment-model adopt judgment-session root-a "$matching"
[[ "$(cat "$tmp/args")" == "$(printf '%s\n' "${judgment_expected_args[@]}")" ]] || fail 'one judgment variable did not give adoption the author prompt'
[[ -s "$root/prompts/publication-author.md" ]] || fail 'the publication author prompt the harness names is missing'

# A second run on the same root reports both identities reused and proceeds.
adopt second-session root-a "$matching"
[[ "$rc" == 0 ]] || fail "second run on an adopted root returned $rc"
[[ "$(grep -c '^auth adopt: .* reused$' "$tmp/second-session/stderr")" == 2 ]] || fail 'second run did not report both identities reused'

# A second, fresh state root matches the same commit: the tree is a function
# of the pinned inputs, not of the root.
adopt other-root-session root-b "$matching"
[[ "$rc" == 0 ]] || fail "another fresh root returned $rc against the same commit"
grep -qx 'auth adopt: claude_code adopted' "$tmp/other-root-session/stderr" || fail 'another fresh root was not adopted'

# A tree commit that differs stops the run before preflight, names the patch
# it wrote, and leaves the checkout untouched. A changed file, an extra file
# under a compared directory, a missing file, a changed mode, and a commit the
# checkout does not hold each count.
for case in "edited:$edited" "extra:$extra" "unlocked:$unlocked" "executable:$executable" \
	"absent:0000000000000000000000000000000000000000"; do
	name=${case%%:*}
	adopt "$name-session" "root-$name" "${case#*:}"
	[[ "$rc" == 2 ]] || fail "$name tree commit returned $rc, not 2"
	grep -qF "review and commit $tmp/$name-session/agent-tree.patch" "$tmp/$name-session/stderr" || fail "$name mismatch did not name the patch"
	cmp -s "$tmp/emitted.patch" "$tmp/$name-session/agent-tree.patch" || fail "$name mismatch left no patch to commit"
done
[[ "$(tree_state)" == "$before" ]] || fail 'the harness changed the agent-tree checkout'

# A failed adoption stops the run and names its log without echoing it: the
# real command's errors can quote a cost owner.
: >"$tmp/fail"
adopt failed-session root-failed "$matching"
rm "$tmp/fail"
[[ "$rc" == 1 ]] || fail "failed adoption returned $rc, not 1"
grep -qF "$tmp/failed-session/auth-adopt.log" "$tmp/failed-session/stderr" || fail 'failed adoption did not name its log'
grep -q cost-owner-sentinel "$tmp/failed-session/auth-adopt.log" || fail 'fixture: the stand-in error did not quote the cost owner'

# An adoption that emits no patch (an unadoptable writer identity) stops too.
: >"$tmp/no-patch"
adopt no-patch-session root-no-patch "$matching"
rm "$tmp/no-patch"
[[ "$rc" == 1 ]] || fail "adoption without a patch returned $rc, not 1"

# Nothing the harness prints carries a cost owner, the account, or an
# identity id, whatever the outcome.
for stderr in "$tmp"/*-session/stderr; do
	if grep -qE 'sentinel|writer-id|reviewer-id' "$stderr"; then
		fail "$stderr carries an adoption input: $(cat "$stderr")"
	fi
done

# The operator's git configuration cannot bend the comparison.
printf '[apply]\n\twhitespace = fix\n[core]\n\tautocrlf = true\n' >"$tmp/gitconfig"
GIT_CONFIG_GLOBAL=$tmp/gitconfig adopt config-session root-config "$matching"
[[ "$rc" == 0 ]] || fail "operator git configuration changed the comparison ($rc)"

# In the harness the step runs after the identities are recorded and before
# the composition preflight, and its inputs are required but never written to
# the session's verification environment.
python3 - "$root/scripts/run-real-work.sh" "$root/scripts/real-work-agent-tree.sh" <<'PY'
import re
import sys

text = open(sys.argv[1], encoding="utf-8").read()
seed = text.index('echo "recording the auth identity binding"')
adopt = text.index('real_work_adopt_agent_tree "$workdir/freesided" "$db_path" "$workdir" || exit $?')
preflight = text.index('"$workdir/freesided" preflight "${preflight_args[@]}" >"$composition_manifest"')
assert seed < adopt < preflight, "adoption does not sit between the identity record and preflight"
required = re.search(r"^required=\((.*?)^\)", text, re.S | re.M).group(1)
for name in ("COST_OWNER", "CLAUDE_ACCOUNT"):
    assert name not in required, f"{name} joined the list written to verification-env.sh"
assert "missing_adoption_inputs=$(real_work_missing_adoption_inputs)" in text, "the harness does not require the adoption inputs"
# Adoption, preflight, and the daemon must agree on whether the publication
# author is on and on its prompt file, so the helper's condition and path are
# the harness's own.
helper = open(sys.argv[2], encoding="utf-8").read()
condition = 'if [[ -n "${FREESIDE_REAL_RUN_JUDGMENT_CLAUDE_BIN:-}${FREESIDE_REAL_RUN_JUDGMENT_CLAUDE_SHA256:-}${FREESIDE_REAL_RUN_JUDGMENT_MODEL:-}${FREESIDE_REAL_RUN_JUDGMENT_AUTH_SNAPSHOT:-}" ]]; then'
assert condition in text and condition in helper, "adoption and the harness switch judgments on differently"
assert '-judgment-publication-author-prompt "$repo_root/prompts/publication-author.md")' in text, "the harness names another author prompt"
assert '-judgment-publication-author-prompt "$real_work_agent_tree_repo_root/prompts/publication-author.md")' in helper, "adoption names another author prompt"
PY

echo 'test-real-work-agent-tree: ok'
