#!/usr/bin/env bash
# Publication-file warnings of run-real-work.sh. The harness prints them right
# after its argument checks, so an empty environment stops it at the
# required-environment refusal: no rig, daemon, or network is involved.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

author='"commit_author":{"app_slug":"example-app","bot_user_id":123}'
issue=https://github.com/example/project/issues/82
printf '%s\n' "$issue" >"$tmp/spec.md"
printf '[]\n' >"$tmp/policy.json"
printf '{"completion_criterion":"pr_merged"}\n' >"$tmp/work-unit.json"
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
source_warning='no work-unit file was given'

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

# expect <literal: yes|no> <source: yes|no> <harness arguments...>
expect() {
	local literal=$1 source=$2
	shift 2
	harness "$@"
	local got_literal=no got_source=no
	! grep -qF "$literal_warning" "$tmp/err" || got_literal=yes
	! grep -qF "$source_warning" "$tmp/err" || got_source=yes
	if [[ "$got_literal" != "$literal" || "$got_source" != "$source" ]]; then
		echo "harness $*: literal warning $got_literal (want $literal), source warning $got_source (want $source)" >&2
		cat "$tmp/err" >&2
		exit 1
	fi
}

base=("$tmp/spec.md" "$tmp/policy.json")
expect no yes "${base[@]}" "$tmp/client.json"
expect no no "${base[@]}" "$tmp/client.json" "$tmp/work-unit.json"
expect no no "${base[@]}" "$tmp/client-no-source.json"
expect yes no "${base[@]}" "$tmp/literal.json"
expect yes no "${base[@]}" "$tmp/literal.json" "$tmp/work-unit.json"
expect yes no "${base[@]}" "$tmp/title-only.json"
# An empty field declares nothing, so it warns like an absent one.
expect no yes "${base[@]}" "$tmp/client-encoded.json"
expect no no "${base[@]}" "$tmp/client-encoded.json" "$tmp/work-unit.json"
expect yes no "${base[@]}" "$tmp/literal-empty-source.json"
# Client-target mode submits the same files, so it warns the same way.
expect no yes --client-target "${base[@]}" "$tmp/client.json"
# A file submit will refuse draws no warning here.
expect no no "${base[@]}" "$tmp/malformed.json"
expect no no "${base[@]}" "$tmp/array.json"

# The warning never repeats the file's own text.
harness "${base[@]}" "$tmp/client.json"
if grep -qF "$issue" "$tmp/err"; then
	echo 'a warning repeated the publication file source issue' >&2
	exit 1
fi

# A resumed session's files were accepted already: no warning.
session=$tmp/session
mkdir -p "$session/submission-inputs"
cp "$tmp/spec.md" "$session/submission-inputs/spec.json"
cp "$tmp/policy.json" "$session/submission-inputs/policy.json"
cp "$tmp/literal.json" "$session/submission-inputs/publication.json"
expect no no --resume-session "$session"

printf 'PASS: publication warnings for literal text, an undeclared source issue, and resume\n'
