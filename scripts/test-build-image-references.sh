#!/usr/bin/env bash
# test-build-image-references.sh — output-contract regression suite for the
# exporter and agent image builders.
#
# Missing registry mode must fail before any expensive build and must never
# print a digest-shaped success value. The external and temporary-loopback
# registry paths are exercised against scripted `go` and Apple `container`
# stand-ins so the exact pushed, pulled, verified, and emitted digest reference
# is pinned without network or a container runtime.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

DIGEST="sha256:$(printf '1%.0s' {1..64})"
OTHER_DIGEST="sha256:$(printf '2%.0s' {1..64})"
REGISTRY_HELPER_DIGEST=sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373
BIN_DIR=$TMP/bin
LOG=$TMP/calls.log
mkdir -p "$BIN_DIR"
: >"$LOG"

cat >"$BIN_DIR/container" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${BUILD_IMAGE_TEST_LOG:?}"

case "$*" in
"run --rm --network none freeside-agent-claude:local claude --version")
	printf '%s\n' '2.1.220 (Claude Code)'
	;;
"run --rm --network none freeside-agent-codex:local codex --version")
	printf '%s\n' 'codex-cli 0.147.0'
	;;
"image inspect docker.io/library/registry@${BUILD_IMAGE_TEST_REGISTRY_HELPER_DIGEST:?}")
	printf '{"digest":"%s"}\n' "$BUILD_IMAGE_TEST_REGISTRY_HELPER_DIGEST"
	;;
"image inspect "*)
	ref=${*: -1}
	digest=${BUILD_IMAGE_TEST_DIGEST:?}
	if [ -n "${BUILD_IMAGE_TEST_EXACT_REF:-}" ] && [ "$ref" = "$BUILD_IMAGE_TEST_EXACT_REF" ]; then
		digest=${BUILD_IMAGE_TEST_SEEDED_DIGEST:?}
	fi
	printf '{"digest":"%s"}\n' "$digest"
	;;
"inspect freeside-"*)
	exit 1
	;;
"delete --force "*)
	[ "${BUILD_IMAGE_TEST_DELETE_FAIL:-0}" -ne 1 ]
	;;
esac
STUB

cat >"$BIN_DIR/go" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf 'go %s\n' "$*" >>"${BUILD_IMAGE_TEST_LOG:?}"
while [ "$#" -gt 0 ]; do
	if [ "$1" = "-o" ]; then
		: >"$2"
		exit 0
	fi
	shift
done
exit 1
STUB

cat >"$BIN_DIR/curl" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf 'curl %s\n' "$*" >>"${BUILD_IMAGE_TEST_LOG:?}"
STUB
chmod +x "$BIN_DIR/container" "$BIN_DIR/go" "$BIN_DIR/curl"

export BUILD_IMAGE_TEST_DIGEST=$DIGEST
export BUILD_IMAGE_TEST_LOG=$LOG
export BUILD_IMAGE_TEST_REGISTRY_HELPER_DIGEST=$REGISTRY_HELPER_DIGEST
export PATH="$BIN_DIR:$PATH"
unset HTTPS_PROXY HTTP_PROXY

pass=0
fail=0
OUT=''
ERR=''
RC=0

report_failure() {
	fail=$((fail + 1))
	printf 'FAIL [%s]: %s\n' "$1" "$2"
	[ -z "$OUT" ] || printf '    stdout | %s\n' "$OUT"
	[ -z "$ERR" ] || printf '    stderr | %s\n' "$ERR"
}

assert_equal() {
	case_name=$1 expected=$2 actual=$3 description=$4
	if [ "$actual" = "$expected" ]; then
		pass=$((pass + 1))
	else
		report_failure "$case_name" "$description: expected '$expected', got '$actual'"
	fi
}

assert_contains() {
	case_name=$1 haystack=$2 needle=$3 description=$4
	case "$haystack" in
	*"$needle"*) pass=$((pass + 1)) ;;
	*) report_failure "$case_name" "$description: missing '$needle'" ;;
	esac
}

assert_in_order() {
	case_name=$1 haystack=$2 first=$3 second=$4 third=$5 description=$6
	case "$haystack" in
	*"$first"*"$second"*"$third"*) pass=$((pass + 1)) ;;
	*) report_failure "$case_name" "$description" ;;
	esac
}

run_builder() {
	script=$1
	shift
	out_file=$TMP/stdout
	err_file=$TMP/stderr
	set +e
	bash "$SCRIPT_DIR/$script" "$@" >"$out_file" 2>"$err_file"
	RC=$?
	set -e
	OUT=$(cat "$out_file")
	ERR=$(cat "$err_file")
}

builders=(
	"build-exporter-image.sh:freeside-exporter"
	"build-agent-claude-image.sh:freeside-agent-claude"
	"build-agent-codex-image.sh:freeside-agent-codex"
)

