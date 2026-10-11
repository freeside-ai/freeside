#!/usr/bin/env bash
# Argument check and publication-file warning of run-real-work.sh. The harness
# prints the warning right after its argument checks, so an empty environment
# stops it at the required-environment refusal: no rig, daemon, or network is
# involved.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

author='"commit_author":{"app_slug":"example-app","bot_user_id":123}'
issue=https://github.com/example/project/issues/82
printf '%s\n' "$issue" >"$tmp/spec.md"
printf '[]\n' >"$tmp/policy.json"
printf '{"completion_criterion":"bound_pr_merged"}\n' >"$tmp/work-unit.json"
printf '{"recipe":"freeside.client-publication/v2","source_issue":"%s",%s}\n' "$issue" "$author" >"$tmp/client.json"
printf '{"recipe":"freeside.client-publication/v2",%s}\n' "$author" >"$tmp/client-no-source.json"
printf '{"title":"Fix the thing","body":"Why and what.",%s}\n' "$author" >"$tmp/literal.json"
printf '{"title":"Fix the thing",%s}\n' "$author" >"$tmp/title-only.json"
# The daemon encodes a client record with empty "title" and "body" keys.
printf '{"title":"","body":"","recipe":"freeside.client-publication/v2","source_issue":"%s",%s}\n' \
	"$issue" "$author" >"$tmp/client-encoded.json"
printf '{"title":"Fix the thing","body":"Why and what.","source_issue":"",%s}\n' "$author" >"$tmp/literal-empty-source.json"
printf 'not json\n' >"$tmp/malformed.json"
printf '[]\n' >"$tmp/array.json"

literal_warning='literal "title" or "body"'

# Run the harness with the given arguments and no environment; it must reach
# the required-environment refusal, whatever it warned about on the way.
harness() {
	local status=0
	env -i PATH="$PATH" bash "$root/scripts/run-real-work.sh" "$@" >"$tmp/out" 2>"$tmp/err" || status=$?
	if [[ "$status" != 2 ]] || ! grep -q 'missing required environment' "$tmp/err" || [[ -s "$tmp/out" ]]; then
		echo "harness $* did not stop at the environment check (exit $status)" >&2
		cat "$tmp/err" >&2
		exit 1
	fi
}

# refused <harness arguments...>: a new session with no work-unit declaration
# stops at the argument check with a usage error that says why.
refused() {
	local status=0
	env -i PATH="$PATH" bash "$root/scripts/run-real-work.sh" "$@" >"$tmp/out" 2>"$tmp/err" || status=$?
	if [[ "$status" != 2 ]] || ! grep -q '^usage: .*<work-unit.json>$' "$tmp/err" ||
		! grep -qF 'never finishes when its pull request merges' "$tmp/err" ||
		! grep -qF '{"completion_criterion":"bound_pr_merged"}' "$tmp/err" ||
		grep -q 'missing required environment' "$tmp/err" || [[ -s "$tmp/out" ]]; then
		echo "harness $* was not refused for its missing work-unit declaration (exit $status)" >&2
		cat "$tmp/err" >&2
		exit 1
	fi
}

# expect <literal: yes|no> <harness arguments...>
expect() {
	local literal=$1
	shift
	harness "$@"
	local got_literal=no
	! grep -qF "$literal_warning" "$tmp/err" || got_literal=yes
	if [[ "$got_literal" != "$literal" ]]; then
		echo "harness $*: literal warning $got_literal (want $literal)" >&2
		cat "$tmp/err" >&2
		exit 1
	fi
}

base=("$tmp/spec.md" "$tmp/policy.json")
refused "${base[@]}" "$tmp/client.json"
refused --client-target "${base[@]}" "$tmp/client.json"

expect no "${base[@]}" "$tmp/client.json" "$tmp/work-unit.json"
expect no "${base[@]}" "$tmp/client-no-source.json" "$tmp/work-unit.json"
expect yes "${base[@]}" "$tmp/literal.json" "$tmp/work-unit.json"
expect yes "${base[@]}" "$tmp/title-only.json" "$tmp/work-unit.json"
# An empty field declares nothing, so it warns like an absent one.
expect no "${base[@]}" "$tmp/client-encoded.json" "$tmp/work-unit.json"
expect yes "${base[@]}" "$tmp/literal-empty-source.json" "$tmp/work-unit.json"
# Client-target mode submits the same files, so it warns the same way.
expect yes --client-target "${base[@]}" "$tmp/literal.json" "$tmp/work-unit.json"
# A file submit will refuse draws no warning here.
expect no "${base[@]}" "$tmp/malformed.json" "$tmp/work-unit.json"
expect no "${base[@]}" "$tmp/array.json" "$tmp/work-unit.json"

# The warning never repeats the file's own text.
harness "${base[@]}" "$tmp/literal.json" "$tmp/work-unit.json"
if grep -qF 'Fix the thing' "$tmp/err"; then
	echo 'a warning repeated the publication file text' >&2
	exit 1
fi

# A resumed session's files were accepted already: no warning, and no
# declaration demanded of a session that retained none.
session=$tmp/session
mkdir -p "$session/submission-inputs"
cp "$tmp/spec.md" "$session/submission-inputs/spec.json"
cp "$tmp/policy.json" "$session/submission-inputs/policy.json"
cp "$tmp/literal.json" "$session/submission-inputs/publication.json"
expect no --resume-session "$session"

printf 'PASS: required work-unit declaration, literal-text warning, and resume\n'
