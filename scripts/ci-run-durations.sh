#!/usr/bin/env bash
# Print job and step seconds, then successful-job medians, for recent CI runs.
# Usage: ci-run-durations.sh <workflow-name> [--branch main] [--limit N]
#        [--event pull_request] [--attempts all|latest] [--whole-gate]
# Requires authenticated gh and jq. Run from the repository to measure.
# Defaults preserve the existing latest-attempt job/step report. --attempts
# all includes failed and cancelled attempts. --whole-gate measures standard
# daemon work through both stable final gates, including caches and child
# jobs, and reports initial queue delay separately. Race jobs are excluded.
set -euo pipefail

usage() {
  echo "usage: $0 <workflow-name> [--branch main] [--limit N] [--event EVENT] [--attempts all|latest] [--whole-gate]" >&2
  exit 2
}

[[ $# -gt 0 ]] || usage
workflow=$1
shift
branch=main
limit=6
event=''
attempts=latest
whole_gate=false
while [[ $# -gt 0 ]]; do
  case $1 in
    --branch) [[ $# -ge 2 && -n $2 ]] || usage; branch=$2; shift 2 ;;
    --limit) [[ $# -ge 2 ]] || usage; limit=$2; shift 2 ;;
    --event) [[ $# -ge 2 && -n $2 ]] || usage; event=$2; shift 2 ;;
    --attempts) [[ $# -ge 2 && $2 =~ ^(all|latest)$ ]] || usage; attempts=$2; shift 2 ;;
    --whole-gate) whole_gate=true; shift ;;
    *) usage ;;
  esac
done
[[ $limit =~ ^[1-9][0-9]*$ ]] || usage

scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
args=(--workflow "$workflow" --branch "$branch" --limit "$limit")
if [[ -n $event ]]; then args+=(--event "$event"); fi
gh run list "${args[@]}" \
  --json databaseId,headSha,createdAt >"$scratch/runs.json"
jq -r '.[].databaseId' "$scratch/runs.json" >"$scratch/ids"
while IFS= read -r run_id; do
  gh api "repos/{owner}/{repo}/actions/runs/$run_id" >"$scratch/run.json"
  latest=$(jq -r '.run_attempt' "$scratch/run.json")
  if [[ $whole_gate == true ]]; then
    # A failed-jobs-only rerun can contain just successful final gates.
    # Only the complete original work graph is an uncached timing sample.
    gh api --paginate "repos/{owner}/{repo}/actions/runs/$run_id/attempts/1/jobs?per_page=100" \
      | jq -c '.jobs[]' >>"$scratch/expected-jobs.jsonl"
  fi
  first=$latest
  if [[ $attempts == all ]]; then first=1; fi
  for ((attempt=first; attempt<=latest; attempt++)); do
    gh api "repos/{owner}/{repo}/actions/runs/$run_id/attempts/$attempt" \
      | jq -c '.' >>"$scratch/attempts.jsonl"
    gh api --paginate "repos/{owner}/{repo}/actions/runs/$run_id/attempts/$attempt/jobs?per_page=100" \
      | jq -c '.jobs[]' >>"$scratch/jobs.jsonl"
  done
done <"$scratch/ids"
touch "$scratch/jobs.jsonl"
touch "$scratch/attempts.jsonl"
touch "$scratch/expected-jobs.jsonl"

jq -rs --slurpfile runs "$scratch/runs.json" '
  def seconds:
    if .started_at != null and .completed_at != null and .status == "completed"
    then (.completed_at | fromdateiso8601) - (.started_at | fromdateiso8601)
    else null end;
  def median:
    sort | length as $n |
    if $n % 2 == 1 then .[$n / 2 | floor]
    else (.[($n / 2) - 1] + .[$n / 2]) / 2 end;
  . as $jobs |
  (["kind", "run", "attempt", "sha", "job", "step", "conclusion", "seconds", "samples"] | @tsv),
  ($jobs[] | . as $job |
    ($runs[0][] | select(.databaseId == $job.run_id) | .headSha) as $sha |
    (["job", .run_id, .run_attempt, $sha, .name, "", .conclusion, seconds, ""] | @tsv),
    (.steps[] | ["step", $job.run_id, $job.run_attempt, $sha, $job.name,
      .name, .conclusion, seconds, ""] | @tsv)),
  ($jobs | map(select(.conclusion == "success" and seconds != null)) |
    group_by(.name)[] |
    ["median", "", "", "", .[0].name, "", "success",
      (map(seconds) | median), length] | @tsv)
' "$scratch/jobs.jsonl"

if [[ $whole_gate == true ]]; then
  jq -rs --slurpfile jobs "$scratch/jobs.jsonl" --slurpfile expected "$scratch/expected-jobs.jsonl" '
    def median:
      sort | length as $n |
      if $n % 2 == 1 then .[$n / 2 | floor]
      else (.[($n / 2) - 1] + .[$n / 2]) / 2 end;
    def required:
      .name | test("^(linux|macos) \\((build|test)");
    def final:
      .name == "linux (build, test, vet, lint)" or .name == "macos (build, test)";
    map(. as $run |
      ($jobs | map(select(.run_id == $run.id and .run_attempt == $run.run_attempt and required))) as $work |
      ($work | map(select(final))) as $gates |
      ($expected | map(select(.run_id == $run.id and required)) | map(.name) | sort) as $names |
      ($work | map(.started_at | select(. != null) | fromdateiso8601) | min) as $start |
      ($gates | map(.completed_at | select(. != null) | fromdateiso8601) | max) as $end |
      {run: .id, attempt: .run_attempt, sha: .head_sha,
       base: ([.pull_requests[].base.sha] | unique | join(",")),
       conclusion: .conclusion,
       complete: (($work | length > 0) and ($gates | length == 2) and
         (($work | map(.name) | sort) == $names) and
         ($work | all(.status == "completed" and .conclusion == "success"))),
       span: (if $start != null and $end != null then $end - $start else null end),
       queue: (if $start != null and .run_attempt == 1 then $start - (.created_at | fromdateiso8601) else null end),
       attempt_queue: (if $start != null and .run_started_at != null
                      then $start - (.run_started_at | fromdateiso8601) else null end)}
    ) as $reports |
    (["kind", "run", "attempt", "head_sha", "base_sha", "conclusion", "complete", "seconds", "samples"] | @tsv),
    ($reports[] | ["whole-gate", .run, .attempt, .sha, .base, .conclusion, .complete, .span, ""] | @tsv),
    ($reports[] | ["initial-queue", .run, .attempt, .sha, .base, .conclusion, .complete, .queue, ""] | @tsv),
    ($reports[] | ["attempt-queue", .run, .attempt, .sha, .base, .conclusion, .complete, .attempt_queue, ""] | @tsv),
    ($reports | map(select(.complete and .span != null)) | select(length > 0) |
      ["whole-gate-median", "", "", "", "", "success", true, (map(.span) | median), length] | @tsv)
  ' "$scratch/attempts.jsonl"
fi