for builder in "${builders[@]}"; do
	script=${builder%%:*}
	image=${builder#*:}
	case_name="$script without registry mode"
	: >"$LOG"
	run_builder "$script"
	assert_equal "$case_name" 2 "$RC" "exit status"
	assert_equal "$case_name" '' "$OUT" "stdout"
	assert_contains "$case_name" "$ERR" "one of --registry HOST[/PATH] or --local-registry-port PORT is required" "actionable error"
	assert_equal "$case_name" '' "$(cat "$LOG")" "build tool calls"

	if [ "$script" = build-agent-codex-image.sh ]; then
		case_name="$script version override without package digest"
		: >"$LOG"
		run_builder "$script" --registry registry.example/freeside --codex-version 0.147.0
		assert_equal "$case_name" 2 "$RC" "exit status"
		assert_contains "$case_name" "$ERR" "--codex-version requires --package-sha256" "paired-pin error"
		assert_equal "$case_name" '' "$(cat "$LOG")" "build tool calls"

		case_name="$script package digest override without version"
		: >"$LOG"
		run_builder "$script" --registry registry.example/freeside \
			--package-sha256 "$(printf 'a%.0s' {1..64})"
		assert_equal "$case_name" 2 "$RC" "exit status"
		assert_contains "$case_name" "$ERR" "--package-sha256 requires --codex-version" "paired-pin error"
		assert_equal "$case_name" '' "$(cat "$LOG")" "build tool calls"

		case_name="$script malformed package digest"
		: >"$LOG"
		run_builder "$script" --registry registry.example/freeside \
			--codex-version 0.147.0 --package-sha256 ABC
		assert_equal "$case_name" 2 "$RC" "exit status"
		assert_contains "$case_name" "$ERR" "--package-sha256 must be 64 lowercase hex digits" "digest-format error"
		assert_equal "$case_name" '' "$(cat "$LOG")" "build tool calls"
	fi

	case_name="$script external registry"
	: >"$LOG"
	args=(--registry registry.example/freeside --ref-tag contract-test)
	case "$script" in
	build-agent-claude-image.sh)
		args+=(--claude-version 2.1.220)
		;;
	build-agent-codex-image.sh)
		args+=(--codex-version 0.147.0 --package-sha256 "$(printf 'a%.0s' {1..64})")
		;;
	esac
	expected_ref="registry.example/freeside/${image}@${DIGEST}"
	export BUILD_IMAGE_TEST_EXACT_REF=$expected_ref
	export BUILD_IMAGE_TEST_SEEDED_DIGEST=$DIGEST
	unset BUILD_IMAGE_TEST_DELETE_FAIL
	run_builder "$script" "${args[@]}"
	call_log=$(cat "$LOG")
	assert_equal "$case_name" 0 "$RC" "exit status"
	assert_equal "$case_name" "$expected_ref" "$OUT" "stdout reference"
	assert_contains "$case_name" "$call_log" "image push --scheme auto registry.example/freeside/${image}:contract-test" "push call"
	assert_contains "$case_name" "$call_log" "image pull --scheme auto $expected_ref" "exact digest pull"
	assert_contains "$case_name" "$call_log" "image inspect $expected_ref" "seeded digest verification"
	if [ "$script" = build-agent-codex-image.sh ]; then
		assert_contains "$case_name" "$call_log" \
			"--build-arg CODEX_VERSION=0.147.0 --build-arg CODEX_PACKAGE_SHA256=$(printf 'a%.0s' {1..64})" \
			"package pin build arguments"
	fi
	assert_in_order "$case_name" "$call_log" \
		"image push --scheme auto registry.example/freeside/${image}:contract-test" \
		"image pull --scheme auto $expected_ref" \
		"image inspect $expected_ref" \
		"push, exact pull, and digest verification were not ordered"

	case_name="$script mismatched registry digest"
	: >"$LOG"
	export BUILD_IMAGE_TEST_SEEDED_DIGEST=$OTHER_DIGEST
	run_builder "$script" "${args[@]}"
	assert_equal "$case_name" 1 "$RC" "exit status"
	assert_equal "$case_name" '' "$OUT" "stdout"
	assert_contains "$case_name" "$ERR" "seeded digest $OTHER_DIGEST does not match built digest $DIGEST" "mismatch error"

	case_name="$script local registry"
	: >"$LOG"
	args=(--local-registry-port 5000 --ref-tag contract-test)
	case "$script" in
	build-agent-claude-image.sh)
		args+=(--claude-version 2.1.220)
		;;
	build-agent-codex-image.sh)
		args+=(--codex-version 0.147.0 --package-sha256 "$(printf 'a%.0s' {1..64})")
		;;
	esac
	expected_ref="127.0.0.1:5000/${image}@${DIGEST}"
	export BUILD_IMAGE_TEST_EXACT_REF=$expected_ref
	export BUILD_IMAGE_TEST_SEEDED_DIGEST=$DIGEST
	run_builder "$script" "${args[@]}"
	call_log=$(cat "$LOG")
	assert_equal "$case_name" 0 "$RC" "exit status"
	assert_equal "$case_name" "$expected_ref" "$OUT" "stdout reference"
	assert_contains "$case_name" "$call_log" "--publish 127.0.0.1:5000:5000" "loopback registry publication"
	assert_contains "$case_name" "$call_log" "image push --scheme http 127.0.0.1:5000/${image}:contract-test" "push call"
	assert_contains "$case_name" "$call_log" "image pull --scheme http $expected_ref" "exact digest pull"
	assert_contains "$case_name" "$call_log" "image inspect $expected_ref" "seeded digest verification"
	assert_contains "$case_name" "$call_log" "delete --force freeside-" "registry cleanup"
	assert_in_order "$case_name" "$call_log" \
		"image push --scheme http 127.0.0.1:5000/${image}:contract-test" \
		"image pull --scheme http $expected_ref" \
		"image inspect $expected_ref" \
		"push, exact pull, and digest verification were not ordered"

	case_name="$script local registry cleanup failure"
	: >"$LOG"
	export BUILD_IMAGE_TEST_DELETE_FAIL=1
	run_builder "$script" "${args[@]}"
	assert_equal "$case_name" 1 "$RC" "exit status"
	assert_equal "$case_name" '' "$OUT" "stdout"
	assert_contains "$case_name" "$ERR" "could not remove temporary registry" "cleanup error"
	unset BUILD_IMAGE_TEST_DELETE_FAIL
done

printf '%s assertions passed, %s failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
