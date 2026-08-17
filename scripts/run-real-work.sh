#!/usr/bin/env bash
# run-real-work.sh — the §11 1A.2 gated-unattended production exercise.
#
# Usage: run-real-work.sh <spec-file> <resolved-policy-keys.json> <publication.json> [work-unit.json]
#
# Submits one source work item through `freesided submit`, runs the daemon with
# the production Claude driver, and pauses at the human specification-approval
# gate. After an operator approves the spec in a Freeside client, the daemon
# runs implementation to a ready-for-review outcome and the script verifies
# the durable export, networkless verification evidence, publication outcome,
# and exact published head with the real-run harness test. A durable
# elaboration failure exits promptly with its recorded diagnostic instead of
# waiting for the global deadline.
#
# It never mints its own preconditions. Every binding below is the
# operator's, supplied through the environment, because each one lands in
# a durable admission record and a script-invented default would make that
# record attest to something nobody approved.
#
# Required environment:
#   FREESIDE_REAL_RUN_STATE_ROOT     daemon state root (holds the SQLite store)
#   FREESIDE_REAL_RUN_LISTEN         fixed, nonzero signet listener address
#   FREESIDE_REAL_RUN_AGENT_IMAGE    digest-pinned admitted project image
#   FREESIDE_WARD_EXPORTER_IMAGE     digest-pinned export helper image
#   FREESIDE_REAL_RUN_REVIEW_IMAGE   digest-pinned Codex reviewer image
#   FREESIDE_REAL_RUN_REVIEW_INPUT_ROOT private root containing the review
#                                    credential and instruction snapshots
#   FREESIDE_REAL_RUN_REVIEW_AUTH_MODE subscription or api_key
#   FREESIDE_REAL_RUN_REVIEW_AUTH_IDENTITY Codex reviewer auth identity id
#   FREESIDE_REAL_RUN_REVIEW_AUTH_SNAPSHOT Codex auth snapshot under the input root
#   FREESIDE_REAL_RUN_REVIEW_INSTRUCTIONS composed AGENTS.md snapshot under the input root
#   FREESIDE_REAL_RUN_REVIEW_MODEL   explicit Codex reviewer model
#   FREESIDE_REAL_RUN_REVIEW_REASONING explicit reviewer reasoning effort
#   FREESIDE_REAL_RUN_REVIEW_COST_OWNER account charged for review
#   FREESIDE_REAL_RUN_SEED_ROOT      daemon-owned exact-base checkout root
#   FREESIDE_REAL_RUN_AUTH_IDENTITY  provider auth identity id
#   FREESIDE_REAL_RUN_AUTH_VOLUME    that identity's credential volume
#   FREESIDE_REAL_RUN_REPO           managed owner/name repository
#   FREESIDE_REAL_RUN_REPOSITORY_ID  canonical numeric repository id
#   FREESIDE_REAL_RUN_BASE_REF       short base branch name (for example main)
#   FREESIDE_REAL_RUN_BASE_SHA       exact 40-character base commit
#   FREESIDE_REAL_RUN_PROMPT_PACKAGE trusted prompt-package file
#   FREESIDE_REAL_RUN_ELABORATION_PROMPT_PACKAGE trusted elaborator prompt-package file
#   FREESIDE_REAL_RUN_INSTRUCTIONS   host vendor-instruction file (CLAUDE.md)
#   FREESIDE_REAL_RUN_APPROVED_RECIPE exact recipe digest approved by onboarding
#   FREESIDE_REAL_RUN_APP_STATE      GitHub App authority state directory
#   FREESIDE_REAL_RUN_APP_CREDS      GitHub App credential directory
#   FREESIDE_REAL_RUN_PROJECT        project id the run belongs to
#   FREESIDE_REAL_RUN_ALLOWED_PATHS  comma-separated declared path scope the
#                                    agent may rewrite (no match-everything
#                                    default: it is a containment control)
# Optional environment:
#   FREESIDE_REAL_RUN_RIG_RELEASE_TIMEOUT_SECONDS clean rig-holder shutdown
#                                    bound (default 30)
#   FREESIDE_REAL_RUN_DIAGNOSTIC_DIR operator-visible diagnostic destination
#                                    (default current directory)
#
# The harness supplies FREESIDE_REAL_RUN_IMPLEMENTATION_RUN_ID,
# FREESIDE_REAL_RUN_IMPLEMENTATION_INVOCATION, and (when present)
# FREESIDE_REAL_RUN_ELABORATION_RUN_ID to its verifier after submit.
# A manual verifier run must set both to the implementation-lane identities;
# the former generic FREESIDE_REAL_RUN_RUN_ID and
# FREESIDE_REAL_RUN_INVOCATION names are rejected.
#
# Requires: Go, Apple `container` running, macOS, an authenticated credential
# volume for the named identity, and an operator watching a Freeside client to
# approve or revise the generated specification. The harness runs and durably
# records the exact production configuration's ward conformance suite before
# the daemon can admit the submitted work.
# The publication JSON is durable operator input with this shape:
#   {"title":"Imperative PR title","body":"Reviewer-ready PR body",
#    "commit_author":{"app_slug":"canonical-app-slug","bot_user_id":123}}
# The slug and bot user ID claim the selected GitHub App bot's public canonical
# attribution fields. Before execution, the daemon resolves that account from
# the App registration selected by its installation token and requires an
# exact match. The fields contain no credential or publication authority.
# The optional fourth argument is the §5.18 work-unit declaration JSON that
# `freesided submit -work-unit` captures (completion criterion, bound issue,
# dependencies). It is operator input like the other three: its canonical
# digest joins the run-identity derivation, so a declared submission is a
# distinct work item from an undeclared submission of the same spec,
# policy, and publication bytes.
set -euo pipefail
umask 077

