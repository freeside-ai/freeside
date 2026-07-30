#!/usr/bin/env bash
# run-real-work.sh — the #237 real execution-export exercise.
#
# Usage: run-real-work.sh <spec-file> <resolved-policy-keys.json>
#
# Submits one operator-approved work item through `freesided submit`,
# runs the daemon with the production Claude driver until the run reaches
# a terminal outcome, then verifies the durable execution record with the
# real-run harness test. Its scope ends at the pre-publication slice: Claude →
# digest-bound inputs → exact-base ward handoff → gauntlet → durable
# `ExecutionExport`. #318 owns clean verification and publication binding.
#
# It never mints its own preconditions. Every binding below is the
# operator's, supplied through the environment, because each one lands in
# a durable admission record and a script-invented default would make that
# record attest to something nobody approved.
#
# Required environment:
#   FREESIDE_REAL_RUN_STATE_ROOT     daemon state root (holds the SQLite store)
#   FREESIDE_REAL_RUN_AGENT_IMAGE    digest-pinned Claude agent image
#   FREESIDE_WARD_EXPORTER_IMAGE     digest-pinned export helper image
#   FREESIDE_REAL_RUN_SEED_ROOT      daemon-owned exact-base checkout root
#   FREESIDE_REAL_RUN_AUTH_IDENTITY  provider auth identity id
#   FREESIDE_REAL_RUN_AUTH_VOLUME    that identity's credential volume
#   FREESIDE_REAL_RUN_REPO           managed owner/name repository
#   FREESIDE_REAL_RUN_REPOSITORY_ID  canonical numeric repository id
#   FREESIDE_REAL_RUN_BASE_REF       short base branch name (for example main)
#   FREESIDE_REAL_RUN_BASE_SHA       exact 40-character base commit
#   FREESIDE_REAL_RUN_PROMPT_PACKAGE trusted prompt-package file
#   FREESIDE_REAL_RUN_INSTRUCTIONS   host vendor-instruction file (CLAUDE.md)
#   FREESIDE_REAL_RUN_APP_STATE      GitHub App authority state directory
#   FREESIDE_REAL_RUN_APP_CREDS      GitHub App credential directory
#   FREESIDE_REAL_RUN_PROJECT        project id the run belongs to
#   FREESIDE_REAL_RUN_ALLOWED_PATHS  comma-separated declared path scope the
#                                    agent may rewrite (no match-everything
#                                    default: it is a containment control)
#   FREESIDE_REAL_RUN_WAIVER_REPO_ID repository id the §5.7 Phase 1A.2
#                                    backup-encryption waiver covers; unattended
#                                    admission has no other backup authorization
#                                    until the encrypted checkpoint lands (#305)
#
# Requires: Go, Apple `container` running, macOS, and an authenticated
# credential volume for the named identity. The harness runs and durably
# records the exact production configuration's ward conformance suite before
# the daemon can admit the submitted work.
set -euo pipefail

spec_file="${1:-}"
policy_file="${2:-}"
if [[ -z "$spec_file" || -z "$policy_file" ]]; then
  echo "usage: run-real-work.sh <spec-file> <resolved-policy-keys.json>" >&2
  exit 2
fi
for path in "$spec_file" "$policy_file"; do
  if [[ ! -f "$path" ]]; then
    echo "run-real-work: $path is not a file" >&2
    exit 2
  fi
done

