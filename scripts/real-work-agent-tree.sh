#!/usr/bin/env bash
# Enrollment and agent-tree check for run-real-work.sh, sourced by the harness
# and by test-real-work-agent-tree.sh.
#
# A fresh state root holds recorded identities and no enrollment, so the
# lineup at FREESIDE_REAL_RUN_AGENT_TREE_COMMIT resolves nothing until
# `freesided auth adopt` has run against that root. The harness runs it here
# and then proves the tree adoption emitted is the tree at the configured
# commit. It reads the agent-tree checkout and never writes to it: the tree
# changes only through a commit the operator reviews.

# real_work_missing_adoption_inputs prints each input adoption needs that the
# environment lacks. They stay out of the harness's `required` list on
# purpose: that list is written in plain text to the retained session's
# verification-env.sh, and a cost owner or an account has no business there.
real_work_missing_adoption_inputs() {
	local name
	for name in FREESIDE_REAL_RUN_COST_OWNER FREESIDE_REAL_RUN_REVIEW_COST_OWNER \
		FREESIDE_REAL_RUN_CLAUDE_ACCOUNT FREESIDE_REAL_RUN_TERMS_BASIS_DATE \
		FREESIDE_REAL_RUN_PRICING_REVISION FREESIDE_REAL_RUN_OFFER_NOT_AFTER; do
		[[ -n "${!name:-}" ]] || printf '%s\n' "$name"
	done
}

# The checkout this file is in: run-real-work.sh's own repo_root, because the
# two sit in one directory.
real_work_agent_tree_repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

# The paths agenttree.ReadCommit reads under the checkout's policy/ (the
# daemon's internal/agenttree): two directories and two files. Anything else
# under policy/ is not the admitted-agent tree and is not compared.
real_work_agent_tree_paths=(policy/agents policy/fragments policy/lineup policy/agents.lock)

# real_work_agent_tree_matches <patch> <agent-tree> <commit> <scratch>
# returns 0 when the tree the patch creates is the tree at <commit>, 1 when it
# differs, and 2 when the comparison could not be made. It compares each
# path's mode, type, and object name, so a file's bytes, mode, and presence
# all count. A <commit> the checkout cannot resolve differs: the operator's
# next step is the same, a commit of the patch. <scratch> is a directory this
# function owns and replaces.
real_work_agent_tree_matches() {
	local patch=$1 tree=$2 commit=$3 scratch=$4 format emitted want have
	format=$(git -C "$tree" rev-parse --show-object-format) || return 2
	rm -rf "$scratch"
	# The operator's git configuration stays out of the scratch repository: an
	# apply.whitespace or autocrlf setting would change the bytes compared.
	GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null \
		git init -q --object-format="$format" "$scratch" || return 2
	GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null \
		git -C "$scratch" apply --index "$patch" || return 2
	emitted=$(git -C "$scratch" write-tree) || return 2
	want=$(git -C "$scratch" ls-tree "$emitted" -- "${real_work_agent_tree_paths[@]}") || return 2
	have=$(git -C "$tree" ls-tree "$commit" -- "${real_work_agent_tree_paths[@]}" 2>/dev/null || true)
	[[ "$want" == "$have" ]] || return 1
}