spec_file="${1:-}"
policy_file="${2:-}"
publication_file="${3:-}"
work_unit_file="${4:-}"
if [[ -z "$spec_file" || -z "$policy_file" || -z "$publication_file" ]]; then
  echo "usage: run-real-work.sh <spec-file> <resolved-policy-keys.json> <publication.json> [work-unit.json]" >&2
  exit 2
fi
for path in "$spec_file" "$policy_file" "$publication_file" ${work_unit_file:+"$work_unit_file"}; do
  if [[ ! -f "$path" ]]; then
    echo "run-real-work: $path is not a file" >&2
    exit 2
  fi
done

required=(
  FREESIDE_REAL_RUN_STATE_ROOT FREESIDE_REAL_RUN_LISTEN
  FREESIDE_REAL_RUN_AGENT_IMAGE FREESIDE_WARD_EXPORTER_IMAGE
  FREESIDE_REAL_RUN_REVIEW_IMAGE FREESIDE_REAL_RUN_REVIEW_INPUT_ROOT
  FREESIDE_REAL_RUN_REVIEW_AUTH_MODE FREESIDE_REAL_RUN_REVIEW_AUTH_IDENTITY
  FREESIDE_REAL_RUN_REVIEW_AUTH_SNAPSHOT FREESIDE_REAL_RUN_REVIEW_INSTRUCTIONS
  FREESIDE_REAL_RUN_REVIEW_MODEL FREESIDE_REAL_RUN_REVIEW_REASONING
  FREESIDE_REAL_RUN_REVIEW_COST_OWNER
  FREESIDE_REAL_RUN_SEED_ROOT FREESIDE_REAL_RUN_AUTH_IDENTITY FREESIDE_REAL_RUN_AUTH_VOLUME
  FREESIDE_REAL_RUN_REPO FREESIDE_REAL_RUN_REPOSITORY_ID FREESIDE_REAL_RUN_BASE_REF
  FREESIDE_REAL_RUN_BASE_SHA FREESIDE_REAL_RUN_PROMPT_PACKAGE
  FREESIDE_REAL_RUN_ELABORATION_PROMPT_PACKAGE FREESIDE_REAL_RUN_INSTRUCTIONS
  FREESIDE_REAL_RUN_APPROVED_RECIPE
  FREESIDE_REAL_RUN_APP_STATE FREESIDE_REAL_RUN_APP_CREDS FREESIDE_REAL_RUN_PROJECT
  FREESIDE_REAL_RUN_ALLOWED_PATHS
)
missing=()
for name in "${required[@]}"; do
  if [[ -z "${!name:-}" ]]; then
    missing+=("$name")
  fi
