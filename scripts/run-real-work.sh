#!/usr/bin/env bash
# run-real-work.sh — the §11 1A.2 gated-unattended production exercise.
#
# Usage: run-real-work.sh <spec-file> <resolved-policy-keys.json> <publication.json> [work-unit.json]
#        run-real-work.sh --client-target <spec-file> <resolved-policy-keys.json> <publication.json> [work-unit.json]
#        run-real-work.sh --resume-session <completed-session-directory>
#        run-real-work.sh --recover-codex-credentials [--approved-recipe <digest>]...
# Resume keeps the existing run and starts no submission or client command.
# Recovery serves paired-client credential recovery with execution disabled.
# It uses the same required environment, but takes no submission files.
#
# Client-target mode submits the three files exactly as the CLI mode does, but
# only as a visibility seed it never follows or verifies. It then waits for the
# operator to submit the real target from a Freeside client and to name it with
# `real-work-session.sh select-target <session> <task-id>`. The harness saves
# that selection durably before it follows anything, resolves the selected
# task's specification and implementation runs by trusted identity (never by
# matching source text), and follows and verifies that task with the same rigor
# as the CLI mode. It requires FREESIDE_REAL_RUN_MANUAL_SUBMISSION_CONFIG and
# cannot combine with --resume-session or --recover-codex-credentials. A saved
# selection cannot be replaced in the same session. Client-target session files:
# mode (the literal client-target), daemon-started-at (Unix nanoseconds recorded
# just before the daemon launch), seed-specification-run (the ignored seed's
# specification run), target.request (the operator's pending selection) and
# target.json (the durable selection plus its resolved implementation run).
#
# Submits one task's source specification through `freesided submit`, runs
# the daemon with the production Claude driver, and pauses at the human
# specification-approval gate. After approval in a Freeside client, the daemon
# runs implementation to a ready-for-review outcome and the script verifies
# the durable export, networkless verification evidence, publication outcome,
# and exact published head with the real-run harness test. A durable
# specification failure exits promptly with its recorded diagnostic instead of
# waiting for the production deadline. Successful verification keeps this same
# daemon alive for a walkthrough until `real-work-session.sh complete` is used.
# See docs/production-walkthrough.md for completion, recovery and restoration.
#
# A scope-conflict question keeps the current policy immutable. To widen scope,
# choose stop, then submit a new run with a new FREESIDE_REAL_RUN_ALLOWED_PATHS
# and the matching resolved-policy paths key. Restart the daemon with that
# matching -allowed-paths value; the script supplies it from the environment.
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
#   FREESIDE_REAL_RUN_REVIEW_INSTRUCTIONS operator-host rules under the input root;
#                                    trusted-base repository AGENTS.md files are
#                                    discovered and composed separately
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
#   FREESIDE_REAL_RUN_REPOSITORY_CHECKOUT operator's local checkout of the
#                                    managed repository (FREESIDE_REAL_RUN_REPO)
#                                    whose origin proves the base and which
#                                    already contains the exact base commit. The
#                                    composition preflight verifies its origin
#                                    and that the base is reachable; it never
#                                    fetches into it, so keep it current.
#   FREESIDE_REAL_RUN_PROMPT_PACKAGE trusted prompt-package file
#   FREESIDE_REAL_RUN_SPECIFICATION_PROMPT_PACKAGE trusted specifier prompt-package file
#   FREESIDE_REAL_RUN_REMEDIATION_PROMPT_PACKAGE trusted remediator prompt-package file
#   FREESIDE_REAL_RUN_INSTRUCTIONS   host vendor-instruction file (CLAUDE.md)
#   FREESIDE_REAL_RUN_APPROVED_RECIPE exact recipe digest approved by onboarding
#   FREESIDE_REAL_RUN_APP_STATE      GitHub App authority state directory
#   FREESIDE_REAL_RUN_APP_CREDS      GitHub App credential directory
#   FREESIDE_REAL_RUN_PROJECT        project id the run belongs to
#   FREESIDE_REAL_RUN_ALLOWED_PATHS  comma-separated declared path scope the
#                                    agent may rewrite (no match-everything
#                                    default: it is a containment control)
# Optional environment:
#   FREESIDE_REAL_RUN_MANUAL_SUBMISSION_CONFIG operator JSON project policy
#                                    for new client tasks; privately retained.
#                                    Resume reuses the retained file unless an
#                                    explicit reviewed replacement is supplied.
#   FREESIDE_REAL_RUN_JUDGMENT_CLAUDE_BIN absolute native Claude CLI path
#   FREESIDE_REAL_RUN_JUDGMENT_CLAUDE_SHA256 exact executable content pin
#   FREESIDE_REAL_RUN_JUDGMENT_MODEL explicit Claude model for daemon judgments
#   FREESIDE_REAL_RUN_JUDGMENT_AUTH_SNAPSHOT existing setup-token file relative
#                                    to REVIEW_INPUT_ROOT; all four go together
#   When all four are set, bind the publication-author role prompt from this
#   checkout's prompts/publication-author.md. Preflight includes its content
#   digest in judgment_configuration_digest.
#   FREESIDE_REAL_RUN_TIMEOUT_SECONDS global supervision deadline in seconds;
#                                    a positive integer (default 3600). Bounds
#                                    the whole supervised workflow (spec-approval
#                                    wait through publication), so it must exceed
#                                    FREESIDE_REAL_RUN_WRITER_STOP_TIMEOUT plus
#                                    the preceding workflow time, or supervision
#                                    returns 124 and stops the daemon before the
#                                    writer budget is reached; raise both together
#   FREESIDE_REAL_RUN_WRITER_STOP_TIMEOUT max time the implementation writer
#                                    container may run before it must reach
#                                    observed stopped (a Go duration such as
#                                    45m; default 45m). Ward's own default is
#                                    10m, which aborts a legitimately longer
#                                    implementation at the writer-termination
#                                    handoff; this raises it for real runs. Must
#                                    stay below FREESIDE_REAL_RUN_TIMEOUT_SECONDS;
#                                    `freesided submit` enforces this and rejects
#                                    a malformed or out-of-range value before the
#                                    run is created.
#   FREESIDE_REAL_RUN_MAX_OBSERVATION_FAILURES consecutive transient
#                                    observation-failure budget before the run
#                                    is abandoned; a positive integer
#                                    (default 10)
#   FREESIDE_REAL_RUN_RIG_RELEASE_TIMEOUT_SECONDS clean rig-holder shutdown
#                                    bound (default 30)
#   FREESIDE_REAL_RUN_RESTORE_TIMEOUT_SECONDS supervised registration/health
#                                    wait after rig release (default 120)
#   FREESIDE_REAL_RUN_DIAGNOSTIC_DIR operator-visible diagnostic destination
#                                    (default ~/Library/Logs/Freeside)
#   FREESIDE_REAL_RUN_BUILD_PROXY   unauthenticated operator HTTP proxy that
#                                    replaces the managed build proxy when
#                                    building the already-pinned images;
#                                    live reachability is recorded not_run
#   FREESIDE_REAL_RUN_RESTORE_DAEMON installed app daemon for upgraded-session
#                                    restoration (default ~/Applications/Freeside.app/
#                                    Contents/Resources/freesided); must match
#                                    the retained daemon's Go build ID
#
# The harness supplies FREESIDE_REAL_RUN_IMPLEMENTATION_RUN_ID,
# FREESIDE_REAL_RUN_IMPLEMENTATION_INVOCATION, and (when present)
# FREESIDE_REAL_RUN_SPECIFICATION_RUN_ID to its verifier after submit.
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
#   {"title":"Imperative PR title","body":"Why and What prose",
#    "branch":"feat/meaningful-task-slug",
#    "commit_author":{"app_slug":"canonical-app-slug","bot_user_id":123}}
# The optional branch is an exact operator-declared head name. Omit it for
# freeside/publish/<identity-hex16>; refs/ and freeside/ names are reserved.
# Freeside writes Verification from the executed recipe and labels agent evidence
# as claims. A Verification heading or section marker in body is refused at
# submit. Publisher-owned sections reserve space, leaving 23,432 bytes for body.
# The slug and bot user ID claim the selected GitHub App bot's public canonical
# attribution fields. Before execution, the daemon resolves that account from
# the App registration selected by its installation token and requires an
# exact match. The fields contain no credential or publication authority.
# The optional fourth argument is the §5.18 work-unit declaration JSON that
# `freesided submit -work-unit` captures (completion criterion, bound issue,
# dependencies). It is operator input like the other three: its canonical
# digest joins the run-identity derivation, so a declared submission is a
# distinct implementation run from an undeclared submission of the same spec,
# policy, and publication bytes.
set -euo pipefail
umask 077

