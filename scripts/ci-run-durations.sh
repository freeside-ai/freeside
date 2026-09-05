#!/usr/bin/env bash
# Print job and step seconds, then successful-job medians, for recent CI runs.
# Usage: ci-run-durations.sh <workflow-name> [--branch main] [--limit N]
# Requires authenticated gh and jq. Run from the repository to measure.
# Re-runs report the latest attempt; save each report before the next re-run.
set -euo pipefail

usage() {
  echo "usage: $0 <workflow-name> [--branch main] [--limit N]" >&2
  exit 2
}

[[ $# -gt 0 ]] || usage
workflow=$1
shift
branch=main
limit=6
while [[ $# -gt 0 ]]; do
  case $1 in
    --branch) [[ $# -ge 2 && -n $2 ]] || usage; branch=$2; shift 2 ;;
    --limit) [[ $# -ge 2 ]] || usage; limit=$2; shift 2 ;;
    *) usage ;;
  esac
done
[[ $limit =~ ^[1-9][0-9]*$ ]] || usage

scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
gh run list --workflow "$workflow" --branch "$branch" --limit "$limit" \
  --json databaseId,headSha,createdAt >"$scratch/runs.json"
jq -r '.[].databaseId' "$scratch/runs.json" >"$scratch/ids"
while IFS= read -r run_id; do
  gh api --paginate "repos/{owner}/{repo}/actions/runs/$run_id/jobs?per_page=100" \
    | jq -c '.jobs[]' >>"$scratch/jobs.jsonl"
done <"$scratch/ids"
touch "$scratch/jobs.jsonl"

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