required=(
  FREESIDE_REAL_RUN_STATE_ROOT FREESIDE_REAL_RUN_AGENT_IMAGE FREESIDE_WARD_EXPORTER_IMAGE
  FREESIDE_REAL_RUN_SEED_ROOT FREESIDE_REAL_RUN_AUTH_IDENTITY FREESIDE_REAL_RUN_AUTH_VOLUME
  FREESIDE_REAL_RUN_REPO FREESIDE_REAL_RUN_REPOSITORY_ID FREESIDE_REAL_RUN_BASE_REF
  FREESIDE_REAL_RUN_BASE_SHA FREESIDE_REAL_RUN_PROMPT_PACKAGE FREESIDE_REAL_RUN_INSTRUCTIONS
  FREESIDE_REAL_RUN_APP_STATE FREESIDE_REAL_RUN_APP_CREDS FREESIDE_REAL_RUN_PROJECT
  FREESIDE_REAL_RUN_ALLOWED_PATHS FREESIDE_REAL_RUN_WAIVER_REPO_ID
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

# The verifier opens the store with the waiver bound to the repository id, so
# a daemon started under a different one admits and exports successfully while
# every verify re-gate refuses the admission. The poll cannot tell that from
# "not finished yet", so it would burn the whole deadline and then report a
# failure for a run that actually completed. Same reason as the checks below:
# fail on the configuration mistake now.
if [[ "$FREESIDE_REAL_RUN_WAIVER_REPO_ID" != "$FREESIDE_REAL_RUN_REPOSITORY_ID" ]]; then
  echo "run-real-work: FREESIDE_REAL_RUN_WAIVER_REPO_ID ($FREESIDE_REAL_RUN_WAIVER_REPO_ID)" \
    "must equal FREESIDE_REAL_RUN_REPOSITORY_ID ($FREESIDE_REAL_RUN_REPOSITORY_ID);" \
    "the verifier binds the backup-encryption waiver to the repository id" >&2
  exit 2
fi

# Digest pinning is the ward's own refusal; checking it here reports a
# configuration mistake now instead of a gate failure deep into a run.
for ref in "$FREESIDE_REAL_RUN_AGENT_IMAGE" "$FREESIDE_WARD_EXPORTER_IMAGE"; do
  if [[ "$ref" != *"@sha256:"* ]]; then
    echo "run-real-work: image reference is not digest-pinned: $ref" >&2
    exit 2
  fi
done

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
workdir="$(mktemp -d)"
daemon_pid=""

cleanup() {
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
  rm -rf "$workdir"
}
trap cleanup EXIT

db_path="$FREESIDE_REAL_RUN_STATE_ROOT/freeside.db"
mkdir -p "$FREESIDE_REAL_RUN_STATE_ROOT" "$FREESIDE_REAL_RUN_SEED_ROOT"

echo "building freesided" >&2
(cd "$repo_root/daemon" && go build -o "$workdir/freesided" ./cmd/freesided)

echo "submitting the work item" >&2
submit_log="$workdir/submit.json"
"$workdir/freesided" submit \
  -db "$db_path" \
  --spec "$spec_file" \
  --policy "$policy_file" \
  --project "$FREESIDE_REAL_RUN_PROJECT" | tee "$submit_log"

invocation_id="$(sed -n 's/.*"invocation_id":"\([^"]*\)".*/\1/p' "$submit_log")"
run_id="$(sed -n 's/.*"run_id":"\([^"]*\)".*/\1/p' "$submit_log")"
if [[ -z "$invocation_id" || -z "$run_id" ]]; then
  echo "run-real-work: submit produced no run identity: $(cat "$submit_log")" >&2
  exit 1
fi
echo "submitted run=$run_id invocation=$invocation_id" >&2

# Seed the durable auth-identity binding before the daemon can reach
# admission. The verifier records it too, but that call happens inside the
# polling loop, i.e. after the daemon is already dispatching: leaving it there
# makes the harness race its own precondition. Running the verifier once here
# is the seeding step; it exits without verifying because no invocation id is
# set yet.
# FREESIDE_REAL_RUN_INVOCATION is unset for this call on purpose: an exported
# value left over from an earlier run would make the seeding step verify that
# old invocation instead of skipping, and its failure would surface here as
# the misleading "could not record the auth identity binding".
echo "recording the auth identity binding" >&2
env -u FREESIDE_REAL_RUN_INVOCATION FREESIDE_REAL_RUN_LIVE_TEST=1 \
  go test -C "$repo_root/daemon" ./internal/integration/ \
    -run TestRealWorkItemProducesExecutionExport -count=1 > "$workdir/seed.log" 2>&1 || {
  echo "run-real-work: could not record the auth identity binding" >&2
  cat "$workdir/seed.log" >&2
  exit 1
}

echo "starting the daemon with the production Claude driver" >&2
"$workdir/freesided" \
  -db "$db_path" \
  -driver claude \
  -agent-image "$FREESIDE_REAL_RUN_AGENT_IMAGE" \
  -exporter-image "$FREESIDE_WARD_EXPORTER_IMAGE" \
  -seed-root "$FREESIDE_REAL_RUN_SEED_ROOT" \
  -state-dir "$FREESIDE_REAL_RUN_STATE_ROOT" \
  -prompt-package "$FREESIDE_REAL_RUN_PROMPT_PACKAGE" \
  -vendor-instructions "$FREESIDE_REAL_RUN_INSTRUCTIONS" \
  -repo "$FREESIDE_REAL_RUN_REPO" \
  -repository-id "$FREESIDE_REAL_RUN_REPOSITORY_ID" \
  -base-ref "$FREESIDE_REAL_RUN_BASE_REF" \
  -base-sha "$FREESIDE_REAL_RUN_BASE_SHA" \
  -auth-identity "$FREESIDE_REAL_RUN_AUTH_IDENTITY" \
  -run-conformance \
  -allowed-paths "$FREESIDE_REAL_RUN_ALLOWED_PATHS" \
  -backup-encryption-waiver-repository-id "$FREESIDE_REAL_RUN_WAIVER_REPO_ID" \
  -publication-state-dir "$FREESIDE_REAL_RUN_APP_STATE" \
  -publication-credentials-dir "$FREESIDE_REAL_RUN_APP_CREDS" \
  > "$workdir/daemon.log" 2>&1 &
daemon_pid=$!

# Wait for the run to reach a terminal outcome. The export verifier below is
# the authority on this slice's success; this loop only bounds the wait.
deadline=$(( SECONDS + ${FREESIDE_REAL_RUN_TIMEOUT_SECONDS:-2400} ))
while (( SECONDS < deadline )); do
  if ! kill -0 "$daemon_pid" 2>/dev/null; then
    echo "run-real-work: the daemon exited before the run finished" >&2
    cat "$workdir/daemon.log" >&2
    exit 1
  fi
  if FREESIDE_REAL_RUN_LIVE_TEST=1 FREESIDE_REAL_RUN_INVOCATION="$invocation_id" \
    go test -C "$repo_root/daemon" ./internal/integration/ \
    -run TestRealWorkItemProducesExecutionExport -count=1 > "$workdir/verify.log" 2>&1; then
    break
  fi
  if grep -q "real run terminal outcome:" "$workdir/verify.log"; then
    echo "run-real-work: the run reached a non-export terminal outcome" >&2
    cat "$workdir/verify.log" >&2
    echo "daemon log:" >&2
    tail -50 "$workdir/daemon.log" >&2
    exit 1
  fi
  sleep 15
done

kill "$daemon_pid" 2>/dev/null || true
wait "$daemon_pid" 2>/dev/null || true
daemon_pid=""

# Positive evidence, not the absence of an error: a Go test binary exits 0
# for a skipped test too, so require the harness's own success line.
verify_log="$workdir/verify-final.log"
FREESIDE_REAL_RUN_LIVE_TEST=1 FREESIDE_REAL_RUN_INVOCATION="$invocation_id" \
  go test -C "$repo_root/daemon" ./internal/integration/ \
    -run TestRealWorkItemProducesExecutionExport -count=1 -v 2>&1 | tee "$verify_log"
if ! grep -q "real execution export verified: head" "$verify_log"; then
  echo "run-real-work: the run did not reach a verified execution export" >&2
  echo "daemon log:" >&2
  tail -50 "$workdir/daemon.log" >&2
  exit 1
fi
echo "run-real-work: verified execution export for run=$run_id invocation=$invocation_id" >&2
