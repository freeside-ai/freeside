#!/usr/bin/env bash
# run-real-work.sh — the §11 1A.2 complete unattended production exercise.
#
# Usage: run-real-work.sh <spec-file> <resolved-policy-keys.json> <publication.json>
#
# Submits one operator-approved work item through `freesided submit`,
# runs the daemon with the production Claude driver until the run reaches
# a ready-for-review outcome, then verifies the durable export, networkless
# verification evidence, publication outcome, and exact published head with
# the real-run harness test.
#
# It never mints its own preconditions. Every binding below is the
# operator's, supplied through the environment, because each one lands in
# a durable admission record and a script-invented default would make that
# record attest to something nobody approved.
#
# Required environment:
#   FREESIDE_REAL_RUN_STATE_ROOT     daemon state root (holds the SQLite store)
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
#   FREESIDE_REAL_RUN_INSTRUCTIONS   host vendor-instruction file (CLAUDE.md)
#   FREESIDE_REAL_RUN_APPROVED_RECIPE exact recipe digest approved by onboarding
#   FREESIDE_REAL_RUN_APP_STATE      GitHub App authority state directory
#   FREESIDE_REAL_RUN_APP_CREDS      GitHub App credential directory
#   FREESIDE_REAL_RUN_PROJECT        project id the run belongs to
#   FREESIDE_REAL_RUN_ALLOWED_PATHS  comma-separated declared path scope the
#                                    agent may rewrite (no match-everything
#                                    default: it is a containment control)
#
# Requires: Go, Apple `container` running, macOS, and an authenticated
# credential volume for the named identity. The harness runs and durably
# records the exact production configuration's ward conformance suite before
# the daemon can admit the submitted work.
# The publication JSON is durable operator input with this shape:
#   {"title":"Imperative PR title","body":"Reviewer-ready PR body",
#    "commit_author":{"app_slug":"canonical-app-slug","bot_user_id":123}}
# The slug and bot user ID claim the selected GitHub App bot's public canonical
# attribution fields. Before execution, the daemon resolves that account from
# the App registration selected by its installation token and requires an
# exact match. The fields contain no credential or publication authority.
set -euo pipefail

spec_file="${1:-}"
policy_file="${2:-}"
publication_file="${3:-}"
if [[ -z "$spec_file" || -z "$policy_file" || -z "$publication_file" ]]; then
  echo "usage: run-real-work.sh <spec-file> <resolved-policy-keys.json> <publication.json>" >&2
  exit 2
fi
for path in "$spec_file" "$policy_file" "$publication_file"; do
  if [[ ! -f "$path" ]]; then
    echo "run-real-work: $path is not a file" >&2
    exit 2
  fi
done

required=(
  FREESIDE_REAL_RUN_STATE_ROOT FREESIDE_REAL_RUN_AGENT_IMAGE FREESIDE_WARD_EXPORTER_IMAGE
  FREESIDE_REAL_RUN_REVIEW_IMAGE FREESIDE_REAL_RUN_REVIEW_INPUT_ROOT
  FREESIDE_REAL_RUN_REVIEW_AUTH_MODE FREESIDE_REAL_RUN_REVIEW_AUTH_IDENTITY
  FREESIDE_REAL_RUN_REVIEW_AUTH_SNAPSHOT FREESIDE_REAL_RUN_REVIEW_INSTRUCTIONS
  FREESIDE_REAL_RUN_REVIEW_MODEL FREESIDE_REAL_RUN_REVIEW_REASONING
  FREESIDE_REAL_RUN_REVIEW_COST_OWNER
  FREESIDE_REAL_RUN_SEED_ROOT FREESIDE_REAL_RUN_AUTH_IDENTITY FREESIDE_REAL_RUN_AUTH_VOLUME
  FREESIDE_REAL_RUN_REPO FREESIDE_REAL_RUN_REPOSITORY_ID FREESIDE_REAL_RUN_BASE_REF
  FREESIDE_REAL_RUN_BASE_SHA FREESIDE_REAL_RUN_PROMPT_PACKAGE FREESIDE_REAL_RUN_INSTRUCTIONS
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
  --publication "$publication_file" \
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
    -run TestRealWorkItemCompletesProductionPipeline -count=1 > "$workdir/seed.log" 2>&1 || {
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
  -prompt-package "$FREESIDE_REAL_RUN_PROMPT_PACKAGE" \
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

# Wait for the run to reach the durable ready state. The verifier below is the
# authority on success; this loop only bounds the wait.
deadline=$(( SECONDS + ${FREESIDE_REAL_RUN_TIMEOUT_SECONDS:-2400} ))
while (( SECONDS < deadline )); do
  if ! kill -0 "$daemon_pid" 2>/dev/null; then
    echo "run-real-work: the daemon exited before the run finished" >&2
    cat "$workdir/daemon.log" >&2
    exit 1
  fi
  if FREESIDE_REAL_RUN_LIVE_TEST=1 FREESIDE_REAL_RUN_INVOCATION="$invocation_id" \
    FREESIDE_REAL_RUN_RUN_ID="$run_id" \
    go test -C "$repo_root/daemon" ./internal/integration/ \
    -run TestRealWorkItemCompletesProductionPipeline -count=1 > "$workdir/verify.log" 2>&1; then
    break
  fi
  if grep -q "real run terminal outcome:" "$workdir/verify.log"; then
    echo "run-real-work: the run reached a failed terminal outcome" >&2
    cat "$workdir/verify.log" >&2
    echo "daemon log:" >&2
    tail -50 "$workdir/daemon.log" >&2
    exit 1
  fi
  if grep -q "real run publication blocked:" "$workdir/verify.log"; then
    echo "run-real-work: publication was durably blocked" >&2
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
  FREESIDE_REAL_RUN_RUN_ID="$run_id" \
  go test -C "$repo_root/daemon" ./internal/integration/ \
    -run TestRealWorkItemCompletesProductionPipeline -count=1 -v 2>&1 | tee "$verify_log"
if ! grep -q "real production pipeline verified: PR #" "$verify_log"; then
  echo "run-real-work: the run did not reach a verified ready publication" >&2
  echo "daemon log:" >&2
  tail -50 "$workdir/daemon.log" >&2
  exit 1
fi
echo "run-real-work: verified ready publication for run=$run_id invocation=$invocation_id" >&2