retained_session=""
recover_codex_credentials=false
recovery_approved_recipe_args=()
if [[ "${1:-}" == --recover-codex-credentials ]]; then
  recover_codex_credentials=true
  shift
  while [[ $# -gt 0 ]]; do
    if [[ "$1" != --approved-recipe || ! "${2:-}" =~ ^sha256:[0-9a-f]{64}$ ]]; then
      echo 'usage: run-real-work.sh --recover-codex-credentials [--approved-recipe sha256:<64 lowercase hex digits>]...' >&2
      exit 2
    fi
    recovery_approved_recipe_args+=(-approved-recipe "$2")
    shift 2
  done
fi
client_target=false
if [[ "${1:-}" == --client-target ]]; then
  client_target=true
  shift
fi
if [[ "${1:-}" == --resume-session ]]; then
  [[ $# == 2 && -d "$2" ]] || { echo 'usage: run-real-work.sh --resume-session <completed-session>' >&2; exit 2; }
  retained_session=$(cd "$2" && pwd)
  set -- "$retained_session/submission-inputs/spec.json" \
    "$retained_session/submission-inputs/policy.json" "$retained_session/submission-inputs/publication.json"
  [[ ! -f "$retained_session/submission-inputs/work-unit.json" ]] || set -- "$@" "$retained_session/submission-inputs/work-unit.json"
fi
if [[ "$client_target" == true && ( -n "$retained_session" || "$recover_codex_credentials" == true ) ]]; then
  echo 'run-real-work: --client-target cannot combine with --resume-session or --recover-codex-credentials' >&2
  exit 2
fi
spec_file="${1:-}"
policy_file="${2:-}"
publication_file="${3:-}"
work_unit_file="${4:-}"
if [[ "$recover_codex_credentials" == false ]]; then
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
fi

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
  FREESIDE_REAL_RUN_BASE_SHA FREESIDE_REAL_RUN_REPOSITORY_CHECKOUT
  FREESIDE_REAL_RUN_PROMPT_PACKAGE
  FREESIDE_REAL_RUN_SPECIFICATION_PROMPT_PACKAGE FREESIDE_REAL_RUN_REMEDIATION_PROMPT_PACKAGE
  FREESIDE_REAL_RUN_INSTRUCTIONS
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
if [[ "$client_target" == true && -z "${FREESIDE_REAL_RUN_MANUAL_SUBMISSION_CONFIG:-}" ]]; then
  echo 'run-real-work: --client-target requires FREESIDE_REAL_RUN_MANUAL_SUBMISSION_CONFIG so a client can create the target task' >&2
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
# shellcheck source=scripts/real-work-lifecycle.sh
source "$repo_root/scripts/real-work-lifecycle.sh"
# shellcheck source=scripts/real-work-client-target.sh
source "$repo_root/scripts/real-work-client-target.sh"
if [[ -n "$retained_session" ]]; then
  python3 "$repo_root/scripts/real-work-retained.py" validate "$retained_session"
fi
diagnostic_dir=${FREESIDE_REAL_RUN_DIAGNOSTIC_DIR:-$HOME/Library/Logs/Freeside}
if [[ -z "${FREESIDE_REAL_RUN_DIAGNOSTIC_DIR:-}" ]]; then
	mkdir -p "$diagnostic_dir"
fi
[[ -d "$diagnostic_dir" ]] || { echo 'run-real-work: diagnostic directory is not a directory' >&2; exit 2; }
diagnostic_dir=$(cd "$diagnostic_dir" && pwd)
workdir="$(mktemp -d "$diagnostic_dir/real-work-session.XXXXXX")"
cp "$repo_root/scripts/real-work-session.sh" "$repo_root/scripts/real-work-lifecycle.sh" \
	"$repo_root/app/scripts/restore-supervised-daemon.sh" \
  "$repo_root/scripts/real-work-verify.sh" "$repo_root/scripts/real-work-retained.py" "$workdir/"
printf 'starting\n' >"$workdir/status"
if [[ "$client_target" == true ]]; then
	printf 'client-target\n' >"$workdir/mode"
fi
echo "run-real-work: retained session: $workdir" >&2
daemon_pid=""
rig_pid=""
rig_acquired=false
specification_run_id=""
implementation_run_id=""
implementation_invocation_id=""
# The client target task, once selected (fresh) or inherited (resume). Empty in
# CLI mode, which keeps the verifier's target checks off.
target_task_id=""
last_supervision_snapshot="$workdir/supervision.json"
diagnostic_path=""
rig_acquisition="$workdir/rig-acquisition.json"
rig_log="$workdir/rig-hold.log"
composition_evidence_tmp=""
db_path="$FREESIDE_REAL_RUN_STATE_ROOT/freeside.db"
listen_address="$FREESIDE_REAL_RUN_LISTEN"
rig_release_timeout=${FREESIDE_REAL_RUN_RIG_RELEASE_TIMEOUT_SECONDS:-30}
# Supervision bounds the whole workflow (spec-approval wait through publication)
# and must outlast the writer budget plus that preceding time, or it returns 124
# and cleanup stops the daemon before the writer's own deadline can fire. The
# default is 45m writer plus 15m of spec-approval, preflight, and publication
# headroom; raise both together when raising the writer budget.
supervision_timeout=${FREESIDE_REAL_RUN_TIMEOUT_SECONDS:-3600}
writer_stop_timeout=${FREESIDE_REAL_RUN_WRITER_STOP_TIMEOUT:-45m}

if [[ ! "$rig_release_timeout" =~ ^[1-9][0-9]*$ ]]; then
	echo "run-real-work: FREESIDE_REAL_RUN_RIG_RELEASE_TIMEOUT_SECONDS must be a positive integer" >&2
	exit 2
fi
# Validate the supervision numeric overrides here, before the rig lease, submit,
# and daemon start, not only where real_work_supervise consumes them. A value
# rejected after submit would tear down a healthy daemon over an already
# dispatched invocation, the exact slot-consuming stranding this harness exists
# to avoid. real_work_supervise keeps its own defensive check for other callers.
if [[ ! "$supervision_timeout" =~ ^[1-9][0-9]*$ ]]; then
	echo "run-real-work: FREESIDE_REAL_RUN_TIMEOUT_SECONDS must be a positive integer" >&2
	exit 2
fi
# The writer-stop timeout is a Go duration. It is validated authoritatively by
# `freesided submit` below (flag.Duration parse, plus the writer-below-
# supervision relationship), before the run is created, so a malformed, out-of-
# range, or unsatisfiable value fails without stranding a submitted run. The
# harness deliberately does not re-derive Go's duration grammar here.
if [[ ! "${FREESIDE_REAL_RUN_MAX_OBSERVATION_FAILURES:-10}" =~ ^[1-9][0-9]*$ ]]; then
	echo "run-real-work: FREESIDE_REAL_RUN_MAX_OBSERVATION_FAILURES must be a positive integer" >&2
	exit 2
fi
if [[ ! -d "$diagnostic_dir" ]]; then
	echo "run-real-work: diagnostic directory is not a directory: $diagnostic_dir" >&2
	exit 2
fi
recovery_state_root=$FREESIDE_REAL_RUN_STATE_ROOT
[[ "$recovery_state_root" == /* ]] || recovery_state_root="$PWD/$recovery_state_root"
printf '%s\n' "$recovery_state_root" >"$workdir/state-root"
printf '%s\n' "$rig_release_timeout" >"$workdir/rig-timeout"

write_diagnostic() {
	local selected_run="" candidate
	# follow opens writable. A refusal before the upgrade boundary must not
	# migrate the retained database just to produce cleanup diagnostics.
	if [[ -n "$retained_session" && ! -f "$workdir/runtime-upgrade-started" ]]; then return 0; fi
	[[ -z "$implementation_run_id" ]] || selected_run=$implementation_run_id
	[[ -n "$selected_run" || -z "$specification_run_id" ]] || selected_run=$specification_run_id
	[[ -n "$selected_run" ]] || return 0
	if [[ ! -s "$last_supervision_snapshot" ]]; then
		for candidate in "$selected_run" "$specification_run_id"; do
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
	local child_pid=$1 children
	# Capture each builtin directly: grouping jobs on a pipeline's left side
	# creates a subshell with an empty job table in macOS Bash.
	children="$(jobs -pr)"$'\n'"$(jobs -ps)"
	grep -qx -- "$child_pid" <<<"$children"
}

rig_child_exists() {
	[[ -n "$rig_pid" ]] && child_job_exists "$rig_pid"
}

run_rig_cleanup() {
	real_work_bounded_rig "$workdir" "$rig_release_timeout" cleanup \
		-state-root "$FREESIDE_REAL_RUN_STATE_ROOT" -token-file "$rig_acquisition"
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
	# Defer exit without exporting an ignored signal disposition to cleanup
	# commands. Their handlers must still receive group cancellation.
	trap 'status=130' INT
	trap 'status=143' TERM
	local previous_status
	previous_status=$(cat "$workdir/status")
	printf 'stopping\n' >"$workdir/status"
	cleanup_failed=false
	[[ "$previous_status" != recovery-required ]] || cleanup_failed=true
	diagnostic_failed=false
	if ! write_diagnostic; then diagnostic_failed=true; fi
  if [[ -n "$daemon_pid" ]] && child_job_exists "$daemon_pid"; then
    kill "$daemon_pid" 2>/dev/null || true
    # A wedged writer container can block the daemon's own shutdown, and this
    # trap is the last thing the operator is waiting on: bound the graceful
    # wait, then stop asking. Leftover runtime objects carry the run labels,
    # so they stay reapable by hand rather than being lost.
    for _ in $(seq 1 30); do
      child_job_exists "$daemon_pid" || break
      sleep 1
    done
    if child_job_exists "$daemon_pid"; then
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
			echo "run-real-work: rig holder exited; checking stale recovery" >&2
			if ! real_work_bounded_rig "$workdir" "$rig_release_timeout" recover \
				-state-root "$FREESIDE_REAL_RUN_STATE_ROOT" -confirm; then
				cleanup_failed=true
			fi
			# A dead holder's exit status is not a clean-release signal. The
			# recovery command above is the sole authority for this path.
			wait "$rig_pid" 2>/dev/null || true
			rig_pid=""
		fi
		if [[ -n "$rig_pid" ]] && ! wait "$rig_pid" 2>/dev/null && [[ "$cleanup_failed" == false ]]; then
			echo "run-real-work: rig holder failed during release; preserving its diagnostics" >&2
			cleanup_failed=true
		fi
	fi
	[[ -z "$composition_evidence_tmp" ]] || rm -f "$composition_evidence_tmp"
	if [[ "$cleanup_failed" == false && "$rig_acquired" == true ]]; then
		: >"$workdir/rig-release-verified"
		printf 'rig-released\n' >"$workdir/status"
		if real_work_restore_supervised "$workdir"; then
			printf 'completed\n' >"$workdir/status"
		else
			cleanup_failed=true
			printf 'recovery-required\n' >"$workdir/status"
		fi
	elif [[ "$cleanup_failed" == true ]]; then
		printf 'recovery-required\n' >"$workdir/status"
	fi
	echo "run-real-work: logs and recovery inputs retained: $workdir" >&2
	if [[ "$cleanup_failed" == true ]]; then
		printf 'Recovery: bash %q recover %q\n' "$workdir/real-work-session.sh" "$workdir" >&2
	fi
	if [[ "$status" -eq 0 && ( "$cleanup_failed" == true || "$diagnostic_failed" == true ) ]]; then
		exit 1
	fi
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Stage the operator's submission inputs once, so the composition preflight
# and the durable submit consume the same bytes even if the original files
# change between the two steps.
submission_inputs="$workdir/submission-inputs"
submission_id=""
if [[ -n "$retained_session" ]]; then
  if [[ -f "$retained_session/submission-id" ]]; then
    submission_id=$(cat "$retained_session/submission-id")
    cp "$retained_session/submission-id" "$workdir/submission-id"
  fi
else
  submission_id=$(python3 -c 'import uuid; print(uuid.uuid4())')
  printf '%s\n' "$submission_id" > "$workdir/submission-id"
fi
if [[ "$recover_codex_credentials" == false ]]; then
  mkdir -p "$submission_inputs"
  cp "$spec_file" "$submission_inputs/spec.json"
  spec_file="$submission_inputs/spec.json"
  cp "$policy_file" "$submission_inputs/policy.json"
  policy_file="$submission_inputs/policy.json"
  cp "$publication_file" "$submission_inputs/publication.json"
  publication_file="$submission_inputs/publication.json"
  if [[ -n "$work_unit_file" ]]; then
    cp "$work_unit_file" "$submission_inputs/work-unit.json"
    work_unit_file="$submission_inputs/work-unit.json"
  fi
  # shellcheck source=scripts/real-work-manual-submission.sh
  source "$repo_root/scripts/real-work-manual-submission.sh"
  real_work_stage_manual_submission \
    "${FREESIDE_REAL_RUN_MANUAL_SUBMISSION_CONFIG:-}" "$retained_session" "$workdir"
fi

if [[ -n "$retained_session" ]]; then
  printf '%s\n' "$retained_session" > "$workdir/predecessor-session"
  approved_composition="$retained_session/retained-composition.json"
  [[ -f "$approved_composition" ]] || approved_composition="$retained_session/composition-manifest.json"
  cp "$approved_composition" "$workdir/retained-composition.json"
  cp "$retained_session/submit.json" "$workdir/submit.json"
  if [[ -f "$retained_session/mode" && "$(cat "$retained_session/mode")" == client-target ]]; then
    # Resume rebinds to the saved client target, not the ignored seed. validate
    # already refused a client-target predecessor with no target.json.
    cp "$retained_session/mode" "$workdir/mode"
    cp "$retained_session/target.json" "$workdir/target.json"
    target_task_id=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["task_id"])' "$workdir/target.json")
  fi
  implementation_run_id=$(python3 "$workdir/real-work-retained.py" identity "$retained_session" run_id)
  implementation_invocation_id=$(python3 "$workdir/real-work-retained.py" identity "$retained_session" implementation_invocation_id)
  specification_run_id=$(python3 "$workdir/real-work-retained.py" identity "$retained_session" specification_run_id)
  printf '%s\n' "$implementation_run_id" > "$workdir/implementation-run"
  printf '%s\n' "$implementation_invocation_id" > "$workdir/implementation-invocation"
  if [[ -f "$retained_session/runtime-upgrade-started" ]]; then
    : >"$workdir/runtime-upgrade-inherited"
    cp "$retained_session/freesided" "$workdir/restore-freesided"
    cp "$retained_session/verify-real-run" "$workdir/restore-verifier"
    cp "$retained_session/build-version" "$workdir/restore-build-version"
  elif [[ -f "$retained_session/runtime-upgrade-inherited" ]]; then
    : >"$workdir/runtime-upgrade-inherited"
    cp "$retained_session/restore-freesided" "$retained_session/restore-verifier" \
      "$retained_session/restore-build-version" "$workdir/"
  fi
fi

echo "building freesided" >&2
# The composition preflight rejects a daemon whose build identity carries a
# -dirty stamp, so refuse a dirty checkout here, before the build, where the
# operator can still see what to commit or stash.
if [[ -n "$(git -C "$repo_root" status --porcelain)" ]]; then
	echo "run-real-work: repository checkout $repo_root is dirty; commit or stash before a production run" >&2
	exit 2
fi
build_version="$(git -C "$repo_root" rev-parse --short=12 HEAD)"
printf '%s\n' "$build_version" >"$workdir/build-version"
(cd "$repo_root/daemon" && go build -ldflags "-X main.version=$build_version" -o "$workdir/freesided" ./cmd/freesided)
(cd "$repo_root/daemon" && go test -c -o "$workdir/verify-real-run" ./internal/integration)


echo "acquiring the production rig lease" >&2
"$workdir/freesided" rig hold \
	-state-root "$FREESIDE_REAL_RUN_STATE_ROOT" \
	-db "$db_path" \
	-listen "$listen_address" \
	-seed-root "$FREESIDE_REAL_RUN_SEED_ROOT" \
	>"$rig_acquisition" 2>>"$rig_log" &
rig_pid=$!
for _ in $(seq 1 100); do
	[[ ! -s "$rig_acquisition" ]] || break
	if ! rig_child_active; then
		wait "$rig_pid" 2>/dev/null || true
		rig_pid=""
		printf 'recovery-required\n' >"$workdir/status"
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
	printf 'recovery-required\n' >"$workdir/status"
	exit 1
fi
rig_acquired=true
FREESIDE_REAL_RUN_STATE_ROOT="$("$workdir/freesided" rig resource \
	-token-file "$rig_acquisition" -name state-root)"
db_path="$("$workdir/freesided" rig resource \
	-token-file "$rig_acquisition" -name database-path)"
listen_address="$("$workdir/freesided" rig resource \
	-token-file "$rig_acquisition" -name listen-address)"
FREESIDE_REAL_RUN_SEED_ROOT="$("$workdir/freesided" rig resource \
	-token-file "$rig_acquisition" -name seed-root)"
export FREESIDE_REAL_RUN_STATE_ROOT FREESIDE_REAL_RUN_SEED_ROOT
printf '%s\n' "$FREESIDE_REAL_RUN_STATE_ROOT" >"$workdir/state-root"
printf '%s\n' "$rig_release_timeout" >"$workdir/rig-timeout"
printf '%s\n' "$listen_address" >"$workdir/listener"
require_live_rig

if [[ "$recover_codex_credentials" == true ]]; then
  real_work_recover_codex_credentials "$workdir" "$db_path" "$listen_address" \
    ${recovery_approved_recipe_args[@]+"${recovery_approved_recipe_args[@]}"}
  exit 0
fi

if [[ -n "$retained_session" ]]; then
  # The daemon is stopped and the fresh rig proves exclusion. Preserve the
  # supported encrypted checkpoint before the ordinary migration below.
  cp "$db_path.checkpoints/latest.backup" "$workdir/pre-upgrade.backup"
  cmp "$db_path.checkpoints/latest.backup" "$workdir/pre-upgrade.backup"
fi

echo "validating the immutable production composition" >&2
composition_manifest="$workdir/composition-manifest.json"
preflight_args=(
	-rig-token-file "$rig_acquisition"
	-server-url "http://$listen_address"
	-agent-image "$FREESIDE_REAL_RUN_AGENT_IMAGE"
	-exporter-image "$FREESIDE_WARD_EXPORTER_IMAGE"
	-review-image "$FREESIDE_REAL_RUN_REVIEW_IMAGE"
	-repo "$FREESIDE_REAL_RUN_REPO"
	# The managed repository's own checkout, not this harness's repo: the base
	# being proved is a commit of FREESIDE_REAL_RUN_REPO, which is absent from the
	# freeside tree, so repo_root can never satisfy the origin-and-base check.
	-repository-checkout "$FREESIDE_REAL_RUN_REPOSITORY_CHECKOUT"
	-repository-id "$FREESIDE_REAL_RUN_REPOSITORY_ID"
	-base-ref "$FREESIDE_REAL_RUN_BASE_REF"
	-base-sha "$FREESIDE_REAL_RUN_BASE_SHA"
	-approved-recipe "$FREESIDE_REAL_RUN_APPROVED_RECIPE"
	-auth-identity "$FREESIDE_REAL_RUN_AUTH_IDENTITY"
	-auth-volume "$FREESIDE_REAL_RUN_AUTH_VOLUME"
	-review-input-root "$FREESIDE_REAL_RUN_REVIEW_INPUT_ROOT"
	-review-auth-mode "$FREESIDE_REAL_RUN_REVIEW_AUTH_MODE"
	-review-auth-identity "$FREESIDE_REAL_RUN_REVIEW_AUTH_IDENTITY"
	-review-auth-snapshot "$FREESIDE_REAL_RUN_REVIEW_AUTH_SNAPSHOT"
	-review-instructions "$FREESIDE_REAL_RUN_REVIEW_INSTRUCTIONS"
	-review-model "$FREESIDE_REAL_RUN_REVIEW_MODEL"
	-review-reasoning-effort "$FREESIDE_REAL_RUN_REVIEW_REASONING"
	-review-cost-owner "$FREESIDE_REAL_RUN_REVIEW_COST_OWNER"
	-publication-state-dir "$FREESIDE_REAL_RUN_APP_STATE"
	-publication-credentials-dir "$FREESIDE_REAL_RUN_APP_CREDS"
	-allowed-paths "$FREESIDE_REAL_RUN_ALLOWED_PATHS"
	-task "$spec_file"
	-policy "$policy_file"
	-publication "$publication_file"
	-project "$FREESIDE_REAL_RUN_PROJECT"
)
if [[ -n "$submission_id" ]]; then
  preflight_args+=(--submission-id "$submission_id")
fi
if [[ -n "$work_unit_file" ]]; then
	preflight_args+=(-work-unit "$work_unit_file")
fi
judgment_names=(FREESIDE_REAL_RUN_JUDGMENT_CLAUDE_BIN FREESIDE_REAL_RUN_JUDGMENT_CLAUDE_SHA256
  FREESIDE_REAL_RUN_JUDGMENT_MODEL FREESIDE_REAL_RUN_JUDGMENT_AUTH_SNAPSHOT)
judgment_args=()
if [[ -n "${FREESIDE_REAL_RUN_JUDGMENT_CLAUDE_BIN:-}${FREESIDE_REAL_RUN_JUDGMENT_CLAUDE_SHA256:-}${FREESIDE_REAL_RUN_JUDGMENT_MODEL:-}${FREESIDE_REAL_RUN_JUDGMENT_AUTH_SNAPSHOT:-}" ]]; then
  for name in "${judgment_names[@]}"; do
    if [[ -z "${!name:-}" ]]; then
      echo "run-real-work: $name is required for subscription judgments" >&2
      exit 2
    fi
  done
  # The clean-checkout check pins this versioned prompt to the daemon build;
  # preflight records its content digest.
  judgment_args=(-judgment-claude-bin "$FREESIDE_REAL_RUN_JUDGMENT_CLAUDE_BIN"
    -judgment-claude-sha256 "$FREESIDE_REAL_RUN_JUDGMENT_CLAUDE_SHA256"
    -judgment-model "$FREESIDE_REAL_RUN_JUDGMENT_MODEL"
    -judgment-auth-snapshot "$FREESIDE_REAL_RUN_JUDGMENT_AUTH_SNAPSHOT"
    -judgment-publication-author-prompt "$repo_root/prompts/publication-author.md")
  preflight_args+=("${judgment_args[@]}")
fi
if [[ -n "${FREESIDE_REAL_RUN_BUILD_PROXY:-}" ]]; then
	preflight_args+=(-build-proxy "$FREESIDE_REAL_RUN_BUILD_PROXY")
fi
if [[ -n "$retained_session" ]]; then
  receipt_args=("$build_version" "$spec_file" "$policy_file" "$publication_file" "$work_unit_file"
    --manual-submission-config "$manual_submission_file"
    "${required[@]}" FREESIDE_REAL_RUN_BUILD_PROXY "${judgment_names[@]}")
  if [[ "$(cat "$retained_session/status")" == recovery-required &&
    -f "$retained_session/runtime-upgrade-started" &&
    -f "$retained_session/runtime-upgrade-receipt.json" ]]; then
    python3 "$workdir/real-work-retained.py" receipt-check \
      "$retained_session/runtime-upgrade-receipt.json" "${receipt_args[@]}"
    echo 'Continuing the same approved migration attempt with unchanged inputs and reviewed build.' >&2
  else
    # Preflight may audit existing-schema authority, but cannot migrate or seed.
    retained_preflight_binary="$retained_session/freesided"
    if [[ ! -f "$retained_session/runtime-upgrade-started" && -f "$retained_session/runtime-upgrade-inherited" ]]; then
      retained_preflight_binary="$workdir/restore-freesided"
    fi
    "$retained_preflight_binary" preflight "${preflight_args[@]}" >"$workdir/pre-upgrade-composition.json"
    python3 "$workdir/real-work-retained.py" compare \
      "$workdir/retained-composition.json" "$workdir/pre-upgrade-composition.json"
  fi
  python3 "$workdir/real-work-retained.py" receipt-write \
    "$workdir/runtime-upgrade-receipt.json" "${receipt_args[@]}"
  : >"$workdir/runtime-upgrade-started"
fi

# Provision the auth identities before the new binary's composition check.
# Scrub all invocation variables so seeding cannot verify a previous run.
echo "recording the auth identity binding" >&2
env -u FREESIDE_REAL_RUN_RUN_ID -u FREESIDE_REAL_RUN_INVOCATION \
  -u FREESIDE_REAL_RUN_IMPLEMENTATION_RUN_ID \
  -u FREESIDE_REAL_RUN_IMPLEMENTATION_INVOCATION \
  -u FREESIDE_REAL_RUN_SPECIFICATION_RUN_ID \
  FREESIDE_REAL_RUN_LIVE_TEST=1 \
  go test -C "$repo_root/daemon" ./internal/integration/ \
    -run TestRealWorkItemCompletesProductionPipeline -count=1 > "$workdir/seed.log" 2>&1 || {
  echo "run-real-work: could not record the auth identity binding" >&2
  cat "$workdir/seed.log" >&2
  exit 1
}
require_live_rig

if ! "$workdir/freesided" preflight "${preflight_args[@]}" >"$composition_manifest"; then
	echo "run-real-work: production composition preflight failed" >&2
	cat "$composition_manifest" >&2
	exit 2
fi

# The state-root evidence path is content-addressed and no-clobber. An exact
# replay converges on the same bytes; different resolved inputs cannot replace
# earlier acceptance evidence.
if [[ -n "$retained_session" ]]; then
  python3 "$repo_root/scripts/real-work-retained.py" compare \
    "$workdir/retained-composition.json" "$composition_manifest"
fi

composition_digest=$(shasum -a 256 "$composition_manifest" | awk '{print $1}')
composition_evidence_dir="$FREESIDE_REAL_RUN_STATE_ROOT/production-evidence/composition"
composition_evidence="$composition_evidence_dir/$composition_digest.json"
mkdir -p "$composition_evidence_dir"
if [[ ! -e "$composition_evidence" ]]; then
	# Publish through a hard link: unlike cp -n, a lost race can neither
	# clobber the winner nor leave a partially copied file at the final name,
	# and the collision check below still compares real bytes.
	composition_evidence_tmp=$(mktemp "$composition_evidence_dir/.composition.XXXXXX")
	if cp "$composition_manifest" "$composition_evidence_tmp" &&
		ln "$composition_evidence_tmp" "$composition_evidence"; then
		rm -f "$composition_evidence_tmp"
		composition_evidence_tmp=""
	else
		rm -f "$composition_evidence_tmp"
		composition_evidence_tmp=""
		if [[ ! -e "$composition_evidence" ]]; then
			echo "run-real-work: could not publish immutable composition evidence" >&2
			exit 1
		fi
	fi
fi
if ! cmp -s "$composition_manifest" "$composition_evidence"; then
	echo "run-real-work: immutable composition evidence collision at $composition_evidence" >&2
	exit 1
fi
echo "production composition manifest: $composition_evidence" >&2

if [[ ${#judgment_args[@]} -gt 0 ]]; then
  judgment_digest=$(python3 - "$composition_evidence" <<'PY'
import json
import re
import sys

with open(sys.argv[1], encoding="utf-8") as source:
    digest = json.load(source).get("judgment_configuration_digest")
if not isinstance(digest, str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
    raise SystemExit("run-real-work: preflight judgment digest is missing or invalid")
print(digest)
PY
  )
  judgment_args+=(-judgment-configuration-digest "$judgment_digest")
fi

if [[ -n "$retained_session" ]]; then
  echo "reattaching retained implementation run=$implementation_run_id; no submission or client command" >&2
else
echo "submitting the task" >&2
require_live_rig
submit_log="$workdir/submit.json"
submit_args=(
  --submission-id "$submission_id"
  -db "$db_path"
  --task "$spec_file"
  --policy "$policy_file"
  --publication "$publication_file"
  --project "$FREESIDE_REAL_RUN_PROJECT"
  --composition-manifest "$composition_manifest"
  --require-composition
  # Vet the writer budget against the supervision deadline at the run-creation
  # boundary. submit parses both authoritatively (flag.Duration) and refuses a
  # malformed, out-of-range, or writer>=supervision value before a run exists.
  -writer-stop-timeout "$writer_stop_timeout"
  -supervision-timeout "${supervision_timeout}s"
)
if [[ -n "$work_unit_file" ]]; then
  submit_args+=(--work-unit "$work_unit_file")
fi
"$workdir/freesided" submit "${submit_args[@]}" | tee "$submit_log"
submit_status=${PIPESTATUS[0]}
if [[ "$submit_status" -ne 0 ]]; then
  echo "run-real-work: submit refused the task before creating a run (exit $submit_status); no run was dispatched" >&2
  exit "$submit_status"
fi

implementation_invocation_id="$(sed -n 's/.*"implementation_invocation_id":"\([^"]*\)".*/\1/p' "$submit_log")"
implementation_run_id="$(sed -n 's/.*"run_id":"\([^"]*\)".*/\1/p' "$submit_log")"
specification_run_id="$(sed -n 's/.*"specification_run_id":"\([^"]*\)".*/\1/p' "$submit_log")"
specification_invocation_id="$(sed -n 's/.*"specification_invocation_id":"\([^"]*\)".*/\1/p' "$submit_log")"
if [[ -z "$implementation_invocation_id" || -z "$implementation_run_id" ]]; then
  echo "run-real-work: submit produced no implementation run identity: $(cat "$submit_log")" >&2
  exit 1
fi
if [[ -n "$specification_run_id" && -n "$specification_invocation_id" ]]; then
  echo "submitted specification run=$specification_run_id invocation=$specification_invocation_id" >&2
elif [[ -n "$specification_run_id" || -n "$specification_invocation_id" ]]; then
  echo "run-real-work: submit produced a partial specification identity: $(cat "$submit_log")" >&2
  exit 1
else
  echo "legacy production-only replay: no specification approval gate" >&2
fi
echo "reserved implementation run=$implementation_run_id invocation=$implementation_invocation_id" >&2

if [[ "$client_target" == true ]]; then
  # The seed made the project visible in sync. Record its specification run so
  # the resolver can refuse reselecting it, then drop every seed identity: the
  # harness follows and verifies only the operator's client target.
  printf '%s\n' "$specification_run_id" > "$workdir/seed-specification-run"
  echo "client-target: submitted visibility seed run=$implementation_run_id; it is never followed or verified" >&2
  implementation_run_id=""
  implementation_invocation_id=""
  specification_run_id=""
  specification_invocation_id=""
fi

fi
if [[ "$client_target" == false ]]; then
printf '%s\n' "$implementation_run_id" > "$workdir/implementation-run"
printf '%s\n' "$implementation_invocation_id" > "$workdir/implementation-invocation"
# Keep a build-bound verifier and only its named configuration so later checks
# do not depend on the source checkout or capture unrelated environment secrets.
{
  for name in "${required[@]}"; do printf 'export %s=%q\n' "$name" "${!name}"; done
  printf 'export FREESIDE_REAL_RUN_IMPLEMENTATION_RUN_ID=%q\n' "$implementation_run_id"
  printf 'export FREESIDE_REAL_RUN_IMPLEMENTATION_INVOCATION=%q\n' "$implementation_invocation_id"
  if [[ -n "$specification_run_id" ]]; then
    printf 'export FREESIDE_REAL_RUN_SPECIFICATION_RUN_ID=%q\n' "$specification_run_id"
  else
    printf 'unset FREESIDE_REAL_RUN_SPECIFICATION_RUN_ID\n'
  fi
  # Set for a resumed client-target session, unset otherwise, so `verify`
  # applies the same target binding as the original run without leaking a
  # stale value into a CLI-mode verify.
  if [[ -n "$target_task_id" ]]; then
    printf 'export FREESIDE_REAL_RUN_TARGET_TASK_ID=%q\n' "$target_task_id"
  else
    printf 'unset FREESIDE_REAL_RUN_TARGET_TASK_ID\n'
  fi
} > "$workdir/verification-env.sh"

specification_verifier_env=(-u FREESIDE_REAL_RUN_SPECIFICATION_RUN_ID)
if [[ -n "$specification_run_id" ]]; then
	specification_verifier_env+=(FREESIDE_REAL_RUN_SPECIFICATION_RUN_ID="$specification_run_id")
fi
fi

echo "starting the daemon with the production Claude driver" >&2
require_live_rig
# FREESIDE_REAL_RUN_LISTEN pins the exact leased listener so an operator's
# paired client can reach the specification-approval gate.
python3 "$workdir/real-work-retained.py" check-manual "$workdir" >/dev/null
if [[ "$client_target" == true ]]; then
  # The client target is submitted while this daemon runs; the seed and any
  # pre-existing task predate this instant. The resolver refuses a task created
  # before it. Unix nanoseconds keep the comparison unambiguous.
  python3 -c 'import time; print(time.time_ns())' >"$workdir/daemon-started-at"
fi
"$workdir/freesided" \
  "${judgment_args[@]}" \
  "${manual_submission_args[@]}" \
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
  -specification-prompt-package "$FREESIDE_REAL_RUN_SPECIFICATION_PROMPT_PACKAGE" \
  -remediation-prompt-package "$FREESIDE_REAL_RUN_REMEDIATION_PROMPT_PACKAGE" \
  -vendor-instructions "$FREESIDE_REAL_RUN_INSTRUCTIONS" \
  -repo "$FREESIDE_REAL_RUN_REPO" \
  -repository-id "$FREESIDE_REAL_RUN_REPOSITORY_ID" \
  -base-ref "$FREESIDE_REAL_RUN_BASE_REF" \
  -base-sha "$FREESIDE_REAL_RUN_BASE_SHA" \
  -auth-identity "$FREESIDE_REAL_RUN_AUTH_IDENTITY" \
  -approved-recipe "$FREESIDE_REAL_RUN_APPROVED_RECIPE" \
  -writer-stop-timeout "$writer_stop_timeout" \
  -operating-mode unattended \
  -run-conformance \
  -allowed-paths "$FREESIDE_REAL_RUN_ALLOWED_PATHS" \
  -publication-state-dir "$FREESIDE_REAL_RUN_APP_STATE" \
  -publication-credentials-dir "$FREESIDE_REAL_RUN_APP_CREDS" \
  >> "$workdir/daemon.log" 2>&1 &
daemon_pid=$!

if [[ -n "$retained_session" ]]; then
  healthy=false
  health_address=$(real_work_local_address "$listen_address")
  for _ in $(seq 1 60); do
    child_job_exists "$daemon_pid" || break
    require_live_rig
    if curl --fail --silent --max-time 2 "http://$health_address/health" > "$workdir/health.json" &&
      python3 "$workdir/real-work-retained.py" health "$workdir/health.json" "$build_version" 2>/dev/null; then
      healthy=true
      break
    fi
    sleep 1
  done
  [[ "$healthy" == true ]] || { echo 'Replacement daemon health/build not verified' >&2; exit 1; }
  bash "$workdir/real-work-verify.sh" retained > "$workdir/verify-retained.log" 2>&1 || {
    echo "Retained checkpoint refused; inspect $workdir/verify-retained.log" >&2
    exit 1
  }
  echo "Retained endpoint restored for run=$implementation_run_id; this is not successor-publication acceptance." >&2
  printf 'Verify the current publication: bash %q verify %q\n' "$workdir/real-work-session.sh" "$workdir" >&2
  printf 'Complete explicitly: bash %q complete %q\n' "$workdir/real-work-session.sh" "$workdir" >&2
  real_work_walkthrough "$workdir" "$daemon_pid"
  exit 0
fi

if [[ "$client_target" == true ]]; then
  # Wait for the operator to submit the real target from a client and name it.
  # This has no deadline; interruption runs the ordinary cleanup trap.
  real_work_await_target "$workdir" "$daemon_pid"
  specification_run_id=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["specification_run_id"])' "$workdir/target.json")
  target_task_id=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["task_id"])' "$workdir/target.json")
  implementation_run_id=""
  # Supervision resolves the implementation run from the selected task once it
  # is recorded. An empty print means "not bound yet"; nonzero means error.
  real_work_resolve_implementation() {
    local rc
    real_work_bind_target "$workdir"
    rc=$?
    case "$rc" in
    0) python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["implementation_run_id"])' "$workdir/target.json"; return 0 ;;
    3) return 0 ;;
    *) return 1 ;;
    esac
  }
  # The verifier's configuration comes from the selection, not the seed. The
  # implementation identity is added to verification-env.sh after binding.
  {
    for name in "${required[@]}"; do printf 'export %s=%q\n' "$name" "${!name}"; done
    printf 'export FREESIDE_REAL_RUN_SPECIFICATION_RUN_ID=%q\n' "$specification_run_id"
    printf 'export FREESIDE_REAL_RUN_TARGET_TASK_ID=%q\n' "$target_task_id"
  } > "$workdir/verification-env.sh"
  specification_verifier_env=(FREESIDE_REAL_RUN_SPECIFICATION_RUN_ID="$specification_run_id")
fi

if [[ -n "$specification_run_id" ]]; then
  echo "gated-unattended: waiting for an operator to approve or revise the generated specification" >&2
  echo "implementation verification resumes automatically after approval" >&2
fi

# Follow durable, read-only snapshots instead of rerunning the integration
# verifier as a polling mechanism. The verifier below remains the one final
# success authority after observation reaches published.
set +e
real_work_supervise "$workdir/freesided" "$db_path" "$specification_run_id" \
	"$implementation_run_id" "$daemon_pid" \
	"$supervision_timeout" "$last_supervision_snapshot"
supervision_status=$?
set -e
if [[ "$supervision_status" -ne 0 ]]; then
	if [[ "$supervision_status" -eq 124 ]]; then
		echo "daemon log:" >&2
		tail -50 "$workdir/daemon.log" >&2
	fi
	exit "$supervision_status"
fi

if [[ "$client_target" == true ]]; then
  # Binding wrote the implementation identity into target.json during
  # supervision. Adopt it now and complete verification-env.sh so verify,
  # complete and recover bind to the selected target, never the seed.
  implementation_run_id=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["implementation_run_id"])' "$workdir/target.json")
  implementation_invocation_id=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["implementation_invocation_id"])' "$workdir/target.json")
  printf '%s\n' "$implementation_run_id" >"$workdir/implementation-run"
  printf '%s\n' "$implementation_invocation_id" >"$workdir/implementation-invocation"
  {
    for name in "${required[@]}"; do printf 'export %s=%q\n' "$name" "${!name}"; done
    printf 'export FREESIDE_REAL_RUN_IMPLEMENTATION_RUN_ID=%q\n' "$implementation_run_id"
    printf 'export FREESIDE_REAL_RUN_IMPLEMENTATION_INVOCATION=%q\n' "$implementation_invocation_id"
    printf 'export FREESIDE_REAL_RUN_SPECIFICATION_RUN_ID=%q\n' "$specification_run_id"
    printf 'export FREESIDE_REAL_RUN_TARGET_TASK_ID=%q\n' "$target_task_id"
  } > "$workdir/verification-env.sh"
fi

# Positive evidence, not the absence of an error: a Go test binary exits 0
# for a skipped test too, so require the harness's own success line.
verify_log="$workdir/verify-final.log"
set +e
env -u FREESIDE_REAL_RUN_RUN_ID -u FREESIDE_REAL_RUN_INVOCATION \
	"${specification_verifier_env[@]}" \
	${target_task_id:+FREESIDE_REAL_RUN_TARGET_TASK_ID="$target_task_id"} \
  FREESIDE_REAL_RUN_LIVE_TEST=1 \
  FREESIDE_REAL_RUN_IMPLEMENTATION_RUN_ID="$implementation_run_id" \
  FREESIDE_REAL_RUN_IMPLEMENTATION_INVOCATION="$implementation_invocation_id" \
  go test -C "$repo_root/daemon" ./internal/integration/ \
    -run TestRealWorkItemCompletesProductionPipeline -count=1 -v 2>&1 | tee -a "$verify_log"
verify_status=${PIPESTATUS[0]}
set -e
if grep -q "real run specification failed:" "$verify_log"; then
	echo "run-real-work: specification failed before implementation admission" >&2
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
printf '%s\n' "$implementation_run_id" >"$workdir/implementation-run"
printf '%s\n' "$implementation_invocation_id" >"$workdir/implementation-invocation"
echo "Walkthrough: run=$implementation_run_id invocation=$implementation_invocation_id endpoint=$listen_address session=$workdir" >&2
printf 'Complete explicitly: bash %q complete %q\n' "$workdir/real-work-session.sh" "$workdir" >&2
real_work_walkthrough "$workdir" "$daemon_pid"