done
if (( ${#missing[@]} > 0 )); then
  echo "run-real-work: missing required environment: ${missing[*]}" >&2
  exit 2
fi

# Digest pinning is the ward's own refusal; checking it here reports a
# configuration mistake now instead of a gate failure deep into a run.
for ref in "$FREESIDE_REAL_RUN_AGENT_IMAGE" "$FREESIDE_WARD_EXPORTER_IMAGE" \
  "$FREESIDE_REAL_RUN_REVIEW_IMAGE"; do
  if [[ "$ref" != *"@sha256:"* ]]; then
    echo "run-real-work: image reference is not digest-pinned: $ref" >&2
    exit 2
  fi
done
if [[ ! "$FREESIDE_REAL_RUN_APPROVED_RECIPE" =~ ^sha256:[0-9a-f]{64}$ ]]; then
  echo "run-real-work: FREESIDE_REAL_RUN_APPROVED_RECIPE is not a canonical digest" >&2
  exit 2
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/run-real-work-supervision.sh
source "$repo_root/scripts/run-real-work-supervision.sh"
workdir="$(mktemp -d)"
daemon_pid=""
rig_pid=""
elaboration_run_id=""
implementation_run_id=""
last_supervision_snapshot="$workdir/supervision.json"
diagnostic_path=""
rig_acquisition="$workdir/rig-acquisition.json"
rig_log="$workdir/rig-hold.log"
db_path="$FREESIDE_REAL_RUN_STATE_ROOT/freeside.db"
listen_address="$FREESIDE_REAL_RUN_LISTEN"
rig_release_timeout=${FREESIDE_REAL_RUN_RIG_RELEASE_TIMEOUT_SECONDS:-30}
diagnostic_dir=${FREESIDE_REAL_RUN_DIAGNOSTIC_DIR:-$PWD}

if [[ ! "$rig_release_timeout" =~ ^[1-9][0-9]*$ ]]; then
	echo "run-real-work: FREESIDE_REAL_RUN_RIG_RELEASE_TIMEOUT_SECONDS must be a positive integer" >&2
	exit 2
fi
if [[ ! -d "$diagnostic_dir" ]]; then
	echo "run-real-work: diagnostic directory is not a directory: $diagnostic_dir" >&2
	exit 2
fi

write_diagnostic() {
	local selected_run="" candidate
	[[ -z "$implementation_run_id" ]] || selected_run=$implementation_run_id
	[[ -n "$selected_run" || -z "$elaboration_run_id" ]] || selected_run=$elaboration_run_id
	[[ -n "$selected_run" ]] || return 0
	if [[ ! -s "$last_supervision_snapshot" ]]; then
		for candidate in "$selected_run" "$elaboration_run_id"; do
			[[ -n "$candidate" ]] || continue
			if "$workdir/freesided" follow -db "$db_path" -run "$candidate" -snapshot \
				-approved-recipe "$FREESIDE_REAL_RUN_APPROVED_RECIPE" \
				>"$last_supervision_snapshot" 2>/dev/null; then
				break
			fi
		done
	fi
	if [[ ! -s "$last_supervision_snapshot" ]]; then
		echo "run-real-work: could not produce the final diagnostic snapshot" >&2
		return 1
	fi
	diagnostic_path=$(mktemp "${diagnostic_dir%/}/freeside-real-work.XXXXXX.json")
	cp "$last_supervision_snapshot" "$diagnostic_path"
	echo "run-real-work: diagnostic artifact: $diagnostic_path" >&2
}

rig_child_active() {
	[[ -n "$rig_pid" ]] && jobs -pr | grep -qx -- "$rig_pid"
}

child_job_exists() {
	local child_pid=$1
	case " $(jobs -pr) $(jobs -ps) " in
	*" $child_pid "*) return 0 ;;
	*) return 1 ;;
	esac
}

rig_child_exists() {
	[[ -n "$rig_pid" ]] && child_job_exists "$rig_pid"
}

run_rig_cleanup() {
	local cleanup_pid cleanup_status cleanup_status_file
	cleanup_status_file=$workdir/rig-cleanup-status
	set -m
	(
		set +m
		set +e
		"$workdir/freesided" rig cleanup \
			-state-root "$FREESIDE_REAL_RUN_STATE_ROOT" \
			-token-file "$rig_acquisition" >/dev/null &
		cleanup_command_pid=$!
		# The command keeps the default TERM disposition. The supervisor ignores
		# TERM only after spawn, so a group cancellation reaches freesided and
		# lets its context terminate procbound runtime subprocess groups.
		trap '' TERM
		wait "$cleanup_command_pid"
		printf '%s\n' "$?" >"$cleanup_status_file"
		# Remain the process-group leader until the parent kills the whole
		# group. This reserves the PGID and keeps a failed cleanup's orphaned
		# runtime child inside the authority boundary until it is terminated.
		while :; do sleep 3600; done
	) &
	cleanup_pid=$!
	for _ in $(seq 1 $((rig_release_timeout * 10))); do
		[[ ! -s "$cleanup_status_file" ]] || break
		sleep 0.1
	done
	if [[ -s "$cleanup_status_file" ]]; then
		read -r cleanup_status <"$cleanup_status_file" || cleanup_status=1
	else
		echo "run-real-work: exact-resource cleanup exceeded ${rig_release_timeout}s; cancelling it" >&2
		cleanup_status=124
		kill -TERM -- "-$cleanup_pid" 2>/dev/null || true
		for _ in $(seq 1 $((rig_release_timeout * 10))); do
			[[ ! -s "$cleanup_status_file" ]] || break
			sleep 0.1
		done
	fi
	# The supervisor never exits by itself, so it keeps this PGID reserved until
	# this signal and prevents the number from naming an unrelated process group.
	kill -KILL -- "-$cleanup_pid" 2>/dev/null || true
	wait "$cleanup_pid" 2>/dev/null || true
	set +m
	rm -f "$cleanup_status_file"
	return "$cleanup_status"
}

require_live_rig() {
	if ! rig_child_active || ! "$workdir/freesided" rig check \
		-state-root "$FREESIDE_REAL_RUN_STATE_ROOT" \
		-token-file "$rig_acquisition"; then
		echo "run-real-work: production rig lease holder is no longer live" >&2
		return 1
	fi
}

cleanup() {
	status=$?
	trap - EXIT
	cleanup_failed=false
	if ! write_diagnostic; then
		cleanup_failed=true
	fi
  if [[ -n "$daemon_pid" ]] && kill -0 "$daemon_pid" 2>/dev/null; then
    kill "$daemon_pid" 2>/dev/null || true
    # A wedged writer container can block the daemon's own shutdown, and this
    # trap is the last thing the operator is waiting on: bound the graceful
    # wait, then stop asking. Leftover runtime objects carry the run labels,
    # so they stay reapable by hand rather than being lost.
    for _ in $(seq 1 30); do
      kill -0 "$daemon_pid" 2>/dev/null || break
      sleep 1
    done
    if kill -0 "$daemon_pid" 2>/dev/null; then
      echo "run-real-work: daemon did not exit within 30s; sending SIGKILL." \
        "Check for leftover \`container\` instances labelled with this run" >&2
      kill -9 "$daemon_pid" 2>/dev/null || true
    fi
    wait "$daemon_pid" 2>/dev/null || true
	fi
	if [[ -n "$rig_pid" ]]; then
		if rig_child_exists; then
			if run_rig_cleanup; then
				if rig_child_exists; then
					kill -USR1 "$rig_pid" 2>/dev/null || true
				fi
				for _ in $(seq 1 "$rig_release_timeout"); do
					rig_child_exists || break
					sleep 1
				done
				if rig_child_exists; then
					echo "run-real-work: rig holder did not exit within ${rig_release_timeout}s; sending SIGKILL and preserving the stale rig manifest" >&2
					kill -KILL "$rig_pid" 2>/dev/null || true
					cleanup_failed=true
				fi
			else
				echo "run-real-work: exact-resource cleanup failed; preserving the stale rig manifest" >&2
				if rig_child_exists; then
					kill -KILL "$rig_pid" 2>/dev/null || true
				fi
				cleanup_failed=true
			fi
		else
			echo "run-real-work: rig holder exited; preserving the stale rig manifest" >&2
			cleanup_failed=true
		fi
		if ! wait "$rig_pid" 2>/dev/null && [[ "$cleanup_failed" == false ]]; then
			echo "run-real-work: rig holder failed during release; preserving its diagnostics" >&2
			cleanup_failed=true
		fi
	fi
	rm -rf "$workdir"
	if [[ "$cleanup_failed" == true && "$status" -eq 0 ]]; then
		exit 1
	fi
	exit "$status"
}
trap cleanup EXIT

echo "building freesided" >&2
build_version="$(git -C "$repo_root" rev-parse --short=12 HEAD)"
(cd "$repo_root/daemon" && go build -ldflags "-X main.version=$build_version" -o "$workdir/freesided" ./cmd/freesided)

echo "acquiring the production rig lease" >&2
"$workdir/freesided" rig hold \
	-state-root "$FREESIDE_REAL_RUN_STATE_ROOT" \
	-db "$db_path" \
	-listen "$listen_address" \
	-seed-root "$FREESIDE_REAL_RUN_SEED_ROOT" \
	>"$rig_acquisition" 2>"$rig_log" &
rig_pid=$!
for _ in $(seq 1 100); do
	[[ ! -s "$rig_acquisition" ]] || break
	if ! rig_child_active; then
		wait "$rig_pid" 2>/dev/null || true
		rig_pid=""
		cat "$rig_log" >&2
		exit 1
	fi
	sleep 0.05
done
if [[ ! -s "$rig_acquisition" ]]; then
	echo "run-real-work: timed out waiting for the production rig lease" >&2
	if rig_child_active; then
		kill -KILL "$rig_pid" 2>/dev/null || true
	fi
	wait "$rig_pid" 2>/dev/null || true
	rig_pid=""
	exit 1
fi
FREESIDE_REAL_RUN_STATE_ROOT="$("$workdir/freesided" rig resource \
	-token-file "$rig_acquisition" -name state-root)"
db_path="$("$workdir/freesided" rig resource \
	-token-file "$rig_acquisition" -name database-path)"
listen_address="$("$workdir/freesided" rig resource \
	-token-file "$rig_acquisition" -name listen-address)"
FREESIDE_REAL_RUN_SEED_ROOT="$("$workdir/freesided" rig resource \
	-token-file "$rig_acquisition" -name seed-root)"
export FREESIDE_REAL_RUN_STATE_ROOT FREESIDE_REAL_RUN_SEED_ROOT
require_live_rig

# Duplicated from observerScript, observerGitScript, and credObserverScript in
# daemon/internal/ward/spec.go. Shell syntax and builtins are excluded; every
# external command any generated observer can execute is listed so an outdated
# exporter pin fails before build and submit.
required_exporter_tools=(
  sh git env mkdir rm cat head find xargs sort cmp sha256sum cut readlink stat ls sync
)
# shellcheck disable=SC2016 # The probed container's shell expands this script.
exporter_probe_script='missing=0
for tool do
  if ! command -v "$tool" >/dev/null 2>&1; then
    printf "freeside-missing-tool:%s\n" "$tool"
    missing=1
  fi
done
if [ "$missing" -ne 0 ]; then exit 127; fi'
exporter_probe_output_cap=$((64 << 10))
exporter_probe_output_file="$workdir/exporter-preflight.log"
exporter_probe_nul_stripped_file="$workdir/exporter-preflight-no-nul.log"
set +e
container run --rm --network none -- \
  "$FREESIDE_WARD_EXPORTER_IMAGE" sh -c "$exporter_probe_script" sh \
  "${required_exporter_tools[@]}" 2>&1 |
  head -c "$((exporter_probe_output_cap + 1))" >"$exporter_probe_output_file"
exporter_probe_pipeline_status=("${PIPESTATUS[@]}")
set -e
exporter_probe_status=${exporter_probe_pipeline_status[0]}
exporter_probe_capture_status=${exporter_probe_pipeline_status[1]}
exporter_probe_output_size=$(wc -c <"$exporter_probe_output_file")
if [[ "$exporter_probe_status" -eq 0 && "$exporter_probe_capture_status" -eq 0 &&
  "$exporter_probe_output_size" -le "$exporter_probe_output_cap" ]]; then
  :
else
  missing_marker_lines=""
  if [[ "$exporter_probe_capture_status" -eq 0 &&
    "$exporter_probe_output_size" -le "$exporter_probe_output_cap" ]] &&
    LC_ALL=C tr -d '\000' <"$exporter_probe_output_file" \
      >"$exporter_probe_nul_stripped_file" &&
    cmp -s "$exporter_probe_output_file" "$exporter_probe_nul_stripped_file"; then
    missing_marker_lines=$(sed -n 's/^freeside-missing-tool://p' \
      "$exporter_probe_output_file")
  fi
  missing_exporter_tools=""
  exporter_probe_markers_valid=true
  while IFS= read -r missing_tool; do
    [[ -n "$missing_tool" ]] || continue
    missing_tool_known=false
    for required_tool in "${required_exporter_tools[@]}"; do
      if [[ "$missing_tool" == "$required_tool" ]]; then
        missing_tool_known=true
        break
      fi
    done
    if [[ "$missing_tool_known" != true ||
      " $missing_exporter_tools " == *" $missing_tool "* ]]; then
      exporter_probe_markers_valid=false
      break
    fi
    missing_exporter_tools="${missing_exporter_tools:+$missing_exporter_tools }$missing_tool"
  done <<EOF
$missing_marker_lines
EOF
  if [[ "$exporter_probe_status" -eq 127 &&
    "$exporter_probe_markers_valid" == true && -n "$missing_exporter_tools" ]]; then
    echo "run-real-work: exporter image $FREESIDE_WARD_EXPORTER_IMAGE is missing required observer tools: $missing_exporter_tools" >&2
    exit 2
  fi
  if [[ "$exporter_probe_output_size" -gt "$exporter_probe_output_cap" ]]; then
    echo "run-real-work: exporter preflight output exceeded the ${exporter_probe_output_cap}-byte cap" >&2
  elif [[ "$exporter_probe_capture_status" -ne 0 ]]; then
    echo "run-real-work: could not capture exporter preflight output" >&2
  fi
  echo "run-real-work: could not preflight exporter image $FREESIDE_WARD_EXPORTER_IMAGE (container exit $exporter_probe_status)" >&2
  exit 2
fi

echo "submitting the work item" >&2
require_live_rig
submit_log="$workdir/submit.json"
submit_args=(
  -db "$db_path"
  --spec "$spec_file"
  --policy "$policy_file"
  --publication "$publication_file"
  --project "$FREESIDE_REAL_RUN_PROJECT"
)
if [[ -n "$work_unit_file" ]]; then
  submit_args+=(--work-unit "$work_unit_file")
fi
"$workdir/freesided" submit "${submit_args[@]}" | tee "$submit_log"

implementation_invocation_id="$(sed -n 's/.*"implementation_invocation_id":"\([^"]*\)".*/\1/p' "$submit_log")"
implementation_run_id="$(sed -n 's/.*"run_id":"\([^"]*\)".*/\1/p' "$submit_log")"
elaboration_run_id="$(sed -n 's/.*"elaboration_run_id":"\([^"]*\)".*/\1/p' "$submit_log")"
elaboration_invocation_id="$(sed -n 's/.*"elaboration_invocation_id":"\([^"]*\)".*/\1/p' "$submit_log")"
if [[ -z "$implementation_invocation_id" || -z "$implementation_run_id" ]]; then
  echo "run-real-work: submit produced no implementation run identity: $(cat "$submit_log")" >&2
  exit 1
fi
if [[ -n "$elaboration_run_id" && -n "$elaboration_invocation_id" ]]; then
  echo "submitted elaboration run=$elaboration_run_id invocation=$elaboration_invocation_id" >&2
elif [[ -n "$elaboration_run_id" || -n "$elaboration_invocation_id" ]]; then
  echo "run-real-work: submit produced a partial elaboration identity: $(cat "$submit_log")" >&2
  exit 1
else
  echo "legacy production-only replay: no elaboration approval gate" >&2
fi
echo "reserved implementation run=$implementation_run_id invocation=$implementation_invocation_id" >&2

elaboration_verifier_env=(-u FREESIDE_REAL_RUN_ELABORATION_RUN_ID)
if [[ -n "$elaboration_run_id" ]]; then
	elaboration_verifier_env+=(FREESIDE_REAL_RUN_ELABORATION_RUN_ID="$elaboration_run_id")
fi

# Seed the durable auth-identity binding before the daemon can reach
# admission. The verifier records it too, but that call happens inside the
# polling loop, i.e. after the daemon is already dispatching: leaving it there
# makes the harness race its own precondition. Running the verifier once here
# is the seeding step; it exits without verifying because no invocation id is
# set yet.
# Both implementation identity variables are unset for this call on purpose:
# exported values left over from an earlier run would make the seeding step
# verify that old invocation instead of skipping, and its failure would surface
# here as the misleading "could not record the auth identity binding". The
# legacy generic names are scrubbed from every verifier call too, so stale
# operator exports cannot trip the verifier's migration guard.
echo "recording the auth identity binding" >&2
env -u FREESIDE_REAL_RUN_RUN_ID -u FREESIDE_REAL_RUN_INVOCATION \
  -u FREESIDE_REAL_RUN_IMPLEMENTATION_RUN_ID \
  -u FREESIDE_REAL_RUN_IMPLEMENTATION_INVOCATION \
	-u FREESIDE_REAL_RUN_ELABORATION_RUN_ID \
  FREESIDE_REAL_RUN_LIVE_TEST=1 \
  go test -C "$repo_root/daemon" ./internal/integration/ \
    -run TestRealWorkItemCompletesProductionPipeline -count=1 > "$workdir/seed.log" 2>&1 || {
  echo "run-real-work: could not record the auth identity binding" >&2
  cat "$workdir/seed.log" >&2
  exit 1
}

echo "starting the daemon with the production Claude driver" >&2
require_live_rig
# FREESIDE_REAL_RUN_LISTEN pins the exact leased listener so an operator's
# paired client can reach the specification-approval gate.
"$workdir/freesided" \
  -listen "$listen_address" \
  -db "$db_path" \
  -driver claude \
  -agent-image "$FREESIDE_REAL_RUN_AGENT_IMAGE" \
  -exporter-image "$FREESIDE_WARD_EXPORTER_IMAGE" \
  -review-image "$FREESIDE_REAL_RUN_REVIEW_IMAGE" \
  -review-input-root "$FREESIDE_REAL_RUN_REVIEW_INPUT_ROOT" \
  -review-auth-mode "$FREESIDE_REAL_RUN_REVIEW_AUTH_MODE" \
  -review-auth-identity "$FREESIDE_REAL_RUN_REVIEW_AUTH_IDENTITY" \
  -review-auth-snapshot "$FREESIDE_REAL_RUN_REVIEW_AUTH_SNAPSHOT" \
  -review-instructions "$FREESIDE_REAL_RUN_REVIEW_INSTRUCTIONS" \
  -review-model "$FREESIDE_REAL_RUN_REVIEW_MODEL" \
  -review-reasoning-effort "$FREESIDE_REAL_RUN_REVIEW_REASONING" \
  -review-cost-owner "$FREESIDE_REAL_RUN_REVIEW_COST_OWNER" \
  -seed-root "$FREESIDE_REAL_RUN_SEED_ROOT" \
  -state-dir "$FREESIDE_REAL_RUN_STATE_ROOT" \
  -rig-token-file "$rig_acquisition" \
  -prompt-package "$FREESIDE_REAL_RUN_PROMPT_PACKAGE" \
  -elaboration-prompt-package "$FREESIDE_REAL_RUN_ELABORATION_PROMPT_PACKAGE" \
  -vendor-instructions "$FREESIDE_REAL_RUN_INSTRUCTIONS" \
  -repo "$FREESIDE_REAL_RUN_REPO" \
  -repository-id "$FREESIDE_REAL_RUN_REPOSITORY_ID" \
  -base-ref "$FREESIDE_REAL_RUN_BASE_REF" \
  -base-sha "$FREESIDE_REAL_RUN_BASE_SHA" \
  -auth-identity "$FREESIDE_REAL_RUN_AUTH_IDENTITY" \
  -approved-recipe "$FREESIDE_REAL_RUN_APPROVED_RECIPE" \
  -operating-mode unattended \
  -run-conformance \
  -allowed-paths "$FREESIDE_REAL_RUN_ALLOWED_PATHS" \
  -publication-state-dir "$FREESIDE_REAL_RUN_APP_STATE" \
  -publication-credentials-dir "$FREESIDE_REAL_RUN_APP_CREDS" \
  > "$workdir/daemon.log" 2>&1 &
daemon_pid=$!

if [[ -n "$elaboration_run_id" ]]; then
  echo "gated-unattended: waiting for an operator to approve or revise the generated specification" >&2
  echo "implementation verification resumes automatically after approval" >&2
fi

# Follow durable, read-only snapshots instead of rerunning the integration
# verifier as a polling mechanism. The verifier below remains the one final
# success authority after observation reaches published.
set +e
real_work_supervise "$workdir/freesided" "$db_path" "$elaboration_run_id" \
	"$implementation_run_id" "$daemon_pid" \
	"${FREESIDE_REAL_RUN_TIMEOUT_SECONDS:-2400}" "$last_supervision_snapshot"
supervision_status=$?
set -e
if [[ "$supervision_status" -ne 0 ]]; then
	if [[ "$supervision_status" -eq 124 ]]; then
		echo "daemon log:" >&2
		tail -50 "$workdir/daemon.log" >&2
	fi
	exit "$supervision_status"
fi

kill "$daemon_pid" 2>/dev/null || true
wait "$daemon_pid" 2>/dev/null || true
daemon_pid=""

# Positive evidence, not the absence of an error: a Go test binary exits 0
# for a skipped test too, so require the harness's own success line.
verify_log="$workdir/verify-final.log"
set +e
env -u FREESIDE_REAL_RUN_RUN_ID -u FREESIDE_REAL_RUN_INVOCATION \
	"${elaboration_verifier_env[@]}" \
  FREESIDE_REAL_RUN_LIVE_TEST=1 \
  FREESIDE_REAL_RUN_IMPLEMENTATION_RUN_ID="$implementation_run_id" \
  FREESIDE_REAL_RUN_IMPLEMENTATION_INVOCATION="$implementation_invocation_id" \
  go test -C "$repo_root/daemon" ./internal/integration/ \
    -run TestRealWorkItemCompletesProductionPipeline -count=1 -v 2>&1 | tee "$verify_log"
verify_status=${PIPESTATUS[0]}
set -e
if grep -q "real run elaboration failed:" "$verify_log"; then
	echo "run-real-work: elaboration failed before implementation admission" >&2
	echo "daemon log:" >&2
	tail -50 "$workdir/daemon.log" >&2
	exit 1
fi
if [[ "$verify_status" -ne 0 ]] ||
	! grep -q "real production pipeline verified: PR #" "$verify_log"; then
  echo "run-real-work: the run did not reach a verified ready publication" >&2
  echo "daemon log:" >&2
  tail -50 "$workdir/daemon.log" >&2
  exit 1
fi
echo "run-real-work: verified ready publication for implementation run=$implementation_run_id invocation=$implementation_invocation_id" >&2