# real_work_adopt_agent_tree <freesided> <db-path> <session>
# adopts both identities on the state root and checks the emitted tree against
# FREESIDE_REAL_RUN_AGENT_TREE at FREESIDE_REAL_RUN_AGENT_TREE_COMMIT. It
# returns 0 when the run may go on to preflight, 1 when adoption failed, and 2
# when the trees differ. The three dated inputs are pinned from the
# environment because auth adopt defaults each to the current date, and a tree
# committed on one day would stop matching on the next.
#
# When the harness runs subscription judgments it gives preflight and the
# daemon the publication author's prompt file, which switches that role on,
# and the role then needs a lineup line naming the same file. Adoption writes
# that line only when it is given the file, so it gets the file under the
# condition and at the path run-real-work.sh uses for judgment_args. Without
# it preflight reports the role unbound and stops every such run.
#
# Adoption's own output, and git's while comparing, goes to files in the
# session and is never echoed: the report and the patch name the identities
# and adoption's errors can quote a cost owner, and the operator pastes
# harness output into public trackers.
real_work_adopt_agent_tree() {
	local freesided=$1 db_path=$2 session=$3
	local patch="$session/agent-tree.patch" report="$session/auth-adopt.json"
	local log="$session/auth-adopt.log" rc=0
	local author_prompt=()
	if [[ -n "${FREESIDE_REAL_RUN_JUDGMENT_CLAUDE_BIN:-}${FREESIDE_REAL_RUN_JUDGMENT_CLAUDE_SHA256:-}${FREESIDE_REAL_RUN_JUDGMENT_MODEL:-}${FREESIDE_REAL_RUN_JUDGMENT_AUTH_SNAPSHOT:-}" ]]; then
		author_prompt=(-judgment-publication-author-prompt "$real_work_agent_tree_repo_root/prompts/publication-author.md")
	fi
	rm -f "$patch"
	"$freesided" auth adopt \
		-db "$db_path" \
		-approved-recipe "$FREESIDE_REAL_RUN_APPROVED_RECIPE" \
		-auth-identity "$FREESIDE_REAL_RUN_AUTH_IDENTITY" \
		-review-auth-identity "$FREESIDE_REAL_RUN_REVIEW_AUTH_IDENTITY" \
		-cost-owner "$FREESIDE_REAL_RUN_COST_OWNER" \
		-review-cost-owner "$FREESIDE_REAL_RUN_REVIEW_COST_OWNER" \
		-claude-account "$FREESIDE_REAL_RUN_CLAUDE_ACCOUNT" \
		-exporter-image "$FREESIDE_WARD_EXPORTER_IMAGE" \
		-auth-store-root "$FREESIDE_REAL_RUN_REVIEW_INPUT_ROOT" \
		-review-model "$FREESIDE_REAL_RUN_REVIEW_MODEL" \
		-terms-basis-date "$FREESIDE_REAL_RUN_TERMS_BASIS_DATE" \
		-pricing-revision "$FREESIDE_REAL_RUN_PRICING_REVISION" \
		-offer-not-after "$FREESIDE_REAL_RUN_OFFER_NOT_AFTER" \
		-prompt-package "$FREESIDE_REAL_RUN_PROMPT_PACKAGE" \
		-specification-prompt-package "$FREESIDE_REAL_RUN_SPECIFICATION_PROMPT_PACKAGE" \
		-remediation-prompt-package "$FREESIDE_REAL_RUN_REMEDIATION_PROMPT_PACKAGE" \
		${author_prompt[@]+"${author_prompt[@]}"} \
		-patch "$patch" >"$report" 2>"$log" || rc=$?
	if [[ "$rc" != 0 || ! -s "$patch" ]]; then
		echo "run-real-work: freesided auth adopt failed or emitted no tree patch; its report is $report and its log is $log" >&2
		return 1
	fi
	# Client and status only; the identity ids stay in the report file.
	python3 - "$report" >&2 <<'PY' || return 1
import json
import sys

with open(sys.argv[1], encoding="utf-8") as source:
    report = json.load(source)
for identity in report["identities"]:
    print(f"auth adopt: {identity['harness_client']} {identity['status']}")
PY
	real_work_agent_tree_matches "$patch" "$FREESIDE_REAL_RUN_AGENT_TREE" \
		"$FREESIDE_REAL_RUN_AGENT_TREE_COMMIT" "$session/agent-tree-scratch" 2>>"$log" || rc=$?
	case "$rc" in
	0) return 0 ;;
	1)
		echo "run-real-work: the agent tree at $FREESIDE_REAL_RUN_AGENT_TREE_COMMIT in $FREESIDE_REAL_RUN_AGENT_TREE is not the tree auth adopt emitted" >&2
		echo "run-real-work: review and commit $patch in that checkout, set FREESIDE_REAL_RUN_AGENT_TREE_COMMIT to the new commit, and rerun; the harness changed nothing in the checkout" >&2
		return 2
		;;
	*)
		echo "run-real-work: could not compare $patch with the agent tree checkout $FREESIDE_REAL_RUN_AGENT_TREE; see $log" >&2
		return 1
		;;
	esac
}
